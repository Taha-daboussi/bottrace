package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/contrib/middleware/ratelimit"
	tdsession "github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
	"golang.org/x/time/rate"
)

const (
	maxMediaSide  = 1280
	maxMediaBytes = 2 << 20
	keepMessages  = 50
)

func userDir() string     { return filepath.Join(stateDir(), "user") }
func sessionPath() string { return filepath.Join(userDir(), "session.json") }
func peersPath() string   { return filepath.Join(userDir(), "peers.json") }
func appPath() string     { return filepath.Join(configDir(), "telegram-app") }
func lockPath() string    { return filepath.Join(userDir(), "rec.lock") }

// lockUser lets one rec -user run at a time: Telegram pushes the updates of a login to one client only, so a second
// client would miss the bot's messages. The lock lasts as long as the process; the file names the holder.
func lockUser(who string) (*os.File, error) {
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(lockPath())
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another rec -user runs (%s): Telegram sends the updates of the login to one client only, so wait until it stops", strings.TrimSpace(string(holder)))
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(who+"\n"), 0)
	return f, nil
}

type telegramApp struct {
	id   int
	hash string
}

// loadApp reads the api id and hash of the bottrace app on my.telegram.org. The values never reach stdout.
func loadApp() (telegramApp, error) {
	p := appPath()
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return telegramApp{}, fmt.Errorf("no Telegram app file at %s: make an app on my.telegram.org and write \"api_id=...\" and \"api_hash=...\" there (mode 600)", p)
	}
	if err != nil {
		return telegramApp{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return telegramApp{}, fmt.Errorf("%s has mode %o: run chmod 600 %s", p, st.Mode().Perm(), p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return telegramApp{}, err
	}
	var app telegramApp
	for _, line := range strings.Split(string(data), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch strings.TrimSpace(k) {
		case "api_id":
			app.id, _ = strconv.Atoi(strings.TrimSpace(v))
		case "api_hash":
			app.hash = strings.TrimSpace(v)
		}
	}
	if app.id == 0 || app.hash == "" {
		return telegramApp{}, fmt.Errorf("%s needs the lines \"api_id=...\" and \"api_hash=...\"", p)
	}
	return app, nil
}

// secureUserDir makes the folder of the session and narrows a wider mode on it and its files.
func secureUserDir() error {
	if err := os.MkdirAll(userDir(), 0o700); err != nil {
		return err
	}
	for _, p := range []string{userDir(), sessionPath(), peersPath()} {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		want := os.FileMode(0o600)
		if st.IsDir() {
			want = 0o700
		}
		if st.Mode().Perm()&^want != 0 {
			if err := os.Chmod(p, want); err != nil {
				return err
			}
			fmt.Printf("bottrace: %s had mode %o, set it to %o\n", p, st.Mode().Perm(), want)
		}
	}
	return nil
}

// newTelegram makes the client of the account. It logs nothing, keeps to about 4 calls a second, and waits out a
// FLOOD_WAIT of at most 2 minutes.
func newTelegram(app telegramApp, store telegram.SessionStorage, handler telegram.UpdateHandler, onWait func(floodwait.FloodWait)) (*telegram.Client, *floodwait.Waiter) {
	waiter := floodwait.NewWaiter().WithMaxWait(2 * time.Minute).WithMaxRetries(3)
	if onWait != nil {
		waiter = waiter.WithCallback(func(_ context.Context, w floodwait.FloodWait) { onWait(w) })
	}
	c := telegram.NewClient(app.id, app.hash, telegram.Options{
		SessionStorage: store,
		UpdateHandler:  handler,
		Device:         telegram.DeviceConfig{DeviceModel: "bottrace", SystemVersion: runtime.GOOS, AppVersion: "bottrace 1"},
		Middlewares:    []telegram.Middleware{waiter, ratelimit.New(rate.Every(250*time.Millisecond), 4)},
	})
	return c, waiter
}

func cmdUser(args []string) error {
	if len(args) == 0 {
		return errors.New("bottrace user login|logout|status")
	}
	app, err := loadApp()
	if err != nil {
		return err
	}
	if err := secureUserDir(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, waiter := newTelegram(app, &tdsession.FileStorage{Path: sessionPath()}, nil, nil)
	run := func(f func(context.Context) error) error {
		return waiter.Run(ctx, func(ctx context.Context) error { return client.Run(ctx, f) })
	}
	switch args[0] {
	case "login":
		return run(func(ctx context.Context) error {
			st, err := client.Auth().Status(ctx)
			if err != nil {
				return err
			}
			if !st.Authorized {
				fmt.Println("Log in to Telegram. Telegram sends the code to the Telegram app of the account.")
				if err := auth.NewFlow(termAuth{in: bufio.NewReader(os.Stdin)}, auth.SendCodeOptions{}).Run(ctx, client.Auth()); err != nil {
					return err
				}
				if st, err = client.Auth().Status(ctx); err != nil {
					return err
				}
			}
			fmt.Printf("logged in as %s; session saved in %s; device \"bottrace\" in Settings > Devices\n", accountName(st.User), sessionPath())
			return nil
		})
	case "status":
		if _, err := os.Stat(sessionPath()); err != nil {
			fmt.Println("not logged in: run bottrace user login")
			return nil
		}
		return run(func(ctx context.Context) error {
			st, err := client.Auth().Status(ctx)
			if err != nil {
				return err
			}
			if !st.Authorized {
				fmt.Println("the session is not logged in: run bottrace user login")
				return nil
			}
			fmt.Printf("logged in as %s\n", accountName(st.User))
			allow, err := loadAllowlist()
			if err != nil {
				return err
			}
			peers := loadPeers()
			for _, e := range allow {
				mode := "read only"
				if e.Write {
					mode = "send and tap"
				}
				id := "not resolved yet"
				if p, ok := peers[e.Username]; ok {
					id = strconv.FormatInt(p.ID, 10)
				}
				fmt.Printf("  @%-28s %-13s id %s\n", e.Username, mode, id)
			}
			return nil
		})
	case "logout":
		if _, err := os.Stat(sessionPath()); err != nil {
			fmt.Println("not logged in")
			return nil
		}
		err := run(func(ctx context.Context) error {
			_, err := client.API().AuthLogOut(ctx)
			return err
		})
		if err != nil {
			return err
		}
		_ = os.Remove(peersPath())
		if err := os.Remove(sessionPath()); err != nil {
			return err
		}
		fmt.Println("logged out; session file removed")
		return nil
	}
	return errors.New("bottrace user login|logout|status")
}

func accountName(u *tg.User) string {
	if u == nil {
		return "?"
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return u.FirstName
}

// termAuth asks for the phone, the code and the 2FA password in the terminal, so none of them is in a flag, an env
// var or the shell history.
type termAuth struct{ in *bufio.Reader }

func (a termAuth) line(prompt string) (string, error) {
	fmt.Print(prompt)
	s, err := a.in.ReadString('\n')
	return strings.TrimSpace(s), err
}

func (a termAuth) Phone(context.Context) (string, error) {
	return a.line("Phone number of the account, with + and the country code: ")
}

func (a termAuth) Code(context.Context, *tg.AuthSentCode) (string, error) {
	return a.line("Code from the Telegram app: ")
}

func (a termAuth) Password(context.Context) (string, error) {
	fmt.Print("Two-step password of the account (not shown): ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return string(b), err
}

func (termAuth) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New("Telegram asks the account to accept new terms: open an official Telegram app first")
}

func (termAuth) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("this phone has no Telegram account: bottrace does not sign up")
}

// memoryCopy reads the session file once and keeps later changes in memory, so a rec run never writes the file that
// bottrace user login and user status use. Only bottrace user login writes it.
type memoryCopy struct {
	mu   sync.Mutex
	data []byte
}

func (m *memoryCopy) LoadSession(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		return nil, tdsession.ErrNotFound
	}
	return m.data, nil
}

func (m *memoryCopy) StoreSession(_ context.Context, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = append([]byte(nil), data...)
	return nil
}

type peer struct {
	ID         int64 `json:"id"`
	AccessHash int64 `json:"access_hash"`
}

func loadPeers() map[string]peer {
	out := map[string]peer{}
	if data, err := os.ReadFile(peersPath()); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

// userClient is the account side of a rec session: it records what the allowlisted bot chats get, and sends and
// taps through Telegram.
type userClient struct {
	sess    *session
	scrub   *scrubber
	allow   allowlist
	proxied func(bot string) bool

	client *telegram.Client
	waiter *floodwait.Waiter
	gaps   *updates.Manager
	lock   *os.File
	ctx    context.Context
	ready  chan struct{}
	api    *tg.Client
	self   *tg.User

	mu      sync.Mutex
	peers   map[string]peer
	names   map[int64]string
	seen    map[int]string
	last    map[string][]json.RawMessage
	fetched map[string]bool
}

// newUserClient makes the account side of rec. only, when not empty, keeps the client to those allowlisted bots.
func newUserClient(scrub *scrubber, proxied func(string) bool, only []string, who string) (*userClient, error) {
	app, err := loadApp()
	if err != nil {
		return nil, err
	}
	allow, err := loadAllowlist()
	if err != nil {
		return nil, err
	}
	if err := secureUserDir(); err != nil {
		return nil, err
	}
	full := allow
	if len(only) > 0 {
		var scoped allowlist
		for _, n := range only {
			e, ok := allow.byName(n)
			if !ok {
				return nil, fmt.Errorf("@%s is not on the allowlist (%s)", strings.TrimPrefix(n, "@"), allowlistPath())
			}
			scoped = append(scoped, e)
		}
		allow = scoped
	}
	data, err := os.ReadFile(sessionPath())
	if err != nil {
		return nil, errors.New("no Telegram login yet: run bottrace user login first")
	}
	lock, err := lockUser(who)
	if err != nil {
		return nil, err
	}
	c := &userClient{scrub: scrub, allow: allow, proxied: proxied, lock: lock, ready: make(chan struct{}),
		peers: loadPeers(), names: map[int64]string{}, seen: map[int]string{}, last: map[string][]json.RawMessage{}, fetched: map[string]bool{}}
	for name, p := range c.peers {
		if _, ok := full.match(p.ID, name, true); !ok {
			delete(c.peers, name)
			continue
		}
		c.names[p.ID] = name
	}
	d := tg.NewUpdateDispatcher()
	d.OnNewMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		c.onMessage(e, u.Message, "message")
		return nil
	})
	d.OnEditMessage(func(_ context.Context, e tg.Entities, u *tg.UpdateEditMessage) error {
		c.onMessage(e, u.Message, "edit")
		return nil
	})
	d.OnDeleteMessages(func(_ context.Context, _ tg.Entities, u *tg.UpdateDeleteMessages) error {
		c.onDelete(u.Messages)
		return nil
	})
	gaps := updates.New(updates.Config{Handler: d, BeforeRun: updates.BeforeRunBuffer})
	c.client, c.waiter = newTelegram(app, &memoryCopy{data: data}, gaps, func(w floodwait.FloodWait) {
		c.sess.add(event{Kind: "note", Note: fmt.Sprintf("Telegram asked the account to wait %s (%s)", w.Duration.Round(time.Second), w.Name)})
	})
	c.gaps = gaps
	return c, nil
}

