// Package commandprompt requests an audible confirmation from the station's native Alice service.
package commandprompt

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/skrashevich/quasar-bridge/internal/localfile"
	"github.com/skrashevich/quasar-bridge/internal/wire"
	alice "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/alice"
	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
)

// Prefix identifies the private utterance on the station's normal TTS connection.
const Prefix = "Код подтверждения команды"
const capability = "AliceCapability"

// Play sends one complete utterance to aliced over loopback. Alice selects her
// normal voice, volume and output. Success requires playback start and completion.
func Play(ctx context.Context, speakerConfig string, number int) error {
	if number < 100 || number > 999 {
		return errors.New("invalid command confirmation number")
	}
	raw, _, err := localfile.Read(speakerConfig, localfile.Follow, 1<<20)
	if err != nil {
		return err
	}
	type service struct {
		Port int `json:"port"`
	}
	var cfg struct {
		Aliced     service `json:"aliced"`
		Bluetoothd service `json:"bluetoothd"`
		IOHub      service `json:"iohub_clients"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return errors.New("invalid speaker configuration")
	}
	var dial net.Dialer
	conn, err := dial.DialContext(ctx, "tcp4", fmt.Sprintf("127.0.0.1:%d", cmp.Or(cfg.Aliced.Port, 9875)))
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	subscribe := &ipc.Remoting_CapabilityAddListenerMethod{CapabilityId: new(capability)}
	text := fmt.Sprintf("%s — %d. Повторяю, %d.", Prefix, number, number)
	vocalize := &ipc.Remoting_AliceCapabilityMethod{
		Method: ipc.Remoting_AliceCapabilityMethod_START_VOCALIZE.Enum(),
		Events: new(true),
		VocalizeDirective: &alice.TAliceCapability_TVocalizeDirective{
			Text:          &text,
			SynthesisMode: alice.TAliceCapability_TVocalizeDirective_Append.Enum(),
		},
	}
	// Older Max firmware exposes bluetoothd without an IOHub and uses the direct RPC field.
	direct := cfg.Bluetoothd.Port != 0 && cfg.IOHub.Port == 0
	for _, call := range []*ipc.Remoting{
		{Method: &ipc.Remoting_CapabilityAddListenerMethod_{CapabilityAddListenerMethod: subscribe}},
		{Method: &ipc.Remoting_AliceCapabilityMethod_{AliceCapabilityMethod: vocalize}},
	} {
		call.RemoteObjectId = new(capability)
		if err := wire.WriteRemoting(conn, call, direct); err != nil {
			return err
		}
	}
	return waitPlayback(bufio.NewReader(conn))
}

func waitPlayback(r io.Reader) error {
	var startedID *string
	for {
		msg, err := wire.ReadQuasarMessage(r, 512<<10)
		if err != nil {
			return err
		}
		remote := wire.UnwrapRemoting(msg)
		event := remote.GetAliceCapabilityListenerMethod()
		if name := remote.GetRemoteObjectId(); event == nil || name != "" && name != capability {
			continue
		}
		id := event.GetRequestId()
		switch event.GetMethod() {
		case ipc.Remoting_AliceCapabilityListenerMethod_TTS_STARTED:
			if startedID != nil && *startedID != id {
				return errors.New("overlapping Alice playback")
			}
			startedID = &id
		case ipc.Remoting_AliceCapabilityListenerMethod_TTS_COMPLETED:
			// Station 2 supplies an ID on STARTED and omits it on COMPLETED.
			if startedID != nil && (id == "" || id == *startedID) {
				return nil
			}
		case ipc.Remoting_AliceCapabilityListenerMethod_ON_ALICE_ERROR:
			return errors.New("Alice could not speak the confirmation")
		}
	}
}
