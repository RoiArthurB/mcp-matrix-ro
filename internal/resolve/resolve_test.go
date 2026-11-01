package resolve

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/RoiArthurB/mcp-matrix-ro/internal/config"
	"github.com/RoiArthurB/mcp-matrix-ro/internal/matrix"
)

func TestFold(t *testing.T) {
	for in, want := range map[string]string{"Mẹ": "me", "Đà Lạt": "da lat", "ÉCOLE": "ecole", "Nguyễn Thị Hà": "nguyen thi ha"} {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
	if !MatchAll(Fold("Họp lúc 10 giờ"), Terms("hop 10")) || MatchAll("abc", nil) {
		t.Error("MatchAll")
	}
}

func newRes(t *testing.T, me string) *Resolver {
	env := map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MCP_MATRIX_RO_ME": me}
	cfg, err := config.FromEnv(func(k string) string { return env[k] }).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	r := New(cfg, nil, OpenCache("", time.Now), slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	r.whoami = "@archive:m"
	r.me["@archive:m"] = true
	return r
}

func TestLocalpart(t *testing.T) {
	r := newRes(t, "")
	for in, want := range map[string]string{"@whatsapp_84901234567:m": "+84901234567", "@telegram_abc:m": "abc", "@alice:m": "alice"} {
		if got := r.Localpart(in); got != want {
			t.Errorf("Localpart(%q) = %q, want %q", in, got, want)
		}
	}
}

func intp(i int) *int { return &i }

func TestClassify(t *testing.T) {
	r := newRes(t, "@whatsapp_849000:m")
	jr := func(heroes []string, joined int, name string) matrix.JoinedRoom {
		var j matrix.JoinedRoom
		j.Summary.Heroes = heroes
		j.Summary.JoinedMemberCount = intp(joined)
		if name != "" {
			j.State.Events = []matrix.Event{{Type: "m.room.name", StateKey: new(string), Content: json.RawMessage(`{"name":"` + name + `"}`)}}
		}
		return j
	}
	cases := []struct {
		name           string
		j              matrix.JoinedRoom
		dm, hidden     bool
		network, title string
	}{
		{"wa dm with bot and owner puppet", jr([]string{"@whatsappbot:m", "@whatsapp_841:m", "@whatsapp_849000:m"}, 4, ""), true, false, "whatsapp", ""},
		{"wa group", jr([]string{"@whatsapp_841:m", "@whatsapp_842:m"}, 5, "Fam"), false, false, "whatsapp", "Fam"},
		{"bridge control room", jr([]string{"@whatsappbot:m"}, 2, ""), false, true, "matrix", ""},
		{"matrix dm", jr([]string{"@bob:m"}, 2, ""), true, false, "matrix", ""},
		{"three people but only one hero known", jr([]string{"@bob:m"}, 3, ""), false, false, "matrix", ""},
		// Named bridged rooms get no heroes; the unseen member of a
		// 3-member WhatsApp room is taken to be the bridge bot.
		{"named wa dm, bot unseen", jr([]string{"@whatsapp_841:m"}, 3, "Bap (WA)"), true, false, "whatsapp", "Bap (WA)"},
		{"named wa group, members unseen", jr([]string{"@whatsapp_841:m"}, 4, "Fam"), false, false, "whatsapp", "Fam"},
	}
	for _, c := range cases {
		e := r.classify("!x", c.j, nil)
		if e.DM != c.dm || e.Hidden != c.hidden || e.Network != c.network || (c.title != "" && e.Name != c.title) {
			t.Errorf("%s: got dm=%v hidden=%v net=%s name=%q", c.name, e.DM, e.Hidden, e.Network, e.Name)
		}
	}
}

func TestRosterCountsPeople(t *testing.T) {
	r := newRes(t, "@whatsapp_849000:m")
	// Only me synced (named room, last message mine): unknown, so a group.
	var j matrix.JoinedRoom
	j.Summary.JoinedMemberCount = intp(3)
	e := r.classify("!x", j, nil)
	if e.DM || !e.partial || e.Network != "matrix" {
		t.Fatalf("partial: %+v", e)
	}
	r.applyRoster(e, []string{"@archive:m", "@whatsapp_841:m", "@whatsappbot:m"})
	if !e.DM || e.Network != "whatsapp" || e.Members != 2 || e.partial {
		t.Errorf("roster: %+v", e)
	}
	// Owner puppet + Matrix account count once; bots not at all.
	r.applyRoster(e, []string{"@archive:m", "@whatsapp_849000:m", "@whatsappbot:m", "@whatsapp_841:m", "@whatsapp_842:m"})
	if e.DM || e.Members != 3 {
		t.Errorf("group roster: %+v", e)
	}
	// A later sync with the same size reuses the cached roster.
	prev := *e
	e = r.classify("!x", func() matrix.JoinedRoom { j.Summary.JoinedMemberCount = intp(5); return j }(), &prev)
	if e.partial || e.Members != 3 || e.Network != "whatsapp" {
		t.Errorf("reused roster: %+v", e)
	}
}