// run connects and streams until ctx ends. It returns at once when the session is not logged in.
func (c *userClient) run(ctx context.Context) error {
	return c.waiter.Run(ctx, func(ctx context.Context) error {
		return c.client.Run(ctx, func(ctx context.Context) error {
			st, err := c.client.Auth().Status(ctx)
			if err != nil {
				return err
			}
			if !st.Authorized {
				return errors.New("the Telegram session is not logged in: run bottrace user login")
			}
			c.api, c.self, c.ctx = c.client.API(), st.User, ctx
			close(c.ready)
			go c.resolveAll(ctx)
			return c.gaps.Run(ctx, c.api, st.User.ID, updates.AuthOptions{IsBot: false, Forget: true})
		})
	})
}

// resolveAll looks up each allowlisted bot that is not in the peer cache yet, so that short updates, which carry no
// user object, still match.
func (c *userClient) resolveAll(ctx context.Context) {
	for _, e := range c.allow {
		c.mu.Lock()
		_, ok := c.peers[e.Username]
		c.mu.Unlock()
		if ok {
			continue
		}
		if _, err := c.resolve(ctx, e); err != nil && ctx.Err() == nil {
			c.sess.add(event{Kind: "note", Bot: e.Username, Note: "could not find @" + e.Username + ": " + err.Error()})
		}
	}
}

