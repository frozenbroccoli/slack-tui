package app

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/kurenn/slack-tui/internal/data"
)

type canvasRenderCache struct {
	markdown string
	width    int
	lines    []string
}

type canvasState struct {
	open, busy                 bool
	generation, request        uint64
	channelID                  string
	items                      []data.Canvas
	selected, scroll           int
	doc                        *data.CanvasDocument
	input                      textinput.Model
	prompt, filter, err        string
	sections                   []data.CanvasSection
	sectionSel                 int
	draft                      *string
	change                     data.CanvasChange
	createTitle, createChannel string
	cache                      *canvasRenderCache
	mergePending               bool
	listLoaded                 bool
}

type canvasResult struct {
	generation, request uint64
	kind                string
	items               []data.Canvas
	doc                 *data.CanvasDocument
	sections            []data.CanvasSection
	applied             bool
	err                 error
}

type canvasEditorResult struct {
	generation         uint64
	text, recoveryPath string
	err                error
}

func (m *Model) closeCanvas() {
	m.canvas.open = false
	m.canvas.busy = false
	m.canvas.generation++
	m.canvas.input.Blur()
}

func (m *Model) openCanvasBrowser(workspace bool) tea.Cmd {
	if m.canvas.draft != nil {
		m.canvas.open = true
		return nil
	}
	input := m.canvas.input
	generation := m.canvas.generation + 1
	m.canvas = canvasState{open: true, generation: generation, input: input, cache: &canvasRenderCache{}}
	if !workspace {
		if c, ok := m.ws.Conversation(m.activeID); ok {
			m.canvas.channelID = c.ID
		}
	}
	return m.canvasListCmd()
}

func (m *Model) openCanvasReference(reference string) tea.Cmd {
	id := strings.TrimSpace(reference)
	if parsed, err := url.Parse(id); err == nil && parsed.Host != "" {
		id = filepath.Base(parsed.Path)
	}
	if !strings.HasPrefix(id, "F") || strings.ContainsAny(id, " /?\n") {
		return m.flash(fmt.Errorf("use /canvas F123 or a Canvas link"))
	}
	if m.canvas.draft != nil {
		return m.flash(fmt.Errorf("save or discard your Canvas draft first"))
	}
	input := m.canvas.input
	generation := m.canvas.generation + 1
	m.canvas = canvasState{open: true, generation: generation, input: input, cache: &canvasRenderCache{}}
	return m.canvasReadCmd(id)
}

func (m *Model) canvasListCmd() tea.Cmd {
	m.canvas.busy = true
	m.canvas.err = ""
	m.canvas.request++
	generation, request, channel, src := m.canvas.generation, m.canvas.request, m.canvas.channelID, m.src
	return func() tea.Msg {
		items, err := src.Canvases(channel)
		return canvasResult{generation: generation, request: request, kind: "list", items: items, err: err}
	}
}
func (m *Model) canvasReadCmd(id string) tea.Cmd {
	m.canvas.busy = true
	m.canvas.err = ""
	m.canvas.request++
	generation, request, src := m.canvas.generation, m.canvas.request, m.src
	return func() tea.Msg {
		doc, err := src.ReadCanvas(id)
		return canvasResult{generation: generation, request: request, kind: "read", doc: &doc, err: err}
	}
}
func (m *Model) handleCanvasResult(result canvasResult) (tea.Model, tea.Cmd) {
	if !m.canvas.open || result.generation != m.canvas.generation || result.request != m.canvas.request {
		return *m, nil
	}
	m.canvas.busy = false
	if result.applied {
		if result.doc != nil && result.doc.ID != "" {
			m.canvas.doc = result.doc
		}
		m.canvas.draft = nil
		m.canvas.createTitle = ""
		m.canvas.sections = nil
		m.canvas.change = data.CanvasChange{}
		m.canvas.mergePending = false
	}
	if result.err != nil {
		m.canvas.err = result.err.Error()
		return *m, nil
	}
	m.canvas.err = ""
	switch result.kind {
	case "list":
		m.canvas.createTitle = ""
		m.canvas.items = result.items
		m.canvas.listLoaded = true
		m.canvas.doc = nil
		m.canvas.selected = clamp(m.canvas.selected, 0, max(0, len(m.filteredCanvases())-1))
	case "read", "saved":
		m.canvas.createTitle = ""
		m.canvas.doc = result.doc
		m.canvas.sections = nil
		m.canvas.scroll = 0
	case "merge":
		if result.doc == nil || !result.doc.Editable {
			m.canvas.err = "Latest Canvas has rich content; whole-document merging is unavailable"
			return *m, nil
		}
		latest := result.doc
		if m.canvas.draft == nil {
			return *m, nil
		}
		initial := *m.canvas.draft
		if m.canvas.change.Operation == "replace" && m.canvas.change.SectionID == "" {
			if m.canvas.doc != nil && latest.Markdown != m.canvas.doc.Markdown {
				initial = "<<<<<<< Your draft\n" + initial + "\n=======\n" + latest.Markdown + "\n>>>>>>> Latest Canvas\n"
				m.canvas.mergePending = true
			}
			m.canvas.change.ExpectedRevision = latest.Revision
		}
		m.canvas.doc = latest
		cmd := m.canvasEditorCmd(initial)
		return *m, cmd
	case "sections":
		m.canvas.sections = result.sections
		m.canvas.sectionSel = 0
		if len(result.sections) == 0 {
			m.canvas.err = "No matching sections"
		}
	case "deleted":
		m.canvas.doc = nil
		cmd := m.canvasListCmd()
		return *m, cmd
	}
	return *m, nil
}

