package bridge

import (
	"context"
	"slices"
	"testing"

	"quasarbridge/internal/dialogue"
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
		{"спроси нейросеть: -2 в третьей степени", "-2 в третьей степени"},
		{"спроси нейросеть", "спроси нейросеть"}, // после срезания пусто — оставляем исходное
		{"какая завтра погода", "какая завтра погода"},
		{"включи свет в гостиной", "включи свет в гостиной"},
		{"алиса, спроси нейросеть: тест", "тест"},
		{"АЛИСА, Спроси свою модель: почему?", "почему?"},
		{"спроси модельный ряд", "спроси модельный ряд"},
		{"спроси модель_название", "спроси модель_название"},
		{"спроси нейросеть123", "спроси нейросеть123"},
		{"спроси нейросеть: .gitignore", ".gitignore"},
		{"спроси нейросеть: !flag", "!flag"},
		{"спроси нейросеть: −2 в третьей степени", "−2 в третьей степени"},
		{"спроси нейросеть: –2 в третьей степени", "–2 в третьей степени"},
		{"спроси нейросеть: \"почему\"", "\"почему\""},
		{"спроси нейросеть — почему небо голубое", "почему небо голубое"},
		{"спроси нейросеть. Почему?", "Почему?"},
		{"спроси нейросеть! Почему?", "Почему?"},
		{"спроси нейросеть .gitignore", ".gitignore"},
		{"спроси нейросеть !flag", "!flag"},
		{"алиса, спроси нейросеть", "алиса, спроси нейросеть"},
		{"  Алиса, какая погода?  ", "  Алиса, какая погода?  "},
		{"что означает «спроси нейросеть»?", "что означает «спроси нейросеть»?"},
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

// Reintroducing an unstripped user turn in later requests revives the ambiguity.
func TestStripModelPrefixKeepsHistoryOriginal(t *testing.T) {
	var requests [][]llm.Message
	e := &Engine{
		StripPrefixes: DefaultStripPrefixes(),
		LLM: completionFunc(func(_ context.Context, m []llm.Message, _ bool) (string, error) {
			requests = append(requests, slices.Clone(m))
			return "спроси нейросеть: это цитата в ответе", nil
		}),
	}
	ctx := t.Context()
	const first = "спроси у нейросети: что такое HTTP"
	if _, err := e.Fallback(ctx, first); err != nil {
		t.Fatal(err)
	}
	const followup = "А что такое TLS?"
	if _, err := e.Fallback(ctx, followup); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || len(requests[1]) != 4 {
		t.Fatalf("requests: %+v", requests)
	}
	if requests[0][1].Content != "что такое HTTP" || requests[1][1].Content != "что такое HTTP" || requests[1][3].Content != followup {
		t.Fatalf("model saw unstripped or altered user messages: %+v", requests)
	}
	if requests[1][2].Content != "спроси нейросеть: это цитата в ответе" {
		t.Fatalf("assistant history changed: %+v", requests[1])
	}
	stored := e.Dialogue.View(ctx).Messages
	if len(stored) != 4 || stored[0].Content != first || stored[2].Content != followup {
		t.Fatalf("stored history lost original text: %+v", stored)
	}
}

func TestStripModelPrefixCustomMatches(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prefixes    []string
		input, want string
	}{
		{"longest first", []string{"спроси", "спроси нейросеть"}, "спроси нейросеть: почему?", "почему?"},
		{"longest empty request", []string{"спроси", "спроси нейросеть"}, "спроси нейросеть", "спроси нейросеть"},
		{"longest punctuation-only request", []string{"спроси", "спроси нейросеть"}, "спроси нейросеть:", "спроси нейросеть:"},
		{"longest reversed", []string{"спроси нейросеть", "спроси"}, "спроси нейросеть: почему?", "почему?"},
		{"unicode shorter folded prefix", []string{"K"}, "K: вопрос", "вопрос"},
		{"unicode longer configured prefix", []string{"K"}, "k: вопрос", "вопрос"},
		{"custom prefix includes Alice", []string{"Алиса, спроси нейросеть"}, "Алиса, спроси нейросеть: вопрос", "вопрос"},
		{"custom Alice overlap", []string{"Алиса", "спроси нейросеть", "Алиса, спроси нейросеть"}, "Алиса, спроси нейросеть: вопрос", "вопрос"},
		{"custom Alice empty longest", []string{"Алиса", "Алиса, спроси нейросеть"}, "Алиса, спроси нейросеть", "Алиса, спроси нейросеть"},
		{"custom prefix includes delimiter", []string{"бот:"}, "бот:вопрос", "вопрос"},
		{"empty entries", []string{" ", "спроси модель"}, "спроси модель: вопрос", "вопрос"},
		{"disabled preserves text", nil, "  спроси нейросеть: вопрос  ", "  спроси нейросеть: вопрос  "},
		{"custom list replaces defaults", []string{"объясни"}, "спроси нейросеть: вопрос", "спроси нейросеть: вопрос"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{StripPrefixes: tc.prefixes}
			if got := e.stripModelPrefix(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Повтор по команде «используй нейросеть» тоже получает срезанный вопрос.
func TestRetryStripsModelPrefix(t *testing.T) {
	var prompts []string
	e := &Engine{
		StripPrefixes: DefaultStripPrefixes(),
		Dialogue:      dialogue.New(dialogue.DefaultConfig()),
		LLM: completionFunc(func(_ context.Context, m []llm.Message, structured bool) (string, error) {
			if structured {
				return `{}`, nil
			}
			for _, message := range m {
				if message.Role == "user" {
					prompts = append(prompts, message.Content)
				}
			}
			return "Небо голубое из-за рассеяния света.", nil
		}),
	}
	ctx := dialogue.WithSpeaker(t.Context(), "a")
	_, first := e.Dialogue.Begin(ctx)
	e.Dialogue.CaptureReplay(ctx, first, "спроси у нейросети: почему небо голубое", "Не могу ответить.", true, false, false, false)
	retryCtx, _, ticket := e.Dialogue.Start(ctx)
	if _, err := e.retryQuestion(retryCtx, ticket); err != nil {
		t.Fatal(err)
	}
	if len(prompts) == 0 || prompts[len(prompts)-1] != "почему небо голубое" {
		t.Fatalf("retry sent %q, want stripped text", prompts)
	}
}

func TestStripModelPrefixAfterHomeRouting(t *testing.T) {
	var request string
	e := &Engine{StripPrefixes: DefaultStripPrefixes(), Home: homeFunc(func(_ context.Context, text string) (string, error) {
		request = text
		return "Включено.", nil
	})}
	result, err := e.Resolve(t.Context(), "Спроси нейросеть: включи свет в кухне")
	if err != nil || result.Route != "home" || request != "включи свет в кухне" {
		t.Fatalf("result=%+v home request=%q err=%v", result, request, err)
	}
}

type prefixConfigurationCompleter struct{ completionFunc }

func (c prefixConfigurationCompleter) CompleteConfiguration(ctx context.Context, messages []llm.Message) (string, error) {
	return c.completionFunc(ctx, messages, false)
}

func TestRetryNormalizesHistoryAndPreservesStandaloneCommand(t *testing.T) {
	for _, configuration := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "configuration"}[configuration], func(t *testing.T) {
			var requests [][]llm.Message
			answerer := completionFunc(func(_ context.Context, messages []llm.Message, _ bool) (string, error) {
				requests = append(requests, slices.Clone(messages))
				return "Ответ.", nil
			})
			e := &Engine{Configuration: configuration, StripPrefixes: DefaultStripPrefixes(), LLM: answerer}
			if configuration {
				e.Assistant = prefixConfigurationCompleter{answerer}
			}
			ctx := dialogue.WithSpeaker(t.Context(), "prefix-retry")
			first := e.Prepare(ctx)
			e.RecordDelivered(first, "спроси нейросеть: настройки quasar bridge", "Настройки доступны.", true, false)
			second := e.Prepare(ctx)
			e.RecordDelivered(second, "спроси свою модель: какие настройки quasar bridge включены", "Не могу ответить.", true, false)
			result, err := e.Resolve(ctx, "спроси нейросеть")
			if err != nil || result.Answer != "Ответ." {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if len(requests) != 1 || len(requests[0]) != 4 {
				t.Fatalf("requests=%+v", requests)
			}
			if requests[0][1].Content != "настройки quasar bridge" || requests[0][3].Content != "какие настройки quasar bridge включены" {
				t.Fatalf("retry reintroduced prefixes: %+v", requests[0])
			}
			if requests[0][2].Content != "Настройки доступны." {
				t.Fatalf("assistant context changed: %+v", requests[0])
			}
		})
	}
}

func TestDefaultModelPrefixesRouteBeforeYandexTime(t *testing.T) {
	for _, text := range []string{
		"спроси у нейросети который час",
		"спроси нейросеть который час",
		"спроси у своей модели который час",
		"спроси свою модель который час",
		"спроси модель который час",
	} {
		e := &Engine{}
		if got := e.Decide(t.Context(), text); got.Route != "llm" || got.Rule != "complex_queries" {
			t.Errorf("%q: %+v", text, got)
		}
	}
	if got := (&Engine{}).Decide(t.Context(), "спроси нейросеть"); got.Source != "dialogue_retry" {
		t.Fatalf("standalone retry replaced by normal request: %+v", got)
	}
}
