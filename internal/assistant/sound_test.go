package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/sound"
)

func soundTools(t *testing.T) *Tools {
	t.Helper()
	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(`{"play_sound":true}`), &cfg); err != nil {
		t.Fatal(err)
	}
	return &Tools{Config: cfg}
}

func TestPlaySoundIsOptIn(t *testing.T) {
	for _, def := range (&Tools{Config: DefaultConfig()}).Definitions() {
		if def.Function.Name == "play_sound" {
			t.Fatal("playback enabled by default")
		}
	}
	if _, err := (&Tools{Config: DefaultConfig()}).Call(t.Context(), "play_sound", `{"source":"notification"}`); err == nil {
		t.Fatal("disabled playback was callable")
	}
	for _, def := range soundTools(t).Definitions() {
		if def.Function.Name == "play_sound" {
			required, ok := def.Function.Parameters["required"].([]string)
			if !ok || !slices.Equal(required, []string{"source"}) {
				t.Fatalf("model needs a required source parameter: %v", def.Function.Parameters)
			}
			return
		}
	}
	t.Fatal("enabled play_sound is absent from model tools")
}

func TestPlaySoundRejectsInvalidArguments(t *testing.T) {
	tools := soundTools(t)
	for _, args := range []string{
		`null`, `{}`, `[]`, `{"source":null}`, `{"source":""}`,
		`{"source":"music"}`, `{"source":1}`, `{"source":true}`, `{"source":{}}`,
		`{"source":"notification","url":"https://example.org/test.mp3"}`,
		`{"source":"notification","extra":true}`, `{"sound":"notification"}`, `{"url":"https://example.org/test.mp3"}`,
		`{"source":"/tmp/test.wav"}`, `{"source":"file:///tmp/test.wav"}`,
		`{"source":"https:///test.mp3"}`, `{"source":"https://user:password@example.org/test.mp3"}`,
		`{"source":"https://example.org/test.mp3#fragment"}`, `{"source":"https://example.org/\nfile"}`,
		`{"source":"https://example.org/` + strings.Repeat("a", 2048) + `"}`,
	} {
		t.Run(args[:min(len(args), 80)], func(t *testing.T) {
			if _, err := tools.Call(t.Context(), "play_sound", args); err == nil || !strings.Contains(err.Error(), "invalid play_sound") {
				t.Fatalf("invalid input accepted or wrong error: %v", err)
			}
		})
	}
}

func TestPlaySoundRequiresCurrentSpeaker(t *testing.T) {
	for _, args := range []string{
		`{"source":"notification"}`, `{"source":"success"}`, `{"source":"error"}`, `{"source":"timer"}`,
		`{"source":"http://` + net.IPv4(10, 0, 0, 1).String() + `/test.wav"}`, `{"source":"https://example.org/test.mp3?part=1"}`,
	} {
		if _, err := soundTools(t).Call(t.Context(), "play_sound", args); err == nil || !strings.Contains(err.Error(), "current speaker") {
			t.Fatalf("playback without speaker reported success or validation failed: %v", err)
		}
	}
}

func TestPlaySoundCanceledBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ctx = sound.WithPlayer(ctx, func(context.Context, sound.Request) error {
		t.Fatal("canceled request reached player")
		return nil
	})
	cancel()
	if _, err := soundTools(t).Call(ctx, "play_sound", `{"source":"notification"}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled playback error: %v", err)
	}
}

func TestPlaySoundQueuesOnCurrentSpeaker(t *testing.T) {
	for _, request := range []sound.Request{{Sound: "notification"}, {URL: "http://192.0.2.10/Test.wav?part=1"}} {
		t.Run(request.Sound+request.URL, func(t *testing.T) {
			calls := 0
			ctx := sound.WithPlayer(t.Context(), func(ctx context.Context, got sound.Request) error {
				calls++
				if got != request {
					t.Errorf("player request changed: got %+v want %+v", got, request)
				}
				return ctx.Err()
			})
			source := request.Sound
			if source == "" {
				source = request.URL
			}
			args, err := json.Marshal(map[string]string{"source": source})
			if err != nil {
				t.Fatal(err)
			}
			result, err := soundTools(t).Call(ctx, "play_sound", string(args))
			if err != nil || calls != 1 {
				t.Fatalf("queued calls=%d error=%v", calls, err)
			}
			value, ok := result.(map[string]any)
			if !ok || value["status"] != "queued" || value["request"] != request {
				t.Fatalf("unexpected playback result: %#v", result)
			}
		})
	}
}

func TestPlaySoundPropagatesPlayerError(t *testing.T) {
	want := errors.New("speaker request canceled before delivery")
	ctx := sound.WithPlayer(t.Context(), func(context.Context, sound.Request) error { return want })
	if result, err := soundTools(t).Call(ctx, "play_sound", `{"source":"success"}`); result != nil || !errors.Is(err, want) {
		t.Fatalf("player failure became success: result=%v error=%v", result, err)
	}
}

func TestPlaySoundIsNotPrefetched(t *testing.T) {
	tools := soundTools(t)
	tools.Config.Time = false
	tools.Config.Web = false
	tools.Config.History = false
	if calls := tools.InitialCalls("воспроизведи сигнал notification"); len(calls) != 0 {
		t.Fatalf("playback was prefetched: %v", calls)
	}
}

func TestPlaybackURLSkipsWebPrefetchAndKeepsClock(t *testing.T) {
	for _, query := range []string{
		"Воспроизведи https://example.org/Test.mp3",
		"Проиграй http://192.0.2.10/Test.wav",
		"Включи аудио https://example.org/Test.mp3",
	} {
		calls := soundTools(t).InitialCalls(query)
		if len(calls) != 1 || calls[0].Name != "current_time" {
			t.Errorf("playback URL prefetched as webpage or clock lost: %v", calls)
		}
	}
	for _, enabled := range []bool{true, false} {
		tools := soundTools(t)
		tools.Config.PlaySound = enabled
		calls := tools.InitialCalls("прочитай страницу https://example.org/Test")
		if len(calls) != 2 || calls[0].Name != "current_time" || calls[1].Name != "read_webpage" {
			t.Errorf("ordinary page prefetch changed: %v", calls)
		}
	}
}
