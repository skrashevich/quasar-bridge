package main

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func diagnosticAddresses(r *http.Request) (valid, local bool) {
	peer, peerErr := netip.ParseAddrPort(r.RemoteAddr)
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	hostIP, hostErr := netip.ParseAddr(host)
	localhost := strings.EqualFold(host, "localhost")
	return peerErr == nil && (localhost || hostErr == nil), peer.Addr().Unmap().IsLoopback() && (localhost || hostIP.Unmap().IsLoopback())
}

// diagnosticsAccess keeps localhost as the default and reads live access state
// for each request. Numeric network hosts avoid trusting arbitrary DNS names.
func diagnosticsAccess(network func() bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		valid, local := diagnosticAddresses(r)
		networkEnabled := network != nil && network()
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		origin := r.Header.Get("Origin")
		if !valid || (!networkEnabled && !local) ||
			r.Header.Get("Sec-Fetch-Site") == "cross-site" || (origin != "" && origin != scheme+"://"+r.Host) {
			http.Error(w, "Diagnostics access denied; localhost is required unless network access is enabled.", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next(w, r)
	}
}
