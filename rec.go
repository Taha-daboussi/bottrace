package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAddr    = "127.0.0.1:8899"
	defaultControl = "127.0.0.1:8898"
	defaultTunnel  = "grafana.net,grafana.com,proxy.golang.org,sum.golang.org,storage.googleapis.com"
)

func envLine(addr string) string {
	return fmt.Sprintf("HTTPS_PROXY=http://%s SSL_CERT_FILE=%s", addr, bundlePath())
}

func cmdEnv() error {
	if _, err := os.Stat(bundlePath()); err != nil {
		if _, err := loadAuthority(); err != nil {
			return err
		}
	}
	fmt.Println(envLine(defaultAddr))
	return nil
}

func cmdRec(args []string) error {
	fs := flag.NewFlagSet("rec", flag.ContinueOnError)
	name := fs.String("name", "", "session name")
	addr := fs.String("addr", defaultAddr, "proxy address (keep it on 127.0.0.1)")
	control := fs.String("control", defaultControl, "control address for tap, send and screen")
	tunnel := fs.String("tunnel", defaultTunnel, "hosts passed through unopened and unrecorded")
	withUser := fs.Bool("user", false, "also run the Telegram user client (log in first with bottrace user login)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ca, err := loadAuthority()
	if err != nil {
		return err
	}
	p := &proxy{ca: ca, scrub: newScrubber(), bots: newBots(), upstream: newUpstream()}
	if *withUser {
		if p.user, err = newUserClient(p.scrub, p.bots.proxied); err != nil {
			return err
		}
	}
	sess, err := newSession(*name)
	if err != nil {
		return err
	}
	defer sess.close()
	p.sess = sess
	for _, h := range strings.Split(*tunnel, ",") {
		if h = strings.TrimSpace(h); h != "" {
			p.tunnel = append(p.tunnel, h)
		}
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	cln, err := net.Listen("tcp", *control)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: time.Minute, ErrorLog: log.New(io.Discard, "", 0)}
	csrv := &http.Server{Handler: p.controlMux(), ReadHeaderTimeout: time.Minute}
	go func() { _ = srv.Serve(ln) }()
	go func() { _ = csrv.Serve(cln) }()

	fmt.Printf("bottrace recording to %s\n\nStart the bot with:\n  %s devbot run <worktree>\n\n", sess.dir, envLine(*addr))
	fmt.Println("Then: bottrace send /start · bottrace tap \"<button>\" · bottrace screen · Ctrl-C to stop and render")

	go p.printLive()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	userCtx, stopUser := context.WithCancel(context.Background())
	defer stopUser()
	userDone := make(chan error, 1)
	if p.user != nil {
		p.user.sess = sess
		go func() { userDone <- p.user.run(userCtx) }()
		select {
		case <-p.user.ready:
			fmt.Printf("User client: logged in as %s, %d chats on the allowlist. Then: bottrace send -user /start · bottrace tap -user \"<button>\"\n", accountName(p.user.self), len(p.user.allow))
		case err := <-userDone:
			return fmt.Errorf("user client: %w", err)
		case <-sig:
			return nil
		}
	}
	select {
	case <-sig:
	case err := <-userDone:
		sess.add(event{Kind: "note", Note: "the user client stopped: " + p.scrub.text(fmt.Sprint(err))})
		<-sig
	}
	stopUser()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = csrv.Shutdown(ctx)
	_ = srv.Close()
	fmt.Println()
	out, err := renderSession(sess.dir)
	if err != nil {
		return err
	}
	printRender(out)
	return nil
}

// printLive prints one short line for each user action and bot call, so the terminal shows the session as it goes.
func (p *proxy) printLive() {
	c, _ := p.sess.subscribe()
	for e := range c {
		switch e.Kind {
		case "tg_update":
			fmt.Printf("%s  user  %s\n", e.T.Format("15:04:05"), updateLine(e))
		case "tg_call":
			status := "ok"
			if e.Error != "" {
				status = e.Error
			}
			fmt.Printf("%s  bot   %-22s %5d ms  %s\n", e.T.Format("15:04:05"), e.Method, e.Ms, status)
		case "http":
			if e.HTTP != nil {
				extra := strings.Join(e.HTTP.RPC, ",")
				if len(e.HTTP.Tx) > 0 {
					extra += " tx " + strings.Join(e.HTTP.Tx, ",")
				}
				if e.HTTP.Error != "" {
					extra += " ERROR " + e.HTTP.Error
				}
				fmt.Printf("%s  out   %s %s %d %d ms %s\n", e.T.Format("15:04:05"), e.HTTP.Method, e.HTTP.Host, e.HTTP.Status, e.Ms, extra)
			}
		case "note":
			fmt.Printf("%s  note  %s\n", e.T.Format("15:04:05"), e.Note)
		case "tg_user":
			fmt.Printf("%s  acct  @%s %s\n", e.T.Format("15:04:05"), e.Bot, accountLine(e))
		}
	}
}

// accountLine is one short line for an event of the user client.
func accountLine(e event) string {
	params, _ := e.Params.(map[string]any)
	res, _ := e.Result.(map[string]any)
	switch e.Method {
	case "tap":
		return fmt.Sprintf("tap %q → %s %d ms", str(params["label"]), answerText(e), e.Ms)
	case "send":
		if e.Error != "" {
			return fmt.Sprintf("send %q → %s", str(params["text"]), e.Error)
		}
		return fmt.Sprintf("send %q", str(params["text"]))
	case "delete":
		return fmt.Sprintf("deleted %v", params["message_ids"])
	}
	return fmt.Sprintf("%s msg %v", e.Method, res["message_id"])
}

// answerText is the bot's answer to a real tap, as the account saw it.
func answerText(e event) string {
	if e.Error != "" {
		return "error: " + e.Error
	}
	res, _ := e.Result.(map[string]any)
	kind := "toast"
	if res["alert"] == true {
		kind = "alert"
	}
	switch {
	case str(res["text"]) != "":
		return kind + ": " + str(res["text"])
	case str(res["url"]) != "":
		return "url: " + str(res["url"])
	}
	return "no toast text"
}

func updateLine(e event) string {
	u, _ := e.Update.(map[string]any)
	src := ""
	if e.Injected {
		src = " (bottrace)"
	}
	if q, ok := u["callback_query"].(map[string]any); ok {
		return "tap " + str(q["data"]) + src
	}
	if m, ok := u["message"].(map[string]any); ok {
		return "message " + strconv.Quote(str(m["text"])) + src
	}
	return "update" + src
}

func (p *proxy) controlMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tap", func(w http.ResponseWriter, r *http.Request) { p.controlInject(w, r, true) })
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) { p.controlInject(w, r, false) })
	mux.HandleFunc("GET /screen", func(w http.ResponseWriter, r *http.Request) {
		chat, _ := strconv.ParseInt(r.URL.Query().Get("chat"), 10, 64)
		b, err := p.bots.pick(chat)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		m, err := b.newest(chat)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		st := fromMessage(m)
		st.Text = p.scrub.text(st.Text)
		fmt.Fprintf(w, "msg %d\n%s", st.ID, textGrid(st, r.URL.Query().Get("data") == "1"))
	})
	mux.HandleFunc("POST /user/tap", func(w http.ResponseWriter, r *http.Request) { p.controlUser(w, r, true) })
	mux.HandleFunc("POST /user/send", func(w http.ResponseWriter, r *http.Request) { p.controlUser(w, r, false) })
	mux.HandleFunc("GET /user/screen", func(w http.ResponseWriter, r *http.Request) {
		if p.user == nil {
			http.Error(w, "rec runs without -user", http.StatusNotFound)
			return
		}
		e, err := p.user.pick(r.URL.Query().Get("bot"), false, p.bots.usernames())
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		m, err := p.user.newest(e)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		st := fromMessage(m)
		st.Text = p.scrub.text(st.Text)
		fmt.Fprintf(w, "@%s msg %d (as the account got it)\n%s", e.Username, st.ID, textGrid(st, r.URL.Query().Get("data") == "1"))
	})
	return mux
}

