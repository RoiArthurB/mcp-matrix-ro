// Package fakehs is a fake Matrix homeserver for tests. It replays fixtures
// from testdata/hs and independently checks every request it receives
// against the read-only allowlist.
package fakehs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/RoiArthurB/mxread/internal/matrix"
)

// Server is a running fake homeserver.
type Server struct {
	*httptest.Server
	SearchUnsupported bool

	mu       sync.Mutex
	requests []string
	bad      []string

	sync     json.RawMessage
	messages map[string]map[string]json.RawMessage
	state    map[string]json.RawMessage
	events   map[string]json.RawMessage
	search   json.RawMessage
	profiles map[string]json.RawMessage
}

// Dir returns the fixture directory (testdata/hs at the module root).
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "hs")
}

// Token is the access token the fake server accepts.
const Token = "test-token"

// New starts a fake homeserver serving the fixtures.
func New() *Server {
	s := &Server{}
	load := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join(Dir(), name+".json"))
		if err != nil {
			panic(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			panic(err)
		}
	}
	load("sync", &s.sync)
	load("messages", &s.messages)
	load("state", &s.state)
	load("events", &s.events)
	load("search", &s.search)
	load("profiles", &s.profiles)
	s.Server = httptest.NewServer(s)
	return s
}

// Requests returns "METHOD path" for every request received.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Violations returns requests that were not on the allowlist.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bad...)
}

// Count returns how many requests matched a path substring.
func (s *Server) Count(sub string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.Contains(r, sub) {
			n++
		}
	}
	return n
}

func errJSON(w http.ResponseWriter, code int, errcode string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"errcode": errcode, "error": errcode})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.EscapedPath())
	if err := matrix.CheckAllowed(r.Method, r.URL); err != nil {
		s.bad = append(s.bad, err.Error())
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+Token && r.URL.Path != "/_matrix/client/versions" {
		errJSON(w, 401, "M_UNKNOWN_TOKEN")
		return
	}
	if err := matrix.CheckAllowed(r.Method, r.URL); err != nil {
		errJSON(w, 403, "M_FORBIDDEN")
		return
	}
	p := r.URL.Path
	write := func(b []byte) { _, _ = w.Write(b) }
	switch {
	case p == "/_matrix/client/versions":
		write([]byte(`{"versions":["v1.9","v1.10","v1.11"]}`))
	case p == "/_matrix/client/v3/account/whoami":
		write([]byte(`{"user_id":"@archive:dom","device_id":"MXREAD"}`))
	case p == "/_matrix/client/v3/joined_rooms":
		var sr matrix.SyncResponse
		_ = json.Unmarshal(s.sync, &sr)
		ids := []string{}
		for id := range sr.Rooms.Join {
			ids = append(ids, id)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"joined_rooms": ids})
	case p == "/_matrix/client/v3/sync":
		var f any
		if json.Unmarshal([]byte(r.URL.Query().Get("filter")), &f) != nil {
			errJSON(w, 400, "M_BAD_JSON")
			return
		}
		write(s.sync)
	case p == "/_matrix/client/v3/search":
		if s.SearchUnsupported {
			errJSON(w, 404, "M_UNRECOGNIZED")
			return
		}
		write(s.search)
	case strings.HasPrefix(p, "/_matrix/client/v3/profile/"):
		if b, ok := s.profiles[strings.TrimPrefix(p, "/_matrix/client/v3/profile/")]; ok {
			write(b)
			return
		}
		errJSON(w, 404, "M_NOT_FOUND")
	case strings.HasPrefix(p, "/_matrix/client/v3/rooms/"):
		s.room(w, r)
	default:
		errJSON(w, 404, "M_UNRECOGNIZED")
	}
}

func (s *Server) room(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/_matrix/client/v3/rooms/")
	parts := strings.Split(rest, "/")
	room, _ := url.PathUnescape(parts[0])
	write := func(b []byte) { _, _ = w.Write(b) }
	switch {
	case len(parts) == 2 && parts[1] == "messages":
		pages, ok := s.messages[room]
		if !ok {
			write([]byte(`{"chunk":[]}`))
			return
		}
		if b, ok := pages[r.URL.Query().Get("from")]; ok {
			write(b)
			return
		}
		write([]byte(`{"chunk":[]}`))
	case len(parts) == 2 && parts[1] == "state":
		if b, ok := s.state[room]; ok {
			write(b)
			return
		}
		errJSON(w, 403, "M_FORBIDDEN")
	case len(parts) == 3 && parts[1] == "event":
		id, _ := url.PathUnescape(parts[2])
		if b, ok := s.events[id]; ok {
			write(b)
			return
		}
		errJSON(w, 404, "M_NOT_FOUND")
	default:
		errJSON(w, 404, "M_NOT_FOUND")
	}
}