func (m *Model) canvasPrompt(mode, placeholder string) tea.Cmd {
	m.canvas.prompt = mode
	m.canvas.input.SetValue("")
	m.canvas.input.Placeholder = placeholder
	m.canvas.err = ""
	return m.canvas.input.Focus()
}
func (m *Model) submitCanvasPrompt() tea.Cmd {
	value := strings.TrimSpace(m.canvas.input.Value())
	mode := m.canvas.prompt
	if value == "" && mode != "filter" {
		m.canvas.err = "Enter a value"
		return nil
	}
	switch mode {
	case "filter":
		m.canvas.filter = value
		m.canvas.selected = 0
	case "find":
		lines := m.canvasLines()
		found := false
		for i, line := range lines {
			if strings.Contains(strings.ToLower(ansi.Strip(line)), strings.ToLower(value)) {
				m.canvas.scroll = i
				found = true
				break
			}
		}
		if !found {
			m.canvas.err = "No matching text"
		}
	case "create", "standalone":
		m.canvas.createTitle = value
		m.canvas.createChannel = m.canvas.channelID
		if mode == "standalone" {
			m.canvas.createChannel = ""
		}
		m.canvas.change = data.CanvasChange{}
		m.canvas.prompt = ""
		m.canvas.input.Blur()
		return m.canvasEditorCmd("")
	case "rename":
		m.canvas.prompt = ""
		m.canvas.input.Blur()
		return m.canvasChangeCmd(data.CanvasChange{Operation: "rename", Markdown: value})
	case "sections":
		m.canvas.busy = true
		m.canvas.request++
		src, id, generation, request := m.src, m.canvas.doc.ID, m.canvas.generation, m.canvas.request
		m.canvas.prompt = ""
		m.canvas.input.Blur()
		return func() tea.Msg {
			sections, err := src.CanvasSections(id, value)
			return canvasResult{generation: generation, request: request, kind: "sections", sections: sections, err: err}
		}
	case "access":
		fields := strings.Fields(value)
		if len(fields) != 2 {
			m.canvas.err = "Use @handle or channel ID, followed by read/write/remove"
			return nil
		}
		target := fields[0]
		if strings.HasPrefix(target, "@") {
			handle := strings.TrimPrefix(target, "@")
			target = ""
			for id, user := range m.ws.Users {
				if strings.EqualFold(user.Handle, handle) {
					target = id
					break
				}
			}
			if target == "" {
				m.canvas.err = "Unknown workspace member"
				return nil
			}
		}
		if fields[1] != "read" && fields[1] != "write" && fields[1] != "remove" {
			m.canvas.err = "Access must be read, write, or remove"
			return nil
		}
		m.canvas.busy = true
		m.canvas.request++
		src, id, generation, request, level := m.src, m.canvas.doc.ID, m.canvas.generation, m.canvas.request, fields[1]
		m.canvas.prompt = ""
		m.canvas.input.Blur()
		return func() tea.Msg {
			err := src.CanvasAccess(id, target, level)
			return canvasResult{generation: generation, request: request, kind: "access", err: err}
		}
	}
	m.canvas.prompt = ""
	m.canvas.input.Blur()
	return nil
}

