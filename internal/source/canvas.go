package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kurenn/slack-tui/internal/data"
)

type slackCanvasResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

type canvasFile struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Name      string `json:"name"`
	Mimetype  string `json:"mimetype"`
	Filetype  string `json:"filetype"`
	URL       string `json:"url_private_download"`
	Permalink string `json:"permalink"`
}

func (f canvasFile) canvas(channelID string) data.Canvas {
	title := f.Title
	if title == "" {
		title = f.Name
	}
	if title == "" {
		title = f.ID
	}
	return data.Canvas{ID: f.ID, Title: title, URL: f.Permalink, ChannelID: channelID}
}
func (f canvasFile) isCanvas() bool {
	return f.Mimetype == "application/vnd.slack-docs" || f.Filetype == "canvas" || f.Filetype == "quip"
}

// Canvases includes the channel's primary Canvas and any shared canvases.
// An empty channel ID lists accessible workspace canvases. Reading never creates.
func (s *Slack) Canvases(channelID string) ([]data.Canvas, error) {
	var out []data.Canvas
	seen := map[string]bool{}
	if channelID != "" {
		id, err := s.channelCanvasID(channelID)
		if err != nil {
			return nil, err
		}
		if id != "" {
			f, err := s.canvasInfo(id)
			if err != nil {
				return nil, err
			}
			out = append(out, f.canvas(channelID))
			seen[id] = true
		}
	}
	for page := 1; ; page++ {
		var resp struct {
			Files  []canvasFile `json:"files"`
			Paging struct {
				Pages int `json:"pages"`
			} `json:"paging"`
		}
		q := url.Values{"count": {"100"}, "page": {strconv.Itoa(page)}}
		if channelID != "" {
			q.Set("channel", channelID)
		}
		// Filter locally: Slack's documented files.list type filters do not include canvas.
		if err := s.canvasGET("files.list", q, &resp); err != nil {
			return nil, err
		}
		for _, f := range resp.Files {
			if f.isCanvas() && !seen[f.ID] {
				out = append(out, f.canvas(""))
				seen[f.ID] = true
			}
		}
		if page >= resp.Paging.Pages {
			break
		}
	}
	return out, nil
}

func (s *Slack) canvasInfo(id string) (canvasFile, error) {
	var resp struct {
		File canvasFile `json:"file"`
	}
	err := s.canvasGET("files.info", url.Values{"file": {id}}, &resp)
	if resp.File.ID == "" {
		resp.File.ID = id
	}
	return resp.File, err
}

