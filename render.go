package main

import (
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const pageCSS = `
:root{--bg:#0e1621;--panel:#17212b;--bubble:#182533;--text:#e6ebef;--muted:#7f91a4;--link:#6ab3f3;--btn:#2b5278;--btnText:#fff;--err:#e5534b;--warn:#d4a72c;--ok:#4fae4e;--user:#2b5278;--code:#1f2c3a;--line:#243241}
@media (prefers-color-scheme: light){:root:not([data-theme=dark]){--bg:#c8d9a5;--panel:#fff;--bubble:#fff;--text:#000;--muted:#708499;--link:#168acd;--btn:rgba(0,0,0,.18);--btnText:#fff;--user:#effdde;--code:#f2f2f2;--line:#e2e8ee}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);font:15px/1.35 -apple-system,"Segoe UI",Roboto,"Noto Sans","Noto Color Emoji",sans-serif}
header.top{position:sticky;top:0;z-index:2;background:var(--panel);padding:10px 16px;border-bottom:1px solid var(--line);display:flex;gap:16px;align-items:center;flex-wrap:wrap}
header.top h1{font-size:16px;margin:0}
header.top a{color:var(--link);text-decoration:none}
main{max-width:980px;margin:0 auto;padding:16px}
.msg{max-width:440px}
.bubble{background:var(--bubble);border-radius:12px 12px 12px 4px;overflow:hidden;box-shadow:0 1px 1px rgba(0,0,0,.2)}
.bubble img,.bubble .media{display:block;width:100%;max-height:340px;object-fit:cover;background:#000}
.bubble .media{padding:40px 0;text-align:center;color:var(--muted);background:var(--code)}
.bubble .text{padding:7px 10px 8px;white-space:pre-wrap;word-wrap:break-word}
.text a,.text .link{color:var(--link);text-decoration:none}
.text code,.text pre{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:13.5px;background:var(--code);border-radius:4px;padding:0 3px}
.text pre{display:block;padding:6px 8px;margin:2px 0;white-space:pre-wrap}
.text blockquote{margin:2px 0;padding:2px 8px;border-left:3px solid var(--link);background:rgba(106,179,243,.08);border-radius:4px}
.text .spoiler{background:var(--muted);color:transparent;border-radius:3px}
.text .spoiler:hover{color:inherit;background:transparent}
.text .raw{font-family:ui-monospace,Menlo,Consolas,monospace;font-size:13px;color:var(--warn)}
.kb{margin-top:3px;display:flex;flex-direction:column;gap:3px}
.kb .row{display:flex;gap:3px}
.kb .b{flex:1 1 0;min-width:0;background:var(--btn);color:var(--btnText);border-radius:6px;padding:8px 6px;text-align:center;font-size:14px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;position:relative}
.kb .b.primary{background:#3390ec}.kb .b.success{background:#3fa34d}.kb .b.danger{background:#d9493f}
.kb .b .icon{display:inline-block;width:18px;height:18px;vertical-align:-4px;margin-right:5px;object-fit:contain}
.text .ce{display:inline-block;width:1.25em;height:1.25em;vertical-align:-0.25em;color:transparent;background-size:contain;background-repeat:no-repeat;background-position:center;overflow:hidden}
.kb .b .k{position:absolute;right:4px;top:2px;font-size:10px;opacity:.7}
.flags{margin:6px 0 0;padding:0;list-style:none;font-size:13px}
.flags li{padding:2px 0}.flags .error{color:var(--err)}.flags .warn{color:var(--warn)}
.meta{color:var(--muted);font-size:12.5px;margin:0 0 4px}
.meta a{color:var(--link)}
.card{background:rgba(0,0,0,.12);border-radius:10px;padding:10px;margin:0 0 14px}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:14px}
.summary{background:var(--panel);border-radius:10px;padding:10px 14px;margin:0 0 16px}
.summary a{color:var(--link)}
.summary .error{color:var(--err)}.summary .warn{color:var(--warn)}
pre.plain{background:var(--panel);padding:10px;border-radius:8px;overflow:auto;font-size:13px}
.tl{display:flex;flex-direction:column;gap:8px}
.tl .item{display:flex;gap:10px;align-items:flex-start}
.tl .t{flex:0 0 74px;color:var(--muted);font:12px ui-monospace,Menlo,Consolas,monospace;padding-top:3px;text-align:right}
.tl .body{flex:1;min-width:0}
.tl .user{align-self:flex-end;background:var(--user);border-radius:12px 12px 4px 12px;padding:6px 10px;max-width:440px;margin-left:auto}
.tl .action .body{display:flex;flex-direction:column}
.tl .line{font:12.5px ui-monospace,Menlo,Consolas,monospace;color:var(--muted);word-break:break-all}
.tl .out .line{padding-left:14px;border-left:2px solid var(--line)}
.tl .line .m{color:var(--text)}
.tl .line a{color:var(--link)}
.tl .line.err,.tl .err .line{color:var(--err)}
.tl .toast{display:inline-block;background:rgba(0,0,0,.55);color:#fff;border-radius:14px;padding:4px 12px;font-size:13px}
.tl .lat{color:var(--ok);font-size:12px}
.tl .lat.slow{color:var(--warn)}
.filters label{margin-right:12px;font-size:13px;color:var(--muted)}
body.no-out .out,body.no-tg .tg,body.no-misc .misc,body.no-usr .usr{display:none}
.tl .usr:not(.action) .body{border-left:3px solid var(--link);padding-left:8px}
body.err-only .item:not(.err){display:none}
body:not(.show-bg) .bg{display:none}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:14px;align-items:start}
.pair{border-radius:10px;padding:10px;margin-bottom:14px;background:rgba(0,0,0,.12)}
.pair.changed{outline:2px solid var(--warn)}
.pair.only{outline:2px dashed var(--muted)}
.diffline{font:12.5px ui-monospace,Menlo,Consolas,monospace;white-space:pre-wrap;margin:8px 0 0}
.diffline .del{color:var(--err)}.diffline .add{color:var(--ok)}
@media (max-width:640px){.cols{grid-template-columns:1fr}.tl .t{flex-basis:56px}}
`

var funcs = template.FuncMap{
	"msg":   msgHTML,
	"since": func(a, b time.Time) string { return fmt.Sprintf("+%.3fs", b.Sub(a).Seconds()) },
	"clock": func(t time.Time) string { return t.Format("15:04:05.000") },
	"pad":   func(n int) string { return fmt.Sprintf("%03d", n) },
}

// msgHTML draws one message: picture, text, keyboard. prefix leads from the page to the session folder.
func msgHTML(st msgState, prefix string) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="msg"><div class="bubble">`)
	if st.Media != nil {
		if st.Media.Path != "" && st.Media.Kind != "video" && st.Media.Kind != "animation" && st.Media.Kind != "document" {
			fmt.Fprintf(&b, `<img src="%s" alt="%s">`, html.EscapeString(prefix+st.Media.Path), st.Media.Kind)
		} else if st.Media.Path != "" {
			fmt.Fprintf(&b, `<video src="%s" muted autoplay loop playsinline style="width:100%%;display:block"></video>`, html.EscapeString(prefix+st.Media.Path))
		} else {
			fmt.Fprintf(&b, `<div class="media">%s</div>`, st.Media.Kind)
		}
	}
	if st.Text != "" {
		b.WriteString(`<div class="text">` + textHTML(st, prefix) + `</div>`)
	}
	b.WriteString(`</div>`)
	if len(st.Rows) > 0 {
		b.WriteString(`<div class="kb">`)
		for _, row := range st.Rows {
			b.WriteString(`<div class="row">`)
			for _, btn := range row {
				mark := ""
				switch btn.Kind {
				case "url", "web_app":
					mark = `<span class="k">↗</span>`
				case "switch_inline":
					mark = `<span class="k">⤴</span>`
				}
				title := btn.Data
				if title == "" {
					title = btn.URL
				}
				icon := ""
				if p := st.Emoji["emoji:"+btn.Icon]; btn.Icon != "" && p != "" {
					icon = `<img class="icon" src="` + html.EscapeString(prefix+p) + `" alt="">`
				} else if btn.Icon != "" {
					icon = `<span class="icon">✦</span>`
				}
				fmt.Fprintf(&b, `<div class="b %s" title="%s">%s%s%s</div>`, html.EscapeString(btn.Style), html.EscapeString(title), icon, html.EscapeString(btn.Text), mark)
			}
			b.WriteString(`</div>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

var screenTmpl = template.Must(template.New("screen").Funcs(funcs).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Screen {{pad .S.N}}</title><style>{{.CSS}}</style></head><body>
<header class="top"><h1>Screen {{.S.N}} of {{.Total}}</h1>
<a href="../index.html">all screens</a><a href="../timeline.html#seq{{.S.Seq}}">timeline</a>
{{if gt .S.N 1}}<a href="{{pad .Prev}}.html">◀ previous</a>{{end}}{{if lt .S.N .Total}}<a href="{{pad .Next}}.html">next ▶</a>{{end}}
<a href="{{pad .S.N}}.txt">text</a></header>
<main><p class="meta">{{.S.Method}} · message {{.S.State.ID}} · chat {{.S.State.Chat}} · {{clock .S.T}} · {{.S.Ms}} ms{{if .S.Bot}} · @{{.S.Bot}}{{end}}</p>
{{msg .S.State "../"}}
{{if .S.Flags}}<ul class="flags">{{range .S.Flags}}<li class="{{.Level}}">{{if eq .Level "error"}}✖{{else}}⚠{{end}} {{.Text}}</li>{{end}}</ul>{{end}}
<h3>Text</h3><pre class="plain">{{.Text}}</pre></main></body></html>`))

var indexTmpl = template.Must(template.New("index").Funcs(funcs).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Bot screens</title><style>{{.CSS}}</style></head><body>
<header class="top"><h1>{{.Name}}</h1><a href="timeline.html">timeline</a><a href="screens.txt">all screens as text</a></header>
<main><div class="summary">{{len .Screens}} screens · {{.Errors}} errors · {{.Warns}} warnings
{{range .Screens}}{{$s := .}}{{range .Flags}}<div class="{{.Level}}"><a href="screens/{{pad $s.N}}.html">#{{$s.N}}</a> {{.Text}}</div>{{end}}{{end}}</div>
<div class="grid">{{range .Screens}}<div class="card"><p class="meta"><a href="screens/{{pad .N}}.html">#{{.N}}</a> {{.Method}} · msg {{.State.ID}} · {{clock .T}}{{if .Flags}} · <span style="color:var(--warn)">{{len .Flags}} flag(s)</span>{{end}}</p>{{msg .State ""}}</div>{{end}}</div>
</main></body></html>`))

