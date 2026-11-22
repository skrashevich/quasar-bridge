package bridge

import (
	"context"
	"github.com/skrashevich/quasar-bridge/internal/llm"
	"testing"
)

type musicCapture struct{ user string }

func (c *musicCapture) Complete(_ context.Context, m []llm.Message, _ bool) (string, error) {
	c.user = m[len(m)-1].Content
	return "Принято", nil
}

func TestMusicWebsiteRequestNormalization(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Включи музыку с сайта abc точка су слэш music", "Включи музыку с сайта https://abc.su/music"},
		{"Объясни light.office", "Объясни light.office"},
		{"Сравни переменные foo.bar и baz.qux", "Сравни переменные foo.bar и baz.qux"},
	} {
		c := new(musicCapture)
		e := Engine{Assistant: c}
		if _, err := e.Fallback(t.Context(), tc.input); err != nil {
			t.Fatal(err)
		}
		if c.user != tc.want {
			t.Errorf("%q got %q want %q", tc.input, c.user, tc.want)
		}
	}
}
