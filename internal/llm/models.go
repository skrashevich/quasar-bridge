package llm

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const (
	// AnthropicBaseURL is the endpoint selected by "claude" in the installer.
	AnthropicBaseURL = "https://api.anthropic.com/v1"
	// anthropicVersion is the oldest API version Anthropic accepts; the
	// endpoint rejects requests without it, including GET /models.
	anthropicVersion = "2023-06-01"
)

// isAnthropic reports whether baseURL is the official Anthropic API. Only the
// exact HTTPS host qualifies, so the x-api-key is never sent anywhere else.
func isAnthropic(baseURL string) bool {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "api.anthropic.com") && u.User == nil
}

// setAuthHeaders adds provider credentials. Anthropic authenticates with
// x-api-key and requires anthropic-version; everything else uses a bearer token.
func setAuthHeaders(req *http.Request, baseURL, token string) {
	if isAnthropic(baseURL) {
		req.Header.Set("anthropic-version", anthropicVersion)
		if token != "" {
			req.Header.Set("x-api-key", token)
		}
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// Models returns the provider's ordered model catalog without running inference.
// ChatGPT returns only models visible to the selected subscription account.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	baseURL, token := strings.TrimRight(c.BaseURL, "/"), c.APIKey
	switch c.API {
	case "", "chat_completions", "responses":
	case "chatgpt":
		if baseURL != "" && baseURL != "https://api.openai.com/v1" {
			return nil, errors.New("ChatGPT requires the official OpenAI endpoint")
		}
		baseURL = "https://api.openai.com/v1"
		if c.AccessToken == nil {
			return nil, errors.New("ChatGPT sign-in is required")
		}
		var err error
		token, err = c.AccessToken(ctx)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ChatGPT sign-in: %w", ctx.Err())
		}
		if err != nil || strings.TrimSpace(token) == "" {
			return nil, errors.New("ChatGPT sign-in unavailable; check credentials or sign in again")
		}
	default:
		return nil, errors.New("unsupported LLM API")
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return nil, errors.New("invalid LLM model catalog endpoint")
	}
	catalogURL := baseURL + "/models"
	if isAnthropic(baseURL) {
		// Anthropic pages the catalog at 20 models by default; 1000 is its maximum.
		catalogURL += "?limit=1000"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, errors.New("invalid LLM model catalog endpoint")
	}
	req.Header.Set("Accept", "application/json")
	setAuthHeaders(req, baseURL, token)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	// Keep API keys and OAuth credentials at the exact configured endpoint,
	// including when a provider redirects within its own domain.
	clone := *hc
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clone.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("LLM model catalog: %w", ctx.Err())
		}
		return nil, errors.New("LLM model catalog transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if c.API == "chatgpt" {
			err := chatGPTHTTPError(resp)
			if ctx.Err() != nil {
				return nil, fmt.Errorf("LLM model catalog: %w", ctx.Err())
			}
			return nil, err
		}
		return nil, fmt.Errorf("LLM model catalog returned HTTP %d", resp.StatusCode)
	}
	const maxCatalogBytes = 2 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if ctx.Err() != nil {
		return nil, fmt.Errorf("LLM model catalog: %w", ctx.Err())
	}
	if err != nil {
		return nil, errors.New("cannot read LLM model catalog")
	}
	if len(data) > maxCatalogBytes {
		return nil, errors.New("LLM model catalog exceeded size limit")
	}
	type entry struct {
		ID          string `json:"id"`
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Visibility  string `json:"visibility"`
	}
	var catalog struct {
		Data   []entry `json:"data"`
		Models []entry `json:"models"`
	}
	if json.Unmarshal(data, &catalog) != nil {
		return nil, errors.New("invalid LLM model catalog")
	}
	entries := catalog.Data
	if c.API == "chatgpt" {
		entries = catalog.Models
	}
	models := make([]Model, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, item := range entries {
		id := item.ID
		if c.API == "chatgpt" {
			if item.Visibility != "list" {
				continue
			}
			id = item.Slug
		}
		if !validModelID(id) {
			return nil, errors.New("LLM model catalog contains an invalid model ID")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, Model{ID: id, Name: cmp.Or(item.Name, item.DisplayName, id)})
	}
	if len(models) == 0 {
		return nil, errors.New("LLM provider returned no available models")
	}
	return models, nil
}

// IDs are also written as the first TSV field by the CLI and consumed by the
// standalone installer. Reject invisible separators instead of altering IDs.
func validModelID(id string) bool {
	if id == "" || len(id) > 512 || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
