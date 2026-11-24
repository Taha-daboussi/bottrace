package main

import (
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

type button struct {
	Text  string
	Data  string
	URL   string
	Style string
	Kind  string // callback, url, web_app, switch_inline, copy, pay, other
	Icon  string // custom emoji id of the button's icon
}

type entity struct {
	Type   string
	Offset int
	Length int
	URL    string
	Emoji  string
}

type mediaInfo struct {
	Kind     string
	FileID   string
	UniqueID string
	Path     string // relative to the session folder, when the bytes are known
}

type msgState struct {
	Chat     int64
	ID       int
	Text     string
	Entities []entity
	Media    *mediaInfo
	Rows     [][]button
	Raw      bool   // text as the bot sent it: Telegram refused it, so no entities
	Mode     string // parse mode of a raw text
	Emoji    map[string]string
}

var screenMethods = map[string]bool{
	"sendMessage": true, "sendPhoto": true, "sendAnimation": true, "sendVideo": true, "sendDocument": true,
	"editMessageText": true, "editMessageCaption": true, "editMessageMedia": true, "editMessageReplyMarkup": true,
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

func fromMessage(m map[string]any) msgState {
	st := msgState{ID: int(num(m["message_id"]))}
	if c, ok := m["chat"].(map[string]any); ok {
		st.Chat = int64(num(c["id"]))
	}
	ents := m["entities"]
	if t, ok := m["text"].(string); ok {
		st.Text = t
	} else if c, ok := m["caption"].(string); ok {
		st.Text = c
		ents = m["caption_entities"]
	}
	if list, ok := ents.([]any); ok {
		for _, e := range list {
			if o, ok := e.(map[string]any); ok {
				st.Entities = append(st.Entities, entity{Type: str(o["type"]), Offset: int(num(o["offset"])), Length: int(num(o["length"])), URL: str(o["url"]), Emoji: str(o["custom_emoji_id"])})
			}
		}
	}
	if f := messageFiles(m); len(f) > 0 {
		st.Media = &mediaInfo{Kind: f[0].Kind, FileID: f[0].FileID, UniqueID: f[0].UniqueID}
	}
	st.Rows = keyboardRows(m["reply_markup"])
	return st
}

func keyboardRows(v any) [][]button {
	rm, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	rows, ok := rm["inline_keyboard"].([]any)
	if !ok {
		return nil
	}
	var out [][]button
	for _, r := range rows {
		cells, ok := r.([]any)
		if !ok {
			continue
		}
		var row []button
		for _, c := range cells {
			o, ok := c.(map[string]any)
			if !ok {
				continue
			}
			btn := button{Text: str(o["text"]), Style: str(o["style"]), Kind: "other", Icon: str(o["icon_custom_emoji_id"])}
			switch {
			case o["callback_data"] != nil:
				btn.Kind, btn.Data = "callback", str(o["callback_data"])
			case o["url"] != nil:
				btn.Kind, btn.URL = "url", str(o["url"])
			case o["web_app"] != nil:
				btn.Kind = "web_app"
				if w, ok := o["web_app"].(map[string]any); ok {
					btn.URL = str(w["url"])
				}
			case o["switch_inline_query"] != nil || o["switch_inline_query_current_chat"] != nil || o["switch_inline_query_chosen_chat"] != nil:
				btn.Kind = "switch_inline"
			case o["copy_text"] != nil:
				btn.Kind = "copy"
			case o["pay"] != nil:
				btn.Kind = "pay"
			}
			row = append(row, btn)
		}
		out = append(out, row)
	}
	return out
}

// fromParams is the screen as the bot asked for it: used when Telegram refused the call, or answered true.
func fromParams(method string, params map[string]any, prev *msgState) msgState {
	var st msgState
	if prev != nil {
		st = *prev
	}
	st.Chat = int64(num(params["chat_id"]))
	if id := int(num(params["message_id"])); id != 0 {
		st.ID = id
	}
	if method != "editMessageReplyMarkup" {
		text, ok := params["text"].(string)
		if !ok {
			text, _ = params["caption"].(string)
		}
		st.Text, st.Entities, st.Raw = text, nil, true
		st.Mode = str(params["parse_mode"])
	}
	if rm, ok := params["reply_markup"]; ok {
		st.Rows = keyboardRows(rm)
	}
	return st
}

type screen struct {
	N      int
	Seq    int
	T      time.Time
	Method string
	Bot    string
	State  msgState
	OK     bool
	Error  string
	Flags  []limitFlag
	Ms     int64
}

type limitFlag struct {
	Level string // error, warn
	Text  string
}

// buildScreens replays the session: each bot call that draws a message is one screen, the message as it then
// looked.
func buildScreens(events []event) ([]screen, map[string]string) {
	media := mediaIndex(events)
	states := map[[2]int64]msgState{}
	botSide := map[string]bool{}
	for _, e := range events {
		if e.Kind == "tg_call" && screenMethods[e.Method] {
			botSide[strings.ToLower(e.Bot)] = true
		}
	}
	var out []screen
	for _, e := range events {
		if e.Kind == "tg_user" && (e.Method == "message" || e.Method == "edit") && !accountOut(e) && !botSide[strings.ToLower(e.Bot)] {
			out = append(out, receivedScreen(e, len(out)+1, media))
			continue
		}
		if e.Kind != "tg_call" || !screenMethods[e.Method] {
			continue
		}
		params, _ := e.Params.(map[string]any)
		key := [2]int64{int64(num(params["chat_id"])), int64(num(params["message_id"]))}
		var prev *msgState
		if p, ok := states[key]; ok {
			prev = &p
		}
		var st msgState
		if m, ok := e.Result.(map[string]any); ok && e.OK {
			st = fromMessage(m)
		} else if prev != nil && strings.Contains(e.Error, "message is not modified") {
			st = *prev
		} else {
			st = fromParams(e.Method, params, prev)
		}
		if st.Media != nil {
			st.Media.Path = media[st.Media.UniqueID]
		} else if len(e.Files) > 0 {
			st.Media = &mediaInfo{Kind: "photo", Path: e.Files[0].Path}
		} else if prev != nil && prev.Media != nil && !e.OK {
			st.Media = prev.Media
		}
		st.Emoji = media
		states[[2]int64{st.Chat, int64(st.ID)}] = st
		sc := screen{N: len(out) + 1, Seq: e.Seq, T: e.T, Method: e.Method, Bot: e.Bot, State: st, OK: e.OK, Error: e.Error, Ms: e.Ms}
		sc.Flags = checkLimits(e, params, st)
		out = append(out, sc)
	}
	return out, media
}

// accountOut says whether a user-side message was sent by the account itself.
func accountOut(e event) bool {
	m, _ := e.Result.(map[string]any)
	meta, _ := m["user_meta"].(map[string]any)
	return meta["out"] == true
}

// receivedScreen is a message as the account got it, for a bot that does not run behind the proxy.
func receivedScreen(e event, n int, media map[string]string) screen {
	m, _ := e.Result.(map[string]any)
	st := fromMessage(m)
	if st.Media != nil {
		st.Media.Path = media[st.Media.UniqueID]
	}
	st.Emoji = media
	return screen{N: n, Seq: e.Seq, T: e.T, Method: e.Method + " (as received)", Bot: e.Bot, State: st, OK: true, Flags: checkLimits(e, nil, st)}
}

func mediaIndex(events []event) map[string]string {
	idx := map[string]string{}
	for _, e := range events {
		for _, m := range e.Media {
			if m.Path != "" && m.UniqueID != "" {
				idx[m.UniqueID] = m.Path
			}
		}
	}
	return idx
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func checkLimits(e event, params map[string]any, st msgState) []limitFlag {
	var fl []limitFlag
	switch {
	case strings.Contains(e.Error, "message is not modified"):
		fl = append(fl, limitFlag{"warn", "the edit changed nothing (Telegram: message is not modified)"})
	case e.Error != "":
		fl = append(fl, limitFlag{"error", "Telegram refused it: " + e.Error})
	}
	rows := st.Rows
	if rm, ok := params["reply_markup"]; ok {
		rows = keyboardRows(rm)
	}
	total := 0
	for i, row := range rows {
		total += len(row)
		if len(row) > 8 {
			fl = append(fl, limitFlag{"error", fmt.Sprintf("row %d has %d buttons: Telegram allows 8", i+1, len(row))})
		}
		for _, b := range row {
			n := len(b.Data)
			switch {
			case n > 64:
				fl = append(fl, limitFlag{"error", fmt.Sprintf("callback data of %q is %d bytes: Telegram allows 64", b.Text, n)})
			case n > 56:
				fl = append(fl, limitFlag{"warn", fmt.Sprintf("callback data of %q is %d bytes, close to the 64 limit", b.Text, n)})
			}
			if strings.TrimSpace(b.Text) == "" {
				fl = append(fl, limitFlag{"error", fmt.Sprintf("row %d has a button with no text", i+1)})
			}
			if w := utf8.RuneCountInString(b.Text); len(row) >= 2 && w > 36/len(row)+4 {
				fl = append(fl, limitFlag{"warn", fmt.Sprintf("%q (%d characters) may be cut on a phone in a row of %d", b.Text, w, len(row))})
			}
		}
	}
	if total > 100 {
		fl = append(fl, limitFlag{"error", fmt.Sprintf("%d buttons: Telegram allows 100", total)})
	}
	n := utf16Len(st.Text)
	caption := st.Media != nil || e.Method == "sendPhoto" || e.Method == "sendAnimation" || e.Method == "sendVideo" || e.Method == "editMessageCaption"
	switch {
	case caption && n > 1024:
		fl = append(fl, limitFlag{"error", fmt.Sprintf("caption is %d characters: Telegram allows 1024", n)})
	case caption && n > 900:
		fl = append(fl, limitFlag{"warn", fmt.Sprintf("caption is %d characters, close to the 1024 limit", n)})
	case !caption && n > 4096:
		fl = append(fl, limitFlag{"error", fmt.Sprintf("text is %d characters: Telegram allows 4096", n)})
	case !caption && n > 3600:
		fl = append(fl, limitFlag{"warn", fmt.Sprintf("text is %d characters, close to the 4096 limit", n)})
	}
	return fl
}

// textHTML draws the message text with its entities, as Telegram shows it.
func textHTML(st msgState, prefix string) string {
	if st.Raw {
		return `<span class="raw">` + html.EscapeString(st.Text) + `</span>`
	}
	units := utf16.Encode([]rune(st.Text))
	return walkEntities(st, func(e entity) string { return openTag(e, st, prefix, units) }, closeTag, html.EscapeString)
}

// walkEntities writes the text of st with open and close around each entity, nested as Telegram nests them.
func walkEntities(st msgState, open, close func(entity) string, esc func(string) string) string {
	units := utf16.Encode([]rune(st.Text))
	ents := append([]entity(nil), st.Entities...)
	sort.SliceStable(ents, func(i, j int) bool {
		if ents[i].Offset != ents[j].Offset {
			return ents[i].Offset < ents[j].Offset
		}
		return ents[i].Length > ents[j].Length
	})
	opens := map[int][]int{}
	for i, e := range ents {
		opens[e.Offset] = append(opens[e.Offset], i)
	}
	var b strings.Builder
	var stack []int
	pos := 0
	closeAt := func(p int) {
		for len(stack) > 0 {
			top := ents[stack[len(stack)-1]]
			if top.Offset+top.Length > p {
				break
			}
			b.WriteString(close(top))
			stack = stack[:len(stack)-1]
		}
	}
	for i := 0; i <= len(units); {
		closeAt(pos)
		for _, idx := range opens[pos] {
			b.WriteString(open(ents[idx]))
			stack = append(stack, idx)
		}
		if i == len(units) {
			break
		}
		r := rune(units[i])
		size := 1
		if utf16.IsSurrogate(r) && i+1 < len(units) {
			r = utf16.DecodeRune(r, rune(units[i+1]))
			size = 2
		}
		b.WriteString(esc(string(r)))
		i += size
		pos += size
	}
	for len(stack) > 0 {
		b.WriteString(close(ents[stack[len(stack)-1]]))
		stack = stack[:len(stack)-1]
	}
	return b.String()
}

func safeHref(u string) string {
	l := strings.ToLower(u)
	if strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "tg://") {
		return html.EscapeString(u)
	}
	return "#"
}

func openTag(e entity, st msgState, prefix string, units []uint16) string {
	switch e.Type {
	case "custom_emoji":
		if p := st.Emoji["emoji:"+e.Emoji]; p != "" {
			return `<span class="ce" style="background-image:url('` + html.EscapeString(prefix+p) + `')">`
		}
	case "bold":
		return "<b>"
	case "italic":
		return "<i>"
	case "underline":
		return "<u>"
	case "strikethrough":
		return "<s>"
	case "spoiler":
		return `<span class="spoiler">`
	case "code":
		return "<code>"
	case "pre":
		return "<pre>"
	case "blockquote", "expandable_blockquote":
		return "<blockquote>"
	case "text_link":
		return `<a href="` + safeHref(e.URL) + `" target="_blank" rel="noopener">`
	case "url":
		end := min(e.Offset+e.Length, len(units))
		return `<a href="` + safeHref(string(utf16.Decode(units[e.Offset:end]))) + `" target="_blank" rel="noopener">`
	case "mention", "hashtag", "cashtag", "bot_command", "email", "phone_number", "text_mention":
		return `<span class="link">`
	}
	return "<span>"
}

func closeTag(e entity) string {
	switch e.Type {
	case "bold":
		return "</b>"
	case "italic":
		return "</i>"
	case "underline":
		return "</u>"
	case "strikethrough":
		return "</s>"
	case "code":
		return "</code>"
	case "pre":
		return "</pre>"
	case "blockquote", "expandable_blockquote":
		return "</blockquote>"
	case "text_link", "url":
		return "</a>"
	}
	return "</span>"
}

// textGrid is the screen as plain text: the message, then the buttons as rows.
func textGrid(st msgState, withData bool) string {
	var b strings.Builder
	if st.Media != nil {
		fmt.Fprintf(&b, "[%s]\n", st.Media.Kind)
	}
	b.WriteString(strings.TrimRight(st.Text, "\n"))
	b.WriteString("\n")
	if len(st.Rows) > 0 {
		b.WriteString(strings.Repeat("─", 32) + "\n")
	}
	for _, row := range st.Rows {
		cells := make([]string, len(row))
		for i, btn := range row {
			label := btn.Text
			if btn.Icon != "" {
				label = "✦ " + label
			}
			switch btn.Kind {
			case "url", "web_app":
				label += " ↗"
			case "switch_inline":
				label += " ⤴"
			}
			if btn.Style != "" {
				label += " | " + btn.Style
			}
			cells[i] = "[ " + label + " ]"
		}
		b.WriteString(strings.Join(cells, " ") + "\n")
	}
	if withData {
		var lines []string
		for _, row := range st.Rows {
			for _, btn := range row {
				switch {
				case btn.Data != "":
					lines = append(lines, fmt.Sprintf("  %s → %s", btn.Text, btn.Data))
				case btn.URL != "":
					lines = append(lines, fmt.Sprintf("  %s → %s", btn.Text, btn.URL))
				}
			}
		}
		if len(lines) > 0 {
			b.WriteString("\nbuttons:\n" + strings.Join(lines, "\n") + "\n")
		}
	}
	return b.String()
}

func screenText(sc screen) string {
	head := fmt.Sprintf("#%03d %s  msg %d  %s", sc.N, sc.Method, sc.State.ID, sc.T.Format("15:04:05.000"))
	var fl strings.Builder
	for _, f := range sc.Flags {
		fmt.Fprintf(&fl, "! %s: %s\n", f.Level, f.Text)
	}
	return head + "\n" + fl.String() + textGrid(sc.State, true)
}
