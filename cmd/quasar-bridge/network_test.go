package main

import (
	"context"
	"encoding/binary"
	"flag"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Removing the explicit resolver must not silently select Android's nonexistent
// loopback DNS server, and forcing UDP must not break truncated-answer fallback.
func TestConfiguredDNSResolves(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "UDP", true: "TCP fallback"}[fallback], func(t *testing.T) {
			server, udpQueries, tcpQueries := localDNSFixture(t, fallback)
			t.Setenv("QUASAR_DNS_SERVER", server)
			original := net.DefaultResolver
			t.Cleanup(func() { net.DefaultResolver = original })
			if err := configureDNS(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			addresses, err := net.DefaultResolver.LookupIP(ctx, "ip4", "bridge-dns.test.")
			if err != nil || !slices.ContainsFunc(addresses, func(ip net.IP) bool { return ip.String() == "192.0.2.57" }) {
				t.Fatalf("configured DNS did not resolve fixture: addresses=%v error=%v", addresses, err)
			}
			if udpQueries.Load() == 0 || (fallback && tcpQueries.Load() == 0) {
				t.Fatalf("DNS transports: UDP=%d TCP=%d", udpQueries.Load(), tcpQueries.Load())
			}
			canceled, stop := context.WithCancel(t.Context())
			stop()
			if _, err := net.DefaultResolver.Dial(canceled, "udp", "127.0.0.2:53"); err == nil {
				t.Fatal("resolver ignored the caller's canceled context")
			}
		})
	}
}

func TestConfiguredDNSValidation(t *testing.T) {
	original := net.DefaultResolver
	t.Cleanup(func() { net.DefaultResolver = original })
	for _, value := range []string{"192.0.2.1", "192.0.2.1:5353", "2001:db8::1", "[2001:db8::1]", "[2001:db8::1]:5353"} {
		t.Setenv("QUASAR_DNS_SERVER", value)
		if err := configureDNS(); err != nil {
			t.Fatalf("valid numeric server rejected: %v", err)
		}
	}
	for _, value := range []string{"dns.example.invalid", "secret-token@192.0.2.1", "192.0.2.1:0", "192.0.2.1:65536", "192.0.2.1:abc", "[::1]:", "udp://192.0.2.1", " 192.0.2.1", "192.0.2.1\nsecret-token"} {
		t.Setenv("QUASAR_DNS_SERVER", value)
		before := net.DefaultResolver
		err := configureDNS()
		if err == nil || !strings.Contains(err.Error(), "QUASAR_DNS_SERVER") || strings.Contains(err.Error(), value) || net.DefaultResolver != before {
			t.Fatalf("invalid setting was accepted, leaked, or mutated resolver: error=%v", err)
		}
	}
	t.Setenv("QUASAR_DNS_SERVER", "")
	before := net.DefaultResolver
	if err := configureDNS(); err != nil || net.DefaultResolver != before {
		t.Fatalf("absent override changed system resolver: %v", err)
	}
}

// Startup validation must reject malformed DNS before a successful -check-config.
func TestConfiguredDNSCheckConfigProcess(t *testing.T) {
	if path := os.Getenv("QUASAR_TEST_DNS_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("quasar-bridge", flag.ExitOnError)
		os.Args = []string{"quasar-bridge", "-config", path, "-check-config"}
		main()
		os.Exit(0)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConfiguredDNSCheckConfigProcess$")
	command.Env = append(os.Environ(), "QUASAR_TEST_DNS_CONFIG="+configFile(t, `{}`), "QUASAR_DNS_SERVER=private-dns-secret.invalid")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "QUASAR_DNS_SERVER") || strings.Contains(string(output), "private-dns-secret") {
		t.Fatalf("CLI accepted or leaked invalid DNS: error=%v output=%s", err, output)
	}
}

func localDNSFixture(t *testing.T, truncated bool) (string, *atomic.Uint32, *atomic.Uint32) {
	t.Helper()
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	udp, err := net.ListenPacket("udp4", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	udpQueries, tcpQueries := new(atomic.Uint32), new(atomic.Uint32)
	response := func(query []byte, isUDP bool) ([]byte, error) {
		var message dnsmessage.Message
		if err := message.Unpack(query); err != nil {
			return nil, err
		}
		message.Response = true
		message.RecursionAvailable = true
		message.Truncated = isUDP && truncated
		if !message.Truncated && len(message.Questions) == 1 && message.Questions[0].Type == dnsmessage.TypeA {
			message.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: message.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 57}}}}
		}
		return message.Pack()
	}
	go func() {
		buffer := make([]byte, 1500)
		for {
			n, address, err := udp.ReadFrom(buffer)
			if err != nil {
				return
			}
			udpQueries.Add(1)
			answer, err := response(buffer[:n], true)
			if err == nil {
				_, _ = udp.WriteTo(answer, address)
			}
		}
	}()
	go func() {
		for {
			connection, err := tcp.Accept()
			if err != nil {
				return
			}
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			var size [2]byte
			if _, err := io.ReadFull(connection, size[:]); err == nil {
				query := make([]byte, binary.BigEndian.Uint16(size[:]))
				if _, err = io.ReadFull(connection, query); err == nil {
					tcpQueries.Add(1)
					if answer, err := response(query, false); err == nil {
						frame := binary.BigEndian.AppendUint16(nil, uint16(len(answer)))
						_, _ = connection.Write(append(frame, answer...))
					}
				}
			}
			_ = connection.Close()
		}
	}()
	return tcp.Addr().String(), udpQueries, tcpQueries
}
