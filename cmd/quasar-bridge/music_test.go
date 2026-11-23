package main

import (
	"github.com/skrashevich/quasar-bridge/internal/assistant"
	"testing"
)

func TestMusicBaseURL(t *testing.T) {
	for _, tc := range []struct{ listen, cert, explicit, want string }{
		{"127.0.0.1:8443", "", "", "http://127.0.0.1:8443"},
		{"[::1]:8443", "", "", "http://[::1]:8443"},
		{"localhost:8443", "", "", "http://localhost:8443"},
		{":8443", "", "", ""},
		{"192.0.2.1:8443", "", "", ""},
		{"127.0.0.1:8443", "cert", "", ""},
		{":8443", "cert", "https://bridge.example", "https://bridge.example"},
	} {
		cfg := config{Listen: tc.listen, TLSCert: tc.cert, AssistantTools: assistant.Config{MusicBaseURL: tc.explicit}}
		if got := musicBaseURL(cfg); got != tc.want {
			t.Errorf("%+v: %q want %q", tc, got, tc.want)
		}
	}
}
