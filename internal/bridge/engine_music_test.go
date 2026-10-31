package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

type sourceClarificationMusic struct{ started []string }

func (*sourceClarificationMusic) CanStartMusic(text string) bool {
	switch text {
	case "Включи музыку с сайта https://audio.example.org/music", "Включи музыку с сайта https://abc.org", "Сыграй музыку с сайта эй би си точка орг", "Воспроизведи музыку с сайта эй би си точка орг", "Алиса, сыграйте музыку с сайта эй би си точка орг":
		return true
	}
	return false
}
func (*sourceClarificationMusic) NeedsMusicSource(text string) bool {
	return strings.HasPrefix(text, "Включи музыку с сайта ") && !strings.Contains(text, "https://")
}
func (m *sourceClarificationMusic) StartMusic(_ context.Context, text string) (string, error) {
	m.started = append(m.started, text)
	return "Включаю музыку.", nil
}
func clarificationEngine(t *testing.T) (*Engine, *sourceClarificationMusic) {
	t.Helper()
	music := new(sourceClarificationMusic)
	return &Engine{Music: music, LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {

		return "", nil
	})}, music
}
func TestMusicSourceClarificationContinuesOnlyWithAddress(t *testing.T) {
	e, music := clarificationEngine(t)
	ctx := dialogue.WithSpeaker(t.Context(), "midi")
	result, err := e.Resolve(ctx, "Включи музыку с сайта абв точка неизвестно")
	if err != nil || result.Route != "music" || !strings.Contains(result.Answer, "по буквам") || len(music.started) != 0 {
		t.Fatalf("prompt=%+v started=%v error=%v", result, music.started, err)
	}
	result, err = e.Resolve(ctx, "https://audio.example.org/music")
	if err != nil || result.Route != "music" || len(music.started) != 1 || music.started[0] != "Включи музыку с сайта https://audio.example.org/music" {
		t.Fatalf("reply=%+v started=%v error=%v", result, music.started, err)
	}
	result, err = e.Resolve(ctx, "https://audio.example.org/music")
	if err != nil || result.Route == "music" || len(music.started) != 1 {
		t.Fatalf("consumed pending replayed: %+v started=%v error=%v", result, music.started, err)
	}
}
func TestMusicSourceClarificationDoesNotLeakOrOverrideCloudPolicy(t *testing.T) {
	e, music := clarificationEngine(t)
	a, b := dialogue.WithSpeaker(t.Context(), "a"), dialogue.WithSpeaker(t.Context(), "b")
	if _, err := e.Resolve(a, "Включи музыку с сайта абв точка неизвестно"); err != nil {
		t.Fatal(err)
	}
	policy := DefaultRoutingConfig()
	policy.DefaultRoute = "yandex"
	policy.Rules = nil
	if err := e.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	result, err := e.Resolve(b, "https://audio.example.org/music")
	if err != nil || result.Route != "yandex" || len(music.started) != 0 {
		t.Fatalf("cross speaker=%+v error=%v", result, err)
	}
	result, err = e.Resolve(a, "Включи музыку с сайта абв точка неизвестно")
	if err != nil || result.Route != "yandex" || len(music.started) != 0 {
		t.Fatalf("cloud overridden=%+v error=%v", result, err)
	}
}
func TestMusicSourceClarificationCancellationAndUnrelatedTurn(t *testing.T) {
	for _, next := range []string{"нет", "отмена", "не надо", "стоп", "начни новый разговор", "какая погода"} {
		t.Run(next, func(t *testing.T) {
			e, music := clarificationEngine(t)
			ctx := dialogue.WithSpeaker(t.Context(), "a")
			if _, err := e.Resolve(ctx, "Включи музыку с сайта абв точка неизвестно"); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Resolve(ctx, next); err != nil {
				t.Fatal(err)
			}
			policy := DefaultRoutingConfig()
			policy.DefaultRoute = "yandex"
			policy.Rules = nil
			if err := e.SetRouting(policy); err != nil {
				t.Fatal(err)
			}
			result, err := e.Resolve(ctx, "https://audio.example.org/music")
			if err != nil || result.Route != "yandex" || len(music.started) != 0 {
				t.Fatalf("stale pending=%+v started=%v error=%v", result, music.started, err)
			}
		})
	}
}
func TestMusicSourceClarificationRejectsStaleDecisionAndExtraCommands(t *testing.T) {
	e, music := clarificationEngine(t)
	ctx := dialogue.WithSpeaker(t.Context(), "a")
	if _, err := e.Resolve(ctx, "Включи музыку с сайта абв точка неизвестно"); err != nil {
		t.Fatal(err)
	}
	decision := e.Decide(ctx, "https://audio.example.org/music")
	e.Prepare(ctx)
	if _, err := e.ResolveDecision(ctx, "https://audio.example.org/music", decision); !errors.Is(err, context.Canceled) || len(music.started) != 0 {
		t.Fatalf("stale reply error=%v started=%v", err, music.started)
	}
	if _, err := e.Resolve(ctx, "Включи музыку с сайта абв точка неизвестно"); err != nil {
		t.Fatal(err)
	}
	policy := DefaultRoutingConfig()
	policy.DefaultRoute = "yandex"
	policy.Rules = nil
	if err := e.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	result, err := e.Resolve(ctx, "https://audio.example.org/music включи свет")
	if err != nil || result.Route != "yandex" || len(music.started) != 0 {
		t.Fatalf("extra command accepted: %+v error=%v", result, err)
	}
}

