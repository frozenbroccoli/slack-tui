package source

// End-to-end coverage of the Slack Web API client (internal/source/slack.go)
// over a fake HTTP server (fake_slack_test.go). slack-go v0.25.0's
// slack.OptionAPIURL lets every test build a real *Slack whose s.api talks to
// an httptest.Server instead of slack.com — zero production changes.
//
// Every assertion here is either about what the client SENT (a form field) or
// a DERIVED value (computed independently of the code under test, e.g. a
// hand-picked unix timestamp's UTC clock reading, or an unread count counted
// by eye from the canned fixture) — never an echo of canned JSON back at
// itself. See docs/coverage-plan.md §3 for the anti-patterns this avoids.
//
// MarkRead/markErr live in slack_markread_test.go. Shared fake-server helpers
// (prefixed fk) live in fake_slack_test.go.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"github.com/kurenn/slack-tui/internal/data"
)

// ── Load ─────────────────────────────────────────────────────────────────

// TestSlackLoadPaginatesFiltersAndSorts drives Load's whole shape in one
// scenario: a two-page conversations.list (proves the cursor loop is
// followed — a regression there would silently drop the second page),
// membership/deletion filtering, group-DM inclusion, and DM name resolution
// for a user missing from the bulk users.list.
func TestSlackLoadPaginatesFiltersAndSorts(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.SetGroupDMs(true)

	fk.on("auth.test", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"team": "Acme Corp", "user": "you", "user_id": "U1"}))
	})
	fk.on("users.list", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{
			"members": []map[string]any{
				{"id": "U1", "name": "me", "profile": map[string]any{}},
				{"id": "U2", "name": "ada", "profile": map[string]any{"real_name": "Ada Lovelace"}},
			},
			"response_metadata": map[string]any{"next_cursor": ""},
		}))
	})
	calls := 0
	fk.on("conversations.list", func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			fkJSON(w, fkOK(map[string]any{
				"channels": []map[string]any{
					{"id": "C2", "is_channel": true, "is_member": true, "name": "zebra", "topic": map[string]any{"value": "z"}},
					{"id": "CNOTMEM", "is_channel": true, "is_member": false, "name": "secret"},
					{"id": "D1", "is_im": true, "user": "U2"},
					{"id": "DDELETED", "is_im": true, "user": "U9", "is_user_deleted": true},
					{"id": "DNOUSER", "is_im": true, "user": ""},
					{"id": "G1", "is_mpim": true, "name": "mpdm-ada--lin-1"},
				},
				"response_metadata": map[string]any{"next_cursor": "page2"},
			}))
		case 2:
			fkJSON(w, fkOK(map[string]any{
				"channels": []map[string]any{
					{"id": "C1", "is_channel": true, "is_member": true, "name": "apple", "topic": map[string]any{"value": "a"}},
					{"id": "D2", "is_im": true, "user": "U3"}, // U3 missing from users.list — resolved below
				},
				"response_metadata": map[string]any{"next_cursor": ""},
			}))
		default:
			t.Fatalf("conversations.list called %d times, want 2 (pagination should stop once next_cursor is empty)", calls)
		}
	})
	fk.on("users.info", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("user"); got != "U3" {
			t.Fatalf("users.info user = %q, want U3", got)
		}
		fkJSON(w, fkOK(map[string]any{"user": map[string]any{"id": "U3", "name": "marco", "profile": map[string]any{}}}))
	})

	ws, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Pagination: both pages' channels present.
	if len(ws.Channels) != 2 {
		t.Fatalf("channels = %d, want 2 (both pages)", len(ws.Channels))
	}
	// Sorted: apple before zebra.
	if ws.Channels[0].Name != "apple" || ws.Channels[1].Name != "zebra" {
		t.Errorf("channels not sorted: %+v", ws.Channels)
	}
	// is_member:false and is_user_deleted / empty-user IMs excluded.
	for _, c := range ws.Channels {
		if c.ID == "CNOTMEM" {
			t.Error("non-member channel should be excluded")
		}
	}
	for _, d := range ws.DMs {
		if d.ID == "DDELETED" || d.ID == "DNOUSER" {
			t.Errorf("deleted-user/no-user IM %q should be excluded", d.ID)
		}
	}
	// 3 DMs expected: D1 (ada), D2 (marco, resolved via users.info), G1 (mpim).
	if len(ws.DMs) != 3 {
		t.Fatalf("dms = %d, want 3: %+v", len(ws.DMs), ws.DMs)
	}
	byID := map[string]data.Conversation{}
	for _, d := range ws.DMs {
		byID[d.ID] = d
	}
	if got := byID["D1"].Name; got != "Ada Lovelace" {
		t.Errorf("D1 name = %q, want Ada Lovelace (from users.list — toUser prefers real_name over the bare name)", got)
	}
	if got := byID["D2"].Name; got != "marco" {
		t.Errorf("D2 name = %q, want marco (resolved via users.info, missing from users.list)", got)
	}
	if got := byID["G1"].Name; got != "ada, lin" {
		t.Errorf("mpim name = %q, want %q (mpimName-cleaned)", got, "ada, lin")
	}
	// Identity + workspace metadata.
	if ws.Name != "Acme Corp" || ws.Handle != "acme-corp" || ws.MeID != "U1" {
		t.Errorf("workspace identity = %+v", ws)
	}
	if ws.Users["U1"].Name != "you" {
		t.Errorf(`me user Name = %q, want "you"`, ws.Users["U1"].Name)
	}
	// handleIDs indexed for outgoing-mention encoding.
	if s.handleIDs["ada"] != "U2" {
		t.Errorf("handleIDs[ada] = %q, want U2", s.handleIDs["ada"])
	}
}

