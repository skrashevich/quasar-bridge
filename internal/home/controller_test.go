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

func TestHomeRejectsServiceMarkupInPlanAnswer(t *testing.T) {
	c := Controller{Tools: &homeFixture{state: "off"}, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
		return `{"answer":"<think>рассуждения</think>Уточните комнату.","actions":[]}`, nil
	})}
	if answer, err := c.Handle(t.Context(), "Включи свет"); err == nil || answer != "" {
		t.Fatalf("service markup accepted: answer=%q error=%v", answer, err)
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
func TestHomeRejectsDuplicateAndGroupMemberActions(t *testing.T) {
	for _, second := range []string{"light.office", "light.member"} {
		t.Run(second, func(t *testing.T) {
			actions := 0
			tools := toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
				var v any = map[string]any{"success": true}
				switch name {
				case "ha_search":
					v = map[string]any{"entities": []Entity{{ID: "light.office", Name: "Group", State: "off", Members: []string{"light.member"}}, {ID: "light.member", Name: "Lamp", State: "off"}}}
				case "ha_get_entity_exposure":
					v = map[string]any{"exposed_to": map[string]any{"conversation": true}}
				case "ha_get_state":
					m := []string{}
					if args["entity_id"] == "light.office" {
						m = []string{"light.member"}
					}
					v = map[string]any{"data": map[string]any{"state": "off", "attributes": map[string]any{"entity_id": m}}}
				case "ha_call_service":
					actions++
				}
				b, _ := json.Marshal(v)
				return b, nil
			})
			c := Controller{Tools: tools, Domains: []string{"light"}, Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
				b, _ := json.Marshal(Plan{Actions: []Action{{Domain: "light", Service: "turn_on", EntityID: "light.office"}, {Domain: "light", Service: "turn_on", EntityID: second}}})
				return string(b), nil
			})}
			if _, err := c.Handle(t.Context(), "Включи свет"); err == nil || actions != 0 {
				t.Fatalf("err=%v actions=%d", err, actions)
			}
		})
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

// User story: the speaker reads home answers aloud, so a reply must name a
// device the way people call it ("Свет в кабинете"), never by its Home
// Assistant entity_id. The prompts ask for that, and the bridge also replaces
// any entity_id the planner still writes, because the model can ignore a prompt.
func TestHomeAnswerNamesDevicesWithoutEntityIDs(t *testing.T) {
	for _, tc := range []struct {
		name, question, reply, want string
	}{
		{"command_answer_with_id_in_brackets", "Включи свет в кабинете", "Свет в кабинете (light.office) уже включён.", "Свет в кабинете уже включён."},
		{"command_answer_with_bare_id", "Включи свет в кабинете", "light.office уже включён.", "Свет в кабинете уже включён."},
		{"state_answer_with_bare_id", "Горит ли свет в кабинете?", "light.office включён.", "Свет в кабинете включён."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &homeFixture{state: "on"}
			c := Controller{Tools: f, Domains: []string{"light"}, Planner: plannerFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
				if !strings.Contains(m[0].Content, "Называй устройства по friendly_name") {
					t.Fatalf("prompt does not ask for friendly names: %q", m[0].Content)
				}
				if !structured {
					return tc.reply, nil
				}
				b, _ := json.Marshal(Plan{Answer: tc.reply, Actions: []Action{}})
				return string(b), nil
			})}
			answer, err := c.Handle(t.Context(), tc.question)
			if err != nil || answer != tc.want || f.actions != 0 {
				t.Fatalf("answer=%q want=%q err=%v actions=%d", answer, tc.want, err, f.actions)
			}
		})
	}
}

func TestDegreesScopeWaterHeaterNotSpeakers(t *testing.T) {
	for text, want := range map[string][]string{
		"Сделай колонку 40 градусов": {"climate"},
		"Сделай колонку 37":          {"climate"},
		"Сделай колонку погромче":    {"climate", "media_player"},
		"Включи телевизор":           {"media_player"},
	} {
		if got := controlDomains(text); !slices.Equal(got, want) {
			t.Errorf("%q: %v", text, got)
		}
	}
	if _, ok := serviceFields["media_player.turn_on"]; !ok {
		t.Error("TV cannot be switched on")
	}
}

