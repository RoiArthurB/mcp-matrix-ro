// Package model holds rendering-independent chat and message types.
package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Chat is one Matrix room as seen by the user.
type Chat struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Network string `json:"network"`
	DM      bool   `json:"dm"`
	Members int    `json:"members,omitempty"`
	LastTS  int64  `json:"last_ts,omitempty"` // ms since epoch
	Hidden  bool   `json:"-"`
	Space   bool   `json:"-"`
}

// Kind returns "DM" or "group".
func (c Chat) Kind() string {
	if c.DM {
		return "DM"
	}
	return "group"
}

// Message is one rendered-ready message.
type Message struct {
	ID       string `json:"id"`
	TS       int64  `json:"ts"`
	SenderID string `json:"sender_id"`
	Sender   string `json:"sender"`
	Text     string `json:"text"`
	Edited   bool   `json:"edited,omitempty"`
	Reply    *Reply `json:"reply,omitempty"`
}

// Reply describes the message a message replies to.
type Reply struct {
	EventID  string `json:"event_id"`
	SenderID string `json:"-"`
	Sender   string `json:"sender,omitempty"`
	Text     string `json:"text,omitempty"`
}

// Time returns the message time.
func (m Message) Time() time.Time { return time.UnixMilli(m.TS) }

// ParseWhen parses an ISO date/datetime or a relative duration ("24h", "3d",
// "90m", "2w") into an absolute time. Relative values are "ago" from now.
// Date-only and zone-less values are interpreted in loc.
func ParseWhen(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	if d, ok := parseRelative(s); ok {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	switch strings.ToLower(s) {
	case "today":
		y, m, d := now.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc), nil
	case "yesterday":
		y, m, d := now.In(loc).AddDate(0, 0, -1).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, loc), nil
	}
	return time.Time{}, fmt.Errorf("bad time %q: use ISO like 2026-10-31T07:00 or relative like 24h, 3d", s)
}

func parseRelative(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	unit := s[len(s)-1]
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil || n < 0 {
		return 0, false
	}
	var u time.Duration
	switch unit {
	case 'm':
		u = time.Minute
	case 'h':
		u = time.Hour
	case 'd':
		u = 24 * time.Hour
	case 'w':
		u = 7 * 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(n * float64(u)), true
}

// NetworkLabel returns a display label for a network tag.
func NetworkLabel(n string) string {
	switch n {
	case "whatsapp":
		return "WhatsApp"
	case "telegram":
		return "Telegram"
	case "signal":
		return "Signal"
	case "matrix":
		return "Matrix"
	case "":
		return "Matrix"
	}
	return strings.ToUpper(n[:1]) + n[1:]
}
