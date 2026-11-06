package bridge

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

// Фразы «спроси нейросеть» и «спроси свою модель» остаются в тексте запроса,
// поэтому системный промпт должен объяснять модели, что это лишь выбор обработчика.
// Иначе модель отвечает «у меня нет инструмента, чтобы подключиться к нейросети».
func TestFallbackPromptExplainsAskNeuralNetworkPrefix(t *testing.T) {
	var system, user string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil || len(body.Messages) < 2 {
			t.Errorf("запрос к LLM не разобран: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		system, user = body.Messages[0].Content, body.Messages[len(body.Messages)-1].Content
		_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Потому что свет рассеивается."}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{LLM: &llm.Client{BaseURL: "https://proxy.example.invalid/v1", Model: "fixture", HTTP: &http.Client{Transport: configuredClassifierTransport{target}}}}
	const question = "Спроси нейросеть: почему небо голубое?"
	result, err := engine.Fallback(t.Context(), question)
	if err != nil || result.Route != "llm" || result.Answer == "" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if user != question {
		t.Errorf("текст запроса изменён: %q", user)
	}
	for _, want := range []string{"спроси нейросеть", "спроси свою модель", "не комментируй"} {
		if !strings.Contains(system, want) {
			t.Errorf("в системном промпте нет %q", want)
		}
	}
}
