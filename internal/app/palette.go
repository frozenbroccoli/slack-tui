package app

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/theme"
	"github.com/kurenn/slack-tui/internal/ui/components"
)

const paletteMaxRows = 8

// palItem is a palette entry plus the action id used to run it.
type palItem struct {
	id   string
	item components.PaletteItem
}

// paletteItems builds the full (unfiltered) command list: jump to any
// channel/DM — most recently opened first — then the commands.
func (m Model) paletteItems() []palItem {
	var items []palItem
	convItem := func(c data.Conversation) palItem {
		if c.Type == "dm" {
			return palItem{"dm:" + c.ID, components.PaletteItem{Icon: "•", Label: c.Name, Hint: "direct message", Kind: "dm"}}
		}
		return palItem{"ch:" + c.ID, components.PaletteItem{Icon: "#", Label: c.Name, Hint: "channel", Kind: "channel"}}
	}
	listed := map[string]bool{}
	for _, id := range m.recent {
		if c, ok := m.ws.Conversation(id); ok && !listed[id] {
			listed[id] = true
			items = append(items, convItem(c))
		}
	}
	for _, c := range append(append([]data.Conversation{}, m.ws.Channels...), m.ws.DMs...) {
		if !listed[c.ID] {
			items = append(items, convItem(c))
		}
	}
	hintsLabel := "Show key hints"
	if m.showHints {
		hintsLabel = "Hide key hints"
	}
	if len(m.workspaces) > 1 {
		items = append(items,
			palItem{"cmd:workspace", components.PaletteItem{Icon: "⇄", Label: "Switch workspace", Hint: m.activeWorkspace, Kind: "cmd"}})
	}
	items = append(items,
		palItem{"cmd:canvases", components.PaletteItem{Icon: "☷", Label: "Canvases", Hint: "browse workspace documents", Kind: "cmd"}},
		palItem{"cmd:join", components.PaletteItem{Icon: "+", Label: "Browse & join channels", Hint: "channels", Kind: "cmd"}},
		palItem{"cmd:settings", components.PaletteItem{Icon: "⚙", Label: "Settings", Hint: "appearance · presence", Kind: "cmd"}},
		palItem{"cmd:statustext", components.PaletteItem{Icon: "✎", Label: "Set status message", Hint: "presence", Kind: "cmd"}},
		palItem{"cmd:theme", components.PaletteItem{Icon: "▣", Label: "Cycle theme", Hint: "appearance", Kind: "cmd"}},
		palItem{"cmd:hints", components.PaletteItem{Icon: "⌨", Label: hintsLabel, Hint: "status bar", Kind: "cmd"}},
		palItem{"cmd:active", components.PaletteItem{Icon: "●", Label: "Set status: active", Hint: "presence", Kind: "cmd"}},
		palItem{"cmd:away", components.PaletteItem{Icon: "○", Label: "Set status: away", Hint: "presence", Kind: "cmd"}},
		palItem{"cmd:dnd", components.PaletteItem{Icon: "◓", Label: "Set status: do not disturb", Hint: "presence", Kind: "cmd"}},
		palItem{"cmd:read", components.PaletteItem{Icon: "✓", Label: "Mark all as read", Hint: "inbox", Kind: "cmd"}},
	)
	hideLabel := "Hide this conversation"
	if _, ok := m.hidden[m.activeID]; ok {
		hideLabel = "Un-hide this conversation"
	}
	items = append(items,
		palItem{"cmd:hide", components.PaletteItem{Icon: "⊘", Label: hideLabel, Hint: "sidebar", Kind: "cmd"}},
	)
	return items
}

