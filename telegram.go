package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type tgReply struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

type upload struct {
	field, name string
	data        []byte
}

// splitBotPath reads /bot<token>/<method>.
func splitBotPath(p string) (token, method string, ok bool) {
	rest, ok := strings.CutPrefix(p, "/bot")
	if !ok {
		return "", "", false
	}
	token, method, ok = strings.Cut(rest, "/")
	return token, method, ok && token != "" && method != ""
}

func (p *proxy) telegram(w http.ResponseWriter, r *http.Request, scheme, target string) {
	token, method, ok := splitBotPath(r.URL.Path)
	if !ok {
		x := p.roundTrip(w, r, scheme, target, nil)
		p.sess.add(event{T: x.started, Kind: "tg_file", Ms: time.Since(x.started).Milliseconds(), HTTP: &httpSum{Method: r.Method, Host: "api.telegram.org", Path: botPath(r.URL.Path), Status: x.status, ResBytes: x.resTotal}})
		return
	}
	b := p.bots.get(token)
	if method == "getUpdates" {
		p.getUpdates(w, r, b, scheme, target)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCapture))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params, uploads := parseParams(r.Header.Get("Content-Type"), body)
	seq := p.sess.nextSeq()
	files := p.saveUploads(seq, uploads)

	if local, note := b.localAnswer(method, params); local != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(local)
		p.sess.add(event{Kind: "tg_call", Bot: b.name(), Method: method, Params: p.decodeParams(params), Files: files, OK: true, Result: true, Local: true, Note: note})
		return
	}
	var note string
	if b.stripInjectedReply(params) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			body = []byte(params.Encode())
			note = "reply to a bottrace message removed"
		}
	}
	x := p.roundTrip(w, r, scheme, target, body)
	e := event{T: x.started, Kind: "tg_call", Bot: b.name(), Method: method, Params: p.decodeParams(params), Files: files, Ms: time.Since(x.started).Milliseconds(), Note: note}
	var reply tgReply
	switch {
	case x.err != nil:
		e.Error = cleanErr(x.err.Error(), r)
		e.Error = botPath(e.Error)
	case json.Unmarshal(x.resBody, &reply) != nil:
		e.Error = fmt.Sprintf("HTTP %d, answer is not JSON", x.status)
	case !reply.OK:
		e.Error = fmt.Sprintf("%d %s", reply.ErrorCode, p.scrub.text(reply.Description))
	default:
		e.OK = true
		var res any
		if json.Unmarshal(reply.Result, &res) == nil {
			e.Media = p.linkMedia(res, files)
			e.Result = p.scrub.value(res)
		}
		b.observeCall(method, params, reply.Result)
	}
	p.sess.add(e)
	if e.OK {
		p.fetchMissing(b, e.Media)
		p.fetchEmoji(b, e.Result)
	}
}

func parseParams(contentType string, body []byte) (url.Values, []upload) {
	mt, mp, _ := mime.ParseMediaType(contentType)
	switch {
	case mt == "multipart/form-data":
		vals := url.Values{}
		var ups []upload
		mr := multipart.NewReader(bytes.NewReader(body), mp["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			if part.FileName() != "" {
				ups = append(ups, upload{field: part.FormName(), name: part.FileName(), data: data})
				vals.Set(part.FormName(), "attach://"+part.FileName())
				continue
			}
			vals.Add(part.FormName(), string(data))
		}
		return vals, ups
	case mt == "application/json":
		var m map[string]any
		vals := url.Values{}
		if json.Unmarshal(body, &m) == nil {
			for k, v := range m {
				if s, ok := v.(string); ok {
					vals.Set(k, s)
					continue
				}
				b, _ := json.Marshal(v)
				vals.Set(k, string(b))
			}
		}
		return vals, nil
	}
	vals, _ := url.ParseQuery(string(body))
	return vals, nil
}

// decodeParams turns the form into JSON values: reply_markup, entities and media are JSON inside the form.
func (p *proxy) decodeParams(vals url.Values) map[string]any {
	if len(vals) == 0 {
		return nil
	}
	out := make(map[string]any, len(vals))
	for k := range vals {
		v := vals.Get(k)
		if strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") {
			var j any
			if json.Unmarshal([]byte(v), &j) == nil {
				out[k] = j
				continue
			}
		}
		out[k] = v
	}
	return p.scrub.value(out).(map[string]any)
}

