package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
)

// A static Linux binary cannot use Android's netd resolver. Keep the override
// process-local so the speaker's own networking configuration remains intact.
func configureDNS() error {
	value := os.Getenv("QUASAR_DNS_SERVER")
	if value == "" {
		return nil
	}
	ipValue := value
	if strings.HasPrefix(ipValue, "[") && strings.HasSuffix(ipValue, "]") {
		ipValue = ipValue[1 : len(ipValue)-1]
	}
	ip, err := netip.ParseAddr(ipValue)
	var server netip.AddrPort
	if err == nil {
		server = netip.AddrPortFrom(ip, 53)
	} else {
		server, err = netip.ParseAddrPort(value)
	}
	if err != nil || server.Port() == 0 {
		return errors.New("QUASAR_DNS_SERVER must be a numeric IP address with an optional port (1-65535)")
	}
	address := server.String()
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return new(net.Dialer).DialContext(ctx, network, address)
		},
	}
	return nil
}
