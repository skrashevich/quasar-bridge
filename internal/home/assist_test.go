package home

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quasarbridge/internal/dialogue"

	"github.com/coder/websocket"
)

func TestAssistContinuesConversationUntilTTL(t *testing.T) {
	token := "long-lived-token"
	pipeline := "01aaaaaaaaaaaaaaaaaaaaaaaa"
	device := "0123456789abcdef0123456789abcdef"
	var saw []map[string]any
	var upgrades atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/websocket" {
			t.Errorf("path %s", r.URL.Path)
		}
		upgrades.Add(1)
		acceptAssist(t, w, r, token, func(cmd map[string]any) []any {
			saw = append(saw, cmd)
			id := "conv-1"
			if _, ok := cmd["conversation_id"]; !ok {
				id = "conv-new"
			}
			return intentMessages(id, "action_done", "Включила свет", true)
		}, nil)
	}))
	defer s.Close()
	now := time.Now()
	a := &Assist{Config: AssistConfig{URL: s.URL, AgentID: "conversation.hall", PipelineID: pipeline, DeviceID: device, Language: "ru", TimeoutMS: 20000, TTLSeconds: 30}, Token: token, now: func() time.Time { return now }}
	ctx := dialogue.WithSpeaker(t.Context(), "station-1")
	reply, err := a.Process(ctx, "включи свет")
	if err != nil || reply.Speech != "Включила свет" || reply.Type != "action_done" || !a.Active(ctx) {
		t.Fatalf("reply=%+v err=%v", reply, err)
	}
	if _, err := a.Process(ctx, "а ярче"); err != nil {
		t.Fatal(err)
	}
	other := dialogue.WithSpeaker(t.Context(), "station-2")
	if _, err := a.Process(other, "выключи"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if a.Active(ctx) {
		t.Fatal("expired conversation still active")
	}
	if _, err := a.Process(ctx, "ещё"); err != nil {
		t.Fatal(err)
	}
	if len(saw) != 4 || upgrades.Load() != 1 {
		t.Fatalf("calls=%d sockets=%d", len(saw), upgrades.Load())
	}
	input, _ := saw[0]["input"].(map[string]any)
	if saw[0]["pipeline"] != pipeline || saw[0]["device_id"] != device || input["text"] != "включи свет" || saw[0]["start_stage"] != "intent" || saw[0]["end_stage"] != "intent" || saw[0]["conversation_id"] != nil || saw[0]["agent_id"] != nil || saw[0]["language"] != nil {
		t.Fatalf("first request: %+v", saw[0])
	}
	if saw[1]["conversation_id"] != "conv-new" || saw[2]["conversation_id"] != nil || saw[3]["conversation_id"] != nil {
		t.Fatalf("conversation ids: %+v", saw)
	}
}

func TestAssistRedialsWhenTheSocketDrops(t *testing.T) {
	token := "token"
	var upgrades atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		acceptAssist(t, w, r, token, func(map[string]any) []any {
			return intentMessages("c", "action_done", "Готово", true)
		}, func() bool { return true })
	}))
	defer s.Close()
	a := &Assist{Config: AssistConfig{URL: s.URL, Language: "ru", TimeoutMS: 20000, TTLSeconds: 300}, Token: token}
	if _, err := a.Process(t.Context(), "включи"); err != nil {
		t.Fatal(err)
	}
	waitSocket(t, a, false)
	if _, err := a.Process(t.Context(), "ещё"); err != nil || upgrades.Load() != 2 {
		t.Fatalf("err=%v sockets=%d", err, upgrades.Load())
	}
}

func waitSocket(t *testing.T, a *Assist, up bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.sessMu.Lock()
		open := a.conn != nil
		a.sessMu.Unlock()
		if open == up {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("socket did not settle")
}

func TestAssistHidesTransportDetailsAndDropsErrorConversation(t *testing.T) {
	token := "super-secret-token"
	hits := 0
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer evil.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, evil.URL+"/steal?access_token="+token, http.StatusFound)
	}))
	defer s.Close()
	a := &Assist{Config: AssistConfig{URL: s.URL + "/", Language: "ru", TimeoutMS: 20000, TTLSeconds: 300}, Token: token}
	a.Config.Normalize()
	_, err := a.Process(t.Context(), "включи свет")
	if err == nil || err.Error() != "Home Assistant transport failed" || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "steal") || strings.Contains(err.Error(), s.URL) || hits != 0 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
	if err := a.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptAssist(t, w, r, token, func(map[string]any) []any {
			return intentMessages("must-not-stick", "error", "Не поняла", false)
		}, nil)
	}))
	defer broken.Close()
	a.Config.URL = broken.URL
	reply, err := a.Process(t.Context(), "включи")
	if err != nil || reply.Type != "error" || reply.Speech != "Не поняла" || a.Active(t.Context()) {
		t.Fatalf("reply=%+v err=%v active=%v", reply, err, a.Active(t.Context()))
	}
}

