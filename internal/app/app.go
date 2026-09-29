// Package app is the root Bubble Tea model: navigation state, the modal
// NORMAL/INSERT keyboard engine (ported from tui-app.jsx's keydown router), and
// the View that composes the panes. Data still comes from the mock workspace.
package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/kurenn/slack-tui/internal/auth"
	"github.com/kurenn/slack-tui/internal/config"
	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/source"
	"github.com/kurenn/slack-tui/internal/theme"
	"github.com/kurenn/slack-tui/internal/ui/components"
)

const (
	sidebarWidth = 30

	focusSidebar  = "sidebar"
	focusMessages = "messages"
	focusThread   = "thread"
)

// composerLines is how many draft rows the composer shows before scrolling.
const maxComposerLines = 4

// draftWidth / threadDraftWidth mirror the textarea widths set at render time —
// height math must wrap at the same column the textarea does.
func (m Model) draftWidth() int {
	centerW := m.width - sidebarWidth - 1
	if m.threadOpen() {
		centerW = m.width - sidebarWidth - m.threadWidth - 2
	}
	return max(4, (centerW-2)-24)
}

func (m Model) threadDraftWidth() int {
	return max(4, (m.threadWidth-2)-24)
}

// wrappedRows counts the display rows value occupies in a textarea of width w —
// logical lines plus soft-wrapping (pasted code often has lines wider than the
// composer; counting only "\n" would leave the box too short to show them).
func wrappedRows(value string, w int) int {
	if w < 1 {
		return 1
	}
	rows := 0
	for _, ln := range strings.Split(value, "\n") {
		rows += lipgloss.Width(ln)/w + 1 // textarea wraps the cursor cell too
	}
	return max(1, rows)
}

// composerHeight is the center composer's total height (borders + draft rows only).
// Chip/note rows (attachRows) are rendered between the message pane and the composer
// and must be subtracted separately — adding them here causes a double-count.
func (m Model) composerHeight() int {
	return 2 + clamp(wrappedRows(m.draft.Value(), m.draftWidth()), 1, maxComposerLines)
}

// threadComposerHeight is the thread composer's total height.
func (m Model) threadComposerHeight() int {
	return 2 + clamp(wrappedRows(m.threadDraft.Value(), m.threadDraftWidth()), 1, maxComposerLines)
}

// syncComposerSizes pushes the current geometry into the persistent textareas.
// This must happen at update time (not on render copies): the textarea
// repositions its viewport to the cursor inside Update, using whatever size it
// has then — sizing only a render copy leaves the real one at a stale height,
// which is how pasted code ended up typing into an invisible row.
func (m *Model) syncComposerSizes() {
	m.draft.SetWidth(m.draftWidth())
	m.draft.SetHeight(m.composerHeight() - 2)
	m.threadDraft.SetWidth(m.threadDraftWidth())
	m.threadDraft.SetHeight(m.threadComposerHeight() - 2)
	// Settle the viewports: the textarea only loads content into its viewport
	// during View, and only repositions to the cursor during Update — so a
	// paste that grows the box and moves the cursor in one event would stay
	// scrolled wrong until the next keystroke. View-then-Update(nil) runs both
	// steps now, against the new size.
	_ = m.draft.View()
	m.draft, _ = m.draft.Update(nil)
	_ = m.threadDraft.View()
	m.threadDraft, _ = m.threadDraft.Update(nil)
}

// Model is the application state.
type Model struct {
	src          source.Source
	ws           *data.Workspace
	prefs        config.Prefs
	tokens       config.Tokens // live credentials; refreshed in place when rotating
	unreadPrimed bool          // first unread poll seen; before that, growth isn't news
	themeStamp   string        // Omarchy theme fingerprint, to notice a desktop re-theme
	pal          theme.Palette
	density      theme.Density
	showHints    bool
	loadErr      error

	messages    map[string][]data.Message
	fullyLoaded map[string]bool // conversations with no more older history
	loading     map[string]bool // conversations with an in-flight history fetch
	meta        map[string]components.Meta
	hidden      map[string]int // conv IDs hidden from the sidebar → unread baseline captured at hide time
	myStatus    string

	activeID     string
	focus        string
	insert       bool
	sideSel      int // flat index into sidebar items
	msgSel       int
	msgExtra     int // extra line scroll within the message pane (for tall messages)
	threadRootID string
	threadSel    int

	draft       textarea.Model
	threadDraft textarea.Model
	drafts      map[string]string // per-conversation unsent draft text

	pendingSeq int  // counter for optimistic (not yet acked) send IDs
	errSeq     int  // generation counter for the auto-clearing error banner
	lastTitleN int  // last unread total pushed to the terminal title
	markWarned bool // a mark-read failure was already reported (it repeats every poll)

	helpOpen bool

	editingID string // message being edited in the composer ("" = composing new)
	prevDraft string // draft parked while editing
	dPending  time.Time
	suggest   suggestState // composer autocomplete popup

	picker      pickerState
	pickerInput textinput.Model
	confirm     confirmState

	findOpen    bool
	findInput   textinput.Model
	searchQuery string // last local-find query (n/N repeat)

	pendingFiles []string // local files staged for the active conversation
	attachOpen   bool     // the "attach file" input overlay is open
	attachInput  textinput.Model
	uploadNote   string // transient "uploading…" note above the composer

	newMark        map[string]string // convID → first-unread message ID (the ── new ── rule)
	pendingUnread  map[string]int    // unread count snapshot for channels opening async
	readSeq        int               // bumped each time a conversation is opened/read
	readSeqOf      map[string]int    // convID → readSeq when last read; guards stale unread polls
	dmPollOffset   int               // rotating window cursor over the dormant DM tail
	chanPollOffset int               // rotating window cursor over the dormant channel tail
	pendingSelect  map[string]string // convID → message ID to select once history lands
	pendingThread  map[string]string // convID → root ID to open as a thread once history lands

	seenReplies map[string]int // (convID+"|"+rootID) → reply count last seen, for the threads inbox "new" badge

	recent       []string // conversation IDs, most recently opened first (palette ordering)
	persistState bool     // production only: save drafts+recency on quit (tests never write)

	workspaces      []config.Workspace // signed-in workspaces (cached at startup)
	activeWorkspace string

	paletteOpen  bool
	paletteQuery textinput.Model
	paletteIndex int

	settingsOpen bool
	settingsSel  int

	canvas canvasState

	statusTextOpen  bool
	statusTextInput textinput.Model

	width, height int
	gPending      time.Time

	// geomCache memoises the MessagesBody geometry for the active conversation.
	// starts/total depend on content, width, density, and the newMark — but NOT
	// on selIndex or focus (selection only restyles lines, never adds/removes them).
	// msgGen is a monotone counter bumped by invalidateGeom() at every mutation
	// site; width, density, and newMark are encoded directly in the key string.
	geomCache struct {
		key    string
		starts []int
		total  int
	}
	msgGen int // bumped whenever active-conversation layout could change

	// mouse drag-to-select state
	selecting bool   // a left-drag selection is in progress
	selActive bool   // a selection is shown (persists after release until next key/click/scroll)
	selAnchor cell   // selection start in content coordinates
	selHead   cell   // selection end in content coordinates
	selPane   string // which pane the active selection lives in

	threadWidth int  // current thread pane width (clamped)
	resizing    bool // a thread-divider drag is in progress

	copyToast  string // transient "copied" toast label ("" = hidden)
	copyToastX int    // toast screen position (clamped in View)
	copyToastY int
	copySeq    int // generation counter for the auto-clearing copy toast
}

