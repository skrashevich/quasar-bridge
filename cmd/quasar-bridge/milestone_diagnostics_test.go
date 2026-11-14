package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
)

// Dashboard queries must use the CLI's simulated player without sharing its
// dialogue identity or losing custom prompts and plain-text answer handling.
func TestMilestoneDiagnosticsQueryCapturesSound(t *testing.T) {
	const generalPrompt = "Custom dashboard dialogue instructions."
	const toolPrompt = "Custom dashboard tool instructions."
	const question = "Подай короткий сигнал и ответь шёпотом"
	var calls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []llm.Message `json:"messages"`
			Tools    []llm.Tool    `json:"tools"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil || len(request.Messages) < 2 || request.Messages[0].Role != "system" {
			t.Errorf("invalid LLM request: %+v, error=%v", request, err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		for _, instruction := range []string{generalPrompt, toolPrompt, `<speaker is_whisper="true">`} {
			if !strings.Contains(request.Messages[0].Content, instruction) {
				t.Errorf("outbound prompt lost %q", instruction)
			}
		}
		if slices.ContainsFunc(request.Messages[1:], func(message llm.Message) bool { return message.Role == "system" }) {
			t.Error("system instructions were split into multiple messages")
		}
		if !slices.ContainsFunc(request.Tools, func(tool llm.Tool) bool { return tool.Function.Name == "play_sound" }) {
			t.Error("sound tool was not advertised")
		}
		message := llm.Message{Role: "assistant"}
		finishReason := "stop"
		switch step := calls.Add(1); step {
		case 1:
			if len(request.Messages) != 2 || request.Messages[1].Content != question {
				t.Errorf("unexpected initial conversation: %+v", request.Messages)
			}
			message.ToolCalls = []llm.ToolCall{{ID: "signal", Type: "function", Function: llm.FunctionCall{Name: "play_sound", Arguments: `{"source":"notification"}`}}}
			finishReason = "tool_calls"
		case 2:
			last := request.Messages[len(request.Messages)-1]
			var result struct {
				Status  string        `json:"status"`
				Request sound.Request `json:"request"`
			}
			if err := json.Unmarshal([]byte(last.Content), &result); err != nil || last.Role != "tool" || last.ToolCallID != "signal" || result.Status != "queued" || result.Request != (sound.Request{Sound: "notification"}) {
				t.Errorf("sound tool did not queue the signal: %+v, error=%v", last, err)
			}
			message.Content = `<speaker is_whisper="true">Готово.`
		default:
			t.Errorf("unexpected LLM call %d", step)
			http.Error(w, "unexpected call", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finishReason}}}); err != nil {
			t.Error(err)
		}
	}))
	defer api.Close()
	client := &llm.Client{BaseURL: api.URL, Model: "fixture", HTTP: api.Client()}
	tools := &assistant.Tools{Config: assistant.Config{PlaySound: true}, TTSMarkup: true}
	engine := &bridge.Engine{
		SystemPrompt: generalPrompt, TTSMarkup: true,
		Assistant: &assistant.Agent{Client: client, Tools: tools, SystemPrompt: toolPrompt, MaxRounds: 3, MaxCalls: 4},
		Dialogue:  dialogue.New(dialogue.DefaultConfig()),
	}
	routing := bridge.DefaultRoutingConfig()
	routing.Rules, routing.DefaultRoute, routing.Classifier.Enabled = nil, "llm", new(false)
	if err := engine.SetRouting(routing); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Engine: engine})
	response := diagnosticAPIRequest(mux, "POST", "query", `{"text":"`+question+`"}`, "application/json")
	var report struct {
		Route             string          `json:"route"`
		Answer            string          `json:"answer"`
		ElapsedMS         int64           `json:"elapsed_ms"`
		Playback          []queryPlayback `json:"playback"`
		PlaybackSimulated bool            `json:"playback_simulated"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil || response.Code != http.StatusOK {
		t.Fatalf("dashboard query failed: status=%d body=%s error=%v", response.Code, response.Body, err)
	}
	if report.Route != "llm" || report.Answer != "Готово." || report.ElapsedMS < 0 {
		t.Errorf("query lost its route or leaked TTS markup: %s", response.Body)
	}
	if !report.PlaybackSimulated || len(report.Playback) != 1 || report.Playback[0].Request != (sound.Request{Sound: "notification"}) || len(report.Playback[0].Tracks) != 0 {
		t.Errorf("dashboard did not report the simulated signal: %s", response.Body)
	}
	if calls.Load() != 2 {
		t.Errorf("LLM calls=%d, want 2", calls.Load())
	}
	if got := engine.Dialogue.View(dialogue.WithSpeaker(t.Context(), "quasar-bridge-dashboard")).Messages; len(got) != 2 || got[0].Role != "user" || got[0].Content != question || got[1].Role != "assistant" || got[1].Content != "Готово." {
		t.Errorf("dashboard history lost its identity or plain answer: %+v", got)
	}
	for _, ctx := range []string{"query-diagnostics", "physical-speaker"} {
		if got := engine.Dialogue.View(dialogue.WithSpeaker(t.Context(), ctx)).Messages; len(got) != 0 {
			t.Errorf("dashboard contaminated %q history: %+v", ctx, got)
		}
	}
	if got := engine.Dialogue.View(t.Context()).Messages; len(got) != 0 {
		t.Errorf("dashboard contaminated unnamed history: %+v", got)
	}
}

