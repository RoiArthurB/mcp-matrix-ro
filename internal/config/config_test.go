package config

import (
	"os"
	"path/filepath"
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
	// A token wins over credentials.
	c, _ = FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MATRIX_USER": "a", "MATRIX_PASSWORD": "p"})).Resolve()
	if c.Token != "t" || c.Password != "" {
		t.Fatalf("token should win: %+v", c)
	}
	if _, err := FromEnv(env(map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_USER": "a"})).Resolve(); err == nil {
		t.Fatal("want error for user without password")
	}
}
