// Package resolve turns raw Matrix rooms into named chats: display names,
// network tagging, DM detection, chat-by-name lookup and caching.
package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/config"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/model"
)

// MessageTypes are the timeline event types mcp-matrix-ro reads.
var MessageTypes = []string{"m.room.message", "m.room.encrypted", "m.sticker"}

// maxProfileFetches bounds global profile lookups per operation.
const maxProfileFetches = 30

// Resolver resolves names and chats. It is safe for concurrent use.
type Resolver struct {
	cfg   *config.Config
	mc    *matrix.Client
	cache *Cache
	log   *slog.Logger
	now   func() time.Time

	mu     sync.Mutex
	whoami string
	me     map[string]bool
}

// New builds a Resolver.
func New(cfg *config.Config, mc *matrix.Client, cache *Cache, log *slog.Logger, now func() time.Time) *Resolver {
	r := &Resolver{cfg: cfg, mc: mc, cache: cache, log: log, now: now, me: map[string]bool{}}
	for _, id := range cfg.Me {
		r.me[id] = true
	}
	return r
}

// Cache returns the underlying cache.
func (r *Resolver) Cache() *Cache { return r.cache }

// WhoAmI returns the account's user ID, from cache or the server.
func (r *Resolver) WhoAmI(ctx context.Context) (string, error) {
	r.mu.Lock()
	w := r.whoami
	r.mu.Unlock()
	if w != "" {
		return w, nil
	}
	if w = r.cache.whoami(); w == "" {
		res, err := r.mc.WhoAmI(ctx)
		if err != nil {
			return "", err
		}
		w = res.UserID
		r.cache.setWhoami(w)
	}
	r.mu.Lock()
	r.whoami = w
	r.me[w] = true
	r.mu.Unlock()
	return w, nil
}

// IsMe reports whether id is one of the owner's IDs.
func (r *Resolver) IsMe(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.me[id]
}

// Network tags a user ID; "" if no network regex matches.
func (r *Resolver) Network(userID string) string {
	for _, n := range r.cfg.Networks {
		if n.Re.MatchString(userID) {
			return n.Name
		}
	}
	return ""
}

func (r *Resolver) isBot(userID string) bool {
	return r.cfg.ExcludeBots != nil && r.cfg.ExcludeBots.MatchString(userID)
}

// Localpart renders a user ID without a display name: bridge puppets
// "@whatsapp_84901234567:dom" become "+84901234567".
func (r *Resolver) Localpart(userID string) string {
	id := userID
	for _, n := range r.cfg.Networks {
		if loc := n.Re.FindStringIndex(id); loc != nil && loc[0] == 0 {
			id = id[loc[1]:]
			lp, _, _ := strings.Cut(id, ":")
			if lp != "" && strings.Trim(lp, "0123456789") == "" {
				return "+" + lp
			}
			return lp
		}
	}
	lp, _, _ := strings.Cut(strings.TrimPrefix(id, "@"), ":")
	return lp
}

// SenderName returns the display name for a user in a room, without network
// I/O: "me", room member name, cached global profile, or localpart.
func (r *Resolver) SenderName(roomID, userID string) string {
	if r.IsMe(userID) {
		return "me"
	}
	if n, ok := r.cache.memberName(roomID, userID); ok && n != "" {
		return n
	}
	if n, _ := r.cache.profile(userID); n != "" {
		return n
	}
	return r.Localpart(userID)
}

// hasName reports whether a non-fallback name is known for the user.
func (r *Resolver) hasName(roomID, userID string) bool {
	if r.IsMe(userID) {
		return true
	}
	if n, ok := r.cache.memberName(roomID, userID); ok && n != "" {
		return true
	}
	_, fresh := r.cache.profile(userID)
	return fresh
}

// LearnMembers records display names from m.room.member events.
func (r *Resolver) LearnMembers(roomID string, evs []matrix.Event) {
	for _, e := range evs {
		if e.Type != "m.room.member" || e.StateKey == nil {
			continue
		}
		var c struct {
			Membership  string `json:"membership"`
			DisplayName string `json:"displayname"`
		}
		if json.Unmarshal(e.Content, &c) != nil {
			continue
		}
		if c.DisplayName != "" {
			r.cache.setMemberName(roomID, *e.StateKey, c.DisplayName)
		}
	}
}

