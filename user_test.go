package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func styledButton(text string, st tg.KeyboardButtonStyle, typ tg.InlineButtonTypeClass) tg.KeyboardInlineButton {
	b := tg.KeyboardInlineButton{Text: text, Type: typ}
	b.SetStyle(st)
	return b
}

func TestUserMessageFormat(t *testing.T) {
	bot := &tg.User{ID: 42, Username: "dawn_test3_ugqr2_bot", Bot: true, FirstName: "Dawn", Phone: "15550001111", AccessHash: 99}
	text := "💰 Balance 1.5 SOL\nGo"
	m := &tg.Message{
		ID:      881,
		Date:    1700000000,
		Message: text,
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityCustomEmoji{Offset: 0, Length: 2, DocumentID: 5368324170671202286},
			&tg.MessageEntityBold{Offset: 3, Length: 7},
			&tg.MessageEntityTextURL{Offset: 19, Length: 2, URL: "https://dawn.trade"},
			&tg.MessageEntityBlockquote{Collapsed: true, Offset: 0, Length: 2},
			&tg.MessageEntityFormattedDate{Offset: 0, Length: 1},
		},
		ReplyMarkup: &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{
			{Buttons: []tg.KeyboardInlineButton{
				styledButton("Positions", tg.KeyboardButtonStyle{BgPrimary: true, Icon: 5368324170671202287}, &tg.InlineButtonTypeCallback{Data: []byte("pos")}),
				styledButton("Sell", tg.KeyboardButtonStyle{BgDanger: true}, &tg.InlineButtonTypeCallback{Data: []byte{0xff, 0x01}}),
			}},
			{Buttons: []tg.KeyboardInlineButton{
				{Text: "Chart", Type: &tg.InlineButtonTypeURL{URL: "https://dexscreener.com"}},
				{Text: "App", Type: &tg.InlineButtonTypeWebView{URL: "https://app.dawn.trade"}},
				{Text: "Share", Type: &tg.InlineButtonTypeSwitchInline{SamePeer: true, Query: "x"}},
				{Text: "Copy", Type: &tg.InlineButtonTypeCopy{CopyText: "addr"}},
			}},
		}},
	}
	got := roundTrip(t, userMessage(m, bot, bot))

	if got["text"] != text || got["message_id"] != float64(881) {
		t.Fatalf("text or id: %v", got)
	}
	from := got["from"].(map[string]any)
	if _, ok := from["phone"]; ok || len(from) != 4 {
		t.Fatalf("from keeps more than id, is_bot, username, first_name: %v", from)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "15550001111") || strings.Contains(string(b), "access_hash") {
		t.Fatalf("phone or access hash leaked: %s", b)
	}
	ents := got["entities"].([]any)
	wantTypes := []string{"custom_emoji", "bold", "text_link", "expandable_blockquote", "formatted_date"}
	for i, w := range wantTypes {
		if e := ents[i].(map[string]any); e["type"] != w {
			t.Fatalf("entity %d type %v, want %s", i, e["type"], w)
		}
	}
	if ents[0].(map[string]any)["custom_emoji_id"] != "5368324170671202286" {
		t.Fatalf("custom emoji id: %v", ents[0])
	}

	st := fromMessage(got)
	if st.Entities[0].Emoji != "5368324170671202286" || st.Entities[2].URL != "https://dawn.trade" {
		t.Fatalf("fromMessage entities: %+v", st.Entities)
	}
	row := st.Rows[0]
	if row[0].Data != "pos" || row[0].Style != "primary" || row[0].Icon != "5368324170671202287" || row[1].Style != "danger" {
		t.Fatalf("first row: %+v", row)
	}
	if string(callbackBytes(row[1].Data)) != string([]byte{0xff, 0x01}) {
		t.Fatalf("binary callback data did not survive: %q", row[1].Data)
	}
	kinds := []string{}
	for _, b := range st.Rows[1] {
		kinds = append(kinds, b.Kind)
	}
	if strings.Join(kinds, ",") != "url,web_app,switch_inline,copy" {
		t.Fatalf("second row kinds: %v", kinds)
	}
	grid := textGrid(st, false)
	if !strings.Contains(grid, "[ ✦ Positions | primary ] [ Sell | danger ]") || !strings.Contains(grid, "[ App ↗ ]") {
		t.Fatalf("grid:\n%s", grid)
	}
}