func (p *proxy) saveUploads(seq int, ups []upload) []mediaRef {
	var out []mediaRef
	for _, u := range ups {
		rel, err := p.sess.saveMedia(seq, u.field+"-"+u.name, u.data)
		if err != nil {
			continue
		}
		out = append(out, mediaRef{Field: u.field, Name: u.name, Path: rel, Size: len(u.data)})
	}
	return out
}

// linkMedia ties the file ids in Telegram's answer to the bytes the bot uploaded, so a later send by file id
// still shows the picture.
func (p *proxy) linkMedia(res any, files []mediaRef) []mediaRef {
	m, ok := res.(map[string]any)
	if !ok {
		return nil
	}
	var out []mediaRef
	for _, f := range messageFiles(m) {
		ref := mediaRef{FileID: f.FileID, UniqueID: f.UniqueID}
		if len(files) > 0 {
			ref.Path = files[0].Path
		}
		out = append(out, ref)
	}
	return out
}

type fileRef struct{ FileID, UniqueID, Kind string }

// messageFiles lists the main file of a message: the largest photo size, or the animation, video or document.
func messageFiles(m map[string]any) []fileRef {
	if sizes, ok := m["photo"].([]any); ok && len(sizes) > 0 {
		if last, ok := sizes[len(sizes)-1].(map[string]any); ok {
			return []fileRef{{str(last["file_id"]), str(last["file_unique_id"]), "photo"}}
		}
	}
	for _, kind := range []string{"animation", "video", "document", "sticker"} {
		if f, ok := m[kind].(map[string]any); ok {
			return []fileRef{{str(f["file_id"]), str(f["file_unique_id"]), kind}}
		}
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// fetchMissing downloads a picture the bot sent by file id and that this session has not seen, so the screen
// page can show it. It runs beside the bot's own calls and is not recorded as one of them.
func (p *proxy) fetchMissing(b *bot, refs []mediaRef) {
	for _, ref := range refs {
		if ref.Path != "" || ref.UniqueID == "" || !b.claimFetch(ref.UniqueID) {
			continue
		}
		go func(ref mediaRef) {
			data, name, err := p.download(b.token, ref.FileID)
			if err != nil {
				p.sess.add(event{Kind: "note", Bot: b.name(), Note: "could not fetch a picture for the screen pages: " + botPath(err.Error())})
				return
			}
			rel, err := p.sess.saveMedia(p.sess.nextSeq(), ref.UniqueID+"-"+name, data)
			if err != nil {
				return
			}
			p.sess.add(event{Kind: "media", Bot: b.name(), Media: []mediaRef{{FileID: ref.FileID, UniqueID: ref.UniqueID, Path: rel, Size: len(data)}}})
		}(ref)
	}
}

func (p *proxy) download(token, fileID string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{Transport: p.upstream}
	form := url.Values{"file_id": {fileID}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/getFile", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return nil, "", errors.New("getFile failed")
	}
	defer res.Body.Close()
	var reply struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(res.Body).Decode(&reply); err != nil || !reply.OK {
		return nil, "", fmt.Errorf("getFile: %s", reply.Description)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.telegram.org/file/bot"+token+"/"+reply.Result.FilePath, nil)
	res2, err := client.Do(req)
	if err != nil {
		return nil, "", errors.New("file download failed")
	}
	defer res2.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res2.Body, 20<<20))
	return data, path.Base(reply.Result.FilePath), err
}

func (p *proxy) getUpdates(w http.ResponseWriter, r *http.Request, b *bot, scheme, target string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params, _ := parseParams(r.Header.Get("Content-Type"), body)
	offset, _ := strconv.Atoi(params.Get("offset"))

	if ups := b.takePending(offset); ups != nil {
		p.answerUpdates(w, b, ups, true)
		return
	}
	if real := b.toReal(offset); real > 0 {
		params.Set("offset", strconv.Itoa(real))
	} else {
		params.Del("offset")
	}
	upURL := *r.URL
	upURL.Scheme = scheme
	upURL.Host = strings.TrimSuffix(target, ":443")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, upURL.String(), strings.NewReader(params.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	type answer struct {
		body []byte
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		res, err := p.upstream.RoundTrip(req)
		if err != nil {
			got <- answer{err: err}
			return
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		got <- answer{data, err}
	}()

	select {
	case a := <-got:
		if a.err != nil {
			if r.Context().Err() == nil {
				p.sess.add(event{Kind: "tg_call", Bot: b.name(), Method: "getUpdates", Error: "upstream: " + botPath(a.err.Error())})
			}
			http.Error(w, "upstream failed", http.StatusBadGateway)
			return
		}
		var reply struct {
			OK     bool              `json:"ok"`
			Result []json.RawMessage `json:"result"`
		}
		if json.Unmarshal(a.body, &reply) != nil || !reply.OK {
			var tr tgReply
			_ = json.Unmarshal(a.body, &tr)
			p.sess.add(event{Kind: "tg_call", Bot: b.name(), Method: "getUpdates", Error: fmt.Sprintf("%d %s", tr.ErrorCode, tr.Description)})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(a.body)
			return
		}
		p.answerUpdates(w, b, b.virtualize(reply.Result), false)
	case <-b.wake:
		cancel()
		ups := b.takePending(offset)
		if ups == nil {
			ups = []json.RawMessage{}
		}
		p.answerUpdates(w, b, ups, true)
	case <-r.Context().Done():
	}
}

func (p *proxy) answerUpdates(w http.ResponseWriter, b *bot, ups []json.RawMessage, injected bool) {
	out, _ := json.Marshal(map[string]any{"ok": true, "result": ups})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
	for _, u := range ups {
		var v any
		if json.Unmarshal(u, &v) != nil {
			continue
		}
		e := event{Kind: "tg_update", Bot: b.name(), Injected: injected}
		if !injected {
			b.observeUpdate(u)
		}
		e.Update = p.scrub.value(v)
		p.sess.add(e)
	}
}

// fetchEmoji downloads the custom emoji a message shows (button icons, emoji in the text), so the screen pages
// draw them as Telegram does. It runs beside the bot's own calls and is not recorded as one of them.
func (p *proxy) fetchEmoji(b *bot, res any) {
	m, ok := res.(map[string]any)
	if !ok {
		return
	}
	var ids []string
	for _, row := range keyboardRows(m["reply_markup"]) {
		for _, btn := range row {
			if btn.Icon != "" && b.claimFetch("emoji:"+btn.Icon) {
				ids = append(ids, btn.Icon)
			}
		}
	}
	for _, key := range []string{"entities", "caption_entities"} {
		list, _ := m[key].([]any)
		for _, e := range list {
			o, _ := e.(map[string]any)
			if id := str(o["custom_emoji_id"]); id != "" && b.claimFetch("emoji:"+id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	go func() {
		for start := 0; start < len(ids); start += 200 {
			batch := ids[start:min(start+200, len(ids))]
			if err := p.fetchEmojiBatch(b, batch); err != nil {
				p.sess.add(event{Kind: "note", Bot: b.name(), Note: "could not fetch custom emoji for the screen pages: " + botPath(err.Error())})
			}
		}
	}()
}

func (p *proxy) fetchEmojiBatch(b *bot, ids []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, _ := json.Marshal(ids)
	form := url.Values{"custom_emoji_ids": {string(list)}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+b.token+"/getCustomEmojiStickers", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := (&http.Client{Transport: p.upstream}).Do(req)
	if err != nil {
		return errors.New("getCustomEmojiStickers failed")
	}
	defer res.Body.Close()
	var reply struct {
		OK     bool `json:"ok"`
		Result []struct {
			CustomEmojiID string `json:"custom_emoji_id"`
			FileID        string `json:"file_id"`
			IsAnimated    bool   `json:"is_animated"`
			IsVideo       bool   `json:"is_video"`
			Thumbnail     *struct {
				FileID string `json:"file_id"`
			} `json:"thumbnail"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(res.Body).Decode(&reply); err != nil || !reply.OK {
		return fmt.Errorf("getCustomEmojiStickers: %s", reply.Description)
	}
	for _, s := range reply.Result {
		fileID := s.FileID
		if s.IsAnimated || s.IsVideo {
			if s.Thumbnail == nil {
				continue
			}
			fileID = s.Thumbnail.FileID
		}
		data, name, err := p.download(b.token, fileID)
		if err != nil {
			continue
		}
		rel, err := p.sess.saveMedia(p.sess.nextSeq(), "emoji-"+s.CustomEmojiID+"-"+name, data)
		if err != nil {
			continue
		}
		p.sess.add(event{Kind: "media", Bot: b.name(), Media: []mediaRef{{UniqueID: "emoji:" + s.CustomEmojiID, Path: rel, Size: len(data)}}})
	}
	return nil
}
