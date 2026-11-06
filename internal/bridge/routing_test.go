package bridge

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"quasarbridge/internal/llm"
)

func TestRoutingConditionsAndOrderedPriority(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.Rules = []RoutingRule{
		{Name: "exclude-music", Route: "home", Match: RoutingMatch{ContainsAllGroups: [][]string{{"свет", "ламп"}, {"включ", "выключ"}}, ContainsNone: []string{"песню"}}},
		{Name: "prefix", Route: "llm", Match: RoutingMatch{StartsWithAny: []string{"спроси"}, ContainsAny: []string{"нейросеть"}}},
		{Name: "regex", Route: "llm", Match: RoutingMatch{Regex: `^объясни\s+.+`}},
	}
	cfg.DefaultRoute = "yandex"
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) { return "", nil })}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ text, route, rule string }{
		{"ВЫКЛЮЧИ ЛАМПУ", "home", "exclude-music"},
		{"включи песню про свет", "yandex", ""},
		{"спроси нейросеть включи свет", "home", "exclude-music"},
		{"спроси нейросеть о море", "llm", "prefix"},
		{"ОБЪЯСНИ подробно", "llm", "regex"},
		{"про море", "yandex", ""},
	} {
		got := e.Decide(t.Context(), tc.text)
		if got.Route != tc.route || got.Rule != tc.rule {
			t.Errorf("%q: %+v", tc.text, got)
		}
	}
}

func TestMusicFromWebsiteRoutesBeforeYandexMusic(t *testing.T) {
	e := &Engine{}
	for _, text := range []string{
		"Включи музыку с сайта abc.su/music",
		"Проиграй песни со страницы https://example.org/music",
		"Воспроизведи аудио абв точка су слэш music",
	} {
		decision := e.Decide(t.Context(), text)
		if decision.Route != "llm" || decision.Rule != "music_from_site" {
			t.Errorf("website music routed incorrectly: %q: %+v", text, decision)
		}
	}
	for _, text := range []string{"Включи музыку", "Включи радио", "Поставь песню"} {
		if decision := e.Decide(t.Context(), text); decision.Route != "yandex" {
			t.Errorf("ordinary Yandex music routing changed: %q: %+v", text, decision)
		}
	}
	if decision := e.Decide(t.Context(), "Не включай музыку с сайта abc.su/music"); decision.Rule == "music_from_site" {
		t.Fatal("negative playback request matched music rule")
	}
}
func TestRoutingValidationAndCallerMutation(t *testing.T) {
	for _, mutate := range []func(*RoutingConfig){
		func(c *RoutingConfig) { c.Rules[0].Route = "bad" },
		func(c *RoutingConfig) { c.Rules[0].Match = RoutingMatch{Regex: "["} },
		func(c *RoutingConfig) { c.Rules[0].Match = RoutingMatch{} },
		func(c *RoutingConfig) { c.Rules[0].Match = RoutingMatch{ContainsAllGroups: [][]string{{}}} },
		func(c *RoutingConfig) { c.Rules[1].Name = c.Rules[0].Name },
		func(c *RoutingConfig) { c.DefaultRoute = "bad" },
		func(c *RoutingConfig) { c.Classifier.TimeoutMS = 0 },
		func(c *RoutingConfig) { c.Refusal.Prefixes = nil },
	} {
		cfg := DefaultRoutingConfig()
		mutate(&cfg)
		if _, err := CompileRouting(cfg); err == nil {
			t.Fatal("bad policy accepted")
		}
	}
	cfg := DefaultRoutingConfig()
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) { return "", nil })}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	revision := e.RoutingStatus().Revision
	for i := range cfg.Rules {
		if cfg.Rules[i].Name == "home_devices" {
			cfg.Rules[i].Match.ContainsAllGroups[0][0] = "mutated"
		}
	}
	if e.Decide(t.Context(), "выключи свет").Route != "home" || e.RoutingStatus().Revision != revision {
		t.Fatal("caller mutation changed active policy")
	}
}
func TestClassifierConfigAndFallback(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.Rules = []RoutingRule{}
	cfg.Classifier.TimeoutMS = 50
	cfg.Classifier.Prompt = "custom classifier"
	cfg.Classifier.OnError = "llm"
	var fail bool
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) { return "", nil }), LLM: completionFunc(func(ctx context.Context, m []llm.Message, structured bool) (string, error) {
		if !structured || m[0].Content != "custom classifier" {
			t.Fatal("classifier config ignored")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 100*time.Millisecond {
			t.Fatal("missing classifier timeout")
		}
		if fail {
			return "", errors.New("offline")
		}
		return `{"route":"home"}`, nil
	})}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	if got := e.Decide(t.Context(), "unknown"); got.Route != "home" || got.Source != "classifier" {
		t.Fatal(got)
	}
	fail = true
	if got := e.Decide(t.Context(), "unknown"); got.Route != "llm" || got.Source != "classifier_error" {
		t.Fatal(got)
	}
}
func TestConfigurableRefusalsAndDisabledRules(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.Rules[0].Disabled = true
	cfg.DefaultRoute = "yandex"
	cfg.Rules = cfg.Rules[:1]
	cfg.Refusal.Prefixes = []string{"нужна помощь"}
	cfg.Refusal.ContainsAny = []string{"повторите"}
	e := &Engine{}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	if e.Decide(t.Context(), "включи свет").Route != "yandex" {
		t.Fatal("disabled rule matched")
	}
	if !e.IsRefusal("Нужна   помощь, повторите") || e.IsRefusal("Я не знаю") {
		t.Fatal("custom refusal matcher ignored")
	}
	cfg.Refusal.Enabled = false
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	if e.IsRefusal("нужна помощь повторите") {
		t.Fatal("refusal matcher not disabled")
	}
}
func TestDefaultPolicyExampleAndConcurrentReplacement(t *testing.T) {
	a := DefaultRoutingConfig()
	b := DefaultRoutingConfig()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("defaults unstable")
	}
	a.Rules[0].Name = "different"
	if b.Rules[0].Name == a.Rules[0].Name {
		t.Fatal("defaults share storage")
	}
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) { return "", nil })}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				d := e.Decide(t.Context(), "выключи свет")
				if d.Route != "home" && d.Route != "yandex" {
					t.Errorf("bad decision: %+v", d)
				}
			}
		})
	}
	for i := range 50 {
		c := DefaultRoutingConfig()
		if i%2 == 0 {
			c.Rules = []RoutingRule{}
			c.DefaultRoute = "yandex"
		}
		if err := e.SetRouting(c); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	raw, _ := json.Marshal(DefaultRoutingConfig())
	var round RoutingConfig
	if err := json.Unmarshal(raw, &round); err != nil || !reflect.DeepEqual(round, DefaultRoutingConfig()) {
		t.Fatal("config roundtrip failed")
	}
}

