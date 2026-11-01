package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// minHTTPTokenLen is the shortest bearer token accepted for the HTTP transport.
const minHTTPTokenLen = 16

// mcpPath is where the Streamable HTTP endpoint is served.
const mcpPath = "/mcp"

// httpFlags configure the optional Streamable HTTP transport of serve.
type httpFlags struct {
	addr string
}

func (h *httpFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&h.addr, "http", os.Getenv("MCP_MATRIX_RO_HTTP"),
		"serve MCP over Streamable HTTP on this address instead of stdio, e.g. 127.0.0.1:8080 or :8080 (MCP_MATRIX_RO_HTTP); needs MCP_MATRIX_RO_HTTP_TOKEN")
}

// listenAddr normalises addr: a bare port or ":port" binds to 127.0.0.1.
// Any other host must be given explicitly (e.g. 0.0.0.0:8080 inside a container).
func listenAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("--http %q: %w", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("--http %q: missing port", addr)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// isLoopback reports whether addr's host is a loopback address or localhost.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// httpToken reads the bearer token from MCP_MATRIX_RO_HTTP_TOKEN_FILE or
// MCP_MATRIX_RO_HTTP_TOKEN. It is never a flag, so it does not show in `ps`.
func httpToken(getenv func(string) string) (string, error) {
	tok := getenv("MCP_MATRIX_RO_HTTP_TOKEN")
	if f := getenv("MCP_MATRIX_RO_HTTP_TOKEN_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("reading MCP_MATRIX_RO_HTTP_TOKEN_FILE: %w", err)
		}
		tok = string(b)
	}
	tok = strings.TrimSpace(tok)
	switch {
	case tok == "":
		return "", errors.New("HTTP mode needs a bearer token: set MCP_MATRIX_RO_HTTP_TOKEN or MCP_MATRIX_RO_HTTP_TOKEN_FILE (e.g. `openssl rand -hex 32`)")
	case len(tok) < minHTTPTokenLen:
		return "", fmt.Errorf("MCP_MATRIX_RO_HTTP_TOKEN is too short: use at least %d characters (e.g. `openssl rand -hex 32`)", minHTTPTokenLen)
	case strings.ContainsAny(tok, " \t\r\n"):
		return "", errors.New("MCP_MATRIX_RO_HTTP_TOKEN must not contain whitespace")
	}
	return tok, nil
}

// requireBearer rejects any request without `Authorization: Bearer <token>`.
// Hashing both sides first makes the comparison constant-time regardless of length.
func requireBearer(token string, log *slog.Logger, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			got, ok = strings.CutPrefix(r.Header.Get("Authorization"), "bearer ")
		}
		h := sha256.Sum256([]byte(strings.TrimSpace(got)))
		if !ok || subtle.ConstantTimeCompare(h[:], want[:]) != 1 {
			log.Warn("rejected unauthenticated HTTP request", "remote", r.RemoteAddr, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp-matrix-ro"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// httpHandler builds the full HTTP handler: POST/GET/DELETE /mcp behind the
// bearer check, and an unauthenticated /healthz that reveals nothing.
func httpHandler(s *mcp.Server, token string, log *slog.Logger) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{
		// Stateless: no Mcp-Session-Id to lose when the container restarts,
		// and nothing to clean up. The server never calls back into the client.
		Stateless:    true,
		JSONResponse: true,
		Logger:       log,
	})
	// Browsers must not be able to drive the server from another origin.
	cop := http.NewCrossOriginProtection()
	mux := http.NewServeMux()
	mux.Handle(mcpPath, cop.Handler(requireBearer(token, log, h)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// serveHTTP runs the MCP server over Streamable HTTP until ctx is cancelled.
func serveHTTP(ctx context.Context, s *mcp.Server, addr, token string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if !isLoopback(addr) {
		log.Warn("HTTP listener is not bound to loopback; keep it unreachable from other machines (e.g. publish the port on 127.0.0.1 only)", "addr", addr)
	}
	srv := &http.Server{
		Handler:           httpHandler(s, token, log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: search may legitimately take ~30 s.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("mcp-matrix-ro serving MCP over HTTP", "url", "http://"+ln.Addr().String()+mcpPath)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return nil
	}
}