func TestUserMessageMedia(t *testing.T) {
	bot := &tg.User{ID: 42, Username: "b", Bot: true}
	photo := &tg.Message{ID: 1, Message: "cap", Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{ID: 7, Sizes: []tg.PhotoSizeClass{
		&tg.PhotoSize{Type: "m", W: 320, H: 200}, &tg.PhotoSize{Type: "y", W: 1280, H: 800}, &tg.PhotoSize{Type: "w", W: 2560, H: 1600},
	}}}}
	got := roundTrip(t, userMessage(photo, bot, bot))
	if got["caption"] != "cap" || got["text"] != nil {
		t.Fatalf("caption: %v", got)
	}
	st := fromMessage(got)
	if st.Media == nil || st.Media.Kind != "photo" || st.Media.UniqueID != "u:photo:7" {
		t.Fatalf("photo: %+v", st.Media)
	}
	if typ, w, _ := photoThumb(photo.Media.(*tg.MessageMediaPhoto).Photo.(*tg.Photo), maxMediaSide); typ != "y" || w != 1280 {
		t.Fatalf("picked size %s %d", typ, w)
	}
	anim := &tg.Message{ID: 2, Media: &tg.MessageMediaDocument{Document: &tg.Document{ID: 8, MimeType: "video/mp4", Attributes: []tg.DocumentAttributeClass{
		&tg.DocumentAttributeVideo{W: 10, H: 10}, &tg.DocumentAttributeAnimated{},
	}}}}
	if st := fromMessage(roundTrip(t, userMessage(anim, bot, bot))); st.Media == nil || st.Media.Kind != "animation" {
		t.Fatalf("animation: %+v", st.Media)
	}
	preview := &tg.Message{ID: 3, Message: "see https://x.y", Media: &tg.MessageMediaWebPage{Webpage: &tg.WebPage{URL: "https://x.y"}}}
	if got := roundTrip(t, userMessage(preview, bot, bot)); got["text"] != "see https://x.y" {
		t.Fatalf("a link preview made the text a caption: %v", got)
	}
}

