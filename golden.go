package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// A golden file is the expected screens of a session, step by step. `snapshot` writes one from an approved
// session with every field set; a person or a model can also write one by hand, for example from a design
// image. A field that is left out is not checked: a design image shows labels and colors, not callback data.
type golden struct {
	From  string       `json:"from,omitempty"`
	Steps []goldenStep `json:"steps"`
}

type goldenStep struct {
	Input   string         `json:"input"`
	Note    string         `json:"note,omitempty"`
	Screens []goldenScreen `json:"screens"`
}

type goldenScreen struct {
	Text   string            `json:"text"`
	Markup *string           `json:"markup,omitempty"`
	Media  *string           `json:"media,omitempty"`
	Rows   *[][]goldenButton `json:"rows,omitempty"`
}

type goldenButton struct {
	Text  string  `json:"text"`
	Data  *string `json:"data,omitempty"`
	URL   *string `json:"url,omitempty"`
	Style *string `json:"style,omitempty"`
	Icon  *string `json:"icon,omitempty"`
}

func ptr(s string) *string { return &s }

func goldenFromState(st msgState) goldenScreen {
	g := goldenScreen{Text: st.Text, Markup: ptr(markup(st, true)), Media: ptr("")}
	if st.Media != nil {
		g.Media = ptr(st.Media.Kind)
	}
	rows := make([][]goldenButton, 0, len(st.Rows))
	for _, r := range st.Rows {
		row := make([]goldenButton, 0, len(r))
		for _, b := range r {
			gb := goldenButton{Text: b.Text, Style: ptr(b.Style), Icon: ptr(b.Icon)}
			switch {
			case b.Data != "":
				gb.Data = ptr(b.Data)
			case b.URL != "":
				gb.URL = ptr(b.URL)
			}
			row = append(row, gb)
		}
		rows = append(rows, row)
	}
	g.Rows = &rows
	return g
}

func cmdSnapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	out := fs.String("o", "", "write the golden file here (default: stdout)")
	only := fs.String("step", "", "only these steps, e.g. 2,5 (default: all)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("snapshot takes one session")
	}
	pos = append(pos, "")
	dir, err := resolveSession(pos[0])
	if err != nil {
		return err
	}
	events, err := readEvents(dir)
	if err != nil {
		return err
	}
	want := map[int]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s = strings.TrimSpace(s); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil {
				return fmt.Errorf("-step: %q is not a number", s)
			}
			want[n] = true
		}
	}
	g := golden{From: dir}
	for _, st := range buildSteps(events) {
		if len(want) > 0 && !want[st.N] {
			continue
		}
		gs := goldenStep{Input: st.Input, Screens: []goldenScreen{}}
		for _, sc := range st.Screens {
			gs.Screens = append(gs.Screens, goldenFromState(sc.State))
		}
		g.Steps = append(g.Steps, gs)
	}
	if len(g.Steps) == 0 {
		return errors.New("no steps to snapshot (bottrace ls shows the user actions of each session)")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(buf.Bytes())
		return err
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o600); err != nil {
		return err
	}
	fmt.Printf("%d steps → %s\n", len(g.Steps), *out)
	return nil
}

var emojiTag = regexp.MustCompile(`<emoji [^>]*>|</emoji>`)

// expectedRows is the golden keyboard with each field it leaves out taken from the button of the same label.
func expectedRows(g [][]goldenButton, actual [][]button, live liveMask) [][]button {
	byLabel := map[string][]button{}
	for _, r := range actual {
		for _, b := range r {
			k := live.apply(b.Text)
			byLabel[k] = append(byLabel[k], b)
		}
	}
	out := make([][]button, 0, len(g))
	for _, r := range g {
		row := make([]button, 0, len(r))
		for _, gb := range r {
			var m button
			k := live.apply(gb.Text)
			if q := byLabel[k]; len(q) > 0 {
				m, byLabel[k] = q[0], q[1:]
			}
			b := button{Text: gb.Text, Kind: m.Kind, Data: m.Data, URL: m.URL, Style: m.Style, Icon: m.Icon}
			switch {
			case gb.Data != nil:
				b.Kind, b.Data, b.URL = "callback", *gb.Data, ""
			case gb.URL != nil:
				b.Data, b.URL = "", *gb.URL
				if b.Kind == "" || b.Kind == "callback" {
					b.Kind = "url"
				}
			}
			if gb.Style != nil {
				b.Style = *gb.Style
			}
			if gb.Icon != nil {
				b.Icon = *gb.Icon
			}
			row = append(row, b)
		}
		out = append(out, row)
	}
	return out
}

