package home

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type plannerFunc func(context.Context, []llm.Message, bool) (string, error)

func (f plannerFunc) Complete(ctx context.Context, m []llm.Message, s bool) (string, error) {
	return f(ctx, m, s)
}

type homeFixture struct {
	notExposed bool
	actions    int
	state      string
	fail       bool
}

func (f *homeFixture) Call(ctx context.Context, name string, args map[string]any) (jsontext.Value, error) {
	var value any
	switch name {
	case "ha_get_skill_guide":
		value = map[string]any{"success": true, "content": "fixture"}
	case "ha_search":
		value = map[string]any{"entities": []any{map[string]any{"entity_id": "light.office", "friendly_name": "Свет в кабинете", "state": f.state, "area": "Кабинет"}}, "has_more": false}
	case "ha_call_service":
		f.actions++
		if args["entity_id"] != "light.office" || args["domain"] != "light" || args["service"] != "turn_on" {
			panic("wrong service target")
		}
		if !f.fail {
			f.state = "on"
		}
		value = map[string]any{"success": true}
	case "ha_get_entity_exposure":
		value = map[string]any{"success": true, "exposed_to": map[string]any{"conversation": !f.notExposed}}
	case "ha_get_state":
		value = map[string]any{"data": map[string]any{"entity_id": "light.office", "state": f.state}}
	default:
		panic("unexpected tool")
	}
	data, _ := json.Marshal(value)
	return data, nil
}

func TestHomeValidatesTargetsAndConfirmsPhysicalState(t *testing.T) {
	for _, tc := range []struct {
		name, entity string
		fail         bool
		wantActions  int
		wantErr      bool
	}{
		{"known", "light.office", false, 1, false},
		{"unknown", "light.nonexistent", false, 0, true},
		{"comma_injection", "light.office,light.other", false, 0, true},
		{"ack_without_change", "light.office", true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &homeFixture{state: "off", fail: tc.fail}
			planner := plannerFunc(func(ctx context.Context, m []llm.Message, s bool) (string, error) {
				if !s || len(m) != 2 || !strings.Contains(m[1].Content, "light.office") {
					t.Fatal("planner lacks validated device context")
				}
				b, _ := json.Marshal(map[string]any{"answer": "Готово", "actions": []any{map[string]any{"domain": "light", "service": "turn_on", "entity_id": tc.entity, "data": map[string]any{}}}})
				return string(b), nil
			})
			c := Controller{Tools: fixture, Planner: planner, Domains: []string{"light"}}
			answer, err := c.Handle(t.Context(), "Включи свет в кабинете")
			if (err != nil) != tc.wantErr {
				t.Fatalf("answer=%q error=%v", answer, err)
			}
			if fixture.actions != tc.wantActions {
				t.Fatalf("service calls=%d want=%d", fixture.actions, tc.wantActions)
			}
			if !tc.wantErr && answer == "" {
				t.Fatal("no reply after confirmed action")
			}
		})
	}
}

func TestLightingRequestInventoryIncludesSwitchesWithoutAllSensors(t *testing.T) {
	got := controlDomains("Горит ли свет в кабинете?")
	if !slices.Contains(got, "light") || !slices.Contains(got, "switch") || slices.Contains(got, "sensor") {
		t.Fatalf("domains=%v", got)
	}
	lanterns := controlDomains("Включи фонарики")
	if !slices.Contains(lanterns, "light") || slices.Contains(lanterns, "sensor") {
		t.Fatalf("lantern domains=%v", lanterns)
	}
}

func TestHomeConfirmsLightBrightnessResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    map[string]any
		state   string
		wantErr bool
	}{
		{"brightness_zero_off", map[string]any{"brightness": 0}, "off", false},
		{"brightness_zero_still_on", map[string]any{"brightness": 0}, "on", true},
		{"brightness_fraction_truncates_to_zero_off", map[string]any{"brightness": 0.5}, "off", false},
		{"brightness_negative_clamps_to_zero_off", map[string]any{"brightness": -1}, "off", false},
		{"brightness_mid_on", map[string]any{"brightness": 127}, "on", false},
		{"brightness_max_on", map[string]any{"brightness": 255}, "on", false},
		{"brightness_positive_still_off", map[string]any{"brightness": 127}, "off", true},
		{"percent_zero_off", map[string]any{"brightness_pct": 0}, "off", false},
		{"percent_zero_still_on", map[string]any{"brightness_pct": 0}, "on", true},
		{"percent_positive_on", map[string]any{"brightness_pct": 50}, "on", false},
		{"percent_rounds_to_zero_off", map[string]any{"brightness_pct": 0.1}, "off", false},
		{"no_brightness_on", nil, "on", false},
		{"no_brightness_still_off", nil, "off", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &homeFixture{state: "off"}
			tools := toolFunc(func(ctx context.Context, name string, args map[string]any) (jsontext.Value, error) {
				raw, err := fixture.Call(ctx, name, args)
				if name == "ha_call_service" {
					fixture.state = tc.state
				}
				return raw, err
			})
			c := Controller{Tools: tools, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
				b, err := json.Marshal(Plan{Actions: []Action{{Domain: "light", Service: "turn_on", EntityID: "light.office", Data: tc.data}}})
				return string(b), err
			})}
			answer, err := c.Handle(t.Context(), "Установи яркость света в кабинете")
			if (err != nil) != tc.wantErr {
				t.Fatalf("answer=%q error=%v", answer, err)
			}
			if fixture.actions != 1 {
				t.Fatalf("service calls=%d want=1", fixture.actions)
			}
			if !tc.wantErr && answer == "" {
				t.Fatal("no reply after confirmed action")
			}
		})
	}
}

