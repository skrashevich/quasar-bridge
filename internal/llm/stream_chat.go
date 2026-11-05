package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// This streaming API is available only for tool-free Chat Completions.
// ChatGPT Responses and assistants with tool invocation use the old path.
func (c *Client) SupportsSentenceStreaming() bool {
	return c.API == "" || c.API == "chat_completions"
}

// CompleteStream streams plain Chat Completions text deltas while also
// returning the exact final concatenated answer. It is intentionally limited
// to non-structured, tool-free completions so the regular tool loop keeps its
// existing deterministic semantics.
func (c *Client) CompleteStream(ctx context.Context, messages []Message, structured bool, onDelta func(string) error) (string, error) {
	if structured {
		return "", errors.New("streaming structured completion is unsupported")
	}
	if err := ValidateChatParameters(c.ChatParameters); err != nil {
		return "", err
	}
	if c.API != "" && c.API != "chat_completions" {
		return "", errors.New("streaming completion requires chat_completions API")
	}
	request := map[string]any{
		"model": c.Model, "messages": messages, "stream": true,
		"temperature": 0, "max_tokens": 1024,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	for key, value := range c.ChatParameters {
		if key == "response_format" {
			continue
		}
		if value == nil {
			delete(request, key)
		} else {
			request[key] = value
		}
	}
	return c.chatCompletionsStream(ctx, request, onDelta)
}

func (c *Client) chatCompletionsStream(ctx context.Context, request map[string]any, onDelta func(string) error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body, err := json.Marshal(request)
	if err != nil {
		return "", errors.New("cannot encode LLM request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", errors.New("invalid LLM endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("LLM transport: %w", ctx.Err())
		}
		return "", errors.New("LLM transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM returned HTTP %d", resp.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return "", errors.New("LLM streaming response is not SSE")
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 32*1024), 1<<20)
	var text strings.Builder
	finishReason := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return "", errors.New("invalid LLM stream event")
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				// Reject leaked service markup before it can reach live speech.
				if err := ValidateAnswer(text.String()); err != nil {
					return "", err
				}
				if onDelta != nil {
					if err := onDelta(choice.Delta.Content); err != nil {
						return "", err
					}
				}
			}
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("LLM transport: %w", ctx.Err())
		}
		return "", errors.New("cannot read LLM stream")
	}
	if finishReason == "length" {
		return "", errors.New("LLM answer exceeded token budget")
	}
	answer := strings.TrimSpace(text.String())
	if answer == "" {
		return "", errors.New("LLM returned no answer")
	}
	if err := ValidateAnswer(answer); err != nil {
		return "", err
	}
	return answer, nil
}
