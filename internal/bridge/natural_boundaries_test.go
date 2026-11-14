package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNaturalPauseFirstLongSentenceStartsAtComma(t *testing.T) {
	first := "В одной далёкой деревне возле заснеженного соснового леса,"
	if utf8.RuneCountInString(first) < firstClauseMinRunes {
		t.Fatal("test fixture too short")
	}
	second := " где старый часовщик проводил вечера за работой."
	var b streamedSentenceBuffer
	var got []string
	emit := func(s string) error { got = append(got, s); return nil }
	if err := b.feed(first, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != first {
		t.Fatalf("first natural comma should be voiced immediately, got %q", got)
	}
	if err := b.feed(second, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != strings.TrimSpace(second) {
		t.Fatalf("rest of sentence should keep its original point, got %q", got)
	}
	if b.pending != "" || b.emitted != first+second {
		t.Fatalf("streaming state lost or repeated text: pending=%q emitted=%q", b.pending, b.emitted)
	}
}

func TestNaturalPauseDoesNotSplitShortSentenceAtComma(t *testing.T) {
	text := "Однажды, Григорий нашёл ключ."
	var b streamedSentenceBuffer
	var chunks []string
	if err := b.feed(text, func(s string) error { chunks = append(chunks, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0] != text {
		t.Fatalf("short sentence must remain intact, got %q", chunks)
	}
}

func TestNaturalPauseLaterSentencesPreferFullStop(t *testing.T) {
	var b streamedSentenceBuffer
	var got []string
	emit := func(s string) error { got = append(got, s); return nil }
	start := "Григорий пришёл."
	later := " Это было в очень длинный и холодный вечер, когда все соседи уже спали, а ветер раскачивал ветви."
	if err := b.feed(start, emit); err != nil {
		t.Fatal(err)
	}
	if err := b.feed(later, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != start || got[1] != strings.TrimSpace(later) {
		t.Fatalf("later sentence should wait for full stop, got %q", got)
	}
}

func TestNaturalPauseLaterVeryLongSentenceUsesCommaNotWordBoundary(t *testing.T) {
	var b streamedSentenceBuffer
	var got []string
	emit := func(s string) error { got = append(got, s); return nil }
	if err := b.feed("Вот начало. ", emit); err != nil {
		t.Fatal(err)
	}
	front := strings.Repeat("когда за окном медленно проходили облака и ветер гудел ", 13) + ","
	back := strings.Repeat(" потом герой продолжал свой рассказ ", 4)
	if err := b.feed(front, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("before long-clause threshold should still wait, got %q", got)
	}
	if err := b.feed(back, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != front {
		t.Fatalf("very long clause should stop at original comma, got %q", got)
	}
	if err := b.feed(".", emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[2] != strings.TrimSpace(back+".") {
		t.Fatalf("remaining clause or punctuation lost: %q", got)
	}
}

func TestNaturalPauseNeverCutsArbitraryUnpunctuatedText(t *testing.T) {
	var b streamedSentenceBuffer
	long := strings.Repeat("очень длинное описание без знаков препинания ", 30)
	var chunks []string
	emit := func(s string) error { chunks = append(chunks, s); return nil }
	if err := b.feed(long, emit); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Fatalf("unexpected mid-sentence split at arbitrary word: %q", chunks)
	}
	if err := b.feed(".", emit); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0] != strings.TrimSpace(long+".") {
		t.Fatalf("whole no-punctuation sentence should be preserved, got %d chunks", len(chunks))
	}
}

func TestNaturalPauseDoesNotCutDecimalComma(t *testing.T) {
	var b streamedSentenceBuffer
	var chunks []string
	emit := func(s string) error { chunks = append(chunks, s); return nil }
	prefix := strings.Repeat("В тот день часы у мастера показывали странные числа ", 2)
	if err := b.feed(prefix+"3,", emit); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Fatalf("a decimal comma must not be treated as a complete clause")
	}
	full := prefix + "3,14 и никакого чуда здесь не было."
	if err := b.feed("14 и никакого чуда здесь не было.", emit); err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || chunks[0] != full {
		t.Fatalf("decimal punctuation was split: %q", chunks)
	}
}

func TestNativeSentenceChunksPreserveSpacesAndSplitTokens(t *testing.T) {
	var b streamedSentenceBuffer
	var got []string
	emit := func(s string) error { got = append(got, s); return nil }
	for _, delta := range []string{"Перв", "ое. Вто", "рое предложение."} {
		if err := b.feedNative(delta, emit); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0] != "Первое." || got[1] != " Второе предложение." {
		t.Fatalf("native wire text must preserve the exact spaces: %q", got)
	}
	if strings.Join(got, "") != "Первое. Второе предложение." || b.pending != "" {
		t.Fatalf("native chunks lost or duplicated text: chunks=%q pending=%q", got, b.pending)
	}
}
