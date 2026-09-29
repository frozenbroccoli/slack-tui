package app

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/source"
	"github.com/kurenn/slack-tui/internal/ui/components"
)

// Message actions: react (a), edit (e), delete (dd), open links (o), and
// workspace search (s). All network work runs in tea.Cmds; local state is
// updated optimistically where it's safe and trued up by the poll otherwise.

type (
	reactMsg struct {
		convID, msgID, name string
		added               bool
		err                 error
	}
	editMsg struct {
		convID, msgID string
		err           error
	}
	deleteMsg struct {
		convID, msgID string
		err           error
	}
	searchMsg struct {
		hits []source.SearchHit
		err  error
	}
	// wsMsg delivers a reloaded workspace (e.g. after toggling group DMs).
	wsMsg struct {
		ws  *data.Workspace
		err error
	}
	// joinableMsg / joinedMsg drive the browse-and-join picker.
	joinableMsg struct {
		convs []data.Conversation
		err   error
	}
	joinedMsg struct {
		conv data.Conversation
		err  error
	}
)

// selectedMsg returns the message under the cursor in the messages pane.
func (m Model) selectedMsg() (data.Message, bool) {
	msgs := m.curMsgs()
	if m.focus != focusMessages || m.msgSel < 0 || m.msgSel >= len(msgs) {
		return data.Message{}, false
	}
	return msgs[m.msgSel], true
}

// selectedReply returns the thread reply under the cursor when the thread
// pane is focused.
func (m Model) selectedReply() (data.Reply, bool) {
	if m.focus != focusThread {
		return data.Reply{}, false
	}
	root, ok := m.threadRoot()
	if !ok {
		return data.Reply{}, false
	}
	if m.threadSel < 0 || m.threadSel >= len(root.Replies) {
		return data.Reply{}, false
	}
	return root.Replies[m.threadSel], true
}

func isPending(id string) bool { return strings.HasPrefix(id, pendingPrefix) }

// ── reactions ───────────────────────────────────────────────────────────────

func (m Model) reactCmd(convID, msgID, name string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		added, err := src.React(convID, msgID, name)
		return reactMsg{convID, msgID, name, added, err}
	}
}

// applyReact updates the local reaction pills after the backend confirmed.
func (m *Model) applyReact(msg reactMsg) tea.Cmd {
	if msg.err != nil {
		return m.flash(msg.err)
	}
	glyph := source.EmojiGlyph(msg.name)
	list := m.messages[msg.convID]
	for i := range list {
		if list[i].ID != msg.msgID {
			continue
		}
		for j := range list[i].Reactions {
			if list[i].Reactions[j].Emoji != glyph {
				continue
			}
			if msg.added {
				list[i].Reactions[j].Count++
			} else if list[i].Reactions[j].Count--; list[i].Reactions[j].Count <= 0 {
				list[i].Reactions = append(list[i].Reactions[:j], list[i].Reactions[j+1:]...)
			}
			if msg.convID == m.activeID {
				m.invalidateGeom() // reaction row may appear or disappear
			}
			return nil
		}
		if msg.added {
			list[i].Reactions = append(list[i].Reactions, data.Reaction{Emoji: glyph, Count: 1})
		}
	}
	if msg.convID == m.activeID {
		m.invalidateGeom() // a new reaction row was added
	}
	return nil
}

// ── edit ────────────────────────────────────────────────────────────────────

// startEdit moves the selected own message into the composer.
func (m *Model) startEdit(msg data.Message) tea.Cmd {
	m.editingID = msg.ID
	m.prevDraft = m.draft.Value()
	m.draft.SetValue(msg.Text)
	m.syncComposerSizes()
	return m.enterInsert(focusMessages)
}

// cancelEdit restores the parked draft.
func (m *Model) cancelEdit() {
	m.editingID = ""
	m.draft.SetValue(m.prevDraft)
	m.prevDraft = ""
	m.syncComposerSizes()
}

