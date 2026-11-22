package main

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func diagnosticClient(r *http.Request) (netip.Addr, bool) {
	peer, peerErr := netip.ParseAddrPort(r.RemoteAddr)
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	hostIP, hostErr := netip.ParseAddr(host)
	localhost := strings.EqualFold(host, "localhost")
	if peerErr != nil || (!localhost && hostErr != nil) {
		return netip.Addr{}, false
	}
	ip := peer.Addr().Unmap()
	return ip, ip.IsLoopback() && (localhost || hostIP.Unmap().IsLoopback())
}

func diagnosticOrigin(r *http.Request) string {
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// diagnosticsAccess keeps localhost as the default and reads live access state
// for each request. Numeric network hosts avoid trusting arbitrary DNS names.
func diagnosticsAccess(network func() bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, local := diagnosticClient(r)
		networkEnabled := network != nil && network()
		origins := r.Header.Values("Origin")
		if !peer.IsValid() || (!networkEnabled && !local) || r.Header.Get("Sec-Fetch-Site") == "cross-site" ||
			len(origins) > 1 || (len(origins) == 1 && origins[0] != "" && origins[0] != diagnosticOrigin(r)) {
			http.Error(w, "Diagnostics access denied; localhost is required unless network access is enabled.", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next(w, r)
	}
}
