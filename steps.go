package main

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// step is one user action (a message or a tap) and the screens it left: the final state of each message
// that changed after it, in the order they were first touched. Step 0 holds what came before the first action.
type step struct {
	N       int
	Input   string
	T       time.Time
	Screens []screen
}

type marker struct {
	T     time.Time
	Seq   int
	Input string
}

// stepMarkers are the user actions of a session. The bot side sees every action as an update, also a tap in
// Telegram Web; a session with no bot side falls back to the account's own send and tap.
func stepMarkers(events []event) []marker {
	var bot, acct []marker
	for _, e := range events {
		switch {
		case e.Kind == "tg_update":
			bot = append(bot, marker{T: e.T, Seq: e.Seq, Input: updateInput(e.Update)})
		case e.Kind == "tg_user" && (e.Method == "send" || e.Method == "tap"):
			acct = append(acct, marker{T: e.T, Seq: e.Seq, Input: accountInput(e)})
		}
	}
	out := bot
	if len(out) == 0 {
		out = acct
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}

func updateInput(u any) string {
	m, _ := u.(map[string]any)
	if cq, ok := m["callback_query"].(map[string]any); ok {
		return "tap " + str(cq["data"])
	}
	for _, k := range []string{"message", "edited_message"} {
		if msg, ok := m[k].(map[string]any); ok {
			if t := str(msg["text"]); t != "" {
				return "send " + t
			}
			for _, kind := range []string{"photo", "document", "video", "sticker", "voice", "contact", "location"} {
				if msg[kind] != nil {
					return "send [" + kind + "]"
				}
			}
			return "send"
		}
	}
	for k := range m {
		if k != "update_id" {
			return k
		}
	}
	return "update"
}

func accountInput(e event) string {
	p, _ := e.Params.(map[string]any)
	if e.Method == "tap" {
		if d := str(p["data"]); d != "" {
			return "tap " + d
		}
		return "tap " + str(p["label"])
	}
	return "send " + str(p["text"])
}

// buildSteps puts each screen under the last action before it, and keeps the final state of each message.
func buildSteps(events []event) []step {
	screens, _ := buildScreens(events)
	marks := stepMarkers(events)
	steps := make([]step, len(marks)+1)
	steps[0] = step{N: 0, Input: "(before the first action)"}
	for i, m := range marks {
		steps[i+1] = step{N: i + 1, Input: m.Input, T: m.T}
	}
	pos := make([]map[[2]int64]int, len(steps))
	for _, sc := range screens {
		k := sort.Search(len(marks), func(i int) bool { return marks[i].T.After(sc.T) })
		st := &steps[k]
		if pos[k] == nil {
			pos[k] = map[[2]int64]int{}
		}
		key := [2]int64{sc.State.Chat, int64(sc.State.ID)}
		if i, ok := pos[k][key]; ok && sc.State.ID != 0 {
			st.Screens[i] = sc
			continue
		}
		pos[k][key] = len(st.Screens)
		st.Screens = append(st.Screens, sc)
	}
	if len(steps[0].Screens) == 0 {
		return steps[1:]
	}
	return steps
}

// liveMask hides values that change between two runs on their own (prices, clocks), when the user asks for it.
type liveMask []*regexp.Regexp

var livePatterns = map[string]string{
	"money":   `[$€£]\s?[\d,]+(\.\d+)?[KMBT]?|[\d,]+(\.\d+)?[KMBT]?\s?(USD|USDC|SOL|ETH|BNB)\b`,
	"percent": `[+-]?\d+(\.\d+)?%`,
	"time":    `\b\d{1,2}:\d{2}(:\d{2})?\b|\b\d+[smhd] ago\b`,
	"number":  `\d+([.,]\d+)*`,
}

func parseLive(names string, extra []string) (liveMask, error) {
	var m liveMask
	for _, n := range strings.Split(names, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		p, ok := livePatterns[n]
		if !ok {
			return nil, fmt.Errorf("-live: unknown kind %q (money, percent, time, number)", n)
		}
		m = append(m, regexp.MustCompile(p))
	}
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("-live-re %q: %w", p, err)
		}
		m = append(m, re)
	}
	return m, nil
}

