package main

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"time"

	alice "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/alice"
	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"google.golang.org/protobuf/proto"
)

// An offline stand-in for the guest's aliced, with the same native RPC frames.
// Only the test operator can read the utterance at this private guest path.
func startSpeakerFixture(ctx context.Context, dir string) error {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	var cfg struct {
		Common struct {
			DeviceType      string `json:"deviceType"`
			SoftwareVersion string `json:"softwareVersion"`
		} `json:"common"`
		Aliced struct {
			Port int `json:"port"`
		} `json:"aliced"`
	}
	if raw, err := os.ReadFile("/system/vendor/quasar/quasar.cfg"); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			listener.Close()
			return err
		}
	}
	cfg.Aliced.Port = listener.Addr().(*net.TCPAddr).Port
	data, err := json.Marshal(cfg)
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "speaker.cfg"), data, 0600)
	}
	if err != nil {
		listener.Close()
		return err
	}
	context.AfterFunc(ctx, func() { listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			if err := mockVocalize(c, dir); err != nil {
				log.Print("native Alice fixture rejected the playback request")
			}
			c.Close()
		}
	}()
	return nil
}

var commandUtterance = regexp.MustCompile(`^Код подтверждения команды — ([1-9][0-9]{2})\. Повторяю, ([1-9][0-9]{2})\.$`)

func mockVocalize(c net.Conn, dir string) error {
	read := func() (*ipc.Remoting, error) {
		var n uint32
		if err := binary.Read(c, binary.LittleEndian, &n); err != nil {
			return nil, err
		}
		if n == 0 || n > 4096 {
			return nil, errors.New("invalid test RPC frame length")
		}
		raw := make([]byte, n)
		if _, err := io.ReadFull(c, raw); err != nil {
			return nil, err
		}
		var frame ipc.QuasarMessage
		var call ipc.Remoting
		if proto.Unmarshal(raw, &frame) != nil || frame.GetType() != ipc.QuasarMessage_TYPE_REMOTING || proto.Unmarshal(frame.GetPayload(), &call) != nil || call.GetRemoteObjectId() != "AliceCapability" {
			return nil, errors.New("expected AliceCapability RPC")
		}
		return &call, nil
	}
	sub, err := read()
	if err != nil || sub.GetCapabilityAddListenerMethod().GetCapabilityId() != "AliceCapability" {
		return errors.New("missing Alice subscription")
	}
	request, err := read()
	if err != nil {
		return err
	}
	method := request.GetAliceCapabilityMethod()
	directive := method.GetVocalizeDirective()
	phrase := directive.GetText()
	code := commandUtterance.FindStringSubmatch(phrase)
	if method.GetMethod() != ipc.Remoting_AliceCapabilityMethod_START_VOCALIZE || !method.GetEvents() || directive.GetTtsSource() != alice.TAliceCapability_TVocalizeDirective_Server || directive.GetVoice() != "" || directive.GetSynthesisMode() != alice.TAliceCapability_TVocalizeDirective_Append || len(code) != 3 || code[1] != code[2] {
		return errors.New("expected the default Alice voice to repeat the confirmation")
	}
	for _, event := range []ipc.Remoting_AliceCapabilityListenerMethod_Method{ipc.Remoting_AliceCapabilityListenerMethod_TTS_STARTED, ipc.Remoting_AliceCapabilityListenerMethod_TTS_COMPLETED} {
		callback := &ipc.Remoting_AliceCapabilityListenerMethod{Method: event.Enum()}
		if event == ipc.Remoting_AliceCapabilityListenerMethod_TTS_STARTED {
			callback.RequestId = new("fixture-playback")
		} else if err := os.WriteFile(filepath.Join(dir, "last-utterance"), []byte(phrase), 0600); err != nil {
			return err
		}
		payload, err := proto.Marshal(&ipc.Remoting{RemoteObjectId: new("AliceCapability"), Method: &ipc.Remoting_AliceCapabilityListenerMethod_{AliceCapabilityListenerMethod: callback}})
		if err != nil {
			return err
		}
		frame, err := proto.Marshal(&ipc.QuasarMessage{Type: ipc.QuasarMessage_TYPE_REMOTING.Enum(), Payload: payload})
		if err != nil {
			return err
		}
		if _, err = c.Write(append(binary.LittleEndian.AppendUint32(nil, uint32(len(frame))), frame...)); err != nil {
			return err
		}
	}
	return nil
}
