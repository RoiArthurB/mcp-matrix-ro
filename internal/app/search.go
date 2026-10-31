package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RoiArthurB/mxread/internal/matrix"
	"github.com/RoiArthurB/mxread/internal/model"
	"github.com/RoiArthurB/mxread/internal/render"
	"github.com/RoiArthurB/mxread/internal/resolve"
)

// SearchParams are the search tool inputs.
type SearchParams struct {
	Query string
	Chat  string
	Since string
	Limit int
}

// Fallback scan bounds (SPEC §5.4).
const (
	scanDefaultWindow = 30 * 24 * time.Hour
	scanMaxEvents     = 500 // per chat
	scanMaxChats      = 50  // most recently active chats
	scanPage          = 100
	scanDeadline      = 25 * time.Second
	serverMaxPages    = 3
)

// Search finds messages, using server-side /search when available and
// falling back to a bounded scan of recent history otherwise.
func (a *App) Search(ctx context.Context, p SearchParams) (*model.SearchResult, error) {
	q := strings.TrimSpace(p.Query)
	if q == "" {
		return nil, userErr("query is required")
	}
	limit := clamp(p.Limit, 15, 1, 50)
	var room *resolve.RoomEntry
	if strings.TrimSpace(p.Chat) != "" {
		var err error
		if room, err = a.Res.Find(ctx, p.Chat); err != nil {
			return nil, err
		}
	}
	var since int64
	if p.Since != "" {
		t, err := a.parseWhen(p.Since)
		if err != nil {
			return nil, err
		}
		since = t.UnixMilli()
	}
	// The index provides room names and hides bridge/control rooms.
	rooms, err := a.Res.Index(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*resolve.RoomEntry, len(rooms))
	for _, r := range rooms {
		byID[r.ID] = r
	}
	res, err := a.serverSearch(ctx, q, room, since, limit, byID)
	if err == nil {
		return res, nil
	}
	if !matrix.IsUnsupported(err) {
		return nil, err
	}
	a.Log.Info("server search unsupported; scanning", "err", err)
	return a.scanSearch(ctx, q, room, since, limit, rooms)
}

func (a *App) serverSearch(ctx context.Context, q string, room *resolve.RoomEntry, since int64, limit int, byID map[string]*resolve.RoomEntry) (*model.SearchResult, error) {
	req := matrix.SearchRequest{Term: q, Limit: limit, Before: 1}
	if room != nil {
		req.Rooms = []string{room.ID}
	}
	res := &model.SearchResult{Query: q, Mode: "server", Hits: []model.SearchHit{}}
	if room != nil {
		res.Scope = "in " + room.Name
	}
	for page := 0; page < serverMaxPages && len(res.Hits) < limit; page++ {
		resp, err := a.MC.Search(ctx, req)
		if err != nil {
			return nil, err
		}
		stop := false
		for _, r := range resp.SearchCategories.RoomEvents.Results {
			ev := r.Result
			if since != 0 && ev.OriginTS < since {
				stop = true // results are ordered by recency
				break
			}
			roomID := ev.RoomID
			info, ok := byID[roomID]
			if ok && info.Hidden && room == nil {
				continue
			}
			if render.IsEdit(&ev) {
				continue
			}
			evs := append(append(append([]matrix.Event{}, r.Context.EventsBefore...), ev), r.Context.EventsAfter...)
			hit := a.hitFrom(ctx, roomID, info, ev.EventID, evs)
			if hit != nil {
				res.Hits = append(res.Hits, *hit)
			}
			if len(res.Hits) >= limit {
				break
			}
		}
		next := resp.SearchCategories.RoomEvents.NextBatch
		if stop || next == "" {
			break
		}
		req.Next = next
	}
	return res, nil
}

// hitFrom converts a hit with its neighbours.
func (a *App) hitFrom(ctx context.Context, roomID string, info *resolve.RoomEntry, id string, evs []matrix.Event) *model.SearchHit {
	msgs := a.convert(ctx, roomID, evs, 2)
	for i, m := range msgs {
		if m.ID != id {
			continue
		}
		h := &model.SearchHit{ChatID: roomID, ChatName: roomID, Message: m}
		if info != nil {
			h.ChatName = info.Name
		}
		if i > 0 {
			b := msgs[i-1]
			h.Before = &b
		}
		if i+1 < len(msgs) {
			af := msgs[i+1]
			h.After = &af
		}
		return h
	}
	return nil
}