// checkKinds says how a screen differs from its golden screen, for the fields the golden screen sets.
func checkKinds(g goldenScreen, st msgState, live liveMask) []string {
	set := map[string]bool{}
	textOK := live.apply(g.Text) == live.apply(st.Text)
	switch {
	case !textOK:
		set["text"] = true
	case g.Text != st.Text:
		set["live"] = true
	}
	if g.Markup != nil && textOK {
		want := live.apply(*g.Markup)
		withEmoji := emojiTag.MatchString(want)
		if live.apply(markup(st, withEmoji)) != want {
			if live.apply(markup(st, false)) != emojiTag.ReplaceAllString(want, "") {
				set["format"] = true
			} else {
				set["emoji"] = true
			}
		}
	}
	if g.Media != nil {
		got := ""
		if st.Media != nil {
			got = st.Media.Kind
		}
		if got != *g.Media {
			set["media"] = true
		}
	}
	if g.Rows != nil {
		for _, k := range buttonKinds(expectedRows(*g.Rows, st.Rows, live), st.Rows, live) {
			set[k] = true
		}
	}
	var out []string
	for _, k := range kindOrder {
		if set[k] {
			out = append(out, k)
		}
	}
	if len(out) > 1 && out[len(out)-1] == "live" {
		out = out[:len(out)-1]
	}
	return out
}

var errCheckFailed = errors.New("the session does not match the golden file")

type checkResult struct {
	Golden  string         `json:"golden"`
	Session string         `json:"session"`
	Passed  bool           `json:"passed"`
	Steps   []checkStepOut `json:"steps"`
}

type checkStepOut struct {
	Input   string           `json:"input"`
	Status  string           `json:"status"` // pass, fail, missing
	Screens []checkScreenOut `json:"screens"`
}

type checkScreenOut struct {
	Status string   `json:"status"` // pass, live, fail, missing
	Kinds  []string `json:"kinds,omitempty"`
	Diff   []string `json:"diff,omitempty"`
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	liveKinds := fs.String("live", "", "values that change on their own: money,percent,time,number")
	jsonOut := fs.String("json", "", "also write the result as JSON here")
	var liveRe stringList
	fs.Var(&liveRe, "live-re", "a regexp of more live values (repeat the flag for more)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("check takes a golden file and a session (default: the latest)")
	}
	if len(pos) == 1 {
		pos = append(pos, "")
	}
	live, err := parseLive(*liveKinds, liveRe)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(pos[0])
	if err != nil {
		return err
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("%s: %w", pos[0], err)
	}
	dir, err := resolveSession(pos[1])
	if err != nil {
		return err
	}
	events, err := readEvents(dir)
	if err != nil {
		return err
	}
	res := runCheck(g, buildSteps(events), live)
	res.Golden, res.Session = pos[0], dir
	printCheck(res)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(res, "", " ")
		if err := os.WriteFile(*jsonOut, b, 0o600); err != nil {
			return err
		}
	}
	if !res.Passed {
		return errCheckFailed
	}
	return nil
}

