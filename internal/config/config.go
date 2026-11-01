// Package config loads mcp-matrix-ro settings from the environment, with CLI flag overrides.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Network tags a room by a regex on sender/member IDs.
type Network struct {
	Name string
	Re   *regexp.Regexp
}

// Config is the resolved configuration.
type Config struct {
	Homeserver string
	Token      string
	User       string // password login; with a token, only a fallback when the token is rejected
	Password   string
	// PasswordErr is why the password fallback is unavailable although a
	// token is set (e.g. an unreadable MATRIX_PASSWORD_FILE). Never fatal.
	PasswordErr error
	TZ          *time.Location
	Me          []string // empty = whoami user only
	Networks    []Network
	ExcludeIDs  map[string]bool
	ExcludeRe   []*regexp.Regexp // on room names
	ExcludeBots *regexp.Regexp
	MaxBody     int
	StateDir    string
	HTTPTimeout time.Duration
	LogLevel    slog.Level
}

// Raw holds the string form of every setting, filled from env then flags.
type Raw struct {
	Homeserver, Token, TokenFile, User, Password, PasswordFile, TZ, Me, Networks, Exclude, ExcludeBots,
	MaxBody, StateDir, HTTPTimeout, Log string
}

// Defaults (SPEC §4).
const (
	DefaultTZ          = "Asia/Ho_Chi_Minh"
	DefaultNetworks    = `whatsapp=^@whatsapp_,telegram=^@telegram_,signal=^@signal_`
	DefaultExcludeBots = `^@(whatsapp|telegram|signal)bot:`
)

// FromEnv reads the environment.
func FromEnv(getenv func(string) string) Raw {
	def := func(k, d string) string {
		if v, ok := lookup(getenv, k); ok {
			return v
		}
		return d
	}
	return Raw{
		Homeserver:   getenv("MATRIX_HOMESERVER"),
		Token:        getenv("MATRIX_ACCESS_TOKEN"),
		TokenFile:    getenv("MATRIX_ACCESS_TOKEN_FILE"),
		User:         getenv("MATRIX_USER"),
		Password:     getenv("MATRIX_PASSWORD"),
		PasswordFile: getenv("MATRIX_PASSWORD_FILE"),
		TZ:           def("MCP_MATRIX_RO_TZ", DefaultTZ),
		Me:           getenv("MCP_MATRIX_RO_ME"),
		Networks:     def("MCP_MATRIX_RO_NETWORKS", DefaultNetworks),
		Exclude:      getenv("MCP_MATRIX_RO_EXCLUDE"),
		ExcludeBots:  def("MCP_MATRIX_RO_EXCLUDE_BOTS", DefaultExcludeBots),
		MaxBody:      def("MCP_MATRIX_RO_MAX_BODY", "500"),
		StateDir:     getenv("MCP_MATRIX_RO_STATE_DIR"),
		HTTPTimeout:  def("MCP_MATRIX_RO_HTTP_TIMEOUT", "20s"),
		Log:          def("MCP_MATRIX_RO_LOG", "warn"),
	}
}

// lookup treats empty values as unset so `VAR=` does not wipe a default.
func lookup(getenv func(string) string, k string) (string, bool) {
	v := getenv(k)
	return v, v != ""
}

