package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"testing"
)

func TestStripPrefixConfigDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		want         []string
	}{
		{"omitted", `{}`, []string{"спроси у нейросети", "спроси нейросеть", "спроси у своей модели", "спроси свою модель", "спроси модель"}},
		{"disabled", `{"llm_strip_prefixes":[]}`, nil},
		{"custom", `{"llm_strip_prefixes":["ответь сам"]}`, []string{"ответь сам"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := readConfig(configFile(t, tc.config))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.LLMStripPrefixes, tc.want) {
				t.Fatalf("prefixes=%q, want %q", cfg.LLMStripPrefixes, tc.want)
			}
		})
	}
}

func TestStripPrefixConfigurationReportsRestartAndPreservesSavedValue(t *testing.T) {
	c := configurationFixture(t, `{}`)
	_, err := c.Get(t.Context(), "llm_strip_prefixes")
	if err != nil {
		t.Fatal(err)
	}
	value, err := c.Set(t.Context(), map[string]jsontext.Value{"llm_strip_prefixes": jsontext.Value(`["ответь сам"]`)})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(configurationResult)
	if !result.RestartRequired || !slices.Contains(result.Pending, "llm_strip_prefixes") {
		t.Fatalf("saved setting not pending: %+v", result)
	}
	if _, err := c.Apply(t.Context(), "reload"); err == nil {
		t.Fatal("non-routing setting was silently hot-reloaded")
	}
	next, err := readConfig(c.path)
	if err != nil || !slices.Equal(next.LLMStripPrefixes, []string{"ответь сам"}) {
		t.Fatalf("saved=%q, err=%v", next.LLMStripPrefixes, err)
	}
	value, err = c.Get(t.Context(), "llm_strip_prefixes")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Settings []struct {
			Active    []string `json:"active"`
			Saved     []string `json:"saved"`
			Effective []string `json:"effective"`
			Pending   bool     `json:"pending"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Settings) != 1 || !state.Settings[0].Pending || !slices.Equal(state.Settings[0].Saved, []string{"ответь сам"}) || !slices.Equal(state.Settings[0].Effective, []string{"ответь сам"}) || slices.Equal(state.Settings[0].Active, state.Settings[0].Saved) {
		t.Fatalf("wrong config state: %s", raw)
	}
}
