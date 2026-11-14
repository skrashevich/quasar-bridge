package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/sound"
)

// A query must preserve custom prompts and real tool results while keeping
// synthesis markup out of its text and reporting captured playback as simulated.
func TestMilestoneQueryPromptsSoundMarkup(t *testing.T) {
	const generalPrompt = "Custom milestone dialogue instructions."
	const toolPrompt = "Custom milestone tool instructions."
	var calls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []llm.Message `json:"messages"`
			Tools    []llm.Tool    `json:"tools"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(request.Messages) == 0 || request.Messages[0].Role != "system" {
			t.Error("missing initial system instructions")
			http.Error(w, "missing system instructions", http.StatusBadRequest)
			return
		}
		for _, instruction := range []string{generalPrompt, toolPrompt, `<speaker is_whisper="true">`} {
			if !strings.Contains(request.Messages[0].Content, instruction) {
				t.Errorf("outbound prompt lost %q", instruction)
			}
		}
		for _, message := range request.Messages[1:] {
			if message.Role == "system" {
				t.Error("system instructions were split into multiple messages")
			}
		}
		if !slices.ContainsFunc(request.Tools, func(tool llm.Tool) bool {
			return tool.Function.Name == "play_sound"
		}) {
			t.Error("sound tool was not advertised")
		}
		message := llm.Message{Role: "assistant"}
		finishReason := "stop"
		switch step := calls.Add(1); step {
		case 1:
			if len(request.Messages) != 2 || request.Messages[1].Role != "user" {
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
				t.Errorf("sound tool did not queue the requested source: %+v, err=%v", last, err)
			}
			message.Content = `<speaker is_whisper="true">Готово.`
		default:
			t.Errorf("unexpected LLM call %d", step)
			http.Error(w, "unexpected LLM call", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finishReason}}}); err != nil {
			t.Error(err)
		}
	}))
	defer api.Close()
	config, err := json.Marshal(map[string]any{
		"mode": "hybrid", "airplay": map[string]bool{"disabled": true},
		"llm_base_url": api.URL, "llm_model": "fixture",
		"llm_system_prompt": generalPrompt, "llm_tts_markup": true,
		"assistant_tools": map[string]any{
			"enabled": true, "system_prompt": toolPrompt, "play_sound": true,
			"web": false, "history": false, "time": false, "updates": false, "configuration": false,
		},
		"routing": map[string]any{"rules": []any{}, "default_route": "llm", "classifier": map[string]bool{"enabled": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runQueryProcess(t, configFile(t, string(config)), "Подай короткий сигнал и ответь шёпотом")
	if err != nil {
		t.Fatalf("query failed: %v, stdout=%s stderr=%s", err, stdout, stderr)
	}
	var report queryResolution
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("invalid query report: %v, stdout=%s", err, stdout)
	}
	if report.Route != "llm" || report.Answer != "Готово." {
		t.Fatalf("query lost its route or leaked synthesis markup: %s", stdout)
	}
	if !report.PlaybackSimulated || len(report.Playback) != 1 || report.Playback[0].Request != (sound.Request{Sound: "notification"}) || len(report.Playback[0].Tracks) != 0 {
		t.Fatalf("query did not report the simulated signal: %s", stdout)
	}
	if calls.Load() != 2 {
		t.Fatalf("LLM HTTP calls=%d, want 2", calls.Load())
	}
}