// FetchNames looks up global profiles for senders without a known name,
// with bounded concurrency and a per-call budget. pairs are room/user.
func (r *Resolver) FetchNames(ctx context.Context, pairs [][2]string) {
	seen := map[string]bool{}
	var todo []string
	for _, p := range pairs {
		if seen[p[1]] || r.hasName(p[0], p[1]) {
			continue
		}
		seen[p[1]] = true
		todo = append(todo, p[1])
		if len(todo) >= maxProfileFetches {
			break
		}
	}
	var wg sync.WaitGroup
	for _, u := range todo {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			p, err := r.mc.Profile(ctx, u)
			if err != nil {
				r.log.Debug("profile lookup failed", "user", u, "err", err)
				r.cache.setProfile(u, "") // remember the miss for profileTTL
				return
			}
			r.cache.setProfile(u, p.DisplayName)
		}(u)
	}
	wg.Wait()
}

// RoomData is a synced room with its timeline.
type RoomData struct {
	*RoomEntry
	Events    []matrix.Event
	Limited   bool
	PrevBatch string
}

// SyncFilter is the inline filter used for every /sync (SPEC §5.1).
func SyncFilter(timelineLimit int) map[string]any {
	none := map[string]any{"not_types": []string{"*"}}
	return map[string]any{
		"presence":     none,
		"account_data": none,
		"room": map[string]any{
			"include_leave": false,
			"account_data":  none,
			"ephemeral":     none,
			"state": map[string]any{
				"types":             []string{"m.room.name", "m.room.canonical_alias", "m.room.member", "m.room.create"},
				"lazy_load_members": true,
			},
			"timeline": map[string]any{
				"types":             MessageTypes,
				"limit":             timelineLimit,
				"lazy_load_members": true,
			},
		},
	}
}