// New builds the production model from saved (or default) prefs. The data
// source is real Slack when a user token is configured (env or tokens.json),
// otherwise the local mock. Call it off the UI thread — Load talks to the
// network (root runs it inside a tea.Cmd behind a connecting screen).
func New() Model {
	prefs, _ := config.Load()

	// Tokens come from the stored file, with env vars overriding per-token.
	saved, _ := config.LoadTokens()
	tok := refreshIfDue(saved).Resolve()

	var src source.Source
	if tok.User != "" {
		sl := source.NewSlack(tok.User)
		sl.SetGroupDMs(prefs.GroupDMs)
		src = sl
	} else {
		src = source.NewMock()
	}
	m := NewWith(src, prefs)
	m.tokens = tok

	// Session state (drafts + palette recency) lives outside NewWith so tests
	// stay hermetic; restore it and arm the save-on-quit.
	if st, err := config.LoadState(); err == nil {
		if st.Drafts != nil {
			m.drafts = st.Drafts
		}
		m.recent = st.Recent
		if st.Hidden != nil {
			m.hidden = st.Hidden
		}
		if d := m.drafts[m.activeID]; d != "" {
			m.draft.SetValue(d)
			m.syncComposerSizes()
		}
	}
	m.persistState = true
	m.workspaces, m.activeWorkspace, _ = config.LoadWorkspaces()

	// Real-time: start Socket Mode if the app + bot tokens are present (m.src
	// may have fallen back to the mock if Load failed).
	if sl, ok := m.src.(*source.Slack); ok && tok.App != "" && tok.Bot != "" {
		sl.StartSocket(tok.App, tok.Bot)
	}
	return m
}

