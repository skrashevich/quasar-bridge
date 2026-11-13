// Local verification control for the ordinary Bridge executable in kuyase.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/wire"
)

func main() {
	mode := flag.String("mode", "command", "upstream or command")
	listen := flag.String("listen", "127.0.0.1:18444", "Loopback mock upstream address")
	bridge := flag.String("bridge", "http://127.0.0.1:8443", "Loopback Bridge HTTP URL")
	action := flag.String("action", "start", "start, stop, or an E2E scenario")
	fixtureURL := flag.String("fixtures", "http://127.0.0.1:18444", "Loopback fixtures URL")
	timeout := flag.Duration("timeout", 45*time.Second, "Command deadline")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch *mode {
	case "upstream":
		var host string
		host, _, err = net.SplitHostPort(*listen)
		if err == nil {
			err = loopback(host)
		}
		if err == nil {
			s := &http.Server{Addr: *listen, Handler: mockUpstream(), ReadHeaderTimeout: 5 * time.Second}
			context.AfterFunc(ctx, func() { s.Close() })
			log.Printf("mock upstream ready at ws://%s/uni.ws (loopback only)", *listen)
			err = s.ListenAndServe()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
		}
	case "command":
		ctx, stop := context.WithTimeout(ctx, *timeout)
		defer stop()
		if *action == "start" || *action == "stop" {
			err = command(ctx, *bridge, *action)
		} else {
			err = scenario(ctx, *bridge, *fixtureURL, *action)
		}
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func loopback(host string) error {
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return nil
	}
	return errors.New("verification control accepts loopback endpoints only")
}

type health struct {
	Status  string `json:"status"`
	Mode    string `json:"mode"`
	AirPlay struct {
		Enabled bool   `json:"enabled"`
		State   string `json:"state"`
		Error   string `json:"error"`
	} `json:"airplay"`
}

func readHealth(ctx context.Context, base string) (health, error) {
	var result health
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return result, err
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("health HTTP status %d", response.StatusCode)
	}
	err = json.UnmarshalRead(response.Body, &result)
	if err == nil && (result.Status != "ok" || result.Mode != "hybrid" || !result.AirPlay.Enabled) {
		err = errors.New("expected healthy hybrid Bridge with AirPlay enabled")
	}
	return result, err
}

func command(ctx context.Context, base, action string) error {
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil {
		return err
	}
	if err = loopback(u.Hostname()); err != nil {
		return err
	}
	if u.Scheme != "http" {
		return errors.New("use HTTP for this isolated guest loopback test")
	}
	base = u.String()
	want, answer := "running", "AirPlay включён."
	if action == "stop" {
		want, answer = "stopped", "AirPlay выключен."
	} else if action != "start" {
		return errors.New("action must be start or stop")
	}
	before, err := readHealth(ctx, base)
	if err != nil {
		return err
	}
	if before.AirPlay.State == want {
		return fmt.Errorf("AirPlay is already %s; this test requires a real state transition", want)
	}
	ws, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/uni.ws?feature=protobuf", nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	id := fmt.Sprintf("kuyase-%s-%d", action, time.Now().UnixNano())
	message := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: id, StreamId: new(int64(1))},
		Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{
			Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus"),
		}},
	}}}
	data, err := (wire.Frame{Client: message, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err = ws.Write(ctx, websocket.MessageBinary, data); err != nil {
		return err
	}
	sawASR, sawAnswer := false, false
	for !sawASR || !sawAnswer {
		_, data, err = ws.Read(ctx)
		if err != nil {
			return err
		}
		frame := wire.Decode(data, wire.ServerToClient)
		event := frame.Server.GetEvent()
		if event.GetHeader().GetRefMessageId() != id {
			continue
		}
		if result := event.GetAsrResult(); result != nil && result.GetEndOfUtt() {
			sawASR = true
		}
		if response := event.GetAliceResponse(); response != nil {
			text := response.GetVoiceResponse().GetOutputSpeech().GetText()
			if !strings.HasPrefix(text, answer) {
				return fmt.Errorf("unexpected local answer: %q", text)
			}
			sawAnswer = true
		}
	}
	ws.CloseNow()
	// Ensure closing the voice request did not stop the application-owned receiver.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}
	after, err := readHealth(ctx, base)
	if err != nil {
		return err
	}
	if after.AirPlay.State != want || after.AirPlay.Error != "" {
		return fmt.Errorf("after closing request: state=%s error=%s", after.AirPlay.State, after.AirPlay.Error)
	}
	log.Printf("PASS actual Bridge VoiceInput -> mocked final ASR -> local AirPlay %s; state=%s after request disconnect", action, want)
	return nil
}
