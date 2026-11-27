package home

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"unicode/utf8"
)

func sampleCatalogEntities() []Entity {
	return []Entity{
		{ID: "climate.water_heater", Name: "Горячая вода", Area: "Кухня", State: "heat", Aliases: []string{"Колонка", "Бойлер"}},
		{ID: "climate.bedroom_ac", Name: "Кондиционер в спальне", Area: "Спальня", State: "off"},
		{ID: "light.kitchen_strip", Name: "Подсветка над столом", Area: "Кухня", State: "on"},
		{ID: "light.nursery_nightlight", Name: "Ночник", Area: "Детская", State: "off"},
		{ID: "light.office_ambient", Name: "Настольная лампа", Area: "Кабинет", State: "off"},
		{ID: "media_player.living_tv", Name: "Телевизор", Area: "Гостиная", State: "off"},
		{ID: "media_player.living_box", Name: "Приставка", Area: "Гостиная", State: "off"},
		{ID: "switch.aquarium", Name: "Аквариум", Area: "Гостиная", State: "on"},
	}
}

func TestHomeHintDistinguishesSongsFromDevices(t *testing.T) {
	c := &Controller{}
	if known, _, _ := c.HomeHint("включи Батарейку"); known {
		t.Fatal("empty catalog must report known=false")
	}
	c.setCatalog(sampleCatalogEntities())

	songs := []string{
		"включи Батарейку",
		"включи Светлана Лобода",
		"включи Солнце Монако",
		"включи Звезда по имени Солнце",
		"поставь Король и Шут Кукла колдуна",
		"включи Белая ночь",
		"включи Группа крови",
		"включи Ветер перемен",
	}
	for _, s := range songs {
		known, matches, summary := c.HomeHint(s)
		if !known || summary == "" {
			t.Fatalf("%q: expected populated catalog", s)
		}
		if matches {
			t.Errorf("%q: song title falsely matched smart-home device or room", s)
		}
	}

	devices := []string{
		"включи свет",
		"включи подсветку",
		"включи ночник",
		"включи приставку",
		"включи телевизор",
		"включи аквариум",
		"включи ленту в детской",
		"включи кондиционер в спальне",
		"сделай колонку 41",
		"включи батареи",
	}
	for _, d := range devices {
		known, matches, _ := c.HomeHint(d)
		if !known || !matches {
			t.Errorf("%q: real smart-home command missed by HomeHint", d)
		}
	}
}

func TestBuildCatalogSummaryValidUTF8AndJSON(t *testing.T) {
	var many []Entity
	for i := range 80 {
		many = append(many, Entity{
			ID:      fmt.Sprintf("light.dev_%d", i),
			Name:    fmt.Sprintf("Длинное русское название светильника номер %d", i),
			Area:    fmt.Sprintf("Комната %d", i%12),
			Aliases: []string{"Подсветка", "Основной свет"},
		})
	}
	c := &Controller{}
	c.setCatalog(many)
	known, _, summary := c.HomeHint("включи свет")
	if !known || summary == "" {
		t.Fatal("expected non-empty summary")
	}
	if !utf8.ValidString(summary) {
		t.Fatalf("summary contains invalid UTF-8: %q", summary)
	}
	if len([]rune(summary)) > 900 {
		t.Fatalf("summary exceeded rune cap: %d", len([]rune(summary)))
	}
	if _, err := json.Marshal(map[string]string{"content": summary}); err != nil {
		t.Fatalf("encoding/json/v2 failed to marshal summary: %v", err)
	}
}

type stubCatalogCaller struct {
	entities []Entity
}

func (s stubCatalogCaller) Call(_ context.Context, tool string, args map[string]any) (jsontext.Value, error) {
	if tool != "ha_search" {
		return nil, fmt.Errorf("unexpected tool %s", tool)
	}
	domain, _ := args["domain_filter"].(string)
	var out []Entity
	for _, e := range s.entities {
		if len(e.ID) > len(domain) && e.ID[:len(domain)+1] == domain+"." {
			out = append(out, e)
		}
	}
	return json.Marshal(map[string]any{"entities": out})
}

func TestRefreshCatalogPopulatesHomeHint(t *testing.T) {
	c := &Controller{Tools: stubCatalogCaller{entities: sampleCatalogEntities()}}
	if err := c.RefreshCatalog(t.Context()); err != nil {
		t.Fatalf("RefreshCatalog failed: %v", err)
	}
	known, matches, _ := c.HomeHint("включи аквариум")
	if !known || !matches {
		t.Fatalf("expected catalog hit for аквариум: known=%v matches=%v", known, matches)
	}
	known, matches, _ = c.HomeHint("включи Белая ночь")
	if !known || matches {
		t.Fatalf("expected non-home for Белая ночь: known=%v matches=%v", known, matches)
	}
}
