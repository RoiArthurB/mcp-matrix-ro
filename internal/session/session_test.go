package session_test

import (
	"context"
	"encoding/json"
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
