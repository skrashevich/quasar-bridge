package bridge

import (
	"regexp"
	"strings"
)

const ttsMarkupPrompt = `
Финальный ответ озвучивает синтезатор Алисы. Для нужного произношения можно поставить + перед ударной гласной (зам+ок), сделать паузу через sil <[500]> (миллисекунды) или указать фонемы после слова в <[...]>.
По явной просьбе меняй озвучку тегами <speaker is_whisper="true"> (шёпот), <speaker is_whisper="false"> (обычная речь), <speaker voice="zahar"> (голос по известному ID) или <speaker audio="alice-sounds-game-win-1.opus"> (звук по известному ID из библиотеки Алисы).
Если доступен search_tts_catalog, ищи нужный голос или звук по короткому названию и подставляй найденный id. Используй точный ID из каталога, запроса или примера.
Для темпа используй <speaker speed="1.2"> (1 — обычный темп), для языка — <speaker lang="en-US">.
Тег <speaker effect="megaphone"> добавляет эффект к штатному голосу. Голос и эффект можно задать в одном теге, например <speaker voice="zahar" effect="megaphone">.
Доступные эффекты — behind_the_wall, hamster, megaphone, pitch_down, psychodelic, pulse, train_announce. Сними эффект через <speaker effect="-">. У тегов <speaker> нет закрывающего тега. Пиши разметку прямо в тексте для синтеза, не описывай её словами.
`

var ttsMarkup = regexp.MustCompile(
	`</?speaker(?:\s+[^<>]*)?/?>|` +
		`\bsil\s*<\[\s*[0-9]+\s*\]>|\s*<\[\s*[a-z]+(?:\s+[a-z]+)*\s*\]>|\+([АЕЁИОУЫЭЮЯаеёиоуыэюя])`)

func (e *Engine) ttsInstructions() string {
	if e.TTSMarkup {
		return ttsMarkupPrompt
	}
	return ""
}

func (r *Resolution) setModelAnswer(text string, useMarkup bool) {
	plain := ttsMarkup.ReplaceAllString(text, "$1")
	r.Answer = plain
	if plain != text {
		r.Answer = strings.Join(strings.Fields(plain), " ")
		if useMarkup {
			r.speech = text
		}
	}
}
