package llm

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
)

// Jev uses the Decisions endpoint; it is not a chat-completion model.
type Jev struct {
	BaseURL, Model, APIKey string
	HTTP                   *http.Client
}

func (j *Jev) Complete(ctx context.Context, messages []Message, structured bool) (string, error) {
	if !structured || len(messages) < 2 || messages[0].Role != "system" {
		return "", errors.New("Jev supports route classification only")
	}
	request := map[string]any{
		"model": j.Model,
		"state": map[string]any{"conversation": messages[1:]},
		"questions": map[string]any{"route": map[string]any{
			"type": "choice", "instructions": messages[0].Content,
			"criteria": map[string]string{
				"yandex": "Use the Yandex route when required by the routing instructions.",
				"llm":    "Use the language model route when required by the routing instructions.",
				"home":   "Use the Home Assistant route when required by the routing instructions.",
			},
		}},
	}
	c := &Client{BaseURL: j.BaseURL, APIKey: j.APIKey, HTTP: j.HTTP}
	data, err := c.post(ctx, "/alpha/decisions", request)
	if err != nil {
		return "", err
	}
	var result struct {
		Answers struct {
			Route struct {
				Type   string `json:"type"`
				Choice string `json:"choice"`
			} `json:"route"`
		} `json:"answers"`
	}
	if json.Unmarshal(data, &result) != nil || result.Answers.Route.Type != "choice" {
		return "", errors.New("invalid Jev decision")
	}
	route := result.Answers.Route.Choice
	if route != "llm" && route != "yandex" && route != "home" {
		return "", errors.New("invalid Jev route")
	}
	answer, _ := json.Marshal(map[string]string{"route": route})
	return string(answer), nil
}
