package app

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/source"
)

func canvasTestKey(m Model, key string) (Model, tea.Cmd) {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	switch key {
	case "enter":
		msg.Type = tea.KeyEnter
	case "esc":
		msg.Type = tea.KeyEsc
	case "ctrl+s":
		msg.Type = tea.KeyCtrlS
	case "ctrl+k":
		msg.Type = tea.KeyCtrlK
	}
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}
func canvasTestRun(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected Canvas command")
	}
	next, follow := m.Update(cmd())
	m = next.(Model)
	if follow != nil {
		next, _ = m.Update(follow())
		m = next.(Model)
	}
	return m
}
func canvasTestDocument(t *testing.T) (Model, *source.Mock, data.Canvas) {
	t.Helper()
	m := newSized()
	mock := m.src.(*source.Mock)
	c, err := mock.CreateCanvas(m.activeID, "Design notes", "# Design notes\n\nDecision: keep things simple.")
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := canvasTestKey(m, "B")
	m = canvasTestRun(t, m, cmd)
	if len(m.canvas.items) != 1 {
		t.Fatalf("browser items=%+v", m.canvas.items)
	}
	m, cmd = canvasTestKey(m, "enter")
	m = canvasTestRun(t, m, cmd)
	if m.canvas.doc == nil || m.canvas.doc.ID != c.ID {
		t.Fatal("Enter did not open selected Canvas")
	}
	return m, mock, c
}

func TestCanvasReadEditAndPublish(t *testing.T) {
	m, mock, c := canvasTestDocument(t)
	if frame := ansi.Strip(m.View()); !strings.Contains(frame, "keep things simple") || !strings.Contains(frame, "Design notes") {
		t.Fatalf("Canvas content not rendered:\n%s", frame)
	}
	docBefore, _ := mock.ReadCanvas(c.ID)
	m.canvas.change = data.CanvasChange{Operation: "replace", ExpectedRevision: m.canvas.doc.Revision}
	draft := "# Design notes\n\nUpdated decision."
	next, _ := m.Update(canvasEditorResult{generation: m.canvas.generation, text: draft})
	m = next.(Model)
	unchanged, _ := mock.ReadCanvas(c.ID)
	if unchanged.Markdown != docBefore.Markdown || m.canvas.draft == nil {
		t.Fatal("returning from editor must create a local draft without publishing")
	}
	m, cmd := canvasTestKey(m, "ctrl+s")
	if !m.canvas.busy {
		t.Fatal("publishing should lock edits while request runs")
	}
	m = canvasTestRun(t, m, cmd)
	updated, err := mock.ReadCanvas(c.ID)
	if err != nil || updated.Markdown != draft || m.canvas.draft != nil || m.canvas.err != "" {
		t.Fatalf("publish state doc=%+v draft=%v error=%q", updated, m.canvas.draft, m.canvas.err)
	}
}

func TestCanvasConflictRetainsDraft(t *testing.T) {
	m, mock, c := canvasTestDocument(t)
	originalRevision := m.canvas.doc.Revision
	if err := mock.ChangeCanvas(c.ID, data.CanvasChange{Operation: "replace", ExpectedRevision: originalRevision, Markdown: "Another teammate's edit"}); err != nil {
		t.Fatal(err)
	}
	draft := "My local draft"
	m.canvas.draft = &draft
	m.canvas.change = data.CanvasChange{Operation: "replace", ExpectedRevision: originalRevision}
	m, cmd := canvasTestKey(m, "ctrl+s")
	m = canvasTestRun(t, m, cmd)
	stored, _ := mock.ReadCanvas(c.ID)
	if m.canvas.draft == nil || *m.canvas.draft != draft || m.canvas.busy || m.canvas.err == "" || stored.Markdown != "Another teammate's edit" {
		t.Fatalf("conflict should preserve both remote and draft: remote=%q draft=%v error=%q", stored.Markdown, m.canvas.draft, m.canvas.err)
	}
}

func TestCanvasBrowsingDoesNotCreateAndIgnoresStaleResults(t *testing.T) {
	m := newSized()
	mock := m.src.(*source.Mock)
	m, cmd := canvasTestKey(m, "B")
	generation, request := m.canvas.generation, m.canvas.request
	m = canvasTestRun(t, m, cmd)
	list, _ := mock.Canvases(m.activeID)
	if len(list) != 0 {
		t.Fatal("opening browser must not create a Canvas")
	}
	m, _ = canvasTestKey(m, "esc")
	m, cmd = canvasTestKey(m, "B")
	m = canvasTestRun(t, m, cmd)
	next, _ := m.Update(canvasResult{generation: generation, request: request, kind: "list", items: []data.Canvas{{ID: "FSTALE", Title: "Old response"}}})
	m = next.(Model)
	if len(m.canvas.items) != 0 {
		t.Fatal("stale Canvas results overwrote current view")
	}
}