func TestClassifierKeepsSnapshotDuringReload(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) { return "", nil }), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		close(started)
		<-release
		return "bad json", nil
	})}
	cfg := DefaultRoutingConfig()
	cfg.Rules = []RoutingRule{}
	cfg.Classifier.OnError = "home"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	result := make(chan RouteDecision, 1)
	go func() { result <- e.Decide(t.Context(), "unknown") }()
	<-started
	cfg.DefaultRoute = "yandex"
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := <-result; got.Route != "home" {
		t.Fatal("classification crossed policy snapshots", got)
	}
	if got := e.Decide(t.Context(), "unknown"); got.Route != "yandex" {
		t.Fatal("next request used old policy", got)
	}
}

func TestShortHistoryRequestsSkipClassifier(t *testing.T) {
	e := &Engine{LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("history request reached classifier")
		return "", nil
	})}
	for _, text := range []string{"о чем мы сегодня общались", "О чём мы сегодня общались?", "что мы обсуждали сегодня"} {
		if d := e.Decide(t.Context(), text); d.Route != "llm" || d.Source != "rule" {
			t.Errorf("%q: %+v", text, d)
		}
	}
}

func TestSpeakerPlaybackControlSkipsClassifier(t *testing.T) {
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) { t.Fatal("speaker control reached home"); return "", nil }), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("speaker control reached classifier")
		return "", nil
	})}
	for _, text := range []string{"Хватит", "Алиса, хватит!", "Стоп", "Останови музыку", "Выключи музыку", "Пауза", "Поставь на паузу", "Продолжи музыку", "Возобнови воспроизведение"} {
		d := e.Decide(t.Context(), text)
		if d.Route != "yandex" || d.Rule != "speaker_playback" || d.Source != "rule" {
			t.Errorf("%q: %+v", text, d)
		}
	}
	p, err := CompileRouting(DefaultRoutingConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Объясни слово хватит", "Не останавливай музыку", "Хватит ли памяти для модели", "Поставь таймер на паузу", "Включи свет", "Поставь музыку с сайта audio.example/music"} {
		if d, ok := p.match(text); ok && d.Rule == "speaker_playback" {
			t.Errorf("false speaker control: %q", text)
		}
	}
}

func TestBridgeUpdateRequestsNeverReachHomeClassifier(t *testing.T) {
	e := &Engine{Home: homeFunc(func(context.Context, string) (string, error) {
		t.Fatal("update reached Home Assistant")
		return "", nil
	}), LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Error("update request reached classifier")
		return `{"route":"home"}`, nil
	})}
	for _, text := range []string{"Проверь обновления", "Есть ли обновления?", "Алиса, проверь обновления, пожалуйста!", "Спроси нейросеть: проверь обновления", "Проверь обновления Quasar Bridge", "Установи новую версию", "Установи обновления"} {
		got := e.Decide(t.Context(), text)
		if got.Route != "llm" || got.Rule != "bridge_updates" {
			t.Errorf("%q: %+v", text, got)
		}
	}
}
