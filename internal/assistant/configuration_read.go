package assistant

import (
	"context"
	"errors"
	"strings"

	"quasarbridge/internal/llm"
)

// CompleteConfiguration permits a factual retry to inspect local settings while
// keeping mutations, network tools, MCP discovery and hosted tools unavailable.
func (a *Agent) CompleteConfiguration(ctx context.Context, messages []llm.Message) (string, error) {
	local := *a
	local.Tools = &configurationReadTools{base: a.Tools}
	return local.Complete(llm.WithoutHostedTools(ctx), messages, false)
}

type configurationReadTools struct{ base Toolset }

func configurationReadName(name string) bool {
	return name == "get_quasar_bridge_config" || name == "current_time"
}

func (t *configurationReadTools) Definitions() []llm.Tool {
	var definitions []llm.Tool
	for _, def := range t.base.Definitions() {
		if configurationReadName(def.Function.Name) {
			definitions = append(definitions, def)
		}
	}
	return definitions
}

func (t *configurationReadTools) Call(ctx context.Context, name, args string) (any, error) {
	if !configurationReadName(name) {
		return nil, errors.New("informational configuration retry allows only local configuration and time reads")
	}
	return t.base.Call(ctx, name, args)
}

func (t *configurationReadTools) ToolStatus() string {
	names := []string{}
	for _, def := range t.base.Definitions() {
		names = append(names, def.Function.Name)
	}
	return "В обычном режиме Bridge установлены следующие инструменты: " + strings.Join(names, ", ") + ". Сейчас идёт справочный повтор: разрешены только чтение конфигурации и времени. Остальные инструменты здесь недоступны для вызова, но это не означает, что их нет в Bridge."
}

func (t *configurationReadTools) InitialCalls(text string) []llm.FunctionCall {
	if p, ok := t.base.(Prefetcher); ok {
		return configurationReadCalls(p.InitialCalls(text))
	}
	return nil
}

func (t *configurationReadTools) InitialCallsForConversation(messages []llm.Message) []llm.FunctionCall {
	if p, ok := t.base.(interface {
		InitialCallsForConversation([]llm.Message) []llm.FunctionCall
	}); ok {
		return configurationReadCalls(p.InitialCallsForConversation(messages))
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return t.InitialCalls(messages[i].Content)
		}
	}
	return nil
}

func configurationReadCalls(calls []llm.FunctionCall) []llm.FunctionCall {
	var filtered []llm.FunctionCall
	for _, call := range calls {
		if configurationReadName(call.Name) {
			filtered = append(filtered, call)
		}
	}
	return filtered
}