// The real music shortcut must return original sources, rather than only its
// internal queue URL, while requiring no physical speaker or LLM connection.
func TestMilestoneDiagnosticsQueryCapturesMusic(t *testing.T) {
	const source = "https://music.example/song.mp3?edition=2"
	reader := assistant.NewWebReader(assistant.DefaultConfig())
	queue, err := assistant.NewMusicQueue("https://bridge.example", reader)
	if err != nil {
		t.Fatal(err)
	}
	liveTracks := []assistant.WebLink{{URL: "https://music.example/live.mp3", Title: "Live track"}}
	var liveQueues []string
	for range 16 {
		stream, err := queue.Register(liveTracks)
		if err != nil {
			t.Fatal(err)
		}
		liveQueues = append(liveQueues, stream)
	}
	player := assistant.NewMusicPlayback(queue)
	tools := &assistant.Tools{Config: assistant.Config{Web: true, PlaySound: true}, WebReader: reader, Playback: player}
	engine := &bridge.Engine{Music: tools, Playback: player}
	if err := engine.SetRouting(bridge.DefaultRoutingConfig()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerDiagnosticsAPI(mux, diagnosticControl{Engine: engine, MusicQueue: queue})
	for i := range 20 {
		response := diagnosticAPIRequest(mux, "POST", "query", `{"text":"Включи музыку с `+source+`"}`, "application/json")
		var report struct {
			Route             string          `json:"route"`
			Answer            string          `json:"answer"`
			Playback          []queryPlayback `json:"playback"`
			PlaybackSimulated bool            `json:"playback_simulated"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil || response.Code != http.StatusOK || report.Route != "music" || report.Answer != "Включаю музыку." {
			t.Fatalf("direct music query %d failed with a full live queue: status=%d body=%s error=%v", i+1, response.Code, response.Body, err)
		}
		if !report.PlaybackSimulated || len(report.Playback) != 1 {
			t.Fatalf("missing simulated music playback: %s", response.Body)
		}
		played := report.Playback[0]
		if played.StreamID == "" || !strings.HasPrefix(played.URL, "https://bridge.example/music-stream/") || len(played.Tracks) != 1 || played.Tracks[0].URL != source {
			t.Fatalf("lost original source or queue identity: %+v", played)
		}
	}
	for _, stream := range liveQueues {
		if got, err := queue.Tracks(stream); err != nil || !slices.Equal(got, liveTracks) {
			t.Fatalf("diagnostic query changed a live queue: tracks=%+v error=%v", got, err)
		}
	}
	if _, err := queue.Register(liveTracks); err == nil {
		t.Fatal("diagnostic query removed a live queue entry")
	}
}

// A sound request may reference an existing live queue: read its metadata
// without copying it into the isolated queue or deleting the real entry.
func TestResolveQueryReadsExistingQueueSources(t *testing.T) {
	queue, err := assistant.NewMusicQueue("https://bridge.example", assistant.NewWebReader(assistant.DefaultConfig()))
	if err != nil {
		t.Fatal(err)
	}
	tracks := []assistant.WebLink{{URL: "https://music.example/live.mp3", Title: "Live track"}}
	stream, err := queue.Register(tracks)
	if err != nil {
		t.Fatal(err)
	}
	report, err := resolveQuery(t.Context(), queryResolverFunc(func(ctx context.Context, _ string) (bridge.Resolution, error) {
		return bridge.Resolution{Route: "llm", Answer: "Готово."}, sound.Play(ctx, sound.Request{URL: stream})
	}), queue, "Повтори этот звук")
	if err != nil || !report.PlaybackSimulated || len(report.Playback) != 1 || !slices.Equal(report.Playback[0].Tracks, tracks) {
		t.Fatalf("existing queue sources were lost: report=%+v error=%v", report, err)
	}
	if got, err := queue.Tracks(stream); err != nil || !slices.Equal(got, tracks) {
		t.Fatalf("diagnostic sound changed a live queue: tracks=%+v error=%v", got, err)
	}
}
