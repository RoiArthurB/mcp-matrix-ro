// Package render converts Matrix events to messages and renders results as
// compact text (for small models) and JSON.
package render

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/model"
)

// ReplySnippet is the length of the quoted parent in a reply.
const ReplySnippet = 60

// Converter turns raw events into messages.
type Converter struct {
	MaxBody int
	// Name returns the display name of a user in a room ("me" for the owner).
	Name func(roomID, userID string) string
}

type relatesTo struct {
	RelType   string `json:"rel_type"`
	EventID   string `json:"event_id"`
	InReplyTo *struct {
		EventID string `json:"event_id"`
	} `json:"m.in_reply_to"`
}

type content struct {
	MsgType  string `json:"msgtype"`
	Body     string `json:"body"`
	FileName string `json:"filename"`
	GeoURI   string `json:"geo_uri"`
	Info     struct {
		Duration int64 `json:"duration"`
	} `json:"info"`
	Voice      json.RawMessage `json:"org.matrix.msc3245.voice"`
	RelatesTo  *relatesTo      `json:"m.relates_to"`
	NewContent *content        `json:"m.new_content"`
}

func parse(raw json.RawMessage) content {
	var c content
	_ = json.Unmarshal(raw, &c)
	return c
}

// IsEdit reports whether the event is an edit (m.replace) of another event.
func IsEdit(e *matrix.Event) bool {
	if e.Type != "m.room.message" {
		return false
	}
	c := parse(e.Content)
	return c.RelatesTo != nil && c.RelatesTo.RelType == "m.replace"
}

type edit struct {
	ts     int64
	sender string
	c      *content
}

// Convert renders events (any order, may include edits) into messages sorted
// oldest first. Edit events are folded into their target; replies are
// resolved against the batch or the reply fallback.
func (cv *Converter) Convert(roomID string, events []matrix.Event) []model.Message {
	edits := map[string]edit{}
	seen := map[string]bool{}
	var base []*matrix.Event
	for i := range events {
		e := &events[i]
		if seen[e.EventID] {
			continue
		}
		seen[e.EventID] = true
		if e.Type == "m.room.message" && !e.Redacted() {
			c := parse(e.Content)
			if c.RelatesTo != nil && c.RelatesTo.RelType == "m.replace" {
				if c.NewContent != nil {
					if prev, ok := edits[c.RelatesTo.EventID]; !ok || e.OriginTS >= prev.ts {
						edits[c.RelatesTo.EventID] = edit{e.OriginTS, e.Sender, c.NewContent}
					}
				}
				continue
			}
		} else if e.Type == "m.room.message" && IsEdit(e) {
			continue
		}
		base = append(base, e)
	}
	sort.SliceStable(base, func(i, j int) bool {
		if base[i].OriginTS != base[j].OriginTS {
			return base[i].OriginTS < base[j].OriginTS
		}
		return base[i].EventID < base[j].EventID
	})
	msgs := make([]model.Message, 0, len(base))
	byID := map[string]int{}
	for _, e := range base {
		m := cv.one(roomID, e, edits)
		byID[m.ID] = len(msgs)
		msgs = append(msgs, m)
	}
	for i := range msgs {
		r := msgs[i].Reply
		if r == nil {
			continue
		}
		if j, ok := byID[r.EventID]; ok {
			cv.SetParent(roomID, &msgs[i], msgs[j])
		} else if r.SenderID != "" && cv.Name != nil {
			r.Sender = cv.Name(roomID, r.SenderID)
		}
	}
	return msgs
}

// ConvertOne renders a single event (e.g. a fetched reply parent).
func (cv *Converter) ConvertOne(roomID string, e matrix.Event) model.Message {
	return cv.one(roomID, &e, nil)
}

