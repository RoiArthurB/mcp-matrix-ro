package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/app"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/render"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/testutil/fakehs"
)

func TestSearchServer(t *testing.T) {
	a, hs := setup(t)
	r, err := a.Search(context.Background(), app.SearchParams{Query: "meeting"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != "server" || len(r.Hits) != 2 {
		t.Fatalf("got mode=%s hits=%d", r.Mode, len(r.Hits))
	}
	golden(t, "search_server", render.Search(r, clock(a)))
	if hs.Count("POST /_matrix/client/v3/search") != 1 {
		t.Error("want one /search call")
	}
	// since filters out older hits
	r, _ = a.Search(context.Background(), app.SearchParams{Query: "meeting", Since: "1d"})
	if len(r.Hits) != 1 {
		t.Errorf("since: want 1 hit, got %d", len(r.Hits))
	}
}

func TestSearchFallbackScan(t *testing.T) {
	a, hs := setup(t, func(s *fakehs.Server) { s.SearchUnsupported = true })
	r, err := a.Search(context.Background(), app.SearchParams{Query: "MEETING"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != "scan" {
		t.Fatalf("want scan mode, got %s", r.Mode)
	}
	out := render.Search(r, clock(a))
	golden(t, "search_scan", out)
	if !strings.HasPrefix(out, `Search "MEETING" via local scan`) {
		t.Errorf("first line must say which mode was used: %s", out)
	}
	if len(r.Hits) != 2 {
		t.Errorf("want 2 hits (recent + 2 days old), got %d", len(r.Hits))
	}
	if hs.Count("/messages") == 0 {
		t.Error("scan should page /messages")
	}
	// Accent-insensitive: "da lat" finds "Đà Lạt".
	r, err = a.Search(context.Background(), app.SearchParams{Query: "da lat", Chat: "carol"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hits) != 1 {
		t.Errorf("accent folding: want 1 hit, got %d", len(r.Hits))
	}
}