// filteredPalette applies the query (leading #@:/ stripped). Substring matches
// on label+hint rank first; fuzzy subsequence matches follow, so "bcknd" still
// finds #backend.
func (m Model) filteredPalette() []palItem {
	q := strings.ToLower(strings.TrimSpace(m.paletteQuery.Value()))
	q = strings.TrimLeft(q, "#@:/")
	all := m.paletteItems()
	if q == "" {
		return all
	}
	var subs, fuzzy []palItem
	for _, it := range all {
		hay := strings.ToLower(it.item.Label + " " + it.item.Hint)
		switch {
		case strings.Contains(hay, q):
			subs = append(subs, it)
		case fuzzyMatch(hay, q):
			fuzzy = append(fuzzy, it)
		}
	}
	return append(subs, fuzzy...)
}

// fuzzyMatch reports whether q is a subsequence of hay (both lowercased).
func fuzzyMatch(hay, q string) bool {
	j := 0
	for i := 0; i < len(hay) && j < len(q); i++ {
		if hay[i] == q[j] {
			j++
		}
	}
	return j == len(q)
}

func (m *Model) openPalette() tea.Cmd {
	m.paletteOpen = true
	m.paletteQuery.SetValue("")
	m.paletteIndex = 0
	return m.paletteQuery.Focus()
}

func (m *Model) closePalette() {
	m.paletteOpen = false
	m.paletteQuery.Blur()
}

// paletteKey handles input while the palette is open.
func (m Model) paletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch {
	case k == "esc":
		m.closePalette()
		return m, nil
	case k == "down", k == "ctrl+n", k == "ctrl+j":
		m.paletteIndex = clamp(m.paletteIndex+1, 0, len(m.filteredPalette())-1)
		return m, nil
	case k == "up", k == "ctrl+p":
		m.paletteIndex = clamp(m.paletteIndex-1, 0, len(m.filteredPalette())-1)
		return m, nil
	case k == "enter":
		items := m.filteredPalette()
		var cmd tea.Cmd
		if m.paletteIndex < len(items) {
			cmd = m.runPalette(items[m.paletteIndex].id)
		}
		m.closePalette()
		return m, cmd
	}
	var cmd tea.Cmd
	m.paletteQuery, cmd = m.paletteQuery.Update(msg)
	m.paletteIndex = clamp(m.paletteIndex, 0, max(0, len(m.filteredPalette())-1))
	return m, cmd
}

// runPalette executes a palette action by id, returning any follow-up command.
func (m *Model) runPalette(id string) tea.Cmd {
	switch {
	case strings.HasPrefix(id, "ch:"):
		return m.openChannel(strings.TrimPrefix(id, "ch:"))
	case strings.HasPrefix(id, "dm:"):
		return m.openChannel(strings.TrimPrefix(id, "dm:"))
	case id == "cmd:workspace":
		return m.openWorkspacePicker()
	case id == "cmd:canvases":
		return m.openCanvasBrowser(true)
	case id == "cmd:join":
		return m.openJoinPicker()
	case id == "cmd:settings":
		m.openSettings()
	case id == "cmd:statustext":
		return m.openStatusText()
	case id == "cmd:theme":
		cyc := theme.CycleFor(theme.OmarchyAvailable())
		i := indexOf(cyc, m.prefs.Theme)
		m.prefs.Theme = cyc[(i+1)%len(cyc)]
		m.pal = theme.Resolve(m.prefs.Theme, m.prefs.Accent)
	case id == "cmd:hints":
		m.showHints = !m.showHints
	case id == "cmd:active":
		return m.setStatus("online")
	case id == "cmd:away":
		return m.setStatus("away")
	case id == "cmd:dnd":
		return m.setStatus("dnd")
	case id == "cmd:read":
		var unread []string
		for k, mm := range m.meta {
			if mm.Unread > 0 {
				unread = append(unread, k)
			}
			m.meta[k] = components.Meta{Unread: 0, Mention: false}
			if _, ok := m.hidden[k]; ok {
				m.hidden[k] = 0 // marking read resets the resurface baseline
			}
		}
		return tea.Batch(m.markAllReadCmd(unread), m.titleCmd())
	case id == "cmd:hide":
		return m.toggleHide(m.activeID)
	}
	return nil
}