// applyEditDraft saves the edited text: optimistic local swap + chat.update.
func (m *Model) applyEditDraft() tea.Cmd {
	text := strings.TrimSpace(m.draft.Value())
	id := m.editingID
	m.cancelEdit()
	m.leaveInsert()
	if text == "" { // empty edit is a no-op (delete is dd)
		return nil
	}
	list := m.messages[m.activeID]
	for i := range list {
		if list[i].ID == id {
			list[i].Text = text
		}
	}
	m.invalidateGeom() // edited text may wrap to a different number of lines
	src, conv := m.src, m.activeID
	return func() tea.Msg { return editMsg{conv, id, src.Edit(conv, id, text)} }
}

// ── delete ──────────────────────────────────────────────────────────────────

// deleteMessage removes the message locally and on the backend.
func (m *Model) deleteMessage(msgID string) tea.Cmd {
	list := m.messages[m.activeID]
	for i := range list {
		if list[i].ID == msgID {
			m.messages[m.activeID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	m.msgSel = clamp(m.msgSel, 0, max(0, len(m.messages[m.activeID])-1))
	m.msgExtra = 0
	m.invalidateGeom()
	src, conv := m.src, m.activeID
	return func() tea.Msg { return deleteMsg{conv, msgID, src.Delete(conv, msgID)} }
}

// ── open links ──────────────────────────────────────────────────────────────

var urlRe = regexp.MustCompile(`https?://[^\s>\])]+`)

// openMsgLinks opens the selected message's link — directly when there's one,
// via the picker when there are several.
func (m *Model) openMsgLinks() tea.Cmd {
	msg, ok := m.selectedMsg()
	if !ok {
		return nil
	}
	links := urlRe.FindAllString(msg.Text, -1)
	links = append(links, msg.Links...)
	seen := map[string]bool{}
	uniq := links[:0]
	for _, l := range links {
		if !seen[l] {
			seen[l] = true
			uniq = append(uniq, l)
		}
	}
	switch len(uniq) {
	case 0:
		return m.flash(fmt.Errorf("no links in this message"))
	case 1:
		return openURLCmd(uniq[0])
	default:
		return m.openLinkPicker(uniq)
	}
}

// openURLCmd opens a URL in the default browser.
func openURLCmd(url string) tea.Cmd {
	return func() tea.Msg {
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", url)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		default:
			cmd = exec.Command("xdg-open", url)
		}
		_ = cmd.Start()
		return nil
	}
}

// ── workspace search ────────────────────────────────────────────────────────

func (m Model) searchCmd(query string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		hits, err := src.Search(query)
		return searchMsg{hits, err}
	}
}

// applySearch fills the open search picker with results.
func (m *Model) applySearch(msg searchMsg) tea.Cmd {
	if msg.err != nil {
		return m.flash(msg.err)
	}
	if !m.picker.open || m.picker.submit == nil {
		return nil // picker was closed while the search ran
	}
	items := make([]palItem, 0, len(msg.hits))
	for _, h := range msg.hits {
		label := h.UserName + ": " + strings.ReplaceAll(h.Text, "\n", " ")
		hint := "#" + h.ConvName + " · " + h.Time
		items = append(items, palItem{id: h.ConvID + "|" + h.MsgID, item: components.PaletteItem{
			Icon: "≡", Label: label, Hint: hint, Kind: "cmd",
		}})
	}
	m.picker.items = items
	m.picker.index = 0
	return nil
}

// ── browse & join channels ──────────────────────────────────────────────────

func (m Model) joinableCmd() tea.Cmd {
	src := m.src
	return func() tea.Msg {
		convs, err := src.Joinable()
		return joinableMsg{convs, err}
	}
}

func (m Model) joinCmd(convID string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		conv, err := src.Join(convID)
		return joinedMsg{conv, err}
	}
}