// refreshIfDue renews a rotating user token that is expired or close to it.
// Safe to call here because New() already runs inside a tea.Cmd, off the UI
// thread, behind the connecting screen.
//
// The new credentials are persisted BEFORE being returned for use: the refresh
// token just spent is single-use and already dead, so a crash after the call
// but before the write would cost the user a re-login. On any failure we return
// the old tokens unchanged and let the normal expired-token error surface —
// refusing to start would be worse than starting and reporting.
func refreshIfDue(t config.Tokens) config.Tokens {
	if os.Getenv("SLACK_USER_TOKEN") != "" {
		return t // an explicit override is the user's to manage
	}
	if !auth.Due(t) {
		return t
	}
	creds := config.LoadOAuthCreds()
	if !creds.Ready() {
		return t // no client ID to refresh against
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fresh, err := auth.Refresh(ctx, creds, t.Refresh)
	if err != nil {
		return t
	}
	if err := config.SaveRefreshed(fresh); err != nil {
		return t
	}
	t.User, t.Refresh, t.ExpiresAt = fresh.User, fresh.Refresh, fresh.ExpiresAt
	return t
}

// ReloadMsg asks root to tear this app down and rebuild it — emitted after the
// active workspace changed.
type ReloadMsg struct{}

// Shutdown releases the app's background resources (the Socket Mode
// connection) before root discards the model on a workspace switch.
func (m Model) Shutdown() {
	if sl, ok := m.src.(*source.Slack); ok {
		sl.StopSocket()
	}
}

// stateSnapshot builds the persisted session state (drafts sans empties, recency, hidden).
func (m Model) stateSnapshot() config.State {
	drafts := map[string]string{}
	for k, v := range m.drafts {
		if strings.TrimSpace(v) != "" {
			drafts[k] = v
		}
	}
	if v := m.draft.Value(); strings.TrimSpace(v) != "" && m.editingID == "" {
		drafts[m.activeID] = v
	}
	hidden := make(map[string]int, len(m.hidden))
	for k, v := range m.hidden {
		hidden[k] = v
	}
	return config.State{Drafts: drafts, Recent: append([]string(nil), m.recent...), Hidden: hidden}
}

// persistStateCmd writes session state off the UI thread (no-op when not persisting, e.g. tests).
func (m *Model) persistStateCmd() tea.Cmd {
	if !m.persistState {
		return nil
	}
	s := m.stateSnapshot()
	return func() tea.Msg { _ = config.SaveState(s); return nil }
}

// quit persists session state and exits — every quit key path funnels here.
func (m *Model) quit() tea.Cmd {
	if m.persistState {
		_ = config.SaveState(m.stateSnapshot())
	}
	return tea.Quit
}

// touchRecent moves a conversation to the front of the recency order.
func (m *Model) touchRecent(id string) {
	out := make([]string, 0, len(m.recent)+1)
	out = append(out, id)
	for _, r := range m.recent {
		if r != id {
			out = append(out, r)
		}
	}
	if len(out) > 50 {
		out = out[:50]
	}
	m.recent = out
}

// NewWith builds the model on an explicit source and prefs. Tests inject the
// mock here so they never touch the network or the user's config files.
func NewWith(src source.Source, prefs config.Prefs) Model {
	ws, err := src.Load()
	var loadErr error
	if err != nil { // fall back to the mock so the app still runs; surface the error
		loadErr = err
		src = source.NewMock()
		ws, _ = src.Load()
	}

	// Onboarding hand-off: adopt the chosen handle as the current user's identity.
	if prefs.Handle != "" {
		me := ws.Users[ws.MeID]
		me.Name, me.Handle = prefs.Handle, prefs.Handle
		ws.Users[ws.MeID] = me
	}

	meta := map[string]components.Meta{}
	for _, c := range append(append([]data.Conversation{}, ws.Channels...), ws.DMs...) {
		meta[c.ID] = components.Meta{Unread: c.Unread, Mention: c.Mention}
	}

	mkInput := func() textinput.Model {
		ti := textinput.New()
		ti.Prompt = ""
		return ti
	}
	mkArea := func() textarea.Model {
		ta := textarea.New()
		ta.Prompt = ""
		ta.ShowLineNumbers = false
		ta.CharLimit = 0
		ta.SetHeight(1)
		// Enter sends (handled by insertKey); newlines come from alt+enter/ctrl+j.
		ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
		// The composer draws its own chrome — flatten the textarea's.
		ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
		ta.FocusedStyle.Base = lipgloss.NewStyle()
		ta.BlurredStyle.Base = lipgloss.NewStyle()
		return ta
	}

	activeID := "engineering"                    // the mock's demo channel
	if _, ok := ws.Conversation(activeID); !ok { // real Slack: pick the first channel/DM
		switch {
		case len(ws.Channels) > 0:
			activeID = ws.Channels[0].ID
		case len(ws.DMs) > 0:
			activeID = ws.DMs[0].ID
		}
	}

	m := Model{
		src:             src,
		ws:              ws,
		prefs:           prefs,
		pal:             theme.Resolve(prefs.Theme, prefs.Accent),
		themeStamp:      theme.OmarchyStamp(),
		density:         theme.ParseDensity(prefs.Density),
		showHints:       true,
		loadErr:         loadErr,
		messages:        map[string][]data.Message{},
		fullyLoaded:     map[string]bool{},
		loading:         map[string]bool{},
		meta:            meta,
		myStatus:        prefs.Status,
		activeID:        activeID,
		focus:           focusMessages,
		draft:           mkArea(),
		threadDraft:     mkArea(),
		drafts:          map[string]string{},
		paletteQuery:    mkInput(),
		statusTextInput: mkInput(),
		pickerInput:     mkInput(),
		findInput:       mkInput(),
		attachInput:     mkInput(),
		canvas:          canvasState{input: mkInput(), cache: &canvasRenderCache{}},
		newMark:         map[string]string{},
		pendingUnread:   map[string]int{},
		readSeqOf:       map[string]int{},
		pendingSelect:   map[string]string{},
		pendingThread:   map[string]string{},
		seenReplies:     map[string]int{},
		hidden:          map[string]int{},
	}
	m.ensureHistory(activeID)
	tw := prefs.ThreadWidth
	if tw == 0 {
		tw = 60
	}
	m.threadWidth = tw
	m.sideSel = m.flatIndexOf(activeID)
	m.msgSel = max(0, len(m.messages[activeID])-1)
	return m
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{pollTick(), themeWatchTick(), dmTailPollTick()}
	if sl, ok := m.src.(*source.Slack); ok {
		if sl.Events() != nil {
			cmds = append(cmds, listenEvents(sl)) // live channel events
		} else {
			cmds = append(cmds, chanPollTick()) // no Socket Mode: poll channel unread instead
		}
		cmds = append(cmds, dmPollTick())       // periodic DM unread (Socket Mode can't see DMs)
		cmds = append(cmds, presencePollTick()) // periodic presence refresh for DM partners
		cmds = append(cmds, m.presenceCmd())    // immediate first fetch so dots are right at startup
		// Immediate unread fetch so sidebar dots appear right after launch
		// instead of waiting for the first poll tick (Load no longer blocks on it).
		cmds = append(cmds, m.unreadCmd(m.chanIDs()), m.unreadCmd(m.dmHeadIDs()), m.unreadCmd(m.dmTailIDs()))
	}
	return tea.Batch(cmds...)
}

// invalidateGeom bumps the generation counter so the next msgGeom call
// recomputes from scratch. Call it at every site that changes the line layout
// of the active conversation's messages (content, reactions, replies, newMark).
// An over-bump is cheap (one recompute); an under-bump would return stale geometry.
func (m *Model) invalidateGeom() { m.msgGen++ }

// ── derived helpers ─────────────────────────────────────────────────────────

func (m Model) sideItems() []components.SideItem {
	hidden := make(map[string]bool, len(m.hidden))
	for id := range m.hidden {
		hidden[id] = true
	}
	return components.BuildSideItems(m.ws, m.meta, hidden)
}

func (m Model) selectable() []int {
	return components.SelectableIndexes(m.sideItems())
}

func (m Model) flatIndexOf(id string) int {
	for i, it := range m.sideItems() {
		if !it.Header && it.Conv.ID == id {
			return i
		}
	}
	return 0
}

// toggleHide hides the conversation (snapshotting current unread as the resurface
// baseline) or un-hides it if already hidden, keeps the sidebar cursor valid, and persists.
func (m *Model) toggleHide(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	if _, ok := m.hidden[id]; ok {
		delete(m.hidden, id)
	} else {
		m.hidden[id] = m.meta[id].Unread // resurface baseline; ~30-unread cap from source.Unread means a maxed-out channel won't auto-resurface via poll — open or post to reset
	}
	// keep the sidebar cursor on a still-visible row: the hidden conversation
	// may have been the selection, and flat indices shift when a row drops out.
	if sel := m.selectable(); len(sel) == 0 {
		m.sideSel = 0
	} else {
		ns := sel[0]
		for _, i := range sel {
			if i <= m.sideSel {
				ns = i
			} else {
				break
			}
		}
		m.sideSel = ns
	}
	return m.persistStateCmd()
}

func (m Model) threadOpen() bool { return m.threadRootID != "" }

func (m Model) focusOrder() []string {
	order := []string{focusSidebar, focusMessages}
	if m.threadOpen() {
		order = append(order, focusThread)
	}
	return order
}

func (m Model) curMsgs() []data.Message { return m.messages[m.activeID] }

func (m Model) threadRoot() (data.Message, bool) {
	if m.threadRootID == "" {
		return data.Message{}, false
	}
	for _, list := range m.messages {
		for _, msg := range list {
			if msg.ID == m.threadRootID {
				return msg, true
			}
		}
	}
	return data.Message{}, false
}

// ── navigation actions ──────────────────────────────────────────────────────

// ensureHistory loads a conversation's messages once and caches them (an empty
// entry still counts as loaded, so we never refetch).
func (m *Model) ensureHistory(id string) {
	if _, ok := m.messages[id]; ok {
		return
	}
	msgs, err := m.src.History(id)
	if err != nil {
		m.loadErr = err
	}
	m.messages[id] = msgs
	if id == m.activeID {
		m.invalidateGeom()
	}
}

func (m *Model) openChannel(id string) tea.Cmd {
	if id != m.activeID { // park the unsent draft; restore the target's
		m.drafts[m.activeID] = m.draft.Value()
		m.draft.SetValue(m.drafts[id])
		m.syncComposerSizes()
	}
	m.pendingFiles = nil // clear staging so files don't accidentally post to the wrong conv
	m.uploadNote = ""
	unread := m.meta[id].Unread // captured before clearing — drives the ── new ── rule
	m.activeID = id
	m.touchRecent(id)
	m.msgExtra = 0
	m.meta[id] = components.Meta{Unread: 0, Mention: false}
	m.readSeq++
	m.readSeqOf[id] = m.readSeq // a slower in-flight unread poll can't re-flag this as unread
	if _, ok := m.hidden[id]; ok {
		m.hidden[id] = 0 // reading resets the resurface baseline so a later message re-shows it
	}
	m.focus = focusMessages
	m.sideSel = m.flatIndexOf(id)
	m.threadRootID = ""
	delete(m.newMark, id)
	if _, ok := m.messages[id]; !ok {
		if _, instant := m.src.(*source.Mock); instant {
			m.ensureHistory(id) // the mock answers in-memory — keep it synchronous
		} else { // real Slack: fetch off the UI thread, show "loading…" meanwhile
			m.loading[id] = true
			if unread > 0 {
				m.pendingUnread[id] = unread // mark + cursor computed when history lands
			}
			m.msgSel = 0
			return tea.Batch(m.historyCmd(id), m.titleCmd())
		}
	}
	m.msgSel = max(0, len(m.messages[id])-1)
	m.applyNewMark(id, unread)
	m.applyPendingSelect(id)
	cmds := []tea.Cmd{m.markReadCmd(id), m.titleCmd()} // persist read state to the backend
	if c := m.applyPendingThread(id); c != nil {
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

// applyNewMark places the ── new ── rule before the first unread message and
// lands the cursor there (unread counts messages after last_read, so the first
// unread is len-unread).
func (m *Model) applyNewMark(id string, unread int) {
	msgs := m.messages[id]
	if unread <= 0 || len(msgs) == 0 {
		return
	}
	idx := max(0, len(msgs)-unread)
	m.newMark[id] = msgs[idx].ID
	if id == m.activeID {
		m.msgSel, m.msgExtra = idx, 0
	}
}

// applyPendingSelect moves the cursor to a requested message (search jump).
func (m *Model) applyPendingSelect(id string) {
	want, ok := m.pendingSelect[id]
	if !ok {
		return
	}
	delete(m.pendingSelect, id)
	for i, msg := range m.messages[id] {
		if msg.ID == want && id == m.activeID {
			m.msgSel, m.msgExtra = i, 0
			return
		}
	}
}

// applyPendingThread opens a requested thread once its root is cached (threads
// inbox jump). Mirrors applyPendingSelect: returns the openThread cmd, or nil.
func (m *Model) applyPendingThread(id string) tea.Cmd {
	root, ok := m.pendingThread[id]
	if !ok {
		return nil
	}
	for _, msg := range m.messages[id] {
		if msg.ID == root {
			delete(m.pendingThread, id)
			return m.openThread(root)
		}
	}
	return nil
}

// pageJump is the selection step for half-page scrolling (Ctrl-d/Ctrl-u).
func (m Model) pageJump() int {
	if n := (m.height - 6) / 4; n > 1 {
		return n
	}
	return 1
}

// msgGeom computes the selected-message line span, total body lines, and the
// message-pane inner height — for line-accurate scrolling.
//
// starts/total are memoised in geomCache: they depend on content, width,
// density, and the newMark but NOT on selIndex or focus (selection only
// restyles existing lines via a left bar + background fill; it never adds or
// removes lines). The cache is invalidated via invalidateGeom() at every
// mutation site. innerH is geometry only — never cached.
func (m *Model) msgGeom() (ss, se, total, innerH int) {
	centerW := m.width - sidebarWidth - 1
	if m.threadOpen() {
		centerW = m.width - sidebarWidth - m.threadWidth - 2
	}
	innerH = (m.height - 2 - m.composerHeight() - m.attachRows()) - 2

	// Build a cheap fingerprint from all inputs that affect line layout.
	// fmt.Sprintf is ~50 ns for these small fields — negligible vs MessagesBody.
	key := fmt.Sprintf("%s|%d|%d|%s|%d", m.activeID, centerW-2, m.density, m.newMark[m.activeID], m.msgGen)
	if m.geomCache.key == key {
		starts := m.geomCache.starts
		total = m.geomCache.total
		msgs := m.curMsgs()
		if n := len(msgs); n > 0 {
			mi := clamp(m.msgSel, 0, n-1)
			ss, se = starts[mi], starts[mi+1]-1
		}
		return
	}

	// Cache miss: recompute. selIndex/focused are set to dummy values because
	// they don't affect starts or len(lines) — only line content (styling).
	msgs := m.curMsgs()
	lines, starts := components.MessagesBody(m.pal, m.ws, msgs, -1, false, m.density, centerW-2, m.newMark[m.activeID])
	total = len(lines)
	m.geomCache.key = key
	m.geomCache.starts = starts
	m.geomCache.total = total

	if n := len(msgs); n > 0 {
		mi := clamp(m.msgSel, 0, n-1)
		ss, se = starts[mi], starts[mi+1]-1
	}
	return
}

// msgViewport returns the message pane's full rendered lines, the scroll offset
// (top), and the inner pane dimensions. It is the single source of truth shared
// by renderCenter (display) and the mouse hit-test helpers (cellAt, selectionText)
// so they never drift.
func (m Model) msgViewport() (lines []string, top, innerW, innerH int) {
	centerW := m.width - sidebarWidth - 1
	if m.threadOpen() {
		centerW = m.width - sidebarWidth - m.threadWidth - 2
	}
	innerW = centerW - 2
	innerH = (m.height - 2 - m.composerHeight() - m.attachRows()) - 2
	msgs := m.curMsgs()
	var starts []int
	lines, starts = components.MessagesBody(m.pal, m.ws, msgs, m.msgSel, m.focus == focusMessages, m.density, innerW, m.newMark[m.activeID])
	ss, se := 0, 0
	if n := len(msgs); n > 0 {
		mi := clamp(m.msgSel, 0, n-1)
		ss, se = starts[mi], starts[mi+1]-1
	}
	if len(lines) > innerH {
		top = clamp(windowBaseTop(ss, se, innerH, len(lines))+m.msgExtra, 0, len(lines)-innerH)
	}
	return
}

// threadViewport returns the thread pane's rendered lines, scroll offset (top),
// inner dimensions, and the left edge screen column. Mirrors msgViewport for the
// thread — shared by renderThread and the mouse hit-test helpers.
func (m Model) threadViewport() (lines []string, top, innerW, innerH, x0 int) {
	root, ok := m.threadRoot()
	if !ok {
		return nil, 0, 0, 0, 0
	}
	innerW = m.threadWidth - 2
	scrollH := (m.height - 2 - 2) - m.threadComposerHeight()
	innerH = scrollH
	x0 = m.width - m.threadWidth + 1
	var starts []int
	lines, starts = components.ThreadScroll(m.pal, m.ws, root, m.threadSel, m.focus == focusThread, innerW)
	ss, se := 0, 0
	if r := len(root.Replies); r > 0 {
		ti := clamp(m.threadSel, 0, r-1)
		ss, se = starts[ti], starts[ti+1]-1
	}
	if len(lines) > scrollH {
		top = windowBaseTop(ss, se, scrollH, len(lines))
	}
	return
}

// scrollMessages line-scrolls the message pane by step (Ctrl-d/Ctrl-u), letting
// you read through a message taller than the viewport. Other panes move selection.
func (m *Model) scrollMessages(step int) {
	if m.focus != focusMessages {
		m.moveSel(step)
		return
	}
	ss, se, total, innerH := m.msgGeom()
	base := windowBaseTop(ss, se, innerH, total)
	maxTop := max(0, total-innerH)
	m.msgExtra = clamp(base+m.msgExtra+step, 0, maxTop) - base
}

// msgScrollStep is the per-keystroke line step when j/k scrolls within a message
// taller than the viewport.
const msgScrollStep = 3

// messagesDown handles j/↓ in the message pane: scroll down through the selected
// message if it overflows the viewport, otherwise advance to the next message.
func (m *Model) messagesDown() {
	msgs := m.curMsgs()
	ss, se, total, innerH := m.msgGeom()
	if total > innerH { // there is something to scroll
		base := windowBaseTop(ss, se, innerH, total)
		curTop := clamp(base+m.msgExtra, 0, total-innerH)
		if se > curTop+innerH-1 { // selected message extends below the viewport
			m.msgExtra = clamp(curTop+msgScrollStep, 0, total-innerH) - base
			return
		}
	}
	if m.msgSel < len(msgs)-1 {
		m.msgSel++
		m.msgExtra = 0
	}
}

// messagesUp handles k/↑ in the message pane: scroll back up within a tall
// message, otherwise move to the previous message.
func (m *Model) messagesUp() {
	if m.msgExtra > 0 {
		ss, se, total, innerH := m.msgGeom()
		base := windowBaseTop(ss, se, innerH, total)
		curTop := clamp(base+m.msgExtra, 0, max(0, total-innerH))
		m.msgExtra = max(0, clamp(curTop-msgScrollStep, 0, max(0, total-innerH))-base)
		return
	}
	if m.msgSel > 0 {
		m.msgSel--
		m.msgExtra = 0
	}
}

// atHistoryTop reports whether the message list is focused, scrolled to its
// first message, and may have more (unloaded) older history.
func (m Model) atHistoryTop() bool {
	return m.focus == focusMessages && m.msgSel == 0 && len(m.curMsgs()) > 0 && !m.fullyLoaded[m.activeID]
}

// jumpUnread opens the next/prev conversation (in sidebar order) with unread,
// wrapping around. No-op if nothing is unread.
func (m *Model) jumpUnread(dir int) tea.Cmd {
	var ids []string
	for _, c := range m.ws.Channels {
		ids = append(ids, c.ID)
	}
	for _, c := range m.ws.DMs {
		ids = append(ids, c.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	start := 0
	for i, id := range ids {
		if id == m.activeID {
			start = i
			break
		}
	}
	for k := 1; k <= len(ids); k++ {
		i := ((start+dir*k)%len(ids) + len(ids)) % len(ids)
		if _, hid := m.hidden[ids[i]]; hid {
			continue // hidden conversations are excluded from ] / [ navigation
		}
		if m.meta[ids[i]].Unread > 0 {
			return m.openChannel(ids[i])
		}
	}
	return nil
}

func (m *Model) openThread(msgID string) tea.Cmd {
	m.threadRootID = msgID
	m.threadSel = 0
	m.focus = focusThread
	m.markThreadSeen(m.activeID, msgID) // opening clears the inbox "new" badge
	m.syncComposerSizes()               // the center pane narrows when the thread opens
	return m.repliesCmd(m.activeID, msgID)
}

// markThreadSeen records the current reply count of a thread root, so the
// threads inbox only flags replies that arrived since you last looked.
func (m *Model) markThreadSeen(convID, rootID string) {
	for _, msg := range m.messages[convID] {
		if msg.ID == rootID {
			m.seenReplies[convID+"|"+rootID] = msg.ReplyCount
			return
		}
	}
}

func (m *Model) closeThread() {
	m.threadRootID = ""
	m.resizing = false
	m.focus = focusMessages
	m.syncComposerSizes()
}

func (m *Model) enterInsert(which string) tea.Cmd {
	m.insert = true
	if which == focusThread {
		return m.threadDraft.Focus()
	}
	return m.draft.Focus()
}

func (m *Model) leaveInsert() {
	m.insert = false
	m.clearSuggest()
	m.draft.Blur()
	m.threadDraft.Blur()
}

// pendingPrefix marks optimistic local messages awaiting the backend's ack.
const pendingPrefix = "pending-"

func nowClock() string { return time.Now().Format("15:04") }

// sendMessage appends the draft optimistically and posts it off the UI thread;
// sentMsg reconciles (real ID on success, removal + draft restore on error).
func (m *Model) sendMessage() tea.Cmd {
	if len(m.pendingFiles) > 0 {
		return m.sendAttachments()
	}
	text := strings.TrimSpace(m.draft.Value())
	if text == "" {
		return nil
	}
	if cmd, done := m.runSlash(text); done {
		return cmd
	}
	return m.sendText(text)
}

// sendText optimistically appends text and posts it (no slash interception).
// Use this from slash handlers that have already transformed the text, so the
// dispatcher is never re-entered on the transformed value.
func (m *Model) sendText(text string) tea.Cmd {
	delete(m.hidden, m.activeID)
	m.pendingSeq++
	pid := fmt.Sprintf("%s%d", pendingPrefix, m.pendingSeq)
	m.messages[m.activeID] = append(m.messages[m.activeID],
		data.Message{ID: pid, UserID: m.ws.MeID, Time: nowClock(), Text: text})
	m.invalidateGeom()
	m.draft.SetValue("")
	m.clearSuggest()
	m.syncComposerSizes()
	m.msgSel = len(m.messages[m.activeID]) - 1
	m.msgExtra = 0
	src, conv := m.src, m.activeID
	return func() tea.Msg {
		msg, err := src.Send(conv, text)
		return sentMsg{convID: conv, pendingID: pid, text: text, msg: msg, err: err}
	}
}

// sendReply mirrors sendMessage for thread replies.
func (m *Model) sendReply() tea.Cmd {
	text := strings.TrimSpace(m.threadDraft.Value())
	if text == "" || m.threadRootID == "" {
		return nil
	}
	delete(m.hidden, m.activeID)
	m.pendingSeq++
	pid := fmt.Sprintf("%s%d", pendingPrefix, m.pendingSeq)
	m.eachRootMsg(m.threadRootID, func(msg *data.Message) {
		msg.Replies = append(msg.Replies, data.Reply{ID: pid, UserID: m.ws.MeID, Time: nowClock(), Text: text})
	})
	m.threadDraft.SetValue("")
	m.clearSuggest()
	m.syncComposerSizes()
	src, conv, root := m.src, m.activeID, m.threadRootID
	return func() tea.Msg {
		r, err := src.SendReply(conv, root, text)
		return sentReplyMsg{convID: conv, rootID: root, pendingID: pid, text: text, reply: r, err: err}
	}
}

// eachRootMsg runs fn on every cached copy of the message with the given ID.
// Because replies and reply counts affect the affordance line (geometry), we
// invalidate the cache unconditionally — the mutation is the uncommon path.
func (m *Model) eachRootMsg(id string, fn func(*data.Message)) {
	for k, list := range m.messages {
		for i := range list {
			if list[i].ID == id {
				fn(&list[i])
			}
		}
		m.messages[k] = list
	}
	m.invalidateGeom()
}

// ── update ──────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		desired := m.prefs.ThreadWidth
		if desired <= 0 {
			desired = 60
		}
		m.threadWidth = m.clampThreadWidth(desired)
		m.syncComposerSizes()
		m.resizing = false
		m.selecting, m.selActive = false, false // reflow invalidates layout-relative selection coords
		return m, nil
	case pollMsg:
		cmds := []tea.Cmd{pollTick(), m.refresh(), m.markReadCmd(m.activeID)}
		if c := m.refreshTokenCmd(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)
	case themeWatchMsg:
		cmds := []tea.Cmd{themeWatchTick()}
		// Only the "omarchy" theme follows the desktop; an explicitly chosen
		// built-in stays put when the desktop re-themes.
		if m.prefs.Theme == theme.OmarchyName {
			if stamp := theme.OmarchyStamp(); stamp != m.themeStamp {
				m.themeStamp = stamp
				m.pal = theme.Resolve(m.prefs.Theme, m.prefs.Accent)
			}
		}
		return m, tea.Batch(cmds...)
	case tokenRefreshedMsg:
		if msg.err != nil {
			// Keep going on the old token: it is valid until it isn't, and the
			// next tick retries. A banner here would fire every poll.
			return m, nil
		}
		m.tokens.User, m.tokens.Refresh, m.tokens.ExpiresAt = msg.toks.User, msg.toks.Refresh, msg.toks.ExpiresAt
		if sl, ok := m.src.(*source.Slack); ok {
			sl.SetUserToken(msg.toks.User)
		}
		return m, nil
	case canvasResult:
		return m.handleCanvasResult(msg)
	case canvasEditorResult:
		return m.handleCanvasEditorResult(msg)
	case dmPollMsg:
		// The fast round covers only the recently-used head; the dormant tail
		// rides the slower dmTailPollMsg so this stays inside the budget.
		return m, tea.Batch(dmPollTick(), m.unreadCmd(m.dmHeadIDs()))
	case dmTailPollMsg:
		cmd := m.unreadCmd(m.dmTailIDs()) // reads the current rotation window…
		m.dmPollOffset += dmPollTail      // …then advance it so next round covers fresh tail
		return m, tea.Batch(dmTailPollTick(), cmd)
	case chanPollMsg:
		cmd := m.unreadCmd(m.chanIDs())
		m.chanPollOffset += chanPollTail
		return m, tea.Batch(chanPollTick(), cmd)
	case presencePollMsg:
		return m, tea.Batch(presencePollTick(), m.presenceCmd())
	case presenceUpdateMsg:
		for id, status := range msg.statuses {
			if u, ok := m.ws.Users[id]; ok {
				u.Status = status
				m.ws.Users[id] = u
			}
		}
		return m, nil
	case unreadMsg:
		// Alerts are computed before the counts are written, since "did this
		// conversation grow" is a comparison against the previous round.
		alerts := m.pollAlerts(msg.counts, msg.seq)
		for id, n := range msg.counts { // only ids actually fetched this round
			if id == m.activeID {
				continue
			}
			if m.readSeqOf[id] > msg.seq {
				continue // opened/read since this poll fired — its count is stale, ignore it
			}
			mm := m.meta[id]
			mm.Unread = n
			if n == 0 {
				mm.Mention = false
			}
			m.meta[id] = mm
			if b, ok := m.hidden[id]; ok && n > b {
				delete(m.hidden, id) // resurface: new messages since hide time
			}
		}
		return m, tea.Batch(append(alerts, m.titleCmd())...)
	case mentionScanMsg:
		if !msg.found {
			return m, nil
		}
		mm := m.meta[msg.convID]
		mm.Mention = true
		m.meta[msg.convID] = mm
		name := msg.convID
		if c, ok := m.ws.Conversation(msg.convID); ok {
			name = c.Name
		}
		return m, tea.Batch(bellCmd, m.notifyCmd(name, msg.author, msg.text))
	case markedMsg:
		// Every poll retries the mark, so a persistent failure (a token issued
		// before the mark-read scopes) would banner forever — say it once.
		if msg.err == nil || m.markWarned {
			return m, nil
		}
		m.markWarned = true
		return m, m.flash(msg.err)
	case presenceMsg:
		return m, m.flash(msg.err)
	case historyMsg:
		delete(m.loading, msg.convID)
		if msg.err != nil {
			return m, m.flash(msg.err)
		}
		cmds := m.applyHistory(msg.convID, msg.msgs)
		if n, ok := m.pendingUnread[msg.convID]; ok { // async channel open: place the ── new ── rule
			delete(m.pendingUnread, msg.convID)
			m.applyNewMark(msg.convID, n)
		}
		m.applyPendingSelect(msg.convID)
		if c := m.applyPendingThread(msg.convID); c != nil {
			cmds = append(cmds, c)
		}
		if msg.convID == m.activeID {
			cmds = append(cmds, m.markReadCmd(msg.convID))
		}
		return m, tea.Batch(cmds...)
	case reactMsg:
		return m, m.applyReact(msg)
	case editMsg:
		return m, m.flash(msg.err)
	case deleteMsg:
		return m, m.flash(msg.err)
	case searchMsg:
		return m, m.applySearch(msg)
	case joinableMsg:
		return m, m.applyJoinable(msg)
	case joinedMsg:
		return m, m.applyJoined(msg)
	case dmOpenedMsg:
		return m, m.applyDMOpened(msg)
	case leftMsg:
		return m, m.applyLeft(msg)
	case wsMsg:
		return m, m.applyWorkspace(msg)
	case repliesMsg:
		if msg.err != nil {
			return m, m.flash(msg.err)
		}
		m.applyReplies(msg.convID, msg.rootID, msg.replies)
		return m, nil
	case olderMsg:
		if msg.err != nil {
			return m, m.flash(msg.err)
		}
		m.prependHistory(msg.convID, msg.msgs)
		return m, nil
	case sentMsg:
		return m, m.applySent(msg)
	case sentReplyMsg:
		return m, m.applySentReply(msg)
	case clearErrMsg:
		if msg.seq == m.errSeq {
			m.loadErr = nil
		}
		return m, nil
	case copyToastMsg:
		if msg.seq == m.copySeq {
			m.copyToast = ""
		}
		return m, nil
	case uploadMsg:
		m.uploadNote = ""
		if msg.err != nil {
			m.pendingFiles = msg.paths // restore staged files so the user can retry
			if strings.TrimSpace(m.draft.Value()) == "" {
				m.draft.SetValue(msg.comment)
				m.syncComposerSizes()
			}
			return m, m.flash(msg.err)
		}
		return m, m.historyCmd(msg.convID) // refresh the conversation that received the upload
	case downloadMsg:
		if msg.err != nil {
			return m, m.flash(msg.err)
		}
		m.copyToast = savedToastLabel(msg.paths)
		m.copyToastX = sidebarWidth + 2
		_, _, _, innerH := m.msgViewport()
		m.copyToastY = clamp(2+innerH-1, 0, m.height-2)
		m.copySeq++
		return m, copyToastTick(m.copySeq)
	case eventMsg:
		cmds := append(m.applyEvent(msg.ev), m.titleCmd())
		if s, ok := m.src.(streamer); ok {
			cmds = append(cmds, listenEvents(s)) // keep listening
		}
		return m, tea.Batch(cmds...)
	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.selActive = false
			m.scrollMessages(-msgScrollStep)
			return m, nil
		case tea.MouseButtonWheelDown:
			m.selActive = false
			m.scrollMessages(msgScrollStep)
			return m, nil
		}
		// Left-drag text selection. Release is handled by Action regardless of
		// button: terminals on X10 mouse encoding report release with
		// MouseButtonNone, so gating release on the left button would drop it.
		switch msg.Action {
		case tea.MouseActionPress:
			if msg.Button != tea.MouseButtonLeft {
				return m, nil
			}
			m.selecting, m.selActive = false, false // a fresh click dismisses any prior selection
			m.copyToast = ""
			// Check for divider grab before text-selection.
			if m.threadOpen() && msg.Y >= 1 && msg.Y < m.height-1 {
				edge := m.width - m.threadWidth
				if msg.X >= edge-1 && msg.X <= edge {
					m.resizing = true
					return m, nil
				}
			}
			if c, pane, ok := m.cellAt(msg.X, msg.Y); ok {
				m.selAnchor, m.selHead = c, c
				m.selPane = pane
				m.selecting, m.selActive = true, true
			}
			return m, nil
		case tea.MouseActionMotion:
			if m.resizing {
				if !m.threadOpen() {
					m.resizing = false
				} else {
					m.threadWidth = m.clampThreadWidth(m.width - msg.X)
					m.invalidateGeom()
					m.syncComposerSizes()
					return m, nil
				}
			}
			if m.selecting {
				if c, pane, ok := m.cellAt(msg.X, msg.Y); ok && pane == m.selPane {
					m.selHead = c
				}
			}
			return m, nil
		case tea.MouseActionRelease:
			if m.resizing {
				m.resizing = false
				m.prefs.ThreadWidth = m.threadWidth // in-memory desired, always
				if m.persistState {
					p := m.prefs
					return m, func() tea.Msg { _ = config.Save(p); return nil }
				}
				return m, nil
			}
			if m.selecting {
				m.selecting = false
				if c, pane, ok := m.cellAt(msg.X, msg.Y); ok && pane == m.selPane {
					m.selHead = c // the release position is authoritative
				}
				text := m.selectionText()
				m.selActive = false // unselect on release
				if text != "" {
					_ = writeClipboard(text)
					m.copyToast = "copied"
					m.copyToastX = msg.X
					m.copyToastY = clamp(msg.Y+1, 0, m.height-2) // just below the release, above the status bar
					m.copySeq++
					return m, copyToastTick(m.copySeq)
				}
			}
			return m, nil
		}
		return m, nil
	case tea.KeyMsg:
		// Any keystroke dismisses a lingering mouse selection.
		if m.selActive {
			m.selActive, m.selecting = false, false
		}
		// Ctrl-K toggles the palette from any mode (Cmd never reaches a terminal).
		if msg.String() == "ctrl+k" {
			if m.canvas.open && m.canvas.busy && m.canvas.draft != nil {
				return m, nil
			}
			if m.paletteOpen {
				m.closePalette()
				return m, nil
			}
			m.closePicker() // the palette supersedes any open picker
			m.closeCanvas()
			return m, m.openPalette()
		}
		if m.helpOpen {
			return m.helpKey(msg)
		}
		if m.confirm.open {
			return m.confirmKey(msg)
		}
		if m.picker.open {
			return m.pickerKey(msg)
		}
		if m.findOpen {
			return m.findKey(msg)
		}
		if m.attachOpen {
			return m.attachKey(msg)
		}
		if m.statusTextOpen {
			return m.statusTextKey(msg)
		}
		if m.settingsOpen {
			return m.settingsKey(msg)
		}
		if m.paletteOpen {
			return m.paletteKey(msg)
		}
		if m.canvas.open {
			return m.canvasKey(msg)
		}
		if msg.Paste {
			if paths := parseDroppedPaths(string(msg.Runes)); len(paths) > 0 {
				return m, m.stageAttachments(paths)
			}
		}
		if m.insert {
			return m.insertKey(msg)
		}
		return m.normalKey(msg)
	}
	// forward non-key msgs (cursor blink) to the active input
	if m.picker.open {
		var cmd tea.Cmd
		m.pickerInput, cmd = m.pickerInput.Update(msg)
		return m, cmd
	}
	if m.findOpen {
		var cmd tea.Cmd
		m.findInput, cmd = m.findInput.Update(msg)
		return m, cmd
	}
	if m.attachOpen {
		var cmd tea.Cmd
		m.attachInput, cmd = m.attachInput.Update(msg)
		return m, cmd
	}
	if m.paletteOpen {
		var cmd tea.Cmd
		m.paletteQuery, cmd = m.paletteQuery.Update(msg)
		return m, cmd
	}
	if m.canvas.open && m.canvas.prompt != "" {
		var cmd tea.Cmd
		m.canvas.input, cmd = m.canvas.input.Update(msg)
		return m, cmd
	}

	if m.insert {
		var cmd tea.Cmd
		if m.focus == focusThread {
			m.threadDraft, cmd = m.threadDraft.Update(msg)
		} else {
			m.draft, cmd = m.draft.Update(msg)
		}
		return m, cmd
	}
	return m, nil
}

func (m Model) insertKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.suggestActive() && m.suggestKey(msg.String()) {
		return m, nil // the popup consumed the key
	}
	switch msg.String() {
	case "ctrl+c":
		return m, m.quit()
	case "esc":
		if len(m.pendingFiles) > 0 { // esc dismisses staged files first, stays in INSERT
			m.pendingFiles = nil
			m.uploadNote = ""
			return m, nil
		}
		if m.editingID != "" {
			m.cancelEdit()
		}
		m.leaveInsert()
		return m, nil
	case "enter":
		if m.focus == focusThread {
			return m, m.sendReply()
		}
		if m.editingID != "" {
			return m, m.applyEditDraft()
		}
		return m, m.sendMessage()
	}
	var cmd tea.Cmd
	if m.focus == focusThread {
		m.threadDraft, cmd = m.threadDraft.Update(msg)
	} else {
		m.draft, cmd = m.draft.Update(msg)
	}
	m.syncComposerSizes() // the content may have grown/shrunk the composer
	m.recomputeSuggest()
	return m, cmd
}

func (m Model) normalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	order := m.focusOrder()
	idx := indexOf(order, m.focus)

	switch k {
	case "ctrl+c", "q":
		return m, m.quit()
	case "ctrl+r": // manual refresh of the active channel + open thread
		return m, m.refresh()
	case "B": // Canvas browser for the active conversation
		cmd := m.openCanvasBrowser(false)
		return m, cmd
	case ",": // open the settings overlay
		m.openSettings()
		return m, nil
	case "?": // keymap cheat sheet
		m.helpOpen = true
		return m, nil
	case "]": // jump to the next conversation with unread
		return m, m.jumpUnread(1)
	case "[": // jump to the previous conversation with unread
		return m, m.jumpUnread(-1)

	case "tab":
		m.focus = order[(idx+1)%len(order)]
	case "shift+tab":
		m.focus = order[(idx-1+len(order))%len(order)]
	case "h", "ctrl+h", "left":
		if idx > 0 {
			m.focus = order[idx-1]
		}
	case "l", "ctrl+l", "right":
		if idx < len(order)-1 {
			m.focus = order[idx+1]
		}

	case "g":
		if !m.gPending.IsZero() && time.Since(m.gPending) < 500*time.Millisecond {
			m.gPending = time.Time{}
			m.jumpTop()
			if m.atHistoryTop() {
				return m, m.loadOlderCmd(m.activeID)
			}
		} else {
			m.gPending = time.Now()
		}
		return m, nil
	case "G":
		m.jumpBottom()

	case "j", "down":
		if m.focus == focusMessages {
			m.messagesDown()
		} else {
			m.moveSel(1)
		}
	case "k", "up":
		if m.focus == focusMessages {
			if m.atHistoryTop() && m.msgExtra == 0 { // top of the list: pull older history
				return m, m.loadOlderCmd(m.activeID)
			}
			m.messagesUp()
		} else {
			m.moveSel(-1)
		}
	case "ctrl+d":
		m.scrollMessages(m.pageJump())
	case "ctrl+u":
		m.scrollMessages(-m.pageJump())

	case "enter":
		switch m.focus {
		case focusSidebar:
			if it := m.sideItems()[m.sideSel]; !it.Header {
				return m, m.openChannel(it.Conv.ID)
			}
		case focusMessages:
			if msgs := m.curMsgs(); m.msgSel < len(msgs) {
				return m, m.openThread(msgs[m.msgSel].ID)
			}
		case focusThread:
			return m, m.enterInsert(focusThread)
		}

	case "t":
		if m.focus == focusMessages {
			if msgs := m.curMsgs(); m.msgSel < len(msgs) {
				return m, m.openThread(msgs[m.msgSel].ID)
			}
		}

	case "a": // react to the selected message
		if msg, ok := m.selectedMsg(); ok && !isPending(msg.ID) {
			return m, m.openReactionPicker(msg.ID)
		}

	case "e": // edit your own selected message
		if msg, ok := m.selectedMsg(); ok && msg.UserID == m.ws.MeID && !isPending(msg.ID) {
			return m, m.startEdit(msg)
		}

	case "d": // dd deletes your own selected message (with confirm)
		if !m.dPending.IsZero() && time.Since(m.dPending) < 500*time.Millisecond {
			m.dPending = time.Time{}
			if msg, ok := m.selectedMsg(); ok && msg.UserID == m.ws.MeID && !isPending(msg.ID) {
				id := msg.ID
				m.openConfirm("delete this message?", func(mm *Model) tea.Cmd {
					return mm.deleteMessage(id)
				})
			}
		} else {
			m.dPending = time.Now()
		}
		return m, nil

	case "o": // open the selected message's link(s)
		if m.focus == focusMessages {
			return m, m.openMsgLinks()
		}

	case "S": // save the selected message's file(s) to ~/Downloads
		if m.focus == focusMessages {
			return m, m.downloadFiles()
		}

	case "A": // open the attach-file prompt
		return m, m.openAttach()

	case "/": // find within the loaded channel history
		return m, m.openFind()
	case "n":
		if m.focus == focusMessages && m.searchQuery != "" {
			return m, m.jumpFind(1)
		}
	case "N":
		if m.focus == focusMessages && m.searchQuery != "" {
			return m, m.jumpFind(-1)
		}

	case "s": // workspace-wide message search
		return m, m.openSearchPicker()

	case "T": // threads inbox: every thread you participate in
		return m, m.openThreadsPicker()

	case "y": // yank the selected message or thread reply to the clipboard
		if m.focus == focusThread {
			if r, ok := m.selectedReply(); ok {
				if err := writeClipboard(r.Text); err != nil {
					return m, m.flash(fmt.Errorf("clipboard: %w", err))
				}
			}
		} else if msg, ok := m.selectedMsg(); ok {
			if err := writeClipboard(msg.Text); err != nil {
				return m, m.flash(fmt.Errorf("clipboard: %w", err))
			}
		}

	case "i":
		which := focusMessages
		if m.focus == focusThread {
			which = focusThread
		}
		if m.focus == focusSidebar {
			m.focus = focusMessages
		}
		return m, m.enterInsert(which)

	case "r":
		if m.focus == focusThread {
			return m, m.enterInsert(focusThread)
		}
		if m.focus == focusMessages {
			if msgs := m.curMsgs(); m.msgSel < len(msgs) {
				openCmd := m.openThread(msgs[m.msgSel].ID)
				return m, tea.Batch(openCmd, m.enterInsert(focusThread))
			}
		}

	case "esc":
		if m.loadErr != nil { // dismiss the error banner first
			m.loadErr = nil
			return m, nil
		}
		if m.threadOpen() {
			m.closeThread()
		}

	case "x": // hide / un-hide the conversation
		target := m.activeID
		if m.focus == focusSidebar {
			items := m.sideItems()
			if m.sideSel >= len(items) || items[m.sideSel].Header {
				return m, nil // cursor isn't on a conversation row
			}
			target = items[m.sideSel].Conv.ID
		}
		return m, m.toggleHide(target)
	}

	// any non-g key cancels a pending gg (same for dd)
	if k != "g" {
		m.gPending = time.Time{}
	}
	if k != "d" {
		m.dPending = time.Time{}
	}
	return m, nil
}

