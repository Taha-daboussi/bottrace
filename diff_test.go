package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type btn map[string]any

func cb(text, data string) btn { return btn{"text": text, "callback_data": data} }

func msg(t *testing.T, id int, text string, ents []map[string]any, rows ...[]btn) map[string]any {
	m := map[string]any{"message_id": id, "chat": map[string]any{"id": 5}, "text": text}
	if ents != nil {
		m["entities"] = ents
	}
	if rows != nil {
		m["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	return roundTrip(t, m)
}

type sessionBuilder struct {
	t   *testing.T
	sec int
	evs []event
}

func (s *sessionBuilder) next() (int, time.Time) {
	s.sec++
	return len(s.evs) + 1, t0.Add(time.Duration(s.sec) * time.Second)
}

func (s *sessionBuilder) send(text string) {
	seq, t := s.next()
	s.evs = append(s.evs, event{Seq: seq, T: t, Kind: "tg_update", Bot: "b", Update: roundTrip(s.t, map[string]any{"update_id": seq, "message": map[string]any{"text": text}})})
}

func (s *sessionBuilder) tap(data string) {
	seq, t := s.next()
	s.evs = append(s.evs, event{Seq: seq, T: t, Kind: "tg_update", Bot: "b", Update: roundTrip(s.t, map[string]any{"update_id": seq, "callback_query": map[string]any{"data": data}})})
}

func (s *sessionBuilder) draw(method string, m map[string]any) {
	seq, t := s.next()
	s.evs = append(s.evs, event{Seq: seq, T: t, Kind: "tg_call", Bot: "b", Method: method, OK: true,
		Params: map[string]any{"chat_id": "5", "message_id": m["message_id"]}, Result: m})
}

func (s *sessionBuilder) write(dir string) string {
	s.t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		s.t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range s.evs {
		if err := enc.Encode(e); err != nil {
			s.t.Fatal(err)
		}
	}
	return dir
}

func runDiff(t *testing.T, a, b *sessionBuilder, flags ...string) (diffJSON, error) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	da, db, out := a.write(filepath.Join(root, "a")), b.write(filepath.Join(root, "b")), filepath.Join(root, "out")
	err := cmdDiff(append([]string{da, db, "-out", out}, flags...))
	var res diffJSON
	raw, rerr := os.ReadFile(filepath.Join(out, "diff.json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if jerr := json.Unmarshal(raw, &res); jerr != nil {
		t.Fatal(jerr)
	}
	return res, err
}

func home(t *testing.T, id int, balance string) map[string]any {
	return msg(t, id, "Dawn "+balance, nil, []btn{cb("Settings", "goto:settings")})
}

func settingsMsg(t *testing.T, id int) map[string]any {
	return msg(t, id, "Settings", nil, []btn{cb("Back", "back")})
}

// A screen that draws first and fills in a second later must not shift the later steps, as the old pairing by
// screen number did.
func TestDiffPairsByStepDespiteAnExtraEdit(t *testing.T) {
	a := &sessionBuilder{t: t}
	a.send("/start")
	a.draw("sendMessage", home(t, 10, "$5"))
	a.tap("goto:settings")
	a.draw("editMessageText", settingsMsg(t, 10))

	b := &sessionBuilder{t: t}
	b.send("/start")
	b.draw("sendMessage", home(t, 20, "loading"))
	b.draw("editMessageText", home(t, 20, "$5"))
	b.tap("goto:settings")
	b.draw("editMessageText", settingsMsg(t, 20))

	res, err := runDiff(t, a, b, "-fail")
	if err != nil {
		t.Fatalf("diff: %v (%+v)", err, res)
	}
	if res.By != "steps" || res.Same != 2 || res.Changed != 0 || res.Only != 0 {
		t.Fatalf("want 2 same steps, got %+v", res)
	}
}

func TestDiffAlignsAMissingStep(t *testing.T) {
	a := &sessionBuilder{t: t}
	a.send("/start")
	a.draw("sendMessage", home(t, 10, "$5"))
	a.tap("goto:settings")
	a.draw("editMessageText", settingsMsg(t, 10))

	b := &sessionBuilder{t: t}
	b.send("/start")
	b.draw("sendMessage", home(t, 20, "$5"))
	b.tap("goto:help")
	b.draw("sendMessage", msg(t, 21, "Help", nil))
	b.tap("goto:settings")
	b.draw("editMessageText", settingsMsg(t, 20))

	res, err := runDiff(t, a, b, "-fail")
	if !errors.Is(err, errChanged) {
		t.Fatalf("-fail should fail on a step that only B has, got %v", err)
	}
	if res.Same != 2 || res.Only != 1 || res.Changed != 0 {
		t.Fatalf("want 2 same and 1 only, got %+v", res)
	}
	if res.Steps[1].Status != "only" || res.Steps[1].InputB != "tap goto:help" {
		t.Fatalf("step 2: %+v", res.Steps[1])
	}
}

func TestDiffLiveValues(t *testing.T) {
	a := &sessionBuilder{t: t}
	a.send("/start")
	a.draw("sendMessage", home(t, 10, "$5.07"))
	b := &sessionBuilder{t: t}
	b.send("/start")
	b.draw("sendMessage", home(t, 20, "$5.06"))

	if res, err := runDiff(t, a, b, "-fail"); !errors.Is(err, errChanged) || res.Changed != 1 {
		t.Fatalf("without -live a price is a change: %v %+v", err, res)
	}
	res, err := runDiff(t, a, b, "-fail", "-live", "money")
	if err != nil || res.Live != 1 || res.Changed != 0 {
		t.Fatalf("with -live money a price is live: %v %+v", err, res)
	}
}

func state(text string, ents []entity, rows ...[]button) msgState {
	return msgState{Text: text, Entities: ents, Rows: rows}
}

func TestChangeKinds(t *testing.T) {
	base := func() msgState {
		return state("Hi ⭐ $5", []entity{{Type: "bold", Offset: 0, Length: 2}, {Type: "custom_emoji", Offset: 3, Length: 1, Emoji: "111"}},
			[]button{{Text: "A", Data: "a", Kind: "callback", Icon: "9"}, {Text: "B", Data: "b", Kind: "callback"}},
			[]button{{Text: "C", Data: "c", Kind: "callback"}})
	}
	cases := []struct {
		name string
		edit func(*msgState)
		live string
		want []string
	}{
		{"same", func(*msgState) {}, "", nil},
		{"text", func(s *msgState) { s.Text = "Ho ⭐ $5" }, "", []string{"text"}},
		{"live", func(s *msgState) { s.Text = "Hi ⭐ $6" }, "money", []string{"live"}},
		{"format", func(s *msgState) { s.Entities[0].Type = "underline" }, "", []string{"format"}},
		{"emoji", func(s *msgState) { s.Entities[1].Emoji = "222" }, "", []string{"emoji"}},
		{"emoji id is not a live number", func(s *msgState) { s.Entities[1].Emoji = "222" }, "number", []string{"emoji"}},
		{"link target is not a live number", func(s *msgState) {
			s.Entities = append(s.Entities, entity{Type: "text_link", Offset: 5, Length: 2, URL: "https://t.me/bot4"})
		}, "number", []string{"format"}},
		{"callback", func(s *msgState) { s.Rows[0][1].Data = "x" }, "", []string{"callback"}},
		{"style", func(s *msgState) { s.Rows[1][0].Style = "danger" }, "", []string{"style"}},
		{"icon", func(s *msgState) { s.Rows[0][0].Icon = "8" }, "", []string{"icon"}},
		{"order", func(s *msgState) { s.Rows[0][0], s.Rows[0][1] = s.Rows[0][1], s.Rows[0][0] }, "", []string{"order"}},
		{"layout", func(s *msgState) { s.Rows = [][]button{{s.Rows[0][0]}, {s.Rows[0][1], s.Rows[1][0]}} }, "", []string{"layout"}},
		{"rename hides no swap", func(s *msgState) {
			s.Rows[1][0].Text = "D"
			s.Rows[0][0], s.Rows[0][1] = s.Rows[0][1], s.Rows[0][0]
		}, "", []string{"buttons", "order"}},
		{"added button keeps the others matched", func(s *msgState) {
			s.Rows = append([][]button{{{Text: "New", Data: "n", Kind: "callback"}}}, s.Rows...)
		}, "", []string{"buttons"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			live, err := parseLive(c.live, nil)
			if err != nil {
				t.Fatal(err)
			}
			b := base()
			c.edit(&b)
			if got := changeKinds(base(), b, live); !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestApplyLineKeepsTargetsAndIds(t *testing.T) {
	live, _ := parseLive("number", nil)
	cases := map[string]string{
		"  1.2 Sell 50% → sell:12 · icon 77":        "  1.2 Sell #% → sell:12 · icon 77",
		"<emoji 5767>⚡</emoji> <bold>$5.07</bold>":  "<emoji 5767>⚡</emoji> <bold>$#</bold>",
		`<a https://t.me/bot4?start=9>Add</a> 10 x`: `<a https://t.me/bot4?start=9>Add</a> # x`,
		"[photo AQAD12]":       "[photo AQAD12]",
		"[ Sell 50% ] [ Buy ]": "[ Sell #% ] [ Buy ]",
	}
	for in, want := range cases {
		if got := live.applyLine(in); got != want {
			t.Errorf("applyLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFullGridShowsWhatTheTextHides(t *testing.T) {
	st := state("Hi", []entity{{Type: "underline", Offset: 0, Length: 2}},
		[]button{{Text: "Go", Data: "goto:x", Kind: "callback", Icon: "77", Style: "primary"}, {Text: "Web", URL: "https://a.b", Kind: "url"}})
	want := "<underline>Hi</underline>\n" +
		"────────────────────────────────\n" +
		"[ Go ] [ Web ]\n" +
		"buttons:\n" +
		"  1.1 Go → goto:x · icon 77 · primary\n" +
		"  1.2 Web → url https://a.b\n"
	if got := fullGrid(st); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
