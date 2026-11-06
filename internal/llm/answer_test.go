package llm

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateAnswer(t *testing.T) {
	for _, tc := range []struct {
		text string
		bad  bool
	}{
		{`<tool_call><function=read_webpage>`, true},
		{`<｜｜DSML｜｜ calls>`, true},
		{`<|DSML| calls>`, true},
		{`<think>рассуждения</think>Ответ.`, true},
		{`Ответ. </analysis>`, true},
		{`<|im_start|>assistant`, true},
		{"Обычный ответ.", false},
		{"Для любого x верно x < |x| + 1.", false},
		{"<p>Пример HTML</p>", false},
	} {
		if err := ValidateAnswer(tc.text); (err != nil) != tc.bad {
			t.Errorf("ValidateAnswer(%q) = %v", tc.text, err)
		}
	}
}

func TestFinalAnswerValidationAcrossAPIs(t *testing.T) {
	const content = `{"text":"<think>рассуждения</think>Ответ."}`
	for _, api := range []string{"chat_completions", "responses"} {
		t.Run(api, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var response any = map[string]any{"choices": []any{map[string]any{"message": Message{Content: content}}}}
				if api == "responses" {
					response = map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": content}}}}}
				}
				_ = json.MarshalWrite(w, response)
			}))
			defer server.Close()
			client := Client{BaseURL: server.URL, API: api}
			if answer, err := client.Complete(t.Context(), nil, false); err == nil || answer != "" {
				t.Fatalf("service markup accepted: answer=%q error=%v", answer, err)
			}
			if answer, err := client.Complete(t.Context(), nil, true); err != nil || answer != content {
				t.Fatalf("structured output changed: answer=%q error=%v", answer, err)
			}
		})
	}
}