func (m *Model) moveSel(delta int) {
	switch m.focus {
	case focusSidebar:
		sel := m.selectable()
		if len(sel) == 0 {
			return // everything hidden / empty workspace — nothing to move to
		}
		pos := indexOfInt(sel, m.sideSel)
		pos = clamp(pos+delta, 0, len(sel)-1)
		m.sideSel = sel[pos]
	case focusMessages:
		m.msgSel = clamp(m.msgSel+delta, 0, len(m.curMsgs())-1)
		m.msgExtra = 0
	case focusThread:
		if root, ok := m.threadRoot(); ok {
			m.threadSel = clamp(m.threadSel+delta, 0, len(root.Replies)-1)
		}
	}
}

func (m *Model) jumpTop() {
	switch m.focus {
	case focusSidebar:
		if sel := m.selectable(); len(sel) > 0 {
			m.sideSel = sel[0]
		}
	case focusMessages:
		m.msgSel, m.msgExtra = 0, 0
	case focusThread:
		m.threadSel = 0
	}
}

func (m *Model) jumpBottom() {
	switch m.focus {
	case focusSidebar:
		if sel := m.selectable(); len(sel) > 0 {
			m.sideSel = sel[len(sel)-1]
		}
	case focusMessages:
		m.msgSel, m.msgExtra = max(0, len(m.curMsgs())-1), 0
	case focusThread:
		if root, ok := m.threadRoot(); ok {
			m.threadSel = max(0, len(root.Replies)-1)
		}
	}
}

