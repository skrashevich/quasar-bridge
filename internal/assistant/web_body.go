package assistant

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/html/charset"
)

var errWebScriptOrigin = errors.New("web player script must stay on the page origin")

type webBody struct {
	url       string
	typ       string
	data      []byte
	truncated bool
}

// readBody shares URL, DNS, redirect, timeout and byte limits. A non-nil origin
// selects the narrower JavaScript reader used only by music discovery.
func (w *WebReader) readBody(ctx context.Context, raw string, origin *url.URL) (webBody, error) {
	if len(raw) > 4096 {
		return webBody{}, errors.New("web URL exceeds limit")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return webBody{}, errors.New("invalid web URL")
	}
	if err := validateWebURL(u); err != nil {
		return webBody{}, err
	}
	if origin != nil && !sameWebOrigin(origin, u) {
		return webBody{}, errWebScriptOrigin
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(w.config.WebTimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return webBody{}, errors.New("invalid web URL")
	}
	req.Header.Set("User-Agent", "QuasarBridge/0.3 (text reader)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json")
	client := w.http
	if origin != nil {
		req.Header.Set("Accept", "application/javascript,text/javascript")
		copy := *w.http
		copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errWebRedirects
			}
			if err := validateWebURL(req.URL); err != nil {
				return err
			}
			if !sameWebOrigin(origin, req.URL) {
				return errWebScriptOrigin
			}
			if w.http.CheckRedirect != nil {
				return w.http.CheckRedirect(req, via)
			}
			return nil
		}
		client = &copy
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return webBody{}, ctx.Err()
		}
		// url.Error includes the full URL; expose only known URL-free causes.
		for _, cause := range []error{errWebURL, errWebPrivate, errWebPrivateDNS, errWebDNS, errWebConnection, errWebRedirects, errWebScriptOrigin} {
			if errors.Is(err, cause) {
				return webBody{}, cause
			}
		}
		if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
			return webBody{}, errors.New("web TLS certificate verification failed")
		}
		if network, ok := errors.AsType[net.Error](err); ok && network.Timeout() {
			return webBody{}, errors.New("web request timed out")
		}
		return webBody{}, errors.New("web request failed or target blocked")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return webBody{}, fmt.Errorf("web page returned HTTP %d", resp.StatusCode)
	}
	typ, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if origin != nil {
		if typ != "application/javascript" && typ != "text/javascript" {
			return webBody{}, errors.New("web player script content type is not supported")
		}
	} else if typ != "text/html" && typ != "application/xhtml+xml" && typ != "text/plain" && typ != "application/json" {
		return webBody{}, errors.New("web content type is not supported")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(w.config.WebMaxBytes)+1))
	if err := ctx.Err(); err != nil {
		return webBody{}, err
	}
	if err != nil {
		return webBody{}, errors.New("cannot read web page")
	}
	result := webBody{url: u.String(), typ: typ}
	if resp.Request != nil {
		result.url = resp.Request.URL.String()
	}
	if len(data) > w.config.WebMaxBytes {
		if origin != nil {
			return webBody{}, errors.New("web player script exceeds size limit")
		}
		data = data[:w.config.WebMaxBytes]
		result.truncated = true
	}
	var decoder io.Reader
	if typ == "text/html" || typ == "application/xhtml+xml" {
		decoder, err = charset.NewReader(bytes.NewReader(data), resp.Header.Get("Content-Type"))
	} else if params["charset"] != "" {
		decoder, err = charset.NewReaderLabel(params["charset"], bytes.NewReader(data))
	}
	if err != nil {
		return webBody{}, errors.New("web page charset is not supported")
	}
	if decoder != nil {
		data, err = io.ReadAll(io.LimitReader(decoder, int64(w.config.WebMaxBytes)*3+1))
		if err := ctx.Err(); err != nil {
			return webBody{}, err
		}
		if err != nil {
			return webBody{}, errors.New("cannot decode web page")
		}
		if len(data) > w.config.WebMaxBytes*3 {
			if origin != nil {
				return webBody{}, errors.New("web player script exceeds size limit")
			}
			data = data[:w.config.WebMaxBytes*3]
			result.truncated = true
		}
	}
	result.data = data
	return result, nil
}
