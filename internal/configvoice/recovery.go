package configvoice

import (
	"regexp"
	"strings"
)

const recoveryTarget = `(?:(?:quasar|qwazar|quazar)[\s-]*bridge|квазар[\s-]*(?:бридж|блич|брич))`
const recoveryPrefix = `(?i)^(?:алиса[,\s]+)?(?:пожалуйста[,\s]+)?`
const recoverySuffix = `(?:[,\s]+пожалуйста)?[.!?,\s]*$`

var recoveryMode = regexp.MustCompile(recoveryPrefix + `(?:аварийно\s+)?(?:переведи|переключи)\s+` + recoveryTarget + `\s+в\s+(?:режим\s+)?(observe|наблюдения|rollback)` + recoverySuffix)
var recoveryRollback = regexp.MustCompile(recoveryPrefix + `(?:выполни\s+)?аварийный\s+(?:откат|rollback)\s+` + recoveryTarget + recoverySuffix)

// RecoveryAction accepts whole, explicitly targeted commands only. It never
// inherits dialogue context: discussing, quoting or negating recovery is inert.
func RecoveryAction(text string) string {
	text = strings.TrimSpace(text)
	if match := recoveryMode.FindStringSubmatch(text); match != nil {
		if strings.EqualFold(match[1], "rollback") {
			return "rollback"
		}
		return "observe"
	}
	if recoveryRollback.MatchString(text) {
		return "rollback"
	}
	return ""
}