func (m *Model) canvasChangeCmd(change data.CanvasChange) tea.Cmd {
	m.canvas.busy = true
	m.canvas.err = ""
	m.canvas.request++
	src, id, generation, request := m.src, m.canvas.doc.ID, m.canvas.generation, m.canvas.request
	return func() tea.Msg {
		if err := src.ChangeCanvas(id, change); err != nil {
			return canvasResult{generation: generation, request: request, err: err}
		}
		doc, err := src.ReadCanvas(id)
		if err != nil {
			err = fmt.Errorf("Canvas saved, but reload failed: %w", err)
		}
		return canvasResult{generation: generation, request: request, kind: "saved", doc: &doc, applied: true, err: err}
	}
}
func (m *Model) saveCanvasDraft() tea.Cmd {
	if m.canvas.draft == nil {
		return nil
	}
	if m.canvas.mergePending {
		m.canvas.err = "Resolve merge markers in the editor before publishing"
		return nil
	}
	if m.canvas.createTitle == "" {
		change := m.canvas.change
		change.Markdown = *m.canvas.draft
		return m.canvasChangeCmd(change)
	}
	m.canvas.busy = true
	m.canvas.request++
	src, generation, request := m.src, m.canvas.generation, m.canvas.request
	title, channel, markdown := m.canvas.createTitle, m.canvas.createChannel, *m.canvas.draft
	return func() tea.Msg {
		c, err := src.CreateCanvas(channel, title, markdown)
		if err != nil {
			return canvasResult{generation: generation, request: request, applied: c.ID != "", err: err}
		}
		doc, err := src.ReadCanvas(c.ID)
		if err != nil {
			err = fmt.Errorf("Canvas %s created, but reload failed: %w", c.ID, err)
		}
		return canvasResult{generation: generation, request: request, kind: "saved", doc: &doc, applied: true, err: err}
	}
}

func (m *Model) canvasEditorCmd(initial string) tea.Cmd {
	dir, err := os.MkdirTemp("", "slack-tui-canvas-")
	if err != nil {
		m.canvas.err = err.Error()
		return nil
	}
	path := filepath.Join(dir, "canvas.md")
	if err = os.WriteFile(path, []byte(initial), 0600); err != nil {
		os.RemoveAll(dir)
		m.canvas.err = err.Error()
		return nil
	}
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		editor = "vi"
	}
	args := strings.Fields(editor)
	command := exec.Command(args[0], append(args[1:], path)...)
	generation := m.canvas.generation
	m.canvas.busy = true
	return tea.ExecProcess(command, func(editorErr error) tea.Msg {
		result := canvasEditorResult{generation: generation, recoveryPath: path, err: editorErr}
		if editorErr != nil {
			return result
		}
		f, err := os.Open(path)
		if err != nil {
			result.err = err
			return result
		}
		raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		f.Close()
		if err != nil {
			result.err = err
			return result
		}
		if len(raw) > 1<<20 {
			result.err = fmt.Errorf("Canvas Markdown exceeds 1 MiB")
			return result
		}
		result.text = string(raw)
		result.recoveryPath = ""
		os.RemoveAll(dir)
		return result
	})
}
func (m *Model) handleCanvasEditorResult(result canvasEditorResult) (tea.Model, tea.Cmd) {
	if !m.canvas.open || result.generation != m.canvas.generation {
		return *m, nil
	}
	m.canvas.busy = false
	if result.err != nil {
		m.canvas.err = fmt.Sprintf("Editor: %v. Draft file: %s", result.err, result.recoveryPath)
		return *m, nil
	}
	if m.canvas.createTitle == "" && m.canvas.change.Operation == "replace" && m.canvas.change.SectionID == "" && m.canvas.doc != nil && result.text == m.canvas.doc.Markdown {
		m.canvas.draft = nil
		m.canvas.mergePending = false
		return *m, nil
	}
	m.canvas.draft = &result.text
	if m.canvas.mergePending {
		unresolved := false
		for _, line := range strings.Split(result.text, "\n") {
			if strings.HasPrefix(line, "<<<<<<< ") || line == "=======" || strings.HasPrefix(line, ">>>>>>> ") {
				unresolved = true
				break
			}
		}
		m.canvas.mergePending = unresolved
	}
	m.canvas.scroll = 0
	m.canvas.err = ""
	return *m, nil
}

