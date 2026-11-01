package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/testutil/fakehs"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mcp-matrix-ro-test")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "mcp-matrix-ro")
	out, err := exec.Command("go", append([]string{"build", "-o", binary}, append(raceFlag(), ".")...)...).CombinedOutput()
	if err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func env(t *testing.T, hs *fakehs.Server, extra ...string) []string {
	return append([]string{
		"PATH=" + os.Getenv("PATH"),
		"MATRIX_HOMESERVER=" + hs.URL,
		"MATRIX_ACCESS_TOKEN=" + fakehs.Token,
		"MCP_MATRIX_RO_ME=@whatsapp_84900000001:dom",
		"MCP_MATRIX_RO_EXCLUDE=^server notices$",
		"MCP_MATRIX_RO_STATE_DIR=" + t.TempDir(),
	}, extra...)
}

func runCmd(t *testing.T, envv []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = envv
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

func noViolations(t *testing.T, hs *fakehs.Server) {
	t.Helper()
	if v := hs.Violations(); len(v) > 0 {
		t.Fatalf("allowlist violations: %v", v)
	}
}

// MCP smoke test: initialize -> tools/list -> tools/call recent, over stdio.
func TestServeSmoke(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	cmd := exec.Command(binary, "serve")
	cmd.Env = env(t, hs, "MCP_MATRIX_RO_LOG=debug")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "smoke"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Tools) != 5 {
		t.Fatalf("want 5 tools, got %d", len(tl.Tools))
	}
	for _, tool := range tl.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s not readOnlyHint", tool.Name)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recent", Arguments: map[string]any{"since": "2026-10-01"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError || !strings.Contains(text, "Mẹ (WhatsApp DM)") {
		t.Fatalf("bad recent result: %s", text)
	}
	_ = cs.Close()
	_ = cmd.Wait()
	logs := stderr.String()
	if strings.Contains(logs, fakehs.Token) {
		t.Error("token leaked into logs")
	}
	if strings.Contains(logs, "Con ăn cơm") {
		t.Error("message body leaked into logs")
	}
	if !strings.Contains(logs, "matrix request") || !strings.Contains(logs, "/_matrix/client/v3/sync") {
		t.Errorf("debug log should list every request line:\n%s", logs)
	}
	noViolations(t, hs)
}

func TestDigest(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	cursor := filepath.Join(t.TempDir(), "sub", "digest.cursor")
	e := env(t, hs)
	out, errOut, code := runCmd(t, e, "digest", "--since", "2026-10-31T00:00", "--format", "md", "--network", "whatsapp")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "## Mẹ (WhatsApp DM)") || strings.Contains(out, "Lab café") {
		t.Errorf("bad md digest:\n%s", out)
	}
	if strings.Contains(out, "showing last") && strings.Contains(out, "Mẹ (WhatsApp DM) · !dm-me:dom · 8 msgs (") {
		t.Error("digest should not apply a total limit")
	}

	out, errOut, code = runCmd(t, e, "digest", "--format", "json", "--since", "30d", "--cursor-file", cursor)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var r struct {
		Total int `json:"total"`
		Chats []struct {
			ID       string `json:"id"`
			Messages []struct {
				Text string `json:"text"`
			} `json:"messages"`
		} `json:"chats"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Total == 0 {
		t.Fatalf("bad json digest (%v): %s", err, out)
	}
	b, err := os.ReadFile(cursor)
	if err != nil {
		t.Fatal("cursor file not written")
	}
	if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err != nil {
		t.Fatalf("bad cursor content %q", b)
	}
	// With a cursor of "now" (newer than every fixture message), the next run is empty.
	out, _, code = runCmd(t, e, "digest", "--format", "json", "--cursor-file", cursor)
	if code != 0 || !strings.Contains(out, `"total": 0`) {
		t.Errorf("second digest should be empty, got exit %d: %s", code, out)
	}
	noViolations(t, hs)
}

func TestCheck(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	out, errOut, code := runCmd(t, env(t, hs), "check")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"Logged in as: @archive:dom", "Search:       supported", "Joined rooms: 8 (5 visible chats: 3 DMs, 2 groups; 3 hidden)", "WhatsApp"} {
		if !strings.Contains(out, want) {
			t.Errorf("check output missing %q:\n%s", want, out)
		}
	}
	noViolations(t, hs)
}

func TestExitCodes(t *testing.T) {
	hs := fakehs.New()
	e := env(t, hs)
	_, errOut, code := runCmd(t, append(e, "MATRIX_ACCESS_TOKEN=wrong"), "digest")
	if code != exitAuth || !strings.Contains(errOut, "authentication failed") || strings.Contains(errOut, "wrong") {
		t.Errorf("auth failure: exit %d, stderr %q", code, errOut)
	}
	hs.Close()
	_, errOut, code = runCmd(t, e, "digest")
	if code != exitNetwork || !strings.Contains(errOut, "cannot reach the homeserver") {
		t.Errorf("network failure: exit %d, stderr %q", code, errOut)
	}
	_, _, code = runCmd(t, []string{"PATH=" + os.Getenv("PATH")}, "check")
	if code != exitUsage {
		t.Errorf("missing config: exit %d", code)
	}
	out, _, code := runCmd(t, nil, "version")
	if code != 0 || !strings.HasPrefix(out, "mcp-matrix-ro ") {
		t.Errorf("version: %d %q", code, out)
	}
}

func TestPasswordLoginCLI(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	base := []string{"PATH=" + os.Getenv("PATH"), "MATRIX_HOMESERVER=" + hs.URL, "MATRIX_USER=archive", "MCP_MATRIX_RO_STATE_DIR=" + t.TempDir()}
	out, errOut, code := runCmd(t, append(base, "MATRIX_PASSWORD="+fakehs.Password), "check")
	if code != 0 || !strings.Contains(out, "Logged in as: @archive:dom") || !strings.Contains(out, "Login:        password (user archive)") {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	_, errOut, code = runCmd(t, append(base[:3], "MCP_MATRIX_RO_STATE_DIR="+t.TempDir(), "MATRIX_PASSWORD=nope"), "check")
	if code != exitAuth || strings.Contains(errOut, "nope") || !strings.Contains(errOut, "refused the password login") {
		t.Fatalf("wrong password: exit %d, stderr %q", code, errOut)
	}
	noViolations(t, hs)
}

// raceFlag builds the binary under test with -race when MCP_MATRIX_RO_TEST_RACE=1.
func raceFlag() []string {
	if os.Getenv("MCP_MATRIX_RO_TEST_RACE") == "1" {
		return []string{"-race"}
	}
	return nil
}