func (c *userClient) resolve(ctx context.Context, e allowEntry) (peer, error) {
	res, err := c.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: e.Username})
	if err != nil {
		return peer{}, err
	}
	for _, uc := range res.Users {
		u, ok := uc.(*tg.User)
		if !ok || !strings.EqualFold(u.Username, e.Username) {
			continue
		}
		if _, ok := c.allow.match(u.ID, u.Username, u.Bot); !ok {
			return peer{}, fmt.Errorf("@%s is user %d, which the allowlist does not let in (not a bot, or another id)", e.Username, u.ID)
		}
		p := peer{ID: u.ID, AccessHash: u.AccessHash}
		c.remember(e.Username, p)
		return p, nil
	}
	return peer{}, fmt.Errorf("@%s not found", e.Username)
}

func (c *userClient) remember(name string, p peer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.peers[name]; ok {
		if old == p {
			return
		}
		delete(c.names, old.ID)
	}
	c.peers[name] = p
	c.names[p.ID] = name
	if data, err := json.Marshal(c.peers); err == nil {
		_ = os.WriteFile(peersPath(), data, 0o600)
	}
}

// botFor gives the allowlisted bot of a private chat, or false. Nothing about other chats is kept.
func (c *userClient) botFor(id int64, e tg.Entities) (*tg.User, allowEntry, bool) {
	if u, ok := e.Users[id]; ok && u.Username != "" {
		entry, ok := c.allow.match(u.ID, u.Username, u.Bot)
		if !ok {
			return nil, allowEntry{}, false
		}
		if u.AccessHash != 0 && !u.Min {
			c.remember(entry.Username, peer{ID: u.ID, AccessHash: u.AccessHash})
		}
		return u, entry, true
	}
	c.mu.Lock()
	name, ok := c.names[id]
	c.mu.Unlock()
	if !ok {
		return nil, allowEntry{}, false
	}
	entry, ok := c.allow.match(id, name, true)
	if !ok {
		return nil, allowEntry{}, false
	}
	return &tg.User{ID: id, Username: name, Bot: true}, entry, true
}

