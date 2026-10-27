package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	cancellation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	continuation "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/continue_request"
	ack "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/input_start_ack"
	stream "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/stream_control"
	tts "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_generate"
	ttsSpeak "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/tts_speak"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

type Resolution struct {
	Route  string
	Answer string
}
type Resolver interface {
	Resolve(context.Context, string) (Resolution, error)
}
type RoutedResolver interface {
	Decide(context.Context, string) RouteDecision
	ResolveDecision(context.Context, string, RouteDecision) (Resolution, error)
}
type FallbackResolver interface {
	Fallback(context.Context, string) (Resolution, error)
}

type nativeRequest struct {
	streaming                       bool
	listenOnStreamStart             bool
	partial                         uint32
	responseID                      string
	final                           bool
	header                          *header.TEventHeader
	isText                          bool
	ttsStarted                      bool
	gated                           bool
	input                           []byte
	id                              string
	request                         *request.TAliceRequestProto
	text                            string
	route                           string
	ctx                             context.Context
	cancel                          context.CancelFunc
	started, canceled, local, cloud bool
	continuation                    []byte
	ttsID                           string
	progressTTS                     string
	audio                           oggSpeech
	audioIsProgress                 bool
	audioStarted                    bool
	audioStream                     uint32
	audioWaiting                    bool
	progressAudioDone               chan struct{}
	soundRequest                    *sound.Request
	soundDirective                  *soundDirective
	soundSealed                     bool
	expires                         *time.Timer
}
type Native struct {
	connectionScope  string
	ctx              context.Context
	cancel           context.CancelFunc
	up, down         Sender
	resolver         Resolver
	mu               sync.Mutex
	requests         map[string]*nativeRequest
	tts              map[string]*nativeRequest
	streams          map[uint32]*nativeRequest
	blocked          map[uint32]*nativeRequest
	wg               sync.WaitGroup
	OnRoute          func(id, route string)
	GateText         bool
	ShouldListen     bool
	DecisionDelay    time.Duration
	ProgressAfter    time.Duration
	ProgressInterval time.Duration
	Speech           func(context.Context, string) ([]byte, error)
}

