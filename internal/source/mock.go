package source

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kurenn/slack-tui/internal/data"
)

// Mock is an in-memory Source backed by the sample workspace. It returns
// instantly, so the app behaves synchronously when using it.
type Mock struct {
	ws          *data.Workspace
	messages    map[string][]data.Message
	reacted     map[string]bool // convID/msgID/name → I reacted (for toggling)
	joinable    []data.Conversation
	uploads     []UploadRecord
	UploadErr   error // if non-nil, Upload returns this error (still records the call)
	downloads   []DownloadRecord
	DownloadErr error    // if non-nil, Download returns this error (still records the call)
	opened      []string // userIDs passed to OpenDM
	left        []string // convIDs passed to Leave
	snoozed     []int    // minutes passed to Snooze
	canvases    map[string]data.CanvasDocument
	canvasSeq   int
}

// UploadRecord captures the arguments of a single Upload call for inspection
// in tests.
type UploadRecord struct {
	ConvID  string
	Paths   []string
	Comment string
}

// DownloadRecord captures the arguments of a single Download call for
// inspection in tests.
type DownloadRecord struct {
	File data.File
	Dir  string
}

// NewMock builds a mock source from the sample workspace.
func NewMock() *Mock {
	ws := data.Mock()
	msgs := make(map[string][]data.Message, len(ws.Messages))
	for k, v := range ws.Messages {
		msgs[k] = append([]data.Message(nil), v...)
	}
	return &Mock{ws: ws, messages: msgs, reacted: map[string]bool{}, canvases: map[string]data.CanvasDocument{},
		joinable: []data.Conversation{
			{ID: "platform", Type: "channel", Name: "platform", Topic: "infra · deploys · clusters"},
			{ID: "watercooler", Type: "channel", Name: "watercooler", Topic: "off-topic, but the good kind"},
		}}
}

func (m *Mock) Load() (*data.Workspace, error) { return m.ws, nil }

func (m *Mock) History(convID string) ([]data.Message, error) {
	out := append([]data.Message(nil), m.messages[convID]...)
	for i := range out {
		out[i].ReplyCount = len(out[i].Replies)
	}
	return out, nil
}

func (m *Mock) HistoryBefore(convID, beforeTS string) ([]data.Message, error) {
	return nil, nil // mock has no older history
}

func (m *Mock) Replies(convID, rootID string) ([]data.Reply, error) {
	for _, msg := range m.messages[convID] {
		if msg.ID == rootID {
			return append([]data.Reply(nil), msg.Replies...), nil
		}
	}
	return nil, nil
}

func (m *Mock) Send(convID, text string) (data.Message, error) {
	msg := data.Message{ID: "m" + stamp(), UserID: m.ws.MeID, Time: hm(), Text: text}
	m.messages[convID] = append(m.messages[convID], msg)
	return msg, nil
}

func (m *Mock) SendReply(convID, rootID, text string) (data.Reply, error) {
	r := data.Reply{ID: "r" + stamp(), UserID: m.ws.MeID, Time: hm(), Text: text}
	for i := range m.messages[convID] {
		if m.messages[convID][i].ID == rootID {
			m.messages[convID][i].Replies = append(m.messages[convID][i].Replies, r)
		}
	}
	return r, nil
}

func (m *Mock) Unread(convID string) (int, error)      { return 0, nil }
func (m *Mock) MarkRead(convID, ts string) error       { return nil }
func (m *Mock) SetPresence(status string) error        { return nil }
func (m *Mock) SetStatusText(text, emoji string) error { return nil }

// Presence returns the mock workspace's already-set statuses for the requested
// ids, so the no-token experience is deterministic and the poll sees no change.
func (m *Mock) Presence(userIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(userIDs))
	for _, id := range userIDs {
		if u, ok := m.ws.Users[id]; ok {
			out[id] = u.Status
		}
	}
	return out, nil
}