func (c *userClient) onMessage(e tg.Entities, mc tg.MessageClass, method string) {
	m, ok := mc.(*tg.Message)
	if !ok {
		return
	}
	pu, ok := m.PeerID.(*tg.PeerUser)
	if !ok {
		return
	}
	bot, entry, ok := c.botFor(pu.UserID, e)
	if !ok {
		return
	}
	from := bot
	if m.Out {
		from = c.self
	}
	conv := userMessage(m, bot, from)
	raw, _ := json.Marshal(conv)
	c.mu.Lock()
	c.seen[m.ID] = entry.Username
	list := slices.DeleteFunc(c.last[entry.Username], func(r json.RawMessage) bool { return rawID(r) == m.ID })
	if !m.Out {
		list = append(list, raw)
	}
	if len(list) > keepMessages {
		list = list[len(list)-keepMessages:]
	}
	c.last[entry.Username] = list
	c.mu.Unlock()
	c.sess.add(event{Kind: "tg_user", Bot: entry.Username, Method: method, Result: c.scrub.value(conv)})
	go c.fetchMedia(c.ctx, entry.Username, m)
}

func (c *userClient) onDelete(ids []int) {
	byBot := map[string][]int{}
	c.mu.Lock()
	for _, id := range ids {
		if name, ok := c.seen[id]; ok {
			byBot[name] = append(byBot[name], id)
			delete(c.seen, id)
			c.last[name] = slices.DeleteFunc(c.last[name], func(r json.RawMessage) bool { return rawID(r) == id })
		}
	}
	c.mu.Unlock()
	for name, list := range byBot {
		c.sess.add(event{Kind: "tg_user", Bot: name, Method: "delete", Params: map[string]any{"message_ids": list}})
	}
}

func rawID(r json.RawMessage) int {
	var m struct {
		MessageID int `json:"message_id"`
	}
	_ = json.Unmarshal(r, &m)
	return m.MessageID
}

func (c *userClient) claim(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched[key] {
		return false
	}
	c.fetched[key] = true
	return true
}

