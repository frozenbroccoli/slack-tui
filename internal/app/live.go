package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kurenn/slack-tui/internal/auth"
	"github.com/kurenn/slack-tui/internal/config"
	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/notify"
	"github.com/kurenn/slack-tui/internal/source"
)

// Poll intervals are a rate-limit budget, not preferences. conversations.history
// is Slack Tier 3 — roughly 50 requests/minute — and every unread count costs
// one call. Measured against a real workspace (~30 channels, 15 polled DMs) the
// previous values spent ~51/min: already over the ceiling, so rounds were
// aborting mid-sweep on 429s and leaving counts stale. That is the same failure
// v0.5.2 fixed for DMs, still present for channels, which polled every channel
// every round with no bound at all.
//
// The budget is now spent where it changes what the user sees: DMs, which are
// the thing that notifies. Channels are bounded like DMs and polled less often —
// a stale badge on a channel nobody is looking at is the cheapest thing to give
// up, and far cheaper than rate-limiting the whole sweep into uselessness.
//
//	DM head (5)           25s   12.0/min
//	DM rotating tail (5)  25s   12.0/min
//	channels (12)        120s    6.0/min
//	active conversation    8s    7.5/min
//	presence              60s    1.0/min
//	                          ≈ 38.5/min
const dmPollInterval = 25 * time.Second

// dmTailPollInterval walks older DMs at the same cadence as recent ones.
// Splitting the ten-request budget equally keeps all DMs making progress.
const dmTailPollInterval = dmPollInterval

const dmDiscoveryInterval = time.Minute

// chanPollInterval refreshes channel unread counts when Socket Mode isn't
// running (user token only) — without it the sidebar badges would freeze at
// their startup values.
const chanPollInterval = 120 * time.Second

// presencePollInterval refreshes the presence dots for DM partners. Tier-3
// rate limit is generous for the small DM set, so 60s is safe and responsive.
const presencePollInterval = 60 * time.Second

type (
	dmPollMsg       struct{}
	dmDiscoveryMsg  struct{}
	dmDiscoveredMsg struct {
		convs []data.Conversation
		err   error
	}
	dmTailPollMsg   struct{}
	chanPollMsg     struct{}
	presencePollMsg struct{}
	// unreadMsg carries the unread counts actually fetched this round (a
	// rate-limit abort returns a partial map; absent ids stay untouched).
	unreadMsg struct {
		counts map[string]int
		err    error
		seq    int // m.readSeq when the poll fired; results for convs read since are stale
	}
	// presenceUpdateMsg carries freshly fetched presence statuses for DM
	// partners (id → "online" | "away"). Distinct from presenceMsg (which
	// pushes the user's OWN presence and carries only an error).
	presenceUpdateMsg struct{ statuses map[string]string }
)

func dmDiscoveryTick() tea.Cmd {
	return tea.Tick(dmDiscoveryInterval, func(time.Time) tea.Msg { return dmDiscoveryMsg{} })
}

func dmTailPollTick() tea.Cmd {
	return tea.Tick(dmTailPollInterval, func(time.Time) tea.Msg { return dmTailPollMsg{} })
}

func dmPollTick() tea.Cmd {
	return tea.Tick(dmPollInterval, func(time.Time) tea.Msg { return dmPollMsg{} })
}

func chanPollTick() tea.Cmd {
	return tea.Tick(chanPollInterval, func(time.Time) tea.Msg { return chanPollMsg{} })
}

func presencePollTick() tea.Cmd {
	return tea.Tick(presencePollInterval, func(time.Time) tea.Msg { return presencePollMsg{} })
}

type presenceMsg struct{ err error }

// setPresenceCmd pushes the current presence (online/away/dnd) to the backend,
// surfacing any error (e.g. a missing users:write scope).
func (m Model) setPresenceCmd() tea.Cmd {
	src, status := m.src, m.myStatus
	return func() tea.Msg { return presenceMsg{src.SetPresence(status)} }
}

