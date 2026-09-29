package app

import (
	"fmt"
	"testing"

	"github.com/kurenn/slack-tui/internal/data"
	"github.com/kurenn/slack-tui/internal/source"
	"github.com/kurenn/slack-tui/internal/ui/components"
)

type dmActivitySource struct{ source.Source }

func (s dmActivitySource) Unread(id string) (int, error) {
	if id == "bad" {
		return 0, fmt.Errorf("missing_scope")
	}
	return 1, nil
}

func TestDiscoveredDMGetsUnreadBadgeWithoutLosingExistingState(t *testing.T) {
	m := newSized()
	m.src = dmActivitySource{m.src}
	m.meta["dm_ada"] = components.Meta{Unread: 7, Mention: true}
	cmd := m.applyDiscoveredDMs(dmDiscoveredMsg{convs: []data.Conversation{
		{ID: "DNEW", Type: "dm", UserID: "ada", Name: "ada"},
		{ID: "DNEW", Type: "dm", UserID: "ada", Name: "ada"},
	}})
	if cmd == nil {
		t.Fatal("new DM was not checked immediately")
	}
	result := cmd().(unreadMsg)
	next, _ := m.Update(result)
	m = next.(Model)
	if m.meta["DNEW"].Unread != 1 {
		t.Fatal("first message did not set unread badge")
	}
	if m.meta["dm_ada"].Unread != 7 || !m.meta["dm_ada"].Mention {
		t.Fatal("existing unread state was lost")
	}
	count := 0
	for _, d := range m.ws.DMs {
		if d.ID == "DNEW" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate DM rows: %d", count)
	}
	if m.dmHeadIDs()[0] != "DNEW" {
		t.Fatal("new DM was not prioritized")
	}
}

func TestUnreadFailureIsVisibleAndKeepsPreviousCounts(t *testing.T) {
	m := newSized()
	m.src = dmActivitySource{m.src}
	m.meta["bad"] = components.Meta{Unread: 3}
	msg := m.unreadCmd([]string{"bad", "design"})().(unreadMsg)
	if msg.err == nil {
		t.Fatal("poll failure was discarded")
	}
	next, _ := m.Update(msg)
	m = next.(Model)
	if m.loadErr == nil {
		t.Fatal("poll failure was not surfaced")
	}
	if m.meta["bad"].Unread != 3 || m.meta["design"].Unread != 1 {
		t.Fatal("partial poll corrupted counts")
	}
}

func TestDMDiscoveryPreservesSidebarCursorAndGroupPreference(t *testing.T) {
	m := newSized()
	m.sideSel = m.flatIndexOf("dm_lin")
	m.applyDiscoveredDMs(dmDiscoveredMsg{convs: []data.Conversation{
		{ID: "DNEW", Type: "dm", UserID: "ada", Name: "aaa"},
		{ID: "GNEW", Type: "dm", Name: "group"},
	}})
	if m.sideItems()[m.sideSel].Conv.ID != "dm_lin" {
		t.Fatal("discovery moved sidebar cursor")
	}
	if _, ok := m.ws.Conversation("GNEW"); ok {
		t.Fatal("discovery ignored disabled group DM preference")
	}
}
