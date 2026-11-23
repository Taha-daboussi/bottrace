package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// Messages the driver sends as the user get ids from here up, so the bot's calls on them can be told apart.
const injectBase = 1_900_000_000

type bots struct {
	mu  sync.Mutex
	all map[string]*bot
}

func newBots() *bots { return &bots{all: map[string]*bot{}} }

func (bs *bots) get(token string) *bot {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if b, ok := bs.all[token]; ok {
		return b
	}
	id, _, _ := strings.Cut(token, ":")
	b := &bot{id: id, token: token, wake: make(chan struct{}, 1), msgs: map[msgKey]json.RawMessage{}, fetched: map[string]bool{}}
	b.load()
	bs.all[token] = b
	return b
}

// pick returns the bot that knows chat, or the only bot that knows a user.
func (bs *bots) pick(chat int64) (*bot, error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	var withUsers []*bot
	for _, b := range bs.all {
		b.mu.Lock()
		_, knows := b.st.Users[chat]
		n := len(b.st.Users)
		b.mu.Unlock()
		if chat != 0 && knows {
			return b, nil
		}
		if n > 0 {
			withUsers = append(withUsers, b)
		}
	}
	switch {
	case len(bs.all) == 0:
		return nil, errors.New("no bot has called Telegram through bottrace yet: start the bot with the env line of `bottrace env`")
	case len(withUsers) == 1:
		return withUsers[0], nil
	case len(withUsers) == 0:
		return nil, errors.New("bottrace has not seen a user yet: send any message to the bot in Telegram first")
	}
	return nil, errors.New("more than one bot: pass -chat")
}

// proxied says whether a bot with this username runs behind the proxy.
func (bs *bots) proxied(name string) bool {
	return slices.ContainsFunc(bs.usernames(), func(u string) bool { return strings.EqualFold(u, name) })
}

func (bs *bots) usernames() []string {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	var out []string
	for _, b := range bs.all {
		b.mu.Lock()
		if b.st.Username != "" {
			out = append(out, b.st.Username)
		}
		b.mu.Unlock()
	}
	return out
}

type msgKey struct {
	chat int64
	id   int
}

type botState struct {
	Shift     int                       `json:"shift"`
	Users     map[int64]json.RawMessage `json:"users"`
	Chats     map[int64]json.RawMessage `json:"chats"`
	NextQuery int                       `json:"next_query"`
	NextMsg   int                       `json:"next_msg"`
	Username  string                    `json:"username"`
}

type pendingUpdate struct {
	build func(id int) json.RawMessage
	done  chan struct{}
}

type bot struct {
	id    string
	token string
	wake  chan struct{}

	mu      sync.Mutex
	st      botState
	pending []*pendingUpdate
	msgs    map[msgKey]json.RawMessage
	order   []msgKey
	fetched map[string]bool
}

func (b *bot) name() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st.Username != "" {
		return b.st.Username
	}
	return b.id
}

func (b *bot) statePath() string { return filepath.Join(stateDir(), "bots", b.id+".json") }

func (b *bot) load() {
	b.st = botState{Users: map[int64]json.RawMessage{}, Chats: map[int64]json.RawMessage{}}
	data, err := os.ReadFile(b.statePath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &b.st)
	if b.st.Users == nil {
		b.st.Users = map[int64]json.RawMessage{}
	}
	if b.st.Chats == nil {
		b.st.Chats = map[int64]json.RawMessage{}
	}
}

// save keeps the id shift on disk: a bot that outlives a bottrace restart still asks with offsets in the shifted
// space, and a lost shift would confirm real updates it never got.
func (b *bot) save() {
	data, err := json.Marshal(b.st)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(b.statePath()), 0o700)
	_ = os.WriteFile(b.statePath(), data, 0o600)
}

func (b *bot) claimFetch(unique string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fetched[unique] {
		return false
	}
	b.fetched[unique] = true
	return true
}

func (b *bot) toReal(offset int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if offset <= 0 {
		return 0
	}
	return offset - b.st.Shift
}

func (b *bot) virtualize(ups []json.RawMessage) []json.RawMessage {
	b.mu.Lock()
	shift := b.st.Shift
	b.mu.Unlock()
	out := make([]json.RawMessage, 0, len(ups))
	for _, u := range ups {
		var m map[string]json.RawMessage
		if json.Unmarshal(u, &m) != nil {
			out = append(out, u)
			continue
		}
		var id int
		_ = json.Unmarshal(m["update_id"], &id)
		m["update_id"] = json.RawMessage(strconv.Itoa(id + shift))
		v, _ := json.Marshal(m)
		out = append(out, v)
	}
	return out
}