// ── async send reconciliation, error flash, notifications ──────────────────

// copyToastTick auto-dismisses the copy toast after a short delay.
func copyToastTick(seq int) tea.Cmd {
	return tea.Tick(1200*time.Millisecond, func(time.Time) tea.Msg { return copyToastMsg{seq} })
}

// flash surfaces an error in the banner and schedules its auto-clear. Returns
// nil for a nil error, so handlers can `return m, m.flash(msg.err)` directly.
func (m *Model) flash(err error) tea.Cmd {
	if err == nil {
		return nil
	}
	m.loadErr = err
	m.errSeq++
	seq := m.errSeq
	return tea.Tick(8*time.Second, func(time.Time) tea.Msg { return clearErrMsg{seq} })
}

// applySent reconciles an optimistic message with the backend's answer: swap in
// the real message, or roll back and restore the draft on error.
func (m *Model) applySent(msg sentMsg) tea.Cmd {
	list := m.messages[msg.convID]
	idx := -1
	for i := range list {
		if list[i].ID == msg.pendingID {
			idx = i
			break
		}
	}
	if msg.err != nil {
		if idx >= 0 {
			m.messages[msg.convID] = append(list[:idx], list[idx+1:]...)
			if msg.convID == m.activeID {
				m.msgSel = clamp(m.msgSel, 0, max(0, len(m.messages[msg.convID])-1))
				m.invalidateGeom()
			}
		}
		if msg.convID == m.activeID && strings.TrimSpace(m.draft.Value()) == "" {
			m.draft.SetValue(msg.text) // give the user their words back
			m.syncComposerSizes()
		}
		return m.flash(msg.err)
	}
	if idx >= 0 {
		already := false // a poll may have delivered the real message first
		for i := range list {
			if list[i].ID == msg.msg.ID {
				already = true
				break
			}
		}
		if already {
			m.messages[msg.convID] = append(list[:idx], list[idx+1:]...)
		} else {
			list[idx] = msg.msg
			m.messages[msg.convID] = list
		}
		if msg.convID == m.activeID {
			m.invalidateGeom()
		}
	}
	// Posting through the API doesn't advance your own read marker the way the
	// official clients do, so a conversation you just sent in stays unread
	// everywhere else. Mark it now instead of waiting for the next poll — which
	// the user may never reach if they switch conversations or quit.
	if msg.convID == m.activeID {
		return m.markReadCmd(msg.convID)
	}
	return nil
}

