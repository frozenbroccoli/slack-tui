package source

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kurenn/slack-tui/internal/data"
)

func TestCanvasID(t *testing.T) {
	for raw, want := range map[string]string{`"F123"`: "F123", `{"file_id":"F456"}`: "F456", `{"canvas_id":"F789"}`: "F789", `{"id":"FABC"}`: "FABC", `null`: ""} {
		if got := canvasID(json.RawMessage(raw)); got != want {
			t.Errorf("canvasID(%s)=%q, want %q", raw, got, want)
		}
	}
}

func TestCanvasHTMLPreservesDocumentStructure(t *testing.T) {
	raw := `<html><head><title>Export</title></head><body><h1>Notes</h1><p>Hello <strong>team</strong> &amp; friends. <a href="https://example.com">Docs</a></p><ul><li><input type="checkbox" checked>Done</li><li>Next</li></ul><blockquote>Quoted</blockquote><pre><code> a &lt; b\n&#96;&#96;&#96;&#96;\n</code></pre><table><tr><th>Name</th><th>Value</th></tr><tr><td>one</td><td>two</td></tr></table></body></html>`
	got, warnings, err := canvasHTMLToMarkdown(raw)
	if err != nil || len(warnings) > 0 {
		t.Fatalf("conversion warnings=%v err=%v", warnings, err)
	}
	for _, want := range []string{"# Notes", "**team**", "& friends", "[Docs](<https://example.com>)", "- [x] Done", "- Next", "> Quoted", "a < b", "`````", "| Name | Value |", "| --- | --- |"} {
		if !strings.Contains(got, want) {
			t.Errorf("Markdown missing %q:\n%s", want, got)
		}
	}
}
func TestCanvasHTMLFlagsLossyContent(t *testing.T) {
	for _, raw := range []string{`<p>Text</p><iframe src="https://example.com"></iframe>`, `<p><img src="image.png" alt="Diagram"></p>`, `<span class="slack-unfurl">linked doc</span>`} {
		_, warnings, err := canvasHTMLToMarkdown(raw)
		if err != nil || len(warnings) == 0 {
			t.Errorf("rich content must be marked non-editable: %q warnings=%v error=%v", raw, warnings, err)
		}
	}
	if _, _, err := canvasHTMLToMarkdown(`<html><body><form>Sign in</form></body></html>`); err == nil {
		t.Error("sign-in export should fail")
	}
}