func (m Model) canvasKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.canvas.prompt != "" {
		switch key {
		case "esc":
			m.canvas.prompt = ""
			m.canvas.input.Blur()
			return m, nil
		case "enter":
			cmd := m.submitCanvasPrompt()
			return m, cmd
		default:
			var cmd tea.Cmd
			m.canvas.input, cmd = m.canvas.input.Update(msg)
			return m, cmd
		}
	}
	if key == "?" {
		m.helpOpen = true
		return m, nil
	}
	if key == "ctrl+c" {
		if m.canvas.busy && m.canvas.draft != nil {
			return m, nil
		}
		if m.canvas.draft != nil {
			m.openConfirm("Discard Canvas draft and quit?", func(mm *Model) tea.Cmd { return mm.quit() })
			return m, nil
		}
		return m, m.quit()
	}
	if m.canvas.busy {
		if key == "esc" && m.canvas.draft == nil {
			m.closeCanvas()
		}
		return m, nil
	}
	if m.canvas.draft != nil {
		switch key {
		case "ctrl+s":
			cmd := m.saveCanvasDraft()
			return m, cmd
		case "R":
			if m.canvas.doc == nil {
				m.canvas.err = "New drafts have no remote version"
				return m, nil
			}
			m.canvas.busy = true
			m.canvas.request++
			src, id, generation, request := m.src, m.canvas.doc.ID, m.canvas.generation, m.canvas.request
			cmd := func() tea.Msg {
				doc, err := src.ReadCanvas(id)
				return canvasResult{generation: generation, request: request, kind: "merge", doc: &doc, err: err}
			}
			return m, cmd
		case "e":
			cmd := m.canvasEditorCmd(*m.canvas.draft)
			return m, cmd
		case "esc":
			m.openConfirm("Discard unsaved Canvas draft?", func(mm *Model) tea.Cmd {
				mm.canvas.draft = nil
				mm.canvas.createTitle = ""
				mm.canvas.mergePending = false
				return nil
			})
		case "j", "down":
			m.canvasScroll(1)
		case "k", "up":
			m.canvasScroll(-1)
		}
		return m, nil
	}
	if len(m.canvas.sections) > 0 {
		switch key {
		case "esc":
			m.canvas.sections = nil
		case "j", "down":
			m.canvas.sectionSel = clamp(m.canvas.sectionSel+1, 0, len(m.canvas.sections)-1)
		case "k", "up":
			m.canvas.sectionSel = clamp(m.canvas.sectionSel-1, 0, len(m.canvas.sections)-1)
		case "e", "a", "i":
			operation := "replace"
			if key == "a" {
				operation = "insert_after"
			}
			if key == "i" {
				operation = "insert_before"
			}
			m.canvas.change = data.CanvasChange{Operation: operation, SectionID: m.canvas.sections[m.canvas.sectionSel].ID}
			initial := ""
			section := m.canvas.sections[m.canvas.sectionSel]
			if key == "e" && section.Editable {
				initial = section.Markdown
			}
			cmd := m.canvasEditorCmd(initial)
			return m, cmd
		case "d":
			id := m.canvas.sections[m.canvas.sectionSel].ID
			m.openConfirm("Delete the selected Canvas section?", func(mm *Model) tea.Cmd {
				return mm.canvasChangeCmd(data.CanvasChange{Operation: "delete", SectionID: id})
			})
		}
		return m, nil
	}
	switch key {
	case "?":
		m.helpOpen = true
		return m, nil
	case "esc", "q":
		if m.canvas.doc != nil {
			m.canvas.doc = nil
			m.canvas.scroll = 0
			if !m.canvas.listLoaded {
				cmd := m.canvasListCmd()
				return m, cmd
			}
		} else {
			m.closeCanvas()
		}
	case "w", "c":
		m.canvas.doc = nil
		m.canvas.filter = ""
		m.canvas.channelID = ""
		if key == "c" {
			m.canvas.channelID = m.activeID
		}
		cmd := m.canvasListCmd()
		return m, cmd
	case "R":
		var cmd tea.Cmd
		if m.canvas.doc != nil {
			cmd = m.canvasReadCmd(m.canvas.doc.ID)
		} else {
			cmd = m.canvasListCmd()
		}
		return m, cmd
	case "n", "N":
		mode := "create"
		if key == "N" {
			mode = "standalone"
		}
		cmd := m.canvasPrompt(mode, "Canvas title")
		return m, cmd
	}
	if m.canvas.doc == nil {
		items := m.filteredCanvases()
		switch key {
		case "j", "down":
			m.canvas.selected = clamp(m.canvas.selected+1, 0, max(0, len(items)-1))
		case "k", "up":
			m.canvas.selected = clamp(m.canvas.selected-1, 0, max(0, len(items)-1))
		case "enter":
			if len(items) > 0 {
				cmd := m.canvasReadCmd(items[m.canvas.selected].ID)
				return m, cmd
			}
		case "f", "/":
			cmd := m.canvasPrompt("filter", "filter Canvas titles")
			return m, cmd
		}
	} else {
		switch key {
		case "j", "down":
			m.canvasScroll(1)
		case "k", "up":
			m.canvasScroll(-1)
		case "ctrl+d":
			m.canvasScroll(max(1, (m.height-10)/2))
		case "ctrl+u":
			m.canvasScroll(-max(1, (m.height-10)/2))
		case "g":
			m.canvas.scroll = 0
		case "G":
			m.canvasScroll(len(m.canvasLines()))
		case "/":
			cmd := m.canvasPrompt("find", "find text in Canvas")
			return m, cmd
		case "e":
			if !m.canvas.doc.Editable {
				m.canvas.err = "Rich content cannot be safely replaced as Markdown. Use a to append or s to target a section."
				return m, nil
			}
			m.canvas.change = data.CanvasChange{Operation: "replace", ExpectedRevision: m.canvas.doc.Revision}
			cmd := m.canvasEditorCmd(m.canvas.doc.Markdown)
			return m, cmd
		case "a", "i":
			operation := "insert_at_end"
			if key == "i" {
				operation = "insert_at_start"
			}
			m.canvas.change = data.CanvasChange{Operation: operation}
			cmd := m.canvasEditorCmd("")
			return m, cmd
		case "r":
			cmd := m.canvasPrompt("rename", "new Canvas title")
			return m, cmd
		case "s":
			cmd := m.canvasPrompt("sections", "find sections containing text")
			return m, cmd
		case "p":
			cmd := m.canvasPrompt("access", "@handle or channel ID read/write/remove")
			return m, cmd
		case "o":
			if m.canvas.doc.URL != "" {
				return m, openURLCmd(m.canvas.doc.URL)
			}
		case "D":
			id := m.canvas.doc.ID
			m.openConfirm("Permanently delete this Canvas?", func(mm *Model) tea.Cmd {
				mm.canvas.busy = true
				mm.canvas.request++
				src, generation, request := mm.src, mm.canvas.generation, mm.canvas.request
				return func() tea.Msg {
					err := src.DeleteCanvas(id)
					return canvasResult{generation: generation, request: request, kind: "deleted", err: err}
				}
			})
		}
	}
	return m, nil
}