// applySentReply is applySent for thread replies.
func (m *Model) applySentReply(msg sentReplyMsg) tea.Cmd {
	if msg.err != nil {
		m.eachRootMsg(msg.rootID, func(root *data.Message) {
			for i := range root.Replies {
				if root.Replies[i].ID == msg.pendingID {
					root.Replies = append(root.Replies[:i], root.Replies[i+1:]...)
					break
				}
			}
		})
		if m.threadRootID == msg.rootID && strings.TrimSpace(m.threadDraft.Value()) == "" {
			m.threadDraft.SetValue(msg.text)
		}
		return m.flash(msg.err)
	}
	m.eachRootMsg(msg.rootID, func(root *data.Message) {
		// A live event (or poll) may have delivered the real reply already; if
		// so, drop the pending copy instead of swapping (mirrors applySent).
		already := false
		for i := range root.Replies {
			if root.Replies[i].ID == msg.reply.ID {
				already = true
				break
			}
		}
		for i := range root.Replies {
			if root.Replies[i].ID == msg.pendingID {
				if already {
					root.Replies = append(root.Replies[:i], root.Replies[i+1:]...)
				} else {
					root.Replies[i] = msg.reply
				}
				return
			}
		}
	})
	return nil
}

// titleCmd reflects the total unread count in the terminal title (when changed).
func (m *Model) titleCmd() tea.Cmd {
	n := 0
	for _, mm := range m.meta {
		n += mm.Unread
	}
	if n == m.lastTitleN {
		return nil
	}
	m.lastTitleN = n
	title := "slack-tui"
	if n > 0 {
		title = fmt.Sprintf("slack-tui (%d)", n)
	}
	return tea.SetWindowTitle(title)
}

