package matrix

import "encoding/json"

// Event is a raw Matrix client event. Content is decoded lazily by callers.
type Event struct {
	Type     string          `json:"type"`
	EventID  string          `json:"event_id"`
	Sender   string          `json:"sender"`
	OriginTS int64           `json:"origin_server_ts"`
	StateKey *string         `json:"state_key,omitempty"`
	Content  json.RawMessage `json:"content"`
	RoomID   string          `json:"room_id,omitempty"`
	Unsigned Unsigned        `json:"unsigned"`
}

// Unsigned holds the parts of unsigned data mcp-matrix-ro uses.
type Unsigned struct {
	RedactedBecause json.RawMessage `json:"redacted_because,omitempty"`
	Relations       struct {
		Replace *Event `json:"m.replace,omitempty"`
	} `json:"m.relations"`
}

// Redacted reports whether the event has been redacted.
func (e *Event) Redacted() bool {
	return len(e.Unsigned.RedactedBecause) > 0 && string(e.Unsigned.RedactedBecause) != "null"
}

// Versions is the response of GET /_matrix/client/versions.
type Versions struct {
	Versions         []string        `json:"versions"`
	UnstableFeatures map[string]bool `json:"unstable_features"`
}

// WhoAmI is the response of GET /account/whoami.
type WhoAmI struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id"`
}

// SyncResponse is the subset of /sync used by mcp-matrix-ro.
type SyncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]JoinedRoom `json:"join"`
	} `json:"rooms"`
}

// JoinedRoom is one entry of rooms.join in a sync response.
type JoinedRoom struct {
	Summary  RoomSummary `json:"summary"`
	State    EventList   `json:"state"`
	Timeline Timeline    `json:"timeline"`
}

// RoomSummary is the room summary returned by sync.
type RoomSummary struct {
	Heroes             []string `json:"m.heroes"`
	JoinedMemberCount  *int     `json:"m.joined_member_count"`
	InvitedMemberCount *int     `json:"m.invited_member_count"`
}

// EventList is {"events": [...]}.
type EventList struct {
	Events []Event `json:"events"`
}

// Timeline is a sync timeline section.
type Timeline struct {
	Events    []Event `json:"events"`
	Limited   bool    `json:"limited"`
	PrevBatch string  `json:"prev_batch"`
}

// MessagesResponse is the response of GET /rooms/{id}/messages.
type MessagesResponse struct {
	Start string  `json:"start"`
	End   string  `json:"end"`
	Chunk []Event `json:"chunk"`
	State []Event `json:"state"`
}

// ContextResponse is the response of GET /rooms/{id}/context/{eventId}.
type ContextResponse struct {
	Event        Event   `json:"event"`
	EventsBefore []Event `json:"events_before"`
	EventsAfter  []Event `json:"events_after"`
	State        []Event `json:"state"`
}

// MembersResponse is the response of GET /rooms/{id}/members.
type MembersResponse struct {
	Chunk []Event `json:"chunk"`
}

// JoinedMembersResponse is the response of GET /rooms/{id}/joined_members.
type JoinedMembersResponse struct {
	Joined map[string]struct {
		DisplayName string `json:"display_name"`
	} `json:"joined"`
}

// Profile is the response of GET /profile/{userId}.
type Profile struct {
	DisplayName string `json:"displayname"`
}

// SearchResponse is the subset of POST /search used by mcp-matrix-ro.
type SearchResponse struct {
	SearchCategories struct {
		RoomEvents struct {
			Count     int            `json:"count"`
			Results   []SearchResult `json:"results"`
			NextBatch string         `json:"next_batch"`
		} `json:"room_events"`
	} `json:"search_categories"`
}

// SearchResult is one hit of a room_events search.
type SearchResult struct {
	Rank    float64 `json:"rank"`
	Result  Event   `json:"result"`
	Context struct {
		EventsBefore []Event `json:"events_before"`
		EventsAfter  []Event `json:"events_after"`
	} `json:"context"`
}