func (a *App) scanSearch(ctx context.Context, q string, room *resolve.RoomEntry, since int64, limit int, rooms []*resolve.RoomEntry) (*model.SearchResult, error) {
	now := a.Now()
	if since == 0 {
		since = now.Add(-scanDefaultWindow).UnixMilli()
	}
	var targets []*resolve.RoomEntry
	if room != nil {
		targets = []*resolve.RoomEntry{room}
	} else {
		for _, r := range rooms { // already sorted by activity
			if !r.Hidden && r.LastTS >= since {
				targets = append(targets, r)
			}
		}
	}
	res := &model.SearchResult{Query: q, Mode: "scan", Hits: []model.SearchHit{}}
	if len(targets) > scanMaxChats {
		targets = targets[:scanMaxChats]
		res.Partial = true
	}
	scope := fmt.Sprintf("last %d events per chat since %s", scanMaxEvents, a.Clock().Date(since))
	if room != nil {
		res.Scope = "in " + room.Name + ", " + scope
	} else {
		res.Scope = fmt.Sprintf("%d chats, %s", len(targets), scope)
	}

	terms := resolve.Terms(q)
	sctx, cancel := context.WithTimeout(ctx, scanDeadline)
	defer cancel()
	var mu sync.Mutex
	var hits []model.SearchHit
	var wg sync.WaitGroup
	sem := make(chan struct{}, matrix.MaxConcurrent)
	for _, r := range targets {
		wg.Add(1)
		go func(r *resolve.RoomEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			evs, complete := a.scanRoom(sctx, r.ID, since)
			if !complete && sctx.Err() != nil {
				mu.Lock()
				res.Partial = true
				mu.Unlock()
			}
			if len(evs) == 0 {
				return
			}
			// Match on raw events first so names are only fetched for hits.
			var any bool
			for i := range evs {
				if matchEvent(&evs[i], terms) {
					any = true
					break
				}
			}
			if !any {
				return
			}
			msgs := a.convert(ctx, r.ID, evs, 0)
			for i, m := range msgs {
				if m.TS < since || !resolve.MatchAll(resolve.Fold(m.Text), terms) {
					continue
				}
				h := model.SearchHit{ChatID: r.ID, ChatName: r.Name, Message: m}
				if i > 0 {
					b := msgs[i-1]
					h.Before = &b
				}
				if i+1 < len(msgs) {
					af := msgs[i+1]
					h.After = &af
				}
				mu.Lock()
				hits = append(hits, h)
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()
	sort.Slice(hits, func(i, j int) bool { return hits[i].Message.TS > hits[j].Message.TS })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	res.Hits = append(res.Hits, hits...)
	return res, nil
}

func matchEvent(e *matrix.Event, terms []string) bool {
	var c struct {
		Body       string `json:"body"`
		NewContent *struct {
			Body string `json:"body"`
		} `json:"m.new_content"`
	}
	if json.Unmarshal(e.Content, &c) != nil {
		return false
	}
	if c.NewContent != nil && resolve.MatchAll(resolve.Fold(c.NewContent.Body), terms) {
		return true
	}
	return resolve.MatchAll(resolve.Fold(c.Body), terms)
}

// scanRoom pages back through a room until since or the event budget.
func (a *App) scanRoom(ctx context.Context, roomID string, since int64) ([]matrix.Event, bool) {
	var evs []matrix.Event
	from := ""
	for len(evs) < scanMaxEvents {
		resp, err := a.MC.Messages(ctx, roomID, matrix.MessagesOpts{From: from, Dir: "b", Limit: scanPage, Filter: messagesFilter()})
		if err != nil {
			return evs, false
		}
		a.Res.LearnMembers(roomID, resp.State)
		evs = append(evs, resp.Chunk...)
		if resp.End == "" || len(resp.Chunk) == 0 || oldestTS(resp.Chunk) < since {
			return evs, true
		}
		from = resp.End
	}
	return evs, true
}
