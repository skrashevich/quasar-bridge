package bridge

import (
	"context"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

// Модель не должна видеть обращение «спроси у нейросети»: иначе она отвечает,
// что не умеет обращаться к другим нейросетям.
func TestStripModelPrefix(t *testing.T) {
	e := &Engine{StripPrefixes: DefaultStripPrefixes()}
	cases := []struct{ in, want string }{
		{"спроси нейросеть: расскажи о звёздах", "расскажи о звёздах"},
		{"Спроси у нейросети который час", "который час"},
		{"спроси у своей модели, что такое HTTP", "что такое HTTP"},
		{"спроси модель\nпочему небо голубое", "почему небо голубое"},
		{"спроси нейросеть", "спроси нейросеть"}, // после срезания пусто — оставляем исходное
		{"какая завтра погода", "какая завтра погода"},
		{"включи свет в гостиной", "включи свет в гостиной"},
		{"алиса, спроси нейросеть: тест", "алиса, спроси нейросеть: тест"},
	}
	for _, tc := range cases {
		if got := e.stripModelPrefix(tc.in); got != tc.want {
			t.Errorf("stripModelPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := (&Engine{}).stripModelPrefix("спроси нейросеть: тест"); got != "спроси нейросеть: тест" {
		t.Errorf("empty config must keep text, got %q", got)
	}
}

// В модель уходит срезанный текст, а в истории диалога остаётся исходная фраза.
func TestStripModelPrefixKeepsHistoryOriginal(t *testing.T) {
	var prompts []string
	e := &Engine{
		StripPrefixes: DefaultStripPrefixes(),
		LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
			if structured {
				return `{"route":"llm"}`, nil
			}
			for _, message := range m {
				if message.Role == "user" {
					prompts = append(prompts, message.Content)
				}
			}
			return "Ответ", nil
		}),
	}
	if _, err := e.Resolve(t.Context(), "спроси у нейросети: что такое HTTP"); err != nil {
		t.Fatal(err)
	}
	if len(prompts) == 0 || prompts[0] != "что такое HTTP" {
		t.Fatalf("model got %q, want stripped text", prompts)
	}
	e.Resolve(t.Context(), "а что такое TLS?")
	if len(prompts) < 2 || !strings.Contains(prompts[1], "спроси у нейросети: что такое HTTP") {
		t.Fatalf("dialogue history lost original text: %q", prompts)
	}
}
