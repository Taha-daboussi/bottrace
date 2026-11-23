package main

import (
	"fmt"
	"html"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type tlItem struct {
	Seq    int
	T      time.Time
	Class  string
	Body   template.HTML
	Repeat int
}

type window struct{ from, to time.Time }

// actionWindows spans each user action: from the action to the bot's last call that follows it with gaps of at
// most 3 s, plus half a second. Outside calls in no window are background work: feeds, pollers, price loops.
func actionWindows(sorted []event) []window {
	var out []window
	for i, e := range sorted {
		if e.Kind != "tg_update" {
			continue
		}
		w := window{from: e.T, to: e.T.Add(20 * time.Second)}
		last := e.T
		seen := false
		for _, n := range sorted[i+1:] {
			if n.Kind == "tg_update" {
				break
			}
			if n.Kind != "tg_call" {
				continue
			}
			if (seen && n.T.Sub(last) > 3*time.Second) || (!seen && n.T.Sub(last) > 20*time.Second) {
				break
			}
			seen = true
			last = n.T
		}
		if seen {
			w.to = last
		}
		w.to = w.to.Add(500 * time.Millisecond)
		out = append(out, w)
	}
	return out
}

func inWindow(ws []window, t time.Time) bool {
	for _, w := range ws {
		if !t.Before(w.from) && !t.After(w.to) {
			return true
		}
	}
	return false
}

func outKey(e event) string {
	if e.HTTP == nil {
		return e.Kind
	}
	return e.Kind + " " + e.HTTP.Method + " " + e.HTTP.Host + " " + e.HTTP.Path + " " + strings.Join(e.HTTP.RPC, ",")
}

var timelineTmpl = template.Must(template.New("tl").Funcs(funcs).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Bot timeline</title><style>{{.CSS}}</style></head><body>
<header class="top"><h1>{{.Name}}</h1><a href="index.html">screens</a>
<span class="filters"><label><input type="checkbox" checked data-hide="no-tg"> Telegram</label><label><input type="checkbox" checked data-hide="no-usr"> account side</label><label><input type="checkbox" checked data-hide="no-out"> outside calls</label><label><input type="checkbox" data-show="show-bg"> background calls ({{.Background}})</label><label><input type="checkbox" checked data-hide="no-misc"> other</label><label><input type="checkbox" data-show="err-only"> errors only</label></span></header>
<main><p class="meta">{{.Actions}} user actions · {{.Calls}} bot calls · {{.Outside}} outside calls ({{.Background}} in the background, hidden by default) · started {{clock .Start}}</p>
<div class="tl">{{range .Items}}<div class="item {{.Class}}" id="seq{{.Seq}}"><div class="t">{{since $.Start .T}}</div><div class="body">{{.Body}}</div></div>{{end}}</div></main>
<script>document.querySelectorAll('[data-hide]').forEach(c=>c.onchange=()=>document.body.classList.toggle(c.dataset.hide,!c.checked));document.querySelectorAll('[data-show]').forEach(c=>c.onchange=()=>document.body.classList.toggle(c.dataset.show,c.checked));</script>
</body></html>`))

func writeTimeline(dir string, events []event, path string) error {
	sorted := append([]event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].T.Before(sorted[j].T) })
	screens, media := buildScreens(events)
	bySeq := map[int]screen{}
	for _, s := range screens {
		bySeq[s.Seq] = s
	}
	windows := actionWindows(sorted)
	var items []tlItem
	actions, calls, outside, background := 0, 0, 0, 0
	var lastOut string
	for i, e := range sorted {
		it := tlItem{Seq: e.Seq, T: e.T}
		switch e.Kind {
		case "tg_update":
			actions++
			it.Class = "tg action"
			it.Body = actionHTML(e, latency(sorted, i))
		case "tg_call":
			calls++
			it.Class = "tg"
			if e.Error != "" && !strings.Contains(e.Error, "message is not modified") {
				it.Class += " err"
			}
			it.Body = callHTML(e, bySeq)
		case "http", "socket":
			outside++
			it.Class = "out"
			if !inWindow(windows, e.T) {
				it.Class += " bg"
				background++
			}
			failed := e.HTTP != nil && (e.HTTP.Error != "" || e.HTTP.Status >= 400)
			if failed {
				it.Class += " err"
			}
			key := outKey(e)
			if !failed && len(e.HTTP.Tx) == 0 && key == lastOut && len(items) > 0 && items[len(items)-1].Class == it.Class {
				prev := &items[len(items)-1]
				prev.Repeat++
				prev.Body = outsideHTML(e, prev.Repeat)
				continue
			}
			lastOut = key
			it.Repeat = 1
			it.Body = outsideHTML(e, 1)
		case "tg_user":
			it.Class = "usr"
			if e.Method == "tap" || e.Method == "send" {
				it.Class += " action"
			}
			if e.Error != "" {
				it.Class += " err"
			}
			it.Body = accountHTML(e, bySeq, media)
		case "media":
			continue
		default:
			it.Class = "misc"
			it.Body = miscHTML(e)
		}
		if e.Kind != "http" && e.Kind != "socket" {
			lastOut = ""
		}
		items = append(items, it)
	}
	start := time.Now()
	if len(sorted) > 0 {
		start = sorted[0].T
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return timelineTmpl.Execute(f, map[string]any{"Name": filepath.Base(dir), "Items": items, "Start": start, "Actions": actions, "Calls": calls, "Outside": outside, "Background": background, "CSS": template.CSS(pageCSS)})
}

// latency is the time from a user action to the bot's first call after it.
func latency(sorted []event, i int) time.Duration {
	for _, e := range sorted[i+1:] {
		if e.Kind == "tg_call" && e.Bot == sorted[i].Bot {
			return e.T.Sub(sorted[i].T)
		}
		if e.Kind == "tg_update" {
			break
		}
	}
	return -1
}

func actionHTML(e event, lat time.Duration) template.HTML {
	u, _ := e.Update.(map[string]any)
	var who, what string
	switch {
	case u["callback_query"] != nil:
		q, _ := u["callback_query"].(map[string]any)
		data := str(q["data"])
		label := data
		if m, ok := q["message"].(map[string]any); ok {
			for _, row := range fromMessage(m).Rows {
				for _, b := range row {
					if b.Data == data {
						label = b.Text
					}
				}
			}
		}
		who = userName(q["from"])
		what = fmt.Sprintf(`👆 tapped <b>%s</b> <span class="line">%s</span>`, html.EscapeString(label), html.EscapeString(data))
	case u["message"] != nil:
		m, _ := u["message"].(map[string]any)
		who = userName(m["from"])
		what = "💬 " + html.EscapeString(str(m["text"]))
		if str(m["text"]) == "" {
			what = "💬 (message without text)"
		}
	case u["inline_query"] != nil:
		q, _ := u["inline_query"].(map[string]any)
		who = userName(q["from"])
		what = "🔎 inline query " + html.EscapeString(str(q["query"]))
	default:
		what = "update"
	}
	var b strings.Builder
	b.WriteString(`<div class="user">` + what + `</div><div class="line" style="text-align:right">` + html.EscapeString(who))
	if e.Injected {
		b.WriteString(" · sent by bottrace")
	}
	if lat >= 0 {
		cls := "lat"
		if lat > 1500*time.Millisecond {
			cls += " slow"
		}
		fmt.Fprintf(&b, ` · <span class="%s">bot answered in %d ms</span>`, cls, lat.Milliseconds())
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// accountHTML draws an event of the user client: a message as the account got it, or a real send or tap.
func accountHTML(e event, bySeq map[int]screen, media map[string]string) template.HTML {
	params, _ := e.Params.(map[string]any)
	var b strings.Builder
	switch e.Method {
	case "tap":
		fmt.Fprintf(&b, `<div class="user">👆 tapped <b>%s</b> <span class="line">%s</span></div>`, html.EscapeString(str(params["label"])), html.EscapeString(str(params["data"])))
		cls := "toast"
		if e.Error != "" {
			cls = "err"
		}
		fmt.Fprintf(&b, `<div class="line" style="text-align:right">account @%s · real tap through Telegram · %d ms · <span class="%s">%s</span></div>`, html.EscapeString(e.Bot), e.Ms, cls, html.EscapeString(answerText(e)))
	case "send":
		fmt.Fprintf(&b, `<div class="user">💬 %s</div><div class="line" style="text-align:right">account → @%s · sent through Telegram · %d ms`, html.EscapeString(str(params["text"])), html.EscapeString(e.Bot), e.Ms)
		if e.Error != "" {
			fmt.Fprintf(&b, ` · <span class="err">%s</span>`, html.EscapeString(e.Error))
		}
		b.WriteString(`</div>`)
	case "delete":
		fmt.Fprintf(&b, `<div class="line">account: @%s deleted message %v</div>`, html.EscapeString(e.Bot), params["message_ids"])
	default:
		m, _ := e.Result.(map[string]any)
		st := fromMessage(m)
		if st.Media != nil {
			st.Media.Path = media[st.Media.UniqueID]
		}
		st.Emoji = media
		meta, _ := m["user_meta"].(map[string]any)
		head := fmt.Sprintf(`<div class="line"><span class="m">%s</span> as the account got it · @%s · msg %d`, html.EscapeString(e.Method), html.EscapeString(e.Bot), st.ID)
		if meta["out"] == true {
			head = fmt.Sprintf(`<div class="line"><span class="m">%s</span> typed by the account in another app · @%s · msg %d`, html.EscapeString(e.Method), html.EscapeString(e.Bot), st.ID)
		}
		b.WriteString(head)
		if sc, ok := bySeq[e.Seq]; ok {
			fmt.Fprintf(&b, ` · <a href="screens/%03d.html">screen %d</a>`, sc.N, sc.N)
			st = sc.State
		}
		b.WriteString(`</div>`)
		b.WriteString(string(msgHTML(st, "")))
	}
	return template.HTML(b.String())
}

func userName(v any) string {
	m, _ := v.(map[string]any)
	if u := str(m["username"]); u != "" {
		return "@" + u
	}
	return str(m["first_name"])
}

func callHTML(e event, bySeq map[int]screen) template.HTML {
	params, _ := e.Params.(map[string]any)
	var b strings.Builder
	head := fmt.Sprintf(`<div class="line"><span class="m">%s</span> %d ms`, html.EscapeString(e.Method), e.Ms)
	if e.Local {
		head += " · answered by bottrace"
	}
	if e.Note != "" && !e.Local {
		head += " · " + html.EscapeString(e.Note)
	}
	if e.Error != "" {
		head += ` · <span class="err">` + html.EscapeString(e.Error) + `</span>`
	}
	b.WriteString(head)
	if sc, ok := bySeq[e.Seq]; ok {
		fmt.Fprintf(&b, ` · <a href="screens/%03d.html">screen %d</a></div>`, sc.N, sc.N)
		b.WriteString(string(msgHTML(sc.State, "")))
		if len(sc.Flags) > 0 {
			b.WriteString(`<ul class="flags">`)
			for _, f := range sc.Flags {
				fmt.Fprintf(&b, `<li class="%s">%s</li>`, f.Level, html.EscapeString(f.Text))
			}
			b.WriteString(`</ul>`)
		}
		return template.HTML(b.String())
	}
	b.WriteString(`</div>`)
	switch e.Method {
	case "answerCallbackQuery":
		if t := str(params["text"]); t != "" {
			kind := "toast"
			if str(params["show_alert"]) == "true" {
				kind = "alert"
			}
			fmt.Fprintf(&b, `<span class="toast">%s: %s</span>`, kind, html.EscapeString(t))
		}
	case "deleteMessage":
		fmt.Fprintf(&b, `<div class="line">deleted message %v</div>`, params["message_id"])
	case "sendChatAction":
		fmt.Fprintf(&b, `<div class="line">%v</div>`, html.EscapeString(fmt.Sprint(params["action"])))
	}
	return template.HTML(b.String())
}

func outsideHTML(e event, repeat int) template.HTML {
	h := e.HTTP
	if h == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="line"><span class="m">%s %s</span>%s`, html.EscapeString(h.Method), html.EscapeString(h.Host), html.EscapeString(h.Path))
	if len(h.RPC) > 0 {
		fmt.Fprintf(&b, ` · <span class="m">%s</span>`, html.EscapeString(strings.Join(h.RPC, ", ")))
	}
	if e.Kind == "socket" {
		b.WriteString(" · websocket")
	}
	if h.Status != 0 {
		fmt.Fprintf(&b, " · %d", h.Status)
	}
	fmt.Fprintf(&b, " · %d ms", e.Ms)
	if repeat > 1 {
		fmt.Fprintf(&b, " · ×%d", repeat)
	}
	if h.Hidden {
		b.WriteString(" · body not recorded")
	}
	for _, tx := range h.Tx {
		fmt.Fprintf(&b, ` · tx %s`, txLink(tx, h.Host))
	}
	if h.Error != "" {
		fmt.Fprintf(&b, ` · <span class="err">%s</span>`, html.EscapeString(h.Error))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

func txLink(tx, host string) string {
	t := html.EscapeString(tx)
	switch {
	case !strings.HasPrefix(tx, "0x"):
		return `<a href="https://solscan.io/tx/` + t + `" target="_blank" rel="noopener">` + t + `</a>`
	case strings.Contains(host, "bsc") || strings.Contains(host, "bnb") || strings.Contains(host, "binance"):
		return `<a href="https://bscscan.com/tx/` + t + `" target="_blank" rel="noopener">` + t + `</a>`
	}
	return t
}

func miscHTML(e event) template.HTML {
	switch e.Kind {
	case "tunnel":
		host := ""
		if e.HTTP != nil {
			host = e.HTTP.Host
			if e.HTTP.Error != "" {
				host += " · " + e.HTTP.Error
			}
		}
		return template.HTML(fmt.Sprintf(`<div class="line">tunnel %s (%s, not opened) · %d ms</div>`, html.EscapeString(host), html.EscapeString(e.Note), e.Ms))
	case "tg_file":
		if e.HTTP != nil {
			return template.HTML(fmt.Sprintf(`<div class="line">file download %s · %d · %d ms</div>`, html.EscapeString(e.HTTP.Path), e.HTTP.Status, e.Ms))
		}
	}
	return template.HTML(`<div class="line">` + html.EscapeString(e.Kind+": "+e.Note) + `</div>`)
}
