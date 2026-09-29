package app

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/ui/components"
)

// slashRe matches /command or /command rest. Group 1 is the command name;
// group 2 is the optional argument (may be empty string).
var slashRe = regexp.MustCompile(`^/([a-z][a-z0-9_+-]*)(?:\s+(.*))?$`)

type dmOpenedMsg struct {
	conv data.Conversation
	err  error
}

type leftMsg struct {
	convID string
	err    error
}

// runSlash dispatches a recognized slash command. It returns (cmd, true) when
// the input was command-shaped — either run or rejected with a flash.
// It returns (nil, false) when the text is not a command (e.g. /usr/local/bin)
// so the caller sends it as a normal message.
func (m *Model) runSlash(text string) (tea.Cmd, bool) {
	g := slashRe.FindStringSubmatch(text)
	if g == nil {
		return nil, false
	}
	cmd, rest := g[1], strings.TrimSpace(g[2])
	switch cmd {
	case "shrug":
		m.draft.SetValue("")
		return m.sendText(strings.TrimSpace(rest + " ¯\\_(ツ)_/¯")), true
	case "me":
		if rest == "" {
			m.draft.SetValue("")
			return nil, true
		}
		m.draft.SetValue("")
		return m.sendText("_" + rest + "_"), true
	case "away":
		m.draft.SetValue("")
		return m.setStatus("away"), true
	case "active":
		m.draft.SetValue("")
		return m.setStatus("online"), true
	case "dnd":
		m.draft.SetValue("")
		m.myStatus = "dnd"
		return m.snoozeCmd(parseMinutes(rest)), true
	case "status":
		m.draft.SetValue("")
		return m.setStatusTextCmd(rest), true
	case "dm":
		m.draft.SetValue("")
		return m.openDMCmd(rest), true
	case "leave":
		m.draft.SetValue("")
		if c, ok := m.ws.Conversation(m.activeID); ok && c.Type != "channel" {
			return m.flash(fmt.Errorf("/leave only works in channels")), true
		}
		return m.leaveCmd(m.activeID), true
	case "search":
		m.draft.SetValue("")
		return m.slashSearch(rest), true
	case "canvas":
		m.draft.SetValue("")
		if rest != "" {
			return m.openCanvasReference(rest), true
		}
		return m.openCanvasBrowser(false), true
	case "canvases":
		m.draft.SetValue("")
		return m.openCanvasBrowser(true), true
	default:
		m.draft.SetValue("")
		return m.flash(fmt.Errorf("/%s isn't supported — try /shrug /me /away /active /dnd /status /dm /leave /search /canvas /canvases", cmd)), true
	}
}

// parseMinutes parses a positive integer from s; returns 120 if absent/invalid.
func parseMinutes(s string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return 120
}

// snoozeCmd calls Snooze off the UI thread, surfacing any error via presenceMsg.
func (m Model) snoozeCmd(min int) tea.Cmd {
	src := m.src
	return func() tea.Msg { return presenceMsg{src.Snooze(min)} }
}

// openDMCmd resolves @handle → user ID and opens a 1:1 DM off the UI thread.
func (m *Model) openDMCmd(arg string) tea.Cmd {
	handle := strings.TrimPrefix(strings.TrimSpace(arg), "@")
	if handle == "" {
		return m.flash(fmt.Errorf("usage: /dm @user"))
	}
	var uid string
	for id, u := range m.ws.Users {
		if strings.EqualFold(u.Handle, handle) {
			uid = id
			break
		}
	}
	if uid == "" {
		return m.flash(fmt.Errorf("no user @%s", handle))
	}
	if uid == m.ws.MeID {
		return m.flash(fmt.Errorf("that's you"))
	}
	src := m.src
	return func() tea.Msg {
		conv, err := src.OpenDM(uid)
		return dmOpenedMsg{conv, err}
	}
}

// leaveCmd leaves convID off the UI thread.
func (m Model) leaveCmd(convID string) tea.Cmd {
	src := m.src
	return func() tea.Msg { return leftMsg{convID, src.Leave(convID)} }
}

// slashSearch opens the workspace search picker, optionally pre-seeding a query.
func (m *Model) slashSearch(q string) tea.Cmd {
	focus := m.openSearchPicker()
	if q == "" {
		return focus
	}
	m.pickerInput.SetValue(q)
	m.picker.lastQ = q
	return tea.Batch(focus, m.searchCmd(q))
}

// ── message handlers for dmOpenedMsg / leftMsg ───────────────────────────────

// applyDMOpened adds the new DM to the workspace (if not already present) and
// switches to it.
func (m *Model) applyDMOpened(msg dmOpenedMsg) tea.Cmd {
	if msg.err != nil {
		return m.flash(msg.err)
	}
	if _, exists := m.ws.Conversation(msg.conv.ID); !exists {
		m.ws.DMs = append(m.ws.DMs, msg.conv)
		sort.Slice(m.ws.DMs, func(i, j int) bool { return m.ws.DMs[i].Name < m.ws.DMs[j].Name })
		m.meta[msg.conv.ID] = components.Meta{}
	}
	return m.openChannel(msg.conv.ID)
}

// applyLeft removes the left channel from the workspace and moves to the next
// available conversation.
func (m *Model) applyLeft(msg leftMsg) tea.Cmd {
	if msg.err != nil {
		return m.flash(msg.err)
	}
	kept := make([]data.Conversation, 0, len(m.ws.Channels))
	for _, c := range m.ws.Channels {
		if c.ID != msg.convID {
			kept = append(kept, c)
		}
	}
	m.ws.Channels = kept
	delete(m.meta, msg.convID)
	delete(m.hidden, msg.convID)
	if m.activeID == msg.convID {
		next := ""
		if len(m.ws.Channels) > 0 {
			next = m.ws.Channels[0].ID
		} else if len(m.ws.DMs) > 0 {
			next = m.ws.DMs[0].ID
		}
		if next != "" {
			return m.openChannel(next)
		}
		m.activeID = ""
		return m.flash(fmt.Errorf("left — no conversations remain"))
	}
	m.sideSel = m.flatIndexOf(m.activeID)
	return m.titleCmd()
}
