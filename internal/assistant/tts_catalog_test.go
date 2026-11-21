package assistant

import (
	"context"
	"encoding/json/v2"
	"slices"
	"testing"

	"quasarbridge/internal/llm"
)

func TestAgentLooksUpTTSVoiceAndSound(t *testing.T) {
	const reply = `<speaker voice="zahar" audio="alice-sounds-animals-cat-1.opus">Мяу.`
	round := 0
	agent := Agent{
		Tools: &Tools{TTSMarkup: true}, MaxRounds: 2, MaxCalls: 2,
		Client: chatFunc(func(_ context.Context, messages []llm.Message, tools []llm.Tool) (llm.Message, error) {
			round++
			switch round {
			case 1:
				if !slices.ContainsFunc(tools, func(tool llm.Tool) bool { return tool.Function.Name == "search_tts_catalog" }) {
					t.Fatal("the provider did not receive the catalog tool")
				}
				return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
					{ID: "voice", Type: "function", Function: llm.FunctionCall{Name: "search_tts_catalog", Arguments: `{"kind":"voice","query":"ЗАХАР"}`}},
					{ID: "sound", Type: "function", Function: llm.FunctionCall{Name: "search_tts_catalog", Arguments: `{"kind":"audio","query":"Кошка № 1"}`}},
				}}, nil
			case 2:
				want := map[string]string{"voice": "zahar", "sound": "alice-sounds-animals-cat-1.opus"}
				for _, message := range messages {
					if message.Role != "tool" {
						continue
					}
					var items []struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal([]byte(message.Content), &items); err != nil || len(items) == 0 || items[0].ID != want[message.ToolCallID] {
						t.Fatalf("wrong catalog result for %s: %s (%v)", message.ToolCallID, message.Content, err)
					}
					delete(want, message.ToolCallID)
				}
				if len(want) != 0 {
					t.Fatalf("missing tool results: %v", want)
				}
				return llm.Message{Role: "assistant", Content: reply}, nil
			default:
				t.Fatal("unexpected model round")
				return llm.Message{}, nil
			}
		}),
	}
	got, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "Озвучь голосом Захара и добавь мяуканье"}}, false)
	if err != nil || got != reply || round != 2 {
		t.Fatalf("answer %q after %d rounds: %v", got, round, err)
	}
}

func TestTTSCatalogRespectsDisabledMarkup(t *testing.T) {
	tools := &Tools{}
	if slices.ContainsFunc(tools.Definitions(), func(tool llm.Tool) bool { return tool.Function.Name == "search_tts_catalog" }) {
		t.Fatal("advertised catalog with TTS markup disabled")
	}
	if _, err := tools.Call(t.Context(), "search_tts_catalog", `{"kind":"voice","query":"Захар"}`); err == nil {
		t.Fatal("executed catalog search with TTS markup disabled")
	}
}