// TestSlackLoadGroupDMsRequestParam: SetGroupDMs controls whether "types"
// includes mpim — the request the plan calls out explicitly. A regression
// here silently shows/hides group DMs regardless of the user's preference.
func TestSlackLoadGroupDMsRequestParam(t *testing.T) {
	fk := fkNewServer(t)
	fk.on("auth.test", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"team": "T", "user": "you", "user_id": "U1"}))
	})
	fk.on("users.list", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"members": []map[string]any{{"id": "U1", "name": "me"}}, "response_metadata": map[string]any{"next_cursor": ""}}))
	})
	fk.on("conversations.list", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"channels": []map[string]any{}, "response_metadata": map[string]any{"next_cursor": ""}}))
	})

	for _, on := range []bool{true, false} {
		s := fkClient(fk)
		s.SetGroupDMs(on)
		if _, err := s.Load(); err != nil {
			t.Fatalf("Load (groupDMs=%v): %v", on, err)
		}
		types := fk.lastForm("conversations.list").Get("types")
		hasMpim := strings.Contains(types, "mpim")
		if hasMpim != on {
			t.Errorf("groupDMs=%v: types=%q, mpim present=%v, want %v", on, types, hasMpim, on)
		}
	}
}

func TestSlackLoadAuthTestError(t *testing.T) {
	fk := fkNewServer(t)
	fk.on("auth.test", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkErr("invalid_auth")) })
	s := fkClient(fk)
	_, err := s.Load()
	if err == nil || !strings.Contains(err.Error(), "auth:") {
		t.Fatalf("Load auth.test error = %v, want wrapped with 'auth:'", err)
	}
}

func TestSlackLoadUsersListError(t *testing.T) {
	fk := fkNewServer(t)
	fk.on("auth.test", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"team": "T", "user": "you", "user_id": "U1"}))
	})
	fk.on("users.list", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkErr("ratelimited_or_whatever")) })
	s := fkClient(fk)
	_, err := s.Load()
	if err == nil || !strings.Contains(err.Error(), "users:") {
		t.Fatalf("Load users.list error = %v, want wrapped with 'users:'", err)
	}
}

func TestSlackLoadConversationsListError(t *testing.T) {
	fk := fkNewServer(t)
	fk.on("auth.test", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"team": "T", "user": "you", "user_id": "U1"}))
	})
	fk.on("users.list", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"members": []map[string]any{}, "response_metadata": map[string]any{"next_cursor": ""}}))
	})
	fk.on("conversations.list", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkErr("internal_error")) })
	s := fkClient(fk)
	_, err := s.Load()
	if err == nil || !strings.Contains(err.Error(), "conversations:") {
		t.Fatalf("Load conversations.list error = %v, want wrapped with 'conversations:'", err)
	}
}

// ── Unread / lastReadOf / setLastRead ───────────────────────────────────

// TestSlackUnreadCountsFilteredMessages: the canned history page represents
// what Slack already returned filtered by Oldest — unreadFor's own job is to
// additionally drop noise subtypes and the user's own messages. 1 noise +
// 2 real messages -> unread = 2, counted by hand from the fixture below.
func TestSlackUnreadCountsFilteredMessages(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.meID = "U1"

	fk.on("conversations.info", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"channel": map[string]any{"id": "C1", "last_read": "1700000000.000000"}}))
	})
	fk.on("conversations.history", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("oldest"); got != "1700000000.000000" {
			t.Errorf("history oldest = %q, want the cached last_read", got)
		}
		fkJSON(w, fkOK(map[string]any{"messages": []map[string]any{
			{"type": "message", "subtype": "channel_join", "user": "U2", "ts": "1700000001.000000"}, // noise
			{"type": "message", "user": "U2", "ts": "1700000002.000000", "text": "hi"},              // real
			{"type": "message", "user": "U3", "ts": "1700000003.000000", "text": "yo"},              // real
			{"type": "message", "user": "U1", "ts": "1700000004.000000", "text": "me too"},          // own, excluded
		}}))
	})

	got, err := s.Unread("C1")
	if err != nil {
		t.Fatalf("Unread: %v", err)
	}
	if got != 2 {
		t.Errorf("Unread = %d, want 2 (1 noise-subtype and 1 own message excluded)", got)
	}
}

// First messages must be unread even before Slack has a last_read cursor.
func TestSlackUnreadWithoutMarkerCountsIncoming(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.meID = "U1"
	fk.on("conversations.info", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"channel": map[string]any{"id": "D1"}}))
	})
	fk.on("conversations.history", func(w http.ResponseWriter, r *http.Request) {
		if r.PostForm.Get("oldest") != "" {
			t.Errorf("unexpected oldest: %s", r.PostForm.Get("oldest"))
		}
		fkJSON(w, fkOK(map[string]any{"messages": []map[string]any{
			{"user": "U2", "ts": "1700000001.000000", "text": "hello"},
			{"user": "U1", "ts": "1700000002.000000", "text": "mine"},
			{"user": "U2", "subtype": "channel_join", "ts": "1700000003.000000"},
		}}))
	})
	got, err := s.Unread("D1")
	if err != nil || got != 1 {
		t.Fatalf("Unread = (%d, %v), want (1, nil)", got, err)
	}
}

