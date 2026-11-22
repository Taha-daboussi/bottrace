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
	if err := fs.Parse(args); err != nil {
		return err
	}
	ca, err := loadAuthority()
	if err != nil {
		return err
	}
	sess, err := newSession(*name)
	if err != nil {
		return err
	}
	defer sess.close()
	p := &proxy{ca: ca, sess: sess, scrub: newScrubber(), bots: newBots(), upstream: newUpstream()}
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
	<-sig
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
		}
	}
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
	return mux
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
			if e.Kind != "tg_call" || e.Bot != botName {
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	label := strings.Join(fs.Args(), " ")
	if label == "" && *data == "" {
		return errors.New("tap needs a button label or -data")
	}
	return post("/tap", tapRequest{Chat: *chat, Msg: *msg, Data: *data, Label: label})
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	chat := fs.Int64("chat", 0, "chat id (needed only with more than one chat)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" {
		return errors.New("send needs the text")
	}
	return post("/send", tapRequest{Chat: *chat, Text: text})
}

func cmdScreen(args []string) error {
	fs := flag.NewFlagSet("screen", flag.ContinueOnError)
	chat := fs.Int64("chat", 0, "chat id")
	data := fs.Bool("data", false, "list the callback data of each button")
	if err := fs.Parse(args); err != nil {
		return err
	}
	q := "?chat=" + strconv.FormatInt(*chat, 10)
	if *data {
		q += "&data=1"
	}
	res, err := http.Get(controlURL("/screen" + q))
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
