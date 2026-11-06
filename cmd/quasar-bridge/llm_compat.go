package main

import (
	"errors"
	"fmt"
	"io"

	"quasarbridge/internal/llm"
)

// Explicit setup diagnostics show provider responses and transport causes.
// Ordinary runtime logging continues to use safe Error strings.
func writeLLMProviderError(out io.Writer, err error) {
	probe, probing := errors.AsType[*llm.ProbeError](err)
	if probing {
		phase := "обычный ответ"
		if probe.Phase == "json" {
			phase = "JSON-ответ"
		}
		fmt.Fprintf(out, "LLM — проверка: %s\n", phase)
	}
	if failure, ok := errors.AsType[*llm.HTTPError](err); ok {
		fmt.Fprintf(out, "LLM HTTP %d — ответ сервера:\n%s\n", failure.StatusCode, failure.Body)
	} else if failure, ok := errors.AsType[*llm.TransportError](err); ok {
		fmt.Fprintf(out, "LLM — ошибка соединения:\n%s\n", failure.Diagnostic())
	}
	if probing && probe.Retrying {
		fmt.Fprintln(out, "Временный сетевой сбой; повтор той же проверки (1/1), параметры не меняются.")
	}
}
