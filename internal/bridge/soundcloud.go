package bridge

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	device "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	push "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/push"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/proto"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

const nativeSoundCloudPrefix = "quasar_soundcloud_"

// Invalid reserved events are still consumed; no provider or cloud sees malformed payloads.
func soundCloudPayloadObject(e *common.TEvent) map[string]any {
	var payload map[string]any
	if p := e.GetPayload(); p != nil {
		if proto.Size(p) > 64<<10 {
			return nil
		}
		payload = p.AsMap()
	} else {
		raw := e.GetPayloadRaw()
		if len(raw) == 0 || len(raw) > 64<<10 || json.Unmarshal(raw, &payload) != nil {
			return nil
		}
	}
	return payload
}

func soundCloudEventPayload(e *common.TEvent) map[string]any {
	payload := soundCloudPayloadObject(e)
	for _, key := range []string{"session_id", "stream_id", "token"} {
		v, ok := payload[key].(string)
		if !ok || len(v) == 0 || len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsFunc(v, unicode.IsControl) {
			return nil
		}
	}
	return payload
}

func (n *Native) soundCloudEvent(req *request.TAliceRequestProto, h *header.TEventHeader) error {
	logSoundCloudCallback(req)
	id := h.GetMessageId()
	if id == "" || len(id) > 256 || strings.TrimSpace(id) != id || strings.ContainsFunc(id, unicode.IsControl) {
		return nil
	}
	scope := "connection:" + n.connectionScope
	if device := req.GetApplication().GetDeviceId(); device != "" {
		scope = "device:" + device
	} else if uuid := req.GetApplication().GetUuid(); uuid != "" {
		scope = "uuid:" + uuid
	}
	ctx, cancel := context.WithTimeout(dialogue.WithSpeaker(n.ctx, scope), 90*time.Second)
	ctx = sound.WithPlaybackState(ctx, nativePlaybackState(req))
	p := &nativeRequest{id: id, header: proto.Clone(h).(*header.TEventHeader), request: proto.Clone(req).(*request.TAliceRequestProto), ctx: ctx, cancel: cancel, local: true, started: true, isText: true, silentEvent: true, route: "soundcloud"}
	p.ctx = sound.WithPlayer(ctx, func(ctx context.Context, r sound.Request) error { return n.queueSound(ctx, p, r) })
	n.mu.Lock()
	if n.requests[id] != nil || len(n.requests) >= 128 || n.ctx.Err() != nil {
		n.mu.Unlock()
		cancel()
		return nil
	}
	p.expires = time.AfterFunc(90*time.Second, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		p.canceled = true
		p.cancel()
		if n.requests[id] == p {
			delete(n.requests, id)
		}
	})
	n.requests[id] = p
	n.mu.Unlock()
	payload := soundCloudEventPayload(req.GetRequest().GetEvent())
	n.wg.Go(func() {
		if handler, ok := n.resolver.(interface {
			HandleEvent(context.Context, string, map[string]any) (bool, error)
		}); ok && payload != nil {
			handled, err := handler.HandleEvent(p.ctx, req.GetRequest().GetEvent().GetName(), payload)
			if err != nil || !handled {
				n.discardSound(p)
			}
		}
		if p.ctx.Err() != nil {
			return
		}
		_ = n.reply(p, "")
	})
	return nil
}

// Complete lifecycle requests silently. Station may ignore their AliceResponse
// directives, so next-track audio uses a separate unsolicited Push in the same
// per-speaker dispatch batch, revalidated after the response write.
func (n *Native) replySoundCloudEvent(p *nativeRequest) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if err := n.ack(p); err != nil {
		return err
	}
	requestID := p.request.GetHeader().GetRequestId()
	if requestID == "" {
		requestID = p.id
	}
	answer := &response.TAliceResponseProto{
		Header:        &response.TAliceResponseProto_THeader{RequestId: new(requestID), ResponseId: new(newID()), DialogId: new(p.request.GetHeader().GetDialogId()), SequenceNumber: new(p.request.GetHeader().GetSequenceNumber())},
		VoiceResponse: &response.TAliceResponseProto_TVoiceResponse{},
		Response:      &response.TAliceResponseProto_TResponse{},
	}
	b, err := (wire.Frame{Server: &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID(), RefMessageId: p.id}, Event: &server.TServerMessage_TEvent_AliceResponse{AliceResponse: answer}}}}, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	err = n.down(p.ctx, websocket.MessageBinary, b)
	slog.Info("native_soundcloud_callback_response", "directives", 0, "sent", err == nil)
	if err != nil {
		return err
	}
	// Both ACK and empty response can block. Validate only after these writes;
	// the caller's dispatch gate prevents a newer music batch overtaking the Push.
	directives := n.takeSound(p)
	if len(directives) == 0 {
		return nil
	}
	message := &server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{MessageId: newID()}, Event: &server.TServerMessage_TEvent_Push{Push: &push.TPush{PushIds: []string{newID()}, Directives: directives}}}}}
	b, err = (wire.Frame{Server: message, Prefix: []byte("AAPI")}).Marshal()
	if err != nil {
		return err
	}
	err = n.down(p.ctx, websocket.MessageBinary, b)
	slog.Info("native_soundcloud_callback_push", "directives", len(directives), "sent", err == nil)
	return err
}

// Log structure and bounded numeric player state only, never opaque callback data.
func logSoundCloudCallback(req *request.TAliceRequestProto) {
	event := req.GetRequest().GetEvent()
	action := strings.TrimPrefix(event.GetName(), nativeSoundCloudPrefix)
	switch action {
	case "started", "stopped", "finished", "failed":
	default:
		action = "unknown"
	}
	payload := soundCloudPayloadObject(event)
	keys := make([]string, 0, min(len(payload), 24))
	for key := range payload {
		if len(key) == 0 || len(key) > 40 {
			continue
		}
		valid := true
		for _, r := range key {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '@') {
				valid = false
				break
			}
		}
		if valid {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	keys = keys[:min(len(keys), 24)]
	state := req.GetRequest().GetDeviceState()
	if state == nil {
		raw := req.GetRequest().GetDeviceStateRaw()
		if len(raw) > 0 && len(raw) <= 1<<20 {
			state = new(device.TDeviceState)
			if proto.Unmarshal(raw, state) != nil {
				state = nil
			}
		}
	}
	player := state.GetAudioPlayer()
	playerState := "Unknown"
	if name, ok := device.TDeviceState_TAudioPlayer_TPlayerState_name[int32(player.GetPlayerState())]; ok {
		playerState = name
	}
	scopeKind := "connection"
	if req.GetApplication().GetDeviceId() != "" {
		scopeKind = "device"
	} else if req.GetApplication().GetUuid() != "" {
		scopeKind = "uuid"
	}
	slog.Info("native_soundcloud_callback", "scope_kind", scopeKind, "action", action, "ignore_answer", event.GetIgnoreAnswer(), "payload_keys", keys, "payload_valid", soundCloudEventPayload(event) != nil, "state_present", state != nil, "player_state", playerState, "has_stream", player.GetCurrentlyPlaying().GetStreamId() != "", "offset_known", player != nil && player.OffsetMs != nil, "offset_ms", max(0, player.GetOffsetMs()), "duration_known", player != nil && player.DurationMs != nil, "duration_ms", max(0, player.GetDurationMs()))
}
