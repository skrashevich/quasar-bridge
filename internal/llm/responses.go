package llm

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
)

type withoutHostedToolsKey struct{}

// WithoutHostedTools disables server-side tools for a single completion. It keeps
// the assistant's history privacy barrier and final-round limit effective.
func WithoutHostedTools(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutHostedToolsKey{}, true)
}

func (c *Client) responses(ctx context.Context, messages []Message, structured bool, tools []Tool) (Message, error) {
	subscription := c.API == "chatgpt"
	if subscription {
		for _, tool := range c.HostedTools {
			if tool != "web_search" {
				return Message{}, errors.New("ChatGPT supports only the web_search hosted tool")
			}
		}
	}
	input := make([]any, 0, len(messages))
	for _, message := range messages {
		if len(message.responseOutput) > 0 {
			for _, item := range message.responseOutput {
				input = append(input, item)
			}
			continue
		}
		if message.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content})
			continue
		}
		if message.Content != "" || len(message.ToolCalls) == 0 {
			if subscription && message.Role == "system" {
				message.Role = "developer"
			}
			input = append(input, map[string]any{"role": message.Role, "content": message.Content})
		}
		for _, call := range message.ToolCalls {
			item := map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}
			if subscription {
				item["namespace"] = "quasar"
			}
			input = append(input, item)
		}
	}
	budget := c.MaxOutputTokens
	if budget == 0 {
		budget = 4096
	}
	if budget < 0 {
		return Message{}, errors.New("invalid LLM output token budget")
	}
	request := map[string]any{
		"model": c.Model, "input": input, "stream": false,
		"store": false, "max_output_tokens": budget,
		"include": []string{"reasoning.encrypted_content"},
	}
	if c.ReasoningEffort != "" {
		request["reasoning"] = map[string]string{"effort": c.ReasoningEffort}
	}
	if subscription {
		request["stream"] = true
		delete(request, "max_output_tokens")
	}
	if structured && !subscription {
		request["text"] = map[string]any{"format": map[string]string{"type": "json_object"}}
	}
	definitions := make([]any, 0, len(tools)+len(c.HostedTools))
	for _, tool := range tools {
		if tool.Type != "function" {
			return Message{}, errors.New("unsupported LLM function tool")
		}
		definitions = append(definitions, map[string]any{
			"type": "function", "name": tool.Function.Name,
			"description": tool.Function.Description, "parameters": tool.Function.Parameters,
			"strict": false,
		})
	}
	if subscription && len(definitions) > 0 {
		definitions = []any{map[string]any{
			"type": "namespace", "name": "quasar",
			"description": "Quasar Bridge assistant tools", "tools": definitions,
		}}
	}
	if !structured && ctx.Value(withoutHostedToolsKey{}) != true {
		for _, tool := range c.HostedTools {
			switch tool {
			case "web_search":
				definitions = append(definitions, map[string]any{"type": tool})
			case "code_interpreter":
				definitions = append(definitions, map[string]any{"type": tool, "container": map[string]string{"type": "auto"}})
			default:
				return Message{}, errors.New("unsupported LLM hosted tool")
			}
		}
	}
	if len(definitions) > 0 {
		request["tools"] = definitions
		request["tool_choice"] = "auto"
	}
	if len(tools) > 0 {
		request["parallel_tool_calls"] = false
	}
	data, err := c.post(ctx, "/responses", request)
	if err != nil {
		return Message{}, err
	}
	return parseResponse(data)
}

func parseResponse(data []byte) (Message, error) {
	var response struct {
		Status            string           `json:"status"`
		Output            []jsontext.Value `json:"output"`
		Error             jsontext.Value   `json:"error"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return Message{}, errors.New("invalid LLM response")
	}
	if response.Status == "failed" || (len(response.Error) > 0 && string(response.Error) != "null") {
		return Message{}, errors.New("LLM response failed")
	}
	if response.Status == "incomplete" {
		if response.IncompleteDetails.Reason == "max_output_tokens" {
			return Message{}, errors.New("LLM answer exceeded token budget")
		}
		return Message{}, errors.New("LLM response incomplete")
	}
	if response.Status != "completed" {
		return Message{}, errors.New("LLM response not completed")
	}
	message := Message{Role: "assistant", responseOutput: response.Output}
	var text strings.Builder
	for _, raw := range response.Output {
		var item struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			Status    string `json:"status"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.Type == "" {
			return Message{}, errors.New("invalid LLM response")
		}
		switch item.Type {
		case "message":
			if item.Status != "" && item.Status != "completed" {
				return Message{}, errors.New("LLM message incomplete")
			}
			if item.Role != "assistant" {
				continue
			}
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					return Message{}, errors.New("LLM refused the request")
				}
			}
		case "function_call":
			if item.Status != "" && item.Status != "completed" {
				return Message{}, errors.New("LLM function call incomplete")
			}
			if item.CallID == "" || item.Name == "" || !jsontext.Value(item.Arguments).IsValid() {
				return Message{}, errors.New("invalid LLM function call")
			}
			message.ToolCalls = append(message.ToolCalls, ToolCall{ID: item.CallID, Type: "function", Function: FunctionCall{Name: item.Name, Arguments: item.Arguments}})
		}
	}
	message.Content = text.String()
	if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
		return Message{}, errors.New("LLM returned no answer")
	}
	return message, nil
}
