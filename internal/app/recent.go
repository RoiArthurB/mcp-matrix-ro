package app

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/RoiArthurB/mxread/internal/matrix"
	"github.com/RoiArthurB/mxread/internal/model"
	"github.com/RoiArthurB/mxread/internal/render"
	"github.com/RoiArthurB/mxread/internal/resolve"
)

// RecentParams are the recent tool / digest inputs.
type RecentParams struct {
	Since   string // ISO or relative; default 24h
	SinceMS int64  // overrides Since when non-zero (digest cursor)
	Limit   int    // total messages; default 40, max 200; -1 = unlimited
	PerChat int    // default 10
	Network string
	Chat    string
}

const maxBackfillPages = 3

// Recent returns the latest messages across chats using one filtered /sync,
// paging back with /messages only for chats whose sync window was too short.
func (a *App) Recent(ctx context.Context, p RecentParams) (*model.RecentResult, error) {
	now := a.Now()
	since := p.SinceMS
	if since == 0 {
		s := p.Since
		if s == "" {
			s = "24h"
		}
		t, err := a.parseWhen(s)
		if err != nil {
			return nil, err
		}
		since = t.UnixMilli()
	}
	limit := p.Limit
	if limit != -1 {
		limit = clamp(limit, 40, 1, 200)
	}
	perChat := clamp(p.PerChat, 10, 1, 100)
	net := strings.ToLower(strings.TrimSpace(p.Network))

	var only string
	if strings.TrimSpace(p.Chat) != "" {
		room, err := a.Res.Find(ctx, p.Chat)
		if err != nil {
			return nil, err
		}
		only = room.ID
	}

	rooms, err := a.Res.Sync(ctx, clamp(perChat+5, 10, 10, 50))
	if err != nil {
		return nil, err
	}
	var selected []*resolve.RoomData
	for _, r := range rooms {
		if (only != "" && r.ID != only) || (only == "" && r.Hidden) || (net != "" && r.Network != net) {
			continue
		}
		if r.LastTS < since {
			continue
		}
		selected = append(selected, r)
	}

	// Backfill chats whose sync timeline was cut before reaching `since`.
	var wg sync.WaitGroup
	for _, r := range selected {
		if !needsBackfill(r, since, perChat) {
			continue
		}
		wg.Add(1)
		go func(r *resolve.RoomData) {
			defer wg.Done()
			a.backfill(ctx, r, since, perChat)
		}(r)
	}
	wg.Wait()

	res := &model.RecentResult{Since: since, Now: now.UnixMilli(), Network: net}
	type ref struct{ chat, msg int }
	var all []ref
	for _, r := range selected {
		msgs := a.convert(ctx, r.ID, r.Events, 3)
		var in []model.Message
		for _, m := range msgs {
			if m.TS >= since {
				in = append(in, m)
			}
		}
		if len(in) == 0 {
			continue
		}
		cm := model.ChatMessages{Chat: r.Chat, Count: len(in)}
		cm.More = r.Limited && oldestTS(r.Events) >= since // window not fully covered
		if len(in) > perChat {
			in = in[len(in)-perChat:]
			cm.More = true
		}
		cm.Messages = in
		res.Chats = append(res.Chats, cm)
	}
	for ci, c := range res.Chats {
		for mi := range c.Messages {
			all = append(all, ref{ci, mi})
		}
	}
	if limit > 0 && len(all) > limit {
		sort.Slice(all, func(i, j int) bool {
			return res.Chats[all[i].chat].Messages[all[i].msg].TS > res.Chats[all[j].chat].Messages[all[j].msg].TS
		})
		keep := map[ref]bool{}
		for _, x := range all[:limit] {
			keep[x] = true
		}
		res.Truncated = true
		var chats []model.ChatMessages
		for ci, c := range res.Chats {
			var kept []model.Message
			for mi, m := range c.Messages {
				if keep[ref{ci, mi}] {
					kept = append(kept, m)
				}
			}
			if len(kept) > 0 {
				c.Messages = kept
				chats = append(chats, c)
			}
		}
		res.Chats = chats
	}
	sort.SliceStable(res.Chats, func(i, j int) bool {
		return lastTS(res.Chats[i].Messages) > lastTS(res.Chats[j].Messages)
	})
	for _, c := range res.Chats {
		res.Total += len(c.Messages)
	}
	return res, nil
}

func lastTS(m []model.Message) int64 {
	if len(m) == 0 {
		return 0
	}
	return m[len(m)-1].TS
}

func oldestTS(evs []matrix.Event) int64 {
	var o int64
	for i, e := range evs {
		if i == 0 || e.OriginTS < o {
			o = e.OriginTS
		}
	}
	return o
}

func needsBackfill(r *resolve.RoomData, since int64, perChat int) bool {
	if !r.Limited || r.PrevBatch == "" || len(r.Events) == 0 || oldestTS(r.Events) < since {
		return false
	}
	n := 0
	for i := range r.Events {
		if !render.IsEdit(&r.Events[i]) {
			n++
		}
	}
	return n <= perChat
}

func (a *App) backfill(ctx context.Context, r *resolve.RoomData, since int64, perChat int) {
	from := r.PrevBatch
	for page := 0; page < maxBackfillPages; page++ {
		resp, err := a.MC.Messages(ctx, r.ID, matrix.MessagesOpts{From: from, Dir: "b", Limit: 50, Filter: messagesFilter()})
		if err != nil {
			a.Log.Debug("backfill failed", "err", err)
			return
		}
		a.Res.LearnMembers(r.ID, resp.State)
		r.Events = append(r.Events, resp.Chunk...)
		if resp.End == "" || len(resp.Chunk) == 0 {
			r.Limited = false
			return
		}
		from = resp.End
		if !needsBackfill(r, since, perChat) {
			return
		}
	}
}