// takePending hands the queued user actions to the poll that asked with offset. Their ids start at the offset, so
// the bot takes them; the shift grows by as many, so the bot's next offset still maps to the same real one.
func (b *bot) takePending(offset int) []json.RawMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return nil
	}
	start := offset
	if start <= 0 {
		start = 1
	}
	out := make([]json.RawMessage, 0, len(b.pending))
	for i, p := range b.pending {
		out = append(out, p.build(start+i))
		close(p.done)
	}
	n := len(b.pending)
	if offset > 0 {
		b.st.Shift += n
	} else {
		b.st.Shift = start + n - 1
	}
	b.pending = nil
	b.save()
	return out
}

func (b *bot) unqueue(p *pendingUpdate) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.pending)
	b.pending = slices.DeleteFunc(b.pending, func(x *pendingUpdate) bool { return x == p })
	return len(b.pending) < n
}

func (b *bot) localAnswer(method string, params url.Values) ([]byte, string) {
	ok := []byte(`{"ok":true,"result":true}`)
	switch method {
	case "answerCallbackQuery":
		if strings.HasPrefix(params.Get("callback_query_id"), "bottrace-") {
			return ok, "answered by bottrace: the tap came from bottrace, so Telegram does not know the query"
		}
	case "deleteMessage", "setMessageReaction":
		if id, _ := strconv.Atoi(params.Get("message_id")); id >= injectBase {
			return ok, "answered by bottrace: the message came from bottrace, so it is not in the chat"
		}
	}
	return nil, ""
}

func (b *bot) stripInjectedReply(params url.Values) bool {
	stripped := false
	if id, _ := strconv.Atoi(params.Get("reply_to_message_id")); id >= injectBase {
		params.Del("reply_to_message_id")
		stripped = true
	}
	if rp := params.Get("reply_parameters"); rp != "" {
		var v struct {
			MessageID int `json:"message_id"`
		}
		if json.Unmarshal([]byte(rp), &v) == nil && v.MessageID >= injectBase {
			params.Del("reply_parameters")
			stripped = true
		}
	}
	return stripped
}

