// Package commandprompt requests an audible confirmation from the station's native Alice service.
package commandprompt

import (
	"context"
	"errors"
	"fmt"

	"github.com/skrashevich/quasarc/station"
)

// Prefix identifies the private utterance on the station's normal TTS connection.
const Prefix = "Код подтверждения команды"

// Play asks Alice to repeat the confirmation number and waits for playback to finish.
func Play(ctx context.Context, speakerConfig string, number int) error {
	if number < 100 || number > 999 {
		return errors.New("invalid command confirmation number")
	}
	client := station.Client{ConfigPath: speakerConfig}
	return client.Speak(ctx, fmt.Sprintf("%s — %d. Повторяю, %d.", Prefix, number, number))
}