func (m Model) filteredCanvases() []data.Canvas {
	var out []data.Canvas
	for _, c := range m.canvas.items {
		if strings.Contains(strings.ToLower(c.Title), strings.ToLower(m.canvas.filter)) {
			out = append(out, c)
		}
	}
	return out
}
func (m *Model) canvasLines() []string {
	width := max(10, m.width-12)
	markdown := ""
	if m.canvas.draft != nil {
		markdown = *m.canvas.draft
	} else if m.canvas.doc != nil {
		markdown = m.canvas.doc.Markdown
	}
	if m.canvas.cache == nil {
		m.canvas.cache = &canvasRenderCache{}
	}
	if m.canvas.cache.width == width && m.canvas.cache.markdown == markdown && m.canvas.cache.lines != nil {
		return m.canvas.cache.lines
	}
	rendered := markdown
	if r, err := glamour.NewTermRenderer(glamour.WithStandardStyle("dark"), glamour.WithWordWrap(width)); err == nil {
		if out, err := r.Render(markdown); err == nil {
			rendered = out
		}
	}
	m.canvas.cache.markdown = markdown
	m.canvas.cache.width = width
	m.canvas.cache.lines = strings.Split(rendered, "\n")
	return m.canvas.cache.lines
}

func (m Model) overlayCanvas(frame string) string {
	width, height := max(18, m.width-4), max(8, m.height-2)
	inner := max(12, width-6)
	rows := max(1, height-9)
	title := "Canvases · workspace"
	if c, ok := m.ws.Conversation(m.canvas.channelID); ok {
		title = "Canvases · " + c.Name
	}
	if m.canvas.doc != nil {
		title = m.canvas.doc.Title + " · " + m.canvas.doc.ID
	}
	if m.canvas.createTitle != "" {
		title = "New Canvas · " + m.canvas.createTitle
	}
	lines := []string{lipgloss.NewStyle().Foreground(m.pal.Accent).Bold(true).Render(ansi.Truncate(title, inner, "…")), ""}
	footer := "j/k select · enter open · / filter · n new · w workspace · ? keys"
	if m.canvas.doc != nil || m.canvas.draft != nil {
		content := m.canvasLines()
		start := clamp(m.canvas.scroll, 0, max(0, len(content)-rows))
		end := min(len(content), start+rows)
		for _, line := range content[start:end] {
			lines = append(lines, ansi.Truncate(line, inner, "…"))
		}
		footer = "j/k scroll · e edit · a append · s sections · ? keys"
		if m.canvas.doc != nil && !m.canvas.doc.Editable {
			lines = append(lines, lipgloss.NewStyle().Foreground(m.pal.Dim).Render("Rich content: "+ansi.Truncate(strings.Join(m.canvas.doc.Warnings, ", "), inner-14, "…")))
		}
	} else {
		items := m.filteredCanvases()
		start := clamp(m.canvas.selected-rows+1, 0, max(0, len(items)-rows))
		end := min(len(items), start+rows)
		if len(items) == 0 {
			lines = append(lines, "No Canvases here. Press n to create one, or w to browse the workspace.")
		}
		for i := start; i < end; i++ {
			marker := "  "
			if i == m.canvas.selected {
				marker = "› "
			}
			lines = append(lines, ansi.Truncate(marker+items[i].Title+"  "+items[i].ID, inner, "…"))
		}
	}
	if len(m.canvas.sections) > 0 {
		lines = lines[:2]
		start := clamp(m.canvas.sectionSel-rows+1, 0, max(0, len(m.canvas.sections)-rows))
		end := min(len(m.canvas.sections), start+rows)
		for i := start; i < end; i++ {
			marker := "  "
			if i == m.canvas.sectionSel {
				marker = "› "
			}
			label := fmt.Sprintf("Section %d · preview unavailable", i+1)
			if text := strings.Join(strings.Fields(m.canvas.sections[i].Markdown), " "); text != "" {
				label = text
			}
			lines = append(lines, ansi.Truncate(marker+label, inner, "…"))
		}
		footer = "j/k select · e replace section · a insert after · i insert before · d delete section · esc back"
	}
	if m.canvas.draft != nil {
		footer = "Unsaved draft · Ctrl+S publish · e edit · R merge latest · esc discard"
		if m.canvas.change.SectionID != "" {
			footer += " · " + m.canvas.change.Operation + " section"
		}
	}
	for len(lines) < rows+2 {
		lines = append(lines, "")
	}
	if m.canvas.busy {
		lines = append(lines, "Loading / saving Canvas…")
	} else if m.canvas.prompt != "" {
		m.canvas.input.Width = inner
		lines = append(lines, m.canvas.input.View())
	} else {
		lines = append(lines, ansi.Truncate(footer, inner, "…"))
	}
	if m.canvas.err != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(m.pal.Red).Render(ansi.Truncate(m.canvas.err, inner, "…")))
	}
	lines = append(lines, lipgloss.NewStyle().Foreground(m.pal.Dim2).Render("R refresh · esc back · /canvas ID opens a document directly"))
	panel := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(m.pal.Border).Background(m.pal.Panel).Width(width-2).Padding(0, 1).Render(strings.Join(lines, "\n"))
	return overlay(frame, panel, 2, 1)
}

func (m *Model) canvasScroll(delta int) {
	rows := max(1, m.height-11)
	m.canvas.scroll = clamp(m.canvas.scroll+delta, 0, max(0, len(m.canvasLines())-rows))
}