func (m liveMask) apply(s string) string {
	for _, re := range m {
		s = re.ReplaceAllString(s, "#")
	}
	return s
}

var markupTag = regexp.MustCompile(`</?(bold|italic|underline|strikethrough|spoiler|code|pre|quote)>|<emoji \d+>|</emoji>|<a [^>]*>|</a>`)

// applyMarkup masks the visible text of a markup string and keeps its tags: an emoji id or a link target is not
// a live value.
func (m liveMask) applyMarkup(s string) string {
	if len(m) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range markupTag.FindAllStringIndex(s, -1) {
		b.WriteString(m.apply(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(m.apply(s[last:]))
	return b.String()
}

var buttonLine = regexp.MustCompile(`^(  \d+\.\d+ )(.*?)( → .*)$`)

// applyLine masks one line of fullGrid: the label of a button line, not its target or icon; the media line not at all.
func (m liveMask) applyLine(l string) string {
	if len(m) == 0 {
		return l
	}
	if p := buttonLine.FindStringSubmatch(l); p != nil {
		return p[1] + m.apply(p[2]) + p[3]
	}
	if strings.HasPrefix(l, "[") && !strings.HasPrefix(l, "[ ") {
		return l
	}
	return m.applyMarkup(l)
}

// markup is the text with its formatting as tags; withEmoji adds the id of each custom emoji.
func markup(st msgState, withEmoji bool) string {
	if st.Raw {
		return "[raw " + st.Mode + "] " + st.Text
	}
	open := func(e entity) string {
		switch e.Type {
		case "custom_emoji":
			if withEmoji {
				return "<emoji " + e.Emoji + ">"
			}
		case "bold", "italic", "underline", "strikethrough", "spoiler", "code", "pre":
			return "<" + e.Type + ">"
		case "blockquote", "expandable_blockquote":
			return "<quote>"
		case "text_link":
			return "<a " + e.URL + ">"
		}
		return ""
	}
	close := func(e entity) string {
		switch e.Type {
		case "custom_emoji":
			if withEmoji {
				return "</emoji>"
			}
		case "bold", "italic", "underline", "strikethrough", "spoiler", "code", "pre":
			return "</" + e.Type + ">"
		case "blockquote", "expandable_blockquote":
			return "</quote>"
		case "text_link":
			return "</a>"
		}
		return ""
	}
	return walkEntities(st, open, close, func(s string) string { return s })
}

// fullGrid is the screen as text with everything a test checks: formatting, custom emoji ids, and for each
// button its target, icon and style.
func fullGrid(st msgState) string {
	var b strings.Builder
	if st.Media != nil {
		fmt.Fprintf(&b, "[%s %s]\n", st.Media.Kind, st.Media.UniqueID)
	}
	b.WriteString(strings.TrimRight(markup(st, true), "\n"))
	b.WriteString("\n")
	if len(st.Rows) == 0 {
		return b.String()
	}
	b.WriteString(strings.Repeat("─", 32) + "\n")
	for _, row := range st.Rows {
		cells := make([]string, len(row))
		for i, btn := range row {
			cells[i] = "[ " + btn.Text + " ]"
		}
		b.WriteString(strings.Join(cells, " ") + "\n")
	}
	b.WriteString("buttons:\n")
	for r, row := range st.Rows {
		for c, btn := range row {
			fmt.Fprintf(&b, "  %d.%d %s → %s", r+1, c+1, btn.Text, buttonTarget(btn))
			if btn.Icon != "" {
				b.WriteString(" · icon " + btn.Icon)
			}
			if btn.Style != "" {
				b.WriteString(" · " + btn.Style)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func buttonTarget(btn button) string {
	switch {
	case btn.Data != "":
		return btn.Data
	case btn.URL != "":
		return btn.Kind + " " + btn.URL
	}
	return btn.Kind
}

// Change kinds, in the order a report lists them.
var kindOrder = []string{"text", "format", "emoji", "media", "buttons", "order", "layout", "callback", "style", "icon", "live"}

// changeKinds says what differs between two states of one screen.
func changeKinds(a, b msgState, live liveMask) []string {
	set := map[string]bool{}
	if a.Text != b.Text {
		if live.apply(a.Text) == live.apply(b.Text) {
			set["live"] = true
		} else {
			set["text"] = true
		}
	}
	if a.Text == b.Text || live.apply(a.Text) == live.apply(b.Text) {
		ma, mb := live.applyMarkup(markup(a, false)), live.applyMarkup(markup(b, false))
		if ma != mb {
			set["format"] = true
		} else if live.applyMarkup(markup(a, true)) != live.applyMarkup(markup(b, true)) {
			set["emoji"] = true
		}
	}
	if (a.Media == nil) != (b.Media == nil) || (a.Media != nil && (a.Media.Kind != b.Media.Kind || a.Media.UniqueID != b.Media.UniqueID)) {
		set["media"] = true
	}
	for _, k := range buttonKinds(a.Rows, b.Rows, live) {
		set[k] = true
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

func buttonKinds(ra, rb [][]button, live liveMask) []string {
	flat := func(rows [][]button) ([]button, []int) {
		var bs []button
		var shape []int
		for _, r := range rows {
			bs = append(bs, r...)
			shape = append(shape, len(r))
		}
		return bs, shape
	}
	ba, sa := flat(ra)
	bb, sb := flat(rb)
	label := func(bs []button) []string {
		out := make([]string, len(bs))
		for i, x := range bs {
			out[i] = live.apply(x.Text)
		}
		return out
	}
	la, lb := label(ba), label(bb)
	var out []string
	switch {
	case slices.Equal(la, lb):
		if !slices.Equal(sa, sb) {
			out = append(out, "layout")
		}
	case slices.Equal(sorted(la), sorted(lb)):
		out = append(out, "order")
	default:
		out = append(out, "buttons")
	}
	byLabel := map[string][]button{}
	for i, x := range ba {
		byLabel[la[i]] = append(byLabel[la[i]], x)
	}
	seen := map[string]bool{}
	for i, y := range bb {
		q := byLabel[lb[i]]
		if len(q) == 0 {
			continue
		}
		x := q[0]
		byLabel[lb[i]] = q[1:]
		if buttonTarget(x) != buttonTarget(y) {
			seen["callback"] = true
		}
		if x.Style != y.Style {
			seen["style"] = true
		}
		if x.Icon != y.Icon {
			seen["icon"] = true
		}
		if x.Text != y.Text {
			seen["live"] = true
		}
	}
	for _, k := range []string{"callback", "style", "icon", "live"} {
		if seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func sorted(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// alignSteps pairs the steps of two sessions by their input, with a longest common subsequence: a step that one
// side has and the other not stays alone, and the rest still line up.
func alignSteps(a, b []step) [][2]*step {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i].Input == b[j].Input {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var out [][2]*step
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i].Input == b[j].Input:
			out = append(out, [2]*step{&a[i], &b[j]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, [2]*step{&a[i], nil})
			i++
		default:
			out = append(out, [2]*step{nil, &b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, [2]*step{&a[i], nil})
	}
	for ; j < m; j++ {
		out = append(out, [2]*step{nil, &b[j]})
	}
	return out
}

// lineDiffBy is lineDiff with equality on keys: two lines with the same key and different text are shown as
// "~ old → new" (a live value), not as a removed and an added line.
func lineDiffBy(a, b, ka, kb []string) []string {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if ka[i] == kb[j] {
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
		case ka[i] == kb[j]:
			if a[i] == b[j] {
				out = append(out, "  "+b[j])
			} else {
				out = append(out, "~ "+a[i]+"  →  "+b[j])
			}
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
