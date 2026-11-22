package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/skrashevich/quasar-bridge/internal/commandprompt"
	"github.com/skrashevich/quasar-bridge/internal/journal"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	stat "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/request_stat"
	stream "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	generate "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_generate"
	speak "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
)

func TestProxyForwardsPrivateSpeechWithoutPublishingIt(t *testing.T) {
	const streamID = 48
	generation := func(id, text string) []byte {
		return aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
			Header: &header.TEventHeader{MessageId: id}, Event: &client.TClientMessage_TEvent_TtsGenerate{TtsGenerate: &generate.TTtsGenerate{Text: &text}},
		}}})
	}
	playback := func(ref, words string) [][]byte {
		h := &header.TEventHeader{RefMessageId: ref, StreamId: new(int64(streamID))}
		tts := &server.TServerMessage_TEvent{Header: h, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &speak.TTtsSpeak{Format: "audio/opus"}}}
		voice := &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: &words}}
		answer := &server.TServerMessage_TEvent{Header: h, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: voice}}}
		close := &stream.TStreamControl{StreamId: streamID, Action: &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}}}
		return [][]byte{
			aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: tts}}),
			append(binary.BigEndian.AppendUint32(nil, streamID), words...),
			aa(&server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: close}}),
			aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: answer}}),
		}
	}
	const secretID = "private-fixture-request"
	private := generation(secretID, commandprompt.Prefix+" — 123.")
	stats := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{
		Header: &header.TEventHeader{MessageId: "playback-stats"}, Event: &client.TClientMessage_TEvent_RequestStat{RequestStat: &stat.TRequestStat{RequestMessageId: secretID}},
	}}})
	public := generation("ordinary-request", "Обычная реплика")
	privateReplies := playback(secretID, "private-fixture-speech")
	publicReplies := playback("ordinary-request", "public-fixture-speech")
	type exchange struct {
		request []byte
		replies [][]byte
	}
	traffic := []exchange{{private, privateReplies}, {stats, nil}, {public, publicReplies}}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for _, step := range traffic {
			_, data, err := c.Read(r.Context())
			if err != nil || !bytes.Equal(data, step.request) {
				t.Error("upstream did not receive the original request")
				return
			}
			for _, reply := range step.replies {
				if err := c.Write(r.Context(), websocket.MessageBinary, reply); err != nil {
					return
				}
			}
		}
		// Let the proxy finish forwarding and journaling before closing the peer.
		_, _, _ = c.Read(r.Context())
	}))
	defer up.Close()
	var log journal.Writer
	defer log.Close()
	config := journal.DefaultConfig()
	config.Enabled, config.Path = true, filepath.Join(t.TempDir(), "turns.jsonl")
	if err := log.Configure(config); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var observed []string
	proxy := httptest.NewServer(&Proxy{Upstream: "ws" + strings.TrimPrefix(up.URL, "http"), Journal: &log,
		Observer: func(_ string, _ websocket.MessageType, data []byte) {
			mu.Lock()
			defer mu.Unlock()
			observed = append(observed, string(data))
		},
	})
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	for _, step := range traffic {
		if err := c.Write(ctx, websocket.MessageBinary, step.request); err != nil {
			t.Fatal(err)
		}
		for _, want := range step.replies {
			_, got, err := c.Read(ctx)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("speaker did not receive the original reply", err)
			}
		}
	}
	want := []string{string(public)}
	for _, reply := range publicReplies {
		want = append(want, string(reply))
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(observed, want) {
		t.Error("packet observer must see exactly the ordinary exchange, even when the stream ID is reused")
	}
	entries, err := os.ReadFile(config.Path)
	if err != nil || bytes.Contains(entries, []byte("private-fixture")) || !bytes.Contains(entries, []byte("public-fixture-speech")) {
		t.Errorf("conversation journal did not preserve the same privacy boundary: %v", err)
	}
}