func TestSlackRefreshDMsPaginatesAndFilters(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.SetGroupDMs(true)
	fk.on("conversations.list", func(w http.ResponseWriter, r *http.Request) {
		if r.Form.Get("types") != "im,mpim" {
			t.Errorf("types = %q", r.Form.Get("types"))
		}
		if r.Form.Get("cursor") == "" {
			fkJSON(w, fkOK(map[string]any{"channels": []map[string]any{
				{"id": "D1", "is_im": true, "user": "U2"},
				{"id": "DDELETED", "is_im": true, "user": "U3", "is_user_deleted": true},
			}, "response_metadata": map[string]any{"next_cursor": "next"}}))
		} else {
			fkJSON(w, fkOK(map[string]any{"channels": []map[string]any{
				{"id": "G1", "is_mpim": true, "name": "mpdm-ada--lin-1"},
			}, "response_metadata": map[string]any{"next_cursor": ""}}))
		}
	})
	convs, err := s.RefreshDMs()
	if err != nil || len(convs) != 2 {
		t.Fatalf("discovery = %v, %v", convs, err)
	}
	if convs[0].ID != "D1" || convs[1].ID != "G1" {
		t.Fatalf("wrong conversations: %v", convs)
	}
	if fk.hitCount("conversations.list") != 2 {
		t.Fatal("pagination not followed")
	}
}

// TestSlackLastReadOfCachesWithinTTL: a second Unread call within the TTL
// must not re-hit conversations.info — that's the whole point of the cache
// (keeps steady-state polling to one call per conversation).
func TestSlackLastReadOfCachesWithinTTL(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("conversations.info", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"channel": map[string]any{"id": "C1", "last_read": "1700000000.000000"}}))
	})
	fk.on("conversations.history", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"messages": []map[string]any{}}))
	})
	if _, err := s.Unread("C1"); err != nil {
		t.Fatalf("Unread #1: %v", err)
	}
	if _, err := s.Unread("C1"); err != nil {
		t.Fatalf("Unread #2: %v", err)
	}
	if n := fk.hitCount("conversations.info"); n != 1 {
		t.Errorf("conversations.info hit %d times, want 1 (second call should hit the cache)", n)
	}
	if n := fk.hitCount("conversations.history"); n != 2 {
		t.Errorf("conversations.history hit %d times, want 2 (history always re-fetched)", n)
	}
}

// TestSlackLastReadOfStaleCacheSurvivesRefreshError: a rate-limited (or any
// failing) conversations.info refresh must not blank the sidebar — the stale
// cached marker is used instead. The stale entry is seeded directly (in
// package) rather than waiting out the real 5-minute TTL.
func TestSlackLastReadOfStaleCacheSurvivesRefreshError(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.setLastRead("C1", "1699999999.000000")
	s.mu.Lock()
	m := s.lastRead["C1"]
	m.seen = m.seen.Add(-2 * lastReadTTL) // force past the TTL
	s.lastRead["C1"] = m
	s.mu.Unlock()

	fk.on("conversations.info", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkErr("internal_error")) })

	ts, err := s.lastReadOf(t.Context(), "C1")
	if err != nil {
		t.Fatalf("lastReadOf should fall back to the stale cache, got error: %v", err)
	}
	if ts != "1699999999.000000" {
		t.Errorf("lastReadOf = %q, want the stale cached marker", ts)
	}
}

// TestSlackLastReadOfNoCacheRefreshError: with nothing cached yet, a failing
// refresh has nothing to fall back to and must return the error.
func TestSlackLastReadOfNoCacheRefreshError(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("conversations.info", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkErr("internal_error")) })
	if _, err := s.lastReadOf(t.Context(), "C1"); err == nil {
		t.Fatal("lastReadOf should error when there is no cache to fall back to")
	}
}

// ── History / HistoryBefore ─────────────────────────────────────────────

// TestSlackHistoryOrderAndDerivedFields: the API returns newest-first;
// History must reverse to oldest-first, and derived fields (Time, Day,
// MentionsMe) must match values worked out independently of the code, not
// echoed from the fixture.
func TestSlackHistoryOrderAndDerivedFields(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.meID = "U1"
	fk.on("conversations.history", func(w http.ResponseWriter, r *http.Request) {
		fkJSON(w, fkOK(map[string]any{"messages": []map[string]any{
			// newest first, as Slack sends it
			{"type": "message", "user": "U2", "ts": "1700000002.000000", "text": "second"},
			{"type": "message", "user": "U2", "ts": "1700000000.000000", "text": "first, pings <@U1>", "reply_count": 3},
		}}))
	})
	msgs, err := s.History("C1")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// messageText renders "<@U1>" through renderText, which — with no users
	// loaded on this Slack value — falls back to the raw id: "@U1".
	if len(msgs) != 2 || msgs[0].Text != "first, pings @U1" || msgs[1].Text != "second" {
		t.Fatalf("History did not reverse newest-first to oldest-first: %+v", msgs)
	}
	// 1700000000 -> 2023-11-14 22:13:20 UTC (hand-computed with `date -u -d @1700000000`).
	if msgs[0].Time != "22:13" {
		t.Errorf("Time = %q, want 22:13 (independently computed from the unix ts)", msgs[0].Time)
	}
	if msgs[0].Day != "Tue Nov 14" {
		t.Errorf("Day = %q, want Tue Nov 14", msgs[0].Day)
	}
	if msgs[0].ReplyCount != 3 {
		t.Errorf("ReplyCount = %d, want 3", msgs[0].ReplyCount)
	}
	if !msgs[0].MentionsMe {
		t.Error("MentionsMe should be true for a message containing <@U1>")
	}
	if msgs[1].MentionsMe {
		t.Error("MentionsMe should be false for a message without the mention")
	}
}