func TestAllowlist(t *testing.T) {
	a, err := parseAllowlist("# test bots\n@Dawn_Test_ugqr2_bot rw\n@cove_trading_bot ro 8495538877  # real Cove\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := a.match(1, "dawn_test_ugqr2_bot", true); !ok || !e.Write {
		t.Fatalf("test bot: %+v %v", e, ok)
	}
	if _, ok := a.match(1, "dawn_test_ugqr2_bot", false); ok {
		t.Fatal("a user that is not a bot matched")
	}
	if _, ok := a.match(8617027065, "cove_trading_bot", true); ok {
		t.Fatal("a lookalike id matched the pinned Cove entry")
	}
	if e, ok := a.match(8495538877, "Cove_Trading_Bot", true); !ok || e.Write {
		t.Fatalf("real Cove: %+v %v", e, ok)
	}
	if _, ok := a.match(5, "someone_else_bot", true); ok {
		t.Fatal("a bot off the list matched")
	}
	for _, bad := range []string{"dawn_bot rw", "@x rx", "@x rw abc", "@x rw 1 2"} {
		if _, err := parseAllowlist(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestBotForDropsChatsOffTheList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(userDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	allow, _ := parseAllowlist("@dawn_test_ugqr2_bot rw\n")
	c := &userClient{allow: allow, peers: map[string]peer{}, names: map[int64]string{}}
	other := tg.Entities{Users: map[int64]*tg.User{9: {ID: 9, Username: "random_bot", Bot: true}, 10: {ID: 10, Username: "friend"}}}
	for _, id := range []int64{9, 10, 11} {
		if _, _, ok := c.botFor(id, other); ok {
			t.Fatalf("chat %d off the list matched", id)
		}
	}
	ours := tg.Entities{Users: map[int64]*tg.User{7: {ID: 7, Username: "dawn_test_ugqr2_bot", Bot: true, AccessHash: 3}}}
	if _, e, ok := c.botFor(7, ours); !ok || e.Username != "dawn_test_ugqr2_bot" {
		t.Fatal("our bot did not match")
	}
}

func TestFindButton(t *testing.T) {
	msg := func(id int, labels ...string) json.RawMessage {
		var row []any
		for _, l := range labels {
			row = append(row, map[string]any{"text": l, "callback_data": "cb:" + l})
		}
		b, _ := json.Marshal(map[string]any{"message_id": id, "reply_markup": map[string]any{"inline_keyboard": []any{row}}})
		return b
	}
	newestFirst := []json.RawMessage{msg(2, "Buy 0.1", "Buy 1"), msg(1, "Positions", "Buy")}
	if raw, b, err := findButton(newestFirst, tapRequest{Label: "Positions"}); err != nil || b.Data != "cb:Positions" || rawID(raw) != 1 {
		t.Fatalf("older message: %v %+v", err, b)
	}
	if _, _, err := findButton(newestFirst, tapRequest{Label: "buy"}); err == nil || !strings.Contains(err.Error(), "matches 2 buttons") {
		t.Fatalf("two loose matches: %v", err)
	}
	if _, b, err := findButton(newestFirst, tapRequest{Data: "cb:Buy 1"}); err != nil || b.Text != "Buy 1" {
		t.Fatalf("by data: %v %+v", err, b)
	}
	if _, _, err := findButton(newestFirst, tapRequest{Label: "Nope"}); !errors.Is(err, errNoButton) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAccountSideInScreensAndTimeline(t *testing.T) {
	now := time.Now()
	recv := func(seq int, bot string) event {
		return event{Seq: seq, T: now.Add(time.Duration(seq) * time.Second), Kind: "tg_user", Bot: bot, Method: "message",
			Result: roundTrip(t, map[string]any{"message_id": seq, "chat": map[string]any{"id": 1}, "text": "hi " + bot})}
	}
	call := event{Seq: 1, T: now, Kind: "tg_call", Bot: "proxied_bot", Method: "sendMessage", OK: true,
		Params: map[string]any{"chat_id": "5"}, Result: map[string]any{"message_id": float64(10), "chat": map[string]any{"id": float64(5)}, "text": "hi"}}
	tap := event{Seq: 4, T: now.Add(4 * time.Second), Kind: "tg_user", Bot: "proxied_bot", Method: "tap", OK: true, Ms: 412,
		Params: map[string]any{"label": "Deposit check", "data": "dep:check"}, Result: map[string]any{"text": "Nothing new yet."}}
	events := []event{call, recv(2, "proxied_bot"), recv(3, "staging_bot"), tap}

	screens, _ := buildScreens(events)
	if len(screens) != 2 || screens[0].Method != "sendMessage" || screens[1].Method != "message (as received)" || screens[1].Bot != "staging_bot" {
		t.Fatalf("screens: %+v", screens)
	}

	dir := t.TempDir()
	if err := writeTimeline(dir, events, dir+"/t.html"); err != nil {
		t.Fatal(err)
	}
	page := readFile(t, dir+"/t.html")
	for _, want := range []string{"account side", "as the account got it", "real tap through Telegram", "toast: Nothing new yet.", "412 ms", `href="screens/002.html"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("timeline misses %q", want)
		}
	}
	if strings.Index(page, "hi proxied_bot") > strings.Index(page, "Deposit check") {
		t.Fatal("timeline is not in time order")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