func TestHomeValidatesEntireBatchBeforeFirstAction(t *testing.T) {
	fixture := &homeFixture{state: "off"}
	c := Controller{Tools: fixture, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return `{"answer":"","actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}},{"domain":"light","service":"turn_on","entity_id":"light.missing","data":{}}]}`, nil
	})}
	if _, err := c.Handle(t.Context(), "Включи свет"); err == nil || fixture.actions != 0 {
		t.Fatalf("error=%v actions=%d", err, fixture.actions)
	}
}

func TestRoomScopeKeepsGroupsAndSeparatelyNamedFixtures(t *testing.T) {
	entities := []Entity{{ID: "light.group", Name: "Кабинет", Area: "Кабинет"}, {ID: "light.chandelier", Name: "Люстра", Area: "Кабинет"}, {ID: "switch.extra", Name: "Лампа кабинет"}, {ID: "light.bedroom", Name: "Спальня", Area: "Спальня"}}
	got := scopeEntities("Горит ли свет в кабинете?", entities)
	if len(got) != 3 || got[2].ID != "switch.extra" {
		t.Fatalf("scope=%v", got)
	}
}

func TestHomeDoesNotExposeStatusLEDsToRoomLighting(t *testing.T) {
	fixture := &homeFixture{state: "on", notExposed: true}
	c := Controller{Tools: fixture, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("unexposed device sent to planner")
		return "", nil
	})}
	answer, err := c.Handle(t.Context(), "Горит ли свет в кабинете?")
	if err != nil || answer == "" || fixture.actions != 0 {
		t.Fatalf("answer=%q error=%v", answer, err)
	}
}

type toolFunc func(context.Context, string, map[string]any) (jsontext.Value, error)

func (f toolFunc) Call(ctx context.Context, name string, args map[string]any) (jsontext.Value, error) {
	return f(ctx, name, args)
}
func TestHomeKeepsGroupWhenPlanAlsoListsItsMember(t *testing.T) {
	for _, second := range []string{"light.office", "light.member"} {
		t.Run(second, func(t *testing.T) {
			state := "off"
			actions := 0
			tools := toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
				var v any = map[string]any{"success": true}
				switch name {
				case "ha_search":
					v = map[string]any{"entities": []Entity{{ID: "light.office", Name: "Фонарики", State: "off", Members: []string{"light.member"}}, {ID: "light.member", Name: "Lamp", State: "off"}}}
				case "ha_get_entity_exposure":
					v = map[string]any{"exposed_to": map[string]any{"conversation": true}}
				case "ha_get_state":
					v = map[string]any{"data": map[string]any{"state": state}}
				case "ha_call_service":
					if args["entity_id"] != "light.office" {
						t.Fatalf("member called separately: %v", args["entity_id"])
					}
					actions++
					state = "on"
				}
				b, _ := json.Marshal(v)
				return b, nil
			})
			c := Controller{Tools: tools, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
				b, _ := json.Marshal(Plan{Actions: []Action{{Domain: "light", Service: "turn_on", EntityID: "light.office"}, {Domain: "light", Service: "turn_on", EntityID: second}}})
				return string(b), nil
			})}
			answer, err := c.Handle(t.Context(), "Включи фонарики")
			if err != nil || actions != 1 || !strings.Contains(answer, "Фонарики") {
				t.Fatalf("answer=%q err=%v actions=%d", answer, err, actions)
			}
		})
	}
}

func TestStateConfirmationWaitsForSlowLight(t *testing.T) {
	reads := 0
	tools := toolFunc(func(_ context.Context, name string, _ map[string]any) (jsontext.Value, error) {
		var v any = map[string]any{"success": true, "exposed_to": map[string]any{"conversation": true}}
		switch name {
		case "ha_search":
			v = map[string]any{"entities": []Entity{{ID: "light.lanterns", Name: "Фонарики", State: "off", Area: "Зал"}}}
		case "ha_get_state":
			reads++
			state := "off"
			if reads >= 3 {
				state = "on"
			}
			v = map[string]any{"data": map[string]any{"state": state}}
		}
		b, _ := json.Marshal(v)
		return b, nil
	})
	c := Controller{Tools: tools, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return `{"answer":"","actions":[{"domain":"light","service":"turn_on","entity_id":"light.lanterns","data":{}}]}`, nil
	})}
	answer, err := c.Handle(t.Context(), "Включи фонарики")
	if err != nil || answer != "Готово: Фонарики." || reads != 3 {
		t.Fatalf("answer=%q err=%v reads=%d", answer, err, reads)
	}
}

