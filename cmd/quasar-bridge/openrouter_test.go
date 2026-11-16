package main

import "testing"

func TestOpenRouterConfiguration(t *testing.T) {
	c := openRouterConfig{Enabled: true, ClassifierModel: "~typesafe/jev-latest", FallbackModel: "openai/gpt-6.1-terra"}
	if err := c.validate("chatgpt"); err != nil {
		t.Fatal(err)
	}
	if err := c.validate("responses"); err == nil {
		t.Fatal("accepted non-subscription primary")
	}
	t.Setenv("QUASAR_OPENROUTER_API_KEY", "")
	if _, _, err := openRouterClients(config{OpenRouter: c}); err == nil {
		t.Fatal("missing key accepted")
	}
	t.Setenv("QUASAR_OPENROUTER_API_KEY", "fixture-router-key")
	j, f, err := openRouterClients(config{OpenRouter: c, LLMReasoningEffort: "medium", LLMMaxOutputTokens: 4096, LLMHostedTools: []string{"web_search"}})
	if err != nil {
		t.Fatal(err)
	}
	if j.Model != "~typesafe/jev-latest" || f.API != "openrouter" || f.Model != "openai/gpt-6.1-terra" || f.ReasoningEffort != "medium" || f.AccessToken != nil {
		t.Fatal("wrong provider separation")
	}
}