// writeClipboard is the seam for the yank path. Tests replace it: the real
// implementation shells out via PATH and would overwrite the developer's actual
// clipboard with mock message text, which `go test ./internal/app/` did.
var writeClipboard = clipboard.WriteAll

// bellCmd rings the terminal bell — BEL prints nothing, so it can't disturb the
// alt screen. Terminals surface it as the usual badge/sound.
func bellCmd() tea.Msg {
	_, _ = os.Stdout.WriteString("\a")
	return nil
}

// ── small utilities ─────────────────────────────────────────────────────────

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return 0
}

func indexOfInt(s []int, v int) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return 0
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// gap renders a 1-col vertical separator filled with the app background.
func (m Model) gap(height int) string {
	return lipgloss.NewStyle().Width(1).Height(height).Background(m.pal.Bg).Render("")
}

// clampThreadWidth keeps the thread pane within usable bounds.
// minW=34 keeps the thread readable; maxW ensures center gets at least 24 cols.
func (m Model) clampThreadWidth(w int) int {
	minW, maxW := 34, m.width-sidebarWidth-2-24
	if maxW < minW {
		maxW = minW
	}
	return clamp(w, minW, maxW)
}

// overlay composites an over block onto base at cell (x,y), ansi-aware.
func overlay(base, over string, x, y int) string {
	baseLines := strings.Split(base, "\n")
	overLines := strings.Split(over, "\n")
	for i, ol := range overLines {
		row := y + i
		if row < 0 || row >= len(baseLines) {
			continue
		}
		baseLines[row] = spliceLine(baseLines[row], ol, x)
	}
	return strings.Join(baseLines, "\n")
}