// markedMsg reports a mark-read attempt's outcome. The result used to be
// discarded, which made a broken read-sync invisible: the sidebar badge clears
// locally whatever the backend says, so a rejected mark only showed up as Slack
// web/desktop stubbornly keeping the conversation unread. The update loop
// surfaces the first failure and then stays quiet.
type markedMsg struct{ err error }

// markReadCmd tells the backend the conversation is read up to its latest
// message, so unread polls/Socket Mode don't re-flag it — and neither do
// Slack's own clients.
func (m Model) markReadCmd(convID string) tea.Cmd {
	ts := lastRealTS(m.messages[convID])
	if ts == "" {
		return nil
	}
	src := m.src
	return func() tea.Msg { return markedMsg{src.MarkRead(convID, ts)} }
}

// lastRealTS returns the newest message ID that is an actual Slack timestamp.
// An optimistic send carries a local "pending-N" ID until the backend acks it;
// conversations.mark rejects that, so a mark firing in the gap between send and
// ack (the 6s poll, or leaving the conversation right after sending) would be
// thrown away. Falling back to the last acked message keeps it valid.
func lastRealTS(msgs []data.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if !isPending(msgs[i].ID) {
			return msgs[i].ID
		}
	}
	return ""
}

// DM unread can't be pushed over Socket Mode, so it's polled. Fanning out one
// conversations.history call per DM doesn't scale: a large DM list (100s) blows
// Slack's per-app history rate limit in a single round, 429s, aborts mid-sweep,
// and leaves most counts stale. Poll five recently-used DMs and rotate five
// older DMs at the same 25-second cadence to keep every thread moving.
const (
	dmPollHead = 5
	dmPollTail = 5
	// Channels get the same treatment DMs got in v0.5.2: the ones you actually
	// use every round, the rest on a rotation.
	chanPollHead = 8
	chanPollTail = 4
)

// dmIDs / chanIDs list the conversations to poll, excluding the active one.
// dmHeadIDs is the fast round: the DMs you actually use. These are what carry
// notification latency, so they get the short interval.
func (m Model) dmHeadIDs() []string {
	ordered := m.dmsByRecency() // recent-first; already excludes the active conv
	if len(ordered) <= dmPollHead {
		return ordered
	}
	return ordered[:dmPollHead]
}

// dmTailIDs is the rotating window over older DMs, refreshed every 25 seconds.
func (m Model) dmTailIDs() []string {
	ordered := m.dmsByRecency()
	if len(ordered) <= dmPollHead {
		return nil
	}
	rest := ordered[dmPollHead:]
	out := make([]string, 0, dmPollTail)
	start := m.dmPollOffset % len(rest)
	for i := 0; i < dmPollTail && i < len(rest); i++ {
		out = append(out, rest[(start+i)%len(rest)])
	}
	return out
}

// dmsByRecency returns every DM's ID ordered most-recently-opened first (then
// load order), skipping the active conversation — the priority order the unread
// poll walks so the DMs you actually use stay fresh.
func (m Model) dmsByRecency() []string {
	isDM := make(map[string]bool, len(m.ws.DMs))
	for _, d := range m.ws.DMs {
		if d.ID != m.activeID {
			isDM[d.ID] = true
		}
	}
	ordered := make([]string, 0, len(isDM))
	seen := make(map[string]bool, len(isDM))
	for _, id := range m.recent {
		if isDM[id] && !seen[id] {
			ordered = append(ordered, id)
			seen[id] = true
		}
	}
	for _, d := range m.ws.DMs {
		if isDM[d.ID] && !seen[d.ID] {
			ordered = append(ordered, d.ID)
			seen[d.ID] = true
		}
	}
	return ordered
}

// dmPartnerIDs collects the UserID of each 1:1 DM, skipping group DMs (mpims)
// whose UserID is empty. These are the only dots visible in the sidebar.
func (m Model) dmPartnerIDs() []string {
	ids := make([]string, 0, len(m.ws.DMs))
	for _, d := range m.ws.DMs {
		if d.UserID != "" {
			ids = append(ids, d.UserID)
		}
	}
	return ids
}

