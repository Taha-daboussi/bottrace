package main

import (
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
	A, B    *screen
	Changed bool
	Lines   template.HTML
	PrefixA string
	PrefixB string
}

var diffTmpl = template.Must(template.New("diff").Funcs(funcs).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Screen diff</title><style>{{.CSS}}</style></head><body>
<header class="top"><h1>{{.A}} ↔ {{.B}}</h1><label class="filters"><input type="checkbox" id="only"> changed only</label></header>
<main><div class="summary">{{.Same}} same · {{.Changed}} changed · {{.Only}} only on one side</div>
<div class="cols"><h3>A: {{.A}}</h3><h3>B: {{.B}}</h3></div>
{{range .Pairs}}<div class="pair{{if .Changed}} changed{{end}}{{if or (not .A) (not .B)}} only{{end}}"><p class="meta">screen {{.N}}{{if .Changed}} · changed{{end}}</p>
<div class="cols"><div>{{if .A}}<p class="meta">{{.A.Method}} · msg {{.A.State.ID}}</p>{{msg .A.State .PrefixA}}{{else}}<p class="meta">no screen</p>{{end}}</div>
<div>{{if .B}}<p class="meta">{{.B.Method}} · msg {{.B.State.ID}}</p>{{msg .B.State .PrefixB}}{{else}}<p class="meta">no screen</p>{{end}}</div></div>
{{if .Changed}}<div class="diffline">{{.Lines}}</div>{{end}}</div>{{end}}
</main><script>document.getElementById('only').onchange=e=>document.querySelectorAll('.pair:not(.changed):not(.only)').forEach(p=>p.style.display=e.target.checked?'none':'')</script></body></html>`))

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	out := fs.String("out", "", "output folder (default: the diffs folder)")
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != 2 {
		return fmt.Errorf("diff takes two sessions")
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
	sa, _ := buildScreens(ea)
	sb, _ := buildScreens(eb)
	dir := *out
	if dir == "" {
		dir = filepath.Join(dataDir(), "diffs", filepath.Base(da)+"__"+filepath.Base(db))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	relA, _ := filepath.Rel(dir, da)
	relB, _ := filepath.Rel(dir, db)
	var pairs []diffPair
	var txt strings.Builder
	same, changed, only := 0, 0, 0
	for i := range max(len(sa), len(sb)) {
		p := diffPair{N: i + 1, PrefixA: relA + "/", PrefixB: relB + "/"}
		var ta, tb string
		if i < len(sa) {
			p.A = &sa[i]
			ta = textGrid(sa[i].State, false)
		}
		if i < len(sb) {
			p.B = &sb[i]
			tb = textGrid(sb[i].State, false)
		}
		switch {
		case p.A == nil || p.B == nil:
			only++
			p.Changed = true
		case ta != tb:
			changed++
			p.Changed = true
		default:
			same++
		}
		if p.Changed {
			lines := lineDiff(strings.Split(strings.TrimRight(ta, "\n"), "\n"), strings.Split(strings.TrimRight(tb, "\n"), "\n"))
			var hb strings.Builder
			fmt.Fprintf(&txt, "=== screen %d\n", i+1)
			for _, l := range lines {
				txt.WriteString(l + "\n")
				cls := ""
				switch l[0] {
				case '-':
					cls = "del"
				case '+':
					cls = "add"
				}
				fmt.Fprintf(&hb, `<div class="%s">%s</div>`, cls, html.EscapeString(l))
			}
			p.Lines = template.HTML(hb.String())
		}
		pairs = append(pairs, p)
	}
	page := filepath.Join(dir, "index.html")
	f, err := os.Create(page)
	if err != nil {
		return err
	}
	err = diffTmpl.Execute(f, map[string]any{"A": filepath.Base(da), "B": filepath.Base(db), "Pairs": pairs, "Same": same, "Changed": changed, "Only": only, "CSS": template.CSS(pageCSS)})
	f.Close()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "diff.txt"), []byte(txt.String()), 0o600); err != nil {
		return err
	}
	fmt.Printf("%d same, %d changed, %d only on one side\n  page: %s\n  text: %s\n", same, changed, only, page, filepath.Join(dir, "diff.txt"))
	return nil
}

// lineDiff is a plain LCS line diff: " " same, "-" only in a, "+" only in b.
func lineDiff(a, b []string) []string {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, "  "+a[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, "- "+a[i])
			i++
		default:
			out = append(out, "+ "+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "- "+a[i])
	}
	for ; j < m; j++ {
		out = append(out, "+ "+b[j])
	}
	return out
}
