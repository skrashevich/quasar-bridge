package llm

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const maxStreamBytes = 2 << 20

// readResponseStream releases output only after a terminal response.completed.
// Some subscription responses omit final output, so retain completed items
// from output_item.done; never turn unfinished deltas into executable actions.
func readResponseStream(body io.Reader) ([]byte, error) {
	limited := &io.LimitedReader{R: body, N: maxStreamBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxStreamBytes+1)
	var data strings.Builder
	first := true
	items := make(map[int]jsontext.Value)
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
			Response    jsontext.Value `json:"response"`
			OutputIndex *int           `json:"output_index"`
			Item        jsontext.Value `json:"item"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil || event.Type == "" {
			return nil, errors.New("invalid LLM response stream")
		}
		switch event.Type {
		case "response.output_item.done":
			if event.OutputIndex == nil || *event.OutputIndex < 0 || len(event.Item) == 0 || string(event.Item) == "null" {
				return nil, errors.New("invalid LLM response stream item")
			}
			if _, exists := items[*event.OutputIndex]; exists {
				return nil, errors.New("duplicate LLM response stream item")
			}
			items[*event.OutputIndex] = event.Item
			return nil, nil
		case "response.completed":
			if len(event.Response) == 0 || string(event.Response) == "null" {
				return nil, errors.New("invalid LLM response stream")
			}
			return restoreStreamOutput(event.Response, items)
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

// Prefer authoritative terminal output. Restore completed items only when the
// successful terminal response omits them, preserving reasoning and tool context.
func restoreStreamOutput(data []byte, items map[int]jsontext.Value) ([]byte, error) {
	var response struct {
		Status string           `json:"status"`
		Output []jsontext.Value `json:"output"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, errors.New("invalid LLM response stream")
	}
	if response.Status != "completed" || len(response.Output) > 0 || len(items) == 0 {
		return data, nil
	}
	indices := make([]int, 0, len(items))
	for index := range items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	output := make([]jsontext.Value, 0, len(items))
	for expected, index := range indices {
		if index != expected {
			return nil, errors.New("LLM response stream output missing items")
		}
		output = append(output, items[index])
	}
	var envelope map[string]jsontext.Value
	if json.Unmarshal(data, &envelope) != nil {
		return nil, errors.New("invalid LLM response stream")
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, errors.New("invalid LLM response stream output")
	}
	envelope["output"] = encoded
	return json.Marshal(envelope)
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
