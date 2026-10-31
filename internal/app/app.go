// Package app implements mcp-matrix-ro's operations (recent, chats, read, search,
// chat_info, check) on top of the read-only Matrix client. The MCP tools and
// the CLI are thin wrappers around it.
package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/config"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/model"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/render"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/resolve"
)

// App wires config, client, resolver and converter.
type App struct {
	Cfg  *config.Config
	MC   *matrix.Client
	Res  *resolve.Resolver
	Conv *render.Converter
	Log  *slog.Logger
	Now  func() time.Time

	parents sync.Map // event ID -> model.Message (reply parents)
}

// New builds an App. now may be nil (time.Now).
func New(cfg *config.Config, mc *matrix.Client, log *slog.Logger, now func() time.Time) *App {
	if now == nil {
		now = time.Now
	}
	cache := resolve.OpenCache(cfg.StateDir, now)
	res := resolve.New(cfg, mc, cache, log, now)
	return &App{
		Cfg: cfg, MC: mc, Res: res, Log: log, Now: now,
		Conv: &render.Converter{MaxBody: cfg.MaxBody, Name: res.SenderName},
	}
}

// Clock returns a render clock at the current time.
func (a *App) Clock() render.Clock { return render.Clock{Loc: a.Cfg.TZ, Now: a.Now()} }

// Close persists caches.
func (a *App) Close() {
	if err := a.Res.Cache().Save(); err != nil {
		a.Log.Warn("saving cache", "err", err)
	}
}

// UserError is an error meant to be shown to the model as-is.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return &UserError{fmt.Sprintf(format, args...)} }

// messagesFilter is the RoomEventFilter used with /messages.
func messagesFilter() map[string]any {
	return map[string]any{"types": resolve.MessageTypes, "lazy_load_members": true}
}

func (a *App) parseWhen(s string) (time.Time, error) {
	t, err := model.ParseWhen(s, a.Now(), a.Cfg.TZ)
	if err != nil {
		return t, &UserError{err.Error()}
	}
	return t, nil
}

func clamp(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	if v < lo {
		v = lo
	}
	if hi > 0 && v > hi {
		v = hi
	}
	return v
}

// convert renders events with names prefetched and reply parents resolved
// (up to parentBudget /event fetches).
func (a *App) convert(ctx context.Context, roomID string, events []matrix.Event, parentBudget int) []model.Message {
	pairs := make([][2]string, 0, len(events))
	for _, e := range events {
		pairs = append(pairs, [2]string{roomID, e.Sender})
	}
	a.Res.FetchNames(ctx, pairs)
	msgs := a.Conv.Convert(roomID, events)
	a.resolveReplies(ctx, roomID, msgs, parentBudget)
	return msgs
}

func (a *App) resolveReplies(ctx context.Context, roomID string, msgs []model.Message, budget int) {
	var need []string
	var fbSenders [][2]string
	for i := range msgs {
		r := msgs[i].Reply
		if r == nil {
			continue
		}
		if p, ok := a.parents.Load(r.EventID); ok {
			a.Conv.SetParent(roomID, &msgs[i], p.(model.Message))
			continue
		}
		if r.SenderID != "" {
			fbSenders = append(fbSenders, [2]string{roomID, r.SenderID})
			continue
		}
		if len(need) < budget {
			need = append(need, r.EventID)
		}
	}
	if len(fbSenders) > 0 {
		a.Res.FetchNames(ctx, fbSenders)
		for i := range msgs {
			if r := msgs[i].Reply; r != nil && r.SenderID != "" {
				r.Sender = a.Res.SenderName(roomID, r.SenderID)
			}
		}
	}
	if len(need) == 0 {
		return
	}
	evs := make([]*matrix.Event, len(need))
	var wg sync.WaitGroup
	for i, id := range need {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			e, err := a.MC.Event(ctx, roomID, id)
			if err != nil {
				a.Log.Debug("reply parent fetch failed", "err", err)
				return
			}
			evs[i] = e
		}(i, id)
	}
	wg.Wait()
	var pairs [][2]string
	for _, e := range evs {
		if e != nil {
			pairs = append(pairs, [2]string{roomID, e.Sender})
		}
	}
	a.Res.FetchNames(ctx, pairs)
	for _, e := range evs {
		if e == nil {
			continue
		}
		a.parents.Store(e.EventID, a.Conv.ConvertOne(roomID, *e))
	}
	for i := range msgs {
		if r := msgs[i].Reply; r != nil && r.Sender == "" {
			if p, ok := a.parents.Load(r.EventID); ok {
				a.Conv.SetParent(roomID, &msgs[i], p.(model.Message))
			}
		}
	}
}

// ---- chats ----

