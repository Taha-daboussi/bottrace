package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxCapture = 32 << 20

type proxy struct {
	ca       *authority
	sess     *session
	scrub    *scrubber
	bots     *bots
	upstream *http.Transport
	tunnel   []string
	user     *userClient
}

func newUpstream() *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.connect(w, r)
	case r.URL.IsAbs():
		p.forward(w, r, r.URL.Scheme, r.URL.Host)
	default:
		http.Error(w, "bottrace is a proxy: set HTTPS_PROXY to this address", http.StatusBadRequest)
	}
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func (p *proxy) tunneled(host string) bool {
	return slices.ContainsFunc(p.tunnel, func(t string) bool { return host == t || strings.HasSuffix(host, "."+t) })
}

func (p *proxy) connect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		return
	}
	target := r.Host
	host := hostOnly(target)
	if p.tunneled(host) {
		p.pipe(conn, target, "listed")
		return
	}
	hello, prefix, err := sniffHello(conn)
	if err != nil {
		conn.Close()
		return
	}
	// A client that speaks only HTTP/2 (gRPC) cannot be served over the HTTP/1.1 this proxy opens: pass it through.
	if len(hello.protos) > 0 && !slices.Contains(hello.protos, "http/1.1") {
		p.pipe(&prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(prefix), conn)}, target, "http2 only")
		return
	}
	name := hello.serverName
	if name == "" {
		name = host
	}
	tconn := tls.Server(&prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(prefix), conn)}, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return p.ca.leaf(name) },
		NextProtos:     []string{"http/1.1"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = tconn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		p.sess.add(event{Kind: "note", Note: "TLS handshake with the bot failed for " + safeHost(host) + ": " + err.Error()})
		conn.Close()
		return
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.forward(w, r, "https", target) }),
		ReadHeaderTimeout: 2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	_ = srv.Serve(newOneConn(tconn))
}

// pipe passes the connection through to target unopened.
func (p *proxy) pipe(client net.Conn, target string, why string) {
	defer client.Close()
	up, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		p.sess.add(event{Kind: "tunnel", Note: why, HTTP: &httpSum{Method: "CONNECT", Host: safeHost(hostOnly(target)), Error: err.Error()}})
		return
	}
	defer up.Close()
	started := time.Now()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
	p.sess.add(event{Kind: "tunnel", Note: why, Ms: time.Since(started).Milliseconds(), HTTP: &httpSum{Method: "CONNECT", Host: safeHost(hostOnly(target))}})
}

type exchange struct {
	started  time.Time
	reqBody  []byte
	status   int
	header   http.Header
	resBody  []byte
	resTotal int
	err      error
	upgraded bool
	streamed bool
}

func (p *proxy) forward(w http.ResponseWriter, r *http.Request, scheme, target string) {
	host := hostOnly(target)
	if host == "api.telegram.org" {
		p.telegram(w, r, scheme, target)
		return
	}
	x := p.roundTrip(w, r, scheme, target, nil)
	p.recordHTTP(r, x)
}

// roundTrip sends r on to target and writes the answer to w. body, when set, replaces the request body.
func (p *proxy) roundTrip(w http.ResponseWriter, r *http.Request, scheme, target string, body []byte) *exchange {
	x := &exchange{started: time.Now()}
	switch {
	case body != nil:
		x.reqBody = body
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	case r.Body != nil && r.Body != http.NoBody && !isUpgrade(r):
		head, rest, whole := capture(r.Body)
		x.reqBody = head
		if whole {
			r.Body = io.NopCloser(bytes.NewReader(head))
		} else {
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(head), rest), r.Body}
		}
	}
	addr := target
	switch {
	case scheme == "https" && strings.HasSuffix(addr, ":443"):
		addr = strings.TrimSuffix(addr, ":443")
	case scheme == "http" && strings.HasSuffix(addr, ":80"):
		addr = strings.TrimSuffix(addr, ":80")
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = scheme
			pr.Out.URL.Host = addr
			if pr.In.Host != "" {
				pr.Out.Host = pr.In.Host
			}
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport:     p.upstream,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(res *http.Response) error {
			x.status = res.StatusCode
			x.header = res.Header
			if res.StatusCode == http.StatusSwitchingProtocols {
				x.upgraded = true
				return nil
			}
			if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
				x.streamed = true
				return nil
			}
			head, rest, whole := capture(res.Body)
			x.resBody = head
			x.resTotal = len(head)
			if whole {
				res.Body.Close()
				res.Body = io.NopCloser(bytes.NewReader(head))
				res.ContentLength = int64(len(head))
				res.Header.Set("Content-Length", strconv.Itoa(len(head)))
				return nil
			}
			res.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(head), rest), res.Body}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			x.err = err
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
	return x
}

// capture reads up to maxCapture bytes. whole says the body ended within them; else rest yields the remainder.
func capture(body io.Reader) (head []byte, rest io.Reader, whole bool) {
	buf, err := io.ReadAll(io.LimitReader(body, maxCapture))
	if err != nil || len(buf) < maxCapture {
		return buf, nil, true
	}
	return buf, body, false
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

type helloInfo struct {
	serverName string
	protos     []string
}

var errSniffed = errors.New("sniffed")

// sniffHello reads the client's TLS ClientHello without answering it, and returns the bytes it read.
func sniffHello(conn net.Conn) (helloInfo, []byte, error) {
	var buf bytes.Buffer
	var info helloInfo
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	err := tls.Server(readOnly{r: io.TeeReader(conn, &buf)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			info = helloInfo{serverName: h.ServerName, protos: slices.Clone(h.SupportedProtos)}
			return nil, errSniffed
		},
	}).Handshake()
	_ = conn.SetReadDeadline(time.Time{})
	if !errors.Is(err, errSniffed) {
		return info, nil, err
	}
	return info, buf.Bytes(), nil
}

type readOnly struct {
	net.Conn
	r io.Reader
}

func (c readOnly) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (readOnly) Write(p []byte) (int, error)      { return len(p), nil }
func (readOnly) Close() error                     { return nil }
func (readOnly) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (readOnly) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (readOnly) SetDeadline(time.Time) error      { return nil }
func (readOnly) SetReadDeadline(time.Time) error  { return nil }
func (readOnly) SetWriteDeadline(time.Time) error { return nil }

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneConn is a listener that hands out one connection, then waits for it to close.
type oneConn struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func newOneConn(c net.Conn) *oneConn {
	l := &oneConn{done: make(chan struct{})}
	l.conn = &closeSignal{Conn: c, done: l.done}
	return l
}

func (l *oneConn) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }

type closeSignal struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closeSignal) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}