func (cv *Converter) one(roomID string, e *matrix.Event, edits map[string]edit) model.Message {
	m := model.Message{ID: e.EventID, TS: e.OriginTS, SenderID: e.Sender}
	if cv.Name != nil {
		m.Sender = cv.Name(roomID, e.Sender)
	}
	switch {
	case e.Redacted():
		m.Text = "[deleted]"
		return m
	case e.Type == "m.room.encrypted":
		m.Text = "[encrypted message]"
		return m
	case e.Type == "m.sticker":
		m.Text = "[sticker]"
		return m
	}
	c := parse(e.Content)
	if c.MsgType == "" && c.Body == "" {
		m.Text = "[deleted]"
		return m
	}
	isReply := c.RelatesTo != nil && c.RelatesTo.InReplyTo != nil && c.RelatesTo.InReplyTo.EventID != ""
	shown := &c
	if ed, ok := edits[e.EventID]; ok && ed.sender == e.Sender {
		shown, m.Edited = ed.c, true
	} else if rep := e.Unsigned.Relations.Replace; rep != nil && rep.Sender == e.Sender {
		if rc := parse(rep.Content); rc.NewContent != nil {
			shown, m.Edited = rc.NewContent, true
		}
	}
	body := shown.Body
	if isReply {
		m.Reply = &model.Reply{EventID: c.RelatesTo.InReplyTo.EventID}
		if !m.Edited {
			var fbSender, fbText string
			body, fbSender, fbText = stripFallback(body)
			m.Reply.SenderID = fbSender
			m.Reply.Text = snippet(fbText, ReplySnippet)
		}
	}
	shownCopy := *shown
	shownCopy.Body = body
	m.Text = cv.text(&shownCopy, m.Sender)
	return m
}

// SetParent fills a reply from its parent message.
func (cv *Converter) SetParent(roomID string, m *model.Message, parent model.Message) {
	if m.Reply == nil {
		return
	}
	m.Reply.SenderID = parent.SenderID
	m.Reply.Sender = parent.Sender
	if m.Reply.Sender == "" && cv.Name != nil {
		m.Reply.Sender = cv.Name(roomID, parent.SenderID)
	}
	m.Reply.Text = snippet(parent.Text, ReplySnippet)
}

func (cv *Converter) text(c *content, sender string) string {
	name := c.FileName
	caption := ""
	if name == "" {
		name = c.Body
	} else if c.Body != "" && c.Body != c.FileName {
		caption = " " + cv.clean(c.Body)
	}
	name = oneLine(name)
	switch c.MsgType {
	case "m.text", "m.notice", "":
		return cv.clean(c.Body)
	case "m.emote":
		return "* " + sender + " " + cv.clean(c.Body)
	case "m.image":
		return "[image: " + name + "]" + caption
	case "m.video":
		return "[video: " + name + "]" + caption
	case "m.file":
		return "[file: " + name + "]" + caption
	case "m.audio":
		if len(c.Voice) > 0 {
			if c.Info.Duration > 0 {
				s := (c.Info.Duration + 500) / 1000
				return fmt.Sprintf("[voice message %d:%02d]", s/60, s%60)
			}
			return "[voice message]"
		}
		return "[audio: " + name + "]" + caption
	case "m.location":
		return "[location: " + c.GeoURI + "]"
	default:
		if c.Body != "" {
			return cv.clean(c.Body)
		}
		return "[" + strings.TrimPrefix(c.MsgType, "m.") + "]"
	}
}

var blankRuns = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)

// clean normalises newlines, collapses blank-line runs and truncates.
func (cv *Converter) clean(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSpace(blankRuns.ReplaceAllString(s, "\n"))
	return Truncate(s, cv.MaxBody)
}

// Truncate cuts s to n runes, appending "…[+K chars]". n<=0 disables.
func Truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	total := utf8.RuneCountInString(s)
	if total <= n {
		return s
	}
	i, k := 0, 0
	for i = range s {
		if k == n {
			break
		}
		k++
	}
	return s[:i] + fmt.Sprintf("…[+%d chars]", total-n)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func snippet(s string, n int) string {
	s = oneLine(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

var fallbackHead = regexp.MustCompile(`^> (?:\* )?<(@[^>]+)> ?(.*)$`)

// stripFallback removes the "> <@user> …" reply fallback from a body and
// returns the remaining body plus the quoted sender/first line, if any.
func stripFallback(body string) (rest, sender, quoted string) {
	if !strings.HasPrefix(body, "> ") && body != ">" {
		return body, "", ""
	}
	lines := strings.Split(body, "\n")
	i := 0
	var q []string
	for ; i < len(lines) && strings.HasPrefix(lines[i], ">"); i++ {
		if i == 0 {
			if m := fallbackHead.FindStringSubmatch(lines[0]); m != nil {
				sender = m[1]
				q = append(q, m[2])
				continue
			}
		}
		q = append(q, strings.TrimPrefix(strings.TrimPrefix(lines[i], ">"), " "))
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	return strings.Join(lines[i:], "\n"), sender, strings.Join(q, " ")
}
