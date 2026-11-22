// Package commandprompt requests an audible confirmation from the station's native Alice service.
package commandprompt

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/skrashevich/quasar-bridge/internal/localfile"
	alice "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/alice"
	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
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
	var cfg struct {
		Aliced struct {
			Port int `json:"port"`
		} `json:"aliced"`
		Bluetoothd struct {
			Port int `json:"port"`
		} `json:"bluetoothd"`
		IOHub struct {
			Port int `json:"port"`
		} `json:"iohub_clients"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return errors.New("invalid speaker configuration")
	}
	port := cfg.Aliced.Port
	if port == 0 {
		port = 9875
	}
	var dial net.Dialer
	conn, err := dial.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	// Older Max firmware carries Remoting directly in the QuasarMessage.
	direct := cfg.Bluetoothd.Port != 0 && cfg.IOHub.Port == 0
	write := func(method *ipc.Remoting) error {
		method.RemoteObjectId = new(capability)
		message := &ipc.QuasarMessage{}
		if direct {
			message.Remoting = method
		} else {
			body, err := proto.Marshal(method)
			if err != nil {
				return err
			}
			message.Type, message.Payload = ipc.QuasarMessage_TYPE_REMOTING.Enum(), body
		}
		body, err := proto.Marshal(message)
		if err != nil {
			return err
		}
		_, err = conn.Write(append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...))
		return err
	}
	subscribe := &ipc.Remoting_CapabilityAddListenerMethod{CapabilityId: new(capability)}
	if err := write(&ipc.Remoting{Method: &ipc.Remoting_CapabilityAddListenerMethod_{CapabilityAddListenerMethod: subscribe}}); err != nil {
		return err
	}
	text := fmt.Sprintf("%s — %d. Повторяю, %d.", Prefix, number, number)
	vocalize := &ipc.Remoting_AliceCapabilityMethod{
		Method: ipc.Remoting_AliceCapabilityMethod_START_VOCALIZE.Enum(), Events: new(true),
		VocalizeDirective: &alice.TAliceCapability_TVocalizeDirective{
			Text: &text, SynthesisMode: alice.TAliceCapability_TVocalizeDirective_Append.Enum(),
		},
	}
	if err := write(&ipc.Remoting{Method: &ipc.Remoting_AliceCapabilityMethod_{AliceCapabilityMethod: vocalize}}); err != nil {
		return err
	}
	return waitPlayback(bufio.NewReader(conn))
}

func waitPlayback(r io.Reader) error {
	var started bool
	var request string
	for {
		var size [4]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(size[:])
		if n == 0 || n > 512<<10 {
			return errors.New("invalid Alice event length")
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return err
		}
		var msg ipc.QuasarMessage
		if err := proto.Unmarshal(body, &msg); err != nil {
			return err
		}
		remote := msg.GetRemoting()
		if remote == nil {
			switch msg.GetType() {
			case ipc.QuasarMessage_TYPE_REMOTING, ipc.QuasarMessage_TYPE_ALICE_STATE:
				// Some firmware wraps AliceCapability callbacks in the AliceState envelope.
				remote = new(ipc.Remoting)
				if proto.Unmarshal(msg.GetPayload(), remote) != nil {
					continue
				}
			default:
				continue
			}
		}
		if name := remote.GetRemoteObjectId(); name != "" && name != capability {
			continue
		}
		event := remote.GetAliceCapabilityListenerMethod()
		if event == nil {
			continue
		}
		switch event.GetMethod() {
		case ipc.Remoting_AliceCapabilityListenerMethod_TTS_STARTED:
			if started && request != event.GetRequestId() {
				return errors.New("overlapping Alice playback")
			}
			started, request = true, event.GetRequestId()
		case ipc.Remoting_AliceCapabilityListenerMethod_TTS_COMPLETED:
			// Station 2 supplies an ID on STARTED and omits it on COMPLETED.
			if started && (event.GetRequestId() == "" || event.GetRequestId() == request) {
				return nil
			}
		case ipc.Remoting_AliceCapabilityListenerMethod_ON_ALICE_ERROR:
			return errors.New("Alice could not speak the confirmation")
		}
	}
}
