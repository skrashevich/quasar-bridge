package home

import (
	"context"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/skrashevich/quasar-bridge/internal/llm"
)

func sampleDirectEntities() []Entity {
	return []Entity{
		{ID: "climate.dhw", Name: "Горячая вода", Area: "Кухня", State: "heat", Aliases: []string{"Колонка", "Газовая колонка"}},
		{ID: "climate.heating", Name: "Отопление", Area: "Кухня", State: "heat", Aliases: []string{"Котёл", "Котел"}},
		{ID: "climate.bedroom_ac", Name: "Кондиционер в спальне", Area: "Спальня", State: "off"},
		{ID: "light.kitchen_strip", Name: "Подсветка над столом", Area: "Кухня", State: "off"},
		{ID: "light.hall_lamp", Name: "Лампа", Area: "Прихожая", State: "off"},
		{ID: "light.bedroom_lamp", Name: "Лампа", Area: "Спальня", State: "off"},
	}
}

func TestDirectOnOffAndSetpointMatching(t *testing.T) {
	c := &Controller{Direct: true}
	c.setNameIndex(sampleDirectEntities())

	// Unique friendly name with Russian case inflection hits fast-path.
	plan, e, ok := c.directOnOffPlan("Алиса, включи подсветку над столом")
	if !ok || e.ID != "light.kitchen_strip" || len(plan.Actions) != 1 || plan.Actions[0].Service != "turn_on" {
		t.Fatalf("expected direct on/off hit, got ok=%v e=%+v plan=%+v", ok, e, plan)
	}

	// Duplicate names must never hit direct fast-path.
	if _, _, ok := c.directOnOffPlan("включи лампу"); ok {
		t.Fatal("ambiguous duplicate device name must fall back to planner")
	}

	for _, tc := range []struct {
		text   string
		wantID string
		wantT  float64
	}{
		{"сделай колонку 41", "climate.dhw", 41},
		{"поставь колонку на 42 градуса", "climate.dhw", 42},
		{"Алиса, сделай температуру в колонке 39", "climate.dhw", 39},
		{"установи горячую воду 40", "climate.dhw", 40},
		{"сделай котёл 22", "climate.heating", 22},
		{"поставь отопление на 21,5", "climate.heating", 21.5},
	} {
		plan, e, ok := c.directSetpointPlan(tc.text)
		if !ok || e.ID != tc.wantID || len(plan.Actions) != 1 || plan.Actions[0].Service != "set_temperature" || plan.Actions[0].Data["temperature"] != tc.wantT {
			t.Fatalf("%q: got ok=%v entity=%s actions=%+v", tc.text, ok, e.ID, plan.Actions)
		}
	}

	// Relative adjustments and powered-off climate devices stay with the LLM planner.
	if _, _, ok := c.directSetpointPlan("сделай колонку теплее на 2 градуса"); ok {
		t.Fatal("relative temperature adjustment must not use direct setpoint")
	}
	if _, _, ok := c.directSetpointPlan("поставь кондиционер в спальне на 22"); ok {
		t.Fatal("powered-off climate entity must stay on LLM path for turn_on + set_temperature")
	}
}

func TestDirectSetpointExecutesWithoutLLMAndConfirmsTargetTemperature(t *testing.T) {
	oldInterval := setpointPollInterval
	setpointPollInterval = time.Millisecond
	t.Cleanup(func() { setpointPollInterval = oldInterval })

	var exposureChecks, stateReads int
	var gotWait any
	c := &Controller{
		Direct: true,
		Tools: toolFunc(func(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
			switch name {
			case "ha_get_skill_guide":
				return jsontext.Value(`{"success":true}`), nil
			case "ha_get_entity_exposure":
				exposureChecks++
				return jsontext.Value(`{"exposed_to":{"conversation":true}}`), nil
			case "ha_call_service":
				gotWait = args["wait"]
				return jsontext.Value(`{"success":true}`), nil
			case "ha_get_state":
				stateReads++
				if stateReads == 1 {
					return jsontext.Value(`{"data":{"state":"heat","attributes":{"temperature":38}}}`), nil
				}
				return jsontext.Value(`{"data":{"state":"heat","attributes":{"temperature":41}}}`), nil
			default:
				t.Fatalf("unexpected tool %q", name)
				return nil, nil
			}
		}),
		Planner: plannerFunc(func(context.Context, []llm.Message, bool) (string, error) {
			t.Fatal("direct setpoint must not invoke LLM planner")
			return "", nil
		}),
	}
	c.setNameIndex(sampleDirectEntities())

	answer, err := c.Handle(t.Context(), "сделай колонку 41")
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
	if answer != "Готово: Горячая вода." {
		t.Fatalf("answer=%q, want %q", answer, "Готово: Горячая вода.")
	}
	if exposureChecks != 1 {
		t.Fatalf("expected fresh Assist exposure check before execution, got %d", exposureChecks)
	}
	if gotWait != false {
		t.Fatalf("expected wait=false for climate.set_temperature, got %#v", gotWait)
	}
	if stateReads != 2 {
		t.Fatalf("expected setpoint confirmation poll to read updated temperature, got %d reads", stateReads)
	}
}