func TestCanvasDiscoveryReadAndMutations(t *testing.T) {
	var mu sync.Mutex
	raw := `<h1 id="SECTION1">General notes</h1><p>Decisions and references</p>`
	var mutations []map[string]any
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer canvas-test" {
			t.Errorf("request %s lacks authenticated token", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/conversations.info":
			io.WriteString(w, `{"ok":true,"channel":{"properties":{"canvas":{"file_id":"F1"}}}}`)
		case "/api/files.info":
			fmt.Fprintf(w, `{"ok":true,"file":{"id":"F1","title":"Notes","mimetype":"application/vnd.slack-docs","url_private_download":%q,"permalink":"https://example.com/notes"}}`, server.URL+"/download")
		case "/api/files.list":
			if r.URL.Query().Get("page") == "1" {
				io.WriteString(w, `{"ok":true,"files":[{"id":"F1","mimetype":"application/vnd.slack-docs"},{"id":"PDF","mimetype":"application/pdf"}],"paging":{"pages":2}}`)
			} else {
				io.WriteString(w, `{"ok":true,"files":[{"id":"F2","title":"Other","mimetype":"application/vnd.slack-docs"}],"paging":{"pages":2}}`)
			}
		case "/download":
			mu.Lock()
			body := raw
			mu.Unlock()
			io.WriteString(w, body)
		case "/api/canvases.sections.lookup":
			io.WriteString(w, `{"ok":true,"sections":[{"id":"SECTION1"}]}`)
		default:
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("invalid mutation payload: %v", err)
			}
			payload["method"] = r.URL.Path
			mu.Lock()
			mutations = append(mutations, payload)
			mu.Unlock()
			if r.URL.Path == "/api/canvases.create" || r.URL.Path == "/api/conversations.canvases.create" {
				io.WriteString(w, `{"ok":true,"canvas_id":"FNEW"}`)
			} else {
				io.WriteString(w, `{"ok":true}`)
			}
		}
	}))
	defer server.Close()
	s := NewSlack("canvas-test")
	s.canvasAPIURL = server.URL + "/api/"
	list, err := s.Canvases("C1")
	if err != nil || len(list) != 2 || list[0].ID != "F1" || list[1].ID != "F2" {
		t.Fatalf("discovery %+v, %v", list, err)
	}
	doc, err := s.ReadCanvas("F1")
	if err != nil || !doc.Editable || !strings.Contains(doc.Markdown, "General notes") {
		t.Fatalf("read %+v, %v", doc, err)
	}
	mu.Lock()
	count := len(mutations)
	mu.Unlock()
	if count != 0 {
		t.Fatal("browsing or reading mutated Slack")
	}
	if err = s.ChangeCanvas("F1", data.CanvasChange{Operation: "replace", Markdown: "new", ExpectedRevision: "stale"}); err == nil {
		t.Fatal("stale document replacement should fail")
	}
	mu.Lock()
	count = len(mutations)
	mu.Unlock()
	if count != 0 {
		t.Fatal("stale replacement reached canvases.edit")
	}
	if err = s.ChangeCanvas("F1", data.CanvasChange{Operation: "replace", Markdown: "# Revised", ExpectedRevision: doc.Revision}); err != nil {
		t.Fatal(err)
	}
	if err = s.ChangeCanvas("F1", data.CanvasChange{Operation: "rename", Markdown: "New title"}); err != nil {
		t.Fatal(err)
	}
	if err = s.ChangeCanvas("F1", data.CanvasChange{Operation: "delete", SectionID: "SECTION1"}); err != nil {
		t.Fatal(err)
	}
	if err = s.ChangeCanvas("F1", data.CanvasChange{Operation: "insert_after", SectionID: "SECTION1", Markdown: "General content"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateCanvas("", "Standalone", "# Document"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateCanvas("C2", "Channel notes", "# Notes"); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.CanvasSections("F1", "notes"); err != nil || len(ids) != 1 || ids[0].ID != "SECTION1" || !ids[0].Editable || !strings.Contains(ids[0].Markdown, "General notes") {
		t.Fatalf("sections=%v, %v", ids, err)
	}
	if err = s.CanvasAccess("F1", "U123", "write"); err != nil {
		t.Fatal(err)
	}
	if err = s.CanvasAccess("F1", "C123", "remove"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteCanvas("F1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	rename := mutations[1]["changes"].([]any)[0].(map[string]any)
	if rename["title_content"] == nil || rename["document_content"] != nil {
		t.Errorf("rename payload %+v", rename)
	}
	deletion := mutations[2]["changes"].([]any)[0].(map[string]any)
	if deletion["section_id"] != "SECTION1" || deletion["document_content"] != nil {
		t.Errorf("section delete payload %+v", deletion)
	}
}

func TestCanvasRichContentCannotBeReplaced(t *testing.T) {
	var server *httptest.Server
	var edits atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files.info":
			fmt.Fprintf(w, `{"ok":true,"file":{"id":"F1","mimetype":"application/vnd.slack-docs","url_private_download":%q}}`, server.URL+"/export")
		case "/export":
			io.WriteString(w, `<p>Notes</p><iframe>Embed</iframe>`)
		default:
			edits.Add(1)
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer server.Close()
	s := NewSlack("test")
	s.canvasAPIURL = server.URL + "/api/"
	doc, err := s.ReadCanvas("F1")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ChangeCanvas("F1", data.CanvasChange{Operation: "replace", Markdown: "replacement", ExpectedRevision: doc.Revision}); err == nil || edits.Load() != 0 {
		t.Fatalf("rich replacement must fail without writes: edits=%d, err=%v", edits.Load(), err)
	}
	if err = s.ChangeCanvas("F1", data.CanvasChange{Operation: "insert_at_end", Markdown: "New paragraph"}); err != nil || edits.Load() != 1 {
		t.Fatalf("append should preserve existing content: edits=%d, err=%v", edits.Load(), err)
	}
}
