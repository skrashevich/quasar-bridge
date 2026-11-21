package main

import (
	"errors"
	"fmt"
	"io"

	"quasarbridge/internal/llm"
)

// Explicit setup diagnostics show the provider response in full. Normal runtime
// logging continues to use HTTPError.Error(), which omits upstream contents.
func writeLLMProviderError(out io.Writer, err error) {
	if failure, ok := errors.AsType[*llm.HTTPError](err); ok {
		fmt.Fprintf(out, "LLM HTTP %d — ответ сервера:\n%s\n", failure.StatusCode, failure.Body)
	}
}