func TestSlackHistoryError(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("conversations.history", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkErr("channel_not_found")) })
	if _, err := s.History("C1"); err == nil {
		t.Fatal("History should surface the API error")
	}
}

func TestSlackHistoryBeforeSendsLatestParam(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("conversations.history", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("latest"); got != "1700000005.000000" {
			t.Errorf("latest = %q, want the beforeTS argument", got)
		}
		if got := r.PostForm.Get("inclusive"); got != "0" {
			t.Errorf("inclusive = %q, want 0", got)
		}
		fkJSON(w, fkOK(map[string]any{"messages": []map[string]any{{"type": "message", "user": "U2", "ts": "1700000001.000000", "text": "older"}}}))
	})
	msgs, err := s.HistoryBefore("C1", "1700000005.000000")
	if err != nil {
		t.Fatalf("HistoryBefore: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "older" {
		t.Fatalf("HistoryBefore = %+v", msgs)
	}
}

// ── Replies ──────────────────────────────────────────────────────────────

// TestSlackRepliesFlattensPagesAndSkipsRoot: a two-page thread must be
// flattened in full (not silently truncated to the first page), and the
// root message (echoed back by conversations.replies) must be excluded.
func TestSlackRepliesFlattensPagesAndSkipsRoot(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	calls := 0
	fk.on("conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.PostForm.Get("ts"); got != "1700000000.000000" {
			t.Errorf("replies ts = %q, want the root ts", got)
		}
		switch calls {
		case 1:
			fkJSON(w, fkOK(map[string]any{
				"has_more":          true,
				"response_metadata": map[string]any{"next_cursor": "cur2"},
				"messages": []map[string]any{
					{"type": "message", "user": "U1", "ts": "1700000000.000000", "text": "root"}, // must be skipped
					{"type": "message", "user": "U2", "ts": "1700000001.000000", "text": "reply1"},
				},
			}))
		case 2:
			if got := r.PostForm.Get("cursor"); got != "cur2" {
				t.Errorf("second page cursor = %q, want cur2", got)
			}
			fkJSON(w, fkOK(map[string]any{
				"has_more": false,
				"messages": []map[string]any{
					{"type": "message", "user": "U3", "ts": "1700000002.000000", "text": "reply2"},
				},
			}))
		default:
			t.Fatalf("conversations.replies called %d times, want 2", calls)
		}
	})
	reps, err := s.Replies("C1", "1700000000.000000")
	if err != nil {
		t.Fatalf("Replies: %v", err)
	}
	if len(reps) != 2 {
		t.Fatalf("Replies = %d, want 2 (root excluded, both pages flattened): %+v", len(reps), reps)
	}
	if reps[0].Text != "reply1" || reps[1].Text != "reply2" {
		t.Errorf("Replies = %+v", reps)
	}
}

// ── Send / SendReply ─────────────────────────────────────────────────────

func TestSlackSendEncodesMentionsAndReturnsMessage(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.meID = "U1"
	s.handleIDs = map[string]string{"ada": "U2"}
	fk.on("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("text"); got != "hey <@U2>" {
			t.Errorf("posted text = %q, want mention encoded to <@U2>", got)
		}
		fkJSON(w, fkOK(map[string]any{"channel": "C1", "ts": "1700000009.000000", "text": "hey <@U2>"}))
	})
	msg, err := s.Send("C1", "hey @ada")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if msg.ID != "1700000009.000000" || msg.UserID != "U1" || msg.Text != "hey @ada" {
		t.Errorf("Send result = %+v", msg)
	}
}

func TestSlackSendReplyIncludesThreadTS(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("thread_ts"); got != "1700000000.000000" {
			t.Errorf("thread_ts = %q, want the root ts", got)
		}
		fkJSON(w, fkOK(map[string]any{"channel": "C1", "ts": "1700000010.000000"}))
	})
	rep, err := s.SendReply("C1", "1700000000.000000", "a reply")
	if err != nil {
		t.Fatalf("SendReply: %v", err)
	}
	if rep.ID != "1700000010.000000" || rep.Text != "a reply" {
		t.Errorf("SendReply result = %+v", rep)
	}
}

// ── React ────────────────────────────────────────────────────────────────

// TestSlackReactTogglesOnAlreadyReacted: reactions.add reporting
// already_reacted must fall through to reactions.remove and report added=false
// — a regression here silently breaks the toggle (reactions get "stuck on").
func TestSlackReactTogglesOnAlreadyReacted(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("reactions.add", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkErr("already_reacted")) })
	removed := false
	fk.on("reactions.remove", func(w http.ResponseWriter, r *http.Request) {
		removed = true
		if got := r.PostForm.Get("name"); got != "fire" {
			t.Errorf("reactions.remove name = %q, want fire", got)
		}
		fkJSON(w, fkOK(nil))
	})
	added, err := s.React("C1", "1700000000.000000", "fire")
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if added {
		t.Error("React should report added=false after falling back to remove")
	}
	if !removed {
		t.Error("reactions.remove was never called")
	}
}

func TestSlackReactPlainAdd(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("reactions.add", func(w http.ResponseWriter, r *http.Request) { fkJSON(w, fkOK(nil)) })
	added, err := s.React("C1", "1700000000.000000", "fire")
	if err != nil || !added {
		t.Fatalf("React = (%v, %v), want (true, nil)", added, err)
	}
}

