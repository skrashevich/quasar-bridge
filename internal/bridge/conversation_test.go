package bridge

import (
	"encoding/json/v2"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	"os"
	"path/filepath"
	"strings"
	"testing"

	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	deferred "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/streaming"
	"google.golang.org/protobuf/encoding/protowire"
	"quasarbridge/internal/journal"
)

func TestConversationTextAndDeliveredResponse(t *testing.T) {
	w := new(journal.Writer)
	defer w.Close()
	c := journal.DefaultConfig()
	c.Enabled = true
	c.Path = filepath.Join(t.TempDir(), "turns.jsonl")
	if err := w.Configure(c); err != nil {
		t.Fatal(err)
	}
	log := newConversation(w)
	msg := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "private-device-id"}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{Request: &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Text: new("привет")}}}}}}}
	log.client(aa(msg))
	log.route("private-device-id", "llm")
	answer := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "private-device-id"}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new("добрый день")}}}}}}}
	log.server(aa(answer), true)
	log.client([]byte("secret-raw-audio"))
	log.server([]byte(`{"token":"secret-auth"}`), true)
	data, err := os.ReadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-device-id") || strings.Contains(string(data), "secret-") {
		t.Fatal("private payload leaked")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("events=%d", len(lines))
	}
	var e journal.Event
	if err := json.Unmarshal([]byte(lines[2]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Route != "llm" || !e.Delivered || e.Speech != "добрый день" || e.Request == "" {
		t.Fatalf("bad response: %+v", e)
	}
}

func TestConversationCorrelatesASRByApplicationRequestID(t *testing.T) {
	w := new(journal.Writer)
	defer w.Close()
	c := journal.DefaultConfig()
	c.Enabled = true
	c.Path = filepath.Join(t.TempDir(), "turns.jsonl")
	w.Configure(c)
	log := newConversation(w)
	input := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "voice-id"}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Header: &request.TAliceRequestProto_THeader{RequestId: new("application-id")}}}}}}
	log.client(aa(input))
	result := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{SubRequestRef: &header.TEventHeader_TSubRequestRef{RequestId: "application-id"}}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("погода")}}}}}}}
	log.server(aa(result), false)
	raw, _ := os.ReadFile(c.Path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var e journal.Event
	json.Unmarshal([]byte(lines[len(lines)-1]), &e)
	if e.Event != "transcript" || e.Text != "погода" || e.Request != journalID("voice-id") || e.Route != "yandex" {
		t.Fatalf("bad transcript: %+v", e)
	}
}

func TestConversationRecordsDeferredSpeechDeltas(t *testing.T) {
	w := new(journal.Writer)
	defer w.Close()
	cfg := journal.DefaultConfig()
	cfg.Enabled = true
	cfg.Path = filepath.Join(t.TempDir(), "turns.jsonl")
	if err := w.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	log := newConversation(w)
	base := new(deferred.TDeferedAliceResponseProto_TBaseResponse)
	voice := protowire.AppendTag(nil, 2, protowire.BytesType)
	voice = protowire.AppendString(voice, "Луна — спутник Земли.")
	base.ProtoReflect().SetUnknown(voice)
	msg := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{
		Header: &header.TEventHeader{RefMessageId: "voice-turn"},
		Event: &server.TServerMessage_TEvent_DeferredAliceResponse{DeferredAliceResponse: &deferred.TDeferedAliceResponseProto{
			TDeferredResponse: &deferred.TDeferedAliceResponseProto_BaseResponse{BaseResponse: base},
		}},
	}}}
	log.server(aa(msg), true)
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	var e journal.Event
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.Event != "progress" || !e.Delivered || e.Speech != "Луна — спутник Земли." || e.Text != "" {
		t.Fatalf("lost spoken delta: %+v", e)
	}
}