// ChatsParams are the chats tool inputs.
type ChatsParams struct {
	ActiveWithin string
	Network      string
	Query        string
	Limit        int
}

// Chats lists visible chats, most recently active first.
func (a *App) Chats(ctx context.Context, p ChatsParams) (*model.ChatsResult, error) {
	rooms, err := a.Res.Index(ctx)
	if err != nil {
		return nil, err
	}
	var cutoff int64
	if p.ActiveWithin != "" {
		t, err := a.parseWhen(p.ActiveWithin)
		if err != nil {
			return nil, err
		}
		cutoff = t.UnixMilli()
	}
	terms := resolve.Terms(p.Query)
	net := strings.ToLower(strings.TrimSpace(p.Network))
	limit := clamp(p.Limit, 30, 1, 300)
	res := &model.ChatsResult{}
	for _, r := range rooms {
		if r.Hidden || (net != "" && r.Network != net) || r.LastTS < cutoff {
			continue
		}
		if len(terms) > 0 && !resolve.MatchAll(resolve.Fold(r.Name), terms) {
			continue
		}
		res.Matching++
		if len(res.Chats) < limit {
			res.Chats = append(res.Chats, r.Chat)
		}
	}
	return res, nil
}

// ---- read ----

// ReadParams are the read tool inputs.
type ReadParams struct {
	Chat   string
	Limit  int
	Before string
	After  string
	Cursor string
}

type readCursor struct {
	Room   string `json:"r"`
	From   string `json:"f,omitempty"`
	Skip   int    `json:"s,omitempty"`
	Page   int    `json:"l"`
	After  int64  `json:"a,omitempty"`
	Before int64  `json:"b,omitempty"`
}