type renderOut struct {
	Index, Timeline, Text string
	Screens               int
	Errors, Warns         int
}

func renderSession(dir string) (renderOut, error) {
	events, err := readEvents(dir)
	if err != nil {
		return renderOut{}, err
	}
	screens, _ := buildScreens(events)
	sdir := filepath.Join(dir, "screens")
	_ = os.RemoveAll(sdir)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		return renderOut{}, err
	}
	out := renderOut{Index: filepath.Join(dir, "index.html"), Timeline: filepath.Join(dir, "timeline.html"), Text: filepath.Join(dir, "screens.txt"), Screens: len(screens)}
	var all strings.Builder
	for i, sc := range screens {
		txt := screenText(sc)
		all.WriteString(txt + "\n")
		for _, f := range sc.Flags {
			if f.Level == "error" {
				out.Errors++
			} else {
				out.Warns++
			}
		}
		if err := os.WriteFile(filepath.Join(sdir, fmt.Sprintf("%03d.txt", sc.N)), []byte(txt), 0o600); err != nil {
			return out, err
		}
		f, err := os.Create(filepath.Join(sdir, fmt.Sprintf("%03d.html", sc.N)))
		if err != nil {
			return out, err
		}
		err = screenTmpl.Execute(f, map[string]any{"S": sc, "Total": len(screens), "Prev": i, "Next": i + 2, "Text": txt, "CSS": template.CSS(pageCSS)})
		f.Close()
		if err != nil {
			return out, err
		}
	}
	if err := os.WriteFile(out.Text, []byte(all.String()), 0o600); err != nil {
		return out, err
	}
	f, err := os.Create(out.Index)
	if err != nil {
		return out, err
	}
	err = indexTmpl.Execute(f, map[string]any{"Name": filepath.Base(dir), "Screens": screens, "Errors": out.Errors, "Warns": out.Warns, "CSS": template.CSS(pageCSS)})
	f.Close()
	if err != nil {
		return out, err
	}
	return out, writeTimeline(dir, events, out.Timeline)
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir, err := resolveSession(fs.Arg(0))
	if err != nil {
		return err
	}
	out, err := renderSession(dir)
	if err != nil {
		return err
	}
	printRender(out)
	if out.Screens == 0 {
		return errors.New("no screens in this session")
	}
	return nil
}

func printRender(out renderOut) {
	fmt.Printf("%d screens, %d errors, %d warnings\n  screens:  %s\n  timeline: %s\n  text:     %s\n", out.Screens, out.Errors, out.Warns, out.Index, out.Timeline, out.Text)
}
