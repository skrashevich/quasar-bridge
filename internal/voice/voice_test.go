package voice

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"Прив+ет, мир":                       "Привет, мир",
		"Раз sil <[300]> два":                "Раз два",
		`<speaker voice="alice">Завтра ясно`: "Завтра ясно",
		"2+2 будет <[small]> четыре":         "2+2 будет четыре",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q)=%q want %q", in, got, want)
		}
	}
}

func TestOpenSendsSpeechRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		data, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/audio/speech" || r.Header.Get("Authorization") != "Bearer key" || json.Unmarshal(data, &body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["input"] != "привет" || body["voice"] != "denis" || body["response_format"] != "opus" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, "audio")
	}))
	defer s.Close()
	body, err := New(Config{BaseURL: s.URL + "/v1/", Voice: "denis", TimeoutMS: 1000}, "key").Open(t.Context(), "привет")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if data, _ := io.ReadAll(body); string(data) != "audio" {
		t.Fatalf("body=%q", data)
	}
}

func TestOpenReportsStatusWithoutURL(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	defer s.Close()
	_, err := New(Config{BaseURL: s.URL, Voice: "x", TimeoutMS: 1000}, "").Open(t.Context(), "a")
	if err == nil || strings.Contains(err.Error(), s.URL) {
		t.Fatalf("err=%v", err)
	}
}

func TestValidate(t *testing.T) {
	good := Config{Enabled: true, BaseURL: "http://192.168.1.2:8880/v1", Voice: "denis", TimeoutMS: 15000}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{{Enabled: true, BaseURL: "ftp://x", Voice: "a", TimeoutMS: 1000}, {Enabled: true, BaseURL: "http://x"}, {Enabled: true, BaseURL: "http://x", Voice: "a"}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if (Config{Enabled: true, YandexVoice: "ermil"}).Validate() != nil {
		t.Fatal("Yandex voice needs no service")
	}
	if (Config{Enabled: true, YandexVoice: "ermil", YandexSpeed: 9}).Validate() == nil {
		t.Fatal("accepted speed")
	}
	if (Config{}).Validate() != nil {
		t.Fatal("disabled config must be valid")
	}
}
