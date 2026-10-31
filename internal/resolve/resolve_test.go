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
	env := map[string]string{"MATRIX_HOMESERVER": "https://m", "MATRIX_ACCESS_TOKEN": "t", "MXREAD_ME": me}
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
	}
	for _, c := range cases {
		e := r.classify("!x", c.j, nil)
		if e.DM != c.dm || e.Hidden != c.hidden || e.Network != c.network || (c.title != "" && e.Name != c.title) {
			t.Errorf("%s: got dm=%v hidden=%v net=%s name=%q", c.name, e.DM, e.Hidden, e.Network, e.Name)
		}
	}
}