// presenceCmd fetches current presence for all DM partners off the UI thread.
func (m Model) presenceCmd() tea.Cmd {
	ids := m.dmPartnerIDs()
	if len(ids) == 0 {
		return nil
	}
	src := m.src
	return func() tea.Msg {
		statuses, _ := src.Presence(ids)
		return presenceUpdateMsg{statuses}
	}
}

func (m Model) chanIDs() []string {
	ids := make([]string, 0, len(m.ws.Channels))
	for _, c := range m.ws.Channels {
		if c.ID != m.activeID {
			ids = append(ids, c.ID)
		}
	}
	return boundedRotation(ids, chanPollHead, chanPollTail, m.chanPollOffset)
}

// boundedRotation returns the first head entries plus a rotating window of
// tail entries from the remainder, so a long list still refreshes completely
// over several rounds without any single round exceeding the budget.
func boundedRotation(ids []string, head, tail, offset int) []string {
	if len(ids) <= head+tail {
		return ids
	}
	rest := ids[head:]
	out := make([]string, 0, head+tail)
	out = append(out, ids[:head]...)
	start := offset % len(rest)
	for i := 0; i < tail; i++ {
		out = append(out, rest[(start+i)%len(rest)])
	}
	return out
}

// unreadCmd fetches unread counts for the given conversations, concurrently,
// off the UI thread. On a rate-limit error it stops issuing further calls and
// reports only what it fetched.
func (m Model) unreadCmd(ids []string) tea.Cmd {
	if len(ids) == 0 {
		return nil
	}
	src, seq := m.src, m.readSeq
	return func() tea.Msg {
		counts := map[string]int{}
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var limited atomic.Bool
		var firstErr error
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if limited.Load() {
					return
				}
				n, err := src.Unread(id)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("unread refresh for %s failed: %w", id, err)
					}
					mu.Unlock()
					if source.IsRateLimited(err) {
						limited.Store(true) // back off; finish this round with what we have
					}
					return
				}
				mu.Lock()
				counts[id] = n
				mu.Unlock()
			}(id)
		}
		wg.Wait()
		return unreadMsg{counts: counts, seq: seq, err: firstErr}
	}
}

// markAllReadCmd marks each conversation read on the backend up to its latest
// message (fetched if not cached). Used by the palette's "Mark all as read".
func (m Model) markAllReadCmd(ids []string) tea.Cmd {
	if len(ids) == 0 {
		return nil
	}
	src := m.src
	latest := map[string]string{}
	for _, id := range ids {
		if ts := lastRealTS(m.messages[id]); ts != "" {
			latest[id] = ts
		}
	}
	return func() tea.Msg {
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ts := latest[id]
				if ts == "" {
					msgs, err := src.History(id)
					if err != nil || len(msgs) == 0 {
						return
					}
					ts = lastRealTS(msgs)
				}
				if err := src.MarkRead(id, ts); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}(id)
		}
		wg.Wait()
		return markedMsg{firstErr}
	}
}

// streamer is a Source that pushes real-time events (Socket Mode).
type streamer interface {
	Events() <-chan source.Event
}

// eventMsg carries one real-time Socket Mode event into the update loop.
type eventMsg struct{ ev source.Event }

// listenEvents blocks on the next stream event and delivers it as a msg.
func listenEvents(s streamer) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.Events()
		if !ok {
			return nil
		}
		return eventMsg{ev}
	}
}

// applyEvent applies a live Socket Mode message event. For a non-active
// conversation it bumps unread (and mention) so the sidebar dot lights up
// immediately. For the active conversation it appends the message (or thread
// reply) so it shows instantly instead of waiting for the 6s poll. It returns
// follow-up cmds (a bell when warranted, a mark-read to keep the backend in
// step); the caller batches in titleCmd and re-arms the listener.
func (m *Model) applyEvent(ev source.Event) []tea.Cmd {
	if ev.ConvID == "" {
		return nil
	}
	if ev.ConvID != m.activeID {
		return m.applyInactiveEvent(ev)
	}
	if ev.ThreadTS != "" {
		return m.applyActiveReply(ev)
	}
	return m.applyActiveRoot(ev)
}

