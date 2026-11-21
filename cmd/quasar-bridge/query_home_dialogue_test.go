package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/home"
	"quasarbridge/internal/llm"
)

type homeDialogueTools func(context.Context, string, map[string]any) (jsontext.Value, error)

func (f homeDialogueTools) Call(ctx context.Context, name string, args map[string]any) (jsontext.Value, error) {
	return f(ctx, name, args)
}

func TestQueryHomeConfirmationKeepsDialogContext(t *testing.T) {
	for _, confirmation := range []string{"да", "нет", "только доступные"} {
		t.Run(confirmation, func(t *testing.T) {
			const question = "норм. включи свет в кабинете"
			const clarification = "Некоторые светильники в кабинете недоступны. Включить только доступные?"
			var actions atomic.Int32
			var cloudCalls int
			plannerCalls := 0
			hc := &home.Controller{Tools: homeDialogueTools(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
				var value any
				switch name {
				case "ha_get_skill_guide":
					value = map[string]any{"success": true, "content": "fixture"}
				case "ha_search":
					var entities []home.Entity
					if args["domain_filter"] == "light" {
						entities = []home.Entity{
							{ID: "light.office", Name: "Свет в кабинете", State: "off", Area: "Кабинет"},
							{ID: "light.office_offline", Name: "Недоступный светильник", State: "unavailable", Area: "Кабинет"},
						}
						for i := range 81 {
							entities = append(entities, home.Entity{ID: fmt.Sprintf("light.other_%d", i), Name: "Свет в гостиной", State: "off", Area: "Гостиная"})
						}
					}
					value = map[string]any{"entities": entities, "has_more": false}
				case "ha_get_entity_exposure":
					value = map[string]any{"exposed_to": map[string]any{"conversation": true}}
				case "ha_call_service":
					if confirmation == "нет" || args["entity_id"] != "light.office" || args["service"] != "turn_on" {
						t.Errorf("unexpected physical action: %+v", args)
					}
					actions.Add(1)
					value = map[string]any{"success": true}
				case "ha_get_state":
					value = map[string]any{"data": map[string]any{"state": "on"}}
				default:
					t.Errorf("unexpected tool %q", name)
				}
				return json.Marshal(value)
			}), Planner: diagnosticCompleter(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				plannerCalls++
				if !structured {
					t.Error("confirmation must use a validated home plan")
				}
				if plannerCalls == 1 {
					return `{"answer":"` + clarification + `","actions":[]}`, nil
				}
				if len(messages) != 4 || messages[1].Role != "user" || messages[1].Content != question || messages[2].Role != "assistant" || messages[2].Content != clarification || !strings.Contains(messages[3].Content, "Запрос: "+confirmation) {
					t.Errorf("home planner lost the clarification: %+v", messages)
				}
				if strings.Contains(messages[len(messages)-1].Content, "light.other_") {
					t.Error("confirmation lost the room and loaded unrelated devices")
				}
				if confirmation == "нет" {
					return `{"answer":"Хорошо, ничего не меняю.","actions":[]}`, nil
				}
				return `{"answer":"Включаю доступный свет.","actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}}]}`, nil
			})}
			engine := &bridge.Engine{Home: hc, Dialogue: dialogue.New(dialogue.DefaultConfig()), LLM: diagnosticCompleter(func(_ context.Context, _ []llm.Message, _ bool) (string, error) {
				return `{"route":"yandex"}`, nil
			})}
			mux := http.NewServeMux()
			registerDiagnosticsAPI(mux, diagnosticControl{Engine: engine, QueryBusy: new(atomic.Bool), Dialogs: newQueryDialogs(time.Minute), YandexQuery: func(_ context.Context, _ string, _ *queryDialog) (string, error) {
				cloudCalls++
				return "Привет, как вы?", nil
			}})
			ask := func(text, id string) (string, string) {
				t.Helper()
				body, _ := json.Marshal(map[string]string{"text": text, "dialog_id": id})
				response := diagnosticAPIRequest(mux, "POST", "query", string(body), "application/json")
				var result struct {
					Route    string `json:"route"`
					DialogID string `json:"dialog_id"`
				}
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
					t.Fatalf("query failed: %d %s", response.Code, response.Body)
				}
				return result.Route, result.DialogID
			}
			_, id := ask("привет", "")
			if route, same := ask(question, id); route != "home" || same != id || actions.Load() != 0 {
				t.Fatalf("clarification failed: route=%s dialog=%s actions=%d", route, same, actions.Load())
			}
			// A new dialog must not inherit the other dialog's pending confirmation.
			if route, other := ask(confirmation, ""); route != "yandex" || other == id || actions.Load() != 0 {
				t.Fatal("home context leaked into a separate API dialog")
			}
			if route, same := ask(confirmation, id); route != "home" || same != id {
				t.Fatalf("confirmation lost home context: route=%s dialog=%s", route, same)
			}
			wantActions := int32(0)
			if confirmation != "нет" {
				wantActions = 1
			}
			if actions.Load() != wantActions || plannerCalls != 2 || cloudCalls != 2 {
				t.Fatalf("actions=%d planner=%d cloud=%d", actions.Load(), plannerCalls, cloudCalls)
			}
		})
	}
}
