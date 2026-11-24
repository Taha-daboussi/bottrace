package main

import (
	"slices"
	"testing"
)

func TestScreenChanges(t *testing.T) {
	base := func() msgState {
		return state("Title\nHi ⭐ $5\nFooter", []entity{{Type: "bold", Offset: 0, Length: 5}, {Type: "custom_emoji", Offset: 9, Length: 1, Emoji: "111"}},
			[]button{{Text: "A", Data: "a", Kind: "callback", Icon: "9"}, {Text: "B", Data: "b", Kind: "callback"}},
			[]button{{Text: "C", Data: "c", Kind: "callback"}})
	}
	cases := []struct {
		name string
		edit func(*msgState)
		live string
		want []string
	}{
		{"words", func(s *msgState) { s.Text = "Title\nHo ⭐ $5\nFooter" }, "", []string{"text"}},
		{"line added", func(s *msgState) { s.Text += "\nNew" }, "", []string{"line-added"}},
		{"line removed", func(s *msgState) { s.Text = "Title\nHi ⭐ $5" }, "", []string{"line-removed"}},
		{"format", func(s *msgState) { s.Entities[0].Type = "underline" }, "", []string{"format"}},
		{"emoji id", func(s *msgState) { s.Entities[1].Emoji = "222" }, "", []string{"emoji"}},
		{"live", func(s *msgState) { s.Text = "Title\nHi ⭐ $6\nFooter" }, "money", []string{"live"}},
		{"callback", func(s *msgState) { s.Rows[0][1].Data = "x" }, "", []string{"callback"}},
		{"style", func(s *msgState) { s.Rows[1][0].Style = "danger" }, "", []string{"style"}},
		{"icon", func(s *msgState) { s.Rows[0][0].Icon = "8" }, "", []string{"icon"}},
		{"rename", func(s *msgState) { s.Rows[1][0].Text = "D" }, "", []string{"label"}},
		{"order", func(s *msgState) { s.Rows[0][0], s.Rows[0][1] = s.Rows[0][1], s.Rows[0][0] }, "", []string{"order"}},
		{"added button changes nothing else", func(s *msgState) {
			s.Rows = append([][]button{{{Text: "New", Data: "n", Kind: "callback"}}}, s.Rows...)
		}, "", []string{"button-added"}},
		{"removed button", func(s *msgState) { s.Rows = s.Rows[:1] }, "", []string{"button-removed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			live, err := parseLive(c.live, nil)
			if err != nil {
				t.Fatal(err)
			}
			b := base()
			c.edit(&b)
			var got []string
			for _, ch := range screenChanges(base(), b, live) {
				got = append(got, ch.Kind)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestChangeLinesAreExact(t *testing.T) {
	a := state("Gas", nil, []button{{Text: "💸 Gas Limits", Data: "goto:gas", Kind: "callback"}})
	b := state("Gas", nil, []button{{Text: "💸 Gas Limits", Data: "goto:degen", Kind: "callback", Style: "danger"}})
	got := screenChanges(a, b, nil)
	if len(got) != 2 || got[0].Before[0] != "💸 Gas Limits → goto:gas" || got[1].After[0] != "💸 Gas Limits → goto:degen · danger" {
		t.Fatalf("got %+v", got)
	}
}

func TestDiffJSONHasChangeIds(t *testing.T) {
	a := &sessionBuilder{t: t}
	a.send("/start")
	a.draw("sendMessage", home(t, 10, "$5"))
	b := &sessionBuilder{t: t}
	b.send("/start")
	b.draw("sendMessage", msg(t, 20, "Dawn $5", nil, []btn{cb("Settings", "goto:other")}))
	res, _ := runDiff(t, a, b)
	ch := res.Steps[0].Screens[0].Changes
	if res.Ids != 1 || len(ch) != 1 || ch[0].ID != "1.1.1" || ch[0].Kind != "callback" {
		t.Fatalf("got %+v", res)
	}
}

func TestHelpTopic(t *testing.T) {
	for _, topic := range []string{"diff", "check", "snapshot", "pages"} {
		if err := helpTopic(topic); err != nil {
			t.Fatalf("%s: %v", topic, err)
		}
	}
	if helpTopic("nope") == nil {
		t.Fatal("an unknown topic must fail")
	}
}
