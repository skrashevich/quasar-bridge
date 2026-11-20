package configvoice

import (
	"regexp"
	"strings"
)

var diagnosticsCommand = regexp.MustCompile(`(?i)^(?:алиса[,\s]+)?(?:пожалуйста[,\s]+)?(включи|выключи|отключи)\s+веб[-\s]*интерфейс(?:[,\s]+пожалуйста)?[.!?,\s]*$`)

// DiagnosticsCommand accepts a whole command and never inherits dialogue context.
// Discussion, quoted commands and combined requests do not change web access.
func DiagnosticsCommand(text string) (enabled, ok bool) {
	match := diagnosticsCommand.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return false, false
	}
	return strings.EqualFold(match[1], "включи"), true
}
