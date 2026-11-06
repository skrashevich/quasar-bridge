package bridge

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	common "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
)

func TestWakeMemoryInheritsOnlyForAutoListening(t *testing.T) {
	now := time.Now()
	var m wakeMemory
	if got := m.resolve("", "auto_listening", now); got != (Wake{}) {
		t.Fatalf("без предыдущего запроса наследовать нечего: %+v", got)
	}
	if got := m.resolve(" Яндекс ", "spotter", now); got != (Wake{Phrase: "яндекс"}) {
		t.Fatalf("слово не нормализовано: %+v", got)
	}
	if got := m.resolve("", "auto_listening", now.Add(30*time.Second)); got != (Wake{Phrase: "яндекс", Inherited: true}) {
		t.Fatalf("продолжение прослушивания должно наследовать слово: %+v", got)
	}
	// Окно наследования скользящее: оно считается от последнего запроса.
	if got := m.resolve("", "auto_listening", now.Add(30*time.Second+wakeInheritTTL)); got != (Wake{Phrase: "яндекс", Inherited: true}) {
		t.Fatalf("окно должно продлеваться: %+v", got)
	}
	if got := m.resolve("", "auto_listening", now.Add(30*time.Second+2*wakeInheritTTL+time.Second)); got != (Wake{}) {
		t.Fatalf("по истечении окна слово забывается: %+v", got)
	}
	// Запрос без слова (текст, интонация) не наследует и сбрасывает память.
	m.resolve("алиса", "spotter", now)
	if got := m.resolve("", "", now.Add(time.Second)); got != (Wake{}) {
		t.Fatalf("запрос без activation_type=auto_listening не наследует слово: %+v", got)
	}
	if got := m.resolve("", "auto_listening", now.Add(2*time.Second)); got != (Wake{}) {
		t.Fatalf("память должна быть сброшена: %+v", got)
	}
	// Активация по интонации внутри разговора — тоже его продолжение
	// (на колонке так приходит уточнение после ответа на «Яндекс, …»).
	m.resolve("алиса", "spotter", now)
	if got := m.resolve("<intonation>", "spotter", now.Add(time.Second)); got != (Wake{Phrase: "алиса", Inherited: true}) {
		t.Fatalf("интонация внутри разговора должна наследовать слово: %+v", got)
	}
	if got := m.resolve("", "auto_listening", now.Add(2*time.Second)); got != (Wake{Phrase: "алиса", Inherited: true}) {
		t.Fatalf("после интонации слово не должно теряться: %+v", got)
	}
	// Интонация вне окна наследования и без предыдущего слова слова не имеет.
	if got := m.resolve("<intonation>", "spotter", now.Add(2*time.Second+wakeInheritTTL+time.Second)); got != (Wake{}) {
		t.Fatalf("по истечении окна интонация без слова: %+v", got)
	}
	if got := m.resolve("<intonation>", "spotter", now.Add(time.Hour)); got != (Wake{}) {
		t.Fatalf("без предыдущего слова наследовать нечего: %+v", got)
	}
}

func TestCompileRoutingValidatesWakeRoutes(t *testing.T) {
	for name, routes := range map[string]map[string]string{
		"неизвестный маршрут":       {"яндекс": "claude"},
		"пустое слово":              {" ": "llm"},
		"служебное значение":        {"<intonation>": "llm"},
		"повтор с другим регистром": {"Яндекс": "llm", "яндекс": "yandex"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultRoutingConfig()
			cfg.WakeRoutes = routes
			if _, err := CompileRouting(cfg); err == nil {
				t.Fatal("некорректная настройка принята")
			}
		})
	}
	cfg := DefaultRoutingConfig()
	cfg.WakeRoutes = map[string]string{" Яндекс ": "llm", "АЛИСА": "yandex", "ясмина": "home"}
	p, err := CompileRouting(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for word, want := range map[string]string{"яндекс": "llm", "алиса": "yandex", "ясмина": "home"} {
		if got, ok := p.wakeRoute(Wake{Phrase: word}); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", word, got, ok, want)
		}
	}
	if _, ok := p.wakeRoute(Wake{}); ok {
		t.Error("без слова вызова маршрут не выбирается")
	}
}

func TestWakeRoutesDoNotChangeDefaultRoutingAndKeepRevisionStable(t *testing.T) {
	data, err := json.Marshal(DefaultRoutingConfig())
	if err != nil || strings.Contains(string(data), "wake_routes") {
		t.Fatalf("настройка не должна попадать в конфигурацию по умолчанию: %s %v", data, err)
	}
	cfg := DefaultRoutingConfig()
	cfg.WakeRoutes = map[string]string{"яндекс": "llm", "алиса": "yandex", "ясмина": "llm"}
	first, err := CompileRouting(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		again, err := CompileRouting(cfg)
		if err != nil || again.Revision() != first.Revision() {
			t.Fatalf("ревизия зависит от порядка ключей карты: %v %q != %q", err, again.Revision(), first.Revision())
		}
	}
	cfg.WakeRoutes["алиса"] = "llm"
	changed, err := CompileRouting(cfg)
	if err != nil || changed.Revision() == first.Revision() {
		t.Fatalf("ревизия должна меняться вместе с настройкой: %v", err)
	}
}