// fetchMedia saves the picture of the message and its custom emoji for the pages. Custom emoji of a bot behind the
// proxy come from the bot side, under the same key.
func (c *userClient) fetchMedia(ctx context.Context, bot string, m *tg.Message) {
	switch t := m.Media.(type) {
	case *tg.MessageMediaPhoto:
		if p, ok := t.Photo.(*tg.Photo); ok {
			key := "u:photo:" + strconv.FormatInt(p.ID, 10)
			if typ, _, _ := photoThumb(p, maxMediaSide); typ != "" && c.claim(key) {
				c.save(ctx, bot, key, "photo.jpg", &tg.InputPhotoFileLocation{ID: p.ID, AccessHash: p.AccessHash, FileReference: p.FileReference, ThumbSize: typ})
			}
		}
	case *tg.MessageMediaDocument:
		if d, ok := t.Document.(*tg.Document); ok && d.Size <= maxMediaBytes {
			key := "u:doc:" + strconv.FormatInt(d.ID, 10)
			if c.claim(key) {
				c.save(ctx, bot, key, "file", &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference})
			}
		}
	}
	if c.proxied != nil && c.proxied(bot) {
		return
	}
	var ids []int64
	for _, e := range m.Entities {
		if ce, ok := e.(*tg.MessageEntityCustomEmoji); ok {
			ids = append(ids, ce.DocumentID)
		}
	}
	if rm, ok := m.ReplyMarkup.(*tg.ReplyInlineMarkup); ok {
		for _, r := range rm.Rows {
			for _, b := range r.Buttons {
				if st, ok := b.GetStyle(); ok && st.Icon != 0 {
					ids = append(ids, st.Icon)
				}
			}
		}
	}
	var want []int64
	for _, id := range ids {
		if !slices.Contains(want, id) && c.claim("emoji:"+strconv.FormatInt(id, 10)) {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return
	}
	docs, err := c.api.MessagesGetCustomEmojiDocuments(ctx, want)
	if err != nil {
		c.sess.add(event{Kind: "note", Bot: bot, Note: "could not get custom emoji for the pages: " + err.Error()})
		return
	}
	for _, dc := range docs {
		d, ok := dc.(*tg.Document)
		if !ok {
			continue
		}
		loc := &tg.InputDocumentFileLocation{ID: d.ID, AccessHash: d.AccessHash, FileReference: d.FileReference}
		if d.MimeType != "image/webp" {
			thumb := ""
			for _, s := range d.Thumbs {
				if ps, ok := s.(*tg.PhotoSize); ok {
					thumb = ps.Type
				}
			}
			if thumb == "" {
				continue
			}
			loc.ThumbSize = thumb
		}
		id := strconv.FormatInt(d.ID, 10)
		c.save(ctx, bot, "emoji:"+id, "emoji-"+id+".webp", loc)
	}
}

func (c *userClient) save(ctx context.Context, bot, key, name string, loc tg.InputFileLocationClass) {
	var buf bytes.Buffer
	if _, err := downloader.NewDownloader().Download(c.api, loc).Stream(ctx, &buf); err != nil {
		if ctx.Err() == nil {
			c.sess.add(event{Kind: "note", Bot: bot, Note: "could not download a file for the pages: " + err.Error()})
		}
		return
	}
	rel, err := c.sess.saveMedia(c.sess.nextSeq(), key+"-"+name, buf.Bytes())
	if err != nil {
		return
	}
	c.sess.add(event{Kind: "media", Bot: bot, Media: []mediaRef{{UniqueID: key, Path: rel, Size: buf.Len()}}})
}

func (c *userClient) waitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return errors.New("the Telegram client is not connected yet")
	}
}

// pick finds the bot of a user command: the named one, else the only bot of this run, else the only allowlisted bot
// that sent a message in this session, else the only allowlisted bot behind the proxy.
func (c *userClient) pick(name string, write bool, proxyBots []string) (allowEntry, error) {
	if name != "" {
		e, ok := c.allow.byName(name)
		if !ok {
			return allowEntry{}, fmt.Errorf("@%s is not on the allowlist (%s)", strings.TrimPrefix(name, "@"), allowlistPath())
		}
		if write && !e.Write {
			return allowEntry{}, fmt.Errorf("@%s is read only on the allowlist: bottrace does not send or tap there", e.Username)
		}
		return e, nil
	}
	fits := func(names []string) []allowEntry {
		var out []allowEntry
		for _, n := range names {
			if e, ok := c.allow.byName(n); ok && (e.Write || !write) && !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
		return out
	}
	var all []string
	for _, e := range c.allow {
		all = append(all, e.Username)
	}
	if l := fits(all); len(l) == 1 {
		return l[0], nil
	}
	c.mu.Lock()
	var active []string
	for n, l := range c.last {
		if len(l) > 0 {
			active = append(active, n)
		}
	}
	c.mu.Unlock()
	if l := fits(active); len(l) == 1 {
		return l[0], nil
	}
	if l := fits(proxyBots); len(l) == 1 {
		return l[0], nil
	}
	var names []string
	for _, e := range c.allow {
		if e.Write || !write {
			names = append(names, "@"+e.Username)
		}
	}
	return allowEntry{}, fmt.Errorf("pass -bot: one of %s", strings.Join(names, ", "))
}

func (c *userClient) inputPeer(ctx context.Context, e allowEntry) (*tg.InputPeerUser, error) {
	c.mu.Lock()
	p, ok := c.peers[e.Username]
	c.mu.Unlock()
	if !ok || (e.ID != 0 && p.ID != e.ID) {
		var err error
		if p, err = c.resolve(ctx, e); err != nil {
			return nil, err
		}
	}
	return &tg.InputPeerUser{UserID: p.ID, AccessHash: p.AccessHash}, nil
}

func (c *userClient) newest(e allowEntry) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.last[e.Username]
	if len(l) == 0 {
		return nil, fmt.Errorf("the account got no message from @%s since rec started: bottrace send -user /start", e.Username)
	}
	var m map[string]any
	err := json.Unmarshal(l[len(l)-1], &m)
	return m, err
}

