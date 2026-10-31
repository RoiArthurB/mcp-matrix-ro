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
	"time"
)

// MaxConcurrent bounds in-flight requests per Client (SPEC §8).
const MaxConcurrent = 4

// Client is the only way mxread talks to the homeserver.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
	sem   chan struct{}
	log   *slog.Logger
}

// Options configures a Client.
type Options struct {
	Homeserver string
	Token      string
	Timeout    time.Duration
	Logger     *slog.Logger
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
	if o.Token == "" {
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
	c := &Client{base: u, token: o.Token, sem: make(chan struct{}, MaxConcurrent), log: o.Logger}
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

// do performs one request. rawPath is already escaped.
func (c *Client) do(ctx context.Context, method, rawPath string, q url.Values, body any, out any) error {
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
	req.Header.Set("Authorization", "Bearer "+c.token)
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