// React toggles a reaction in the in-memory store.
func (m *Mock) React(convID, msgID, name string) (bool, error) {
	key := convID + "/" + msgID + "/" + name
	added := !m.reacted[key]
	m.reacted[key] = added
	glyph := emojiOf(name)
	list := m.messages[convID]
	for i := range list {
		if list[i].ID != msgID {
			continue
		}
		for j := range list[i].Reactions {
			if list[i].Reactions[j].Emoji == glyph {
				if added {
					list[i].Reactions[j].Count++
				} else if list[i].Reactions[j].Count--; list[i].Reactions[j].Count <= 0 {
					list[i].Reactions = append(list[i].Reactions[:j], list[i].Reactions[j+1:]...)
				}
				return added, nil
			}
		}
		if added {
			list[i].Reactions = append(list[i].Reactions, data.Reaction{Emoji: glyph, Count: 1})
		}
	}
	return added, nil
}

func (m *Mock) Edit(convID, msgID, text string) error {
	for i := range m.messages[convID] {
		if m.messages[convID][i].ID == msgID {
			m.messages[convID][i].Text = text
		}
	}
	return nil
}

func (m *Mock) Delete(convID, msgID string) error {
	list := m.messages[convID]
	for i := range list {
		if list[i].ID == msgID {
			m.messages[convID] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return nil
}

// Joinable lists the not-yet-joined sample channels.
func (m *Mock) Joinable() ([]data.Conversation, error) {
	return append([]data.Conversation(nil), m.joinable...), nil
}

// Join hands the channel over; the app adds it to the workspace.
func (m *Mock) Join(convID string) (data.Conversation, error) {
	for i, c := range m.joinable {
		if c.ID == convID {
			m.joinable = append(m.joinable[:i], m.joinable[i+1:]...)
			return c, nil
		}
	}
	return data.Conversation{}, fmt.Errorf("unknown channel %q", convID)
}

// Upload records the call in-memory and returns UploadErr (nil by default).
func (m *Mock) Upload(convID string, paths []string, comment string) error {
	m.uploads = append(m.uploads, UploadRecord{ConvID: convID, Paths: paths, Comment: comment})
	return m.UploadErr
}

// Uploads returns all Upload calls recorded so far.
func (m *Mock) Uploads() []UploadRecord { return m.uploads }

// Download records the call and returns the joined path. Returns DownloadErr if
// set (call is still recorded so tests can inspect it).
func (m *Mock) Download(file data.File, destDir string) (string, error) {
	m.downloads = append(m.downloads, DownloadRecord{File: file, Dir: destDir})
	return filepath.Join(destDir, file.Name), m.DownloadErr
}

// Downloads returns all Download calls recorded so far.
func (m *Mock) Downloads() []DownloadRecord { return m.downloads }

// Search does a naive case-insensitive substring search over all messages.
func (m *Mock) Search(query string) ([]SearchHit, error) {
	q := strings.ToLower(query)
	var out []SearchHit
	for _, conv := range append(append([]data.Conversation{}, m.ws.Channels...), m.ws.DMs...) {
		for _, msg := range m.messages[conv.ID] {
			if !strings.Contains(strings.ToLower(msg.Text), q) {
				continue
			}
			name := m.ws.Users[msg.UserID].Name
			out = append(out, SearchHit{
				ConvID: conv.ID, ConvName: conv.Name, UserName: name,
				Time: msg.Time, MsgID: msg.ID, Text: msg.Text,
			})
		}
	}
	return out, nil
}

// OpenDM records the userID and returns a deterministic DM conversation.
func (m *Mock) OpenDM(userID string) (data.Conversation, error) {
	m.opened = append(m.opened, userID)
	return data.Conversation{ID: "dm_" + userID, Type: "dm", UserID: userID, Name: userID}, nil
}

// Opened returns the userIDs that were passed to OpenDM, in call order.
func (m *Mock) Opened() []string { return m.opened }

// Leave records the convID and returns nil.
func (m *Mock) Leave(convID string) error {
	m.left = append(m.left, convID)
	return nil
}

// Left returns the convIDs that were passed to Leave, in call order.
func (m *Mock) Left() []string { return m.left }

// Snooze records the minutes and returns nil.
func (m *Mock) Snooze(minutes int) error {
	m.snoozed = append(m.snoozed, minutes)
	return nil
}

// Snoozed returns the minutes values passed to Snooze, in call order.
func (m *Mock) Snoozed() []int { return m.snoozed }

func hm() string    { now := time.Now(); return fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute()) }
func stamp() string { return strings.TrimPrefix(fmt.Sprintf("%d", time.Now().UnixNano()), "1") }
