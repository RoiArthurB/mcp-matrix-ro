package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RoiArthurB/mxread/internal/matrix"
)

func ev(id, sender string, ts int64, content string) matrix.Event {
	return matrix.Event{Type: "m.room.message", EventID: id, Sender: sender, OriginTS: ts, Content: json.RawMessage(content)}
}

func conv() *Converter {
	return &Converter{MaxBody: 20, Name: func(_, u string) string { return strings.TrimPrefix(strings.SplitN(u, ":", 2)[0], "@") }}
}

func TestStripFallback(t *testing.T) {
	rest, sender, q := stripFallback("> <@bob:x> first line\n> second line\n\nmy answer\nline 2")
	if rest != "my answer\nline 2" || sender != "@bob:x" || q != "first line second line" {
		t.Fatalf("got %q %q %q", rest, sender, q)
	}
	rest, sender, _ = stripFallback("> quoting without a header\n\nok")
	if rest != "ok" || sender != "" {
		t.Fatalf("got %q %q", rest, sender)
	}
	if rest, _, _ := stripFallback("no fallback"); rest != "no fallback" {
		t.Fatal(rest)
	}
}

func TestTruncateAndBlankLines(t *testing.T) {
	c := conv()
	if got := c.clean("a\n\n\n \nb"); got != "a\nb" {
		t.Errorf("blank runs: %q", got)
	}
	if got := Truncate("ăăăăăăăăăă", 4); got != "ăăăă…[+6 chars]" {
		t.Errorf("truncate: %q", got)
	}
	if got := Truncate("short", 0); got != "short" {
		t.Errorf("no limit: %q", got)
	}
}

func TestConvertEditsRepliesAndKinds(t *testing.T) {
	c := conv()
	evs := []matrix.Event{
		ev("$1", "@ann:x", 1, `{"msgtype":"m.text","body":"hello"}`),
		ev("$2", "@bob:x", 2, `{"msgtype":"m.text","body":"> <@ann:x> hello\n\nhi!","m.relates_to":{"m.in_reply_to":{"event_id":"$1"}}}`),
		ev("$3", "@bob:x", 3, `{"msgtype":"m.text","body":" * hi there","m.new_content":{"msgtype":"m.text","body":"hi there"},"m.relates_to":{"rel_type":"m.replace","event_id":"$2"}}`),
		ev("$4", "@eve:x", 4, `{"msgtype":"m.text","body":" * hacked","m.new_content":{"msgtype":"m.text","body":"hacked"},"m.relates_to":{"rel_type":"m.replace","event_id":"$1"}}`),
		ev("$5", "@bob:x", 5, `{"msgtype":"m.text","body":"> <@zed:x> something old\n\nreplying","m.relates_to":{"m.in_reply_to":{"event_id":"$old"}}}`),
		ev("$6", "@bob:x", 6, `{"msgtype":"m.audio","body":"a.ogg","info":{"duration":65400}}`),
		ev("$7", "@bob:x", 7, `{"msgtype":"m.audio","body":"a.ogg","info":{"duration":65400},"org.matrix.msc3245.voice":{}}`),
		ev("$8", "@bob:x", 8, `{}`),
		{Type: "m.room.encrypted", EventID: "$9", Sender: "@bob:x", OriginTS: 9, Content: json.RawMessage(`{"ciphertext":"x"}`)},
		ev("$1", "@ann:x", 1, `{"msgtype":"m.text","body":"duplicate"}`),
	}
	msgs := c.Convert("!r", evs)
	var got []string
	for _, m := range msgs {
		var b strings.Builder
		writeMessage(&b, m)
		got = append(got, b.String())
	}
	want := []string{
		"ann: hello", // edit by another user ignored
		"bob: [↩ ann: hello] hi there (edited)", // reply resolved in batch, edit applied
		"bob: [↩ zed: something old] replying",  // parent from fallback
		"bob: [audio: a.ogg]",
		"bob: [voice message 1:05]",
		"bob: [deleted]",
		"bob: [encrypted message]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
