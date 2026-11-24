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
	Emoji string       `json:"emoji,omitempty"` // "loose": emoji in text and labels are not checked (for a file from an image)
	Steps []goldenStep `json:"steps"`
}

type goldenStep struct {
	Input   string         `json:"input"`
	Nth     int            `json:"nth,omitempty"` // the nth time this input comes in the session (1 = the first); 0: matched in order
	Note    string         `json:"note,omitempty"`
	Screens []goldenScreen `json:"screens"`
}

type goldenScreen struct {
	Emoji  string            `json:"emoji,omitempty"` // "loose" or "exact"; empty: the file's setting
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

// snapshotSteps is the golden form of each step of a session (of the steps in want, when it lists any).
func snapshotSteps(events []event, want map[int]bool) []goldenStep {
	var out []goldenStep
	seen := map[string]int{}
	for _, st := range buildSteps(events) {
		seen[st.Input]++
		if len(want) > 0 && !want[st.N] {
			continue
		}
		gs := goldenStep{Input: st.Input, Nth: seen[st.Input], Screens: []goldenScreen{}}
		for _, sc := range st.Screens {
			gs.Screens = append(gs.Screens, goldenFromState(sc.State))
		}
		out = append(out, gs)
	}
	return out
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
	g := golden{From: dir, Steps: snapshotSteps(events, want)}
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

const emojiChar = `[\x{1F000}-\x{1FAFF}\x{2300}-\x{23FF}\x{2600}-\x{27BF}\x{2B00}-\x{2BFF}\x{3030}\x{303D}\x{3297}\x{3299}]`

// looseEmoji drops emoji (with their variation selector, skin tone and joiner parts) and the space after each:
// an image shows a picture, not which code point, and not whether it is in the label or a custom icon.
var looseEmoji = liveMask{
	{regexp.MustCompile(`(` + emojiChar + `[\x{FE0F}\x{20E3}]*(\x{200D}` + emojiChar + `[\x{FE0F}]*)*|\x{FE0F}) ?`), ""},
}

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
		want := live.applyMarkup(*g.Markup)
		withEmoji := emojiTag.MatchString(want)
		if live.applyMarkup(markup(st, withEmoji)) != want {
			if live.applyMarkup(markup(st, false)) != emojiTag.ReplaceAllString(want, "") {
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
	Status string   `json:"status"` // pass, live, fail, missing, extra
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

// runCheck finds the session step of each golden step: the nth step with its input, or, with no nth, the next one
// in order by input. A session step that the golden file does not list is not checked; a golden step that the
// session does not have fails. In a step, the golden screens pair with the session's screens in order, and a
// screen that the golden step does not list fails as extra.
func runCheck(g golden, steps []step, live liveMask) checkResult {
	matched := make([]*step, len(g.Steps))
	byInput := map[string][]*step{}
	for i := range steps {
		byInput[steps[i].Input] = append(byInput[steps[i].Input], &steps[i])
	}
	var inOrder []step
	for i, s := range g.Steps {
		if s.Nth == 0 {
			inOrder = append(inOrder, step{N: i, Input: s.Input})
		} else if occ := byInput[s.Input]; s.Nth <= len(occ) {
			matched[i] = occ[s.Nth-1]
		}
	}
	for _, ab := range alignSteps(inOrder, steps) {
		if ab[0] != nil {
			matched[ab[0].N] = ab[1]
		}
	}
	res := checkResult{Passed: true}
	for gi, gs := range g.Steps {
		got := matched[gi]
		out := checkStepOut{Input: gs.Input, Status: "pass", Screens: []checkScreenOut{}}
		if got == nil {
			out.Status, res.Passed = "missing", false
			res.Steps = append(res.Steps, out)
			continue
		}
		for i, gsc := range gs.Screens {
			so := checkScreenOut{Status: "pass"}
			sl := live
			if gsc.Emoji == "loose" || (gsc.Emoji == "" && g.Emoji == "loose") {
				sl = append(append(liveMask{}, looseEmoji...), live...)
			}
			if i >= len(got.Screens) {
				so.Status = "missing"
			} else {
				st := got.Screens[i].State
				so.Kinds = checkKinds(gsc, st, sl)
				switch {
				case len(so.Kinds) == 1 && so.Kinds[0] == "live":
					so.Status = "live"
				case len(so.Kinds) > 0:
					so.Status = "fail"
					want := goldenText(gsc)
					got := goldenText(likeGolden(goldenFromState(st), gsc))
					wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
					for _, l := range lineDiffBy(wl, gl, maskAll(wl, sl), maskAll(gl, sl)) {
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
		for _, sc := range got.Screens[min(len(gs.Screens), len(got.Screens)):] {
			first, _, _ := strings.Cut(sc.State.Text, "\n")
			out.Screens = append(out.Screens, checkScreenOut{Status: "extra", Diff: []string{"+ " + first}})
			out.Status, res.Passed = "fail", false
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
			fmt.Fprintf(&b, "  %d.%d %s", r+1, c+1, gb.Text)
			switch {
			case gb.Data != nil:
				b.WriteString(" → " + *gb.Data)
			case gb.URL != nil:
				b.WriteString(" → " + *gb.URL)
			default:
				b.WriteString(" →")
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
			switch sc.Status {
			case "pass":
				continue
			case "live":
				fmt.Printf("  screen %d: live (only values that -live or loose emoji leave out differ)\n", i+1)
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