// applyInactiveEvent is the original behavior: light up unread/mention on a
// conversation the user isn't looking at, ringing the bell for mentions and DMs.
func (m *Model) applyInactiveEvent(ev source.Event) []tea.Cmd {
	conv, ok := m.ws.Conversation(ev.ConvID)
	if !ok {
		return nil
	}
	if ev.Msg.UserID == m.ws.MeID {
		return nil // our own message from another client
	}
	delete(m.hidden, ev.ConvID) // a new message resurfaces a hidden conversation
	meta := m.meta[ev.ConvID]
	// Only main-timeline messages bump the channel badge — matching Slack's own
	// count and the authoritative last_read derivation (which sees root messages
	// only). Plain thread replies live in the Threads view, not the channel
	// badge; counting them here ballooned thread-heavy channels for Socket Mode
	// users. A reply that @-mentions you still counts, so it surfaces.
	threadReply := ev.ThreadTS != "" && ev.ThreadTS != ev.Msg.ID
	if !threadReply || ev.Msg.MentionsMe {
		meta.Unread++
	}
	if ev.Msg.MentionsMe {
		meta.Mention = true
	}
	m.meta[ev.ConvID] = meta
	if ev.Msg.MentionsMe || conv.Type == "dm" {
		return []tea.Cmd{bellCmd, m.notifyCmd(conv.Name, ev.Msg.UserID, ev.Msg.Text)}
	}
	return nil
}

// applyActiveRoot appends a root message to the conversation in view. If history
// isn't cached yet (or is in flight) it does nothing — the fetch will include
// this message. The message is deduped by ID (the poll or our own optimistic
// send may already have it). No bell: the user is looking at this conversation.
func (m *Model) applyActiveRoot(ev source.Event) []tea.Cmd {
	msgs, cached := m.messages[ev.ConvID]
	if !cached || m.loading[ev.ConvID] {
		return nil
	}
	for i := range msgs {
		if msgs[i].ID == ev.Msg.ID {
			return nil // already present (poll/optimistic send)
		}
	}
	atBottom := len(msgs) == 0 || m.msgSel >= len(msgs)-1
	m.messages[ev.ConvID] = append(msgs, ev.Msg)
	m.invalidateGeom()
	if atBottom {
		m.msgSel = len(m.messages[ev.ConvID]) - 1
		m.msgExtra = 0
	}
	return []tea.Cmd{m.markReadCmd(ev.ConvID)}
}

// applyActiveReply lands a thread reply under its root in the conversation in
// view. The reply is deduped by ID against the root's replies; ReplyCount is
// kept consistent. It rings the bell only when the root is the user's own
// message and that thread isn't currently open (an agent answered a thread I
// started while I look elsewhere in the conversation). If the root isn't cached,
// it does nothing — the poll will true it up.
func (m *Model) applyActiveReply(ev source.Event) []tea.Cmd {
	found, mine := false, false
	m.eachRootMsg(ev.ThreadTS, func(root *data.Message) {
		found = true
		mine = root.UserID == m.ws.MeID
		for i := range root.Replies {
			if root.Replies[i].ID == ev.Msg.ID {
				return // already present (poll/optimistic send)
			}
		}
		root.Replies = append(root.Replies, data.Reply{
			ID: ev.Msg.ID, UserID: ev.Msg.UserID, Time: ev.Msg.Time, Text: ev.Msg.Text,
		})
		root.ReplyCount = max(root.ReplyCount+1, len(root.Replies))
	})
	if !found {
		return nil
	}
	if mine && m.threadRootID != ev.ThreadTS {
		name := "thread"
		if c, ok := m.ws.Conversation(ev.ConvID); ok {
			name = c.Name
		}
		return []tea.Cmd{bellCmd, m.notifyCmd(name+" (thread)", ev.Msg.UserID, ev.Msg.Text)}
	}
	return nil
}

