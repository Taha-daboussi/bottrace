package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"os"
	"path/filepath"
	"strings"
)

type diffPair struct {
	N       int
	Step    string
	A, B    *screen
	Changed bool
	Live    bool
	Kinds   []string
	Lines   template.HTML
	PrefixA string
	PrefixB string
}

var diffTmpl = template.Must(template.New("diff").Funcs(funcs).Funcs(template.FuncMap{"join": strings.Join}).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Screen diff</title><style>{{.CSS}}</style></head><body>
<header class="top"><h1>{{.A}} ↔ {{.B}}</h1><label class="filters"><input type="checkbox" id="only"> changed only</label></header>
<main><div class="summary">{{.Same}} same · {{.Changed}} changed · {{.Only}} only on one side{{if .Live}} · {{.Live}} with live values only{{end}} · paired by {{.By}}</div>
<div class="cols"><h3>A: {{.A}}</h3><h3>B: {{.B}}</h3></div>
{{range .Pairs}}<div class="pair{{if .Changed}} changed{{end}}{{if or (not .A) (not .B)}} only{{end}}"><p class="meta">{{.Step}}{{if .Kinds}} · {{join .Kinds ", "}}{{end}}</p>
<div class="cols"><div>{{if .A}}<p class="meta">{{.A.Method}} · msg {{.A.State.ID}}</p>{{msg .A.State .PrefixA}}{{else}}<p class="meta">no screen</p>{{end}}</div>
<div>{{if .B}}<p class="meta">{{.B.Method}} · msg {{.B.State.ID}}</p>{{msg .B.State .PrefixB}}{{else}}<p class="meta">no screen</p>{{end}}</div></div>
{{if or .Changed .Live}}<div class="diffline">{{.Lines}}</div>{{end}}</div>{{end}}
</main><script>document.getElementById('only').onchange=e=>document.querySelectorAll('.pair:not(.changed):not(.only)').forEach(p=>p.style.display=e.target.checked?'none':'')</script></body></html>`))

// parseArgs parses flags before, between and after the positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return pos, nil
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// errChanged makes `diff -fail` exit with 1 when the sessions differ.
var errChanged = errors.New("the sessions differ")

type diffJSONScreen struct {
	Status  string       `json:"status"`
	Kinds   []string     `json:"kinds,omitempty"`
	MsgA    int          `json:"msg_a,omitempty"`
	MsgB    int          `json:"msg_b,omitempty"`
	Changes []diffChange `json:"changes,omitempty"`
}

type diffJSONStep struct {
	Step    string           `json:"step"`
	InputA  string           `json:"input_a,omitempty"`
	InputB  string           `json:"input_b,omitempty"`
	Status  string           `json:"status"`
	Kinds   []string         `json:"kinds,omitempty"`
	Changes []diffChange     `json:"changes,omitempty"`
	Screens []diffJSONScreen `json:"screens"`
}

type diffJSON struct {
	A       string         `json:"a"`
	B       string         `json:"b"`
	By      string         `json:"paired_by"`
	Same    int            `json:"same"`
	Changed int            `json:"changed"`
	Only    int            `json:"only"`
	Live    int            `json:"live"`
	Ids     int            `json:"change_count"`
	Steps   []diffJSONStep `json:"steps"`
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	out := fs.String("out", "", "output folder (default: the diffs folder)")
	by := fs.String("by", "auto", "pair screens by: steps (each user action), screens (screen number), auto (steps when both sessions have actions)")
	liveKinds := fs.String("live", "", "values that change on their own, as live and not as a change: money,percent,time,number")
	fail := fs.Bool("fail", false, "exit with 1 when a screen changed or is only on one side")
	var liveRe stringList
	fs.Var(&liveRe, "live-re", "a regexp of more live values (repeat the flag for more)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("diff takes two sessions")
	}
	live, err := parseLive(*liveKinds, liveRe)
	if err != nil {
		return err
	}
	da, err := resolveSession(pos[0])
	if err != nil {
		return err
	}
	db, err := resolveSession(pos[1])
	if err != nil {
		return err
	}
	ea, err := readEvents(da)
	if err != nil {
		return err
	}
	eb, err := readEvents(db)
	if err != nil {
		return err
	}
	mode := *by
	switch mode {
	case "auto":
		mode = "screens"
		if len(stepMarkers(ea)) > 0 && len(stepMarkers(eb)) > 0 {
			mode = "steps"
		}
	case "steps", "screens":
	default:
		return fmt.Errorf("-by: %q is not steps, screens or auto", mode)
	}
	dir := *out
	if dir == "" {
		dir = filepath.Join(dataDir(), "diffs", filepath.Base(da)+"__"+filepath.Base(db))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	relA, _ := filepath.Rel(dir, da)
	relB, _ := filepath.Rel(dir, db)
	res := diffJSON{A: filepath.Base(da), B: filepath.Base(db), By: mode}
	var pairs []diffPair
	var txt strings.Builder
	for si, sp := range pairSteps(ea, eb, mode) {
		js := diffJSONStep{Step: sp.label, InputA: sp.inA, InputB: sp.inB, Screens: []diffJSONScreen{}}
		kinds := map[string]bool{}
		stepStatus := "same"
		if sp.oneSide && len(sp.screens) == 0 {
			stepStatus = "only"
			res.Only++
			kind, in := "step-added", sp.inB
			if sp.inA != "" {
				kind, in = "step-removed", sp.inA
			}
			c := diffChange{ID: fmt.Sprintf("%d.0.1", si+1), Kind: kind}
			if kind == "step-added" {
				c.After = []string{in}
			} else {
				c.Before = []string{in}
			}
			js.Changes = []diffChange{c}
			res.Ids++
			fmt.Fprintf(&txt, "=== %s (only, no screen)\n» %s %s: %s\n", sp.label, c.ID, c.Kind, in)
		}
		for sci, sc := range sp.screens {
			p := diffPair{Step: sp.label, A: sc[0], B: sc[1], PrefixA: relA + "/", PrefixB: relB + "/"}
			p.N = len(pairs) + 1
			jsc := diffJSONScreen{}
			var la, lb []string
			if p.A != nil {
				la = strings.Split(strings.TrimRight(fullGrid(p.A.State), "\n"), "\n")
				jsc.MsgA = p.A.State.ID
			}
			if p.B != nil {
				lb = strings.Split(strings.TrimRight(fullGrid(p.B.State), "\n"), "\n")
				jsc.MsgB = p.B.State.ID
			}
			switch {
			case p.A == nil:
				p.Changed, jsc.Status = true, "only"
				res.Only++
				jsc.Changes = []diffChange{{Kind: "screen-added", After: lb}}
			case p.B == nil:
				p.Changed, jsc.Status = true, "only"
				res.Only++
				jsc.Changes = []diffChange{{Kind: "screen-removed", Before: la}}
			default:
				p.Kinds = changeKinds(p.A.State, p.B.State, live)
				if len(p.Kinds) > 0 {
					jsc.Changes = screenChanges(p.A.State, p.B.State, live)
				}
				switch {
				case len(p.Kinds) == 0:
					jsc.Status = "same"
					res.Same++
				case len(p.Kinds) == 1 && p.Kinds[0] == "live":
					p.Live, jsc.Status = true, "live"
					res.Live++
				default:
					p.Changed, jsc.Status = true, "changed"
					res.Changed++
				}
			}
			jsc.Kinds = p.Kinds
			for _, k := range p.Kinds {
				kinds[k] = true
			}
			stepStatus = worse(stepStatus, jsc.Status)
			if p.Changed || p.Live {
				lines := lineDiffBy(la, lb, maskAll(la, live), maskAll(lb, live))
				var hb strings.Builder
				fmt.Fprintf(&txt, "=== %s (%s)\n", sp.label, strings.Join(append([]string{jsc.Status}, p.Kinds...), ", "))
				for _, l := range lines {
					txt.WriteString(l + "\n")
					cls := ""
					switch l[0] {
					case '-':
						cls = "del"
					case '+':
						cls = "add"
					case '~':
						cls = "live"
					}
					fmt.Fprintf(&hb, `<div class="%s">%s</div>`, cls, html.EscapeString(l))
				}
				p.Lines = template.HTML(hb.String())
			}
			for k := range jsc.Changes {
				c := &jsc.Changes[k]
				c.ID = fmt.Sprintf("%d.%d.%d", si+1, sci+1, k+1)
				res.Ids++
				fmt.Fprintf(&txt, "» %s %s: %s  ⇒  %s\n", c.ID, c.Kind, strings.Join(c.Before, " / "), strings.Join(c.After, " / "))
			}
			js.Screens = append(js.Screens, jsc)
			pairs = append(pairs, p)
		}
		js.Status = stepStatus
		for _, k := range kindOrder {
			if kinds[k] {
				js.Kinds = append(js.Kinds, k)
			}
		}
		res.Steps = append(res.Steps, js)
	}
	page := filepath.Join(dir, "index.html")
	f, err := os.Create(page)
	if err != nil {
		return err
	}
	err = diffTmpl.Execute(f, map[string]any{"A": res.A, "B": res.B, "Pairs": pairs, "Same": res.Same, "Changed": res.Changed, "Only": res.Only, "Live": res.Live, "By": mode, "CSS": template.CSS(pageCSS)})
	f.Close()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "diff.txt"), []byte(txt.String()), 0o600); err != nil {
		return err
	}
	jb, _ := json.MarshalIndent(res, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "diff.json"), jb, 0o600); err != nil {
		return err
	}
	fmt.Printf("%d same, %d changed, %d only on one side, %d live only, %d changes (paired by %s)\n  page: %s\n  text: %s\n  json: %s\n",
		res.Same, res.Changed, res.Only, res.Live, res.Ids, mode, page, filepath.Join(dir, "diff.txt"), filepath.Join(dir, "diff.json"))
	if *fail && (res.Changed > 0 || res.Only > 0) {
		return errChanged
	}
	return nil
}

func maskAll(lines []string, live liveMask) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = live.applyLine(l)
	}
	return out
}

func worse(a, b string) string {
	rank := map[string]int{"same": 0, "live": 1, "changed": 2, "only": 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

type stepPair struct {
	label    string
	inA, inB string
	oneSide  bool
	screens  [][2]*screen
}

// pairSteps lines up the screens of two sessions: by step (the screens each user action left, the steps matched
// by their input), or by screen number for sessions with no actions.
func pairSteps(ea, eb []event, mode string) []stepPair {
	if mode == "screens" {
		sa, _ := buildScreens(ea)
		sb, _ := buildScreens(eb)
		var out []stepPair
		for i := range max(len(sa), len(sb)) {
			var p [2]*screen
			if i < len(sa) {
				p[0] = &sa[i]
			}
			if i < len(sb) {
				p[1] = &sb[i]
			}
			out = append(out, stepPair{label: fmt.Sprintf("screen %d", i+1), screens: [][2]*screen{p}})
		}
		return out
	}
	var out []stepPair
	for _, ab := range alignSteps(buildSteps(ea), buildSteps(eb)) {
		sp := stepPair{}
		var na, nb int
		switch {
		case ab[0] != nil && ab[1] != nil:
			sp.inA, sp.inB = ab[0].Input, ab[1].Input
			sp.label = fmt.Sprintf("step %d · %s", ab[1].N, ab[1].Input)
			na, nb = len(ab[0].Screens), len(ab[1].Screens)
		case ab[0] != nil:
			sp.inA, sp.oneSide = ab[0].Input, true
			sp.label = fmt.Sprintf("step %d of A only · %s", ab[0].N, ab[0].Input)
			na = len(ab[0].Screens)
		default:
			sp.inB, sp.oneSide = ab[1].Input, true
			sp.label = fmt.Sprintf("step %d of B only · %s", ab[1].N, ab[1].Input)
			nb = len(ab[1].Screens)
		}
		for i := range max(na, nb) {
			var p [2]*screen
			if i < na {
				p[0] = &ab[0].Screens[i]
			}
			if i < nb {
				p[1] = &ab[1].Screens[i]
			}
			sp.screens = append(sp.screens, p)
		}
		out = append(out, sp)
	}
	return out
}
