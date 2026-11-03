// Package voice streams speech from an OpenAI-compatible /audio/speech service.
// The speaker has no Opus encoder, so the service must return Ogg/Opus itself.
package voice

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Config is the "tts" block of the bridge configuration.
type Config struct {
	Enabled       bool   `json:"enabled"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	Voice         string `json:"voice"`
	TimeoutMS     int    `json:"timeout_ms"`
	RevoiceYandex bool   `json:"revoice_yandex"`
}

func DefaultConfig() Config { return Config{TimeoutMS: 15000} }

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("tts.base_url must be an HTTP or HTTPS URL")
	}
	if c.Voice == "" {
		return errors.New("tts.voice is required")
	}
	if c.TimeoutMS < 500 || c.TimeoutMS > 60000 {
		return errors.New("tts.timeout_ms must be between 500 and 60000")
	}
	return nil
}

type Client struct {
	endpoint, model, voice, key string
	http                        *http.Client
}

func New(c Config, key string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = time.Duration(c.TimeoutMS) * time.Millisecond
	return &Client{endpoint: strings.TrimRight(c.BaseURL, "/") + "/audio/speech", model: c.Model, voice: c.Voice, key: key, http: &http.Client{Transport: transport}}
}

// Open starts synthesis. The caller reads Ogg/Opus until EOF and closes the body.
func (c *Client) Open(ctx context.Context, text string) (io.ReadCloser, error) {
	payload, err := json.Marshal(map[string]any{"model": c.model, "voice": c.voice, "input": text, "response_format": "opus"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Transport errors can include the service URL; callers only log the class.
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return nil, errors.New("speech service timed out")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("speech service is unreachable")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("speech service returned HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

var (
	speakerTag = regexp.MustCompile(`<speaker[^>]*>`)
	pause      = regexp.MustCompile(`sil\s*<\[\s*\d+\s*\]>`)
	markup     = regexp.MustCompile(`<\[[^\]]*\]>`)
	stress     = regexp.MustCompile(`\+([а-яёА-ЯЁ])`)
	spaces     = regexp.MustCompile(`\s+`)
)

// Clean removes Yandex speech markup, which another engine would read aloud.
func Clean(text string) string {
	text = speakerTag.ReplaceAllString(text, " ")
	text = pause.ReplaceAllString(text, " ")
	text = markup.ReplaceAllString(text, " ")
	text = stress.ReplaceAllString(text, "$1")
	return strings.TrimSpace(spaces.ReplaceAllString(text, " "))
}
