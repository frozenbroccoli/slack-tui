package source

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kurenn/slack-tui/internal/data"
)

func (m *Mock) Canvases(channelID string) ([]data.Canvas, error) {
	var out []data.Canvas
	for _, doc := range m.canvases {
		if channelID == "" || doc.ChannelID == channelID {
			out = append(out, doc.Canvas)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (m *Mock) ReadCanvas(id string) (data.CanvasDocument, error) {
	doc, ok := m.canvases[id]
	if !ok {
		return data.CanvasDocument{}, fmt.Errorf("Canvas %s not found", id)
	}
	doc.Warnings = append([]string(nil), doc.Warnings...)
	doc.Sections = append([]data.CanvasSection(nil), doc.Sections...)
	return doc, nil
}
func (m *Mock) CreateCanvas(channelID, title, markdown string) (data.Canvas, error) {
	if strings.TrimSpace(title) == "" {
		return data.Canvas{}, fmt.Errorf("Canvas title cannot be empty")
	}
	if channelID != "" {
		for _, doc := range m.canvases {
			if doc.ChannelID == channelID {
				return data.Canvas{}, fmt.Errorf("channel_canvas_already_exists")
			}
		}
	}
	m.canvasSeq++
	c := data.Canvas{ID: fmt.Sprintf("F-CANVAS-%d", m.canvasSeq), Title: title, ChannelID: channelID}
	m.canvases[c.ID] = data.CanvasDocument{Canvas: c, Markdown: markdown, Revision: canvasRevision(markdown), Editable: true}
	return c, nil
}
func (m *Mock) ChangeCanvas(id string, change data.CanvasChange) error {
	doc, err := m.ReadCanvas(id)
	if err != nil {
		return err
	}
	switch change.Operation {
	case "rename":
		doc.Title = change.Markdown
	case "replace":
		if change.SectionID != "" {
			return fmt.Errorf("mock does not model Slack section IDs")
		}
		if !doc.Editable || change.ExpectedRevision == "" || change.ExpectedRevision != doc.Revision {
			return fmt.Errorf("Canvas changed while editing; draft retained")
		}
		doc.Markdown = change.Markdown
	case "insert_at_end":
		doc.Markdown = strings.TrimSpace(doc.Markdown) + "\n\n" + change.Markdown
	case "insert_at_start":
		doc.Markdown = change.Markdown + "\n\n" + doc.Markdown
	default:
		return fmt.Errorf("mock does not model operation %q", change.Operation)
	}
	doc.Revision = canvasRevision(doc.Markdown)
	m.canvases[id] = doc
	return nil
}
func (m *Mock) CanvasSections(id, text string) ([]data.CanvasSection, error) {
	if _, err := m.ReadCanvas(id); err != nil {
		return nil, err
	}
	return nil, nil
}
func (m *Mock) CanvasAccess(id, targetID, level string) error { _, err := m.ReadCanvas(id); return err }
func (m *Mock) DeleteCanvas(id string) error {
	if _, err := m.ReadCanvas(id); err != nil {
		return err
	}
	delete(m.canvases, id)
	return nil
}
