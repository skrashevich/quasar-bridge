package llm

import (
	"strings"
	"testing"
)

func TestAppendOperatorNoteKeepsBuiltInPrompt(t *testing.T) {
	base := "Встроенная инструкция"
	if got := AppendOperatorNote(base, "  \n"); got != base {
		t.Fatalf("empty note changed prompt: %q", got)
	}
	got := AppendOperatorNote(base, "  Ты находишься в комнате «Зал». ")
	if !strings.HasPrefix(got, base) || !strings.Contains(got, "Дополнительные указания оператора") || !strings.Contains(got, "комнате «Зал»") {
		t.Fatalf("note not appended: %q", got)
	}
}
