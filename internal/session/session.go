// Package session builds the Matrix client from config: a configured access
// token, a password login whose token is cached in the state dir (so restarts
// reuse the same device instead of creating a new one), or a token with the
// password login as fallback when the homeserver rejects the token.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/config"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
)

// DeviceName is shown in the account's session list.
const DeviceName = "mcp-matrix-ro (read-only)"

// Saved is the cached result of a password login.
type Saved struct {
	Homeserver  string `json:"homeserver"`
	User        string `json:"user"`
	UserID      string `json:"user_id"`
	DeviceID    string `json:"device_id"`
	AccessToken string `json:"access_token"`
}

// File returns the session file path.
func File(stateDir string) string { return filepath.Join(stateDir, "session.json") }

func load(path string) (*Saved, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Saved
	return &s, json.Unmarshal(b, &s)
}

func save(path string, s *Saved) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Mode describes how cfg logs in, for logs and `check`.
func Mode(cfg *config.Config) string {
	switch {
	case cfg.Token != "" && cfg.Password != "":
		return "access token, with password login as fallback (user " + cfg.User + ")"
	case cfg.Token != "":
		return "access token"
	default:
		return "password (user " + cfg.User + ")"
	}
}

// NewClient returns the read-only client for cfg. transport may be nil.
//
// With a password, the session (token and device) is shared through
// session.json by every process using the same state dir (serve, check,
// digest). They all log in on the same device, and a login on an existing
// device replaces that device's token, so before logging in again a process
// first tries the token another process saved. That way a `check` or
// `digest` run cannot leave a long-running `serve` with a dead token.
func NewClient(cfg *config.Config, log *slog.Logger, transport http.RoundTripper) (*matrix.Client, error) {
	opts := matrix.Options{Homeserver: cfg.Homeserver, Token: cfg.Token, Timeout: cfg.HTTPTimeout, Logger: log, Transport: transport}
	if cfg.Token != "" && cfg.PasswordErr != nil {
		log.Warn("password fallback disabled", "err", cfg.PasswordErr)
	}
	if cfg.Token != "" && cfg.Password == "" {
		return matrix.New(opts)
	}
	if cfg.User == "" || cfg.Password == "" {
		return nil, errors.New("no access token and no user/password configured")
	}
	path := File(cfg.StateDir)
	stored := func() *Saved {
		s, err := load(path)
		if err != nil || s.Homeserver != cfg.Homeserver || s.User != cfg.User || s.AccessToken == "" {
			return nil
		}
		return s
	}
	var deviceID string
	if s := stored(); s != nil {
		deviceID = s.DeviceID
		if opts.Token == "" {
			opts.Token = s.AccessToken
		}
	}
	var c *matrix.Client
	opts.Reauth = func(ctx context.Context, failed string) (string, error) {
		s := stored()
		if s != nil {
			deviceID = s.DeviceID
			if failed != "" && s.AccessToken != failed && s.AccessToken != cfg.Token {
				log.Info("using the session saved by another login", "device", s.DeviceID)
				return s.AccessToken, nil
			}
		}
		log.Warn("logging in with password", "user", cfg.User, "device", deviceID)
		r, err := c.Login(ctx, matrix.LoginRequest{User: cfg.User, Password: cfg.Password, DeviceID: deviceID, DeviceName: DeviceName})
		if err != nil {
			return "", err
		}
		deviceID = r.DeviceID
		ns := &Saved{Homeserver: cfg.Homeserver, User: cfg.User, UserID: r.UserID, DeviceID: r.DeviceID, AccessToken: r.AccessToken}
		if err := save(path, ns); err != nil {
			log.Warn("could not save the session; other processes and the next start will log in again", "path", path, "err", err)
		}
		return r.AccessToken, nil
	}
	var err error
	c, err = matrix.New(opts)
	return c, err
}
