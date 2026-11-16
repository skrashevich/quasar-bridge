package llm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
)

// ErrChatGPTQuota identifies an explicit exhausted subscription quota. Network,
// authentication and generic rate-limit failures must not spend fallback credit.
var ErrChatGPTQuota = errors.New("ChatGPT usage limit reached; review https://chatgpt.com/settings/usage")

// QuotaFallback retries only the failed inference round, preserving completed
// tool results and the original request deadline and cancellation context.
type QuotaFallback struct {
	Primary, Secondary *Client
}

func (f *QuotaFallback) Complete(ctx context.Context, messages []Message, structured bool) (string, error) {
	m, err := f.chat(ctx, messages, structured, nil)
	if err != nil {
		return "", err
	}
	if len(m.ToolCalls) > 0 {
		return "", errors.New("unexpected tool call in plain completion")
	}
	return strings.TrimSpace(m.Content), nil
}

func (f *QuotaFallback) Chat(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	return f.chat(ctx, messages, false, tools)
}

func (f *QuotaFallback) chat(ctx context.Context, messages []Message, structured bool, tools []Tool) (Message, error) {
	m, err := f.Primary.chat(ctx, messages, structured, tools)
	if !errors.Is(err, ErrChatGPTQuota) || ctx.Err() != nil || f.Secondary == nil {
		return m, err
	}
	slog.Info("llm_quota_fallback", "provider", "openrouter", "model", f.Secondary.Model)
	// Foreign Responses items are normalized at the provider boundary.
	// Already executed tools are supplied as results; credentials stay separate.
	return f.Secondary.chat(ctx, messages, structured, tools)
}
