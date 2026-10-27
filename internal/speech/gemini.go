package speech

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const maxAudioBytes = 16 << 20

type Gemini struct {
	APIKey string
	Model  string
	Voice  string
	Style  string
	Client *http.Client
	FFmpeg string
}

type responseBody struct {
	Steps []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Data string `json:"data"`
		} `json:"content"`
	} `json:"steps"`
}

func (g *Gemini) Synthesize(ctx context.Context, text string) ([]byte, error) {
	if strings.TrimSpace(g.APIKey) == "" {
		return nil, errors.New("Gemini TTS API key is missing")
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 16000 {
		return nil, errors.New("speech text is empty or too long")
	}
	model, voice, style := g.Model, g.Voice, g.Style
	if model == "" {
		model = "gemini-3.8-flash-lite-tts"
	}
	if voice == "" {
		voice = "algenib"
	}
	if style == "" {
		style = "Speak Russian naturally and clearly with conversational pacing."
	}
	body, err := json.Marshal(map[string]any{
		"model": model,
		"input": []any{map[string]any{"type": "user_input", "content": []any{map[string]any{
			"type": "text", "text": text,
			"annotations": []any{map[string]string{"type": "speech_metadata", "style": style}},
		}}}},
		"response_format":   map[string]string{"type": "audio"},
		"generation_config": map[string]any{"speech_config": []any{map[string]string{"voice": voice}}},
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/interactions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", g.APIKey)
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 75 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Gemini TTS request: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read Gemini TTS response: %w", err)
	}
	if len(raw) > 32<<20 {
		return nil, errors.New("Gemini TTS response exceeded size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Gemini TTS returned HTTP %d", response.StatusCode)
	}
	var parsed responseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode Gemini TTS response: %w", err)
	}
	var wav []byte
	for i := len(parsed.Steps) - 1; i >= 0 && wav == nil; i-- {
		if parsed.Steps[i].Type != "model_output" {
			continue
		}
		for j := len(parsed.Steps[i].Content) - 1; j >= 0; j-- {
			if parsed.Steps[i].Content[j].Type == "audio" {
				wav, err = base64.StdEncoding.DecodeString(parsed.Steps[i].Content[j].Data)
				break
			}
		}
	}
	if err != nil || len(wav) == 0 {
		return nil, errors.New("Gemini TTS response contains no audio")
	}
	if len(wav) > maxAudioBytes {
		return nil, errors.New("Gemini TTS audio exceeded size limit")
	}
	return g.toOggOpus(ctx, wav)
}

func (g *Gemini) toOggOpus(ctx context.Context, wav []byte) ([]byte, error) {
	ffmpeg := g.FFmpeg
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	path, err := exec.LookPath(ffmpeg)
	if err != nil {
		return nil, errors.New("ffmpeg is required for Gemini voice output")
	}
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-f", "wav", "-i", "pipe:0", "-vn", "-ac", "1", "-ar", "48000", "-c:a", "libopus", "-b:a", "32k", "-application", "voip", "-f", "ogg", "pipe:1")
	cmd.Stdin = bytes.NewReader(wav)
	var ogg, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &ogg, max: maxAudioBytes}
	cmd.Stderr = &limitedWriter{w: &stderr, max: 4096}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("encode Gemini audio: %s", strings.TrimSpace(stderr.String()))
		}
		return nil, errors.New("encode Gemini audio with ffmpeg")
	}
	if ogg.Len() == 0 || !bytes.HasPrefix(ogg.Bytes(), []byte("OggS")) {
		return nil, errors.New("ffmpeg returned invalid Ogg audio")
	}
	return ogg.Bytes(), nil
}

type limitedWriter struct {
	w      io.Writer
	max, n int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.max-w.n {
		return 0, errors.New("output exceeded size limit")
	}
	n, err := w.w.Write(p)
	w.n += n
	return n, err
}