func TestMusicSourceClarificationAcceptsSpelledAddressAndPromptsAgainOnUnknownSuffix(t *testing.T) {
	e, music := clarificationEngine(t)
	ctx := dialogue.WithSpeaker(t.Context(), "speaker")
	if _, err := e.Resolve(ctx, "Включи музыку с сайта абв точка неизвестно"); err != nil {
		t.Fatal(err)
	}
	result, err := e.Resolve(ctx, "абв точка неизвестно")
	if err != nil || result.Route != "music" || !strings.Contains(result.Answer, "по буквам") || len(music.started) != 0 {
		t.Fatalf("ambiguous reply=%+v started=%v error=%v", result, music.started, err)
	}
	result, err = e.Resolve(ctx, "эй би си точка орг")
	if err != nil || result.Route != "music" || len(music.started) != 1 || music.started[0] != "Включи музыку с сайта https://abc.org" {
		t.Fatalf("spelled reply=%+v started=%v error=%v", result, music.started, err)
	}
}

func TestPendingMusicSourceDoesNotSwallowNewCommands(t *testing.T) {
	for _, text := range []string{"Сыграй музыку с сайта эй би си точка орг", "Воспроизведи музыку с сайта эй би си точка орг", "Алиса, сыграйте музыку с сайта эй би си точка орг"} {
		t.Run(text, func(t *testing.T) {
			e, music := clarificationEngine(t)
			ctx := dialogue.WithSpeaker(t.Context(), "speaker")
			if _, err := e.Resolve(ctx, "Включи музыку с сайта абв точка неизвестно"); err != nil {
				t.Fatal(err)
			}
			d := e.Decide(ctx, text)
			if d.Source == "music_source_prompt" || d.Source == "music_source_reply" {
				t.Fatalf("new command swallowed by pending reply: %+v", d)
			}
			if _, err := e.ResolveDecision(ctx, text, d); err != nil {
				t.Fatal(err)
			}
			if len(music.started) != 1 || music.started[0] != text {
				t.Fatalf("new command not executed: %v", music.started)
			}
		})
	}
}

func TestPendingMusicSourceDoesNotSwallowUnrelatedAddressDiscussion(t *testing.T) {
	e, _ := clarificationEngine(t)
	ctx := dialogue.WithSpeaker(t.Context(), "speaker")
	if _, err := e.Resolve(ctx, "Включи музыку с сайта абв точка неизвестно"); err != nil {
		t.Fatal(err)
	}
	policy := DefaultRoutingConfig()
	policy.DefaultRoute = "yandex"
	policy.Rules = nil
	if err := e.SetRouting(policy); err != nil {
		t.Fatal(err)
	}
	d := e.Decide(ctx, "сравни сайты abc точка неизвестно")
	if d.Route != "yandex" {
		t.Fatalf("discussion swallowed by pending reply: %+v", d)
	}
	if e.dialogue().View(ctx).PendingMusicSource {
		t.Fatal("unrelated request retained pending source")
	}
}

func TestMusicClarificationWithRealToolsAndMissingSource(t *testing.T) {
	for _, text := range []string{"Включи музыку с сайта", "Включи музыку с сайта abc.invalid", "Включи музыку с сайта абв точка слух"} {
		t.Run(text, func(t *testing.T) {
			cfg := assistant.DefaultConfig()
			cfg.PlaySound = true
			reader := assistant.NewWebReader(cfg)
			queue, err := assistant.NewMusicQueue("http://127.0.0.1:8443", reader)
			if err != nil {
				t.Fatal(err)
			}
			tools := &assistant.Tools{Config: cfg, WebReader: reader, Playback: assistant.NewMusicPlayback(queue)}
			engine := &Engine{Music: tools}
			result, err := engine.Resolve(dialogue.WithSpeaker(t.Context(), "clarification"), text)
			if err != nil || result.Route != "music" || !strings.Contains(result.Answer, "по буквам") {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}