// RegisterFlags adds override flags for every setting to fs.
// The token and password are deliberately not flags (they would show in `ps`).
func (r *Raw) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&r.Homeserver, "homeserver", r.Homeserver, "homeserver base URL (MATRIX_HOMESERVER)")
	fs.StringVar(&r.TokenFile, "token-file", r.TokenFile, "file containing the access token (MATRIX_ACCESS_TOKEN_FILE)")
	fs.StringVar(&r.User, "user", r.User, "Matrix user ID or localpart for password login (MATRIX_USER)")
	fs.StringVar(&r.PasswordFile, "password-file", r.PasswordFile, "file containing the password (MATRIX_PASSWORD_FILE)")
	fs.StringVar(&r.TZ, "tz", r.TZ, "display timezone (MCP_MATRIX_RO_TZ)")
	fs.StringVar(&r.Me, "me", r.Me, "comma list of user IDs rendered as 'me' (MCP_MATRIX_RO_ME)")
	fs.StringVar(&r.Networks, "networks", r.Networks, "name=regex pairs for network tagging (MCP_MATRIX_RO_NETWORKS)")
	fs.StringVar(&r.Exclude, "exclude", r.Exclude, "room IDs or name regexes to hide (MCP_MATRIX_RO_EXCLUDE)")
	fs.StringVar(&r.ExcludeBots, "exclude-bots", r.ExcludeBots, "hide rooms whose only other member matches (MCP_MATRIX_RO_EXCLUDE_BOTS)")
	fs.StringVar(&r.MaxBody, "max-body", r.MaxBody, "truncate bodies to N chars (MCP_MATRIX_RO_MAX_BODY)")
	fs.StringVar(&r.StateDir, "state-dir", r.StateDir, "local cache dir (MCP_MATRIX_RO_STATE_DIR)")
	fs.StringVar(&r.HTTPTimeout, "http-timeout", r.HTTPTimeout, "per-request timeout (MCP_MATRIX_RO_HTTP_TIMEOUT)")
	fs.StringVar(&r.Log, "log", r.Log, "log level: debug|info|warn|error (MCP_MATRIX_RO_LOG)")
}

// Resolve validates and converts Raw into Config.
func (r Raw) Resolve() (*Config, error) {
	c := &Config{Homeserver: strings.TrimRight(strings.TrimSpace(r.Homeserver), "/")}
	if c.Homeserver == "" {
		return nil, errors.New("MATRIX_HOMESERVER is required")
	}
	var err error
	if r.TokenFile != "" { // the file wins over the variable
		b, err := os.ReadFile(r.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading MATRIX_ACCESS_TOKEN_FILE: %w", err)
		}
		if c.Token, err = cleanToken(decodeText(b), "MATRIX_ACCESS_TOKEN_FILE"); err != nil {
			return nil, err
		}
	} else if c.Token, err = cleanToken(r.Token, "MATRIX_ACCESS_TOKEN"); err != nil {
		return nil, err
	}
	// With a token, the password settings are only a fallback for when the
	// homeserver rejects the token, so a leftover MATRIX_PASSWORD_FILE (missing
	// secret, unreadable file) cannot break a token setup: the error is kept
	// in PasswordErr and the fallback is disabled.
	c.User = strings.TrimSpace(r.User)
	c.Password = r.Password
	if r.PasswordFile != "" {
		b, err := os.ReadFile(r.PasswordFile)
		switch {
		case err != nil && c.Token == "":
			return nil, fmt.Errorf("reading MATRIX_PASSWORD_FILE: %w", err)
		case err != nil:
			c.Password, c.PasswordErr = "", fmt.Errorf("reading MATRIX_PASSWORD_FILE: %w", err)
		default:
			c.Password = strings.TrimRight(decodeText(b), "\r\n")
		}
	}
	if c.Token != "" && (c.User == "" || c.Password == "") {
		c.User, c.Password = "", ""
	}
	switch {
	case c.Token != "": // a configured token always wins
	case c.User != "" && c.Password != "":
	case c.User != "":
		return nil, errors.New("MATRIX_USER is set but MATRIX_PASSWORD / MATRIX_PASSWORD_FILE is missing")
	default:
		return nil, errors.New("set MATRIX_ACCESS_TOKEN (or _FILE), or MATRIX_USER + MATRIX_PASSWORD (or _FILE)")
	}
	if c.TZ, err = time.LoadLocation(r.TZ); err != nil {
		return nil, fmt.Errorf("MCP_MATRIX_RO_TZ: %w", err)
	}
	c.Me = splitList(r.Me)
	for _, pair := range splitList(r.Networks) {
		name, re, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("MCP_MATRIX_RO_NETWORKS: bad entry %q (want name=regex)", pair)
		}
		rx, err := regexp.Compile(re)
		if err != nil {
			return nil, fmt.Errorf("MCP_MATRIX_RO_NETWORKS %s: %w", name, err)
		}
		c.Networks = append(c.Networks, Network{Name: strings.ToLower(name), Re: rx})
	}
	c.ExcludeIDs = map[string]bool{}
	for _, e := range splitList(r.Exclude) {
		if strings.HasPrefix(e, "!") {
			c.ExcludeIDs[e] = true
			continue
		}
		rx, err := regexp.Compile("(?i)" + e)
		if err != nil {
			return nil, fmt.Errorf("MCP_MATRIX_RO_EXCLUDE %q: %w", e, err)
		}
		c.ExcludeRe = append(c.ExcludeRe, rx)
	}
	if r.ExcludeBots != "" {
		if c.ExcludeBots, err = regexp.Compile(r.ExcludeBots); err != nil {
			return nil, fmt.Errorf("MCP_MATRIX_RO_EXCLUDE_BOTS: %w", err)
		}
	}
	if c.MaxBody, err = strconv.Atoi(r.MaxBody); err != nil || c.MaxBody < 0 {
		return nil, fmt.Errorf("MCP_MATRIX_RO_MAX_BODY: want a non-negative integer, got %q", r.MaxBody)
	}
	if c.HTTPTimeout, err = time.ParseDuration(r.HTTPTimeout); err != nil || c.HTTPTimeout <= 0 {
		return nil, fmt.Errorf("MCP_MATRIX_RO_HTTP_TIMEOUT: want a duration like 20s, got %q", r.HTTPTimeout)
	}
	if err := c.LogLevel.UnmarshalText([]byte(r.Log)); err != nil {
		return nil, fmt.Errorf("MCP_MATRIX_RO_LOG: %w", err)
	}
	c.StateDir = r.StateDir
	if c.StateDir == "" {
		c.StateDir = defaultStateDir()
	}
	return c, nil
}

func defaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "mcp-matrix-ro")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "mcp-matrix-ro")
	}
	return filepath.Join(os.TempDir(), "mcp-matrix-ro")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// decodeText returns file contents as a string, dropping a UTF-8 byte order
// mark and decoding UTF-16 files (as written by some Windows editors and
// PowerShell's `>` redirection).
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:])
	case len(b) >= 2 && (b[0] == 0xFF && b[1] == 0xFE || b[0] == 0xFE && b[1] == 0xFF):
		le := b[0] == 0xFF
		b = b[2:]
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if le {
				u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
			} else {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			}
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}

// cleanToken trims surrounding whitespace (including CRLF line endings),
// tolerates a pasted "Bearer " prefix or surrounding quotes, and rejects
// anything that cannot be sent in an HTTP header. The token itself is never
// included in the error.
func cleanToken(s, source string) (string, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "\uFEFF"))
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	if len(s) > 7 && strings.EqualFold(s[:7], "bearer ") {
		s = strings.TrimSpace(s[7:])
	}
	if k, v, ok := strings.Cut(s, "="); ok && k == "MATRIX_ACCESS_TOKEN" {
		s = strings.TrimSpace(v)
	}
	for i, r := range s {
		var what string
		switch {
		case r == '\n' || r == '\r':
			what = "a line break (the token must be on a single line, with nothing else in the file)"
		case r == 0:
			what = "a NUL byte (is the file saved in an unusual encoding?)"
		case r < 0x20 || r == 0x7f:
			what = fmt.Sprintf("a control character (U+%04X)", r)
		case r == ' ' || r == '\t':
			what = "a space in the middle (only the token itself should be there)"
		case r == utf8.RuneError || r > 0x7e:
			what = fmt.Sprintf("a non-ASCII character (U+%04X)", r)
		default:
			continue
		}
		return "", fmt.Errorf("%s: the token contains %s at character %d of %d; Matrix access tokens are printable ASCII without spaces", source, what, utf8.RuneCountInString(s[:i])+1, utf8.RuneCountInString(s))
	}
	return s, nil
}
