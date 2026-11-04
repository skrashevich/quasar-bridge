package bridge

import (
	"context"
	"strings"
	"testing"

	directives "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	deferred "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/streaming"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

func TestCloudReplyAnimationsDoNotInvalidateRetry(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		for _, tc := range []struct {
			name, kind string
			allowed    bool
		}{
			{"tts_play_placeholder", "client_action", true},
			{"draw_animation_directive", "client_action", true},
			{"draw_scled_animations", "client_action", true},
			{"play_audio", "client_action", false},
			{"turn_on", "server_action", false},
			{"unknown_action", "client_action", false},
			{"draw_animation_directive", "unknown_type", false},
		} {
			name := tc.kind + "/" + tc.name
			if streamed {
				name += "/streamed"
			}
			t.Run(name, func(t *testing.T) {
				calls := 0
				engine := &Engine{LLM: completionFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
					calls++
					if m[len(m)-1].Content != "почему небо голубое" {
						t.Errorf("wrong repeated question: %+v", m)
					}
					return "Ответ нейросети о рассеянии света.", nil
				})}
				speaker := dialogue.WithSpeaker(t.Context(), "speaker")
				ctx := engine.Prepare(speaker)
				native := NewNative(t.Context(), nil, nil, engine)
				defer native.Close()
				p := &nativeRequest{ctx: ctx, text: "почему небо голубое", route: "yandex", cloud: true}
				ds := []*directives.TDirective{{Type: tc.kind, Name: tc.name}, {Type: "client_action", Name: "tts_play_placeholder"}}
				cloud := &response.TAliceResponseProto{
					Response:      &response.TAliceResponseProto_TResponse{Directives: ds, IsStreaming: streamed},
					VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("Ответ Алисы.")}},
				}
				if streamed {
					cloud.VoiceResponse.OutputSpeech.Text = new("")
				}
				native.captureCloud(p, &server.TServerMessage_TEvent{Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: cloud}})
				if streamed {
					part := &deferred.TDeferedAliceResponseProto{IsLast: true, TDeferredResponse: &deferred.TDeferedAliceResponseProto_BaseResponse{BaseResponse: &deferred.TDeferedAliceResponseProto_TBaseResponse{Text: new("Ответ Алисы."), Directives: ds}}}
					native.captureCloud(p, &server.TServerMessage_TEvent{Event: &server.TServerMessage_TEvent_DeferredAliceResponse{DeferredAliceResponse: part}})
				}
				got, err := engine.Resolve(speaker, "используй нейросеть")
				if err != nil {
					t.Fatal(err)
				}
				if tc.allowed {
					if got.Answer != "Ответ нейросети о рассеянии света." || calls != 1 {
						t.Fatalf("ordinary reply not repeatable: %+v calls=%d", got, calls)
					}
				} else if !strings.Contains(got.Answer, "Нет завершённого вопроса") || calls != 0 {
					t.Fatalf("action became repeatable: %+v calls=%d", got, calls)
				}
				if tc.name != "tts_play_placeholder" && canReplaceResponse(cloud) {
					t.Fatal("retry change widened automatic cloud replacement")
				}
			})
		}
	}
}
