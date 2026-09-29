package app

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kurenn/slack-tui/internal/theme"
)

// Help overlay — the full keymap cheat sheet (`?` to open).

type helpRow struct{ keys, desc string }

var helpRows = []helpRow{
	{"j / k · ↓ / ↑", "move · scroll tall messages"},
	{"gg / G", "jump to top / bottom"},
	{"ctrl+d / ctrl+u", "half-page scroll"},
	{"tab · h / l · ← / →", "switch panes"},
	{"enter / t", "open thread"},
	{"i", "write message"},
	{"r", "reply in thread"},
	{"alt+enter", "newline in the composer"},
	{"a", "react to message"},
	{"e", "edit your message"},
	{"dd", "delete your message"},
	{"o", "open message links"},
	{"A", "attach file · or drag-drop"},
	{"/ · n / N", "find in channel · next/prev"},
	{"s", "search the workspace"},
	{"T", "threads inbox"},
	{"B · /canvas", "browse and edit channel Canvases"},
	{"y", "yank message to clipboard"},
	{"S", "save file(s) to ~/Downloads"},
	{"drag", "select text · copies on release"},
	{"drag divider", "resize the thread pane"},
	{"] / [", "next / prev unread"},
	{"x", "hide · un-hide conversation"},
	{"/cmd", "slash commands · /shrug /dm /away …"},
	{"ctrl+k", "command palette"},
	{",", "settings"},
	{"ctrl+r", "refresh"},
	{"esc", "close · dismiss"},
	{"q", "quit"},
}

func (m Model) helpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, m.quit()
	case "esc", "q", "?", "enter":
		m.helpOpen = false
	}
	return m, nil
}

// overlayHelp composites the keymap card over the frame.
func (m Model) overlayHelp(frame string) string {
	p := m.pal
	w := 52
	inner := w - 4

	keyStyle := lipgloss.NewStyle().Foreground(p.Accent).Width(17)
	descStyle := lipgloss.NewStyle().Foreground(p.Fg)

	body := []string{
		theme.FillBg(lipgloss.NewStyle().Foreground(p.Fg).Bold(true).Render(" keys"), inner, p.Panel),
		theme.FillBg("", inner, p.Panel),
	}
	rows := helpRows
	if m.canvas.open {
		rows = canvasHelpRows
	}
	for _, r := range rows {
		line := " " + keyStyle.Render(r.keys) + descStyle.Render(r.desc)
		body = append(body, theme.FillBg(line, inner, p.Panel))
	}
	body = append(body,
		theme.FillBg("", inner, p.Panel),
		theme.FillBg(lipgloss.NewStyle().Foreground(p.Dim2).Render(" esc close"), inner, p.Panel),
	)

	bs := lipgloss.NewStyle().Foreground(p.Accent)
	box := []string{bs.Render("┌" + strings.Repeat("─", w-2) + "┐")}
	for _, l := range body {
		box = append(box, bs.Render("│")+" "+l+" "+bs.Render("│"))
	}
	box = append(box, bs.Render("└"+strings.Repeat("─", w-2)+"┘"))

	x := (m.width - w) / 2
	y := (m.height - len(box)) / 2
	if y < 1 {
		y = 1
	}
	return overlay(frame, strings.Join(box, "\n"), x, y)
}

var canvasHelpRows = []helpRow{
	{"j/k · ctrl+d/u", "select or scroll"},
	{"g / G", "document top / bottom"},
	{"enter · /", "open · filter or find"},
	{"w / c", "workspace / channel browser"},
	{"n / N", "new channel / standalone Canvas"},
	{"e", "edit in terminal editor"},
	{"a / i", "append / prepend content"},
	{"ctrl+s", "publish draft preview"},
	{"r", "rename Canvas"},
	{"s", "find sections by text"},
	{"section e/a/i", "replace / insert after / before"},
	{"section d", "delete selected section"},
	{"p", "adjust existing access"},
	{"o", "open in Slack browser"},
	{"D", "delete whole Canvas"},
	{"R · esc", "refresh · back / discard draft"},
}
