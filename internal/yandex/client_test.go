package yandex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	account "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

// These tests exercise the actual WebSocket transport, not a canned completer.
func TestQueryTextProtocol(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		{"cards", `{"response":{"cards":[{"text":"Первая строка"},{"text":"Вторая строка"}]}}`, "Первая строка\nВторая строка"},
		{"legacy card", `{"response":{"card":{"text":"Москва"}}}`, "Москва"},
		{"speech only", `{"voice_response":{"output_speech":{"text":"Пять часов"}}}`, "Пять часов"},
		{"legacy speech", `{"response":{"output_speech":{"text":"Пять часов"}}}`, "Пять часов"},
		{"streamed", `{"response":{"cards":[{"text":"Первая строка целиком"}]}}`, "Первая строка целиком"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "OAuth account-fixture" {
					t.Error("missing account authorization")
				}
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer ws.CloseNow()
				read := func() map[string]any {
					t.Helper()
					kind, b, err := ws.Read(r.Context())
					if err != nil || kind != websocket.MessageText {
						t.Errorf("invalid client frame: %v", err)
						return nil
					}
					var m map[string]any
					if json.Unmarshal(b, &m) != nil {
						t.Error("invalid client JSON")
						return nil
					}
					return m["event"].(map[string]any)
				}
				sync := read()
				if sync == nil {
					return
				}
				if sync["header"].(map[string]any)["name"] != "SynchronizeState" {
					t.Error("query sent before synchronization")
				}
				payload := sync["payload"].(map[string]any)
				if payload["oauth_token"] != "account-fixture" || len(payload["supported_features"].([]any)) != 0 {
					t.Error("invalid authentication or playback capabilities")
				}
				writeDirective(t, ws, "SynchronizeStateResponse", "", `{}`)
				query := read()
				if query == nil {
					return
				}
				h := query["header"].(map[string]any)
				id := h["messageId"].(string)
				p := query["payload"].(map[string]any)
				req := p["request"].(map[string]any)
				if h["name"] != "TextInput" || h["namespace"] != "Vins" || id == "" || p["header"].(map[string]any)["dialog_id"] != "" || req["voice_session"] != false || req["event"].(map[string]any)["text"] != "который час" {
					t.Error("text request changed the question or reused a voice dialog")
				}
				application := p["application"].(map[string]any)
				instant, err := time.Parse(time.RFC3339, application["client_time"].(string))
				if err != nil {
					t.Error(err)
				} else if _, offset := instant.Zone(); offset != 10800 {
					t.Error("configured timezone not sent")
				}
				if err := ws.Write(r.Context(), websocket.MessageBinary, []byte("discarded audio")); err != nil {
					t.Error(err)
					return
				}
				writeDirective(t, ws, "Ping", "", `{}`)
				pong := read()
				if pong == nil {
					return
				}
				pongHeader := pong["header"].(map[string]any)
				if pongHeader["name"] != "Pong" || pongHeader["refMessageId"] != "server-Ping" {
					t.Error("server ping was not acknowledged")
				}
				writeDirective(t, ws, "VinsResponse", "unrelated", `{"response":{"card":{"text":"Чужой ответ"}}}`)
				if tc.name == "streamed" {
					writeDirective(t, ws, "VinsResponse", id, `{"response":{"cards":[{"text":"Первая строка"}],"is_streaming":true}}`)
				}
				writeDirective(t, ws, "VinsResponse", id, tc.payload)
				// The client must close, never send TTS/audio/player acknowledgments.
				_, _, err = ws.Read(r.Context())
				if err == nil {
					t.Error("unexpected playback message")
				}
			}))
			defer server.Close()
			c := &Client{endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), token: "account-fixture", apiKey: "app-fixture", appID: "aliced", timezone: "Europe/Moscow"}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			answer, err := c.Query(ctx, "который час")
			if err != nil || answer != tc.want {
				t.Fatalf("answer=%q error=%v", answer, err)
			}
		})
	}
}

func writeDirective(t *testing.T, ws *websocket.Conn, name, ref, payload string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"directive": map[string]any{"header": map[string]any{"namespace": "Vins", "name": name, "messageId": "server-" + name, "refMessageId": ref}, "payload": jsontext.Value(payload)}})
	if err != nil {
		t.Error(err)
		return
	}
	if err = ws.Write(t.Context(), websocket.MessageText, b); err != nil {
		t.Error(err)
	}
}

func TestQueryErrors(t *testing.T) {
	for _, name := range []string{"InvalidAuth", "EventException", "empty", "cancel"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer ws.CloseNow()
				if _, _, err = ws.Read(r.Context()); err != nil {
					return
				}
				writeDirective(t, ws, "SynchronizeStateResponse", "", `{}`)
				_, b, err := ws.Read(r.Context())
				if err != nil {
					return
				}
				var m struct {
					Event struct {
						Header struct {
							ID string `json:"messageId"`
						} `json:"header"`
					} `json:"event"`
				}
				json.Unmarshal(b, &m)
				if name == "cancel" {
					_, _, _ = ws.Read(r.Context())
					return
				}
				if name == "empty" {
					writeDirective(t, ws, "VinsResponse", m.Event.Header.ID, `{"response":{"directives":[{"type":"audio_play"}]}}`)
				} else {
					writeDirective(t, ws, name, m.Event.Header.ID, `{"message":"private-token-and-provider-details"}`)
				}
				_, _, _ = ws.Read(r.Context())
			}))
			defer server.Close()
			c := &Client{endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), token: "fixture"}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			answer, err := c.Query(ctx, "вопрос")
			if err == nil || answer != "" || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe or missing error: %q %v", answer, err)
			}
			want := map[string]string{"InvalidAuth": "authorization", "EventException": "rejected", "empty": "no textual", "cancel": "deadline exceeded"}[name]
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("wrong error category: %v", err)
			}
			if name == "cancel" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost timeout: %v", err)
			}
		})
	}
}

