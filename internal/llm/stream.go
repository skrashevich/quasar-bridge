package llm

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxStreamBytes = 2 << 20

type deltaConsumerKey struct{}

// WithTextDeltas attaches a request-scoped consumer for incremental output.
// Only message text deltas are delivered, never tool arguments or reasoning.
func WithTextDeltas(ctx context.Context, consumer func(string) error) context.Context {
	return context.WithValue(ctx, deltaConsumerKey{}, consumer)
}

// readResponseStream releases output only after the terminal response. Some
// providers omit its output array, so retain completed items, never deltas.
func readResponseStream(body io.Reader) ([]byte, error) {
	return readResponseStreamWithDeltas(body, nil)
}

func readResponseStreamWithDeltas(body io.Reader, onDelta func(string) error) ([]byte, error) {
	limited := &io.LimitedReader{R: body, N: maxStreamBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxStreamBytes+1)
	var data strings.Builder
	items := make(map[int]jsontext.Value)
	first := true
	consume := func() ([]byte, error) {
		if data.Len() == 0 {
			return nil, nil
		}
		payload := data.String()
		data.Reset()
		if payload == "[DONE]" {
			return nil, errors.New("LLM response stream not completed")
		}
		var event struct {
			Type        string         `json:"type"`
			Delta       string         `json:"delta"`
			Response    jsontext.Value `json:"response"`
			OutputIndex *int           `json:"output_index"`
			Item        jsontext.Value `json:"item"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil || event.Type == "" {
			return nil, errors.New("invalid LLM response stream")
		}
		switch event.Type {
		case "response.output_text.delta":
			if onDelta != nil && event.Delta != "" {
				if err := onDelta(event.Delta); err != nil {
					return nil, err
				}
			}
			return nil, nil
		case "response.output_item.done":
			var item struct {
				Type string `json:"type"`
			}
			if event.OutputIndex == nil || *event.OutputIndex < 0 || json.Unmarshal(event.Item, &item) != nil || item.Type == "" {
				return nil, errors.New("invalid LLM response stream")
			}
			if _, duplicate := items[*event.OutputIndex]; duplicate {
				return nil, errors.New("invalid LLM response stream")
			}
			items[*event.OutputIndex] = event.Item
			return nil, nil
		case "response.completed":
			if len(event.Response) == 0 || string(event.Response) == "null" {
				return nil, errors.New("invalid LLM response stream")
			}
			if len(items) > 0 {
				var response map[string]jsontext.Value
				var output []jsontext.Value
				if json.Unmarshal(event.Response, &response) != nil {
					return nil, errors.New("invalid LLM response stream")
				}
				if raw, ok := response["output"]; ok && json.Unmarshal(raw, &output) != nil {
					return nil, errors.New("invalid LLM response stream")
				}
				if len(output) == 0 {
					// Allocate by item count, never by an untrusted output index.
					output = make([]jsontext.Value, len(items))
					for index, item := range items {
						if index >= len(output) {
							return nil, errors.New("invalid LLM response stream")
						}
						output[index] = item
					}
					var err error
					response["output"], err = json.Marshal(output)
					if err != nil {
						return nil, errors.New("invalid LLM response stream")
					}
					return json.Marshal(response)
				}
			}
			return event.Response, nil
		case "response.failed", "error":
			if err := chatGPTErrorCode([]byte(payload)); err != nil {
				return nil, err
			}
			return nil, errors.New("LLM response failed")
		case "response.incomplete":
			return nil, errors.New("LLM response incomplete")
		default:
			return nil, nil
		}
	}
	for scanner.Scan() {
		if limited.N <= 0 {
			return nil, errors.New("LLM response exceeded size limit")
		}
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\xef\xbb\xbf")
			first = false
		}
		if line == "" {
			if response, err := consume(); response != nil || err != nil {
				return response, err
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		if data.Len() > 0 {
			data.WriteByte('\n')
		}
		data.WriteString(strings.TrimPrefix(value, " "))
	}
	if limited.N <= 0 {
		return nil, errors.New("LLM response exceeded size limit")
	}
	if scanner.Err() != nil {
		return nil, errors.New("cannot read LLM response stream")
	}
	if response, err := consume(); response != nil || err != nil {
		return response, err
	}
	return nil, errors.New("LLM response stream not completed")
}

// Only documented error codes are surfaced; upstream messages may contain
// request data or account details and are deliberately discarded.
func chatGPTErrorCode(data []byte) error {
	type codedError struct {
		Code string `json:"code"`
	}
	var envelope struct {
		Code     string     `json:"code"`
		Error    codedError `json:"error"`
		Response struct {
			Error codedError `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return nil
	}
	code := envelope.Error.Code
	if code == "" {
		code = envelope.Response.Error.Code
	}
	if code == "" {
		code = envelope.Code
	}
	switch code {
	case "subscription_sharing_usage_limit_exceeded":
		return errors.New("ChatGPT usage limit reached; review https://chatgpt.com/settings/usage")
	case "subscription_sharing_user_not_eligible":
		return errors.New("ChatGPT plan usage is unavailable for this account or workspace")
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		return errors.New("ChatGPT usage is temporarily unavailable; retry later")
	case "subscription_sharing_unsupported_capability":
		return errors.New("ChatGPT rejected an unsupported model, input or tool")
	case "subscription_sharing_route_not_supported":
		return errors.New("ChatGPT plan usage does not support this API route")
	case "subscription_sharing_invalid_user":
		return errors.New("ChatGPT authentication rejected; sign in again")
	case "chatpass_v2_scope_not_authorized", "chatpass_v2_invalid_authorization_context":
		return errors.New("ChatGPT plan permission does not authorize this operation")
	default:
		return nil
	}
}

func chatGPTHTTPError(response *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err == nil {
		if coded := chatGPTErrorCode(data); coded != nil {
			return coded
		}
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return errors.New("ChatGPT authentication rejected; sign in again")
	case http.StatusForbidden:
		return errors.New("ChatGPT plan access denied")
	case http.StatusTooManyRequests:
		return errors.New("ChatGPT usage limit reached; review https://chatgpt.com/settings/usage")
	default:
		return fmt.Errorf("ChatGPT returned HTTP %d", response.StatusCode)
	}
}