// ── Edit / Delete ────────────────────────────────────────────────────────

func TestSlackEditSendsEncodedText(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.handleIDs = map[string]string{"ada": "U2"}
	fk.on("chat.update", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("text"); got != "fix for <@U2>" {
			t.Errorf("chat.update text = %q, want mention encoded", got)
		}
		fkJSON(w, fkOK(map[string]any{"channel": "C1", "ts": "1700000000.000000"}))
	})
	if err := s.Edit("C1", "1700000000.000000", "fix for @ada"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
}

func TestSlackDelete(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("chat.delete", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("ts"); got != "1700000000.000000" {
			t.Errorf("chat.delete ts = %q", got)
		}
		fkJSON(w, fkOK(map[string]any{"channel": "C1", "ts": "1700000000.000000"}))
	})
	if err := s.Delete("C1", "1700000000.000000"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// ── Joinable / Join / OpenDM / Leave ────────────────────────────────────

func TestSlackJoinable(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("conversations.list", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("types"); got != "public_channel" {
			t.Errorf("Joinable types = %q, want public_channel only", got)
		}
		fkJSON(w, fkOK(map[string]any{
			"channels": []map[string]any{
				{"id": "C1", "is_channel": true, "is_member": false, "name": "zeta"},
				{"id": "C2", "is_channel": true, "is_member": true, "name": "already-in"},
				{"id": "C3", "is_channel": true, "is_member": false, "name": "alpha"},
			},
			"response_metadata": map[string]any{"next_cursor": ""},
		}))
	})
	convs, err := s.Joinable()
	if err != nil {
		t.Fatalf("Joinable: %v", err)
	}
	if len(convs) != 2 {
		t.Fatalf("Joinable = %d, want 2 (member channel excluded): %+v", len(convs), convs)
	}
	if convs[0].Name != "alpha" || convs[1].Name != "zeta" {
		t.Errorf("Joinable not sorted: %+v", convs)
	}
}

func TestSlackJoin(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("conversations.join", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("channel"); got != "C1" {
			t.Errorf("conversations.join channel = %q, want C1", got)
		}
		fkJSON(w, fkOK(map[string]any{"channel": map[string]any{"id": "C1", "is_channel": true, "name": "general", "topic": map[string]any{"value": "t"}}}))
	})
	conv, err := s.Join("C1")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if conv.ID != "C1" || conv.Name != "general" || conv.Topic != "t" {
		t.Errorf("Join result = %+v", conv)
	}
}

func TestSlackOpenDMResolvesName(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.users = map[string]data.User{"U2": {ID: "U2", Name: "ada"}}
	fk.on("conversations.open", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("return_im"); got != "true" {
			t.Errorf("return_im = %q, want true", got)
		}
		if got := r.PostForm.Get("users"); got != "U2" {
			t.Errorf("users = %q, want U2", got)
		}
		fkJSON(w, fkOK(map[string]any{"channel": map[string]any{"id": "D1", "is_im": true, "user": "U2"}}))
	})
	conv, err := s.OpenDM("U2")
	if err != nil {
		t.Fatalf("OpenDM: %v", err)
	}
	if conv.ID != "D1" || conv.Name != "ada" {
		t.Errorf("OpenDM result = %+v, want Name resolved from s.users", conv)
	}
}

func TestSlackLeave(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("conversations.leave", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("channel"); got != "C1" {
			t.Errorf("conversations.leave channel = %q", got)
		}
		fkJSON(w, fkOK(nil))
	})
	if err := s.Leave("C1"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
}

// ── Snooze / SetPresence / SetStatusText / Presence ─────────────────────

func TestSlackSnoozeDefaultsAndCustomMinutes(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	var got string
	fk.on("dnd.setSnooze", func(w http.ResponseWriter, r *http.Request) {
		got = r.PostForm.Get("num_minutes")
		fkJSON(w, fkOK(nil))
	})
	if err := s.Snooze(30); err != nil {
		t.Fatalf("Snooze(30): %v", err)
	}
	if got != "30" {
		t.Errorf("num_minutes = %q, want 30", got)
	}
	if err := s.Snooze(0); err != nil {
		t.Fatalf("Snooze(0): %v", err)
	}
	if got != "120" {
		t.Errorf("num_minutes = %q, want 120 (0 defaults to 2h)", got)
	}
}

func TestSlackSetPresenceStates(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	var endSnoozeHit, setSnoozeHit bool
	var presenceArg string
	fk.on("dnd.endSnooze", func(w http.ResponseWriter, r *http.Request) { endSnoozeHit = true; fkJSON(w, fkOK(nil)) })
	fk.on("dnd.setSnooze", func(w http.ResponseWriter, r *http.Request) { setSnoozeHit = true; fkJSON(w, fkOK(nil)) })
	fk.on("users.setPresence", func(w http.ResponseWriter, r *http.Request) {
		presenceArg = r.PostForm.Get("presence")
		fkJSON(w, fkOK(nil))
	})

	if err := s.SetPresence("dnd"); err != nil {
		t.Fatalf("SetPresence(dnd): %v", err)
	}
	if !setSnoozeHit {
		t.Error("dnd should call dnd.setSnooze")
	}
	if endSnoozeHit {
		t.Error("dnd should not call dnd.endSnooze")
	}

	endSnoozeHit, setSnoozeHit = false, false
	if err := s.SetPresence("away"); err != nil {
		t.Fatalf("SetPresence(away): %v", err)
	}
	if !endSnoozeHit {
		t.Error("away should end any snooze")
	}
	if presenceArg != "away" {
		t.Errorf("presence = %q, want away", presenceArg)
	}

	endSnoozeHit = false
	if err := s.SetPresence("online"); err != nil {
		t.Fatalf("SetPresence(online): %v", err)
	}
	if !endSnoozeHit {
		t.Error("online should end any snooze")
	}
	if presenceArg != "auto" {
		t.Errorf("presence = %q, want auto (Slack has no literal 'online')", presenceArg)
	}
}