func TestSpeakerUsesCurrentAccountAndPreservesFiles(t *testing.T) {
	dir := t.TempDir()
	storagePath := filepath.Join(dir, "account.dat")
	configPath := filepath.Join(dir, "quasar.cfg")
	data, err := proto.Marshal(&account.AccountStorage{CurrentAccountId: new(uint64(2)), Accounts: []*account.AccountInfo{
		{Id: new(uint64(1)), AuthToken: new("other-account")},
		{Id: new(uint64(2)), AuthToken: new("legacy-token"), Auth: &account.AccountInfo_OAuthInfo{Token: new("current-token")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(storagePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"authd": map[string]string{"accountStorageFile": storagePath}, "aliced": map[string]string{"apiKey": "native-key", "app_id": "native-app", "app_version": "1.0"}, "unrelated": true})
	os.WriteFile(configPath, cfg, 0600)
	c, err := NewSpeaker(configPath, "Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	if c.token != "current-token" || c.appID != "native-app" || c.apiKey != "native-key" {
		t.Fatal("wrong account or application selected")
	}
	after, _ := os.ReadFile(storagePath)
	if string(after) != string(data) {
		t.Fatal("account storage changed")
	}
	// An unknown current account must not fall back to another user's token.
	var storage account.AccountStorage
	if err = proto.Unmarshal(data, &storage); err != nil {
		t.Fatal(err)
	}
	storage.CurrentAccountId = new(uint64(3))
	missing, _ := proto.Marshal(&storage)
	if err = os.WriteFile(storagePath, missing, 0600); err != nil {
		t.Fatal(err)
	}
	if c, err = NewSpeaker(configPath, "Europe/Moscow"); err == nil || c != nil || strings.Contains(err.Error(), "other-account") {
		t.Fatalf("unknown current account was accepted: %v", err)
	}
}

func TestQueryDoesNotRedirectAccountCredentials(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); w.WriteHeader(http.StatusUnauthorized) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/private-redirect", http.StatusFound)
	}))
	defer redirect.Close()
	c := &Client{endpoint: "ws" + strings.TrimPrefix(redirect.URL, "http"), token: "account-fixture"}
	answer, err := c.Query(t.Context(), "вопрос")
	if err == nil || answer != "" || reached.Load() != 0 || strings.Contains(err.Error(), "private-redirect") {
		t.Fatalf("account redirect followed or exposed: reached=%d answer=%q error=%v", reached.Load(), answer, err)
	}
}

func TestQueryKeepsServerConversationContext(t *testing.T) {
	var mu sync.Mutex
	var firstID, device string
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if _, _, err = ws.Read(r.Context()); err != nil {
			return
		}
		writeDirective(t, ws, "SynchronizeStateResponse", "", `{}`)
		_, b, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		var m struct {
			Event struct {
				Header struct {
					ID string `json:"messageId"`
				} `json:"header"`
				Payload struct {
					Header struct {
						Dialog   string `json:"dialog_id"`
						Session  string `json:"session_id"`
						Previous string `json:"prev_req_id"`
						Sequence int    `json:"sequence_number"`
					} `json:"header"`
					Application struct {
						Device string `json:"uuid"`
					} `json:"application"`
				} `json:"payload"`
			} `json:"event"`
		}
		if err = json.Unmarshal(b, &m); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		turn++
		n := turn
		if n == 1 {
			firstID = m.Event.Header.ID
			device = m.Event.Payload.Application.Device
		} else if m.Event.Payload.Header.Dialog != "cloud-dialog" || m.Event.Payload.Header.Session != "serialized-context" || m.Event.Payload.Header.Previous != firstID || m.Event.Payload.Header.Sequence != 2 || device != m.Event.Payload.Application.Device {
			t.Error("follow-up lost server context or diagnostic identity")
		}
		mu.Unlock()
		writeDirective(t, ws, "VinsResponse", m.Event.Header.ID, `{"header":{"dialog_id":"cloud-dialog","session_id":"serialized-context"},"response":{"cards":[{"text":"Ответ"}]}}`)
		_, _, _ = ws.Read(r.Context())
	}))
	defer server.Close()
	c := &Client{endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), token: "fixture"}
	for _, text := range []string{"какая погода в Москве", "а завтра?"} {
		if answer, err := c.Query(t.Context(), text); err != nil || answer != "Ответ" {
			t.Fatalf("query=%q answer=%q error=%v", text, answer, err)
		}
	}
}
