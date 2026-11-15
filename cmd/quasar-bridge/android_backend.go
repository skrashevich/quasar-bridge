package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"strings"
)

const androidBackendPrefix = "/android-backend/"

func androidBackendProxyEnabled() (bool, error) {
	switch os.Getenv("QUASAR_ANDROID_BACKEND_PROXY") {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errors.New("QUASAR_ANDROID_BACKEND_PROXY must be empty or 1")
	}
}

func registerAndroidBackend(mux *http.ServeMux, endpoint string) {
	// This target is deliberately not configurable: native credentials must go
	// only to the official HTTPS backend, irrespective of inbound URL/Host.
	upstream := &url.URL{Scheme: "https", Host: "quasar.yandex.net"}
	mux.Handle(androidBackendPrefix, newAndroidBackendProxy(upstream, http.DefaultTransport, endpoint))
}

func newAndroidBackendProxy(upstream *url.URL, transport http.RoundTripper, endpoint string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, strings.TrimSuffix(androidBackendPrefix, "/"))
			if r.In.URL.RawPath != "" {
				r.Out.URL.RawPath = strings.TrimPrefix(r.In.URL.RawPath, strings.TrimSuffix(androidBackendPrefix, "/"))
			}
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			r.Out.Header.Del("Accept-Encoding")
		},
		Transport: transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Android backend unavailable", http.StatusBadGateway)
		},
		ModifyResponse: func(response *http.Response) error {
			// Never let this proxy or the native client's own redirect handling
			// carry an OAuth header to a different origin.
			if response.StatusCode >= 300 && response.StatusCode < 400 {
				return errors.New("Android backend redirect rejected")
			}
			if response.Request.Method != http.MethodGet || response.Request.URL.Path != "/get_sync_info" || response.StatusCode < 200 || response.StatusCode >= 300 {
				return nil
			}
			return rewriteAndroidSyncResponse(response, endpoint)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip, parseErr := netip.ParseAddr(host)
		if err != nil || parseErr != nil || !ip.Unmap().IsLoopback() {
			http.Error(w, "Android backend requires loopback", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func rewriteAndroidSyncResponse(response *http.Response, endpoint string) error {
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	_ = response.Body.Close()
	if err != nil || len(data) > limit || response.Header.Get("Content-Encoding") != "" {
		return errors.New("invalid Android backend configuration response")
	}
	var document map[string]jsontext.Value
	if json.Unmarshal(data, &document) != nil || document == nil {
		return errors.New("invalid Android backend configuration response")
	}
	var status string
	if json.Unmarshal(document["status"], &status) != nil {
		return errors.New("invalid Android backend configuration response")
	}
	if status == "ok" {
		var changed bool
		changed, err = rewriteAndroidSyncEndpoint(document, endpoint)
		if err != nil {
			return err
		}
		if changed {
			data, err = json.Marshal(document)
			if err != nil {
				return errors.New("invalid Android backend configuration response")
			}
			for _, name := range []string{"ETag", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Length"} {
				response.Header.Del(name)
				response.Trailer.Del(name)
			}
			response.Header.Set("Cache-Control", "no-store")
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	response.ContentLength = int64(len(data))
	return nil
}

func rewriteAndroidSyncEndpoint(document map[string]jsontext.Value, endpoint string) (bool, error) {
	configData, present := document["config"]
	if !present || bytes.Equal(bytes.TrimSpace(configData), []byte("null")) {
		return false, nil
	}
	var config map[string]jsontext.Value
	if json.Unmarshal(configData, &config) != nil || config == nil {
		return false, errors.New("invalid Android backend configuration response")
	}
	systemData, present := config["system_config"]
	if !present {
		return false, nil
	}
	var system map[string]jsontext.Value
	if json.Unmarshal(systemData, &system) != nil || system == nil {
		return false, errors.New("invalid Android backend configuration response")
	}
	if _, present := system["uniProxyUrl"]; !present {
		return false, nil
	}
	system["uniProxyUrl"], _ = json.Marshal(endpoint)
	config["system_config"], _ = json.Marshal(system)
	document["config"], _ = json.Marshal(config)
	return true, nil
}
