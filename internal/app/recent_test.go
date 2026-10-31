package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/RoiArthurB/mxread/internal/app"
	"github.com/RoiArthurB/mxread/internal/render"
)

func TestRecent(t *testing.T) {
	a, hs := setup(t)
	r, err := a.Recent(context.Background(), app.RecentParams{})
	if err != nil {
		t.Fatal(err)
	}
	out := render.Recent(r, clock(a))
	golden(t, "recent", out)
	if n := hs.Count("/sync"); n != 1 {
		t.Errorf("want exactly 1 sync, got %d", n)
	}
	// Only the group whose sync window was too short is paged back.
	if n := hs.Count("/messages"); n != 1 {
		t.Errorf("want 1 backfill /messages call, got %d: %v", n, hs.Requests())
	}
	for _, hidden := range []string{"Server Notices", "!bridge:dom", "Successfully logged in", "Carol"} {
		if strings.Contains(out, hidden) {
			t.Errorf("output contains %q", hidden)
		}
	}
	if strings.Count(out, render.Disclaimer) != 1 {
		t.Error("disclaimer must appear exactly once")
	}
	if len(out) > 4000 {
		t.Errorf("recent output too large for the fixture: %d chars", len(out))
	}
}

func TestRecentLimitsAndFilters(t *testing.T) {
	a, _ := setup(t)
	ctx := context.Background()
	r, err := a.Recent(ctx, app.RecentParams{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 5 || !r.Truncated {
		t.Errorf("limit: total=%d truncated=%v", r.Total, r.Truncated)
	}
	if r.Chats[0].ID != "!dm-me:dom" {
		t.Errorf("newest chat first, got %s", r.Chats[0].ID)
	}
	golden(t, "recent_limit5", render.Recent(r, clock(a)))

	r, _ = a.Recent(ctx, app.RecentParams{Network: "matrix", Since: "2026-10-31"})
	if len(r.Chats) != 1 || r.Chats[0].ID != "!lab:dom" {
		t.Errorf("network filter: %+v", r.Chats)
	}
	golden(t, "recent_matrix", render.Recent(r, clock(a)))

	r, _ = a.Recent(ctx, app.RecentParams{Chat: "tango", PerChat: 3})
	if len(r.Chats) != 1 || len(r.Chats[0].Messages) != 3 || !r.Chats[0].More {
		t.Errorf("chat filter/per_chat: %+v", r.Chats)
	}

	r, _ = a.Recent(ctx, app.RecentParams{Since: "30d", Limit: -1, PerChat: 100})
	found := false
	for _, c := range r.Chats {
		if c.ID == "!old:dom" {
			found = true
		}
	}
	if !found {
		t.Error("30d window should include the old DM")
	}
	if _, err := a.Recent(ctx, app.RecentParams{Since: "last tuesday"}); !app.IsUserError(err) {
		t.Errorf("want user error for bad since, got %v", err)
	}
}
