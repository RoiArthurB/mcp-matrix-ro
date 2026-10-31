package tools_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/app"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/config"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/testutil/fakehs"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/tools"
)

// MaxSchemaChars is the budget for all tool definitions together (SPEC §10).
const MaxSchemaChars = 4000

func connect(t *testing.T) (*mcp.ClientSession, *fakehs.Server) {
	t.Helper()
	hs := fakehs.New()
	t.Cleanup(hs.Close)
	env := map[string]string{"MATRIX_HOMESERVER": hs.URL, "MATRIX_ACCESS_TOKEN": fakehs.Token, "MXREAD_STATE_DIR": t.TempDir(),
		"MXREAD_ME": "@whatsapp_84900000001:dom", "MXREAD_EXCLUDE": "^server notices$"}
	cfg, err := config.FromEnv(func(k string) string { return env[k] }).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc, _ := matrix.New(matrix.Options{Homeserver: cfg.Homeserver, Token: cfg.Token, Logger: log})
	a := app.New(cfg, mc, log, func() time.Time { return time.UnixMilli(1793448000000) })
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-matrix-ro", Version: "test"}, nil)
	tools.Register(s, a)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, hs
}

func TestToolsListAndSchemaBudget(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 5 {
		t.Fatalf("want exactly 5 tools, got %d", len(res.Tools))
	}
	want := map[string]bool{"recent": true, "chats": true, "read": true, "search": true, "chat_info": true}
	for _, tl := range res.Tools {
		if !want[tl.Name] {
			t.Errorf("unexpected tool %s", tl.Name)
		}
		a := tl.Annotations
		if a == nil || !a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s: must be readOnlyHint=true, destructiveHint=false", tl.Name)
		}
		if tl.OutputSchema != nil {
			t.Errorf("%s: output schema adds tokens; leave it out", tl.Name)
		}
		if n := strings.Count(tl.Description, "\n") + 1; n > 3 {
			t.Errorf("%s: description has %d lines (max 3)", tl.Name, n)
		}
	}
	b, _ := json.Marshal(res.Tools)
	t.Logf("tool definitions: %d chars", len(b))
	if len(b) >= MaxSchemaChars {
		t.Fatalf("tool definitions are %d chars, budget is %d", len(b), MaxSchemaChars)
	}
}

func TestCallRecentAndErrors(t *testing.T) {
	cs, hs := connect(t)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recent", Arguments: map[string]any{"since": "24h"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("recent failed: %v", res.Content)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "## Mẹ (WhatsApp DM)") || !strings.HasPrefix(text, "Messages since") {
		t.Errorf("unexpected recent text:\n%s", text)
	}
	if res.StructuredContent == nil {
		t.Error("want structuredContent")
	}
	if len(res.Content) != 1 {
		t.Errorf("want only the text block, got %d blocks", len(res.Content))
	}

	// Ambiguous chat name: candidates, not a guess.
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{"chat": "a"}})
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "matches") || !strings.Contains(txt, "!tango:dom") {
		t.Errorf("want candidates, got %s", txt)
	}
	// Missing required argument is rejected by schema validation.
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "chat_info", Arguments: map[string]any{}})
	if !res.IsError {
		t.Error("want error for missing chat")
	}
	// Extra, unknown arguments are tolerated.
	for _, name := range []string{"chats", "search", "chat_info"} {
		args := map[string]any{"chat": "tango", "query": "meeting"}
		if name == "chats" {
			args = map[string]any{}
		}
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Errorf("%s failed: %v %s", name, err, res.Content[0].(*mcp.TextContent).Text)
		}
	}
	if v := hs.Violations(); len(v) > 0 {
		t.Fatalf("allowlist violations: %v", v)
	}
}
