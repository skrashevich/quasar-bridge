// Package configvoice recognizes administration of this Bridge, including
// spellings observed in the speaker's actual ASR transcripts.
package configvoice

import (
	"regexp"
	"strings"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/llm"
)

var request = regexp.MustCompile(`(?i)(?:(?:quasar|qwazar|quazar)[\s-]*bridge|квазар[\s-]*(?:бридж|блич|брич)|(?:настрой[а-яё]*|конфигурац[а-яё]*|перезапуст[а-яё]*|перезапуск)[\sа-яё]{0,40}(?:мост[а-яё]*|бридж[а-яё]*))`)

// Only elliptical questions inherit the subject. An explicit object such as
// "настройки телевизора" must go through its normal routing rules.
var followup = regexp.MustCompile(`^(?:а\s+)?(?:(?:расскажи|покажи|перечисли|назови|скажи)\s+)?(?:какие\s+)?(?:у тебя\s+)?(?:есть\s+)?(?:текущие\s+)?(?:настройки|функции|утилиты|инструменты|параметры)(?:\s+(?:сейчас|есть|включены|выключены|доступны|активны|установлены|используются|у тебя))*$`)
var modelFollowup = regexp.MustCompile(`^(?:а\s+)?(?:какая|какую)\s+модель(?:\s+(?:сейчас|выбрана|настроена|используется|используешь|у тебя))*$`)

func IsRequest(text string) bool { return request.MatchString(text) }

func isFollowup(text string) bool {
	text = strings.Join(strings.Fields(strings.ReplaceAll(dialogue.Normalize(text), ",", " ")), " ")
	switch text {
	case "подробнее", "расскажи подробнее", "а подробнее", "продолжай", "повтори":
		return true
	}
	return followup.MatchString(text) || modelFollowup.MatchString(text)
}

// InConversation receives earlier messages, excluding the current user turn.
// A new unrelated topic stops inheritance; household actions are never captured
// merely because a preceding question concerned Bridge configuration.
func InConversation(text string, earlier []llm.Message) bool {
	if IsRequest(text) {
		return true
	}
	if !isFollowup(text) {
		return false
	}
	for i := len(earlier) - 1; i >= 0; i-- {
		message := earlier[i]
		if message.Role != "user" {
			continue
		}
		if IsRequest(message.Content) {
			return true
		}
		if !isFollowup(message.Content) && !dialogue.IsFollowup(message.Content) {
			return false
		}
	}
	return false
}