func NewNative(ctx context.Context, up, down Sender, resolver Resolver) *Native {
	ctx, cancel := context.WithCancel(ctx)
	return &Native{connectionScope: newID(), ctx: ctx, cancel: cancel, up: up, down: down, resolver: resolver, ShouldListen: true, requests: map[string]*nativeRequest{}, tts: map[string]*nativeRequest{}, streams: map[uint32]*nativeRequest{}, blocked: map[uint32]*nativeRequest{}}
}
func (n *Native) Close() {
	n.cancel()
	n.mu.Lock()
	for _, p := range n.requests {
		p.canceled = true
		p.cancel()
		p.expires.Stop()
	}
	n.mu.Unlock()
	n.wg.Wait()
}
func (n *Native) Client(ctx context.Context, k websocket.MessageType, data []byte) error {
	f := wire.Decode(data, wire.ClientToServer)
	if f.Client == nil || f.Client.GetEvent() == nil {
		return n.up(ctx, k, data)
	}
	e := f.Client.GetEvent()
	if stat := e.GetRequestStat(); stat != nil {
		kind := "none"
		raw := strings.ToLower(stat.GetError())
		if raw != "" {
			kind = "other"
			if strings.Contains(raw, "timeout") || strings.Contains(raw, "timed out") || strings.Contains(raw, "deadline") {
				kind = "timeout"
			} else if strings.Contains(raw, "cancel") {
				kind = "canceled"
			}
		}
		timestamps := stat.GetTimestamps()
		reported := func(value string) bool { return value != "" && value != "0" }
		slog.Info("native_playback_status", "request", requestTag(stat.GetRequestMessageId()), "error_class", kind, "playback_reported", reported(timestamps.GetOnSoundPlayerBeginTime()) || reported(timestamps.GetOnPlayerBegin()), "finished_reported", reported(timestamps.GetOnSoundPlayerEndTime()) || reported(timestamps.GetOnPlayerEnd()))
	}
	var req *request.TAliceRequestProto
	switch f.Kind() {
	case "TextInput":
		req = e.GetTextInput()
	case "VoiceInput":
		req = e.GetVoiceInput()
	case "SpeechInput":
		req = e.GetSpeechInput()
	}
	if req != nil && f.Kind() == "TextInput" && strings.TrimSpace(req.GetRequest().GetEvent().GetText()) == "" {
		return n.up(ctx, k, data)
	}
	if req != nil {
		id := e.GetHeader().GetMessageId()
		if id == "" {
			return n.up(ctx, k, data)
		}
		scope := "connection:" + n.connectionScope
		if id := req.GetApplication().GetDeviceId(); id != "" {
			scope = "device:" + id
		} else if id := req.GetApplication().GetUuid(); id != "" {
			scope = "uuid:" + id
		}
		pctx, cancel := context.WithTimeout(dialogue.WithSpeaker(n.ctx, scope), 90*time.Second)
		playerState := nativePlaybackState(req)
		pctx = sound.WithPlaybackState(pctx, playerState)
		slog.Info("native_player_state", "request", requestTag(id), "present", playerState.Present, "player_state", playerState.PlayerState, "offset_known", playerState.OffsetKnown, "offset_ms", playerState.OffsetMS, "has_stream", playerState.StreamID != "", "stream_tag", requestTag(playerState.StreamID))
		p := &nativeRequest{id: id, request: proto.Clone(req).(*request.TAliceRequestProto), text: req.GetRequest().GetEvent().GetText(), ctx: pctx, cancel: cancel}
		p.ctx = sound.WithPlayer(p.ctx, func(ctx context.Context, request sound.Request) error {
			return n.queueSound(ctx, p, request)
		})
		p.header = proto.Clone(e.GetHeader()).(*header.TEventHeader)
		p.isText = f.Kind() == "TextInput"
		p.input = append([]byte{}, data...)
		p.gated = !p.isText || n.GateText

		slog.Info("native_request_shape", "request", requestTag(id), "text_bytes", len(p.text), "event_type", req.GetRequest().GetEvent().GetType().String(), "stream", e.GetHeader().GetStreamId(), "unknown_request_bytes", len(req.ProtoReflect().GetUnknown()), "payload_bytes", len(req.GetRequest().GetEvent().GetPayloadRaw()))
		n.mu.Lock()
		if n.requests[id] != nil {
			n.mu.Unlock()
			cancel()
			return nil
		}
		if len(n.requests) >= 128 {
			n.mu.Unlock()
			cancel()
			return errors.New("too many pending native requests")
		}
		if preparer, ok := n.resolver.(interface {
			Prepare(context.Context) context.Context
		}); ok {
			p.ctx = preparer.Prepare(p.ctx)
		}
		p.expires = time.AfterFunc(90*time.Second, func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			p.canceled = true
			p.cancel()
			delete(n.requests, p.id)
			delete(n.tts, p.ttsID)
			delete(n.tts, p.progressTTS)
			for stream, q := range n.blocked {
				if q == p {
					delete(n.blocked, stream)
				}
			}
			for stream, q := range n.streams {
				if q == p {
					delete(n.streams, stream)
				}
			}
		})
		n.requests[id] = p
		n.mu.Unlock()
		if p.gated {
			gated, err := wire.EnableApproval(f)
			if err != nil {
				return err
			}
			if err := n.up(ctx, k, gated); err != nil {
				return err
			}
		}
		// The original input stays local until routing has selected exactly one executor.
		n.maybeResolve(p)
		return nil
	}
	if c := e.GetContinueRequest(); c != nil {
		n.mu.Lock()
		p := n.requests[c.GetRequestMessageId()]
		if p != nil && (p.canceled || p.local) {
			n.mu.Unlock()
			return nil
		}
		if p != nil && !p.cloud {
			if p.continuation != nil {
				n.mu.Unlock()
				return nil
			}
			p.continuation = append([]byte{}, data...)
			n.mu.Unlock()
			n.maybeResolve(p)
			return nil
		}
		n.mu.Unlock()
	}
	if c := e.GetCancelRequest(); c != nil {
		n.mu.Lock()
		p := n.requests[c.GetRequestMessageId()]
		ttsID := ""
		if p != nil {
			p.canceled = true
			p.cancel()
			ttsID = p.ttsID
		}
		n.mu.Unlock()
		if ttsID != "" {
			_ = n.sendCancel(ctx, ttsID)
		}
	}
	return n.up(ctx, k, data)
}
func (n *Native) findLocked(h *header.TEventHeader) (*nativeRequest, bool) {
	if h == nil {
		return nil, false
	}
	for _, id := range []string{h.GetRefMessageId(), h.GetSubRequestRef().GetMessageId(), h.GetSubRequestRef().GetRequestId()} {
		if p := n.tts[id]; p != nil {
			return p, true
		}
		if p := n.requests[id]; p != nil {
			return p, false
		}
		if id != "" {
			for _, p := range n.requests {
				if p.request.GetHeader().GetRequestId() == id {
					return p, false
				}
			}
		}
	}
	return nil, false
}
func (n *Native) Server(ctx context.Context, k websocket.MessageType, data []byte) error {
	f := wire.Decode(data, wire.ServerToClient)
	if f.Server == nil {
		if len(data) >= 4 {
			n.mu.Lock()
			p := n.streams[binary.BigEndian.Uint32(data)]
			drop := n.blocked[binary.BigEndian.Uint32(data)] != nil || (p != nil && p.canceled)
			if p != nil && p.streaming && !drop {
				out, err := p.audio.feed(data[4:])
				if err == nil && len(out) > 0 {
					err = n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), out...))
				}
				if err != nil {
					p.cancel()
				}
				n.mu.Unlock()
				return err
			}
			n.mu.Unlock()
			if drop {
				return nil
			}
		}
		return n.down(ctx, k, data)
	}
	if sc := f.Server.GetStreamControl(); sc != nil {
		n.mu.Lock()
		if n.blocked[uint32(sc.GetStreamId())] != nil {
			delete(n.blocked, uint32(sc.GetStreamId()))
			n.mu.Unlock()
			return nil
		}
		p := n.tts[sc.GetMessageId()]
		if p == nil {
			p = n.streams[uint32(sc.GetStreamId())]
		}
		if sc.GetClose() != nil {
			delete(n.streams, uint32(sc.GetStreamId()))
		}
		original := n.requests[sc.GetMessageId()]
		if p != nil {
			if p.canceled {
				n.mu.Unlock()
				return nil
			}
			if p.streaming && sc.GetClose() != nil {
				if err := p.audio.endSource(); err != nil {
					p.cancel()
					n.mu.Unlock()
					return err
				}
				if p.audioIsProgress && sc.GetClose().GetReason() == 0 {
					if tail := p.audio.flushPending(0, 0); len(tail) > 0 {
						if err := n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), tail...)); err != nil {
							n.mu.Unlock()
							return err
						}
					}
					if !p.audioWaiting {
						p.audioWaiting = true
						n.waitAudio(p)
						close(p.progressAudioDone)
					}
					n.mu.Unlock()
					return nil
				}
				end := p.audio.finish()
				if err := n.down(ctx, k, append(binary.BigEndian.AppendUint32(nil, p.audioStream), end...)); err != nil {
					n.mu.Unlock()
					return err
				}
				sc.StreamId = int64(p.audioStream)
			}
			sc.MessageId = p.id
			n.mu.Unlock()
			out, err := f.Marshal()
			if err != nil {
				return err
			}
			return n.down(ctx, k, out)
		}
		drop := original != nil && original.local && (original.header.StreamId == nil || sc.GetStreamId() != original.header.GetStreamId())
		n.mu.Unlock()
		if drop {
			return nil
		}
		return n.down(ctx, k, data)
	}
	e := f.Server.GetEvent()
	if e == nil {
		return n.down(ctx, k, data)
	}

	n.mu.Lock()
	if f.Kind() == "TtsSpeak" {
		delete(n.streams, uint32(e.GetHeader().GetStreamId()))
		delete(n.blocked, uint32(e.GetHeader().GetStreamId()))
	}
	p, isTTS := n.findLocked(e.GetHeader())
	if r := e.GetAliceResponse(); r != nil {
		var ds []string
		for _, d := range r.GetResponse().GetDirectives() {
			ds = append(ds, d.GetType()+":"+d.GetName())
		}

		slog.Info("native_cloud_shape", "matched", p != nil, "directives", ds, "cards", len(r.GetResponse().GetCards()), "voice_text_bytes", len(r.GetVoiceResponse().GetOutputSpeech().GetText()))
	}
	if p == nil {
		n.mu.Unlock()
		return n.down(ctx, k, data)
	}
	if isTTS {
		if p.canceled {
			n.mu.Unlock()
			return nil
		}
		e.Header.RefMessageId = p.id
		if ref := e.Header.SubRequestRef; ref != nil {
			ref.MessageId = p.id
			ref.RequestId = p.request.GetHeader().GetRequestId()
		}
		if e.GetTtsSpeak() != nil {
			n.streams[uint32(e.Header.GetStreamId())] = p
			if p.streaming {
				if e.GetTtsSpeak().GetFormat() != "audio/opus" {
					p.cancel()
					n.mu.Unlock()
					return errors.New("streaming progress requires Opus")
				}
				p.audioIsProgress = p.ttsID == p.progressTTS
				p.audio.next()
				p.audio.source.discardAudio = p.audioIsProgress
				if p.audioStarted {
					p.audioWaiting = false
					n.mu.Unlock()
					return nil
				}
				p.audioStream = uint32(e.Header.GetStreamId())
				p.audioStarted = true
			}
			slog.Info("native_tts_ready", "request", requestTag(p.id), "format", e.GetTtsSpeak().GetFormat(), "stream", e.Header.GetStreamId())
		}
		n.mu.Unlock()
		out, err := f.Marshal()
		if err != nil {
			return err
		}
		return n.down(ctx, k, out)
	}

	if f.Kind() == "TtsSpeak" {
		p.ttsStarted = true
		if p.local || p.canceled {
			n.blocked[uint32(e.GetHeader().GetStreamId())] = p
		}
	}
	if p.local || p.canceled {
		n.mu.Unlock()
		return nil
	}
	if r := e.GetAliceResponse(); r != nil && p.cloud && !p.ttsStarted && canReplaceResponse(r) {
		text := ""
		for _, card := range r.GetResponse().GetCards() {
			text += card.GetText() + " "
		}
		if strings.TrimSpace(text) == "" {
			text = r.GetVoiceResponse().GetOutputSpeech().GetText()
		}

		if resolver, ok := n.resolver.(FallbackResolver); ok && n.isRefusal(text) {
			p.local = true
			if n.OnRoute != nil {
				n.OnRoute(p.id, "llm")
			}
			p.ttsID = newID()
			n.tts[p.ttsID] = p
			query := p.text
			n.mu.Unlock()
			n.wg.Go(func() {
				result, err := resolver.Fallback(p.ctx, query)
				if p.ctx.Err() != nil {
					return
				}
				if errors.Is(err, context.Canceled) {
					n.discardStale(p)
					return
				}
				if err != nil {
					n.discardSound(p)
					result.Answer = "Сейчас не получилось получить ответ локальной модели."
				}
				_ = n.sendCancel(p.ctx, p.id)
				if err := n.reply(p, result.Answer); err != nil {
					slog.Warn("native_reply_failed", "request", requestTag(p.id))
				}
				slog.Info("native_fallback", "request", requestTag(p.id), "route", "llm")
			})
			return nil
		}
	}
	switch f.Kind() {
	case "AliceResponse":
		if !p.cloud {
			p.canceled = true
			p.cancel()
			slog.Warn("native_approval_not_honored", "request", requestTag(p.id))
		}
	case "ServerRequestReady":
		slog.Info("native_server_ready", "request", requestTag(p.id))
	case "AsrResult":
		asr := e.GetAsrResult()
		if asr.GetEndOfUtt() {
			p.final = true
			slog.Info("native_asr_final", "request", requestTag(p.id), "hypotheses", len(asr.GetRecognition()), "code", asr.GetResponseCode().String())
		}
		if asr.GetEndOfUtt() && len(asr.GetRecognition()) > 0 {
			r := asr.GetRecognition()[0]
			p.text = r.GetNormalized()
			if p.text == "" {
				var words []string
				for _, w := range r.GetWords() {
					words = append(words, w.GetValue())
				}
				p.text = strings.Join(words, " ")
			}
		}

	}
	n.mu.Unlock()
	n.maybeResolve(p)
	if err := n.down(ctx, k, data); err != nil {
		return err
	}
	n.captureCloud(p, e)
	return nil
}
func (n *Native) maybeResolve(p *nativeRequest) {
	n.mu.Lock()
	if p.started || p.canceled || (strings.TrimSpace(p.text) == "" && !p.final) {
		n.mu.Unlock()
		return
	}
	p.started = true
	text := p.text
	n.mu.Unlock()
	n.wg.Go(func() {
		if n.DecisionDelay > 0 {
			select {
			case <-time.After(n.DecisionDelay):
			case <-p.ctx.Done():
				return
			}
		}
		result := Resolution{Route: "yandex"}
		var err error
		if n.resolver != nil && strings.TrimSpace(text) != "" {
			if routed, ok := n.resolver.(RoutedResolver); ok {
				decision := routed.Decide(p.ctx, text)
				n.mu.Lock()
				p.listenOnStreamStart = decision.Source == "dialogue_retry"
				n.mu.Unlock()
				slog.Info("native_route_decision", "request", requestTag(p.id), "route", decision.Route, "source", decision.Source)
				stop := func() {}
				if decision.Route != "yandex" && n.ProgressAfter > 0 {
					stop = n.progress(p)
				}
				result, err = routed.ResolveDecision(p.ctx, text, decision)
				stop()
			} else {
				result, err = n.resolver.Resolve(p.ctx, text)
			}
		}
		if p.ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			n.discardStale(p)
			return
		}
		if err != nil {
			n.discardSound(p)
			if result.Route == "home" {
				result.Answer = "Не удалось подтвердить выполнение команды в умном доме."
			} else if result.Route == "music" {
				result.Answer = "Не удалось включить музыку с этой страницы."
			} else {
				result.Route = "llm"
				result.Answer = "Сейчас не получилось получить ответ локальной модели."
			}
		}
		n.mu.Lock()
		if p.canceled {
			n.mu.Unlock()
			return
		}
		if n.OnRoute != nil {
			n.OnRoute(p.id, result.Route)
		}
		p.route = result.Route
		if result.Route == "yandex" {
			p.cloud = true
			var err error
			if p.gated {
				control := p.continuation
				if control == nil {
					control, err = n.makeContinue(p)
				}
				if err == nil {
					err = n.up(p.ctx, websocket.MessageBinary, control)
				}
			} else {
				err = n.up(p.ctx, websocket.MessageBinary, p.input)
				if err == nil && p.continuation != nil {
					err = n.up(p.ctx, websocket.MessageBinary, p.continuation)
				}
			}
			n.mu.Unlock()
			slog.Info("native_route", "request", requestTag(p.id), "route", "yandex", "sent", err == nil)
			return
		}
		p.local = true
		if !p.streaming {
			p.ttsID = newID()
			n.tts[p.ttsID] = p
		}
		n.mu.Unlock()
		slog.Info("native_route", "request", requestTag(p.id), "route", result.Route)
		if p.gated && !p.streaming {
			if err := n.sendCancel(p.ctx, p.id); err != nil {
				return
			}
		}
		if err := n.reply(p, result.Answer); err != nil {
			slog.Warn("native_reply_failed", "request", requestTag(p.id))
		}
	})
}
func (n *Native) sendCancel(ctx context.Context, id string) error {
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: newID()}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: id, Reason: &cancellation.TCancelRequest_LocalExecution{LocalExecution: &cancellation.TCancelRequest_TReasonLocalExecution{}}}}}}}
	b, err := (wire.Frame{Client: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.up(ctx, websocket.MessageBinary, b)
}
func (n *Native) reply(p *nativeRequest, text string) error {
	if p.streaming {
		return n.finishDeferred(p, text)
	}
	requestID := p.request.GetHeader().GetRequestId()
	if requestID == "" {
		requestID = p.id
	}
	answer := &response.TAliceResponseProto{Header: &response.TAliceResponseProto_THeader{RequestId: new(requestID), ResponseId: new(newID()), DialogId: new(p.request.GetHeader().GetDialogId()), SequenceNumber: new(p.request.GetHeader().GetSequenceNumber())}, VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{OutputSpeech: &response.TAliceResponseProto_TVoiceResponse_TOutputSpeech{Text: new(text)}, HasVoiceResponse: true}, Response: &response.TAliceResponseProto_TResponse{Cards: []*response.TAliceResponseProto_TResponse_TCard{{Type: "simple_text", Text: new(text), CardId: new(newID())}}}}
	silentPlayback := n.hasSilentPlayback(p)
	answer.VoiceResponse.ShouldListen = n.ShouldListen && !silentPlayback
	if silentPlayback {
		answer.VoiceResponse.HasVoiceResponse = false
		answer.VoiceResponse.OutputSpeech.Text = new("")
	}
	answer.Response.Directives = n.takeSound(p)
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: answer}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if !p.gated && !p.cloud {
		if err := n.ack(p); err != nil {
			return err
		}
	}
	if err := n.down(p.ctx, websocket.MessageBinary, b); err != nil {
		return err
	}
	n.captureDelivered(p, text, true, silentPlayback || len(answer.Response.Directives) > 0, true)
	if silentPlayback {
		return nil
	}
	return n.synthesize(p, text)
}
func (n *Native) synthesize(p *nativeRequest, text string) error {
	if n.Speech != nil && p.route == "llm" {
		ogg, err := n.Speech(p.ctx, text)
		if err == nil {
			if err = n.speakAudio(p, ogg); err == nil {
				return nil
			}
		}
		slog.Warn("gemini_tts_failed_using_yandex_voice", "request", requestTag(p.id), "error", err)
	}
	return n.synthesizeYandex(p, text)
}

