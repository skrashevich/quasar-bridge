package llm

import "strings"

const operatorNoteLead = "Дополнительные указания оператора. Комната и место из этих указаний важнее общего правила переспрашивать комнату. Формат ответа, запрет выдумывать устройства и запрет сообщать об исполнении без результата сохраняются:\n"

// AppendOperatorNote keeps a single system message and adds optional operator
// text after the built-in instructions. Chat templates that allow only one
// system message still accept the request.
func AppendOperatorNote(base, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return base
	}
	return base + "\n" + operatorNoteLead + note
}
