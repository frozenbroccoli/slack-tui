// Package data defines the workspace domain types and the mock workspace ported
// from the handoff's data.js. The mock layer implements the same shape a real
// Slack data source will later satisfy, so the views never learn where messages
// come from.
package data

// User is a workspace member. Color is a syntax-token key
// (blue/green/purple/orange/cyan/red/yellow) used to tint the username.
type User struct {
	ID     string
	Name   string
	Handle string
	Color  string
	Status string // online | away | dnd | offline
}

// Conversation is a channel or DM in the sidebar.
type Conversation struct {
	ID      string
	Type    string // "channel" | "dm"
	Name    string
	Topic   string
	UserID  string // for DMs: the other person
	Unread  int
	Mention bool
}

// Canvas is a channel or standalone Slack document.
type Canvas struct {
	ID        string
	Title     string
	URL       string
	ChannelID string
}

// CanvasDocument contains a readable Markdown export and a revision fingerprint.
// Editable is false when conversion would discard rich Slack content.
type CanvasDocument struct {
	Canvas
	Markdown string
	Revision string
	Editable bool
	Warnings []string
	Sections []CanvasSection
}

// CanvasSection combines Slack's editable section ID with an export preview,
// when the export provides a matching identifier.
type CanvasSection struct {
	ID       string `json:"id"`
	Markdown string
	Editable bool
}

// CanvasChange is a general Canvas operation. Full replacement requires the
// revision read by the editor; targeted operations use Slack section IDs.
type CanvasChange struct {
	Operation        string
	SectionID        string
	Markdown         string
	ExpectedRevision string
}

// File is a Slack-hosted attachment on a message (uploaded file or snippet).
type File struct {
	ID   string
	Name string
	URL  string // url_private_download (or url_private as fallback)
	Size int
	Mime string // mimetype reported by Slack (e.g. "image/png")
}

// Reply is a single threaded reply under a root message.
type Reply struct {
	ID     string
	UserID string
	Time   string
	Text   string
}

// Message is a top-level message in a conversation. ReplyCount is the thread
// size even when Replies haven't been fetched yet (lazy load on thread open).
// Day ("Mon Jan 2") drives the date separators; empty means unknown (mock).
// Extra holds non-text annotations ([file: …]) rendered after the body — kept
// out of Text so editing a message never sends placeholders to the backend.
// Links holds file/attachment permalinks for the `o` open-link action.
// Files holds structured metadata for attached files (download support).
type Message struct {
	ID         string
	UserID     string
	Time       string
	Day        string
	Text       string
	Extra      []string
	Links      []string
	Files      []File
	Reactions  []Reaction
	Replies    []Reply
	ReplyCount int
	MentionsMe bool
}

// Reaction is an emoji + count pill.
type Reaction struct {
	Emoji string
	Count int
}

// Workspace bundles everything the app renders.
type Workspace struct {
	Name     string
	Handle   string
	MeID     string
	Users    map[string]User
	Channels []Conversation
	DMs      []Conversation
	Messages map[string][]Message // keyed by conversation ID
}

// Me returns the current user.
func (w *Workspace) Me() User { return w.Users[w.MeID] }

// Conversation looks up a channel or DM by ID.
func (w *Workspace) Conversation(id string) (Conversation, bool) {
	for _, c := range w.Channels {
		if c.ID == id {
			return c, true
		}
	}
	for _, c := range w.DMs {
		if c.ID == id {
			return c, true
		}
	}
	return Conversation{}, false
}

