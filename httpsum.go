package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

var hiddenBodyHosts = []string{"privy.io", "auth", "oauth", "login", "token"}

func bodyHidden(host string) bool {
	h := strings.ToLower(host)
	return slices.ContainsFunc(hiddenBodyHosts, func(s string) bool { return strings.Contains(h, s) })
}

var txFields = map[string]bool{"signature": true, "txhash": true, "tx_hash": true, "txid": true, "transactionhash": true, "txhashes": true, "signatures": true}

var sendMethods = map[string]bool{"sendTransaction": true, "eth_sendRawTransaction": true, "sendBundle": true, "eth_sendBundle": true}

func (p *proxy) recordHTTP(r *http.Request, x *exchange) {
	host := hostOnly(r.Host)
	if host == "" {
		host = r.URL.Hostname()
	}
	sum := &httpSum{
		Method:   r.Method,
		Host:     safeHost(host),
		Path:     safePath(r.URL),
		Status:   x.status,
		ReqBytes: len(x.reqBody),
		ResBytes: x.resTotal,
	}
	kind := "http"
	switch {
	case x.upgraded:
		kind = "socket"
	case x.err != nil:
		sum.Error = cleanErr(x.err.Error(), r)
	case bodyHidden(host):
		sum.Hidden = true
	default:
		p.summarize(sum, x)
	}
	p.sess.add(event{T: x.started, Kind: kind, Ms: time.Since(x.started).Milliseconds(), HTTP: sum})
}

// cleanErr keeps the error's words and drops the host and path, which can carry a key.
func cleanErr(msg string, r *http.Request) string {
	if r.URL.Path != "" && r.URL.Path != "/" {
		msg = strings.ReplaceAll(msg, r.URL.Path, safePath(r.URL))
	}
	if h := r.URL.Hostname(); h != "" {
		msg = strings.ReplaceAll(msg, h, safeHost(h))
	}
	if r.Host != "" {
		msg = strings.ReplaceAll(msg, r.Host, safeHost(r.Host))
	}
	return msg
}

func (p *proxy) summarize(sum *httpSum, x *exchange) {
	var methods []string
	var reqAny any
	if json.Unmarshal(x.reqBody, &reqAny) == nil {
		switch t := reqAny.(type) {
		case map[string]any:
			if m, ok := t["method"].(string); ok {
				methods = append(methods, m)
			}
		case []any:
			for _, item := range t {
				if o, ok := item.(map[string]any); ok {
					if m, ok := o["method"].(string); ok {
						methods = append(methods, m)
					}
				}
			}
		}
	}
	sum.RPC = methods
	var resAny any
	if json.Unmarshal(x.resBody, &resAny) != nil {
		return
	}
	sending := slices.ContainsFunc(methods, func(m string) bool { return sendMethods[m] })
	var txs []string
	var errs []string
	visit(resAny, "", func(key string, v any) {
		s, ok := v.(string)
		switch {
		case ok && len(methods) == 0 && txFields[strings.ToLower(key)] && looksTx(s):
			txs = append(txs, s)
		case ok && key == "result" && sending && looksTx(s):
			txs = append(txs, s)
		case key == "error":
			switch e := v.(type) {
			case string:
				errs = append(errs, e)
			case map[string]any:
				if m, ok := e["message"].(string); ok {
					errs = append(errs, m)
				}
			}
		}
	})
	slices.Sort(txs)
	txs = slices.Compact(txs)
	for _, tx := range txs {
		p.scrub.allow(tx)
	}
	sum.Tx = txs
	if len(errs) > 0 {
		msg := p.scrub.text(strings.Join(errs, "; "))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		sum.Error = msg
	}
	if sum.Error == "" && x.status >= 400 {
		if m, ok := resAny.(map[string]any); ok {
			if s, ok := m["message"].(string); ok {
				sum.Error = p.scrub.text(s)
			}
		}
	}
}

func visit(v any, key string, fn func(string, any)) {
	fn(key, v)
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == "error" || txFields[strings.ToLower(k)] {
				fn(k, x)
				if arr, ok := x.([]any); ok {
					for _, item := range arr {
						fn(k, item)
					}
				}
				continue
			}
			visit(x, k, fn)
		}
	case []any:
		for _, x := range t {
			visit(x, key, fn)
		}
	}
}

func looksTx(s string) bool {
	if h, ok := strings.CutPrefix(s, "0x"); ok {
		return len(h) == 64 && isHex(h)
	}
	return len(s) >= 80 && len(s) <= 90 && base58Len(s) == 64
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}
