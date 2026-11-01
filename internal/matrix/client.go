// Package matrix is a minimal, read-only Matrix client-server API client.
//
// Every request goes through a transport that refuses anything not on the
// allowlist in allowlist.go, before any network I/O happens.
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxConcurrent bounds in-flight requests per Client (SPEC §8).
const MaxConcurrent = 4

// Client is the only way mcp-matrix-ro talks to the homeserver.
type Client struct {
	base   *url.URL
	http   *http.Client
	sem    chan struct{}
	log    *slog.Logger
	reauth func(ctx context.Context, failed string) (string, error)

	refreshMu sync.Mutex // serialises Reauth calls
	mu        sync.Mutex // guards token
	token     string
}

// Options configures a Client.
type Options struct {
	Homeserver string
	Token      string
	// Reauth, if set, obtains a new access token (password login). It is
	// called when Token is empty (failed == "") and after the homeserver
	// rejects the token failed, at most twice per request: the first call
	// may return a token another process saved, the second must log in.
	Reauth  func(ctx context.Context, failed string) (string, error)
	Timeout time.Duration
	Logger  *slog.Logger
	// Transport is the underlying transport (tests); defaults to http.DefaultTransport.
	Transport http.RoundTripper
}

// New builds a Client. The homeserver URL must be http(s).
func New(o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(o.Homeserver, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid homeserver URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("invalid homeserver URL %q: need http(s)://host", o.Homeserver)
	}
	if o.Token == "" && o.Reauth == nil {
		return nil, errors.New("missing access token")
	}
	if o.Timeout <= 0 {
		o.Timeout = 20 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	next := o.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	c := &Client{base: u, token: o.Token, reauth: o.Reauth, sem: make(chan struct{}, MaxConcurrent), log: o.Logger}
	g := &guardTransport{scheme: u.Scheme, host: u.Host, next: next, log: func(m, p string) {
		c.log.Debug("matrix request", "method", m, "path", p)
	}}
	c.http = &http.Client{Transport: g, Timeout: o.Timeout}
	return c, nil
}

// Error is a Matrix error response.
type Error struct {
	Status  int
	ErrCode string `json:"errcode"`
	Message string `json:"error"`
}

func (e *Error) Error() string {
	if e.ErrCode != "" {
		return fmt.Sprintf("matrix: HTTP %d %s: %s", e.Status, e.ErrCode, e.Message)
	}
	return fmt.Sprintf("matrix: HTTP %d", e.Status)
}

// IsUnsupported reports whether err means "endpoint not implemented".
func IsUnsupported(err error) bool {
	var me *Error
	if errors.As(err, &me) {
		return me.Status == http.StatusNotFound || me.Status == http.StatusMethodNotAllowed ||
			me.Status == http.StatusNotImplemented || me.ErrCode == "M_UNRECOGNIZED"
	}
	return false
}

// IsAuth reports whether err is an authentication failure.
func IsAuth(err error) bool {
	var me *Error
	if errors.As(err, &me) {
		return me.Status == http.StatusUnauthorized || me.ErrCode == "M_UNKNOWN_TOKEN" || me.ErrCode == "M_MISSING_TOKEN"
	}
	return false
}

// AuthError is an authentication failure that the client could not recover
// from, explained in terms of the configuration. IsAuth is true for it.
type AuthError struct {
	// Password is true when a password login is configured (alone or as a
	// fallback for a token).
	Password bool
	// LoginFailed is true when the password login itself failed.
	LoginFailed bool
	Err         error
}

func (e *AuthError) Error() string { return e.Hint() + " (" + e.Err.Error() + ")" }

func (e *AuthError) Unwrap() error { return e.Err }

// Hint tells the user what to fix, without the underlying error.
func (e *AuthError) Hint() string {
	switch {
	case e.LoginFailed:
		return "the homeserver refused the password login: check MATRIX_USER and MATRIX_PASSWORD / MATRIX_PASSWORD_FILE"
	case e.Password:
		return "the homeserver rejected the access token even right after a password login"
	default:
		return "the homeserver rejected the access token (expired, logged out, or replaced by a newer login on the same device): " +
			"set a fresh MATRIX_ACCESS_TOKEN, or set MATRIX_USER + MATRIX_PASSWORD so the server can log in again by itself"
	}
}

func path(segs ...string) string {
	var b strings.Builder
	for i, s := range segs {
		if i%2 == 0 {
			b.WriteString(s) // literal
		} else {
			b.WriteString(url.PathEscape(s)) // parameter
		}
	}
	return b.String()
}

const loginPath = "/_matrix/client/v3/login"

func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// refresh replaces the token via Reauth, unless another goroutine already
// replaced the token that failed. Only one Reauth runs at a time; requests
// that do not need a new token are not blocked while it runs.
func (c *Client) refresh(ctx context.Context, failed string) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.currentToken() != failed {
		return nil
	}
	tok, err := c.reauth(ctx, failed)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.token = tok
	c.mu.Unlock()
	return nil
}