func TestSlackSetStatusText(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("users.profile.set", func(w http.ResponseWriter, r *http.Request) {
		profile := r.PostForm.Get("profile")
		if !strings.Contains(profile, `"status_text":"brb"`) || !strings.Contains(profile, `"status_emoji":":coffee:"`) {
			t.Errorf("profile payload = %s, want status_text/status_emoji set", profile)
		}
		fkJSON(w, fkOK(map[string]any{"profile": map[string]any{}}))
	})
	if err := s.SetStatusText("brb", ":coffee:"); err != nil {
		t.Fatalf("SetStatusText: %v", err)
	}
}

func TestSlackPresenceOmitsErroringID(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("users.getPresence", func(w http.ResponseWriter, r *http.Request) {
		switch r.PostForm.Get("user") {
		case "U1":
			fkJSON(w, fkOK(map[string]any{"presence": "active"}))
		case "U2":
			fkJSON(w, fkOK(map[string]any{"presence": "away"}))
		default:
			fkJSON(w, fkErr("user_not_found"))
		}
	})
	got, err := s.Presence([]string{"U1", "U2", "UBOGUS"})
	if err != nil {
		t.Fatalf("Presence: %v", err)
	}
	if got["U1"] != "online" {
		t.Errorf("U1 = %q, want online (active -> online)", got["U1"])
	}
	if got["U2"] != "away" {
		t.Errorf("U2 = %q, want away", got["U2"])
	}
	if _, ok := got["UBOGUS"]; ok {
		t.Error("erroring id should be omitted, not present with a zero value")
	}
}

func TestSlackPresenceEmptyInput(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.on("users.getPresence", func(w http.ResponseWriter, r *http.Request) { t.Fatal("should not be called for an empty id list") })
	got, err := s.Presence(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Presence(nil) = (%v, %v), want (empty, nil)", got, err)
	}
}

// ── Search ───────────────────────────────────────────────────────────────

func TestSlackSearchMapsHits(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.users = map[string]data.User{"U2": {ID: "U2", Name: "ada"}}
	fk.on("search.messages", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("query"); got != "deploy" {
			t.Errorf("query = %q, want deploy", got)
		}
		fkJSON(w, fkOK(map[string]any{"messages": map[string]any{
			"total": 1,
			"matches": []map[string]any{
				{"channel": map[string]any{"id": "C1", "name": "general"}, "user": "U2", "username": "fallback", "ts": "1700000000.000000", "text": "deploy done"},
			},
		}}))
	})
	hits, err := s.Search("deploy")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("Search = %d hits, want 1", len(hits))
	}
	h := hits[0]
	if h.ConvID != "C1" || h.ConvName != "general" || h.UserName != "ada" || h.MsgID != "1700000000.000000" || h.Text != "deploy done" {
		t.Errorf("Search hit = %+v, want UserName resolved from s.users (ada, not the raw username)", h)
	}
}

// ── Upload / Download ────────────────────────────────────────────────────

func TestSlackUploadSendsBytesAndCompletesWithComment(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	s.handleIDs = map[string]string{"ada": "U2"}

	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p1, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploadedBody string
	fk.on("files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("filename"); got != "a.txt" {
			t.Errorf("filename = %q, want a.txt", got)
		}
		fkJSON(w, fkOK(map[string]any{"upload_url": fk.srv.URL + "/upload/xyz", "file_id": "F1"}))
	})
	fk.raw("/upload/xyz", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		defer f.Close()
		b := make([]byte, 512)
		n, _ := f.Read(b)
		uploadedBody = string(b[:n])
		w.WriteHeader(http.StatusOK)
	})
	fk.on("files.completeUploadExternal", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PostForm.Get("files"); !strings.Contains(got, `"id":"F1"`) {
			t.Errorf("completeUploadExternal files = %s, want it to list F1", got)
		}
		if got := r.PostForm.Get("initial_comment"); got != "for <@U2>" {
			t.Errorf("initial_comment = %q, want mention-encoded", got)
		}
		fkJSON(w, fkOK(map[string]any{"files": []map[string]any{{"id": "F1", "title": "a.txt"}}}))
	})

	if err := s.Upload("C1", []string{p1}, "for @ada"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if uploadedBody != "hello world" {
		t.Errorf("uploaded bytes = %q, want %q", uploadedBody, "hello world")
	}
}

func TestSlackUploadStatErrors(t *testing.T) {
	s := &Slack{}
	dir := t.TempDir()

	if err := s.Upload("C1", []string{filepath.Join(dir, "missing.txt")}, ""); err == nil {
		t.Error("Upload should error on a missing file")
	}
	if err := s.Upload("C1", []string{dir}, ""); err == nil {
		t.Error("Upload should error when the path is a directory")
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload("C1", []string{empty}, ""); err == nil {
		t.Error("Upload should error on an empty file")
	}
	if err := s.Upload("C1", nil, ""); err != nil {
		t.Errorf("Upload with no paths should no-op, got %v", err)
	}
}

func TestSlackDownloadSavesBytes(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.raw("/files/report.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 fake"))
	})
	dest := t.TempDir()
	path, err := s.Download(data.File{Name: "report.pdf", URL: fk.srv.URL + "/files/report.pdf", Mime: "application/pdf"}, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "%PDF-1.4 fake" {
		t.Errorf("downloaded content = %q", got)
	}
}

