package assistant

import (
	"encoding/json/v2"
	"fmt"

	"quasarbridge/internal/update"
)

// A short update question already has a complete, trusted tool result. Answer it
// directly so model latency and interpretation cannot turn an unknown build into
// an incorrect "up to date" confirmation.
func updateCheckReply(result string) string {
	var status update.Status
	if json.Unmarshal([]byte(result), &status) != nil || status.LatestVersion == "" {
		return "Не удалось проверить обновления. Попробуйте ещё раз позже."
	}
	if !status.CurrentVersionKnown {
		return fmt.Sprintf("Последняя стабильная версия — %s. На колонке сборка разработки, поэтому сравнить версии автоматически нельзя.", status.LatestVersion)
	}
	if status.UpdateAvailable {
		return fmt.Sprintf("Есть новая версия — %s. Сейчас установлена версия %s.", status.LatestVersion, status.CurrentVersion)
	}
	return fmt.Sprintf("Установлена версия %s. Более новой стабильной версии нет.", status.CurrentVersion)
}