// maxRefresh bounds the token replacements per request.
const maxRefresh = 2

// do performs one request, replacing the token (at most maxRefresh times) if
// it is missing or rejected and a Reauth function is configured. rawPath is
// already escaped.
func (c *Client) do(ctx context.Context, method, rawPath string, q url.Values, body any, out any) error {
	if rawPath == loginPath {
		// No token, and no refresh: Login runs inside refresh via Reauth.
		return c.doOnce(ctx, method, rawPath, q, body, out, "")
	}
	if c.reauth == nil {
		err := c.doOnce(ctx, method, rawPath, q, body, out, c.currentToken())
		if IsAuth(err) {
			return &AuthError{Err: err}
		}
		return err
	}
	tok := c.currentToken()
	if tok == "" {
		if err := c.relogin(ctx, ""); err != nil {
			return err
		}
		tok = c.currentToken()
	}
	err := c.doOnce(ctx, method, rawPath, q, body, out, tok)
	for i := 0; i < maxRefresh && IsAuth(err); i++ {
		c.log.Warn("access token rejected by the homeserver; getting a new one", "path", strings.SplitN(rawPath, "?", 2)[0])
		if rerr := c.relogin(ctx, tok); rerr != nil {
			return rerr
		}
		tok = c.currentToken()
		err = c.doOnce(ctx, method, rawPath, q, body, out, tok)
	}
	if IsAuth(err) {
		return &AuthError{Password: true, Err: err}
	}
	return err
}

// relogin wraps refresh, marking authentication failures as a failed login.
func (c *Client) relogin(ctx context.Context, failed string) error {
	err := c.refresh(ctx, failed)
	if err == nil {
		return nil
	}
	if IsAuth(err) {
		c.log.Error("password login failed", "err", err)
		return &AuthError{Password: true, LoginFailed: true, Err: err}
	}
	c.log.Warn("could not log in again", "err", err)
	return fmt.Errorf("logging in again: %w", err)
}

func (c *Client) doOnce(ctx context.Context, method, rawPath string, q url.Values, body any, out any, token string) error {
	u := *c.base
	u.RawPath = strings.TrimRight(c.base.EscapedPath(), "/") + rawPath
	p, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return err
	}
	u.Path = p
	u.RawQuery = q.Encode()

	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	if rawPath != loginPath {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		me := &Error{Status: resp.StatusCode}
		_ = json.Unmarshal(data, me)
		return me
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("matrix: decoding %s: %w", strings.SplitN(rawPath, "?", 2)[0], err)
	}
	return nil
}

// Versions calls GET /_matrix/client/versions (unauthenticated, but the token is harmless).
func (c *Client) Versions(ctx context.Context) (*Versions, error) {
	var v Versions
	return &v, c.do(ctx, http.MethodGet, "/_matrix/client/versions", nil, nil, &v)
}