func TestAssistConfigRejectsSecretsInURLAndOptionalWhenDisabled(t *testing.T) {
	var disabled AssistConfig
	disabled.Normalize()
	if err := disabled.Validate(""); err != nil {
		t.Fatal(err)
	}
	cfg := AssistConfig{URL: "https://ha.example.invalid:8123/", AgentID: "conversation.default"}
	cfg.Normalize()
	if cfg.URL != "https://ha.example.invalid:8123" || cfg.Language != "ru" || cfg.TimeoutMS != 20000 || cfg.TTLSeconds != 300 {
		t.Fatalf("defaults=%+v", cfg)
	}
	if err := cfg.Validate(""); err == nil {
		t.Fatal("missing token accepted")
	}
	for _, raw := range []string{"https://user:secret@ha.example.invalid", "https://ha.example.invalid/api", "https://ha.example.invalid?token=1", "file:///tmp/ha"} {
		c := AssistConfig{URL: raw, Language: "ru", TimeoutMS: 20000, TTLSeconds: 300}
		if err := c.Validate("token"); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	ok := AssistConfig{URL: "https://ha.example.invalid", PipelineID: "01aaaaaaaaaaaaaaaaaaaaaaaa", DeviceID: "0123456789abcdef0123456789abcdef", Language: "ru", TimeoutMS: 20000, TTLSeconds: 300}
	if err := ok.Validate("token"); err != nil {
		t.Fatal(err)
	}
	badDevice := ok
	badDevice.DeviceID = "device"
	if err := badDevice.Validate("token"); err == nil {
		t.Fatal("accepted device id")
	}
}

type plannerCalls struct{ n int }

func (p *plannerCalls) Handle(context.Context, string) (string, error) {
	p.n++
	return "из планировщика", nil
}

func TestGatewayContinueConversationDefaultsOff(t *testing.T) {
	token := "token"
	var keep atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptAssist(t, w, r, token, func(map[string]any) []any {
			return intentMessagesContinue("c", "action_done", "Свет включён", true, keep.Load())
		}, nil)
	}))
	defer s.Close()
	planner := &plannerCalls{}
	for _, tc := range []struct {
		name   string
		config bool
		event  bool
		want   bool
	}{
		{"default ignores pipeline flag", false, true, false},
		{"enabled follows pipeline", true, true, true},
		{"enabled stays closed", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keep.Store(tc.event)
			g := &Gateway{Assist: &Assist{Config: AssistConfig{URL: s.URL, Language: "ru", TimeoutMS: 20000, TTLSeconds: 300, ContinueConversation: tc.config}, Token: token}, Devices: planner}
			answer, listen, err := g.Turn(t.Context(), "включи свет")
			if err != nil || answer != "Свет включён" || listen == nil || *listen != tc.want || planner.n != 0 {
				t.Fatalf("answer=%q listen=%v err=%v calls=%d", answer, listen, err, planner.n)
			}
		})
	}
}

func TestGatewaySpeaksAssistantAndFallsBackOnlyOnFailure(t *testing.T) {
	token := "token"
	var mode atomic.Value
	mode.Store("ok")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptAssist(t, w, r, token, func(map[string]any) []any {
			switch mode.Load().(string) {
			case "ok":
				return intentMessages("c", "action_done", "Фонарики включены", true)
			case "empty":
				return intentMessages("c", "action_done", "", false)
			case "error":
				return intentMessages("c", "error", "не вышло", false)
			default:
				return nil
			}
		}, nil)
	}))
	defer s.Close()
	planner := &plannerCalls{}
	g := &Gateway{Assist: &Assist{Config: AssistConfig{URL: s.URL, Language: "ru", TimeoutMS: 20000, TTLSeconds: 300}, Token: token}, Devices: planner}
	answer, err := g.Handle(t.Context(), "включи фонарики")
	if err != nil || answer != "Фонарики включены" || planner.n != 0 {
		t.Fatalf("answer=%q err=%v calls=%d", answer, err, planner.n)
	}
	mode.Store("empty")
	answer, err = g.Handle(t.Context(), "ещё")
	if err != nil || answer != "Готово." || planner.n != 0 {
		t.Fatalf("empty answer=%q err=%v calls=%d", answer, err, planner.n)
	}
	mode.Store("error")
	answer, err = g.Handle(t.Context(), "включи")
	if err != nil || answer != "из планировщика" || planner.n != 1 {
		t.Fatalf("fallback=%q err=%v calls=%d", answer, err, planner.n)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.Handle(ctx, "включи"); err == nil || planner.n != 1 {
		t.Fatalf("canceled request continued: %v calls=%d", err, planner.n)
	}
	mode.Store("down")
	answer, err = g.Handle(t.Context(), "включи")
	if err != nil || answer != "из планировщика" || planner.n != 2 {
		t.Fatalf("transport fallback=%q err=%v calls=%d", answer, err, planner.n)
	}
}