// runCheck lines up the golden steps with the session steps by input. A session step that the golden file does
// not list is not checked; a golden step that the session does not have fails. In a step, the golden screens pair
// with the session's screens in order, and extra session screens are not checked.
func runCheck(g golden, steps []step, live liveMask) checkResult {
	gsteps := make([]step, len(g.Steps))
	for i, s := range g.Steps {
		gsteps[i] = step{N: i, Input: s.Input}
	}
	res := checkResult{Passed: true}
	for _, ab := range alignSteps(gsteps, steps) {
		if ab[0] == nil {
			continue
		}
		gs := g.Steps[ab[0].N]
		out := checkStepOut{Input: gs.Input, Status: "pass", Screens: []checkScreenOut{}}
		if ab[1] == nil {
			out.Status, res.Passed = "missing", false
			res.Steps = append(res.Steps, out)
			continue
		}
		for i, gsc := range gs.Screens {
			so := checkScreenOut{Status: "pass"}
			if i >= len(ab[1].Screens) {
				so.Status = "missing"
			} else {
				st := ab[1].Screens[i].State
				so.Kinds = checkKinds(gsc, st, live)
				switch {
				case len(so.Kinds) == 1 && so.Kinds[0] == "live":
					so.Status = "live"
				case len(so.Kinds) > 0:
					so.Status = "fail"
					want := goldenText(gsc)
					got := goldenText(likeGolden(goldenFromState(st), gsc))
					wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
					for _, l := range lineDiffBy(wl, gl, maskAll(wl, live), maskAll(gl, live)) {
						if !strings.HasPrefix(l, "  ") {
							so.Diff = append(so.Diff, l)
						}
					}
				}
			}
			if so.Status == "fail" || so.Status == "missing" {
				out.Status, res.Passed = "fail", false
			}
			out.Screens = append(out.Screens, so)
		}
		res.Steps = append(res.Steps, out)
	}
	return res
}

// likeGolden drops from got each field that want does not check, so a failed check shows only real differences.
func likeGolden(got, want goldenScreen) goldenScreen {
	switch {
	case want.Markup == nil:
		got.Markup = nil
	case !emojiTag.MatchString(*want.Markup):
		got.Markup = ptr(emojiTag.ReplaceAllString(*got.Markup, ""))
	}
	if want.Media == nil {
		got.Media = nil
	}
	if want.Rows == nil || got.Rows == nil {
		got.Rows = want.Rows
		return got
	}
	var data, url, style, icon bool
	for _, r := range *want.Rows {
		for _, b := range r {
			data, url = data || b.Data != nil, url || b.URL != nil
			style, icon = style || b.Style != nil, icon || b.Icon != nil
		}
	}
	for _, r := range *got.Rows {
		for i := range r {
			if !data {
				r[i].Data = nil
			}
			if !url {
				r[i].URL = nil
			}
			if !style {
				r[i].Style = nil
			}
			if !icon {
				r[i].Icon = nil
			}
		}
	}
	return got
}

// goldenText is a golden screen as lines, for the diff of a failed check: only the fields it sets.
func goldenText(g goldenScreen) string {
	var b strings.Builder
	if g.Markup != nil {
		b.WriteString(*g.Markup)
	} else {
		b.WriteString(g.Text)
	}
	b.WriteString("\n")
	if g.Media != nil && *g.Media != "" {
		fmt.Fprintf(&b, "[%s]\n", *g.Media)
	}
	if g.Rows == nil {
		return b.String()
	}
	for r, row := range *g.Rows {
		for c, gb := range row {
			fmt.Fprintf(&b, "%d.%d %s", r+1, c+1, gb.Text)
			if gb.Data != nil {
				b.WriteString(" → " + *gb.Data)
			}
			if gb.URL != nil {
				b.WriteString(" → " + *gb.URL)
			}
			if gb.Icon != nil && *gb.Icon != "" {
				b.WriteString(" · icon " + *gb.Icon)
			}
			if gb.Style != nil && *gb.Style != "" {
				b.WriteString(" · " + *gb.Style)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func printCheck(r checkResult) {
	for _, s := range r.Steps {
		fmt.Printf("%-7s %s\n", s.Status, s.Input)
		for i, sc := range s.Screens {
			if sc.Status == "pass" {
				continue
			}
			fmt.Printf("  screen %d: %s %s\n", i+1, sc.Status, strings.Join(sc.Kinds, ", "))
			for _, l := range sc.Diff {
				fmt.Printf("    %s\n", l)
			}
		}
	}
	if r.Passed {
		fmt.Println("PASS")
	} else {
		fmt.Println("FAIL")
	}
}