func TestHomeWarmNeverPlansOrActuates(t *testing.T) {
	f := &homeFixture{state: "off"}
	c := Controller{Tools: f, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		t.Fatal("warm called planner")
		return "", nil
	})}
	if err := c.Warm(t.Context(), "Кабинет"); err != nil || f.actions != 0 {
		t.Fatalf("err=%v actions=%d", err, f.actions)
	}
}

func TestUnspecifiedRoomUsesAreaNamedInSystemPrompt(t *testing.T) {
	note := "Ты находишься в комнате «Зал» в городе «Москва». Если пользователь не указал комнату, считай, что речь идёт о Зале. Не спрашивай комнату, если в Зале есть свет."
	entities := []Entity{{ID: "light.hall", Name: "Люстра", State: "off", Area: "Зал"}, {ID: "light.kitchen", Name: "Кухня", State: "off", Area: "Кухня"}, {ID: "light.room", Name: "Комната", State: "off", Area: "Комната"}}
	saw := func(text string) string {
		t.Helper()
		var inventory string
		tools := toolFunc(func(_ context.Context, name string, _ map[string]any) (jsontext.Value, error) {
			var v any = map[string]any{"success": true, "exposed_to": map[string]any{"conversation": true}}
			if name == "ha_search" {
				v = map[string]any{"entities": entities}
			}
			b, _ := json.Marshal(v)
			return b, nil
		})
		c := Controller{Tools: tools, Domains: []string{"light"}, SystemPrompt: note, Planner: plannerFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
			if !structured || len(m) != 2 {
				t.Fatalf("planner messages=%+v structured=%v", m, structured)
			}
			inventory = m[1].Content
			return `{"answer":"Включаю свет в Зале.","actions":[]}`, nil
		})}
		if _, err := c.Handle(t.Context(), text); err != nil {
			t.Fatal(err)
		}
		return inventory
	}
	hall := saw("Включи свет")
	if !strings.Contains(hall, "light.hall") || strings.Contains(hall, "light.kitchen") || strings.Contains(hall, "light.room") {
		t.Fatalf("default room did not narrow inventory: %s", hall)
	}
	kitchen := saw("Включи свет на кухне")
	if !strings.Contains(kitchen, "light.kitchen") || strings.Contains(kitchen, "light.hall") {
		t.Fatalf("explicit room lost: %s", kitchen)
	}
	generic := "Не спрашивай комнату."
	tools := toolFunc(func(_ context.Context, name string, _ map[string]any) (jsontext.Value, error) {
		var v any = map[string]any{"success": true}
		if name == "ha_search" {
			v = map[string]any{"entities": entities}
		}
		if name == "ha_get_entity_exposure" {
			v = map[string]any{"exposed_to": map[string]any{"conversation": true}}
		}
		b, _ := json.Marshal(v)
		return b, nil
	})
	var open string
	c := Controller{Tools: tools, Domains: []string{"light"}, SystemPrompt: generic, Planner: plannerFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
		open = m[1].Content
		return `{"answer":"Какую комнату?","actions":[]}`, nil
	})}
	if _, err := c.Handle(t.Context(), "Включи свет"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(open, "light.hall") || !strings.Contains(open, "light.kitchen") || !strings.Contains(open, "light.room") {
		t.Fatalf("generic room word narrowed inventory: %s", open)
	}
}

func TestHomePlannerReceivesDefaultRoomNote(t *testing.T) {
	note := "Ты находишься в комнате «Зал». Если пользователь не указал комнату, считай, что речь идёт о Зале."
	for _, tc := range []struct {
		name, text string
		structured bool
	}{
		{"command", "Включи свет", true},
		{"state", "Горит ли свет?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &homeFixture{state: "off"}
			c := Controller{Tools: f, Domains: []string{"light"}, SystemPrompt: note, Planner: plannerFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
				if structured != tc.structured || len(m) != 2 || m[0].Role != "system" {
					t.Fatalf("messages=%+v structured=%v", m, structured)
				}
				if !strings.Contains(m[0].Content, "Дополнительные указания оператора") || !strings.Contains(m[0].Content, "комнате «Зал»") {
					t.Fatalf("planner missed room note: %q", m[0].Content)
				}
				if !strings.Contains(m[1].Content, "light.office") {
					t.Fatal("inventory missing")
				}
				if structured {
					return `{"answer":"Включаю свет в Зале.","actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}}]}`, nil
				}
				return "Свет выключен.", nil
			})}
			if _, err := c.Handle(t.Context(), tc.text); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStateQuestionCannotPlanPhysicalActions(t *testing.T) {
	f := &homeFixture{state: "off"}
	c := Controller{Tools: f, Domains: []string{"light"}, Planner: plannerFunc(func(_ context.Context, _ []llm.Message, structured bool) (string, error) {
		if structured {
			t.Fatal("state question used action planner")
		}
		return "Свет выключен.", nil
	})}
	answer, err := c.Handle(t.Context(), "Горит ли свет в кабинете?")
	if err != nil || answer != "Свет выключен." || f.actions != 0 {
		t.Fatalf("answer=%q err=%v actions=%d", answer, err, f.actions)
	}
}