// pollInterval is how often the active channel (and open thread) are refreshed.
const pollInterval = 6 * time.Second

type (
	pollMsg    struct{}
	historyMsg struct {
		convID string
		msgs   []data.Message
		err    error
	}
	olderMsg struct {
		convID string
		msgs   []data.Message
		err    error
	}
	repliesMsg struct {
		convID, rootID string
		replies        []data.Reply
		err            error
	}
	// sentMsg / sentReplyMsg reconcile an optimistic send with the backend.
	sentMsg struct {
		convID, pendingID, text string
		msg                     data.Message
		err                     error
	}
	sentReplyMsg struct {
		convID, rootID, pendingID, text string
		reply                           data.Reply
		err                             error
	}
	// clearErrMsg removes the error banner if no newer error replaced it.
	clearErrMsg struct{ seq int }
	// copyToastMsg clears the "copied" toast if no newer copy replaced it.
	copyToastMsg struct{ seq int }
)

func pollTick() tea.Cmd {
	return tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
}

// historyCmd fetches a conversation's messages off the UI thread.
func (m Model) historyCmd(convID string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		msgs, err := src.History(convID)
		return historyMsg{convID, msgs, err}
	}
}

// repliesCmd fetches a thread's replies off the UI thread.
func (m Model) repliesCmd(convID, rootID string) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		reps, err := src.Replies(convID, rootID)
		return repliesMsg{convID, rootID, reps, err}
	}
}

// refresh re-fetches the active channel and the open thread.
func (m Model) refresh() tea.Cmd {
	cmds := []tea.Cmd{m.historyCmd(m.activeID)}
	if m.threadOpen() {
		cmds = append(cmds, m.repliesCmd(m.activeID, m.threadRootID))
	}
	return tea.Batch(cmds...)
}

// applyHistory replaces a conversation's cached messages, carrying over any
// already-loaded thread replies and preserving the selection (following the
// bottom if the user was already there). It returns follow-up cmds: bots and
// agents commonly answer in a thread under your message — when a poll shows a
// new reply on a thread you started, fetch it (so the inline preview renders)
// and ring the bell, otherwise the answer is just a silent "└ 1 reply" line.
func (m *Model) applyHistory(convID string, msgs []data.Message) []tea.Cmd {
	old := m.messages[convID]
	loaded := map[string][]data.Reply{}
	prevCount := map[string]int{}
	for _, o := range old {
		if len(o.Replies) > 0 {
			loaded[o.ID] = o.Replies
		}
		prevCount[o.ID] = o.ReplyCount
	}
	var cmds []tea.Cmd
	rung := false
	for _, msg := range msgs {
		prev, known := prevCount[msg.ID]
		if known && msg.ReplyCount > prev && msg.UserID == m.ws.MeID {
			cmds = append(cmds, m.repliesCmd(convID, msg.ID))
			if !rung && msg.ID != m.threadRootID { // an open thread is already in view
				name := "thread"
				if c, ok := m.ws.Conversation(convID); ok {
					name = c.Name
				}
				cmds = append(cmds, bellCmd, m.notifyCmd(name+" (thread)", "", "new reply to your message"))
				rung = true
			}
		}
	}
	for i := range msgs {
		if r, ok := loaded[msgs[i].ID]; ok {
			msgs[i].Replies = r
		}
	}
	// Carry over optimistic sends the backend hasn't acked yet — a poll
	// snapshot taken between send and ack doesn't contain them.
	for _, o := range old {
		if strings.HasPrefix(o.ID, pendingPrefix) {
			msgs = append(msgs, o)
		}
	}

	if convID != m.activeID {
		m.messages[convID] = msgs
		return cmds
	}
	atBottom := len(old) == 0 || m.msgSel >= len(old)-1
	selID := ""
	if m.msgSel >= 0 && m.msgSel < len(old) {
		selID = old[m.msgSel].ID
	}
	m.messages[convID] = msgs
	m.invalidateGeom()
	if atBottom {
		m.msgSel = max(0, len(msgs)-1)
		m.msgExtra = 0
	} else {
		m.msgSel = indexOfMsg(msgs, selID)
	}
	return cmds
}

