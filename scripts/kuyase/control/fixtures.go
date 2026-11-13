package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/llm"
	"quasarbridge/internal/wire"
)

type counters struct {
	Voice, Cloud, LLM, TTS, HomeWrites, HomeReads, Unexpected int
}

type fixture struct {
	mu     sync.Mutex
	counts counters
	light  string
}

func mockUpstream() http.Handler { return &fixture{light: "off"} }

func (f *fixture) fail(w http.ResponseWriter, message string) {
	f.counts.Unexpected++
	log.Printf("UNEXPECTED %s", message)
	http.Error(w, message, http.StatusBadRequest)
}

func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/uni.ws" {
		f.voice(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /stats":
		_ = json.MarshalWrite(w, f.counts)
	case "POST /v1/chat/completions":
		f.completion(w, r)
	case "POST /mcp":
		f.home(w, r)
	default:
		f.fail(w, "unknown fixture endpoint")
	}
}

func (f *fixture) completion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string        `json:"model"`
		Messages []llm.Message `json:"messages"`
	}
	if json.UnmarshalRead(http.MaxBytesReader(w, r.Body, 1<<20), &body) != nil || body.Model != "kuyase-fixture" || len(body.Messages) < 2 {
		f.fail(w, "invalid completion request")
		return
	}
	f.counts.LLM++
	last := body.Messages[len(body.Messages)-1].Content
	answer := ""
	switch {
	case strings.Contains(last, "Вопрос: Горит ли свет в кабинете?"):
		if !strings.Contains(last, `"state":"on"`) {
			f.fail(w, "home planner lacks fresh state")
			return
		}
		answer = "Свет в кабинете включён."
	case strings.Contains(last, "Запрос: Включи свет в кабинете"):
		if !strings.Contains(last, "light.office") {
			f.fail(w, "home planner lacks inventory")
			return
		}
		answer = `{"answer":"Готово","actions":[{"domain":"light","service":"turn_on","entity_id":"light.office","data":{}}]}`
	case last == "первый вопрос":
		if len(body.Messages) != 2 {
			f.fail(w, "unexpected history on first question")
			return
		}
		answer = "Первый ответ модели."
	case last == "а вчера?":
		if len(body.Messages) != 4 || body.Messages[1].Content != "первый вопрос" || body.Messages[2].Role != "assistant" || body.Messages[2].Content != "Первый ответ модели." {
			f.fail(w, "dialogue context lost or contaminated")
			return
		}
		answer = "Ответ с учётом первого вопроса."
	case last == "проверка после сброса":
		if len(body.Messages) != 2 {
			f.fail(w, "reset retained history")
			return
		}
		answer = "История очищена."
	case last == "проверка другой колонки":
		if len(body.Messages) != 2 {
			f.fail(w, "device history leaked")
			return
		}
		answer = "Отдельный диалог."
	case last == "ошибка провайдера":
		http.Error(w, "intentional provider failure", http.StatusServiceUnavailable)
		return
	default:
		f.fail(w, "unexpected completion content")
		return
	}
	_ = json.MarshalWrite(w, map[string]any{"choices": []any{map[string]any{"message": llm.Message{Role: "assistant", Content: answer}, "finish_reason": "stop"}}})
}

