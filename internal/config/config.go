// Package config loads mxread settings from the environment, with CLI flag overrides.
package config

import (
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
)

// Network tags a room by a regex on sender/member IDs.
type Network struct {
	Name string
	Re   *regexp.Regexp
}

// Config is the resolved configuration.
type Config struct {
	Homeserver  string
	Token       string
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
	Homeserver, Token, TokenFile, TZ, Me, Networks, Exclude, ExcludeBots,
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
		Homeserver:  getenv("MATRIX_HOMESERVER"),
		Token:       getenv("MATRIX_ACCESS_TOKEN"),
		TokenFile:   getenv("MATRIX_ACCESS_TOKEN_FILE"),
		TZ:          def("MXREAD_TZ", DefaultTZ),
		Me:          getenv("MXREAD_ME"),
		Networks:    def("MXREAD_NETWORKS", DefaultNetworks),
		Exclude:     getenv("MXREAD_EXCLUDE"),
		ExcludeBots: def("MXREAD_EXCLUDE_BOTS", DefaultExcludeBots),
		MaxBody:     def("MXREAD_MAX_BODY", "500"),
		StateDir:    getenv("MXREAD_STATE_DIR"),
		HTTPTimeout: def("MXREAD_HTTP_TIMEOUT", "20s"),
		Log:         def("MXREAD_LOG", "warn"),
	}
}

// lookup treats empty values as unset so `VAR=` does not wipe a default.
func lookup(getenv func(string) string, k string) (string, bool) {
	v := getenv(k)
	return v, v != ""
}

// RegisterFlags adds override flags for every setting to fs.
// The token itself is deliberately not a flag (it would show in `ps`).
func (r *Raw) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&r.Homeserver, "homeserver", r.Homeserver, "homeserver base URL (MATRIX_HOMESERVER)")
	fs.StringVar(&r.TokenFile, "token-file", r.TokenFile, "file containing the access token (MATRIX_ACCESS_TOKEN_FILE)")
	fs.StringVar(&r.TZ, "tz", r.TZ, "display timezone (MXREAD_TZ)")
	fs.StringVar(&r.Me, "me", r.Me, "comma list of user IDs rendered as 'me' (MXREAD_ME)")
	fs.StringVar(&r.Networks, "networks", r.Networks, "name=regex pairs for network tagging (MXREAD_NETWORKS)")
	fs.StringVar(&r.Exclude, "exclude", r.Exclude, "room IDs or name regexes to hide (MXREAD_EXCLUDE)")
	fs.StringVar(&r.ExcludeBots, "exclude-bots", r.ExcludeBots, "hide rooms whose only other member matches (MXREAD_EXCLUDE_BOTS)")
	fs.StringVar(&r.MaxBody, "max-body", r.MaxBody, "truncate bodies to N chars (MXREAD_MAX_BODY)")
	fs.StringVar(&r.StateDir, "state-dir", r.StateDir, "local cache dir (MXREAD_STATE_DIR)")
	fs.StringVar(&r.HTTPTimeout, "http-timeout", r.HTTPTimeout, "per-request timeout (MXREAD_HTTP_TIMEOUT)")
	fs.StringVar(&r.Log, "log", r.Log, "log level: debug|info|warn|error (MXREAD_LOG)")
}

// Resolve validates and converts Raw into Config.
func (r Raw) Resolve() (*Config, error) {
	c := &Config{Homeserver: strings.TrimRight(strings.TrimSpace(r.Homeserver), "/")}
	if c.Homeserver == "" {
		return nil, errors.New("MATRIX_HOMESERVER is required")
	}
	c.Token = strings.TrimSpace(r.Token)
	if r.TokenFile != "" {
		b, err := os.ReadFile(r.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading MATRIX_ACCESS_TOKEN_FILE: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	}
	if c.Token == "" {
		return nil, errors.New("MATRIX_ACCESS_TOKEN or MATRIX_ACCESS_TOKEN_FILE is required")
	}
	var err error
	if c.TZ, err = time.LoadLocation(r.TZ); err != nil {
		return nil, fmt.Errorf("MXREAD_TZ: %w", err)
	}
	c.Me = splitList(r.Me)
	for _, pair := range splitList(r.Networks) {
		name, re, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("MXREAD_NETWORKS: bad entry %q (want name=regex)", pair)
		}
		rx, err := regexp.Compile(re)
		if err != nil {
			return nil, fmt.Errorf("MXREAD_NETWORKS %s: %w", name, err)
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
			return nil, fmt.Errorf("MXREAD_EXCLUDE %q: %w", e, err)
		}
		c.ExcludeRe = append(c.ExcludeRe, rx)
	}
	if r.ExcludeBots != "" {
		if c.ExcludeBots, err = regexp.Compile(r.ExcludeBots); err != nil {
			return nil, fmt.Errorf("MXREAD_EXCLUDE_BOTS: %w", err)
		}
	}
	if c.MaxBody, err = strconv.Atoi(r.MaxBody); err != nil || c.MaxBody < 0 {
		return nil, fmt.Errorf("MXREAD_MAX_BODY: want a non-negative integer, got %q", r.MaxBody)
	}
	if c.HTTPTimeout, err = time.ParseDuration(r.HTTPTimeout); err != nil || c.HTTPTimeout <= 0 {
		return nil, fmt.Errorf("MXREAD_HTTP_TIMEOUT: want a duration like 20s, got %q", r.HTTPTimeout)
	}
	if err := c.LogLevel.UnmarshalText([]byte(r.Log)); err != nil {
		return nil, fmt.Errorf("MXREAD_LOG: %w", err)
	}
	c.StateDir = r.StateDir
	if c.StateDir == "" {
		c.StateDir = defaultStateDir()
	}
	return c, nil
}

func defaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "mxread")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "mxread")
	}
	return filepath.Join(os.TempDir(), "mxread")
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