// Mock returns the sample monospace-labs workspace from the handoff.
func Mock() *Workspace {
	users := map[string]User{
		"me":    {ID: "me", Name: "you", Handle: "you", Color: "fg", Status: "online"},
		"ada":   {ID: "ada", Name: "ada.k", Handle: "ada", Color: "purple", Status: "online"},
		"lin":   {ID: "lin", Name: "lin.z", Handle: "lin", Color: "green", Status: "online"},
		"marco": {ID: "marco", Name: "marco", Handle: "marco", Color: "orange", Status: "away"},
		"priya": {ID: "priya", Name: "priya.r", Handle: "priya", Color: "cyan", Status: "online"},
		"tomo":  {ID: "tomo", Name: "tomo", Handle: "tomo", Color: "yellow", Status: "dnd"},
		"bot":   {ID: "bot", Name: "ci-bot", Handle: "ci-bot", Color: "red", Status: "online"},
	}

	channels := []Conversation{
		{ID: "general", Type: "channel", Name: "general", Topic: "company-wide announcements & water-cooler", Unread: 0, Mention: false},
		{ID: "engineering", Type: "channel", Name: "engineering", Topic: "core runtime · build system · perf", Unread: 3, Mention: true},
		{ID: "design", Type: "channel", Name: "design", Topic: "product surface · TUI specs · iconography", Unread: 1, Mention: false},
		{ID: "releases", Type: "channel", Name: "releases", Topic: "ship logs · changelogs · tags", Unread: 0, Mention: false},
		{ID: "incidents", Type: "channel", Name: "incidents", Topic: "pager · postmortems · status", Unread: 2, Mention: false},
		{ID: "random", Type: "channel", Name: "random", Topic: "gifs, snacks, off-topic", Unread: 0, Mention: false},
	}

	dms := []Conversation{
		{ID: "dm_ada", Type: "dm", Name: "ada.k", UserID: "ada", Unread: 2, Mention: true},
		{ID: "dm_lin", Type: "dm", Name: "lin.z", UserID: "lin", Unread: 0, Mention: false},
		{ID: "dm_marco", Type: "dm", Name: "marco", UserID: "marco", Unread: 0, Mention: false},
		{ID: "dm_priya", Type: "dm", Name: "priya.r", UserID: "priya", Unread: 0, Mention: false},
	}

	messages := map[string][]Message{
		"general": {
			{ID: "g1", UserID: "ada", Time: "08:31", Text: "morning all — standup notes are in #engineering today, threading the perf stuff there.", Reactions: []Reaction{{"☕", 3}}},
			{ID: "g2", UserID: "priya", Time: "08:34", Text: "reminder: design review at 2pm. agenda → the new status bar + command palette spec."},
			{ID: "g3", UserID: "tomo", Time: "09:02", Text: "welcome @you to the workspace! ping me if the keybindings feel off, we tuned them this week.", Reactions: []Reaction{{"👋", 5}, {"🎉", 2}}, Replies: []Reply{
				{ID: "g3r1", UserID: "me", Time: "09:05", Text: "thanks! the vim nav already feels right at home"},
				{ID: "g3r2", UserID: "tomo", Time: "09:06", Text: "try `t` on a message to pop a thread — that one took a while to get smooth"},
			}},
			{ID: "g4", UserID: "lin", Time: "09:40", Text: "coffee order going out, react with ☕ in the next 5 min", Reactions: []Reaction{{"☕", 8}}},
		},
		"engineering": {
			{ID: "e1", UserID: "ada", Time: "09:12", Text: "pushed the render-loop refactor. we no longer repaint the whole grid on every keystroke — only dirty cells.", Reactions: []Reaction{{"🚀", 4}}},
			{ID: "e2", UserID: "marco", Time: "09:18", Text: "huge. what does the diff look like for the hot path?"},
			{ID: "e3", UserID: "ada", Time: "09:21", Text: "roughly:\n```\nfor cell in dirty:\n    buf.move(cell.row, cell.col)\n    buf.write(cell.glyph, cell.style)\nbuf.flush()\n```\nso `O(dirty)` instead of `O(rows*cols)`.", Reactions: []Reaction{{"🔥", 6}}, Replies: []Reply{
				{ID: "e3r1", UserID: "marco", Time: "09:24", Text: "and `flush()` coalesces the writes into one syscall?"},
				{ID: "e3r2", UserID: "ada", Time: "09:25", Text: "yep, single write() per frame. measured 1.8ms → 0.3ms on the 240-col case."},
				{ID: "e3r3", UserID: "lin", Time: "09:27", Text: "that is going to make the scroll-back buttery. nice work @ada"},
			}},
			{ID: "e4", UserID: "bot", Time: "09:33", Text: "build #4821 passed in 47s · 0 warnings · coverage 91.2% (+0.4)", Reactions: []Reaction{{"✅", 2}}},
			{ID: "e5", UserID: "lin", Time: "09:38", Text: "hey @you — can you take the keyboard-focus ticket? it pairs well with the work you did on modes.", MentionsMe: true},
		},
		"design": {
			{ID: "d1", UserID: "priya", Time: "11:02", Text: "new spec for the pane borders — single-line box drawing, title embedded in the top rule. mock attached.", Reactions: []Reaction{{"👏", 3}}, Files: []File{{ID: "F1", Name: "spec.pdf", URL: "https://files.slack.com/files-pri/T0/spec.pdf", Size: 24576}}},
			{ID: "d2", UserID: "priya", Time: "11:03", Text: "token palette stays multi-color: usernames, #channels, @mentions and `code` each get a distinct hue. it should read like a good editor theme.", Reactions: []Reaction{{"🎨", 4}}},
			{ID: "d3", UserID: "tomo", Time: "11:20", Text: "agreed on the embedded titles. one ask: keep the active pane border in the accent so focus is obvious at a glance."},
		},
		"releases": {
			{ID: "r1", UserID: "bot", Time: "07:00", Text: "tagged `v0.9.0` — “phosphor”. highlights: command palette, thread rail, presence dots.", Reactions: []Reaction{{"🏷️", 5}}},
			{ID: "r2", UserID: "ada", Time: "07:14", Text: "changelog is in the repo under `CHANGELOG.md`. ping me for the migration note on keybinds."},
		},
		"incidents": {
			{ID: "i1", UserID: "bot", Time: "02:14", Text: "PAGE: latency p99 on the sync gateway crossed 800ms for 5m. auto-opened INC-204."},
			{ID: "i2", UserID: "marco", Time: "02:31", Text: "acked. it was a thundering-herd on reconnect after the deploy. rolled back, p99 back to 120ms.", Reactions: []Reaction{{"🙏", 3}}},
		},
		"random": {
			{ID: "ra1", UserID: "lin", Time: "13:02", Text: "my terminal has 14 tabs open and somehow that feels normal now", Reactions: []Reaction{{"😅", 4}}},
			{ID: "ra2", UserID: "tomo", Time: "13:09", Text: "we live in the box-drawing characters now. ┌─┐ is home. └─┘", Reactions: []Reaction{{"📦", 6}}},
		},
		"dm_ada": {
			{ID: "da1", UserID: "ada", Time: "09:44", Text: "when you get a sec — wanna pair on the focus-ring states? I have the border colors but the transitions feel abrupt."},
			{ID: "da2", UserID: "ada", Time: "09:45", Text: "no rush, after standup is fine @you", MentionsMe: true},
		},
		"dm_lin": {
			{ID: "dl1", UserID: "lin", Time: "Yesterday", Text: "sent you the keyboard-focus ticket. it’s small but fiddly — happy to rubber-duck."},
		},
		"dm_marco": {
			{ID: "dm1", UserID: "marco", Time: "Mon", Text: "thanks for the review on the flush() change 🙏"},
		},
		"dm_priya": {
			{ID: "dp1", UserID: "priya", Time: "Mon", Text: "design review notes are in #design if you missed it"},
		},
	}

	return &Workspace{
		Name:     "monospace-labs",
		Handle:   "monospace-labs",
		MeID:     "me",
		Users:    users,
		Channels: channels,
		DMs:      dms,
		Messages: messages,
	}
}
