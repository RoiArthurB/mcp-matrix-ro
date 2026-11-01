package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"MATRIX_HOMESERVER": "https://m.example.org/", "MATRIX_ACCESS_TOKEN": "tok",
	})).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.Homeserver != "https://m.example.org" || c.TZ.String() != DefaultTZ || c.MaxBody != 500 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if len(c.Networks) != 3 || c.Networks[0].Name != "whatsapp" || !c.Networks[0].Re.MatchString("@whatsapp_8412:ex") {
		t.Fatalf("bad networks: %+v", c.Networks)
	}
	if !c.ExcludeBots.MatchString("@whatsappbot:ex") {
		t.Fatal("bot regex")
	}
}

func TestTokenFileAndExclude(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("  filetok\n"), 0o600)
	r := FromEnv(env(map[string]string{
		"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "envtok",
		"MATRIX_ACCESS_TOKEN_FILE": f, "MCP_MATRIX_RO_EXCLUDE": "!abc:m, server notices ,^WhatsApp bridge",
		"MCP_MATRIX_RO_ME": "@archive:m,@whatsapp_84:m",
	}))
	c, err := r.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "filetok" || !c.ExcludeIDs["!abc:m"] || len(c.ExcludeRe) != 2 || len(c.Me) != 2 {
		t.Fatalf("got %+v", c)
	}
	if !c.ExcludeRe[0].MatchString("Server Notices") {
		t.Fatal("exclude regex should be case-insensitive")
	}
}

func TestErrors(t *testing.T) {
	for _, m := range []map[string]string{
		{},
		{"MATRIX_HOMESERVER": "https://m"},
		{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MCP_MATRIX_RO_TZ": "Mars/Base"},
		{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MCP_MATRIX_RO_NETWORKS": "bad"},
		{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MCP_MATRIX_RO_MAX_BODY": "x"},
		{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MCP_MATRIX_RO_HTTP_TIMEOUT": "soon"},
	} {
		if _, err := FromEnv(env(m)).Resolve(); err == nil {
			t.Errorf("want error for %v", m)
		}
	}
}

func TestPasswordLogin(t *testing.T) {
	f := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(f, []byte(" p@ss word \n"), 0o600)
	c, err := FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_USER": "archive", "MATRIX_PASSWORD_FILE": f})).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if c.User != "archive" || c.Password != " p@ss word " || c.Token != "" {
		t.Fatalf("got %+v", c)
	}
	// A token is used first; the credentials are kept as a fallback.
	c, _ = FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MATRIX_USER": "a", "MATRIX_PASSWORD": "p"})).Resolve()
	if c.Token != "t" || c.User != "a" || c.Password != "p" {
		t.Fatalf("token first, password as fallback: %+v", c)
	}
	if _, err := FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_USER": "a"})).Resolve(); err == nil {
		t.Fatal("want error for user without password")
	}
}

func tokenFrom(t *testing.T, content []byte) (string, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(f, content, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN_FILE": f})).Resolve()
	if err != nil {
		return "", err
	}
	return c.Token, nil
}

func TestTokenFileCleanup(t *testing.T) {
	utf16le := []byte{0xFF, 0xFE}
	for _, r := range "syt_abc\r\n" {
		utf16le = append(utf16le, byte(r), 0)
	}
	for name, content := range map[string][]byte{
		"LF":          []byte("syt_abc\n"),
		"CRLF":        []byte("syt_abc\r\n"),
		"spaces/tabs": []byte(" \tsyt_abc \t\n\n"),
		"UTF-8 BOM":   []byte("\xEF\xBB\xBFsyt_abc\n"),
		"UTF-16 LE":   utf16le,
		"quoted":      []byte("\"syt_abc\"\n"),
		"bearer":      []byte("Bearer syt_abc\n"),
		"env style":   []byte("MATRIX_ACCESS_TOKEN=syt_abc\n"),
	} {
		got, err := tokenFrom(t, content)
		if err != nil || got != "syt_abc" {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
}

func TestTokenFileInvalid(t *testing.T) {
	for name, tc := range map[string]struct {
		content []byte
		want    string
	}{
		"two lines":    {[]byte("syt_abc\nsyt_def\n"), "line break"},
		"inner CR":     {[]byte("syt_a\rbc"), "line break"},
		"inner space":  {[]byte("syt_a bc"), "space"},
		"control char": {[]byte("syt_a\x01bc"), "control character"},
		"NUL":          {[]byte("s\x00y\x00t\x00"), "NUL"},
		"non-ASCII":    {[]byte("syt_é"), "non-ASCII"},
	} {
		_, err := tokenFrom(t, tc.content)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "MATRIX_ACCESS_TOKEN_FILE") {
			t.Errorf("%s: want error mentioning %q, got %v", name, tc.want, err)
			continue
		}
		if strings.Contains(err.Error(), "syt_") {
			t.Errorf("%s: error leaks the token: %v", name, err)
		}
	}
	// The variable form is validated too.
	if _, err := FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "a\nb"})).Resolve(); err == nil || !strings.Contains(err.Error(), "MATRIX_ACCESS_TOKEN:") {
		t.Errorf("env token: got %v", err)
	}
	// A bad variable is ignored when the file wins.
	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("good\n"), 0o600)
	if c, err := FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "a\nb", "MATRIX_ACCESS_TOKEN_FILE": f})).Resolve(); err != nil || c.Token != "good" {
		t.Errorf("file should win: %v", err)
	}
}

func TestTokenIgnoresPasswordSettings(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("syt_abc\n"), 0o600)
	c, err := FromEnv(env(map[string]string{
		"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN_FILE": f,
		"MATRIX_USER": "archive", "MATRIX_PASSWORD_FILE": "/run/secrets/does-not-exist",
	})).Resolve()
	if err != nil {
		t.Fatalf("missing password file must not matter with a token: %v", err)
	}
	if c.Token != "syt_abc" || c.User != "" || c.Password != "" || c.PasswordErr == nil {
		t.Fatalf("got %+v", c)
	}
}
