package model

// ChatMessages is one chat block in a recent/digest result.
type ChatMessages struct {
	Chat
	Count    int       `json:"count"`               // messages in the window that were found
	More     bool      `json:"more,omitempty"`      // older messages in the window were not shown
	AtLeast  bool      `json:"count_min,omitempty"` // Count is a lower bound (window not fully scanned)
	Messages []Message `json:"messages"`
}

// RecentResult is the result of the recent tool and digest CLI.
type RecentResult struct {
	Since     int64          `json:"since"`
	Now       int64          `json:"now"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated,omitempty"` // total limit reached
	Network   string         `json:"network,omitempty"`
	Chats     []ChatMessages `json:"chats"`
}

// ChatsResult is the result of the chats tool.
type ChatsResult struct {
	Matching int    `json:"matching"`
	Chats    []Chat `json:"chats"`
}

// ReadResult is the result of the read tool.
type ReadResult struct {
	Chat     Chat      `json:"chat"`
	Messages []Message `json:"messages"`
	Cursor   string    `json:"cursor,omitempty"` // for older messages
	After    int64     `json:"after,omitempty"`
	Before   int64     `json:"before,omitempty"`
}

// SearchHit is one search match with optional neighbouring messages.
type SearchHit struct {
	ChatID   string   `json:"chat_id"`
	ChatName string   `json:"chat"`
	Message  Message  `json:"message"`
	Before   *Message `json:"before,omitempty"`
	After    *Message `json:"after,omitempty"`
}

// SearchResult is the result of the search tool.
type SearchResult struct {
	Query   string      `json:"query"`
	Mode    string      `json:"mode"` // "server" or "scan"
	Scope   string      `json:"scope,omitempty"`
	Partial bool        `json:"partial,omitempty"`
	Hits    []SearchHit `json:"hits"`
}

// Member is one chat member.
type Member struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Bot  bool   `json:"bot,omitempty"`
	Me   bool   `json:"me,omitempty"`
}

// ChatInfoResult is the result of the chat_info tool.
type ChatInfoResult struct {
	Chat
	Topic        string   `json:"topic,omitempty"`
	Alias        string   `json:"alias,omitempty"`
	Created      int64    `json:"created,omitempty"`
	MemberList   []Member `json:"member_list"`
	MoreMembers  int      `json:"more_members,omitempty"`
	CreatorID    string   `json:"creator,omitempty"`
	JoinedTotal  int      `json:"joined"`
	IsBridgeRoom bool     `json:"-"`
}
