package assistant

import "testing"

func TestProxyFromEnvAcceptsOnlySocks(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"socks5://192.0.2.5:1080", "192.0.2.5:1080"},
		{"socks5h://proxy.invalid:1080/", "proxy.invalid:1080"},
		{"http://proxy.invalid:3128", ""},
		{"", ""},
		{"socks5://", ""},
	} {
		t.Setenv("HTTPS_PROXY", tc.value)
		t.Setenv("ALL_PROXY", "")
		got := proxyFromEnv()
		if tc.want == "" {
			if got != nil {
				t.Errorf("%q: unexpected dialer %q", tc.value, got.address)
			}
			continue
		}
		if got == nil || got.address != tc.want {
			t.Errorf("%q: got %v want %q", tc.value, got, tc.want)
		}
	}
}
