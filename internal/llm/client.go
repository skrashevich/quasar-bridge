package llm

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// Preserve reasoning and other Responses items only for this tool loop.
	// They are deliberately absent from the Chat Completions wire format.
	responseOutput []jsontext.Value
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}
type Function struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
type Client struct {
	BaseURL         string
	Model           string
	APIKey          string
	API             string
	ReasoningEffort string
	MaxOutputTokens int
	HostedTools     []string
	ChatParameters  map[string]any
	HTTP            *http.Client
}

var serviceMarkup = regexp.MustCompile(`(?i)(?:</?[|｜]|<\s*/?\s*(?:[\w.-]+:)?(?:(?:tool_calls?|function_calls?|tool_responses?|tool_results?|think(?:ing)?|analysis|reasoning(?:_content)?)\b|(?:function|parameter|invoke)(?:\s*=|\s+|\s*>|$)))`)

// ValidateAnswer rejects service markup rather than speaking unexecuted calls
// or internal reasoning. Structured planning bypasses this speech check.
func ValidateAnswer(text string) error {
	if serviceMarkup.MatchString(text) {
		return errors.New("LLM returned service markup instead of a final answer")
	}
	return nil
}

// ValidateChatParameters protects the request fields controlled by the bridge.
// Parameter values are validated by the configured API.
func ValidateChatParameters(parameters map[string]any) error {
	for key := range parameters {
		switch key {
		case "temperature", "max_tokens", "max_completion_tokens", "response_format", "chat_template_kwargs", "reasoning_effort":
		default:
			return errors.New("unsupported LLM chat parameter")
		}
	}
	return nil
}

func (c *Client) Complete(ctx context.Context, messages []Message, structured bool) (string, error) {
	m, err := c.chat(ctx, messages, structured, nil)
	if err != nil {
		return "", err
	}
	if len(m.ToolCalls) > 0 {
		return "", errors.New("unexpected tool call in plain completion")
	}
	return strings.TrimSpace(m.Content), nil
}
func (c *Client) Chat(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	return c.chat(ctx, messages, false, tools)
}
func (c *Client) chat(ctx context.Context, messages []Message, structured bool, tools []Tool) (Message, error) {
	if err := ValidateChatParameters(c.ChatParameters); err != nil {
		return Message{}, err
	}
	var m Message
	var err error
	switch c.API {
	case "", "chat_completions":
		m, err = c.chatCompletions(ctx, messages, structured, tools)
	case "responses":
		if len(c.ChatParameters) > 0 {
			return Message{}, errors.New("LLM chat parameters require chat_completions API")
		}
		m, err = c.responses(ctx, messages, structured, tools)
	default:
		return Message{}, errors.New("unsupported LLM API")
	}
	if err != nil {
		return Message{}, err
	}
	if !structured && len(m.ToolCalls) == 0 {
		if err := ValidateAnswer(m.Content); err != nil {
			return Message{}, err
		}
	}
	return m, nil
}

func (c *Client) chatCompletions(ctx context.Context, messages []Message, structured bool, tools []Tool) (Message, error) {
	request := map[string]any{
		"model": c.Model, "messages": messages, "stream": false,
		"temperature": 0, "max_tokens": 1024,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	if structured {
		request["response_format"] = map[string]string{"type": "json_object"}
	}
	for key, value := range c.ChatParameters {
		if key == "response_format" && !structured {
			continue
		}
		if value == nil {
			delete(request, key)
		} else {
			request[key] = value
		}
	}
	if len(tools) > 0 {
		request["tools"] = tools
		request["tool_choice"] = "auto"
		request["parallel_tool_calls"] = false
	}
	data, err := c.post(ctx, "/chat/completions", request)
	if err != nil {
		return Message{}, err
	}
	var result struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return Message{}, errors.New("invalid LLM response")
	}
	if len(result.Choices) == 0 || (strings.TrimSpace(result.Choices[0].Message.Content) == "" && len(result.Choices[0].Message.ToolCalls) == 0) {
		return Message{}, errors.New("LLM returned no answer")
	}
	if result.Choices[0].FinishReason == "length" {
		return Message{}, errors.New("LLM answer exceeded token budget")
	}
	m := result.Choices[0].Message
	m.Role = "assistant"
	return m, nil
}

func (c *Client) post(ctx context.Context, path string, request any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	body, err := json.Marshal(request)
	if err != nil {
		return nil, errors.New("cannot encode LLM request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid LLM endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
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
			return nil, fmt.Errorf("LLM transport: %w", ctx.Err())
		}
		return nil, errors.New("LLM transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM returned HTTP %d", resp.StatusCode)
	}
	const maxResponseBytes = 2 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("LLM transport: %w", ctx.Err())
		}
		return nil, errors.New("cannot read LLM response")
	}
	if len(data) > maxResponseBytes {
		return nil, errors.New("LLM response exceeded size limit")
	}
	return data, nil
}