// applyJoinable fills the open join picker with the channel list.
func (m *Model) applyJoinable(msg joinableMsg) tea.Cmd {
	if msg.err != nil {
		m.closePicker()
		return m.flash(msg.err)
	}
	if !m.picker.open || m.picker.kind != "join" {
		return nil
	}
	items := make([]palItem, 0, len(msg.convs))
	for _, c := range msg.convs {
		items = append(items, palItem{id: c.ID, item: components.PaletteItem{
			Icon: "#", Label: c.Name, Hint: c.Topic, Kind: "channel",
		}})
	}
	m.picker.items = items
	m.picker.index = 0
	m.pickerInput.Placeholder = "join a channel…"
	return nil
}

// applyJoined adds the joined channel to the sidebar and opens it.
func (m *Model) applyJoined(msg joinedMsg) tea.Cmd {
	if msg.err != nil {
		return m.flash(msg.err)
	}
	if _, exists := m.ws.Conversation(msg.conv.ID); !exists {
		m.ws.Channels = append(m.ws.Channels, msg.conv)
		sort.Slice(m.ws.Channels, func(i, j int) bool { return m.ws.Channels[i].Name < m.ws.Channels[j].Name })
		m.meta[msg.conv.ID] = components.Meta{}
	}
	return m.openChannel(msg.conv.ID)
}

// ── workspace reload (group DMs toggle) ─────────────────────────────────────

func (m Model) reloadCmd() tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ws, err := src.Load()
		return wsMsg{ws, err}
	}
}

// applyWorkspace swaps in a reloaded workspace, keeping unread state and the
// active conversation where they still exist.
func (m *Model) applyWorkspace(msg wsMsg) tea.Cmd {
	if msg.err != nil {
		return m.flash(msg.err)
	}
	old := m.meta
	m.ws = msg.ws
	meta := map[string]components.Meta{}
	for _, c := range append(append([]data.Conversation{}, m.ws.Channels...), m.ws.DMs...) {
		if om, ok := old[c.ID]; ok {
			meta[c.ID] = om
		} else {
			meta[c.ID] = components.Meta{Unread: c.Unread, Mention: c.Mention}
		}
	}
	m.meta = meta
	if _, ok := m.ws.Conversation(m.activeID); !ok {
		switch {
		case len(m.ws.Channels) > 0:
			return m.openChannel(m.ws.Channels[0].ID)
		case len(m.ws.DMs) > 0:
			return m.openChannel(m.ws.DMs[0].ID)
		}
	}
	m.sideSel = m.flatIndexOf(m.activeID)
	return m.titleCmd()
}

// Discovery runs separately from history polling; conversations.list has its
// own rate budget and avoids reloading all users and channels every minute.
func (m Model) discoverDMsCmd() tea.Cmd {
	src, ok := m.src.(interface {
		RefreshDMs() ([]data.Conversation, error)
	})
	if !ok {
		return nil
	}
	return func() tea.Msg { convs, err := src.RefreshDMs(); return dmDiscoveredMsg{convs, err} }
}

func (m *Model) applyDiscoveredDMs(msg dmDiscoveredMsg) tea.Cmd {
	if msg.err != nil {
		return m.flash(msg.err)
	}
	selectedID := m.activeID
	items := m.sideItems()
	if m.sideSel >= 0 && m.sideSel < len(items) && !items[m.sideSel].Header {
		selectedID = items[m.sideSel].Conv.ID
	}
	known := map[string]bool{}
	for _, d := range m.ws.DMs {
		known[d.ID] = true
	}
	var added []string
	for _, d := range msg.convs {
		if d.ID == "" || known[d.ID] || (d.UserID == "" && !m.prefs.GroupDMs) {
			continue
		}
		known[d.ID] = true
		if u, ok := m.ws.Users[d.UserID]; ok && u.Name != "" {
			d.Name = u.Name
		}
		m.ws.DMs = append(m.ws.DMs, d)
		m.meta[d.ID] = components.Meta{}
		m.touchRecent(d.ID)
		added = append(added, d.ID)
	}
	if len(added) == 0 {
		return nil
	}
	sort.Slice(m.ws.DMs, func(i, j int) bool { return m.ws.DMs[i].Name < m.ws.DMs[j].Name })
	m.sideSel = m.flatIndexOf(selectedID)
	return m.unreadCmd(added)
}
