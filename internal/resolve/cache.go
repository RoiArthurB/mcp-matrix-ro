package resolve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/fsutil"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/model"
)

// TTL is how long the room index and names are considered fresh (SPEC §8).
const TTL = 10 * time.Minute

// profileTTL bounds how long a global display name is reused.
const profileTTL = 24 * time.Hour

// RoomEntry is a cached room plus classification details.
type RoomEntry struct {
	model.Chat
	Hidden bool     `json:"hidden,omitempty"`
	Space  bool     `json:"space,omitempty"`
	Others []string `json:"others,omitempty"` // non-me member IDs known (heroes etc.)
	Joined int      `json:"joined,omitempty"` // server's joined member count
	// Roster is the full joined member list, fetched for small rooms that
	// /sync described only partially; RosterCount is the joined count then.
	Roster      []string `json:"roster,omitempty"`
	RosterCount int      `json:"roster_n,omitempty"`

	partial bool // some joined members are unknown
}

type nameEntry struct {
	Name string `json:"n"`
	At   int64  `json:"at"`
}

type cacheData struct {
	Version  int                          `json:"v"`
	WhoAmI   string                       `json:"whoami,omitempty"`
	IndexAt  int64                        `json:"index_at,omitempty"`
	Rooms    map[string]*RoomEntry        `json:"rooms"`
	Members  map[string]map[string]string `json:"members"` // room -> user -> display name
	Profiles map[string]nameEntry         `json:"profiles"`
}

// Cache is an in-memory cache mirrored to a JSON file in the state dir.
// It holds names and room metadata only, never message bodies.
type Cache struct {
	saveMu sync.Mutex
	mu     sync.Mutex
	path   string
	now    func() time.Time
	d      cacheData
	dirty  bool
}

// OpenCache loads dir/cache.json if present. dir may be empty (memory only).
func OpenCache(dir string, now func() time.Time) *Cache {
	c := &Cache{now: now}
	if dir != "" {
		c.path = filepath.Join(dir, "cache.json")
		if b, err := os.ReadFile(c.path); err == nil {
			_ = json.Unmarshal(b, &c.d)
		}
	}
	if c.d.Version != 1 {
		c.d = cacheData{Version: 1}
	}
	if c.d.Rooms == nil {
		c.d.Rooms = map[string]*RoomEntry{}
	}
	if c.d.Members == nil {
		c.d.Members = map[string]map[string]string{}
	}
	if c.d.Profiles == nil {
		c.d.Profiles = map[string]nameEntry{}
	}
	return c
}

// Save writes the cache atomically if it changed. Errors are non-fatal.
// The snapshot is taken under the lock but written outside it, so readers
// are not held up by disk I/O; saveMu keeps concurrent saves in order.
func (c *Cache) Save() error {
	if c.path == "" {
		return nil
	}
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(c.d)
	c.dirty = false
	c.mu.Unlock()
	if err == nil {
		err = fsutil.WriteFileAtomic(c.path, b)
	}
	if err != nil {
		c.mu.Lock()
		c.dirty = true // try again next time
		c.mu.Unlock()
	}
	return err
}

func (c *Cache) whoami() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.d.WhoAmI
}

func (c *Cache) setWhoami(u string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.d.WhoAmI != u {
		c.d.WhoAmI, c.dirty = u, true
	}
}

// index returns the cached rooms if fresh.
func (c *Cache) index() ([]*RoomEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.d.IndexAt == 0 || c.now().Sub(time.UnixMilli(c.d.IndexAt)) > TTL {
		return nil, false
	}
	out := make([]*RoomEntry, 0, len(c.d.Rooms))
	for _, r := range c.d.Rooms {
		cp := *r
		out = append(out, &cp)
	}
	return out, true
}

func (c *Cache) room(id string) (*RoomEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.d.Rooms[id]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// putRooms stores rooms. full=true replaces the whole index (complete sync).
func (c *Cache) putRooms(rooms []*RoomEntry, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if full {
		c.d.Rooms = make(map[string]*RoomEntry, len(rooms))
		c.d.IndexAt = c.now().UnixMilli()
		keep := make(map[string]bool, len(rooms))
		for _, r := range rooms {
			keep[r.ID] = true
		}
		for id := range c.d.Members { // forget rooms we left
			if !keep[id] {
				delete(c.d.Members, id)
			}
		}
	}
	for _, r := range rooms {
		cp := *r
		c.d.Rooms[r.ID] = &cp
	}
	c.dirty = true
}

func (c *Cache) memberName(room, user string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.d.Members[room][user]
	return n, ok
}

func (c *Cache) setMemberName(room, user, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.d.Members[room]
	if m == nil {
		m = map[string]string{}
		c.d.Members[room] = m
	}
	if m[user] != name {
		m[user] = name
		c.dirty = true
	}
}

func (c *Cache) profile(user string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.d.Profiles[user]
	if !ok || c.now().Sub(time.UnixMilli(e.At)) > profileTTL {
		return e.Name, false
	}
	return e.Name, true
}

func (c *Cache) setProfile(user, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.d.Profiles[user] = nameEntry{Name: name, At: c.now().UnixMilli()}
	c.dirty = true
}