// WhoAmI calls GET /account/whoami.
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmI, error) {
	var w WhoAmI
	return &w, c.do(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", nil, nil, &w)
}

// JoinedRooms calls GET /joined_rooms.
func (c *Client) JoinedRooms(ctx context.Context) ([]string, error) {
	var r struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/joined_rooms", nil, nil, &r)
	return r.JoinedRooms, err
}

// Sync performs a single non-blocking /sync with an inline filter.
// set_presence=offline is always added. since may be empty (initial sync).
func (c *Client) Sync(ctx context.Context, filter any, since string) (*SyncResponse, error) {
	f, err := json.Marshal(filter)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("filter", string(f))
	q.Set("timeout", "0")
	q.Set("set_presence", "offline")
	q.Set("full_state", "false")
	if since != "" {
		q.Set("since", since)
	}
	var r SyncResponse
	return &r, c.do(ctx, http.MethodGet, "/_matrix/client/v3/sync", q, nil, &r)
}

// RoomState calls GET /rooms/{id}/state.
func (c *Client) RoomState(ctx context.Context, roomID string) ([]Event, error) {
	var evs []Event
	return evs, c.do(ctx, http.MethodGet, path("/_matrix/client/v3/rooms/", roomID, "/state"), nil, nil, &evs)
}

// StateEvent calls GET /rooms/{id}/state/{type}/{stateKey} and decodes the content into out.
func (c *Client) StateEvent(ctx context.Context, roomID, evType, stateKey string, out any) error {
	return c.do(ctx, http.MethodGet, path("/_matrix/client/v3/rooms/", roomID, "/state/", evType, "/", stateKey), nil, nil, out)
}

// MessagesOpts are the parameters of /messages.
type MessagesOpts struct {
	From   string // pagination token; empty = from the end (dir=b) per spec v1.3+
	Dir    string // "b" (default) or "f"
	Limit  int
	Filter any // inline RoomEventFilter
}

// Messages calls GET /rooms/{id}/messages.
func (c *Client) Messages(ctx context.Context, roomID string, o MessagesOpts) (*MessagesResponse, error) {
	q := url.Values{}
	if o.Dir == "" {
		o.Dir = "b"
	}
	q.Set("dir", o.Dir)
	if o.From != "" {
		q.Set("from", o.From)
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Filter != nil {
		f, err := json.Marshal(o.Filter)
		if err != nil {
			return nil, err
		}
		q.Set("filter", string(f))
	}
	var r MessagesResponse
	return &r, c.do(ctx, http.MethodGet, path("/_matrix/client/v3/rooms/", roomID, "/messages"), q, nil, &r)
}

// Members calls GET /rooms/{id}/members?membership=join.
func (c *Client) Members(ctx context.Context, roomID string) ([]Event, error) {
	var r MembersResponse
	q := url.Values{"membership": {"join"}}
	return r.Chunk, c.do(ctx, http.MethodGet, path("/_matrix/client/v3/rooms/", roomID, "/members"), q, nil, &r)
}

// JoinedMembers calls GET /rooms/{id}/joined_members.
func (c *Client) JoinedMembers(ctx context.Context, roomID string) (*JoinedMembersResponse, error) {
	var r JoinedMembersResponse
	return &r, c.do(ctx, http.MethodGet, path("/_matrix/client/v3/rooms/", roomID, "/joined_members"), nil, nil, &r)
}

// Event calls GET /rooms/{id}/event/{eventId}.
func (c *Client) Event(ctx context.Context, roomID, eventID string) (*Event, error) {
	var e Event
	return &e, c.do(ctx, http.MethodGet, path("/_matrix/client/v3/rooms/", roomID, "/event/", eventID), nil, nil, &e)
}

// Context calls GET /rooms/{id}/context/{eventId}.
func (c *Client) Context(ctx context.Context, roomID, eventID string, limit int) (*ContextResponse, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	var r ContextResponse
	return &r, c.do(ctx, http.MethodGet, path("/_matrix/client/v3/rooms/", roomID, "/context/", eventID), q, nil, &r)
}

// Profile calls GET /profile/{userId}.
func (c *Client) Profile(ctx context.Context, userID string) (*Profile, error) {
	var p Profile
	return &p, c.do(ctx, http.MethodGet, path("/_matrix/client/v3/profile/", userID), nil, nil, &p)
}

// SearchRequest is a room_events search.
type SearchRequest struct {
	Term   string
	Rooms  []string
	Limit  int
	Before int // context events before/after each hit
	Next   string
}

// Search calls POST /search (read-only despite being a POST).
func (c *Client) Search(ctx context.Context, r SearchRequest) (*SearchResponse, error) {
	filter := map[string]any{"limit": r.Limit, "types": []string{"m.room.message"}}
	if len(r.Rooms) > 0 {
		filter["rooms"] = r.Rooms
	}
	body := map[string]any{"search_categories": map[string]any{"room_events": map[string]any{
		"search_term": r.Term,
		"keys":        []string{"content.body"},
		"order_by":    "recent",
		"filter":      filter,
		"event_context": map[string]any{
			"before_limit": r.Before, "after_limit": r.Before, "include_profile": false,
		},
	}}}
	q := url.Values{}
	if r.Next != "" {
		q.Set("next_batch", r.Next)
	}
	var out SearchResponse
	return &out, c.do(ctx, http.MethodPost, "/_matrix/client/v3/search", q, body, &out)
}

// LoginRequest is a password login (m.login.password).
type LoginRequest struct {
	User       string // MXID or localpart
	Password   string
	DeviceID   string // reuse a previous device instead of creating a new one
	DeviceName string
}

// LoginResponse is the subset of the /login response the client keeps.
type LoginResponse struct {
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
	UserID      string `json:"user_id"`
}

// Login calls POST /login with a password. It is the only allowlisted call
// that changes server state: it creates (or reuses) a device for this client.
func (c *Client) Login(ctx context.Context, r LoginRequest) (*LoginResponse, error) {
	body := map[string]any{
		"type":       "m.login.password",
		"identifier": map[string]any{"type": "m.id.user", "user": r.User},
		"password":   r.Password,
	}
	if r.DeviceID != "" {
		body["device_id"] = r.DeviceID
	}
	if r.DeviceName != "" {
		body["initial_device_display_name"] = r.DeviceName
	}
	var out LoginResponse
	if err := c.do(ctx, http.MethodPost, loginPath, nil, body, &out); err != nil {
		var me *Error
		if errors.As(err, &me) && me.Status == http.StatusForbidden {
			// Wrong credentials are reported as 403 M_FORBIDDEN; surface them
			// as an authentication failure (IsAuth).
			return nil, &Error{Status: http.StatusUnauthorized, ErrCode: me.ErrCode, Message: "login failed: " + me.Message}
		}
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("matrix: login returned no access token")
	}
	return &out, nil
}