func TestDecideRoutesByWakeWord(t *testing.T) {
	engine := new(Engine)
	cfg := DefaultRoutingConfig()
	cfg.WakeRoutes = map[string]string{"яндекс": "llm", "алиса": "yandex"}
	if err := engine.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text string
		wake       Wake
		route      string
		source     string
		rule       string
	}{
		{"яндекс идёт к модели, хотя правила отправили бы в умный дом", "включи свет в кабинете", Wake{Phrase: "яндекс"}, "llm", "wake_word", ""},
		{"алиса идёт к Яндексу, хотя правила отправили бы к модели", "спроси нейросеть: почему небо голубое", Wake{Phrase: "алиса"}, "yandex", "wake_word", ""},
		{"продолжение прослушивания идёт тем же путём", "а почему закат красный", Wake{Phrase: "яндекс", Inherited: true}, "llm", "wake_word", ""},
		{"без слова действуют правила", "включи свет в кабинете", Wake{}, "yandex", "home_disabled", "home_devices"},
		{"слово не из списка не мешает правилам", "включи свет в кабинете", Wake{Phrase: "ясмина"}, "yandex", "home_disabled", "home_devices"},
		{"сброс диалога важнее слова вызова", "новый разговор", Wake{Phrase: "алиса"}, "llm", "dialogue_reset", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithWake(t.Context(), tc.wake)
			d := engine.Decide(ctx, tc.text)
			if d.Route != tc.route || d.Source != tc.source || d.Rule != tc.rule {
				t.Fatalf("маршрут %s/%s (правило %q), ожидался %s/%s (правило %q)", d.Route, d.Source, d.Rule, tc.route, tc.source, tc.rule)
			}
			if p := engine.policy().PreviewWake(tc.text, tc.wake); tc.source == "wake_word" && (p.Route != tc.route || p.Source != "wake_word") {
				t.Fatalf("предпросмотр %+v расходится с решением", p)
			}
		})
	}
}

func TestWakeRouteToDisabledHomeFallsBackToYandex(t *testing.T) {
	engine := new(Engine)
	cfg := DefaultRoutingConfig()
	cfg.WakeRoutes = map[string]string{"яндекс": "home"}
	if err := engine.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	d := engine.Decide(WithWake(t.Context(), Wake{Phrase: "яндекс"}), "что-нибудь")
	if d.Route != "yandex" || d.Source != "home_disabled" {
		t.Fatalf("без Home Assistant запрос должен уйти в Яндекс: %+v", d)
	}
}

// Слово вызова из запроса колонки должно дойти до маршрутизации, а продолжение
// прослушивания — унаследовать его.
func TestNativePassesWakeWordToRouting(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	type seen struct {
		text string
		wake Wake
	}
	got := make(chan seen, 4)
	resolver := resolverFunc(func(ctx context.Context, text string) (Resolution, error) {
		got <- seen{text, wakeFrom(ctx)}
		return Resolution{Route: "yandex"}, nil
	})
	proxy := &Proxy{Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http"), NewSession: func(ctx context.Context, up, down Sender) Session {
		return NewNative(ctx, up, down, resolver)
	}}
	server := httptest.NewServer(proxy)
	defer server.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	send := func(id, text, phrase, activation string) {
		in := &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{
			Event:          &common.TEvent{Type: common.EEventType_text_input.Enum(), Text: &text},
			ActivationType: &activation,
		}}
		if phrase != "" {
			in.SpotterPhrase = &phrase
		}
		msg := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
			Header: &header.TEventHeader{MessageId: id},
			Event:  &client.TClientMessage_TEvent_TextInput{TextInput: in},
		}}}
		if err := ws.Write(ctx, websocket.MessageBinary, aa(msg)); err != nil {
			t.Fatal(err)
		}
	}
	want := func(text string, wake Wake) {
		t.Helper()
		select {
		case s := <-got:
			if s.text != text || s.wake != wake {
				t.Fatalf("получено %+v, ожидалось %q и %+v", s, text, wake)
			}
		case <-ctx.Done():
			t.Fatalf("запрос %q не дошёл до маршрутизации", text)
		}
	}
	send("w-1", "первый вопрос", "Яндекс", "spotter")
	want("первый вопрос", Wake{Phrase: "яндекс"})
	send("w-2", "уточнение", "", "auto_listening")
	want("уточнение", Wake{Phrase: "яндекс", Inherited: true})
	send("w-3", "вопрос по интонации", "<intonation>", "spotter")
	want("вопрос по интонации", Wake{Phrase: "яндекс", Inherited: true})
}