func TestSlackDownloadUniquifiesExistingName(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.raw("/files/note.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("second"))
	})
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "note.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := s.Download(data.File{Name: "note.txt", URL: fk.srv.URL + "/files/note.txt", Mime: "text/plain"}, dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Base(path) != "note (1).txt" {
		t.Errorf("saved path = %q, want a uniquified name", path)
	}
	orig, err := os.ReadFile(filepath.Join(dest, "note.txt"))
	if err != nil || string(orig) != "first" {
		t.Errorf("original file was clobbered: %q, %v", orig, err)
	}
}

// TestSlackDownloadDetectsSignInPage: a token lacking files:read gets an HTML
// sign-in page instead of the file — Download must detect and fail loudly
// rather than silently save the wrong bytes.
func TestSlackDownloadDetectsSignInPage(t *testing.T) {
	fk := fkNewServer(t)
	s := fkClient(fk)
	fk.raw("/files/secret.zip", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>please sign in</body></html>"))
	})
	dest := t.TempDir()
	_, err := s.Download(data.File{Name: "secret.zip", URL: fk.srv.URL + "/files/secret.zip", Mime: "application/zip"}, dest)
	if err == nil || !strings.Contains(err.Error(), "sign-in page") {
		t.Fatalf("Download error = %v, want a sign-in-page error", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "secret.zip")); statErr == nil {
		t.Error("the corrupt (HTML) file should have been removed, not left on disk")
	}
}

func TestSlackDownloadNoURL(t *testing.T) {
	s := &Slack{}
	if _, err := s.Download(data.File{Name: "x"}, t.TempDir()); err == nil {
		t.Error("Download should error when the file has no URL")
	}
}

// ── sanitizeName / createUnique (pure) ──────────────────────────────────

