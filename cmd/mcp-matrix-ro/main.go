// Command mcp-matrix-ro gives an AI agent read-only access to one Matrix account:
// an MCP server over stdio (serve), a digest CLI for cron (digest), and a
// setup checker (check).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/app"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/config"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/model"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/render"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/session"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/tools"
)

var version = "dev"

// Exit codes.
const (
	exitOK      = 0
	exitFail    = 1
	exitUsage   = 2
	exitAuth    = 3
	exitNetwork = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `mcp-matrix-ro — read-only Matrix access for AI agents

Usage:
  mcp-matrix-ro serve   [flags]   MCP server over stdio
  mcp-matrix-ro digest  [flags]   print recent messages (Markdown or JSON)
  mcp-matrix-ro check   [flags]   verify login, server features and chat counts
  mcp-matrix-ro version

Configuration comes from environment variables (see README); flags override.
Run "mcp-matrix-ro <command> -h" for flags.
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "mcp-matrix-ro", version)
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "serve", "digest", "check":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}

	raw := config.FromEnv(os.Getenv)
	fs := flag.NewFlagSet("mcp-matrix-ro "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	raw.RegisterFlags(fs)
	var d digestFlags
	if cmd == "digest" {
		d.register(fs)
	}
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	cfg, err := raw.Resolve()
	if err != nil {
		fmt.Fprintln(stderr, "mcp-matrix-ro: config:", err)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	mc, err := session.NewClient(cfg, log, nil)
	if err != nil {
		fmt.Fprintln(stderr, "mcp-matrix-ro: config:", err)
		return exitUsage
	}
	a := app.New(cfg, mc, log, nil)
	defer a.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, a, log, stderr)
	case "digest":
		return digest(ctx, a, d, stdout, stderr)
	default:
		return check(ctx, a, cfg, stdout, stderr)
	}
}

func serve(ctx context.Context, a *app.App, log *slog.Logger, stderr io.Writer) int {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-matrix-ro", Version: version}, nil)
	tools.Register(s, a)
	log.Info("mcp-matrix-ro serving MCP on stdio")
	if err := s.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		fmt.Fprintln(stderr, "mcp-matrix-ro: serve:", err)
		return exitFail
	}
	return exitOK
}

// failure maps an error to a clear stderr message and an exit code.
func failure(stderr io.Writer, what string, err error) int {
	var nerr net.Error
	var uerr *app.UserError
	switch {
	case matrix.IsAuth(err):
		fmt.Fprintf(stderr, "mcp-matrix-ro: %s: authentication failed: the access token or password was rejected (%v)\n", what, err)
		return exitAuth
	case errors.As(err, &uerr):
		fmt.Fprintf(stderr, "mcp-matrix-ro: %s: %v\n", what, err)
		return exitUsage
	case app.IsUserError(err):
		fmt.Fprintf(stderr, "mcp-matrix-ro: %s: %v\n", what, err)
		return exitUsage
	case errors.As(err, &nerr), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(stderr, "mcp-matrix-ro: %s: cannot reach the homeserver: %v\n", what, err)
		return exitNetwork
	default:
		var me *matrix.Error
		if errors.As(err, &me) {
			fmt.Fprintf(stderr, "mcp-matrix-ro: %s: homeserver error: %v\n", what, err)
			return exitNetwork
		}
		fmt.Fprintf(stderr, "mcp-matrix-ro: %s: %v\n", what, err)
		return exitFail
	}
}

// ---- digest ----

type digestFlags struct {
	since, network, format, cursorFile, chat string
	perChat                                  int
}

func (d *digestFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&d.since, "since", "24h", "ISO time or relative (24h, 3d); ignored when --cursor-file holds a timestamp")
	fs.StringVar(&d.network, "network", "", "only this network (whatsapp, matrix, ...)")
	fs.StringVar(&d.chat, "chat", "", "only this chat (name or room ID)")
	fs.StringVar(&d.format, "format", "md", "output format: md|json")
	fs.IntVar(&d.perChat, "per-chat", 20, "max messages per chat")
	fs.StringVar(&d.cursorFile, "cursor-file", "", "file holding the last digest time; read and updated (local only)")
}

func digest(ctx context.Context, a *app.App, d digestFlags, stdout, stderr io.Writer) int {
	if d.format != "md" && d.format != "json" {
		fmt.Fprintln(stderr, "mcp-matrix-ro: digest: --format must be md or json")
		return exitUsage
	}
	start := a.Now()
	p := app.RecentParams{Since: d.since, Limit: -1, PerChat: d.perChat, Network: d.network, Chat: d.chat}
	if d.cursorFile != "" {
		ts, err := readCursorFile(d.cursorFile)
		if err != nil {
			fmt.Fprintln(stderr, "mcp-matrix-ro: digest:", err)
			return exitUsage
		}
		p.SinceMS = ts
	}
	r, err := a.Recent(ctx, p)
	if err != nil {
		return failure(stderr, "digest", err)
	}
	switch d.format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(stderr, "mcp-matrix-ro: digest:", err)
			return exitFail
		}
	default:
		fmt.Fprint(stdout, render.Recent(r, a.Clock()))
	}
	if d.cursorFile != "" {
		if err := writeCursorFile(d.cursorFile, start); err != nil {
			fmt.Fprintln(stderr, "mcp-matrix-ro: digest: updating cursor file:", err)
			return exitFail
		}
	}
	return exitOK
}

// readCursorFile returns the stored timestamp (ms), or 0 if the file is absent.
func readCursorFile(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli(), nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return ms, nil
	}
	return 0, fmt.Errorf("cursor file %s: unreadable timestamp %q", path, s)
}

func writeCursorFile(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(t.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- check ----

func check(ctx context.Context, a *app.App, cfg *config.Config, stdout, stderr io.Writer) int {
	r, err := a.Check(ctx)
	if err != nil {
		return failure(stderr, "check", err)
	}
	fmt.Fprintf(stdout, "Homeserver:   %s\n", cfg.Homeserver)
	fmt.Fprintf(stdout, "Logged in as: %s (device %s)\n", r.UserID, r.DeviceID)
	me := cfg.Me
	if len(me) == 0 {
		me = []string{r.UserID}
	}
	fmt.Fprintf(stdout, "Shown as me:  %s\n", strings.Join(me, ", "))
	fmt.Fprintf(stdout, "Spec versions: %s\n", strings.Join(lastN(r.Versions, 4), ", "))
	fmt.Fprintf(stdout, "Search:       %s\n", r.Search)
	fmt.Fprintf(stdout, "Joined rooms: %d (%d visible chats: %d DMs, %d groups; %d hidden)\n", r.Joined, r.Visible, r.DMs, r.Groups, r.Hidden)
	nets := make([]string, 0, len(r.ByNetwork))
	for n := range r.ByNetwork {
		nets = append(nets, n)
	}
	sort.Strings(nets)
	for _, n := range nets {
		fmt.Fprintf(stdout, "  %-10s %d\n", model.NetworkLabel(n), r.ByNetwork[n])
	}
	fmt.Fprintf(stdout, "Timezone:     %s\n", cfg.TZ)
	fmt.Fprintf(stdout, "State dir:    %s\n", cfg.StateDir)
	fmt.Fprintf(stdout, "Took:         %s\n", r.Elapsed.Round(time.Millisecond))
	return exitOK
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