// loadOlderCmd fetches the page of history before the oldest cached message.
func (m Model) loadOlderCmd(convID string) tea.Cmd {
	msgs := m.messages[convID]
	if len(msgs) == 0 {
		return nil
	}
	oldest := msgs[0].ID
	src := m.src
	return func() tea.Msg {
		older, err := src.HistoryBefore(convID, oldest)
		return olderMsg{convID, older, err}
	}
}

// prependHistory prepends an older page, keeping the selection on the same
// message; an empty page marks the conversation fully loaded.
func (m *Model) prependHistory(convID string, older []data.Message) {
	if len(older) == 0 {
		m.fullyLoaded[convID] = true
		return
	}
	m.messages[convID] = append(older, m.messages[convID]...)
	if convID == m.activeID {
		m.msgSel += len(older)
		m.invalidateGeom()
	}
}

// applyReplies updates a thread root's replies in the cache.
func (m *Model) applyReplies(convID, rootID string, replies []data.Reply) {
	list := m.messages[convID]
	for i := range list {
		if list[i].ID == rootID {
			list[i].Replies = replies
			list[i].ReplyCount = len(replies)
		}
	}
	m.messages[convID] = list
	if convID == m.activeID {
		m.invalidateGeom() // reply count + preview text affect the affordance line
	}
	if m.threadRootID == rootID {
		m.threadSel = clamp(m.threadSel, 0, max(0, len(replies)-1))
		m.seenReplies[convID+"|"+rootID] = len(replies) // viewing it clears the inbox "new" badge
	}
}

func indexOfMsg(msgs []data.Message, id string) int {
	for i := range msgs {
		if msgs[i].ID == id {
			return i
		}
	}
	return max(0, len(msgs)-1)
}

// tokenRefreshedMsg carries the outcome of a mid-session token refresh.
type tokenRefreshedMsg struct {
	toks config.Tokens
	err  error
}

