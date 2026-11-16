package main

import (
	"errors"
	"os"
	"strings"

	"quasarbridge/internal/llm"
)

type openRouterConfig struct {
	Enabled         bool   `json:"enabled"`
	ClassifierModel string `json:"classifier_model"`
	FallbackModel   string `json:"fallback_model"`
}

func (c openRouterConfig) validate(api string) error {
	if !c.Enabled {
		return nil
	}
	if api != "chatgpt" {
		return errors.New("openrouter fallback requires primary chatgpt API")
	}
	if c.ClassifierModel != "~typesafe/jev-latest" {
		return errors.New("openrouter classifier_model must be ~typesafe/jev-latest")
	}
	if strings.TrimSpace(c.FallbackModel) == "" {
		return errors.New("openrouter fallback_model is required")
	}
	return nil
}

func openRouterClients(cfg config) (*llm.Jev, *llm.Client, error) {
	if !cfg.OpenRouter.Enabled {
		return nil, nil, nil
	}
	key := strings.TrimSpace(os.Getenv("QUASAR_OPENROUTER_API_KEY"))
	if key == "" {
		return nil, nil, errors.New("QUASAR_OPENROUTER_API_KEY is required")
	}
	classifier := &llm.Jev{BaseURL: "https://openrouter.ai/api", Model: cfg.OpenRouter.ClassifierModel, APIKey: key}
	fallback := &llm.Client{
		BaseURL: "https://openrouter.ai/api/v1", API: "openrouter", Model: cfg.OpenRouter.FallbackModel, APIKey: key,
		ReasoningEffort: cfg.LLMReasoningEffort, MaxOutputTokens: cfg.LLMMaxOutputTokens, HostedTools: cfg.LLMHostedTools,
	}
	return classifier, fallback, nil
}