func (n *Native) synthesizeYandex(p *nativeRequest, text string) error {
	t := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: p.ttsID}, Event: &client.TClientMessage_TEvent_TtsGenerate{TtsGenerate: &tts.TTtsGenerate{Text: new(text), Lang: new("ru-RU"), Format: new("audio/opus")}}}}}
	b, err := (wire.Frame{Client: t, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.up(p.ctx, websocket.MessageBinary, b)
}

// speakAudio injects the Gemini Opus result through the same Station event path
// as Yandex TTS, so existing stream handling, progress audio, and interruption
// behavior remain in force.
func (n *Native) speakAudio(p *nativeRequest, ogg []byte) error {
	if len(ogg) == 0 || len(ogg) > 16<<20 {
		return errors.New("invalid synthesized audio size")
	}
	streamID := uint32(time.Now().UnixNano())
	if streamID == 0 {
		streamID = 1
	}
	speak := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.ttsID, StreamId: new(int64(streamID))}, Event: &server.TServerMessage_TEvent_TtsSpeak{TtsSpeak: &ttsSpeak.TTtsSpeak{Format: "audio/opus"}}}}}
	b, err := (wire.Frame{Server: speak, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	if err := n.Server(p.ctx, websocket.MessageBinary, b); err != nil {
		return err
	}
	if err := n.Server(p.ctx, websocket.MessageBinary, append(binary.BigEndian.AppendUint32(nil, streamID), ogg...)); err != nil {
		return err
	}
	closeMsg := &server.TServerMessage{Message: &server.TServerMessage_StreamControl{StreamControl: &stream.TStreamControl{MessageId: p.ttsID, StreamId: int64(streamID), Action: &stream.TStreamControl_Close{Close: &stream.TStreamControl_TActionClose{}}}}}
	b, err = (wire.Frame{Server: closeMsg, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.Server(p.ctx, websocket.MessageBinary, b)
}
func requestTag(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum[:4])
}

// Quasar uses Yandex TGUID spelling (8-8-8-8), not RFC UUID spelling.
func newID() string {
	return sound.NewStreamID()
}

func (n *Native) makeContinue(p *nativeRequest) ([]byte, error) {
	h := proto.Clone(p.header).(*header.TEventHeader)
	h.MessageId = newID()
	h.RefMessageId = ""
	h.StreamId = nil
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: h, Event: &client.TClientMessage_TEvent_ContinueRequest{ContinueRequest: &continuation.TContinueRequest{RequestMessageId: p.id}}}}}
	return (wire.Frame{Client: m, Prefix: []byte("AAPI")}).Marshal()
}

func (n *Native) ack(p *nativeRequest) error {
	m := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id}, Event: &server.TServerMessage_TEvent_InputStartAck{InputStartAck: &ack.TInputStartAck{RequestStartTime: uint64(time.Now().UnixMilli())}}}}}
	b, err := (wire.Frame{Server: m, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	return n.down(p.ctx, websocket.MessageBinary, b)
}

func canReplaceResponse(r *response.TAliceResponseProto) bool {
	for _, d := range r.GetResponse().GetDirectives() {
		if d.GetType() != "client_action" || d.GetName() != "tts_play_placeholder" {
			return false
		}
	}
	return true
}

func (n *Native) isRefusal(text string) bool {
	if detector, ok := n.resolver.(interface{ IsRefusal(string) bool }); ok {
		return detector.IsRefusal(text)
	}
	return IsRefusal(text)
}

func (n *Native) discardStale(p *nativeRequest) {
	n.mu.Lock()
	p.canceled = true
	p.cancel()
	n.mu.Unlock()
	_ = n.sendCancel(n.ctx, p.id)
}
