package home

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

type inventoryPage struct {
	Entities   []Entity `json:"entities"`
	HasMore    bool     `json:"has_more"`
	Partial    bool     `json:"partial"`
	NextOffset int      `json:"next_offset"`
}

func largeSensorInventory() []Entity {
	entities := make([]Entity, 2785)
	for i := range entities {
		entities[i] = Entity{ID: fmt.Sprintf("sensor.fixture_%04d", i), Name: "Температура спальни", State: "19.5", Area: "Спальня"}
	}
	entities[2401].Name, entities[2401].State, entities[2401].Area = "Температура кабинета", "25.1", "Кабинет"
	entities[2402].Name, entities[2402].State, entities[2402].Area = "Скрытая температура кабинета", "26.2", "Кабинет"
	return entities
}

func sensorInventoryPage(t *testing.T, entities []Entity, args map[string]any) (jsontext.Value, error) {
	t.Helper()
	if args["domain_filter"] != "sensor" || args["include_hidden"] != false || args["limit"] != 200 {
		t.Fatalf("unexpected inventory arguments: %v", args)
	}
	offset := args["offset"].(int)
	if offset < 0 || offset >= len(entities) || offset%200 != 0 {
		t.Fatalf("unexpected inventory offset: %d", offset)
	}
	end := min(offset+200, len(entities))
	return json.Marshal(inventoryPage{Entities: entities[offset:end], HasMore: end < len(entities), NextOffset: end})
}

func TestEntitiesReadsAll2785Sensors(t *testing.T) {
	fixture := largeSensorInventory()
	c := Controller{Domains: []string{"sensor"}, Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
		if name != "ha_search" {
			t.Fatalf("unexpected tool: %s", name)
		}
		return sensorInventoryPage(t, fixture, args)
	})}
	entities, err := c.Entities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 2785 {
		t.Fatalf("inventory contains %d sensors, want 2785", len(entities))
	}
	for i, e := range entities {
		if want := fmt.Sprintf("sensor.fixture_%04d", i); e.ID != want {
			t.Fatalf("entity %d: got %q, want %q", i, e.ID, want)
		}
	}
}

func TestHomeQuestionScopesAndChecksExposureAfterLargeInventory(t *testing.T) {
	fixture := largeSensorInventory()
	var exposureChecks atomic.Int32
	plannerCalls := 0
	c := Controller{Domains: []string{"sensor"}, Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
		switch name {
		case "ha_get_skill_guide":
			return json.Marshal(map[string]any{"success": true, "content": "fixture"})
		case "ha_search":
			return sensorInventoryPage(t, fixture, args)
		case "ha_get_entity_exposure":
			id := args["entity_id"]
			if id != "sensor.fixture_2401" && id != "sensor.fixture_2402" {
				t.Errorf("unrelated room reached exposure lookup: %v", id)
			}
			exposureChecks.Add(1)
			return json.Marshal(map[string]any{"exposed_to": map[string]any{"conversation": id == "sensor.fixture_2401"}})
		default:
			t.Errorf("unexpected tool, including physical action: %s", name)
			return nil, errors.New("unexpected tool")
		}
	}), Planner: plannerFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
		plannerCalls++
		if structured || len(messages) != 2 || exposureChecks.Load() != 2 {
			t.Fatal("state planner called without completed exposure checks")
		}
		data, _, ok := strings.Cut(strings.TrimPrefix(messages[1].Content, "Устройства: "), "\nВопрос: ")
		var entities []Entity
		if !ok || json.Unmarshal([]byte(data), &entities) != nil {
			t.Fatalf("invalid planner inventory: %s", messages[1].Content)
		}
		if len(entities) != 1 || entities[0].ID != "sensor.fixture_2401" || entities[0].State != "25.1" {
			t.Fatalf("planner inventory must contain only the late exposed cabinet sensor: %+v", entities)
		}
		return "В кабинете 25,1 градуса.", nil
	})}
	answer, err := c.Handle(t.Context(), "Какая температура в кабинете?")
	if err != nil || answer != "В кабинете 25,1 градуса." || plannerCalls != 1 {
		t.Fatalf("answer=%q err=%v planner calls=%d", answer, err, plannerCalls)
	}
}