func (b *bot) observeCall(method string, params url.Values, result json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch method {
	case "getMe":
		var me struct {
			Username string `json:"username"`
		}
		if json.Unmarshal(result, &me) == nil && me.Username != "" && me.Username != b.st.Username {
			b.st.Username = me.Username
			b.save()
		}
		return
	case "deleteMessage":
		chat, _ := strconv.ParseInt(params.Get("chat_id"), 10, 64)
		id, _ := strconv.Atoi(params.Get("message_id"))
		k := msgKey{chat, id}
		delete(b.msgs, k)
		b.order = slices.DeleteFunc(b.order, func(x msgKey) bool { return x == k })
		return
	}
	var list []json.RawMessage
	if json.Unmarshal(result, &list) != nil {
		list = []json.RawMessage{result}
	}
	for _, raw := range list {
		var m struct {
			MessageID int `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		}
		if json.Unmarshal(raw, &m) != nil || m.MessageID == 0 {
			continue
		}
		k := msgKey{m.Chat.ID, m.MessageID}
		b.msgs[k] = raw
		b.order = append(slices.DeleteFunc(b.order, func(x msgKey) bool { return x == k }), k)
	}
}

func (b *bot) observeUpdate(raw json.RawMessage) {
	var u struct {
		Message *struct {
			From json.RawMessage `json:"from"`
			Chat json.RawMessage `json:"chat"`
		} `json:"message"`
		Callback *struct {
			From    json.RawMessage `json:"from"`
			Message *struct {
				Chat json.RawMessage `json:"chat"`
			} `json:"message"`
		} `json:"callback_query"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	var from, chat json.RawMessage
	switch {
	case u.Message != nil:
		from, chat = u.Message.From, u.Message.Chat
	case u.Callback != nil && u.Callback.Message != nil:
		from, chat = u.Callback.From, u.Callback.Message.Chat
	default:
		return
	}
	var c struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(chat, &c) != nil || c.Type != "private" || len(from) == 0 {
		return
	}
	b.mu.Lock()
	b.st.Users[c.ID] = from
	b.st.Chats[c.ID] = chat
	b.save()
	b.mu.Unlock()
}

func (b *bot) chatFor(chat int64) (int64, error) {
	if chat != 0 {
		if _, ok := b.st.Users[chat]; !ok {
			return 0, fmt.Errorf("bottrace has not seen a user in chat %d: send any message there in Telegram first", chat)
		}
		return chat, nil
	}
	switch len(b.st.Users) {
	case 0:
		return 0, errors.New("bottrace has not seen a user yet: send any message to the bot in Telegram first")
	case 1:
		for c := range b.st.Users {
			return c, nil
		}
	}
	ids := make([]string, 0, len(b.st.Users))
	for c := range b.st.Users {
		ids = append(ids, strconv.FormatInt(c, 10))
	}
	sort.Strings(ids)
	return 0, fmt.Errorf("more than one chat (%s): pass -chat", strings.Join(ids, ", "))
}

type tapRequest struct {
	Chat  int64  `json:"chat"`
	Msg   int    `json:"msg"`
	Data  string `json:"data"`
	Label string `json:"label"`
	Text  string `json:"text"`
	Bot   string `json:"bot,omitempty"`
}

// tap queues a button tap on the newest bot message in the chat that has the button.
func (b *bot) tap(req tapRequest) (*pendingUpdate, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chat, err := b.chatFor(req.Chat)
	if err != nil {
		return nil, "", err
	}
	var msgs []json.RawMessage
	for i := len(b.order) - 1; i >= 0; i-- {
		k := b.order[i]
		if k.chat == chat && (req.Msg == 0 || k.id == req.Msg) {
			msgs = append(msgs, b.msgs[k])
		}
	}
	msg, btn, err := findButton(msgs, req)
	if errors.Is(err, errNoButton) {
		return nil, "", fmt.Errorf("no button %q on the bot messages bottrace saw in chat %d (bottrace screen shows the newest one)", cmpOr(req.Label, req.Data), chat)
	}
	if err != nil {
		return nil, "", err
	}
	if btn.Data == "" {
		return nil, "", fmt.Errorf("button %q has no callback data: it is a %s button %s", btn.Text, btn.Kind, btn.URL)
	}
	b.st.NextQuery++
	qid := fmt.Sprintf("bottrace-%d", b.st.NextQuery)
	user := b.st.Users[chat]
	data := btn.Data
	b.save()
	p := &pendingUpdate{done: make(chan struct{}), build: func(id int) json.RawMessage {
		out, _ := json.Marshal(map[string]any{"update_id": id, "callback_query": map[string]any{
			"id": qid, "from": user, "message": msg, "chat_instance": "bottrace", "data": data,
		}})
		return out
	}}
	b.pending = append(b.pending, p)
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return p, btn.Text, nil
}

// send queues a text message from the user.
func (b *bot) send(req tapRequest) (*pendingUpdate, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	chat, err := b.chatFor(req.Chat)
	if err != nil {
		return nil, err
	}
	b.st.NextMsg++
	id := injectBase + b.st.NextMsg
	user, chatObj, text := b.st.Users[chat], b.st.Chats[chat], req.Text
	b.save()
	msg := map[string]any{"message_id": id, "from": user, "chat": chatObj, "date": time.Now().Unix(), "text": text}
	if strings.HasPrefix(text, "/") {
		cmd, _, _ := strings.Cut(text, " ")
		msg["entities"] = []map[string]any{{"type": "bot_command", "offset": 0, "length": len(utf16.Encode([]rune(cmd)))}}
	}
	p := &pendingUpdate{done: make(chan struct{}), build: func(uid int) json.RawMessage {
		out, _ := json.Marshal(map[string]any{"update_id": uid, "message": msg})
		return out
	}}
	b.pending = append(b.pending, p)
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return p, nil
}

// newest returns the newest bot message in the chat.
func (b *bot) newest(chat int64) (map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if chat == 0 {
		c, err := b.chatFor(0)
		if err != nil {
			return nil, err
		}
		chat = c
	}
	for i := len(b.order) - 1; i >= 0; i-- {
		if b.order[i].chat != chat {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b.msgs[b.order[i]], &m) == nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("no bot message seen in chat %d since bottrace started: send /start (bottrace send /start)", chat)
}

var errNoButton = errors.New("no such button")

// findButton looks through messages, newest first, for the button that req names: the exact callback data or label
// on the newest message that has one, else the one button whose label contains the text.
func findButton(msgs []json.RawMessage, req tapRequest) (json.RawMessage, button, error) {
	type hit struct {
		msg json.RawMessage
		btn button
	}
	var exact, loose []hit
	for _, raw := range msgs {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		st := fromMessage(m)
		for _, row := range st.Rows {
			for _, btn := range row {
				switch {
				case req.Data != "" && btn.Data == req.Data:
					exact = append(exact, hit{raw, btn})
				case req.Data == "" && strings.TrimSpace(btn.Text) == strings.TrimSpace(req.Label):
					exact = append(exact, hit{raw, btn})
				case req.Data == "" && req.Label != "" && strings.Contains(strings.ToLower(btn.Text), strings.ToLower(req.Label)):
					loose = append(loose, hit{raw, btn})
				}
			}
		}
		if len(exact) > 0 || len(loose) > 0 {
			break
		}
	}
	switch {
	case len(exact) > 0:
		return exact[0].msg, exact[0].btn, nil
	case len(loose) == 1:
		return loose[0].msg, loose[0].btn, nil
	case len(loose) > 1:
		names := make([]string, len(loose))
		for i, l := range loose {
			names[i] = strconv.Quote(l.btn.Text)
		}
		return nil, button{}, fmt.Errorf("%q matches %d buttons: %s", req.Label, len(loose), strings.Join(names, ", "))
	}
	return nil, button{}, errNoButton
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