func canvasRevision(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (s *Slack) ReadCanvas(id string) (data.CanvasDocument, error) {
	f, err := s.canvasInfo(id)
	if err != nil {
		return data.CanvasDocument{}, err
	}
	if !f.isCanvas() {
		return data.CanvasDocument{}, fmt.Errorf("%s is not a Canvas", id)
	}
	if f.URL == "" {
		return data.CanvasDocument{}, fmt.Errorf("Slack returned no download URL for Canvas %s", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var b limitedBuffer
	b.limit = 10 << 20
	if err = s.api.GetFileContext(ctx, f.URL, &b); err != nil {
		return data.CanvasDocument{}, fmt.Errorf("download Canvas: %w", err)
	}
	raw := b.buf.String()
	markdown, warnings, err := canvasHTMLToMarkdown(raw)
	if err != nil {
		return data.CanvasDocument{}, err
	}
	return data.CanvasDocument{Canvas: f.canvas(""), Markdown: markdown, Revision: canvasRevision(raw), Editable: len(warnings) == 0, Warnings: warnings, Sections: canvasHTMLSections(raw)}, nil
}

func (s *Slack) CreateCanvas(channelID, title, markdown string) (data.Canvas, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return data.Canvas{}, fmt.Errorf("Canvas title cannot be empty")
	}
	if len(markdown) > 1<<20 {
		return data.Canvas{}, fmt.Errorf("Canvas Markdown exceeds 1 MiB")
	}
	payload := map[string]any{"document_content": map[string]string{"type": "markdown", "markdown": markdown}}
	method := "canvases.create"
	if channelID == "" {
		payload["title"] = title
	} else {
		method = "conversations.canvases.create"
		payload["channel_id"] = channelID
	}
	var resp struct {
		ID string `json:"canvas_id"`
	}
	if err := s.canvasPOST(method, payload, &resp); err != nil {
		return data.Canvas{}, err
	}
	if resp.ID == "" {
		return data.Canvas{}, fmt.Errorf("Slack created a Canvas but returned no ID")
	}
	c := data.Canvas{ID: resp.ID, Title: title, ChannelID: channelID}
	if channelID != "" {
		if err := s.ChangeCanvas(resp.ID, data.CanvasChange{Operation: "rename", Markdown: title}); err != nil {
			return c, fmt.Errorf("Canvas %s was created, but renaming failed: %w", resp.ID, err)
		}
	}
	return c, nil
}

func (s *Slack) ChangeCanvas(id string, change data.CanvasChange) error {
	if len(change.Markdown) > 1<<20 {
		return fmt.Errorf("Canvas Markdown exceeds 1 MiB")
	}
	payload := map[string]any{"operation": change.Operation}
	switch change.Operation {
	case "rename":
		if strings.TrimSpace(change.Markdown) == "" {
			return fmt.Errorf("Canvas title cannot be empty")
		}
		payload["title_content"] = map[string]string{"type": "markdown", "markdown": change.Markdown}
	case "delete":
		if change.SectionID == "" {
			return fmt.Errorf("section deletion requires a section ID")
		}
	case "insert_before", "insert_after":
		if change.SectionID == "" {
			return fmt.Errorf("relative insertion requires a section ID")
		}
		payload["document_content"] = map[string]string{"type": "markdown", "markdown": change.Markdown}
	case "replace", "insert_at_start", "insert_at_end":
		if change.Operation == "replace" && change.SectionID == "" {
			if change.ExpectedRevision == "" {
				return fmt.Errorf("read the Canvas before replacing its content")
			}
			latest, err := s.ReadCanvas(id)
			if err != nil {
				return err
			}
			if !latest.Editable {
				return fmt.Errorf("this Canvas contains rich content that cannot be replaced through Markdown; append content or edit a section instead")
			}
			if latest.Revision != change.ExpectedRevision {
				return fmt.Errorf("the Canvas changed while you were editing; your draft is retained, refresh before merging")
			}
		}
		payload["document_content"] = map[string]string{"type": "markdown", "markdown": change.Markdown}
	default:
		return fmt.Errorf("unsupported Canvas operation %q", change.Operation)
	}
	if change.SectionID != "" {
		payload["section_id"] = change.SectionID
	}
	return s.canvasPOST("canvases.edit", map[string]any{"canvas_id": id, "changes": []any{payload}}, nil)
}

func (s *Slack) CanvasSections(id, text string) ([]data.CanvasSection, error) {
	var resp struct {
		Sections []data.CanvasSection `json:"sections"`
	}
	if err := s.canvasPOST("canvases.sections.lookup", map[string]any{"canvas_id": id, "criteria": map[string]any{"contains_text": text}}, &resp); err != nil {
		return nil, err
	}
	doc, err := s.ReadCanvas(id)
	if err != nil {
		return nil, err
	}
	previews := map[string]data.CanvasSection{}
	for _, section := range doc.Sections {
		previews[section.ID] = section
	}
	for i, section := range resp.Sections {
		if preview, ok := previews[section.ID]; ok {
			resp.Sections[i] = preview
		}
	}
	return resp.Sections, nil
}

// CanvasAccess adjusts existing access, without sending messages or sharing links.
// Slack requires the Canvas to have already been shared with the target.
func (s *Slack) CanvasAccess(id, targetID, level string) error {
	key := "user_ids"
	if strings.HasPrefix(targetID, "C") || strings.HasPrefix(targetID, "G") {
		key = "channel_ids"
	}
	payload := map[string]any{"canvas_id": id, key: []string{targetID}}
	method := "canvases.access.set"
	if level == "remove" {
		method = "canvases.access.delete"
	} else {
		if level != "read" && level != "write" && level != "owner" {
			return fmt.Errorf("access must be read, write, owner, or remove")
		}
		payload["access_level"] = level
	}
	if targetID == "" {
		return fmt.Errorf("access requires a user or channel ID")
	}
	return s.canvasPOST(method, payload, nil)
}

func (s *Slack) DeleteCanvas(id string) error {
	return s.canvasPOST("canvases.delete", map[string]string{"canvas_id": id}, nil)
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("Canvas download exceeds %d MB", b.limit>>20)
	}
	return b.buf.Write(p)
}

func (s *Slack) channelCanvasID(channelID string) (string, error) {
	var resp struct {
		Channel struct {
			Properties struct {
				Canvas json.RawMessage `json:"canvas"`
			} `json:"properties"`
		} `json:"channel"`
	}
	if err := s.canvasGET("conversations.info", url.Values{"channel": {channelID}}, &resp); err != nil {
		return "", err
	}
	return canvasID(resp.Channel.Properties.Canvas), nil
}

func canvasID(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return id
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	for _, key := range []string{"file_id", "canvas_id", "id"} {
		if json.Unmarshal(fields[key], &id) == nil && id != "" {
			return id
		}
	}
	return ""
}

func (s *Slack) canvasGET(method string, query url.Values, result any) error {
	requestURL := s.canvasEndpoint(method)
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	return s.canvasDo(req, result)
}

func (s *Slack) canvasPOST(method string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Slack %s request: %w", method, err)
	}
	req, err := http.NewRequest(http.MethodPost, s.canvasEndpoint(method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return s.canvasDo(req, result)
}

func (s *Slack) canvasEndpoint(method string) string {
	base := s.canvasAPIURL
	if base == "" {
		base = "https://slack.com/api/"
	}
	return strings.TrimRight(base, "/") + "/" + method
}

func (s *Slack) canvasDo(req *http.Request, result any) error {
	req.Header.Set("Authorization", "Bearer "+s.userToken)
	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Slack Canvas request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Slack Canvas HTTP %s", resp.Status)
	}
	var envelope slackCanvasResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode Slack Canvas response: %w", err)
	}
	if !envelope.OK {
		message := envelope.Error
		if envelope.Detail != "" {
			message += ": " + envelope.Detail
		}
		if envelope.Error == "missing_scope" {
			switch req.URL.Path {
			case "/api/files.info", "/api/files.list":
				message += " (add files:read in OAuth & Permissions, then run slack-tui login again)"
			case "/api/canvases.sections.lookup":
				message += " (add canvases:read in OAuth & Permissions, then run slack-tui login again)"
			case "/api/conversations.info":
				message += " (add channels:read in OAuth & Permissions, then run slack-tui login again)"
			default:
				message += " (add canvases:write in OAuth & Permissions, then run slack-tui login again)"
			}
		}
		return fmt.Errorf("Slack Canvas: %s", message)
	}
	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("decode Slack Canvas response: %w", err)
		}
	}
	return nil
}
