package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"quasarbridge/internal/assistant"
	"quasarbridge/internal/bridge"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

type configurationChatFunc func(context.Context, []llm.Message, []llm.Tool) (llm.Message, error)

func (f configurationChatFunc) Chat(ctx context.Context, messages []llm.Message, tools []llm.Tool) (llm.Message, error) {
	return f(ctx, messages, tools)
}

type noConfigurationCompleter struct{}

func (noConfigurationCompleter) Complete(context.Context, []llm.Message, bool) (string, error) {
	return "У меня нет инструментов.", nil
}

func TestConfigurationReadIsPrefetchedForNaturalFollowup(t *testing.T) {
	c := configurationFixture(t, `{"dialogue_context":{"ttl_seconds":420}}`)
	tools := &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}
	messages := []llm.Message{{Role: "user", Content: "Расскажи о конфигурации qwazar bridge"}, {Role: "assistant", Content: "Настройки доступны."}, {Role: "user", Content: "Расскажи какие настройки есть"}, {Role: "assistant", Content: "Могу посмотреть."}, {Role: "user", Content: "А какие функции сейчас включены"}}
	for _, call := range tools.InitialCallsForConversation(messages) {
		if call.Name == "get_quasar_bridge_config" {
			return
		}
	}
	t.Fatal("configuration context was not prefetched for the natural followup")
}

func TestConfigurationRetryUsesCurrentSettingsWithoutWrites(t *testing.T) {
	c := configurationFixture(t, `{"dialogue_context":{"ttl_seconds":420}}`)
	before, _ := os.ReadFile(c.path)
	modelCalls := 0
	a := &assistant.Agent{Tools: &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}, MaxRounds: 2, MaxCalls: 2, Client: configurationChatFunc(func(_ context.Context, messages []llm.Message, defs []llm.Tool) (llm.Message, error) {
		modelCalls++
		found := false
		for _, message := range messages {
			if message.Role == "tool" && strings.Contains(message.Content, `"active":420`) {
				found = true
			}
		}
		if !found {
			t.Error("current configuration unavailable to retry model")
		}
		for _, def := range defs {
			if def.Function.Name != "get_quasar_bridge_config" && def.Function.Name != "current_time" && def.Function.Name != "get_quasar_bridge_routing" && def.Function.Name != "test_quasar_bridge_routing" {
				t.Errorf("retry offered nonlocal or mutating tool %s", def.Function.Name)
			}
		}
		return llm.Message{Role: "assistant", Content: "Контекст — 420 секунд."}, nil
	})}
	e := &bridge.Engine{Configuration: true, LLM: noConfigurationCompleter{}, Assistant: a}
	ctx := dialogue.WithSpeaker(t.Context(), "midi-config-retry")
	seed := e.Prepare(ctx)
	e.RecordDelivered(seed, "Какие у тебя есть утилиты для управления qwazar bridge", "У меня нет отдельных утилит.", true, false)
	result, err := e.Resolve(ctx, "Спроси нейросеть")
	if err != nil || result.Answer != "Контекст — 420 секунд." || modelCalls != 1 {
		t.Fatalf("retry did not read configuration: result=%+v calls=%d err=%v", result, modelCalls, err)
	}
	after, _ := os.ReadFile(c.path)
	if !bytes.Equal(before, after) {
		t.Fatal("informational retry changed configuration")
	}
}

func TestConfigurationReadOnlyRetryRejectsHallucinatedWrite(t *testing.T) {
	c := configurationFixture(t, `{"dialogue_context":{"ttl_seconds":420}}`)
	before, _ := os.ReadFile(c.path)
	calls := 0
	a := &assistant.Agent{Tools: &assistant.Tools{Config: assistant.DefaultConfig(), Configuration: c}, MaxRounds: 2, MaxCalls: 2, Client: configurationChatFunc(func(_ context.Context, messages []llm.Message, _ []llm.Tool) (llm.Message, error) {
		calls++
		if calls == 1 {
			return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "unexpected-write", Type: "function", Function: llm.FunctionCall{Name: "set_quasar_bridge_config", Arguments: `{"changes":{"dialogue_context.ttl_seconds":600}}`}}}}, nil
		}
		if last := messages[len(messages)-1]; last.Role != "tool" || !strings.Contains(last.Content, `"error"`) {
			t.Error("read-only retry did not reject model write")
		}
		return llm.Message{Role: "assistant", Content: "Контекст — 420 секунд."}, nil
	})}
	reader, ok := any(a).(interface {
		CompleteConfiguration(context.Context, []llm.Message) (string, error)
	})
	if !ok {
		t.Fatal("assistant lacks a read-only configuration completion path")
	}
	if _, err := reader.CompleteConfiguration(t.Context(), []llm.Message{{Role: "user", Content: "Какая конфигурация quasar bridge"}}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(c.path)
	if !bytes.Equal(before, after) {
		t.Fatal("model bypassed read-only retry and changed configuration")
	}
}
