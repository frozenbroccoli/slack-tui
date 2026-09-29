package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/theme"
)

// SideItem is one flat sidebar entry: a section header or a conversation row.
// The flat list (headers interleaved with rows) matches the prototype so the
// keyboard cursor index lines up with what's drawn.
type SideItem struct {
	Header bool
	Label  string
	Conv   data.Conversation
}

// BuildSideItems flattens channels and DMs into the rendered list, applying live
// unread/mention meta. meta maps conversation id → {unread, mention}.
// Conversations whose ID appears in hidden (value true) are omitted; section
// headers are always included.
func BuildSideItems(ws *data.Workspace, meta map[string]Meta, hidden map[string]bool) []SideItem {
	items := make([]SideItem, 0, len(ws.Channels)+len(ws.DMs)+2)
	appendSection := func(label string, conversations []data.Conversation) {
		items = append(items, SideItem{Header: true, Label: label})
		start := len(items)
		var counts [3]int
		rank := func(c data.Conversation) int {
			if c.Mention {
				return 0
			}
			if c.Unread > 0 {
				return 1
			}
			return 2
		}
		for _, c := range conversations {
			if !hidden[c.ID] {
				counts[rank(applyMeta(c, meta))]++
			}
		}
		items = append(items, make([]SideItem, counts[0]+counts[1]+counts[2])...)
		positions := [3]int{start, start + counts[0], start + counts[0] + counts[1]}
		for _, c := range conversations {
			if hidden[c.ID] {
				continue
			}
			c = applyMeta(c, meta)
			r := rank(c)
			items[positions[r]] = SideItem{Conv: c}
			positions[r]++
		}
	}
	appendSection("── channels ──", ws.Channels)
	appendSection("── direct messages ──", ws.DMs)
	return items
}

// Meta is the per-conversation unread/mention state the app mutates.
type Meta struct {
	Unread  int
	Mention bool
}

func applyMeta(c data.Conversation, meta map[string]Meta) data.Conversation {
	if m, ok := meta[c.ID]; ok {
		c.Unread = m.Unread
		c.Mention = m.Mention
	}
	return c
}

// SelectableIndexes returns the flat-list indexes that are rows (not headers).
func SelectableIndexes(items []SideItem) []int {
	var idx []int
	for i, it := range items {
		if !it.Header {
			idx = append(idx, i)
		}
	}
	return idx
}

// SidebarBody renders the sidebar list to a body string. selIndex is a flat-list
// index; the cursor highlight only shows when focused.
// SidebarBody renders the sidebar to lines plus the line index of the cursor row
// (so the caller can scroll it into view).
func SidebarBody(p theme.Palette, ws *data.Workspace, items []SideItem, activeID string, selIndex int, focused bool, innerW int) (lines []string, cursorLine int) {
	header := lipgloss.NewStyle().Foreground(p.Dim2)
	lines = append(lines, "") // top breathing room
	for i, it := range items {
		if it.Header {
			lines = append(lines, " "+header.Render(it.Label))
			continue
		}
		if i == selIndex {
			cursorLine = len(lines)
		}
		lines = append(lines, sideRow(p, ws, it.Conv, i == selIndex && focused, it.Conv.ID == activeID, innerW))
	}
	return lines, cursorLine
}

// sideRow renders one conversation row. The active/cursor highlight is inset one
// column from each pane edge (a contained block, not a full-bleed band); the
// cursor adds an accent bar in the left margin.
func sideRow(p theme.Palette, ws *data.Workspace, c data.Conversation, cursor, active bool, w int) string {
	unread := c.Unread > 0
	nameColor := p.Dim
	if unread || active || cursor {
		nameColor = p.Fg
	}
	sigilColor := p.Dim2
	if unread || active {
		sigilColor = p.Dim
	}
	var sigil string
	if c.Type == "channel" {
		sigil = lipgloss.NewStyle().Foreground(sigilColor).Bold(true).Render("#")
	} else {
		sigil = PresenceDot(p, ws.Users[c.UserID].Status)
	}
	name := lipgloss.NewStyle().Foreground(nameColor).Bold(unread || active).Render(c.Name)

	var badge string
	switch {
	case c.Mention:
		badge = lipgloss.NewStyle().Foreground(p.Bg).Background(p.Orange).Render(fmt.Sprintf(" @%d ", c.Unread))
	case unread:
		badge = lipgloss.NewStyle().Foreground(p.Fg).Background(p.BadgeBg).Render(fmt.Sprintf(" %d ", c.Unread))
	}

	region := w - 2 // 1-col margin on each side
	lead := " " + sigil + " " + name
	gap := region - lipgloss.Width(lead) - lipgloss.Width(badge) - 1
	if gap < 1 {
		gap = 1
	}
	content := lead + strings.Repeat(" ", gap) + badge + " "

	// Left margin marks state: cursor bar, then an unread dot (orange for a
	// mention, accent otherwise).
	leftMargin, rightMargin := " ", " "
	switch {
	case cursor:
		leftMargin = lipgloss.NewStyle().Foreground(p.Accent).Render("▌")
	case c.Mention:
		leftMargin = lipgloss.NewStyle().Foreground(p.Orange).Bold(true).Render("●")
	case unread:
		leftMargin = lipgloss.NewStyle().Foreground(p.Accent).Bold(true).Render("●")
	}
	if active || cursor {
		content = theme.FillBg(content, region, p.SelBg)
	}
	return leftMargin + content + rightMargin
}
