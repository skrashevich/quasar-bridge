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
var routingFollowup = regexp.MustCompile(`^(?:а\s+)?(?:(?:покажи|перечисли|назови|расскажи)\s+)?(?:какие\s+)?(?:у тебя\s+)?(?:есть\s+)?(?:текущие\s+)?(?:правила|маршруты)(?:\s+(?:сейчас|есть|включены|выключены|доступны|активны|действуют|установлены))*$`)
var routingMutationFollowup = regexp.MustCompile(`^(?:а\s+)?(?:включи|отключи|выключи|удали|измени|поменяй|перемести|передвинь|поставь)\s+(?:это\s+)?правило(?:\s+.*)?$`)

func IsRequest(text string) bool { return request.MatchString(text) || IsRoutingRequest(text) }

// A request about directing questions is administration, rather than a request
// to execute the weather/home/media example embedded inside that question.
func IsRoutingRequest(text string) bool {
	text = dialogue.Normalize(text)
	if strings.Contains(text, "классификатор") {
		return true
	}
	if (strings.Contains(text, "правил") || strings.Contains(text, "маршрут")) && request.MatchString(text) {
		return true
	}
	if strings.Contains(text, "маршрутизац") && (strings.Contains(text, "правил") || strings.Contains(text, "запрос") || strings.Contains(text, "настрой")) {
		return true
	}
	if strings.Contains(text, "маршрут") && (strings.Contains(text, "фраз") || strings.Contains(text, "запрос") || strings.Contains(text, "по умолчанию")) {
		return true
	}
	question := strings.Contains(text, "запрос") || strings.Contains(text, "вопрос")
	direction := strings.Contains(text, "отправля") || strings.Contains(text, "направля") || strings.Contains(text, "передава") || strings.Contains(text, "обрабатыва")
	target := strings.Contains(text, "нейросет") || strings.Contains(text, "яндекс") || strings.Contains(text, "алис") || strings.Contains(text, "home assistant") || strings.Contains(text, "умный дом")
	return question && direction && target
}

func RoutingInConversation(text string, earlier []llm.Message) bool {
	if IsRoutingRequest(text) {
		return true
	}
	text = dialogue.Normalize(text)
	if !routingFollowup.MatchString(text) && !routingMutationFollowup.MatchString(text) {
		return false
	}
	for i := len(earlier) - 1; i >= 0; i-- {
		if earlier[i].Role != "user" {
			continue
		}
		if IsRoutingRequest(earlier[i].Content) {
			return true
		}
		previous := dialogue.Normalize(earlier[i].Content)
		if !routingFollowup.MatchString(previous) && !routingMutationFollowup.MatchString(previous) && !dialogue.IsFollowup(previous) {
			return false
		}
	}
	return false
}

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
	if RoutingInConversation(text, earlier) {
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