// Sync performs one filtered initial sync and refreshes the room index.
func (r *Resolver) Sync(ctx context.Context, timelineLimit int) ([]*RoomData, error) {
	if _, err := r.WhoAmI(ctx); err != nil {
		return nil, err
	}
	resp, err := r.mc.Sync(ctx, SyncFilter(timelineLimit), "")
	if err != nil {
		return nil, err
	}
	out := make([]*RoomData, 0, len(resp.Rooms.Join))
	var partial []*RoomEntry
	for id, jr := range resp.Rooms.Join {
		r.LearnMembers(id, jr.State.Events)
		prev, _ := r.cache.room(id)
		e := r.classify(id, jr, prev)
		out = append(out, &RoomData{RoomEntry: e, Events: jr.Timeline.Events, Limited: jr.Timeline.Limited, PrevBatch: jr.Timeline.PrevBatch})
		if e.partial && e.Joined <= maxRosterSize && !e.Space {
			partial = append(partial, e)
		}
	}
	r.fetchRosters(ctx, partial)
	var missing [][2]string
	for _, d := range out {
		if d.DM && !d.Hidden && d.Name == "" {
			for _, o := range d.Others {
				missing = append(missing, [2]string{d.ID, o})
			}
		}
	}
	if len(missing) > 0 {
		r.FetchNames(ctx, missing)
	}
	entries := make([]*RoomEntry, len(out))
	for i, d := range out {
		r.finishName(d.RoomEntry)
		entries[i] = d.RoomEntry
	}
	r.cache.putRooms(entries, true)
	sort.Slice(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, nil
}

// maxRosterSize is the largest room whose full member list is fetched when
// /sync left members out. Bridged DMs are small (me, my puppet, the contact,
// the bridge bot); bigger rooms are groups whatever the unknown members are.
const maxRosterSize = 6

// maxRosterFetches bounds /joined_members calls per sync; rosters are cached,
// so later syncs only fetch rooms that are new or changed size.
const maxRosterFetches = 40

// fetchRosters loads the full member list of rooms /sync described only
// partially (Synapse sends no heroes for named rooms, and lazy loading only
// includes recent senders), so bots and the owner's IDs are not mistaken for
// people. Failures keep the partial classification.
func (r *Resolver) fetchRosters(ctx context.Context, rooms []*RoomEntry) {
	if len(rooms) > maxRosterFetches {
		sort.Slice(rooms, func(i, j int) bool { return rooms[i].LastTS > rooms[j].LastTS })
		rooms = rooms[:maxRosterFetches]
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, e := range rooms {
		wg.Add(1)
		sem <- struct{}{}
		go func(e *RoomEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			jm, err := r.mc.JoinedMembers(ctx, e.ID)
			if err != nil {
				r.log.Debug("joined_members failed", "room", e.ID, "err", err)
				return
			}
			ids := make([]string, 0, len(jm.Joined))
			for id, m := range jm.Joined {
				ids = append(ids, id)
				if m.DisplayName != "" {
					r.cache.setMemberName(e.ID, id, m.DisplayName)
				}
			}
			r.applyRoster(e, ids)
		}(e)
	}
	wg.Wait()
}

// SetRoster reclassifies a room from its complete list of joined member IDs
// (e.g. from its full state) and stores the result in the index.
func (r *Resolver) SetRoster(e *RoomEntry, joined []string) {
	r.applyRoster(e, joined)
	r.applyNameExclude(e)
	r.cache.putRooms([]*RoomEntry{e}, false)
}

func (r *Resolver) applyRoster(e *RoomEntry, joined []string) {
	ids := append([]string(nil), joined...)
	sort.Strings(ids)
	e.Roster, e.RosterCount = ids, len(ids)
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	r.applyMembers(e, set, len(ids), true)
}

// classify builds a RoomEntry from sync data. Name is left empty when the
// room has no name or alias (filled by finishName once member names are
// known).
func (r *Resolver) classify(id string, jr matrix.JoinedRoom, prev *RoomEntry) *RoomEntry {
	e := &RoomEntry{Chat: model.Chat{ID: id}}
	var name, alias string
	var created int64
	members := map[string]bool{}
	for _, ev := range jr.State.Events {
		switch ev.Type {
		case "m.room.name":
			var c struct{ Name string }
			_ = json.Unmarshal(ev.Content, &c)
			name = strings.TrimSpace(c.Name)
		case "m.room.canonical_alias":
			var c struct {
				Alias string `json:"alias"`
			}
			_ = json.Unmarshal(ev.Content, &c)
			alias = c.Alias
		case "m.room.create":
			created = ev.OriginTS
			var c struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(ev.Content, &c)
			e.Space = c.Type == "m.space"
		case "m.room.member":
			var c struct{ Membership string }
			_ = json.Unmarshal(ev.Content, &c)
			if ev.StateKey != nil && c.Membership == "join" {
				members[*ev.StateKey] = true
			}
		}
	}
	for _, h := range jr.Summary.Heroes {
		members[h] = true
	}
	var last int64
	for _, ev := range jr.Timeline.Events {
		members[ev.Sender] = true
		if ev.OriginTS > last {
			last = ev.OriginTS
		}
	}
	if last == 0 && prev != nil {
		last = prev.LastTS
	}
	if last == 0 {
		last = created
	}
	e.LastTS = last
	if prev != nil && prev.Network != "" && prev.Network != "matrix" {
		e.Network = prev.Network // a room's network does not change
	}

	joined := 0
	if jr.Summary.JoinedMemberCount != nil {
		joined = *jr.Summary.JoinedMemberCount
	} else if prev != nil {
		joined = prev.Joined
	}
	if r.whoami != "" {
		members[r.whoami] = true // the syncing account is always joined
	}
	if joined == 0 {
		joined = len(members)
	}
	if prev != nil && len(prev.Roster) > 0 && prev.RosterCount == joined {
		// Same size as when the full list was fetched: reuse it.
		for _, m := range prev.Roster {
			members[m] = true
		}
		if len(members) == joined {
			e.Roster, e.RosterCount = prev.Roster, prev.RosterCount
		}
	}
	r.applyMembers(e, members, joined, len(members) >= joined)

	switch {
	case name != "":
		e.Name = name
	case alias != "":
		e.Name = alias
	}
	return e
}

// applyMembers sets Network, Others, DM, Members, Hidden and the partial
// flag from the member IDs known for a room. joined is the server's joined
// member count; when complete is false some joined members are unknown.
func (r *Resolver) applyMembers(e *RoomEntry, known map[string]bool, joined int, complete bool) {
	ids := make([]string, 0, len(known))
	for m := range known {
		ids = append(ids, m)
	}
	sort.Strings(ids)
	var others, humans, bots []string
	meCount := 0
	network, meNetwork := "", ""
	for _, m := range ids {
		if r.IsMe(m) {
			meCount++
			if meNetwork == "" {
				meNetwork = r.Network(m)
			}
			continue
		}
		if network == "" {
			network = r.Network(m)
		}
		others = append(others, m)
		if r.isBot(m) {
			bots = append(bots, m)
		} else {
			humans = append(humans, m)
		}
	}
	switch {
	case network != "":
	case meNetwork != "":
		network = meNetwork
	case e.Network != "":
		network = e.Network
	default:
		network = "matrix"
	}
	e.Network = network
	e.Others = others
	if meCount == 0 {
		meCount = 1
	}

	unknown := 0
	if !complete && joined > len(known) {
		unknown = joined - len(known)
	}
	e.partial, e.Joined = unknown > 0, joined
	if unknown > 0 && network != "matrix" && len(bots) == 0 {
		// Bridged rooms always hold the bridge bot: when the member list
		// is incomplete, count one unknown member as that bot.
		unknown--
	}
	// DM: exactly one other person, and no unknown members left.
	e.DM = len(humans) == 1 && unknown == 0
	// Members counts people: bridge bots are left out and the owner's
	// accounts (Matrix user, bridge puppets) count once.
	people := len(humans) + unknown + 1
	if joined < people {
		people = joined
	}
	e.Members = people
	e.Hidden = e.Space || (len(humans) == 0 && len(bots) > 0 && unknown == 0) || r.cfg.ExcludeIDs[e.ID]
}

func (r *Resolver) applyNameExclude(e *RoomEntry) {
	for _, re := range r.cfg.ExcludeRe {
		if re.MatchString(e.Name) {
			e.Hidden = true
		}
	}
}

// finishName names DMs after their other member and unnamed groups after
// up to three members, then applies the name exclusions.
func (r *Resolver) finishName(e *RoomEntry) {
	defer r.applyNameExclude(e)
	if e.Name != "" {
		return
	}
	var names []string
	for _, o := range e.Others {
		if !r.isBot(o) {
			names = append(names, r.SenderName(e.ID, o))
		}
	}
	switch {
	case len(names) == 0:
		e.Name = e.ID
	case e.DM || len(names) == 1:
		e.Name = names[0]
	case len(names) <= 3:
		e.Name = strings.Join(names, ", ")
	default:
		e.Name = fmt.Sprintf("%s and %d others", strings.Join(names[:3], ", "), len(names)-3)
	}
}

// Index returns all joined rooms (including hidden ones), sorted by last
// activity, from cache when fresh or via a light sync.
func (r *Resolver) Index(ctx context.Context) ([]*RoomEntry, error) {
	if _, err := r.WhoAmI(ctx); err != nil {
		return nil, err
	}
	if rooms, ok := r.cache.index(); ok {
		sort.Slice(rooms, func(i, j int) bool { return rooms[i].LastTS > rooms[j].LastTS })
		return rooms, nil
	}
	data, err := r.Sync(ctx, 1)
	if err != nil {
		return nil, err
	}
	out := make([]*RoomEntry, len(data))
	for i, d := range data {
		out[i] = d.RoomEntry
	}
	return out, nil
}

// Room returns the index entry for a room ID, if known.
func (r *Resolver) Room(ctx context.Context, id string) (*RoomEntry, bool) {
	if _, err := r.Index(ctx); err != nil {
		return nil, false
	}
	return r.cache.room(id)
}

// AmbiguousError lists the chats a name matched.
type AmbiguousError struct {
	Query      string
	Candidates []*RoomEntry
}

func (e *AmbiguousError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d chats; call again with one of these room IDs:", e.Query, len(e.Candidates))
	for i, c := range e.Candidates {
		if i == 8 {
			fmt.Fprintf(&b, "\n… and %d more (be more specific)", len(e.Candidates)-8)
			break
		}
		fmt.Fprintf(&b, "\n- %s (%s %s) · %s", c.Name, model.NetworkLabel(c.Network), c.Kind(), c.ID)
	}
	return b.String()
}

// NotFoundError means no chat matched.
type NotFoundError struct{ Query string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no chat matches %q; use the chats tool to list chats", e.Query)
}

// Find resolves a room ID, alias or name (case- and accent-insensitive
// substring). Ambiguous names return *AmbiguousError.
func (r *Resolver) Find(ctx context.Context, q string) (*RoomEntry, error) {
	q = strings.TrimSpace(q)
	rooms, err := r.Index(ctx)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(q, "!") {
		for _, e := range rooms {
			if e.ID == q {
				return e, nil
			}
		}
		return nil, &NotFoundError{q}
	}
	fq := Fold(q)
	if fq == "" {
		return nil, &NotFoundError{q}
	}
	var exact, sub, hiddenSub []*RoomEntry
	for _, e := range rooms {
		fn := Fold(e.Name)
		switch {
		case fn == fq && !e.Hidden:
			exact = append(exact, e)
		case strings.Contains(fn, fq):
			if e.Hidden {
				hiddenSub = append(hiddenSub, e)
			} else {
				sub = append(sub, e)
			}
		}
	}
	for _, set := range [][]*RoomEntry{exact, sub, hiddenSub} {
		switch len(set) {
		case 0:
			continue
		case 1:
			return set[0], nil
		default:
			return nil, &AmbiguousError{Query: q, Candidates: set}
		}
	}
	return nil, &NotFoundError{q}
}