func TestHomeRejectsIncompleteInventoryBeforePlanning(t *testing.T) {
	one := []Entity{{ID: "sensor.office", Name: "Температура кабинета", State: "25.1", Area: "Кабинет"}}
	two := []Entity{{ID: "sensor.other", State: "20"}}
	for _, tc := range []struct {
		name   string
		pages  map[int]inventoryPage
		cancel bool
	}{
		{name: "repeated_entities", pages: map[int]inventoryPage{0: {Entities: one, HasMore: true, NextOffset: 200}, 200: {Entities: one, HasMore: true, NextOffset: 400}, 400: {Entities: two}}},
		{name: "empty_page", pages: map[int]inventoryPage{0: {Entities: one, HasMore: true, NextOffset: 200}, 200: {HasMore: true, NextOffset: 400}, 400: {Entities: two}}},
		{name: "nonadvancing_offset", pages: map[int]inventoryPage{0: {Entities: one, HasMore: true, NextOffset: 0}}},
		{name: "partial_first_page", pages: map[int]inventoryPage{0: {Entities: one, Partial: true}}},
		{name: "partial_later_page", pages: map[int]inventoryPage{0: {Entities: one, HasMore: true, NextOffset: 200}, 200: {Entities: two, Partial: true}}},
		{name: "canceled_caller_ignored_by_tool", cancel: true, pages: map[int]inventoryPage{0: {Entities: one, HasMore: true, NextOffset: 200}, 200: {Entities: two}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			searches, plannerCalls, actions := 0, 0, 0
			c := Controller{Domains: []string{"sensor"}, Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
				switch name {
				case "ha_get_skill_guide":
					return json.Marshal(map[string]any{"success": true, "content": "fixture"})
				case "ha_search":
					searches++
					page, ok := tc.pages[args["offset"].(int)]
					if !ok {
						return nil, errors.New("unexpected inventory offset")
					}
					if tc.cancel {
						cancel()
					}
					return json.Marshal(page)
				case "ha_get_entity_exposure":
					return json.Marshal(map[string]any{"exposed_to": map[string]any{"conversation": true}})
				case "ha_call_service":
					actions++
					return json.Marshal(map[string]any{"success": true})
				default:
					return nil, errors.New("unexpected tool")
				}
			}), Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
				plannerCalls++
				return "Температура 25,1 градуса.", nil
			})}
			answer, err := c.Handle(ctx, "Какая температура в кабинете?")
			if err == nil || answer != "" || plannerCalls != 0 || actions != 0 {
				t.Fatalf("answer=%q err=%v planner calls=%d actions=%d", answer, err, plannerCalls, actions)
			}
			if tc.cancel && (!errors.Is(err, context.Canceled) || searches != 1) {
				t.Fatalf("canceled inventory: err=%v search calls=%d, want cancellation after first search", err, searches)
			}
		})
	}
}

func TestEntitiesDeduplicatesOverlappingPagesThatMakeProgress(t *testing.T) {
	pages := map[int]inventoryPage{
		0:   {Entities: []Entity{{ID: "sensor.one"}, {ID: "sensor.two"}}, HasMore: true, NextOffset: 200},
		200: {Entities: []Entity{{ID: "sensor.two"}, {ID: "sensor.three"}}, HasMore: true, NextOffset: 400},
		400: {Entities: []Entity{{ID: "sensor.three"}, {ID: "sensor.four"}}},
	}
	c := Controller{Domains: []string{"sensor"}, Tools: toolFunc(func(_ context.Context, _ string, args map[string]any) (jsontext.Value, error) {
		return json.Marshal(pages[args["offset"].(int)])
	})}
	entities, err := c.Entities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 4 || entities[0].ID != "sensor.one" || entities[1].ID != "sensor.two" || entities[2].ID != "sensor.three" || entities[3].ID != "sensor.four" {
		t.Fatalf("deduplicated inventory=%v", entities)
	}
}

func TestHomeAppliesEntityLimitToExposedRoomInventory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exposed int
		plans   int
	}{
		{name: "two_exposed_of_100", exposed: 2, plans: 1},
		{name: "81_exposed_of_100", exposed: 81},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entities := make([]Entity, 100)
			exposed := make(map[string]bool, tc.exposed)
			for i := range entities {
				entities[i] = Entity{ID: fmt.Sprintf("sensor.office_%03d", i), Name: "Температура кабинета", State: "25.1", Area: "Кабинет"}
				if i < tc.exposed {
					exposed[entities[i].ID] = true
				}
			}
			plannerCalls := 0
			c := Controller{Domains: []string{"sensor"}, Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
				switch name {
				case "ha_get_skill_guide":
					return json.Marshal(map[string]any{"success": true, "content": "fixture"})
				case "ha_search":
					return sensorInventoryPage(t, entities, args)
				case "ha_get_entity_exposure":
					return json.Marshal(map[string]any{"exposed_to": map[string]any{"conversation": exposed[args["entity_id"].(string)]}})
				default:
					t.Errorf("unexpected tool, including physical action: %s", name)
					return nil, errors.New("unexpected tool")
				}
			}), Planner: plannerFunc(func(_ context.Context, messages []llm.Message, structured bool) (string, error) {
				plannerCalls++
				if structured || len(messages) != 2 {
					t.Fatal("unexpected state planner input")
				}
				data, _, ok := strings.Cut(strings.TrimPrefix(messages[1].Content, "Устройства: "), "\nВопрос: ")
				var got []Entity
				if !ok || json.Unmarshal([]byte(data), &got) != nil || len(got) != 2 || got[0].ID != "sensor.office_000" || got[1].ID != "sensor.office_001" {
					t.Fatalf("planner inventory must contain only two exposed sensors: %s", messages[1].Content)
				}
				return "В кабинете 25,1 градуса.", nil
			})}
			answer, err := c.Handle(t.Context(), "Какая температура в кабинете?")
			if err != nil || plannerCalls != tc.plans {
				t.Fatalf("answer=%q err=%v planner calls=%d, want %d", answer, err, plannerCalls, tc.plans)
			}
			if tc.plans == 0 && !strings.Contains(answer, "слишком много устройств") {
				t.Fatalf("missing clarification for too many exposed sensors: %q", answer)
			}
			if tc.plans == 1 && answer != "В кабинете 25,1 градуса." {
				t.Fatalf("unexpected answer with only two exposed sensors: %q", answer)
			}
		})
	}
}
