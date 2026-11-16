package main

import "testing"

func TestBackendCompatRequiresTLSAndDefaultsOff(t *testing.T) {
	cfg, err := parseConfig([]byte(`{}`), false)
	if err != nil || cfg.YandexBackendCompat {
		t.Fatal("compat should default off")
	}
	if _, err := parseConfig([]byte(`{"yandex_backend_compat":true}`), false); err == nil {
		t.Fatal("plaintext backend credentials accepted")
	}
	cfg, err = parseConfig([]byte(`{"yandex_backend_compat":true,"tls_cert":"fixture.pem","tls_key":"fixture.key"}`), false)
	if err != nil || !cfg.YandexBackendCompat {
		t.Fatal("TLS compatibility config rejected")
	}
}