func TestSanitizeNameTable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{"/abs/path/x.txt", "x.txt"},
		{"", "file"},
		{".", "file"},
		{"..", "file"},
	}
	for _, c := range cases {
		if got := sanitizeName(c.in); got != c.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCreateUniqueCollisions(t *testing.T) {
	dir := t.TempDir()
	f1, p1, err := createUnique(dir, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	f1.Close()
	if filepath.Base(p1) != "x.txt" {
		t.Errorf("first path = %q, want x.txt", p1)
	}
	f2, p2, err := createUnique(dir, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if filepath.Base(p2) != "x (1).txt" {
		t.Errorf("second path = %q, want %q", p2, "x (1).txt")
	}
	f3, p3, err := createUnique(dir, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	f3.Close()
	if filepath.Base(p3) != "x (2).txt" {
		t.Errorf("third path = %q, want %q", p3, "x (2).txt")
	}
}

// ── IsRateLimited / retry behavior ───────────────────────────────────────

func TestIsRateLimitedWrapsRealAndPlainErrors(t *testing.T) {
	rl := &slack.RateLimitedError{RetryAfter: 0}
	if !IsRateLimited(fmt.Errorf("wrapped: %w", rl)) {
		t.Error("IsRateLimited should see through fmt.Errorf wrapping via errors.As")
	}
	if IsRateLimited(errors.New("plain error")) {
		t.Error("a plain error should not be reported as rate-limited")
	}
	if IsRateLimited(nil) {
		t.Error("nil error should not be reported as rate-limited")
	}
}

// TestSlackRateLimitedSurfacesWithoutRetryClient: built WITHOUT OptionRetry
// (unlike fkClient) so a 429 surfaces immediately as a *slack.RateLimitedError
// instead of the client sleeping through slack-go's built-in retries.
func TestSlackRateLimitedSurfacesWithoutRetryClient(t *testing.T) {
	fk := fkNewServer(t)
	fk.on("auth.test", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	s := NewSlack("xoxp-test")
	s.api = slack.New("xoxp-test", slack.OptionAPIURL(fk.srv.URL+"/")) // no OptionRetry
	_, err := s.Load()
	if err == nil {
		t.Fatal("Load should surface the 429 as an error")
	}
	if !IsRateLimited(err) {
		t.Errorf("Load error = %v, want IsRateLimited to recognize it", err)
	}
}

// ── pure mapping helpers not covered by slack_test.go ───────────────────

func TestToUserPrefersDisplayThenRealThenName(t *testing.T) {
	cases := []struct {
		name string
		in   slack.User
		want data.User
	}{
		{
			name: "bare name, no profile",
			in:   slack.User{ID: "U1", Name: "ada"},
			want: data.User{ID: "U1", Name: "ada", Handle: "ada", Status: "online"},
		},
		{
			name: "real name overrides bare name",
			in:   slack.User{ID: "U1", Name: "ada", Profile: slack.UserProfile{RealName: "Ada Lovelace"}},
			want: data.User{ID: "U1", Name: "Ada Lovelace", Handle: "ada", Status: "online"},
		},
		{
			name: "display name overrides real name",
			in:   slack.User{ID: "U1", Name: "ada", Profile: slack.UserProfile{RealName: "Ada Lovelace", DisplayName: "ada.l"}},
			want: data.User{ID: "U1", Name: "ada.l", Handle: "ada", Status: "online"},
		},
		{
			name: "deleted user is offline",
			in:   slack.User{ID: "U1", Name: "ghost", Deleted: true},
			want: data.User{ID: "U1", Name: "ghost", Handle: "ghost", Status: "offline"},
		},
		{
			name: "bot is offline",
			in:   slack.User{ID: "U1", Name: "bot", IsBot: true},
			want: data.User{ID: "U1", Name: "bot", Handle: "bot", Status: "offline"},
		},
	}
	for _, c := range cases {
		got := toUser(c.in)
		if got.Name != c.want.Name || got.Handle != c.want.Handle || got.Status != c.want.Status || got.ID != c.want.ID {
			t.Errorf("%s: toUser = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestMessageAuthorPriority(t *testing.T) {
	cases := []struct {
		name string
		m    slack.Message
		want string
	}{
		{"user id wins", slack.Message{Msg: slack.Msg{User: "U1", Username: "ignored", BotID: "B1"}}, "U1"},
		{"username when no user id", slack.Message{Msg: slack.Msg{Username: "deploybot", BotID: "B1"}}, "deploybot"},
		{"bot profile name when no username", slack.Message{Msg: slack.Msg{BotID: "B1", BotProfile: &slack.BotProfile{Name: "CI Bot"}}}, "CI Bot"},
		{"bot fallback when only bot id", slack.Message{Msg: slack.Msg{BotID: "B1"}}, "bot"},
		{"empty when nothing identifies the author", slack.Message{}, ""},
	}
	for _, c := range cases {
		if got := messageAuthor(c.m); got != c.want {
			t.Errorf("%s: messageAuthor = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestToMessageMapsReactionsAndAttachments: not covered by the existing
// blocks/rich-text/files tests in slack_test.go.
func TestToMessageMapsReactionsAndAttachments(t *testing.T) {
	s := &Slack{users: map[string]data.User{}}
	m := slack.Message{Msg: slack.Msg{
		Timestamp: "1700000000.000000",
		User:      "U1",
		Text:      "shipped",
		Reactions: []slack.ItemReaction{{Name: "fire", Count: 3}, {Name: "totally-custom-emoji", Count: 1}},
		Attachments: []slack.Attachment{
			{Title: "Build log", TitleLink: "https://ci/1"},
			{Fallback: "no title here"},
		},
	}}
	got := s.toMessage(m)
	if len(got.Reactions) != 2 || got.Reactions[0].Emoji != "🔥" || got.Reactions[0].Count != 3 {
		t.Errorf("Reactions = %+v", got.Reactions)
	}
	if got.Reactions[1].Emoji != ":totally-custom-emoji:" {
		t.Errorf("unknown custom emoji should fall back to :name:, got %q", got.Reactions[1].Emoji)
	}
	wantExtras := []string{"[attachment: Build log]", "[attachment: no title here]"}
	if len(got.Extra) != 2 || got.Extra[0] != wantExtras[0] || got.Extra[1] != wantExtras[1] {
		t.Errorf("Extra = %+v, want %+v", got.Extra, wantExtras)
	}
	if len(got.Links) != 1 || got.Links[0] != "https://ci/1" {
		t.Errorf("Links = %+v, want the titled attachment's link only", got.Links)
	}
}

// ── socket.go pure edges ────────────────────────────────────────────────

func TestSocketAuthorPriority(t *testing.T) {
	cases := []struct {
		name string
		in   *slackevents.MessageEvent
		want string
	}{
		{"user wins", &slackevents.MessageEvent{User: "U1", Username: "ignored", BotID: "B1"}, "U1"},
		{"username when no user", &slackevents.MessageEvent{Username: "deploybot", BotID: "B1"}, "deploybot"},
		{"bot fallback", &slackevents.MessageEvent{BotID: "B1"}, "bot"},
		{"empty", &slackevents.MessageEvent{}, ""},
	}
	for _, c := range cases {
		if got := socketAuthor(c.in); got != c.want {
			t.Errorf("%s: socketAuthor = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEventsNilBeforeStart(t *testing.T) {
	s := &Slack{}
	if s.Events() != nil {
		t.Error("Events() should be nil before StartSocket is ever called")
	}
}

// TestEmojiGlyphIsThePublicEmojiOf: EmojiGlyph is the exported entry point
// the reaction picker (a different package) calls — pin it against hand-
// picked glyphs independently of emojiOf so a regression that detaches the
// two (e.g. EmojiGlyph forwarding to the wrong table) fails here.
func TestEmojiGlyphIsThePublicEmojiOf(t *testing.T) {
	if EmojiGlyph("fire") != "🔥" {
		t.Errorf("EmojiGlyph(fire) = %q, want 🔥", EmojiGlyph("fire"))
	}
	if EmojiGlyph("not-a-real-emoji") != ":not-a-real-emoji:" {
		t.Errorf("EmojiGlyph should fall back to :name: for unknown emoji, got %q", EmojiGlyph("not-a-real-emoji"))
	}
}

// ── SetUserToken ─────────────────────────────────────────────────────────
//
// SetUserToken is deliberately not tested: it rebuilds s.api from scratch
// (slack.New(tok, slack.OptionRetry(3))) without slack.OptionAPIURL, so a
// hermetic test would need either a production change (out of scope — the
// plan's whole bet is zero production changes) or to let the rebuilt client
// make a real call to slack.com, which CONTRIBUTING.md's hermeticity rule
// forbids. (Confirmed the hard way: an earlier draft of this test did call
// out to production Slack and got a real invalid_auth response back.)
