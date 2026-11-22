package assistant

import (
	"context"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/llm"
	"github.com/skrashevich/quasar-bridge/internal/sound"
)

func TestPrivateHistoryBlocksSoundURL(t *testing.T) {
	for _, withMCP := range []bool{false, true} {
		t.Run(map[bool]string{false: "builtin", true: "mcp_wrapper"}[withMCP], func(t *testing.T) {
			tools := soundTools(t)
			tools.Config.Time = false
			if withMCP {
				tools.Config.MCPServers = []MCPServerConfig{{Name: "disabled", Disabled: true}}
			}
			played := 0
			ctx := sound.WithPlayer(t.Context(), func(context.Context, sound.Request) error {
				played++
				return nil
			})
			rounds := 0
			agent := Agent{Tools: tools, MaxCalls: 2, MaxRounds: 2, Client: chatFunc(func(_ context.Context, _ []llm.Message, defs []llm.Tool) (llm.Message, error) {
				rounds++
				if rounds > 1 {
					return llm.Message{Content: "Запрос отклонён."}, nil
				}
				for _, def := range defs {
					if def.Function.Name == "play_sound" {
						t.Error("network playback offered after private history")
					}
				}
				return llm.Message{ToolCalls: []llm.ToolCall{{ID: "play", Type: "function", Function: llm.FunctionCall{Name: "play_sound", Arguments: `{"source":"https://audio.example/test.mp3?history=private"}`}}}}, nil
			})}
			if _, _, err := agent.CompleteDialogue(ctx, []llm.Message{{Role: "user", Content: "Воспроизведи звук"}}, true); err != nil {
				t.Fatal(err)
			}
			if played != 0 {
				t.Fatalf("private history could leave through a playback URL: %d calls", played)
			}
		})
	}
}