func (f *fixture) home(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.UnmarshalRead(http.MaxBytesReader(w, r.Body, 1<<20), &rpc) != nil {
		f.fail(w, "invalid MCP request")
		return
	}
	var result any
	if rpc.Method == "initialize" {
		result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "kuyase", "version": "1"}}
	} else if rpc.Method == "tools/call" {
		var value any
		switch rpc.Params.Name {
		case "ha_get_skill_guide":
			value = map[string]any{"success": true, "content": "fixture"}
		case "ha_search":
			f.counts.HomeReads++
			value = map[string]any{"entities": []any{map[string]any{"entity_id": "light.office", "friendly_name": "Свет в кабинете", "state": f.light, "area": "Кабинет"}}, "has_more": false}
		case "ha_get_entity_exposure":
			value = map[string]any{"success": true, "exposed_to": map[string]bool{"conversation": true}}
		case "ha_call_service":
			if rpc.Params.Arguments["entity_id"] != "light.office" || rpc.Params.Arguments["domain"] != "light" || rpc.Params.Arguments["service"] != "turn_on" {
				f.fail(w, "wrong home service target")
				return
			}
			f.counts.HomeWrites++
			f.light = "on"
			value = map[string]bool{"success": true}
		case "ha_get_state":
			if rpc.Params.Arguments["entity_id"] != "light.office" {
				f.fail(w, "wrong state target")
				return
			}
			f.counts.HomeReads++
			value = map[string]any{"data": map[string]string{"entity_id": "light.office", "state": f.light}}
		default:
			f.fail(w, "unknown home tool")
			return
		}
		result = map[string]any{"structuredContent": value}
	} else {
		f.fail(w, "unknown MCP method")
		return
	}
	_ = json.MarshalWrite(w, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
}

func sendEvent(ctx context.Context, ws *websocket.Conn, event *server.TServerMessage_TEvent) error {
	data, err := (wire.Frame{Server: &server.TServerMessage{Message: &server.TServerMessage_Event{Event: event}}, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageBinary, data)
}

func (f *fixture) voice(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	pending := map[string]string{}
	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		frame := wire.Decode(data, wire.ClientToServer)
		event := frame.Client.GetEvent()
		id := event.GetHeader().GetMessageId()
		f.mu.Lock()
		switch frame.Kind() {
		case "VoiceInput":
			f.counts.Voice++
			name := strings.Split(id, "-")
			var phrase string
			if len(name) >= 3 {
				phrase = phrases[name[1]]
			}
			if phrase == "" || !event.GetVoiceInput().GetRequest().GetUniproxyOptions().GetWaitClientApproval() {
				f.counts.Unexpected++
				log.Print("FAIL unknown or ungated VoiceInput")
			} else {
				pending[id] = phrase
			}
			f.mu.Unlock()
			if phrase == "" {
				return
			}
			err = sendEvent(r.Context(), ws, &server.TServerMessage_TEvent{
				Header: &header.TEventHeader{MessageId: "mock-asr-" + id, RefMessageId: id},
				Event:  &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), ResponseCode: recognition.EAsrResponseCode_OK.Enum(), Recognition: []*recognition.TAsrRecognition{{Normalized: new(phrase)}}}},
			})
		case "ContinueRequest":
			original := event.GetContinueRequest().GetRequestMessageId()
			phrase := pending[original]
			answer := cloudAnswers[phrase]
			if answer == "" {
				f.counts.Unexpected++
				log.Print("FAIL local request leaked to cloud")
			} else {
				f.counts.Cloud++
			}
			f.mu.Unlock()
			if answer == "" {
				return
			}
			err = sendEvent(r.Context(), ws, &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: "cloud-" + original, RefMessageId: original}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: &response.TAliceResponseProto{VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new(answer)}}}}})
		case "TtsGenerate":
			tts := event.GetTtsGenerate()
			if tts.GetText() == "" || tts.GetLang() != "ru-RU" || tts.GetFormat() != "audio/opus" {
				f.counts.Unexpected++
				log.Print("FAIL invalid synthesis request")
			}
			f.counts.TTS++
			f.mu.Unlock()
		case "CancelRequest":
			delete(pending, event.GetCancelRequest().GetRequestMessageId())
			f.mu.Unlock()
		default:
			f.counts.Unexpected++
			log.Printf("UNEXPECTED_UPSTREAM_EVENT event=%s", frame.Kind())
			f.mu.Unlock()
		}
		if err != nil {
			log.Printf("fixture write: %v", err)
			return
		}
	}
}

func readCounters(ctx context.Context, base string) (counters, error) {
	var value counters
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/stats", nil)
	if err != nil {
		return value, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return value, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return value, fmt.Errorf("stats HTTP %d", resp.StatusCode)
	}
	err = json.UnmarshalRead(resp.Body, &value)
	return value, err
}
