package llm

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// HTTPError keeps provider details separate from its safe runtime error string.
// Body is intended for explicit diagnostics because provider messages can contain
// credentials, request contents or private endpoint URLs.
type HTTPError struct {
	StatusCode int
	Parameter  string
	// Body is available only to explicit diagnostics. Error deliberately omits it.
	Body       string
	compatible bool
}

func (e *HTTPError) Error() string {
	if e.Parameter != "" {
		return fmt.Sprintf("LLM returned HTTP %d (request parameter: %s)", e.StatusCode, e.Parameter)
	}
	return fmt.Sprintf("LLM returned HTTP %d", e.StatusCode)
}

func llmHTTPError(resp *http.Response) error {
	failure := &HTTPError{StatusCode: resp.StatusCode}
	const maxErrorBytes = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes+1))
	failure.Body = string(body)
	if len(body) > maxErrorBytes {
		failure.Body = string(body[:maxErrorBytes]) + "\n[LLM error response exceeded 2097152 bytes; remaining body omitted]"
		return failure
	}
	if err != nil {
		failure.Body += "\n[LLM error response could not be read completely]"
		return failure
	}
	if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnprocessableEntity {
		return failure
	}
	type envelope struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
			Code    any    `json:"code"`
			Status  string `json:"status"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	var details []envelope
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		_ = json.Unmarshal(body, &details)
	} else {
		var detail envelope
		_ = json.Unmarshal(body, &detail)
		details = []envelope{detail}
	}
	// Unknown/bare 400 responses still permit the bounded fallback sequence.
	// An explicit non-parameter failure takes precedence over any field names.
	for _, detail := range details {
		text := strings.ToLower(detail.Error.Message + " " + fmt.Sprint(detail.Error.Code) + " " + detail.Error.Status + " " + detail.Error.Type)
		for _, reason := range []string{
			"failed_precondition", "api key", "api_key", "apikey", "authentication", "unauthorized",
			"permission", "billing", "quota", "rate limit", "rate_limit", "location", "region", "resource_exhausted",
			"model_not_found", "unsupported_model", "not_found", "model is not supported", "model not supported", "model does not exist",
		} {
			if strings.Contains(text, reason) {
				return failure
			}
		}
		if detail.Error.Param != "" && !compatibilityParameter(detail.Error.Param) {
			return failure
		}
	}
	for _, detail := range details {
		if detail.Error.Param != "" {
			failure.Parameter = detail.Error.Param
			break
		}
		first := len(detail.Error.Message)
		for _, name := range []string{"chat_template_kwargs", "temperature", "response_format", "max_completion_tokens", "max_tokens", "reasoning_effort"} {
			if at := strings.Index(detail.Error.Message, name); at >= 0 && at < first {
				failure.Parameter = name
				first = at
			}
		}
		if failure.Parameter != "" {
			break
		}
	}
	failure.compatible = true
	return failure
}

func compatibilityParameter(name string) bool {
	switch name {
	case "chat_template_kwargs", "temperature", "response_format", "max_tokens", "max_completion_tokens", "reasoning_effort":
		return true
	default:
		return false
	}
}

// ProbeCompatibility tries a bounded set of Chat Completions parameter profiles.
// It never changes the live client: only a profile that produces both a plain
// answer and a JSON object is returned for the installer to persist explicitly.
func (c *Client) ProbeCompatibility(ctx context.Context, report func(error)) (map[string]any, error) {
	if c.API != "" && c.API != "chat_completions" {
		return nil, errors.New("LLM compatibility probing requires chat_completions API")
	}
	if err := ValidateChatParameters(c.ChatParameters); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	probe := *c
	probe.ChatParameters = maps.Clone(c.ChatParameters)
	if probe.ChatParameters == nil {
		probe.ChatParameters = make(map[string]any)
	}
	var last error
	for range 10 {
		last = probe.checkCompatibility(ctx)
		if last == nil {
			return probe.ChatParameters, nil
		}
		if report != nil {
			report(last)
		}
		failure, ok := errors.AsType[*HTTPError](last)
		if !ok || !failure.compatible || ctx.Err() != nil {
			return nil, last
		}
		if !nextCompatibilityParameters(probe.ChatParameters, failure.Parameter) {
			break
		}
	}
	return nil, fmt.Errorf("no compatible LLM parameter configuration found: %w", last)
}

func (c *Client) checkCompatibility(ctx context.Context) error {
	if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "Reply with one word: ok"}}, false); err != nil {
		return err
	}
	answer, err := c.Complete(ctx, []Message{{Role: "user", Content: `Reply only with this JSON object: {"ok":true}`}}, true)
	if err != nil {
		return err
	}
	var object map[string]any
	if json.Unmarshal([]byte(answer), &object) != nil || object == nil {
		return errors.New("LLM compatibility check did not return a JSON object")
	}
	return nil
}

func nextCompatibilityParameters(parameters map[string]any, rejected string) bool {
	// A named rejected parameter is handled first, retaining other defaults.
	if compatibilityParameter(rejected) && omitCompatibilityParameter(parameters, rejected) {
		return true
	}
	for _, name := range []string{"chat_template_kwargs", "temperature", "response_format", "reasoning_effort", "max_tokens", "max_completion_tokens"} {
		if omitCompatibilityParameter(parameters, name) {
			return true
		}
	}
	return false
}

func omitCompatibilityParameter(parameters map[string]any, name string) bool {
	value, set := parameters[name]
	if (set && value == nil) || (!set && (name == "reasoning_effort" || name == "max_completion_tokens")) {
		return false
	}
	parameters[name] = nil
	if name == "max_tokens" {
		if _, configured := parameters["max_completion_tokens"]; !configured {
			if !set {
				value = 1024
			}
			parameters["max_completion_tokens"] = value
		}
	}
	return true
}
