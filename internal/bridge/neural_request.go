package bridge

import (
	"regexp"
	"strings"
)

var neuralRequestPrefix = regexp.MustCompile(`(?i)^\s*(?:алиса[,\s]+)?(?:спроси|используй)\s+нейросеть(?:[,\s:;.!?—–-]+|$)`)

// Strip only a leading routing instruction with a nonempty question. Exact
// commands without a question remain available to the previous-question retry.
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