func TestCanvasDraftSurvivesPaletteAndDeleteConfirmationIsVisible(t *testing.T) {
	m, _, _ := canvasTestDocument(t)
	m, _ = canvasTestKey(m, "D")
	if !m.confirm.open || !strings.Contains(ansi.Strip(m.View()), "Permanently delete this Canvas?") {
		t.Fatal("Canvas deletion confirmation must be visible above the reader")
	}
	m, _ = canvasTestKey(m, "esc")
	draft := "Unpublished content"
	m.canvas.draft = &draft
	m, _ = canvasTestKey(m, "ctrl+k")
	m, _ = canvasTestKey(m, "esc")
	m, _ = canvasTestKey(m, "B")
	if !m.canvas.open || m.canvas.draft == nil || *m.canvas.draft != draft {
		t.Fatal("palette navigation should retain and resume Canvas draft")
	}
}

func TestCanvasCommandsAreGeneral(t *testing.T) {
	for _, command := range []string{"/canvas", "/canvases"} {
		m := newSized()
		cmd, handled := m.runSlash(command)
		if !handled || !m.canvas.open || cmd == nil {
			t.Fatalf("%s did not open Canvas browser", command)
		}
		if command == "/canvases" && m.canvas.channelID != "" {
			t.Fatal("/canvases should browse workspace documents")
		}
	}
	m := newSized()
	cmd := m.runPalette("cmd:canvases")
	if cmd == nil || !m.canvas.open || m.canvas.channelID != "" {
		t.Fatal("Canvases palette action should browse workspace documents")
	}
}

func TestCanvasPublishCannotLoseItsResultThroughNavigation(t *testing.T) {
	m, _, _ := canvasTestDocument(t)
	draft := "A published update"
	m.canvas.draft = &draft
	m.canvas.change = data.CanvasChange{Operation: "replace", ExpectedRevision: m.canvas.doc.Revision}
	m, cmd := canvasTestKey(m, "ctrl+s")
	m, _ = canvasTestKey(m, "ctrl+k")
	if m.paletteOpen || !m.canvas.open {
		t.Fatal("publishing must finish before navigating away with its draft")
	}
	m, _ = canvasTestKey(m, "esc")
	if !m.canvas.open {
		t.Fatal("Esc must not discard an in-flight publication result")
	}
	m = canvasTestRun(t, m, cmd)
	if m.canvas.draft != nil || m.canvas.busy {
		t.Fatal("publication did not clear busy/draft state")
	}
}

func TestCanvasCreationPublishesOnlyOnSave(t *testing.T) {
	m := newSized()
	m, cmd := canvasTestKey(m, "B")
	m = canvasTestRun(t, m, cmd)
	m.canvas.createTitle = "Meeting notes"
	next, _ := m.Update(canvasEditorResult{generation: m.canvas.generation, text: "# Meeting notes\n\nDecisions"})
	m = next.(Model)
	items, _ := m.src.Canvases("")
	if len(items) != 0 {
		t.Fatal("a new Canvas draft must not create a remote document")
	}
	m, cmd = canvasTestKey(m, "ctrl+s")
	m = canvasTestRun(t, m, cmd)
	items, _ = m.src.Canvases("")
	if len(items) != 1 || items[0].Title != "Meeting notes" || m.canvas.doc == nil || m.canvas.draft != nil {
		t.Fatalf("created Canvas state %+v", items)
	}
}

func TestCanvasUnresolvedMergeCannotPublish(t *testing.T) {
	m, mock, c := canvasTestDocument(t)
	draft := "<<<<<<< Your draft\nMine\n=======\nTheirs\n>>>>>>> Latest Canvas"
	m.canvas.mergePending = true
	m.canvas.draft = &draft
	m, cmd := canvasTestKey(m, "ctrl+s")
	if cmd != nil || m.canvas.err == "" {
		t.Fatal("unresolved merge must be blocked before sending an API request")
	}
	stored, _ := mock.ReadCanvas(c.ID)
	if stored.Markdown == draft {
		t.Fatal("merge markers were published")
	}
}

func TestCanvasDirectOpenReturnsToLoadedBrowser(t *testing.T) {
	m := newSized()
	c, err := m.src.CreateCanvas("", "Notes", "Directly opened document")
	if err != nil {
		t.Fatal(err)
	}
	cmd, handled := m.runSlash("/canvas " + c.ID)
	if !handled {
		t.Fatal("direct Canvas command was not handled")
	}
	m = canvasTestRun(t, m, cmd)
	m, cmd = canvasTestKey(m, "esc")
	m = canvasTestRun(t, m, cmd)
	if m.canvas.doc != nil || len(m.canvas.items) != 1 {
		t.Fatal("back from direct open should load the workspace browser")
	}
}
