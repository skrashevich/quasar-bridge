package bridge

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Wake описывает, каким словом вызова разбудили колонку. Колонка сообщает его
// в запросе (SpotterPhrase), а мост использует для маршрутизации (wake_routes).
type Wake struct {
	// Phrase — слово вызова в нижнем регистре («алиса», «яндекс») или пусто,
	// если слова не было (активация по интонации) или оно неизвестно.
	Phrase string
	// Inherited — слово не прозвучало: колонка продолжила слушать после ответа,
	// и слово взято из предыдущего запроса.
	Inherited bool
}

type wakeKey struct{}

// WithWake добавляет слово вызова в контекст запроса.
func WithWake(ctx context.Context, w Wake) context.Context {
	return context.WithValue(ctx, wakeKey{}, w)
}

func wakeFrom(ctx context.Context) Wake {
	w, _ := ctx.Value(wakeKey{}).(Wake)
	return w
}

// NormalizeWake приводит слово вызова к виду, в котором оно хранится в настройках.
func NormalizeWake(phrase string) string {
	return strings.ToLower(strings.TrimSpace(phrase))
}

// wakeInheritTTL ограничивает, как долго после последнего запроса продолжение
// разговора без слова наследует прежнее слово вызова.
const wakeInheritTTL = 2 * time.Minute

// wakeMemory помнит слово последнего запроса одной колонки (одного соединения).
type wakeMemory struct {
	mu     sync.Mutex
	phrase string
	at     time.Time
}

// resolve определяет слово вызова запроса.
//
// Колонка присылает SpotterPhrase: настоящее слово («алиса», «яндекс»), значение
// в угловых скобках для активации без слова («<intonation>») или пустую строку при
// автоматическом продолжении прослушивания (activation_type=auto_listening).
// Продолжение разговора без слова — автоматическое продолжение прослушивания
// и активация по интонации — наследует слово предыдущего запроса, чтобы ответ
// на уточнение шёл тем же путём, что и сам вопрос. Колонка сама решает, какой из
// двух способов применить, поэтому различать их нельзя.
func (m *wakeMemory) resolve(phrase, activationType string, now time.Time) Wake {
	phrase = NormalizeWake(phrase)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case phrase != "" && !strings.HasPrefix(phrase, "<"):
		m.phrase, m.at = phrase, now
		return Wake{Phrase: phrase}
	case (phrase == "" && activationType == "auto_listening" || strings.HasPrefix(phrase, "<")) &&
		m.phrase != "" && now.Sub(m.at) <= wakeInheritTTL:
		m.at = now
		return Wake{Phrase: m.phrase, Inherited: true}
	default:
		m.phrase = ""
		return Wake{}
	}
}
