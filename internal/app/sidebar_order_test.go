package app

import (
	"testing"

	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/ui/components"
)

func sidebarFixture() Model {
	m := newSized()
	m.ws.Channels = []data.Conversation{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	m.ws.DMs = []data.Conversation{{ID: "d", Type: "dm"}, {ID: "e", Type: "dm"}}
	m.meta = map[string]components.Meta{"b": {Unread: 2}, "c": {Unread: 1, Mention: true}, "e": {Unread: 1}}
	m.activeID = "a"
	m.sidePinID = ""
	m.sideSel = m.flatIndexOf("a")
	return m
}

func TestSidebarOrderingAndCursorIdentity(t *testing.T) {
	m := sidebarFixture()
	want := []string{"c", "b", "a", "e", "d"}
	var got []string
	for _, item := range m.sideItems() {
		if !item.Header {
			got = append(got, item.Conv.ID)
		}
	}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	m.sideSel = m.flatIndexOf("d")
	next, _ := m.Update(unreadMsg{counts: map[string]int{"d": 3}, seq: m.readSeq})
	n := next.(Model)
	if n.sideItems()[n.sideSel].Conv.ID != "d" {
		t.Fatal("background reordering moved cursor to another conversation")
	}
	if n.flatIndexOf("d") >= n.flatIndexOf("e") {
		t.Fatal("equal unread priority did not retain workspace order")
	}
}

func TestOpenedSidebarRowStaysUntilSwitch(t *testing.T) {
	m := sidebarFixture()
	before := m.flatIndexOf("b")
	m.openChannel("b")
	if m.flatIndexOf("b") != before {
		t.Fatal("reading moved the opened row")
	}
	if m.meta["b"].Unread != 0 {
		t.Fatal("opening did not clear unread")
	}
	m.openChannel("a")
	if m.flatIndexOf("b") <= m.flatIndexOf("a") {
		t.Fatal("previous row was still pinned after switching")
	}
}