func TestAssistSubscriptionAckIsNotTheAnswer(t *testing.T) {
	token := "token"
	var upgrades atomic.Int32
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		acceptAssist(t, w, r, token, func(map[string]any) []any {
			if n.Add(1) == 1 {
				return []any{map[string]any{"type": "result", "success": false}}
			}
			return intentMessages("c", "action_done", "Свет включён", true)
		}, nil)
	}))
	defer s.Close()
	a := &Assist{Config: AssistConfig{URL: s.URL, Language: "ru", TimeoutMS: 20000, TTLSeconds: 300}, Token: token}
	if _, err := a.Process(t.Context(), "включи свет"); err == nil || a.Active(t.Context()) {
		t.Fatal(err)
	}
	reply, err := a.Process(t.Context(), "ещё")
	if err != nil || reply.Speech != "Свет включён" || upgrades.Load() != 1 {
		t.Fatalf("reply=%+v err=%v sockets=%d", reply, err, upgrades.Load())
	}
}

func acceptAssist(t *testing.T, w http.ResponseWriter, r *http.Request, token string, reply func(map[string]any) []any, stop func() bool) {
	t.Helper()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Errorf("accept: %v", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()
	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			t.Errorf("marshal: %v", err)
			return false
		}
		return conn.Write(ctx, websocket.MessageText, b) == nil
	}
	if !send(map[string]string{"type": "auth_required"}) {
		return
	}
	var auth struct {
		Type        string `json:"type"`
		AccessToken string `json:"access_token"`
	}
	if err := readJSON(ctx, conn, &auth); err != nil {
		return
	}
	if auth.Type != "auth" || auth.AccessToken != token {
		send(map[string]string{"type": "auth_invalid"})
		return
	}
	if !send(map[string]string{"type": "auth_ok"}) {
		return
	}
	for {
		var cmd map[string]any
		if err := readJSON(ctx, conn, &cmd); err != nil {
			return
		}
		switch cmd["type"] {
		case "ping":
			if !send(map[string]any{"id": cmd["id"], "type": "pong"}) {
				return
			}
			continue
		case "unsubscribe_events":
			if !send(map[string]any{"id": cmd["id"], "type": "result", "success": true}) {
				return
			}
			continue
		}
		msgs := reply(cmd)
		if msgs == nil {
			return
		}
		for _, msg := range msgs {
			if m, ok := msg.(map[string]any); ok {
				m["id"] = cmd["id"]
			}
			if !send(msg) {
				return
			}
		}
		if stop != nil && stop() {
			return
		}
	}
}

func intentMessages(conv, kind, speech string, local bool) []any {
	return intentMessagesContinue(conv, kind, speech, local, false)
}

func intentMessagesContinue(conv, kind, speech string, local, continueConversation bool) []any {
	return []any{
		map[string]any{"type": "result", "success": true, "result": nil},
		map[string]any{"type": "event", "event": map[string]any{"type": "run-start", "data": map[string]any{"conversation_id": nil, "pipeline": "hall"}}},
		map[string]any{"type": "event", "event": map[string]any{"type": "intent-start", "data": map[string]any{"intent_input": "hidden"}}},
		map[string]any{"type": "event", "event": map[string]any{"type": "intent-end", "data": map[string]any{
			"processed_locally": local,
			"intent_output": map[string]any{
				"conversation_id":       conv,
				"continue_conversation": continueConversation,
				"response": map[string]any{
					"response_type": kind,
					"speech":        map[string]any{"plain": map[string]any{"speech": speech}},
				},
			},
		}}},
		map[string]any{"type": "event", "event": map[string]any{"type": "run-end"}},
	}
}
