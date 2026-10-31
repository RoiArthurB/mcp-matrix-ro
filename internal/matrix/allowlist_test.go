package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// failTransport fails the test if any request reaches the network layer.
type failTransport struct {
	t     *testing.T
	calls int
}

func (f *failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	f.t.Errorf("request reached the network: %s %s", r.Method, r.URL)
	return nil, errors.New("network disabled")
}

const hs = "https://matrix.example.org"

var forbidden = []struct{ method, path string }{
	{"PUT", "/_matrix/client/v3/rooms/!r:ex/send/m.room.message/txn1"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/send/m.room.message"},
	{"PUT", "/_matrix/client/v3/rooms/!r:ex/state/m.room.name/"},
	{"PUT", "/_matrix/client/v3/rooms/!r:ex/state/m.room.name"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/state"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/receipt/m.read/$e"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/read_markers"},
	{"PUT", "/_matrix/client/v3/rooms/!r:ex/typing/@a:ex"},
	{"POST", "/_matrix/client/v3/join/!r:ex"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/join"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/invite"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/leave"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/forget"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/kick"},
	{"POST", "/_matrix/client/v3/rooms/!r:ex/ban"},
	{"PUT", "/_matrix/client/v3/rooms/!r:ex/redact/$e/txn"},
	{"POST", "/_matrix/client/v3/user/@a:ex/filter"},
	{"GET", "/_matrix/client/v3/user/@a:ex/filter/1"},
	{"PUT", "/_matrix/client/v3/user/@a:ex/account_data/m.direct"},
	{"GET", "/_matrix/client/v3/user/@a:ex/account_data/m.direct"},
	{"PUT", "/_matrix/client/v3/user/@a:ex/rooms/!r:ex/account_data/x"},
	{"PUT", "/_matrix/client/v3/presence/@a:ex/status"},
	{"GET", "/_matrix/client/v3/presence/@a:ex/status"},
	{"PUT", "/_matrix/client/v3/sendToDevice/m.x/txn"},
	{"POST", "/_matrix/client/v3/keys/upload"},
	{"POST", "/_matrix/client/v3/keys/query"},
	{"POST", "/_matrix/client/v3/logout"},
	{"POST", "/_matrix/client/v3/logout/all"},
	{"POST", "/_matrix/client/v3/createRoom"},
	{"PUT", "/_matrix/client/v3/profile/@a:ex/displayname"},
	{"PUT", "/_matrix/client/v3/directory/room/%23a:ex"},
	{"DELETE", "/_matrix/client/v3/devices/DEV"},
	{"POST", "/_matrix/media/v3/upload"},
	{"GET", "/_matrix/media/v3/download/ex/abc"},
	{"GET", "/_matrix/client/v1/media/download/ex/abc"},
	// allowed paths with the wrong method
	{"POST", "/_matrix/client/v3/sync?set_presence=offline"},
	{"PUT", "/_matrix/client/v3/rooms/!r:ex/messages"},
	{"DELETE", "/_matrix/client/v3/rooms/!r:ex/event/$e"},
	{"GET", "/_matrix/client/v3/search"},
	{"HEAD", "/_matrix/client/versions"},
	// sync presence rule
	{"GET", "/_matrix/client/v3/sync"},
	{"GET", "/_matrix/client/v3/sync?set_presence=online"},
	{"GET", "/_matrix/client/v3/sync?set_presence=unavailable"},
	{"GET", "/_matrix/client/v3/sync?set_presence=offline&set_presence=online"},
	// old API versions / prefixes
	{"GET", "/_matrix/client/r0/joined_rooms"},
	{"GET", "/_matrix/client/v3/joined_rooms/"},
	{"GET", "/_synapse/admin/v1/users"},
	// traversal / smuggling
	{"GET", "/_matrix/client/v3/rooms/!r:ex/../../join/!r:ex"},
	{"GET", "/_matrix/client/v3/rooms/%2e%2e/state"},
	{"GET", "/_matrix/client/v3/rooms/!r:ex%2F..%2F..%2Fjoin/messages"},
	{"GET", "/_matrix/client/v3/rooms/!r:ex%5C..%5Cjoin/messages"},
	{"GET", "/_matrix/client/v3/rooms/!r:ex/messages/extra"},
	{"GET", "/_matrix/client/v3/rooms//state"},
	{"GET", "/_matrix/client/v3/profile/@a:ex/displayname"},
}

func TestForbiddenRequestsRefusedBeforeNetwork(t *testing.T) {
	ft := &failTransport{t: t}
	c, err := New(Options{Homeserver: hs, Token: "secret", Transport: ft})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range forbidden {
		t.Run(f.method+" "+f.path, func(t *testing.T) {
			p, q, _ := strings.Cut(f.path, "?")
			req, err := http.NewRequest(f.method, hs+p, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.URL.RawQuery = q
			_, err = c.http.Do(req)
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("want ErrForbidden, got %v", err)
			}
			// Also through the client's own request path.
			err = c.do(context.Background(), f.method, p, nil, nil, nil)
			if f.path == "/_matrix/client/v3/sync" || strings.HasPrefix(f.path, "/_matrix/client/v3/sync?") && f.method == "GET" {
				return // c.do does not add set_presence; covered above
			}
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("c.do: want ErrForbidden, got %v", err)
			}
		})
	}
	if ft.calls != 0 {
		t.Fatalf("%d forbidden requests reached the transport", ft.calls)
	}
}

func TestOtherHostRefused(t *testing.T) {
	ft := &failTransport{t: t}
	c, _ := New(Options{Homeserver: hs, Token: "secret", Transport: ft})
	for _, u := range []string{
		"https://evil.example.org/_matrix/client/versions",
		"http://matrix.example.org/_matrix/client/versions",
		"https://matrix.example.org:8448/_matrix/client/versions",
	} {
		req, _ := http.NewRequest("GET", u, nil)
		if _, err := c.http.Do(req); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: want ErrForbidden, got %v", u, err)
		}
	}
}

// recorder is a fake homeserver that records requests and independently
// verifies them against the allowlist.
type recorder struct {
	mu   sync.Mutex
	seen []string
	bad  []string
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	rc.seen = append(rc.seen, r.Method+" "+r.URL.EscapedPath())
	if err := CheckAllowed(r.Method, r.URL); err != nil {
		rc.bad = append(rc.bad, err.Error())
	}
	if r.Header.Get("Authorization") != "Bearer secret" {
		rc.bad = append(rc.bad, "missing auth on "+r.URL.Path)
	}
	rc.mu.Unlock()
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/state") && !strings.Contains(r.URL.Path, "/state/"):
		_, _ = w.Write([]byte(`[]`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func TestEveryClientCallIsAllowlisted(t *testing.T) {
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c, err := New(Options{Homeserver: srv.URL + "/", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	room, ev, user := "!room:ex.org", "$ev/with+odd=chars", "@alice:ex.org"
	calls := []func() error{
		func() error { _, e := c.Versions(ctx); return e },
		func() error { _, e := c.WhoAmI(ctx); return e },
		func() error { _, e := c.JoinedRooms(ctx); return e },
		func() error { _, e := c.Sync(ctx, map[string]any{"room": map[string]any{}}, ""); return e },
		func() error { _, e := c.RoomState(ctx, room); return e },
		func() error { var v any; return c.StateEvent(ctx, room, "m.room.name", "", &v) },
		func() error { var v any; return c.StateEvent(ctx, room, "m.room.member", user, &v) },
		func() error { _, e := c.Messages(ctx, room, MessagesOpts{Limit: 5, Filter: map[string]any{}}); return e },
		func() error { _, e := c.Members(ctx, room); return e },
		func() error { _, e := c.JoinedMembers(ctx, room); return e },
		func() error { _, e := c.Event(ctx, room, ev); return e },
		func() error { _, e := c.Context(ctx, room, ev, 1); return e },
		func() error { _, e := c.Profile(ctx, user); return e },
		func() error { _, e := c.Search(ctx, SearchRequest{Term: "x", Limit: 1}); return e },
	}
	for i, f := range calls {
		if err := f(); err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if len(rc.bad) > 0 {
		t.Fatalf("non-allowlisted requests reached the server:\n%s", strings.Join(rc.bad, "\n"))
	}
	if len(rc.seen) != len(calls) {
		t.Fatalf("want %d requests, got %d: %v", len(calls), len(rc.seen), rc.seen)
	}
	// Every allowlist rule is exercised at least once (keeps the list honest).
	for _, r := range allowlist {
		hit := false
		for _, s := range rc.seen {
			m, p, _ := strings.Cut(s, " ")
			if m == r.method && r.re.MatchString(p) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("allowlist rule %s %s is never used by the client", r.method, r.re)
		}
	}
}

func TestSyncAlwaysOffline(t *testing.T) {
	rc := &recorder{}
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		rc.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c, _ := New(Options{Homeserver: srv.URL, Token: "secret"})
	if _, err := c.Sync(context.Background(), map[string]any{}, "s1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "set_presence=offline") || !strings.Contains(gotQuery, "timeout=0") {
		t.Fatalf("bad sync query: %s", gotQuery)
	}
}

func TestErrorsDoNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"Invalid token"}`))
	}))
	defer srv.Close()
	c, _ := New(Options{Homeserver: srv.URL, Token: "supersecret"})
	_, err := c.WhoAmI(context.Background())
	if !IsAuth(err) {
		t.Fatalf("want auth error, got %v", err)
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatal("token leaked in error")
	}
}
