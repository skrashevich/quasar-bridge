package bridge

import (
	"regexp"
	"strings"
)

var neuralRequestPrefix = regexp.MustCompile(`(?i)^\s*(?:алиса[,\s]+)?(?:спроси|используй)\s+нейросеть(?:[,\s:;.!?—–-]+|$)`)

// Keep bare commands intact so they can repeat the previous question.
func stripNeuralRequestPrefix(text string) string {
	span := neuralRequestPrefix.FindStringIndex(text)
	if span == nil {
		return text
	}
	question := strings.TrimSpace(text[span[1]:])
	if question == "" {
		return text
	}
	return question
}
