package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/testutil/fakehs"
)

const testHTTPToken = "test-http-token-0123456789abcdef"

func TestListenAddr(t *testing.T) {
	for in, want := range map[string]string{
		"8080":           "127.0.0.1:8080",
		":8080":          "127.0.0.1:8080",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"0.0.0.0:8080":   "0.0.0.0:8080",
		"[::1]:8080":     "[::1]:8080",
	} {
		got, err := listenAddr(in)
		if err != nil || got != want {
			t.Errorf("listenAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := listenAddr("localhost:"); err == nil {
		t.Error("missing port should fail")
	}
	if !isLoopback("127.0.0.1:1") || !isLoopback("[::1]:1") || !isLoopback("localhost:1") || isLoopback("0.0.0.0:1") {
		t.Error("isLoopback is wrong")
	}
}

func TestHTTPToken(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := httpToken(env(nil)); err == nil {
		t.Error("missing token should fail")
	}
	if _, err := httpToken(env(map[string]string{"MCP_MATRIX_RO_HTTP_TOKEN": "short"})); err == nil {
		t.Error("short token should fail")
	}
	if tok, err := httpToken(env(map[string]string{"MCP_MATRIX_RO_HTTP_TOKEN": " " + testHTTPToken + "\n"})); err != nil || tok != testHTTPToken {
		t.Errorf("got %q, %v", tok, err)
	}
}

func TestRequireBearer(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := requireBearer(testHTTPToken, log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		}
	}))
	for auth, want := range map[string]int{
		"Bearer " + testHTTPToken:     200,
		"bearer " + testHTTPToken:     200,
		"BEARER  " + testHTTPToken:    200,
		"Bearer":                      401,
		"Bearer " + testHTTPToken[1:]: 401,
		"Basic " + testHTTPToken:      401,
		testHTTPToken:                 401,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader("{}"))
		req.Header.Set("Authorization", auth)
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("auth %q: status %d, want %d", auth, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(strings.Repeat("x", maxRequestBody+1)))
	req.Header.Set("Authorization", "Bearer "+testHTTPToken)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status %d", rec.Code)
	}
}

func TestServeHTTPRequiresToken(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	_, errOut, code := runCmd(t, env(t, hs), "serve", "--http", "127.0.0.1:0")
	if code != exitUsage || !strings.Contains(errOut, "MCP_MATRIX_RO_HTTP_TOKEN") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

type bearer struct {
	tok  string
	base http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return b.base.RoundTrip(r)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// HTTP smoke test: auth is enforced, then several clients call tools concurrently
// against one long-lived process.
func TestServeHTTPSmoke(t *testing.T) {
	hs := fakehs.New()
	defer hs.Close()
	addr := freeAddr(t)
	cmd := exec.Command(binary, "serve", "--http", addr)
	cmd.Env = env(t, hs, "MCP_MATRIX_RO_LOG=debug", "MCP_MATRIX_RO_HTTP_TOKEN="+testHTTPToken)
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	base := "http://" + addr

	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up: %v\n%s", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	for _, auth := range []string{"", "Bearer wrong-token-0123456789", "Basic " + testHTTPToken} {
		req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hc := &http.Client{Transport: bearer{testHTTPToken, http.DefaultTransport}}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "http-smoke"}, nil).Connect(ctx,
				&mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc, DisableStandaloneSSE: true}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer cs.Close()
			tl, err := cs.ListTools(ctx, nil)
			if err != nil || len(tl.Tools) != 5 {
				t.Errorf("tools/list: %v", err)
				return
			}
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "recent", Arguments: map[string]any{"since": "2026-10-01"}})
			if err != nil {
				t.Error(err)
				return
			}
			if text := res.Content[0].(*mcp.TextContent).Text; res.IsError || !strings.Contains(text, "Mẹ (WhatsApp DM)") {
				t.Errorf("bad recent result: %s", text)
			}
			// chats goes through the shared room index.
			res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "chats", Arguments: map[string]any{}})
			if err != nil {
				t.Error(err)
				return
			}
			if text := res.Content[0].(*mcp.TextContent).Text; res.IsError || !strings.Contains(text, "Mẹ") {
				t.Errorf("bad chats result: %s", text)
			}
		}()
	}
	wg.Wait()

	logs := stderr.String()
	if strings.Contains(logs, fakehs.Token) || strings.Contains(logs, testHTTPToken) {
		t.Error("token leaked into logs")
	}
	if strings.Contains(logs, "DATA RACE") {
		t.Errorf("data race in server:\n%s", logs)
	}
	noViolations(t, hs)
}

// lockedBuffer is a bytes.Buffer safe to read while a child process writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
