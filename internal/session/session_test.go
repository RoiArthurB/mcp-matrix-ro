package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/config"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/session"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/testutil/fakehs"
)

func cfg(t *testing.T, hs *fakehs.Server, dir, password string) *config.Config {
	env := map[string]string{"MATRIX_HOMESERVER": hs.URL, "MATRIX_USER": "archive", "MATRIX_PASSWORD": password, "MCP_MATRIX_RO_STATE_DIR": dir}
	c, err := config.FromEnv(func(k string) string { return env[k] }).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestPasswordLoginCachesSession(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	dir := t.TempDir()
	ctx := context.Background()

	c, err := session.NewClient(cfg(t, hs, dir, fakehs.Password), discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := c.WhoAmI(ctx); err != nil || w.UserID != "@archive:dom" {
		t.Fatalf("whoami after login: %v %v", w, err)
	}
	if hs.LoginCount() != 1 {
		t.Fatalf("want 1 login, got %d", hs.LoginCount())
	}
	st, err := os.Stat(session.File(dir))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("session file: %v %v", st, err)
	}

	// A restarted process reuses the cached token: no new login, no new device.
	c2, _ := session.NewClient(cfg(t, hs, dir, fakehs.Password), discard, nil)
	if _, err := c2.WhoAmI(ctx); err != nil {
		t.Fatal(err)
	}
	if hs.LoginCount() != 1 {
		t.Fatalf("cached session not reused: %d logins", hs.LoginCount())
	}

	// Token invalidated server-side: one re-login on the same device.
	hs.Rotate("rotated-token")
	if _, err := c2.JoinedRooms(ctx); err != nil {
		t.Fatal(err)
	}
	if hs.LoginCount() != 2 {
		t.Fatalf("want a re-login, got %d logins", hs.LoginCount())
	}
	b, _ := os.ReadFile(session.File(dir))
	var s session.Saved
	_ = json.Unmarshal(b, &s)
	if s.AccessToken != "rotated-token" || s.DeviceID != "NEWDEVICE" {
		t.Fatalf("session not updated / device not reused: %+v", s)
	}
	if v := hs.Violations(); len(v) > 0 {
		t.Fatalf("allowlist violations: %v", v)
	}
}

func TestWrongPasswordIsAuthError(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	c, _ := session.NewClient(cfg(t, hs, t.TempDir(), "wrong"), discard, nil)
	_, err := c.WhoAmI(context.Background())
	if !matrix.IsAuth(err) {
		t.Fatalf("want auth error, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatal("password leaked into error")
	}
}

// A `check` or `digest` run that logs in again on the shared device replaces
// the token a long-running `serve` holds. serve must pick up the saved token
// instead of failing, and without logging in once more.
func TestOtherProcessLoginDoesNotBreakServe(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	hs.DeviceTokens = true
	hs.ValidToken = "unused"
	dir := t.TempDir()
	ctx := context.Background()

	serve, _ := session.NewClient(cfg(t, hs, dir, fakehs.Password), discard, nil)
	if _, err := serve.WhoAmI(ctx); err != nil {
		t.Fatal(err)
	}
	// Another process finds its saved token dead (e.g. an earlier manual
	// login on the device) and logs in again on the same device.
	b, _ := os.ReadFile(session.File(dir))
	var s session.Saved
	_ = json.Unmarshal(b, &s)
	cli, err := matrix.New(matrix.Options{Homeserver: hs.URL, Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := cli.Login(ctx, matrix.LoginRequest{User: "archive", Password: fakehs.Password, DeviceID: s.DeviceID})
	if err != nil {
		t.Fatal(err)
	}
	s.AccessToken = r.AccessToken
	nb, _ := json.Marshal(s)
	if err := os.WriteFile(session.File(dir), nb, 0o600); err != nil {
		t.Fatal(err)
	}
	logins := hs.LoginCount()

	if _, err := serve.JoinedRooms(ctx); err != nil {
		t.Fatalf("serve after another login: %v", err)
	}
	if hs.LoginCount() != logins {
		t.Fatalf("serve logged in again (%d -> %d) instead of reusing the saved token", logins, hs.LoginCount())
	}
	// Without a usable saved token, serve logs in by itself.
	_ = os.Remove(session.File(dir))
	if _, err := cli.Login(ctx, matrix.LoginRequest{User: "archive", Password: fakehs.Password, DeviceID: s.DeviceID}); err != nil {
		t.Fatal(err)
	}
	if _, err := serve.JoinedRooms(ctx); err != nil {
		t.Fatalf("serve re-login: %v", err)
	}
	if v := hs.Violations(); len(v) > 0 {
		t.Fatalf("allowlist violations: %v", v)
	}
}

// With both a token and a password, a rejected token falls back to a
// password login, whose session is then saved and reused.
func TestTokenFallsBackToPassword(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	hs.DeviceTokens = true
	hs.ValidToken = "unused"
	dir := t.TempDir()
	env := map[string]string{"MATRIX_HOMESERVER": hs.URL, "MATRIX_ACCESS_TOKEN": "syt_dead", "MATRIX_USER": "archive", "MATRIX_PASSWORD": fakehs.Password, "MCP_MATRIX_RO_STATE_DIR": dir}
	c, err := config.FromEnv(func(k string) string { return env[k] }).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(session.Mode(c), "fallback") {
		t.Fatalf("mode: %s", session.Mode(c))
	}
	mc, _ := session.NewClient(c, discard, nil)
	if _, err := mc.WhoAmI(context.Background()); err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if hs.LoginCount() != 1 {
		t.Fatalf("want 1 login, got %d", hs.LoginCount())
	}
	// The next start still has the dead token configured, but reuses the
	// saved session instead of logging in again.
	mc2, _ := session.NewClient(c, discard, nil)
	if _, err := mc2.WhoAmI(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hs.LoginCount() != 1 {
		t.Fatalf("saved session not reused: %d logins", hs.LoginCount())
	}
}

func TestTokenOnlyHasNoFallback(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	env := map[string]string{"MATRIX_HOMESERVER": hs.URL, "MATRIX_ACCESS_TOKEN": "syt_dead", "MCP_MATRIX_RO_STATE_DIR": t.TempDir()}
	c, _ := config.FromEnv(func(k string) string { return env[k] }).Resolve()
	mc, _ := session.NewClient(c, discard, nil)
	_, err := mc.WhoAmI(context.Background())
	var ae *matrix.AuthError
	if !errors.As(err, &ae) || ae.Password || hs.LoginCount() != 0 {
		t.Fatalf("want token-only auth error and no login, got %v (%d logins)", err, hs.LoginCount())
	}
}
