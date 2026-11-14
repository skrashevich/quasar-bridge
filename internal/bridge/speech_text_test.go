package bridge

import (
	"strings"
	"testing"
)

func TestSpeechText(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{
			name:  "reported ChatGPT source citation",
			input: "«Бавария» играет с «Кёльном» в пятницу, 20 ноября, в 22:30 по московскому времени. Матч пройдёт дома, в Мюнхене. ([app.fcbayern.com](https://app.fcbayern.com/en/?utm_source=openai)) .",
			want:  "«Бавария» играет с «Кёльном» в пятницу, 20 ноября, в 22:30 по московскому времени. Матч пройдёт дома, в Мюнхене.",
		},
		{"readable link label", "Смотри [расписание матчей](https://example.org/schedule).", "Смотри расписание матчей."},
		{"newline before source destination", "Ответ готов. ([example.org]\n(https://example.org)).", "Ответ готов."},
		{"whitespace before readable destination", "Смотри [расписание] \t\n (https://example.org).", "Смотри расписание."},
		{"meaningful source label", "[Источник света](https://example.org/lamp) — лампа.", "Источник света — лампа."},
		{"URL source label", "Ответ готов. ([https://example.org/a](https://example.org/a)).", "Ответ готов."},
		{"www source label", "Ответ готов. ([www.example.org](https://example.org)).", "Ответ готов."},
		{"bare URLs", "Ответ https://example.org/a и http://example.org/b готов.", "Ответ и готов."},
		{"www URL", "Смотри www.example.org/page, затем продолжай.", "Смотри, затем продолжай."},
		{"autolink", "Ответ готов. <https://example.org/a> Следующее предложение.", "Ответ готов. Следующее предложение."},
		{"URL sentence punctuation", "Смотри https://example.org/a. Следующее предложение!", "Смотри. Следующее предложение!"},
		{"URL query punctuation", "Смотри https://example.org/a?q=one&n=2, затем продолжай.", "Смотри, затем продолжай."},
		{"apostrophe in bare URL", "Смотри https://example.org/O'Brien. Дальше.", "Смотри. Дальше."},
		{"apostrophe in autolink", "Смотри <https://example.org/O'Brien>. Дальше.", "Смотри. Дальше."},
		{"balanced URL parentheses", "Ответ (https://example.org/wiki/Foo_(bar)). Дальше.", "Ответ. Дальше."},
		{"nested destination parentheses", "Смотри [статью](https://example.org/a(b(c))). Дальше.", "Смотри статью. Дальше."},
		{"useful outer parentheses", "Ответ ([подробнее](https://example.org)).", "Ответ (подробнее)."},
		{"URL only", "https://example.org/a", "Ссылка доступна в текстовом ответе."},
		{"citation only", "([example.org](https://example.org)).", "Ссылка доступна в текстовом ответе."},
		{"markup URL only", `<speaker voice="zahar"><speaker is_whisper="false">https://example.org`, `<speaker voice="zahar"><speaker is_whisper="false">Ссылка доступна в текстовом ответе.`},
		{"markup and links", `<speaker voice="zahar"  speed="1.2">Зам+ок sil <[500]> и транскрипция <[t r a n s k rr ii p c y j schwa]>. ([example.org](https://example.org))`, `<speaker voice="zahar"  speed="1.2">Зам+ок sil <[500]> и транскрипция <[t r a n s k rr ii p c y j schwa]>.`},
		{"ordinary text unchanged", "  Зам+ок.\nНовый абзац C++ и 2+2.  ", "  Зам+ок.\nНовый абзац C++ и 2+2.  "},
		{"empty unchanged", "", ""},
		{"whitespace unchanged", " \n ", " \n "},
		{"sound markup unchanged", `<speaker audio="alice-sounds-game-win-1.opus">`, `<speaker audio="alice-sounds-game-win-1.opus">`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := speechText(tc.input); got != tc.want {
				t.Fatalf("speechText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSpeechTextBoundedMalformedBrackets(t *testing.T) {
	text := strings.Repeat("[", 100_000) + "https://example.org"
	if got := speechText(text); got != "Ссылка доступна в текстовом ответе." {
		t.Fatalf("malformed brackets leaked URL or produced empty speech: %q", got)
	}
}

func TestSpeechTextDeepBracketsStillRemoveURLs(t *testing.T) {
	text := "До " + strings.Repeat("(", 20_000) + "https://example.org" + strings.Repeat(")", 20_000) + " после."
	if got := speechText(text); got != "До после." {
		t.Fatalf("deep brackets lost text or leaked URL: %q", got)
	}
}