func encodeCursor(c readCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (readCursor, error) {
	var c readCursor
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.Room == "" || c.Page <= 0 {
		return c, userErr("invalid cursor; call read without cursor")
	}
	return c, nil
}

const maxReadPages = 20

// Read returns up to limit messages of one chat, newest page first,
// rendered oldest to newest, with a cursor for older messages.
func (a *App) Read(ctx context.Context, p ReadParams) (*model.ReadResult, error) {
	limit := clamp(p.Limit, 30, 1, 100)
	var cur readCursor
	var room *resolve.RoomEntry
	var err error
	if p.Cursor != "" {
		if cur, err = decodeCursor(p.Cursor); err != nil {
			return nil, err
		}
		if room, err = a.Res.Find(ctx, cur.Room); err != nil {
			return nil, err
		}
	} else {
		if strings.TrimSpace(p.Chat) == "" {
			return nil, userErr("chat is required")
		}
		if room, err = a.Res.Find(ctx, p.Chat); err != nil {
			return nil, err
		}
		cur = readCursor{Room: room.ID, Page: min(100, limit+20)}
		if p.After != "" {
			t, err := a.parseWhen(p.After)
			if err != nil {
				return nil, err
			}
			cur.After = t.UnixMilli()
		}
		if p.Before != "" {
			t, err := a.parseWhen(p.Before)
			if err != nil {
				return nil, err
			}
			cur.Before = t.UnixMilli()
		}
	}

	var window, edits []matrix.Event
	from, skip := cur.From, cur.Skip
	next := ""
	count := 0
pages:
	for page := 0; page < maxReadPages; page++ {
		resp, err := a.MC.Messages(ctx, room.ID, matrix.MessagesOpts{From: from, Dir: "b", Limit: cur.Page, Filter: messagesFilter()})
		if err != nil {
			return nil, err
		}
		a.Res.LearnMembers(room.ID, resp.State)
		for i := skip; i < len(resp.Chunk); i++ {
			e := resp.Chunk[i]
			if render.IsEdit(&e) {
				edits = append(edits, e)
				continue
			}
			if cur.Before != 0 && e.OriginTS >= cur.Before {
				continue
			}
			if cur.After != 0 && e.OriginTS <= cur.After {
				break pages
			}
			window = append(window, e)
			count++
			if count == limit {
				if i+1 < len(resp.Chunk) {
					c := cur
					c.From, c.Skip = from, i+1
					next = encodeCursor(c)
				} else if resp.End != "" {
					c := cur
					c.From, c.Skip = resp.End, 0
					next = encodeCursor(c)
				}
				break pages
			}
		}
		skip = 0
		if resp.End == "" || len(resp.Chunk) == 0 {
			break
		}
		from = resp.End
		if page == maxReadPages-1 {
			c := cur
			c.From, c.Skip = from, 0
			next = encodeCursor(c)
		}
	}
	msgs := a.convert(ctx, room.ID, append(window, edits...), 10)
	// Edits only fold into their targets; keep only window messages.
	keep := make(map[string]bool, len(window))
	for _, e := range window {
		keep[e.EventID] = true
	}
	out := msgs[:0]
	for _, m := range msgs {
		if keep[m.ID] {
			out = append(out, m)
		}
	}
	return &model.ReadResult{Chat: room.Chat, Messages: out, Cursor: next, After: cur.After, Before: cur.Before}, nil
}

// ---- chat_info ----

const maxMembersShown = 50

// ChatInfo describes one chat and its members.
func (a *App) ChatInfo(ctx context.Context, chat string) (*model.ChatInfoResult, error) {
	room, err := a.Res.Find(ctx, chat)
	if err != nil {
		return nil, err
	}
	state, err := a.MC.RoomState(ctx, room.ID)
	if err != nil {
		return nil, err
	}
	a.Res.LearnMembers(room.ID, state)
	res := &model.ChatInfoResult{Chat: room.Chat}
	var members []model.Member
	meShown := false
	for _, e := range state {
		switch e.Type {
		case "m.room.topic":
			var c struct{ Topic string }
			_ = json.Unmarshal(e.Content, &c)
			res.Topic = c.Topic
		case "m.room.canonical_alias":
			var c struct {
				Alias string `json:"alias"`
			}
			_ = json.Unmarshal(e.Content, &c)
			res.Alias = c.Alias
		case "m.room.create":
			res.Created = e.OriginTS
			res.CreatorID = e.Sender
		case "m.room.member":
			var c struct {
				Membership  string `json:"membership"`
				DisplayName string `json:"displayname"`
			}
			_ = json.Unmarshal(e.Content, &c)
			if e.StateKey == nil || c.Membership != "join" {
				continue
			}
			id := *e.StateKey
			res.JoinedTotal++
			if a.Res.IsMe(id) {
				if meShown {
					continue // the owner's accounts collapse into one "me"
				}
				meShown = true
			}
			members = append(members, model.Member{
				ID: id, Name: a.Res.SenderName(room.ID, id), Me: a.Res.IsMe(id),
				Bot: a.Cfg.ExcludeBots != nil && a.Cfg.ExcludeBots.MatchString(id),
			})
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		mi, mj := members[i], members[j]
		if mi.Me != mj.Me {
			return mi.Me
		}
		if mi.Bot != mj.Bot {
			return mj.Bot
		}
		return resolve.Fold(mi.Name) < resolve.Fold(mj.Name)
	})
	res.Members = res.JoinedTotal
	if len(members) > maxMembersShown {
		res.MoreMembers = len(members) - maxMembersShown
		members = members[:maxMembersShown]
	}
	res.MemberList = members
	return res, nil
}

// ---- check ----

// CheckResult is the output of `mcp-matrix-ro check`.
type CheckResult struct {
	UserID    string
	DeviceID  string
	Versions  []string
	Search    string
	Joined    int
	Visible   int
	Hidden    int
	DMs       int
	Groups    int
	ByNetwork map[string]int
	Elapsed   time.Duration
}

// Check verifies connectivity and capabilities.
func (a *App) Check(ctx context.Context) (*CheckResult, error) {
	start := time.Now()
	res := &CheckResult{ByNetwork: map[string]int{}}
	w, err := a.MC.WhoAmI(ctx)
	if err != nil {
		return nil, fmt.Errorf("whoami: %w", err)
	}
	res.UserID, res.DeviceID = w.UserID, w.DeviceID
	v, err := a.MC.Versions(ctx)
	if err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	res.Versions = v.Versions
	switch _, err := a.MC.Search(ctx, matrix.SearchRequest{Term: "mcp-matrix-ro", Limit: 1}); {
	case err == nil:
		res.Search = "supported (server mode)"
	case matrix.IsUnsupported(err):
		res.Search = "not supported (search will use local scan)"
	default:
		res.Search = "error: " + err.Error()
	}
	joined, err := a.MC.JoinedRooms(ctx)
	if err != nil {
		return nil, fmt.Errorf("joined_rooms: %w", err)
	}
	res.Joined = len(joined)
	data, err := a.Res.Sync(ctx, 1)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	for _, d := range data {
		if d.Hidden {
			res.Hidden++
			continue
		}
		res.Visible++
		res.ByNetwork[d.Network]++
		if d.DM {
			res.DMs++
		} else {
			res.Groups++
		}
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// IsUserError reports whether err should be shown to the model verbatim.
func IsUserError(err error) bool {
	var ue *UserError
	var ae *resolve.AmbiguousError
	var nf *resolve.NotFoundError
	return errors.As(err, &ue) || errors.As(err, &ae) || errors.As(err, &nf)
}
