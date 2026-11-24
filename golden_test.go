package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func goldenOf(t *testing.T, s *sessionBuilder) golden {
	t.Helper()
	b, err := json.Marshal(golden{Steps: snapshotSteps(s.evs, nil)})
	if err != nil {
		t.Fatal(err)
	}
	var back golden
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	return back
}

func guardScreen(t *testing.T, id int, style, data string) map[string]any {
	b := cb("Gas Limits", data)
	if style != "" {
		b["style"] = style
	}
	b["icon_custom_emoji_id"] = "55"
	return msg(t, id, "🛡 Guardrails\nTap an option.", []map[string]any{{"type": "bold", "offset": 3, "length": 10}}, []btn{b}, []btn{cb("Back", "back")})
}

func guardSession(t *testing.T, style, data string) *sessionBuilder {
	s := &sessionBuilder{t: t}
	s.send("/start")
	s.draw("sendMessage", home(t, 10, "$5"))
	s.tap("goto:set_guards")
	s.draw("editMessageText", guardScreen(t, 10, style, data))
	return s
}

func TestSnapshotThenCheck(t *testing.T) {
	g := goldenOf(t, guardSession(t, "", "goto:gas"))
	if res := runCheck(g, buildSteps(guardSession(t, "", "goto:gas").evs), nil); !res.Passed {
		t.Fatalf("same session must pass: %+v", res)
	}
	res := runCheck(g, buildSteps(guardSession(t, "danger", "goto:degen").evs), nil)
	if res.Passed || res.Steps[1].Status != "fail" || !slices.Equal(res.Steps[1].Screens[0].Kinds, []string{"callback", "style"}) {
		t.Fatalf("want a callback and style fail on step 2: %+v", res)
	}
}

func TestCheckSkipsFieldsTheGoldenLeavesOut(t *testing.T) {
	g := golden{Steps: []goldenStep{{Input: "tap goto:set_guards", Screens: []goldenScreen{{
		Text: "🛡 Guardrails\nTap an option.",
		Rows: &[][]goldenButton{{{Text: "Gas Limits", Style: ptr("")}}, {{Text: "Back"}}},
	}}}}}
	if res := runCheck(g, buildSteps(guardSession(t, "", "goto:other").evs), nil); !res.Passed {
		t.Fatalf("no data and no icon in the golden: a new target must not fail: %+v", res)
	}
	if res := runCheck(g, buildSteps(guardSession(t, "success", "goto:gas").evs), nil); res.Passed {
		t.Fatal(`"style": "" in the golden must fail on a colored button`)
	}
}

func TestCheckFailsOnAMissingStep(t *testing.T) {
	g := golden{Steps: []goldenStep{{Input: "tap goto:help", Screens: []goldenScreen{{Text: "Help"}}}}}
	res := runCheck(g, buildSteps(guardSession(t, "", "goto:gas").evs), nil)
	if res.Passed || res.Steps[0].Status != "missing" {
		t.Fatalf("want missing: %+v", res)
	}
}

func TestLooseEmoji(t *testing.T) {
	g := golden{Emoji: "loose", Steps: []goldenStep{{Input: "tap goto:set_guards", Screens: []goldenScreen{{
		Text:   "🛡️ Guardrails\nTap an option.",
		Markup: ptr("🛡️ <bold>Guardrails</bold>\nTap an option."),
		Rows:   &[][]goldenButton{{{Text: "💸 Gas Limits"}}, {{Text: "⬅️ Back"}}},
	}}}}}
	if res := runCheck(g, buildSteps(guardSession(t, "", "goto:gas").evs), nil); !res.Passed {
		t.Fatalf("loose: a variation selector and an icon written into the label must pass: %+v", res)
	}
	g.Steps[0].Screens[0].Text = "🛡️ Guardrail\nTap an option."
	if res := runCheck(g, buildSteps(guardSession(t, "", "goto:gas").evs), nil); res.Passed {
		t.Fatal("loose still checks the words")
	}
	g.Emoji = ""
	g.Steps[0].Screens[0].Text = "🛡️ Guardrails\nTap an option."
	if res := runCheck(g, buildSteps(guardSession(t, "", "goto:gas").evs), nil); res.Passed {
		t.Fatal("exact: 🛡️ is not 🛡")
	}
}

func TestBarePageHasOnlyTheMessage(t *testing.T) {
	dir := guardSession(t, "", "goto:gas").write(t.TempDir())
	if _, err := renderSession(dir); err != nil {
		t.Fatal(err)
	}
	page := readFile(t, dir+"/screens/002.bare.html")
	for _, bad := range []string{"message 10", " ms", "chat 5", "previous", "timeline"} {
		if strings.Contains(page, bad) {
			t.Fatalf("bare page has %q", bad)
		}
	}
	if !strings.Contains(page, "Guardrails") || !strings.Contains(page, "Gas Limits") {
		t.Fatal("bare page misses the message")
	}
}

func TestCheckFailsOnAnExtraScreen(t *testing.T) {
	g := goldenOf(t, guardSession(t, "", "goto:gas"))
	s := guardSession(t, "", "goto:gas")
	s.draw("sendMessage", msg(t, 11, "New message", nil))
	res := runCheck(g, buildSteps(s.evs), nil)
	if res.Passed || res.Steps[1].Status != "fail" || res.Steps[1].Screens[1].Status != "extra" {
		t.Fatalf("want an extra screen fail: %+v", res)
	}
}

func TestNthPicksTheRightRepeat(t *testing.T) {
	s := &sessionBuilder{t: t}
	s.send("/start")
	s.draw("sendMessage", home(t, 10, "first"))
	s.send("/start")
	s.draw("sendMessage", home(t, 11, "second"))
	g := goldenOf(t, s)
	g.Steps = g.Steps[1:]
	if g.Steps[0].Nth != 2 {
		t.Fatalf("snapshot should write nth 2, got %d", g.Steps[0].Nth)
	}
	if res := runCheck(g, buildSteps(s.evs), nil); !res.Passed {
		t.Fatalf("the second /start must be checked against the second: %+v", res)
	}
}
