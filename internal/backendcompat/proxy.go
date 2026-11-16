// Package backendcompat adapts cloud transport settings for firmware that
// overrides the local WSS configuration. It never stores or logs payloads.
package backendcompat

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const Prefix = "/yandex-backend"
const maxResponse = 8 << 20

type Proxy struct {
	proxy     *httputil.ReverseProxy
	active    chan struct{}
	requests  atomic.Uint64
	rewritten atomic.Uint64
	unchanged atomic.Uint64
	failures  atomic.Uint64
}

type Status struct {
	Requests  uint64 `json:"requests"`
	Rewritten uint64 `json:"rewritten"`
	Unchanged uint64 `json:"unchanged"`
	Failures  uint64 `json:"failures"`
}

// New has a fixed upstream: clients cannot select a host or disable TLS.
func New() *Proxy {
	target, _ := url.Parse("https://quasar.yandex.net")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Yandex stays direct, independently of the OpenAI proxy.
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	return newProxy(target, transport)
}

func newProxy(target *url.URL, transport http.RoundTripper) *Proxy {
	p := &Proxy{active: make(chan struct{}, 16)}
	p.proxy = &httputil.ReverseProxy{
		Transport: transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.URL.User = nil
			r.Out.URL.Opaque = ""
			r.Out.Header.Del("Proxy-Authorization")
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, Prefix)
			if r.In.URL.RawPath != "" {
				r.Out.URL.RawPath = strings.TrimPrefix(r.In.URL.RawPath, Prefix)
			}
			r.Out.Header.Set("Accept-Encoding", "identity")
			if r.Out.URL.Path == "/get_sync_info" {
				r.Out.Header.Del("If-None-Match")
				r.Out.Header.Del("If-Modified-Since")
			}
		},
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			p.failures.Add(1)
			http.Error(w, "Yandex backend unavailable", http.StatusBadGateway)
		},
	}
	return p
}

func (p *Proxy) Snapshot() Status {
	return Status{p.requests.Load(), p.rewritten.Load(), p.unchanged.Load(), p.failures.Load()}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, Prefix+"/") ||
		(r.URL.RawPath != "" && !strings.HasPrefix(r.URL.RawPath, Prefix+"/")) {
		http.NotFound(w, r)
		return
	}
	select {
	case p.active <- struct{}{}:
		defer func() { <-p.active }()
	default:
		http.Error(w, "backend busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	p.requests.Add(1)
	p.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (p *Proxy) modifyResponse(r *http.Response) error {
	if r.Request.Method != http.MethodGet || r.Request.URL.Path != "/get_sync_info" || r.StatusCode != http.StatusOK {
		return nil
	}
	defer r.Body.Close()
	raw, err := readBounded(r.Body)
	if err != nil {
		return err
	}
	body := raw
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		z, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return errors.New("invalid backend encoding")
		}
		body, err = readBounded(z)
		z.Close()
		if err != nil {
			return err
		}
	default:
		p.unchanged.Add(1)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	patched, changed := patchSync(body)
	if !changed {
		p.unchanged.Add(1)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	p.rewritten.Add(1)
	r.Body = io.NopCloser(bytes.NewReader(patched))
	r.ContentLength = int64(len(patched))
	r.Header.Set("Content-Length", strconv.Itoa(len(patched)))
	r.Header.Set("Cache-Control", "no-store")
	for _, h := range []string{"Content-Encoding", "ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest"} {
		r.Header.Del(h)
	}
	return nil
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxResponse+1))
	if err != nil || len(b) > maxResponse {
		return nil, errors.New("invalid backend response")
	}
	return b, nil
}

// Edit only config.system_config.voiceDialogSettings. Preserve unknown JSON
// members as raw values, including large integers and per-device settings.
// A layer may be an object or a JSON-encoded string. Unexpected schemas pass
// through unchanged so an API change does not destroy the native config.
func patchSync(raw []byte) ([]byte, bool) {
	var envelope map[string]jsontext.Value
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return raw, false
	}
	var status string
	if json.Unmarshal(envelope["status"], &status) != nil || status != "ok" {
		return raw, false
	}
	config, ok := editObject(envelope["config"], func(config map[string]jsontext.Value) bool {
		system, ok := editObject(config["system_config"], func(system map[string]jsontext.Value) bool {
			voice, ok := editObject(system["voiceDialogSettings"], func(voice map[string]jsontext.Value) bool {
				voice["uniProxyPreferQuic"] = jsontext.Value(`false`)
				voice["uniProxyQuicUrl"] = jsontext.Value(`""`)
				return true
			})
			if ok {
				system["voiceDialogSettings"] = voice
			}
			return ok
		})
		if ok {
			config["system_config"] = system
		}
		return ok
	})
	if !ok {
		return raw, false
	}
	envelope["config"] = config
	result, err := json.Marshal(envelope)
	if err != nil {
		return raw, false
	}
	return result, true
}

func editObject(raw jsontext.Value, edit func(map[string]jsontext.Value) bool) (jsontext.Value, bool) {
	if len(raw) == 0 {
		return raw, false
	}
	var encoded string
	quoted := json.Unmarshal(raw, &encoded) == nil
	if quoted {
		raw = jsontext.Value(encoded)
	}
	var object map[string]jsontext.Value
	if json.Unmarshal(raw, &object) != nil || object == nil || !edit(object) {
		return raw, false
	}
	result, err := json.Marshal(object)
	if err != nil {
		return raw, false
	}
	if quoted {
		result, err = json.Marshal(string(result))
	}
	return result, err == nil
}