// spliceLine replaces the [x, x+width(over)) slice of base with over, ansi-aware,
// padding the left segment with spaces if base is shorter than x.
func spliceLine(base, over string, x int) string {
	ow := lipgloss.Width(over)
	left := ansi.Truncate(base, x, "")
	if lw := lipgloss.Width(left); lw < x {
		left += strings.Repeat(" ", x-lw)
	}
	right := ansi.TruncateLeft(base, x+ow, "")
	return left + "\x1b[0m" + over + "\x1b[0m" + right
}

// windowLines slices lines to a height-h window that keeps [selStart,selEnd] visible.
func windowLines(lines []string, selStart, selEnd, h int) []string {
	if h <= 0 {
		return nil
	}
	if len(lines) <= h {
		return lines
	}
	top := windowBaseTop(selStart, selEnd, h, len(lines))
	return lines[top : top+h]
}

// windowBaseTop is the viewport top that keeps the selection [selStart,selEnd]
// visible (anchored to its bottom, but never past its top).
func windowBaseTop(selStart, selEnd, h, total int) int {
	if h <= 0 || total <= h {
		return 0
	}
	top := 0
	if selEnd >= h {
		top = selEnd - h + 1
	}
	if selStart < top {
		top = selStart
	}
	return clamp(top, 0, total-h)
}
