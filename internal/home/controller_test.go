package home

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
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

func TestHomeClarificationUsesFreshAvailability(t *testing.T) {
	fixture := &homeFixture{state: "unavailable"}
	history := []llm.Message{
		{Role: "user", Content: "Включи свет в кабинете"},
		{Role: "assistant", Content: "Включить доступный светильник?"},
	}
	c := Controller{Tools: fixture, Planner: plannerFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		if !structured || len(messages) != 4 || !strings.Contains(messages[3].Content, `"state":"unavailable"`) {
			t.Errorf("home clarification did not use current inventory: %+v", messages)
		}
		return `{"answer":"Включаю свет","actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}}]}`, nil
	})}
	if _, err := c.HandleDialogue(t.Context(), "да", history); err == nil || fixture.actions != 0 {
		t.Fatalf("old consent bypassed current availability: err=%v actions=%d", err, fixture.actions)
	}
}

func TestHomeClarificationPrefersTheNewRoom(t *testing.T) {
	entities := []Entity{
		{ID: "light.office", Name: "Свет в кабинете", State: "off", Area: "Кабинет"},
		{ID: "light.bedroom", Name: "Свет в спальне", State: "on", Area: "Спальня"},
	}
	c := Controller{Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
		switch name {
		case "ha_get_skill_guide":
			return json.Marshal(map[string]any{"success": true})
		case "ha_search":
			if args["domain_filter"] == "light" {
				return json.Marshal(map[string]any{"entities": entities, "has_more": false})
			}
			return json.Marshal(map[string]any{"entities": []Entity{}, "has_more": false})
		case "ha_get_entity_exposure":
			if args["entity_id"] != "light.bedroom" {
				t.Error("previous room captured a new explicit room", args)
			}
			return json.Marshal(map[string]any{"exposed_to": map[string]any{"conversation": true}})
		default:
			t.Fatalf("read-only followup called %q", name)
			return nil, nil
		}
	}), Planner: plannerFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		inventory := messages[len(messages)-1].Content
		if strings.Contains(inventory, "light.office") || !strings.Contains(inventory, "light.bedroom") {
			t.Error("new room was not used for inventory", inventory)
		}
		return `{"answer":"В спальне свет включён.","actions":[]}`, nil
	})}
	answer, err := c.HandleDialogue(t.Context(), "а в спальне?", []llm.Message{{Role: "user", Content: "Горит ли свет в кабинете?"}, {Role: "assistant", Content: "В кабинете свет выключен."}})
	if err != nil || answer != "В спальне свет включён." {
		t.Fatal(answer, err)
	}
}

func TestExplicitRoomsOverridePreviousRoom(t *testing.T) {
	entities := []Entity{
		{ID: "light.office", Name: "Свет в кабинете", State: "off", Area: "Кабинет"},
		{ID: "light.bedroom", Name: "Свет в спальне", State: "off", Area: "Спальня"},
	}
	c := Controller{Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
		switch name {
		case "ha_get_skill_guide":
			return json.Marshal(map[string]any{"success": true})
		case "ha_search":
			if args["domain_filter"] == "light" {
				return json.Marshal(map[string]any{"entities": entities, "has_more": false})
			}
			return json.Marshal(map[string]any{"entities": []Entity{}, "has_more": false})
		case "ha_get_entity_exposure":
			return json.Marshal(map[string]any{"exposed_to": map[string]any{"conversation": true}})
		default:
			t.Fatalf("unexpected tool %q", name)
			return nil, nil
		}
	}), Planner: plannerFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
		inventory := messages[len(messages)-1].Content
		if !strings.Contains(inventory, "light.office") || !strings.Contains(inventory, "light.bedroom") {
			t.Errorf("explicit request for both rooms was narrowed by old office context: %s", inventory)
		}
		return `{"answer":"Уточните действие.","actions":[]}`, nil
	})}
	for _, text := range []string{"Включи свет в кабинете и спальне", "Во всей квартире.", "Во всём доме"} {
		_, err := c.HandleDialogue(t.Context(), text, []llm.Message{
			{Role: "user", Content: "Выключи свет в кабинете"},
			{Role: "assistant", Content: "Во всём доме или только в кабинете?"},
		})
		if err != nil {
			t.Fatal(text, err)
		}
	}
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

func TestClimateStartAndTemperaturePlan(t *testing.T) {
	const id = "climate.bedroom"
	on := Action{Domain: "climate", Service: "turn_on", EntityID: id, Data: map[string]any{}}
	temp := Action{Domain: "climate", Service: "set_temperature", EntityID: id, Data: map[string]any{"temperature": 22.0}}
	groupOn := Action{Domain: "climate", Service: "turn_on", EntityID: "climate.group"}
	groupTemp := temp
	groupTemp.EntityID = groupOn.EntityID
	cool := temp
	cool.Data = map[string]any{"temperature": 22.0, "hvac_mode": "cool"}
	off := Action{Domain: "climate", Service: "turn_off", EntityID: id}
	for _, tc := range []struct {
		name  string
		plan  []Action
		allow bool
	}{
		{"start_then_temperature", []Action{on, temp}, true},
		{"explicit_mode_single_service", []Action{cool}, true},
		{"reversed", []Action{temp, on}, false},
		{"duplicate_temperature", []Action{on, temp, temp}, false},
		{"conflicting_power", []Action{on, off}, false},
		{"group_then_member", []Action{groupOn, temp}, false},
		{"same_group", []Action{groupOn, groupTemp}, false},
		{"missing_temperature", []Action{on, {Domain: "climate", Service: "set_temperature", EntityID: id}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []Action
			c := Controller{Domains: []string{"climate"}, Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
				switch name {
				case "ha_get_skill_guide":
					return jsontext.Value(`{"success":true}`), nil
				case "ha_search":
					return json.Marshal(map[string]any{"entities": []Entity{
						{ID: id, Name: "Кондиционер", State: "off", Area: "Спальня"},
						{ID: "climate.group", Name: "Группа", State: "off", Area: "Спальня", Members: []string{id}},
					}})
				case "ha_get_entity_exposure":
					return jsontext.Value(`{"exposed_to":{"conversation":true}}`), nil
				case "ha_call_service":
					calls = append(calls, Action{Domain: args["domain"].(string), Service: args["service"].(string), EntityID: args["entity_id"].(string), Data: args["data"].(map[string]any)})
					return jsontext.Value(`{"success":true}`), nil
				case "ha_get_state":
					return jsontext.Value(`{"data":{"state":"cool"}}`), nil
				default:
					t.Fatalf("unexpected tool %q", name)
					return nil, nil
				}
			}), Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
				raw, err := json.Marshal(Plan{Actions: tc.plan})
				return string(raw), err
			})}
			answer, err := c.Handle(t.Context(), "Включи кондиционер в спальне на 22")
			if tc.allow {
				if err != nil || answer != "Команда отправлена: Кондиционер." || !reflect.DeepEqual(calls, tc.plan) {
					t.Fatalf("answer=%q calls=%+v err=%v", answer, calls, err)
				}
			} else if err == nil || len(calls) != 0 {
				t.Fatalf("invalid plan reached device: calls=%v err=%v", calls, err)
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