func TestHomeNumericRequestsKeepTheIntendedDeviceInInventory(t *testing.T) {
	for _, tc := range []struct {
		name, request, domain, service string
		data                           map[string]any
		wantSpeaker                    bool
	}{
		{"speaker_percent", "Сделай колонку на 20 процентов", "media_player", "volume_set", map[string]any{"volume_level": 0.2}, true},
		{"speaker_percent_symbol", "Установи громкость колонки на 20%", "media_player", "volume_set", map[string]any{"volume_level": 0.2}, true},
		{"speaker_number_in_name", "Включи колонку Станция 2", "media_player", "turn_on", nil, true},
		{"heater_integer", "Сделай колонку 37", "climate", "set_temperature", map[string]any{"temperature": 37}, false},
		{"heater_decimal_comma", "Установи колонку на 37,5", "climate", "set_temperature", map[string]any{"temperature": 37.5}, false},
		{"heater_explicit_degrees", "Сделай колонку 37,5 градусов", "climate", "set_temperature", map[string]any{"temperature": 37.5}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			speaker := Entity{ID: "media_player.station_2", Name: "Колонка Станция 2", State: "off"}
			heater := Entity{ID: "climate.water_heater", Name: "Газовая колонка", State: "heat"}
			target := heater
			if tc.wantSpeaker {
				target = speaker
			}
			var searched []string
			actions, plannerCalls := 0, 0
			tools := toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
				var value any = map[string]any{"success": true}
				switch name {
				case "ha_get_skill_guide":
				case "ha_search":
					domain, _ := args["domain_filter"].(string)
					searched = append(searched, domain)
					var entities []Entity
					if domain == "media_player" {
						entities = append(entities, speaker)
					}
					if domain == "climate" {
						entities = append(entities, heater)
					}
					value = map[string]any{"entities": entities}
				case "ha_get_entity_exposure":
					value = map[string]any{"exposed_to": map[string]any{"conversation": true}}
				case "ha_call_service":
					actions++
					if args["entity_id"] != target.ID || args["domain"] != tc.domain || args["service"] != tc.service {
						t.Fatalf("wrong service call: %v", args)
					}
					got, err := json.Marshal(args["data"])
					if err != nil {
						t.Fatal(err)
					}
					want, err := json.Marshal(tc.data)
					if err != nil || string(got) != string(want) {
						t.Fatalf("service data=%s want=%s error=%v", got, want, err)
					}
				case "ha_get_state":
					value = map[string]any{"data": map[string]any{"entity_id": target.ID, "state": "on"}}
				default:
					t.Fatalf("unexpected tool: %s", name)
				}
				return json.Marshal(value)
			})
			c := Controller{Tools: tools, Planner: plannerFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				plannerCalls++
				if !structured || len(messages) != 2 {
					t.Fatalf("invalid planning request: structured=%v messages=%v", structured, messages)
				}
				if !strings.Contains(messages[1].Content, target.ID) {
					t.Fatalf("intended target missing from planner inventory: %s", messages[1].Content)
				}
				if strings.Contains(messages[1].Content, speaker.ID) != tc.wantSpeaker {
					t.Fatalf("speaker presence does not match request: %s", messages[1].Content)
				}
				b, err := json.Marshal(Plan{Actions: []Action{{Domain: tc.domain, Service: tc.service, EntityID: target.ID, Data: tc.data}}})
				return string(b), err
			})}
			answer, err := c.Handle(t.Context(), tc.request)
			if err != nil || answer != "Команда отправлена: "+target.Name+"." || actions != 1 || plannerCalls != 1 {
				t.Fatalf("answer=%q error=%v actions=%d planner calls=%d", answer, err, actions, plannerCalls)
			}
			if slices.Contains(searched, "media_player") != tc.wantSpeaker || !slices.Contains(searched, "climate") {
				t.Fatalf("searched domains=%v", searched)
			}
		})
	}
}

func TestHomeFollowupPreservesOriginalRequestAndRoomScope(t *testing.T) {
	const request = "Включи свет в кабинете"
	const question = "Люстру или торшер?"
	const reply = "оба"
	entities := []Entity{
		{ID: "light.office_ceiling", Name: "Люстра", Area: "Кабинет", State: "off"},
		{ID: "light.office_floor", Name: "Торшер", Area: "Кабинет", State: "off"},
		{ID: "light.bedroom", Name: "Свет в спальне", Area: "Спальня", State: "off"},
	}
	var searched, acted []string
	plannerCalls := 0
	tools := toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
		var value any = map[string]any{"success": true}
		switch name {
		case "ha_get_skill_guide":
		case "ha_search":
			domain, _ := args["domain_filter"].(string)
			searched = append(searched, domain)
			if domain == "light" {
				value = map[string]any{"entities": entities}
			} else {
				value = map[string]any{"entities": []Entity{}}
			}
		case "ha_get_entity_exposure":
			value = map[string]any{"exposed_to": map[string]any{"conversation": true}}
		case "ha_call_service":
			id, _ := args["entity_id"].(string)
			if args["domain"] != "light" || args["service"] != "turn_on" || (id != entities[0].ID && id != entities[1].ID) {
				t.Fatalf("wrong followup service call: %v", args)
			}
			acted = append(acted, id)
		case "ha_get_state":
			value = map[string]any{"data": map[string]any{"entity_id": args["entity_id"], "state": "on"}}
		default:
			t.Fatalf("unexpected tool: %s", name)
		}
		return json.Marshal(value)
	})
	c := Controller{Tools: tools, Planner: plannerFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		plannerCalls++
		if !structured || len(messages) != 2 {
			t.Fatalf("invalid followup planning request: structured=%v messages=%v", structured, messages)
		}
		for _, part := range []string{request, question, "Запрос: " + reply, entities[0].ID, entities[1].ID} {
			if !strings.Contains(messages[1].Content, part) {
				t.Fatalf("followup context missing %q: %s", part, messages[1].Content)
			}
		}
		if strings.Contains(messages[1].Content, entities[2].ID) {
			t.Fatalf("followup lost original room scope: %s", messages[1].Content)
		}
		b, err := json.Marshal(Plan{Actions: []Action{
			{Domain: "light", Service: "turn_on", EntityID: entities[0].ID},
			{Domain: "light", Service: "turn_on", EntityID: entities[1].ID},
		}})
		return string(b), err
	})}
	answer, err := c.HandleFollowup(t.Context(), reply, request, question)
	if err != nil || answer != "Готово: Люстра, Торшер." || plannerCalls != 1 {
		t.Fatalf("answer=%q error=%v planner calls=%d", answer, err, plannerCalls)
	}
	if !slices.Equal(searched, []string{"light", "switch"}) || !slices.Equal(acted, []string{entities[0].ID, entities[1].ID}) {
		t.Fatalf("searched domains=%v actions=%v", searched, acted)
	}
}
