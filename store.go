package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type event struct {
	Seq      int        `json:"seq"`
	T        time.Time  `json:"t"`
	Kind     string     `json:"kind"` // tg_call, tg_update, tg_file, http, socket, tunnel, media, note
	Bot      string     `json:"bot,omitempty"`
	Method   string     `json:"method,omitempty"`
	Params   any        `json:"params,omitempty"`
	Files    []mediaRef `json:"files,omitempty"`
	OK       bool       `json:"ok,omitempty"`
	Error    string     `json:"error,omitempty"`
	Result   any        `json:"result,omitempty"`
	Ms       int64      `json:"ms,omitempty"`
	Local    bool       `json:"local,omitempty"`
	Update   any        `json:"update,omitempty"`
	Injected bool       `json:"injected,omitempty"`
	HTTP     *httpSum   `json:"http,omitempty"`
	Media    []mediaRef `json:"media,omitempty"`
	Note     string     `json:"note,omitempty"`
}

type mediaRef struct {
	Field    string `json:"field,omitempty"`
	Name     string `json:"name,omitempty"`
	Path     string `json:"path,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	UniqueID string `json:"file_unique_id,omitempty"`
	Size     int    `json:"size,omitempty"`
}

type httpSum struct {
	Method   string   `json:"method"`
	Host     string   `json:"host"`
	Path     string   `json:"path"`
	Status   int      `json:"status,omitempty"`
	ReqBytes int      `json:"req_bytes,omitempty"`
	ResBytes int      `json:"res_bytes,omitempty"`
	RPC      []string `json:"rpc,omitempty"`
	Tx       []string `json:"tx,omitempty"`
	Error    string   `json:"error,omitempty"`
	Hidden   bool     `json:"hidden,omitempty"`
}

type session struct {
	dir  string
	mu   sync.Mutex
	file *os.File
	seq  int
	subs []chan event
}

var nameClean = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func newSession(name string) (*session, error) {
	stamp := time.Now().Format("20060102-150405")
	if name != "" {
		stamp += "-" + nameClean.ReplaceAllString(name, "-")
	}
	dir := filepath.Join(sessionsDir(), stamp)
	if err := os.MkdirAll(filepath.Join(dir, "media"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	latest := filepath.Join(sessionsDir(), "latest")
	_ = os.Remove(latest)
	_ = os.Symlink(stamp, latest)
	return &session{dir: dir, file: f}, nil
}

func (s *session) add(e event) event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	e.Seq = s.seq
	if e.T.IsZero() {
		e.T = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		b, _ = json.Marshal(event{Seq: e.Seq, T: e.T, Kind: "note", Note: "could not encode event: " + err.Error()})
	}
	_, _ = s.file.Write(append(b, '\n'))
	for _, c := range s.subs {
		select {
		case c <- e:
		default:
		}
	}
	return e
}

func (s *session) subscribe() (<-chan event, func()) {
	c := make(chan event, 256)
	s.mu.Lock()
	s.subs = append(s.subs, c)
	s.mu.Unlock()
	return c, func() {
		s.mu.Lock()
		s.subs = slices.DeleteFunc(s.subs, func(x chan event) bool { return x == c })
		s.mu.Unlock()
	}
}

func (s *session) saveMedia(seq int, name string, data []byte) (string, error) {
	rel := filepath.Join("media", fmt.Sprintf("%06d-%s", seq, nameClean.ReplaceAllString(name, "-")))
	return rel, os.WriteFile(filepath.Join(s.dir, rel), data, 0o600)
}

func (s *session) nextSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq + 1
}

func (s *session) close() { _ = s.file.Close() }

func resolveSession(arg string) (string, error) {
	if arg == "" {
		arg = "latest"
	}
	if st, err := os.Stat(arg); err == nil && st.IsDir() {
		return filepath.Abs(arg)
	}
	p := filepath.Join(sessionsDir(), arg)
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return filepath.EvalSymlinks(p)
	}
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		return "", err
	}
	var hits []string
	for _, e := range entries {
		if e.IsDir() && strings.Contains(e.Name(), arg) {
			hits = append(hits, e.Name())
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no session matches %q (bottrace ls)", arg)
	case 1:
		return filepath.Join(sessionsDir(), hits[0]), nil
	}
	return "", fmt.Errorf("%q matches %d sessions: %s", arg, len(hits), strings.Join(hits, ", "))
}

func readEvents(dir string) ([]event, error) {
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

func cmdList() error {
	entries, err := os.ReadDir(sessionsDir())
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("no sessions yet")
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		evs, _ := readEvents(filepath.Join(sessionsDir(), e.Name()))
		calls, actions := 0, 0
		for _, ev := range evs {
			switch ev.Kind {
			case "tg_call":
				calls++
			case "tg_update":
				actions++
			}
		}
		fmt.Printf("%-40s %4d user actions  %4d bot calls  %5d events\n", e.Name(), actions, calls, len(evs))
	}
	return nil
}
