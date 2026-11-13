package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"quasarbridge/internal/wire"
)

var phrases = map[string]string{
	"start": "включи эйрплей", "stop": "выключи эйрплей",
	"weather": "какая завтра погода", "music": "включи музыку", "timer": "поставь таймер на пять минут",
	"llm": "спроси нейросеть: первый вопрос", "followup": "а вчера?",
	"reset": "начни новый разговор", "clean": "спроси нейросеть: проверка после сброса",
	"isolation": "спроси нейросеть: проверка другой колонки",
	"home":      "Включи свет в кабинете", "state": "Горит ли свет в кабинете?",
	"failure": "спроси нейросеть: ошибка провайдера",
}
var cloudAnswers = map[string]string{
	"какая завтра погода": "Завтра солнечно.", "включи музыку": "Включаю музыку.", "поставь таймер на пять минут": "Таймер на пять минут поставлен.",
}

type turn struct {
	name, device, answer string
	text                 bool
	want                 counters
}

func scenario(ctx context.Context, base, fixtures, name string) error {
	for _, endpoint := range []string{base, fixtures} {
		u, err := url.Parse(endpoint)
		if err != nil {
			return err
		}
		if err := loopback(u.Hostname()); err != nil {
			return err
		}
		if u.Scheme != "http" {
			return fmt.Errorf("E2E endpoints must use HTTP")
		}
	}
	local := counters{Voice: 1, LLM: 1, TTS: 1}
	first := turn{"llm", "dialogue-a", "Первый ответ модели.", false, local}
	var turns []turn
	switch name {
	case "weather", "music", "timer":
		turns = []turn{{name, "cloud", cloudAnswers[phrases[name]], false, counters{Voice: 1, Cloud: 1}}}
	case "llm":
		turns = []turn{{"llm", "voice-llm", first.answer, false, local}}
	case "text":
		turns = []turn{{"llm", "text-llm", first.answer, true, counters{LLM: 1, TTS: 1}}}
	case "dialogue":
		turns = []turn{first, {"followup", first.device, "Ответ с учётом первого вопроса.", false, local}}
	case "isolation":
		turns = []turn{first, {"isolation", "dialogue-b", "Отдельный диалог.", false, local}}
	case "reset":
		turns = []turn{first, {"reset", first.device, "Начинаем новый разговор.", false, counters{Voice: 1, TTS: 1}}, {"clean", first.device, "История очищена.", false, local}}
	case "home":
		turns = []turn{{"home", "home", "Готово: Свет в кабинете.", false, counters{Voice: 1, LLM: 1, TTS: 1, HomeWrites: 1, HomeReads: 3}}, {"state", "home", "Свет в кабинете включён.", false, counters{Voice: 1, LLM: 1, TTS: 1, HomeReads: 2}}}
	case "failure":
		turns = []turn{{"failure", "failure", "Сейчас не получилось получить ответ локальной модели.", false, local}, {"llm", "failure", first.answer, false, local}}
	default:
		return fmt.Errorf("unknown scenario %q", name)
	}
	// Each scenario uses distinct speaker IDs, even when the mock lives across cases.
	for i, input := range turns {
		input.device = name + "-" + input.device
		if err := exchange(ctx, base, fixtures, input); err != nil {
			return fmt.Errorf("%s turn %d: %w", name, i+1, err)
		}
	}
	log.Printf("PASS %s: answer, backend calls, routing, TTS and request correlation", name)
	return nil
}

func exchange(ctx context.Context, base, fixtures string, input turn) error {
	before, err := readCounters(ctx, fixtures)
	if err != nil {
		return err
	}
	if before.Unexpected != 0 {
		return fmt.Errorf("fixture already recorded unexpected calls: %+v", before)
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	ws, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/uni.ws?feature=protobuf", nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	id := fmt.Sprintf("kuyase-%s-%d", input.name, time.Now().UnixNano())
	body := &request.TAliceRequestProto{
		Application: &app.TClientInfoProto{DeviceId: new(input.device)},
		Request:     &request.TAliceRequestProto_TRequest{Event: &common.TEvent{Type: common.EEventType_voice_input.Enum()}}, Format: new("audio/opus"),
	}
	event := &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: id, StreamId: new(int64(1))}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: body}}
	if input.text {
		body.Request.Event.Text = new(phrases[input.name])
		body.Request.Event.Type = common.EEventType_text_input.Enum()
		event.Event = &client.TClientMessage_TEvent_TextInput{TextInput: body}
	}
	data, err := (wire.Frame{Client: &client.TClientMessage{Message: &client.TClientMessage_Event{Event: event}}, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err = ws.Write(ctx, websocket.MessageBinary, data); err != nil {
		return err
	}
	sawASR, sawAnswer := input.text, false
	for !sawASR || !sawAnswer {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		event := wire.Decode(data, wire.ServerToClient).Server.GetEvent()
		if event.GetHeader().GetRefMessageId() != id {
			continue
		}
		if asr := event.GetAsrResult(); asr != nil && asr.GetEndOfUtt() {
			if len(asr.GetRecognition()) == 0 || asr.GetRecognition()[0].GetNormalized() != phrases[input.name] {
				return fmt.Errorf("ASR transcript changed")
			}
			sawASR = true
		}
		if response := event.GetAliceResponse(); response != nil {
			answer := response.GetVoiceResponse().GetOutputSpeech().GetText()
			if answer != input.answer {
				return fmt.Errorf("answer=%q want=%q", answer, input.answer)
			}
			sawAnswer = true
		}
	}
	// reply() sends AliceResponse before TtsGenerate. Keep the request alive until
	// the upstream has observed synthesis, rather than cancelling it on the answer.
	for {
		after, err := readCounters(ctx, fixtures)
		if err != nil {
			return err
		}
		diff := counters{Voice: after.Voice - before.Voice, Cloud: after.Cloud - before.Cloud, LLM: after.LLM - before.LLM, TTS: after.TTS - before.TTS, HomeWrites: after.HomeWrites - before.HomeWrites, HomeReads: after.HomeReads - before.HomeReads, Unexpected: after.Unexpected - before.Unexpected}
		if diff == input.want {
			break
		}
		if diff.Unexpected > 0 || diff.Voice > input.want.Voice || diff.Cloud > input.want.Cloud || diff.LLM > input.want.LLM || diff.TTS > input.want.TTS || diff.HomeWrites > input.want.HomeWrites || diff.HomeReads > input.want.HomeReads {
			return fmt.Errorf("backend calls=%+v want=%+v", diff, input.want)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("backend calls=%+v want=%+v: %w", diff, input.want, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if _, err := readHealth(ctx, strings.TrimSuffix(base, "/")); err != nil {
		return err
	}
	return nil
}
