// Package session builds the Matrix client from config: either a configured
// access token, or a password login whose token is cached in the state dir
// so restarts reuse the same device instead of creating a new one.
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

// NewClient returns the read-only client for cfg. transport may be nil.
func NewClient(cfg *config.Config, log *slog.Logger, transport http.RoundTripper) (*matrix.Client, error) {
	opts := matrix.Options{Homeserver: cfg.Homeserver, Token: cfg.Token, Timeout: cfg.HTTPTimeout, Logger: log, Transport: transport}
	if cfg.Token != "" {
		return matrix.New(opts)
	}
	if cfg.User == "" || cfg.Password == "" {
		return nil, errors.New("no access token and no user/password configured")
	}
	path := File(cfg.StateDir)
	var deviceID string
	if s, err := load(path); err == nil && s.Homeserver == cfg.Homeserver && s.User == cfg.User {
		opts.Token, deviceID = s.AccessToken, s.DeviceID
	}
	var c *matrix.Client
	opts.Reauth = func(ctx context.Context) (string, error) {
		log.Info("logging in with password", "user", cfg.User)
		r, err := c.Login(ctx, matrix.LoginRequest{User: cfg.User, Password: cfg.Password, DeviceID: deviceID, DeviceName: DeviceName})
		if err != nil {
			return "", err
		}
		deviceID = r.DeviceID
		s := &Saved{Homeserver: cfg.Homeserver, User: cfg.User, UserID: r.UserID, DeviceID: r.DeviceID, AccessToken: r.AccessToken}
		if err := save(path, s); err != nil {
			log.Warn("could not cache the session; the next start will log in again", "err", err)
		}
		return r.AccessToken, nil
	}
	var err error
	c, err = matrix.New(opts)
	return c, err
}