// send writes a message to the bot as the account.
func (c *userClient) send(ctx context.Context, e allowEntry, text string) (event, error) {
	pr, err := c.inputPeer(ctx, e)
	if err != nil {
		return event{}, err
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	start := time.Now()
	res, err := c.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: pr, Message: text, RandomID: int64(binary.LittleEndian.Uint64(rnd[:]))})
	ev := event{Kind: "tg_user", Bot: e.Username, Method: "send", Params: map[string]any{"text": text}, T: start, Ms: time.Since(start).Milliseconds()}
	if err != nil {
		ev.Error = c.scrub.text(err.Error())
	} else {
		ev.OK = true
		ev.Result = map[string]any{"message_id": sentID(res)}
	}
	return c.sess.add(c.scrubEvent(ev)), nil
}

func sentID(res tg.UpdatesClass) int {
	switch t := res.(type) {
	case *tg.UpdateShortSentMessage:
		return t.ID
	case *tg.Updates:
		for _, u := range t.Updates {
			if m, ok := u.(*tg.UpdateMessageID); ok {
				return m.ID
			}
		}
	}
	return 0
}

// tap presses a callback button of the newest message from the bot that has it, through Telegram, and records the
// bot's real answer: the toast or alert text.
func (c *userClient) tap(ctx context.Context, e allowEntry, req tapRequest) (event, string, error) {
	c.mu.Lock()
	var msgs []json.RawMessage
	for _, r := range slices.Backward(c.last[e.Username]) {
		if req.Msg == 0 || rawID(r) == req.Msg {
			msgs = append(msgs, r)
		}
	}
	c.mu.Unlock()
	raw, btn, err := findButton(msgs, req)
	if errors.Is(err, errNoButton) {
		return event{}, "", fmt.Errorf("no button %q on the messages the account got from @%s since rec started (bottrace screen -user shows the newest one)", cmpOr(req.Label, req.Data), e.Username)
	}
	if err != nil {
		return event{}, "", err
	}
	if btn.Data == "" {
		return event{}, "", fmt.Errorf("button %q has no callback data: it is a %s button %s", btn.Text, btn.Kind, btn.URL)
	}
	pr, err := c.inputPeer(ctx, e)
	if err != nil {
		return event{}, "", err
	}
	msgID := rawID(raw)
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	ans, err := c.api.MessagesGetBotCallbackAnswer(tctx, &tg.MessagesGetBotCallbackAnswerRequest{Peer: pr, MsgID: msgID, Data: callbackBytes(btn.Data)})
	ev := event{Kind: "tg_user", Bot: e.Username, Method: "tap", T: start, Ms: time.Since(start).Milliseconds(),
		Params: map[string]any{"msg_id": msgID, "data": btn.Data, "label": btn.Text}}
	if err != nil {
		ev.Error = err.Error()
	} else {
		ev.OK = true
		ev.Result = map[string]any{"text": ans.Message, "alert": ans.Alert, "url": ans.URL, "cache_time": ans.CacheTime}
	}
	return c.sess.add(c.scrubEvent(ev)), btn.Text, nil
}

func (c *userClient) scrubEvent(e event) event {
	e.Error = c.scrub.text(e.Error)
	if e.Params != nil {
		e.Params = c.scrub.value(e.Params)
	}
	if e.Result != nil {
		e.Result = c.scrub.value(e.Result)
	}
	return e
}
