package app_test

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RoiArthurB/mxread/internal/app"
	"github.com/RoiArthurB/mxread/internal/config"
	"github.com/RoiArthurB/mxread/internal/matrix"
	"github.com/RoiArthurB/mxread/internal/render"
	"github.com/RoiArthurB/mxread/internal/resolve"
	"github.com/RoiArthurB/mxread/internal/testutil/fakehs"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Now is fixed: Sat 31 Oct 2026 19:00 in Asia/Ho_Chi_Minh.
var now = time.UnixMilli(1793448000000)

func setup(t *testing.T, opts ...func(*fakehs.Server)) (*app.App, *fakehs.Server) {
	t.Helper()
	hs := fakehs.New()
	for _, o := range opts {
		o(hs)
	}
	t.Cleanup(func() {
		hs.Close()
		if v := hs.Violations(); len(v) > 0 {
			t.Errorf("non-allowlisted requests reached the server:\n%s", strings.Join(v, "\n"))
		}
	})
	env := map[string]string{
		"MATRIX_HOMESERVER":   hs.URL,
		"MATRIX_ACCESS_TOKEN": fakehs.Token,
		"MXREAD_ME":           "@whatsapp_84900000001:dom",
		"MXREAD_EXCLUDE":      "^server notices$",
		"MXREAD_STATE_DIR":    t.TempDir(),
	}
	cfg, err := config.FromEnv(func(k string) string { return env[k] }).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc, err := matrix.New(matrix.Options{Homeserver: cfg.Homeserver, Token: cfg.Token, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	return app.New(cfg, mc, log, func() time.Time { return now }), hs
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(fakehs.Dir(), "..", "golden", name+".txt")
	if *update {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file (run go test ./... -update): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s mismatch (run go test ./... -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func clock(a *app.App) render.Clock { return a.Clock() }

func TestChats(t *testing.T) {
	a, _ := setup(t)
	r, err := a.Chats(context.Background(), app.ChatsParams{})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "chats", render.Chats(r, clock(a)))
	for _, c := range r.Chats {
		switch c.ID {
		case "!bridge:dom", "!notices:dom", "!space:dom":
			t.Errorf("hidden room %s listed", c.ID)
		case "!dm-me:dom":
			if !c.DM || c.Name != "Mẹ" || c.Network != "whatsapp" {
				t.Errorf("bad DM classification: %+v", c)
			}
		case "!tango:dom":
			if c.DM || c.Network != "whatsapp" {
				t.Errorf("bad group classification: %+v", c)
			}
		case "!old:dom":
			if !c.DM || c.Name != "Carol" || c.Network != "matrix" {
				t.Errorf("bad matrix DM: %+v", c)
			}
		}
	}
	r, _ = a.Chats(context.Background(), app.ChatsParams{ActiveWithin: "3d", Network: "matrix"})
	if r.Matching != 1 || r.Chats[0].ID != "!lab:dom" {
		t.Errorf("filtered chats: %+v", r)
	}
}

func TestIndexCachedAcrossProcesses(t *testing.T) {
	a, hs := setup(t)
	ctx := context.Background()
	if _, err := a.Chats(ctx, app.ChatsParams{}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	// A new App over the same state dir must not sync again.
	b := app.New(a.Cfg, a.MC, a.Log, a.Now)
	if _, err := b.Chats(ctx, app.ChatsParams{}); err != nil {
		t.Fatal(err)
	}
	if n := hs.Count("/sync"); n != 1 {
		t.Fatalf("want 1 sync, got %d", n)
	}
	if n := hs.Count("/whoami"); n != 1 {
		t.Fatalf("want 1 whoami, got %d", n)
	}
}

func TestFindByName(t *testing.T) {
	a, _ := setup(t)
	ctx := context.Background()
	for q, want := range map[string]string{"me": "!dm-me:dom", "MẸ": "!dm-me:dom", "tango": "!tango:dom", "lab cafe": "!lab:dom", "!old:dom": "!old:dom"} {
		r, err := a.Res.Find(ctx, q)
		if err != nil || r.ID != want {
			t.Errorf("Find(%q) = %v, %v; want %s", q, r, err, want)
		}
	}
	if _, err := a.Res.Find(ctx, "zzz"); err == nil {
		t.Error("want not found")
	}
	_, err := a.Res.Find(ctx, "a") // matches several
	if _, ok := err.(*resolve.AmbiguousError); !ok {
		t.Errorf("want ambiguous, got %v", err)
	}
}

func TestRead(t *testing.T) {
	a, hs := setup(t)
	ctx := context.Background()
	r, err := a.Read(ctx, app.ReadParams{Chat: "Mẹ", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "read_dm", render.Read(r, clock(a)))
	if hs.Count("/event/") != 1 {
		t.Errorf("reply parent should be fetched once, got %d", hs.Count("/event/"))
	}

	r, err = a.Read(ctx, app.ReadParams{Chat: "tango", Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "read_group_page1", render.Read(r, clock(a)))
	if r.Cursor == "" {
		t.Fatal("want cursor")
	}
	r2, err := a.Read(ctx, app.ReadParams{Chat: "tango", Limit: 4, Cursor: r.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "read_group_page2", render.Read(r2, clock(a)))
	seen := map[string]bool{}
	for _, m := range append(r.Messages, r2.Messages...) {
		if seen[m.ID] {
			t.Errorf("message %s on both pages", m.ID)
		}
		seen[m.ID] = true
	}

	r, err = a.Read(ctx, app.ReadParams{Chat: "tango", After: "2026-10-31T17:00", Before: "2026-10-31T17:48"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "read_group_window", render.Read(r, clock(a)))
}

func TestReadAmbiguousAndBadCursor(t *testing.T) {
	a, _ := setup(t)
	if _, err := a.Read(context.Background(), app.ReadParams{Chat: "a"}); !app.IsUserError(err) {
		t.Errorf("want user error, got %v", err)
	}
	if _, err := a.Read(context.Background(), app.ReadParams{Chat: "x", Cursor: "garbage"}); !app.IsUserError(err) {
		t.Errorf("want user error, got %v", err)
	}
}

func TestChatInfo(t *testing.T) {
	a, _ := setup(t)
	r, err := a.ChatInfo(context.Background(), "TANGO")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "chat_info", render.ChatInfo(r, clock(a)))
}

func TestCheck(t *testing.T) {
	a, _ := setup(t)
	r, err := a.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.UserID != "@archive:dom" || r.Joined != 7 || r.Visible != 4 || r.Hidden != 3 || r.DMs != 2 || !strings.HasPrefix(r.Search, "supported") {
		b, _ := json.Marshal(r)
		t.Fatalf("unexpected check: %s", b)
	}
	a2, _ := setup(t, func(s *fakehs.Server) { s.SearchUnsupported = true })
	r, _ = a2.Check(context.Background())
	if !strings.HasPrefix(r.Search, "not supported") {
		t.Fatalf("want unsupported, got %s", r.Search)
	}
}