// controlUser sends or taps through the real Telegram, then prints the bot's answer, each message as the account
// got it, and the bot's calls when the bot runs behind the proxy.
func (p *proxy) controlUser(w http.ResponseWriter, r *http.Request, tap bool) {
	if p.user == nil {
		http.Error(w, "rec runs without -user", http.StatusNotFound)
		return
	}
	var req tapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := p.user.waitReady(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	e, err := p.user.pick(req.Bot, true, p.bots.usernames())
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	sub, unsub := p.sess.subscribe()
	defer unsub()
	var b bytes.Buffer
	if tap {
		ev, label, err := p.user.tap(ctx, e, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		fmt.Fprintf(&b, "→ tapped %q in @%s through Telegram (%d ms)\n%s\n", label, e.Username, ev.Ms, answerText(ev))
	} else {
		ev, err := p.user.send(ctx, e, req.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		fmt.Fprintf(&b, "→ sent %q to @%s through Telegram (%d ms)\n", req.Text, e.Username, ev.Ms)
		if ev.Error != "" {
			fmt.Fprintf(&b, "error: %s\n", ev.Error)
		}
	}
	got := collectEvents(sub, func(x event) bool {
		return (x.Kind == "tg_user" && x.Bot == e.Username && x.Method != "tap" && x.Method != "send") ||
			(x.Kind == "tg_call" && strings.EqualFold(x.Bot, e.Username))
	})
	var calls []event
	for _, x := range got {
		if x.Kind == "tg_call" {
			calls = append(calls, x)
			continue
		}
		if x.Method == "delete" {
			params, _ := x.Params.(map[string]any)
			fmt.Fprintf(&b, "\ndeleted msg %v\n", params["message_ids"])
			continue
		}
		m, _ := x.Result.(map[string]any)
		if meta, _ := m["user_meta"].(map[string]any); meta["out"] == true {
			continue
		}
		st := fromMessage(m)
		fmt.Fprintf(&b, "\n── msg %d as the account got it (%s)\n%s", st.ID, x.Method, textGrid(st, false))
	}
	if len(calls) > 0 {
		fmt.Fprintf(&b, "\n── the bot's calls (bot side)%s", callsText(calls))
	}
	if len(got) == 0 {
		fmt.Fprintf(&b, "\n(no new message from @%s in 20 s)\n", e.Username)
	}
	_, _ = w.Write(b.Bytes())
}

func (p *proxy) controlInject(w http.ResponseWriter, r *http.Request, tap bool) {
	var req tapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b, err := p.bots.pick(req.Chat)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	sub, unsub := p.sess.subscribe()
	defer unsub()
	var pu *pendingUpdate
	var head string
	if tap {
		var label string
		pu, label, err = b.tap(req)
		head = fmt.Sprintf("→ tapped %q", label)
	} else {
		pu, err = b.send(req)
		head = fmt.Sprintf("→ sent %q", req.Text)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	select {
	case <-pu.done:
	case <-time.After(45 * time.Second):
		if b.unqueue(pu) {
			http.Error(w, "the bot did not ask Telegram for updates in 45 s: is it running with the env line of `bottrace env`?", http.StatusGatewayTimeout)
			return
		}
	case <-r.Context().Done():
		b.unqueue(pu)
		return
	}
	calls := collectCalls(sub, b.name())
	fmt.Fprintln(w, head)
	fmt.Fprint(w, callsText(calls))
}

// collectCalls gathers the bot's calls after a user action: up to 20 s for the first one, then until it is quiet
// for 1.5 s, and 60 s at most.
func collectCalls(sub <-chan event, botName string) []event {
	return collectEvents(sub, func(e event) bool { return e.Kind == "tg_call" && e.Bot == botName })
}

func collectEvents(sub <-chan event, keep func(event) bool) []event {
	var calls []event
	first := time.NewTimer(20 * time.Second)
	defer first.Stop()
	limit := time.NewTimer(60 * time.Second)
	defer limit.Stop()
	quiet := time.NewTimer(time.Hour)
	defer quiet.Stop()
	for {
		select {
		case e := <-sub:
			if !keep(e) {
				continue
			}
			calls = append(calls, e)
			first.Stop()
			quiet.Reset(1500 * time.Millisecond)
		case <-first.C:
			return calls
		case <-quiet.C:
			return calls
		case <-limit.C:
			return calls
		}
	}
}

// callsText prints what the bot did: each message it drew, as it looks after the last change, then toasts,
// deletes and errors.
func callsText(calls []event) string {
	screens, _ := buildScreens(calls)
	latest := map[int]screen{}
	var order []int
	for _, s := range screens {
		if _, seen := latest[s.State.ID]; !seen {
			order = append(order, s.State.ID)
		}
		latest[s.State.ID] = s
	}
	var b bytes.Buffer
	for _, id := range order {
		s := latest[id]
		fmt.Fprintf(&b, "\n── msg %d (%s)\n", id, s.Method)
		for _, f := range s.Flags {
			fmt.Fprintf(&b, "! %s: %s\n", f.Level, f.Text)
		}
		b.WriteString(textGrid(s.State, false))
	}
	for _, e := range calls {
		params, _ := e.Params.(map[string]any)
		switch {
		case e.Method == "answerCallbackQuery" && str(params["text"]) != "":
			fmt.Fprintf(&b, "\ntoast: %s\n", str(params["text"]))
		case e.Method == "deleteMessage" && e.OK:
			fmt.Fprintf(&b, "\ndeleted msg %v\n", params["message_id"])
		case e.Error != "" && !screenMethods[e.Method]:
			fmt.Fprintf(&b, "\nerror: %s: %s\n", e.Method, e.Error)
		}
	}
	if len(calls) == 0 {
		b.WriteString("\n(the bot made no Telegram call in 20 s)\n")
	}
	return b.String()
}

func controlURL(path string) string {
	addr := os.Getenv("BOTTRACE_CONTROL")
	if addr == "" {
		addr = defaultControl
	}
	return "http://" + addr + path
}

func cmdTap(args []string) error {
	fs := flag.NewFlagSet("tap", flag.ContinueOnError)
	chat := fs.Int64("chat", 0, "chat id (needed only with more than one chat)")
	msg := fs.Int("msg", 0, "message id (default: the newest message with the button)")
	data := fs.String("data", "", "tap the button with this callback data")
	user, bot := userFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	label := strings.Join(fs.Args(), " ")
	if label == "" && *data == "" {
		return errors.New("tap needs a button label or -data")
	}
	return post(controlPath("/tap", *user), tapRequest{Chat: *chat, Msg: *msg, Data: *data, Label: label, Bot: *bot})
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	chat := fs.Int64("chat", 0, "chat id (needed only with more than one chat)")
	user, bot := userFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" {
		return errors.New("send needs the text")
	}
	return post(controlPath("/send", *user), tapRequest{Chat: *chat, Text: text, Bot: *bot})
}

func cmdScreen(args []string) error {
	fs := flag.NewFlagSet("screen", flag.ContinueOnError)
	chat := fs.Int64("chat", 0, "chat id")
	data := fs.Bool("data", false, "list the callback data of each button")
	user, bot := userFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := "?chat=" + strconv.FormatInt(*chat, 10)
	if *data {
		q += "&data=1"
	}
	if *bot != "" {
		q += "&bot=" + url.QueryEscape(*bot)
	}
	res, err := http.Get(controlURL(controlPath("/screen", *user) + q))
	if err != nil {
		return fmt.Errorf("is `bottrace rec` running? %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return errors.New(strings.TrimSpace(string(body)))
	}
	fmt.Print(string(body))
	return nil
}

func userFlags(fs *flag.FlagSet) (*bool, *string) {
	return fs.Bool("user", false, "go through the real Telegram account (rec must run with -user)"),
		fs.String("bot", "", "the bot of a -user command, as @username (needed only when more than one bot fits)")
}

func controlPath(path string, user bool) string {
	if user {
		return "/user" + path
	}
	return path
}

func post(path string, req tapRequest) error {
	body, _ := json.Marshal(req)
	client := &http.Client{Timeout: 2 * time.Minute}
	res, err := client.Post(controlURL(path), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("is `bottrace rec` running? %w", err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return errors.New(strings.TrimSpace(string(out)))
	}
	fmt.Print(string(out))
	return nil
}