// refreshTokenCmd renews the rotating user token when it is close to expiring,
// so a session outliving Slack's ~12-hour token keeps working. Returns nil when
// nothing is due, which is the common case on every poll.
//
// Persisting before reporting is deliberate: the spent refresh token is dead
// the moment Slack answers, so the new one must reach disk even if this process
// dies immediately after.
func (m Model) refreshTokenCmd() tea.Cmd {
	if os.Getenv("SLACK_USER_TOKEN") != "" || !auth.Due(m.tokens) {
		return nil
	}
	refresh := m.tokens.Refresh
	return func() tea.Msg {
		creds := config.LoadOAuthCreds()
		if !creds.Ready() {
			return tokenRefreshedMsg{err: fmt.Errorf("no client id to refresh with")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		toks, err := auth.Refresh(ctx, creds, refresh)
		if err != nil {
			return tokenRefreshedMsg{err: err}
		}
		if err := config.SaveRefreshed(toks); err != nil {
			return tokenRefreshedMsg{err: err}
		}
		return tokenRefreshedMsg{toks: toks}
	}
}

// themeWatchInterval is how often we look for a desktop theme change. It's a
// single stat of colors.toml, so it can be frequent enough that re-theming
// looks instant without costing anything.
const themeWatchInterval = 2 * time.Second

// themeWatchMsg asks the model to re-read the desktop theme.
type themeWatchMsg struct{}

// themeWatchTick polls rather than hooking `omarchy hook install theme-set`,
// which would need per-machine setup and a way to signal every running client.
// Polling keeps this working out of the box on any machine.
func themeWatchTick() tea.Cmd {
	return tea.Tick(themeWatchInterval, func(time.Time) tea.Msg { return themeWatchMsg{} })
}

// notifyCmd posts a desktop notification for a message the user isn't looking
// at. It rides alongside bellCmd at exactly the same call sites, so there is one
// definition of "worth interrupting for" — mentions, DMs, and replies to your
// own threads — rather than two that can drift apart.
//
// Returns nil when notifications are off, which tea.Batch discards.
func (m Model) notifyCmd(convName, userID, text string) tea.Cmd {
	if !m.prefs.Notifications() {
		return nil
	}
	title := convName
	if body := strings.TrimSpace(text); body != "" {
		if who := m.displayName(userID); who != "" {
			body = who + ": " + body
		}
		return func() tea.Msg { notify.Send(title, body); return nil }
	}
	return nil
}

// displayName resolves a user id to a handle for the notification body, since a
// raw U0… id would tell the reader nothing.
func (m Model) displayName(userID string) string {
	if userID == "" {
		return ""
	}
	if u, ok := m.ws.Users[userID]; ok {
		return u.Handle
	}
	return ""
}

// ── alerts without Socket Mode ───────────────────────────────────────────────

// mentionScanCap bounds how many channels one poll round will fetch history for
// to look for a mention. Unread polling has already caused a rate-limit
// incident once (v0.5.2), so this stays small: a round that grew more channels
// than this alerts about the first few and lets the next round catch the rest.
const mentionScanCap = 3

// pollAlerts turns a round of polled unread counts into alerts.
//
// Every other alert path in this file hangs off a Socket Mode event, which needs
// the xapp and xoxb tokens. Those cannot be issued by a loopback sign-in at all,
// so most installs never run Socket Mode — and for them nothing ever rang the
// bell or raised a notification, despite both being advertised.
//
// DMs alert on any increase: a direct message is addressed to you by
// definition. Channels are noisier, so they only alert on an actual mention,
// which costs a history fetch — issued only for channels whose count grew.
func (m *Model) pollAlerts(counts map[string]int, seq int) []tea.Cmd {
	// The first round after launch compares against counts from the initial
	// load, which are frequently zero — alerting on those would greet the user
	// with a notification per unread conversation on every start.
	if !m.unreadPrimed {
		m.unreadPrimed = true
		return nil
	}
	var cmds []tea.Cmd
	var scan []string
	for id, n := range counts {
		if id == m.activeID || m.readSeqOf[id] > seq {
			continue
		}
		prev := m.meta[id].Unread
		if n <= prev {
			continue
		}
		conv, ok := m.ws.Conversation(id)
		if !ok {
			continue
		}
		if conv.Type == "dm" || conv.Type == "group" {
			cmds = append(cmds, bellCmd, m.notifyCmd(conv.Name, "", plural(n-prev)))
			continue
		}
		if len(scan) < mentionScanCap {
			scan = append(scan, id)
		}
	}
	for _, id := range scan {
		cmds = append(cmds, m.mentionScanCmd(id, counts[id]-m.meta[id].Unread))
	}
	return cmds
}

// plural renders the notification body for n newly-arrived messages.
func plural(n int) string {
	if n == 1 {
		return "1 new message"
	}
	return fmt.Sprintf("%d new messages", n)
}

// mentionScanMsg reports a mention found by scanning a channel's new messages.
type mentionScanMsg struct {
	convID string
	author string
	text   string
	found  bool
}

// mentionScanCmd looks at the n messages that just arrived in a channel and
// reports the most recent one that mentions the user. Only the new messages are
// examined, so an older mention already seen is not re-announced.
func (m Model) mentionScanCmd(convID string, n int) tea.Cmd {
	src, me := m.src, m.ws.MeID
	if n < 1 {
		n = 1
	}
	return func() tea.Msg {
		msgs, err := src.History(convID)
		if err != nil || len(msgs) == 0 {
			return mentionScanMsg{convID: convID}
		}
		if n < len(msgs) {
			msgs = msgs[len(msgs)-n:]
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].MentionsMe && msgs[i].UserID != me {
				return mentionScanMsg{convID: convID, author: msgs[i].UserID, text: msgs[i].Text, found: true}
			}
		}
		return mentionScanMsg{convID: convID}
	}
}
