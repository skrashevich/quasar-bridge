package bridge

import (
	_ "embed"
	"errors"
	"os"
	"strings"
)

// Built-in prompts live in prompts/*.txt so users can copy and adjust them.
var (
	//go:embed prompts/assistant.txt
	assistantPromptFile    string
	defaultAssistantPrompt = cleanPrompt(assistantPromptFile)

	//go:embed prompts/retry.txt
	retryPromptFile    string
	defaultRetryPrompt = cleanPrompt(retryPromptFile)
)

// PromptFiles names optional files that replace built-in dialogue prompts.
// An empty path keeps the built-in text.
type PromptFiles struct {
	Assistant string `json:"assistant_file,omitzero"`
	Retry     string `json:"retry_file,omitzero"`
}

// Prompts holds prompt overrides; an empty field selects the built-in prompt.
type Prompts struct {
	Assistant, Retry string
}

// LoadPrompts reads the configured files once, so edits require a restart.
func LoadPrompts(files PromptFiles) (Prompts, error) {
	var p Prompts
	for _, item := range []struct {
		path, name string
		target     *string
	}{
		{files.Assistant, "assistant_file", &p.Assistant},
		{files.Retry, "retry_file", &p.Retry},
	} {
		if item.path == "" {
			continue
		}
		data, err := os.ReadFile(item.path)
		if err != nil {
			return Prompts{}, errors.New("cannot read prompts." + item.name)
		}
		text := cleanPrompt(string(data))
		if text == "" {
			return Prompts{}, errors.New("prompts." + item.name + " must not be empty")
		}
		*item.target = text
	}
	return p, nil
}

// cleanPrompt drops the trailing newline editors add to text files.
func cleanPrompt(text string) string {
	return strings.TrimSpace(text)
}

func promptOr(override, builtIn string) string {
	if override != "" {
		return override
	}
	return builtIn
}
