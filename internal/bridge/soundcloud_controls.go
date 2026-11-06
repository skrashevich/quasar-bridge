package bridge

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"

	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

type soundCloudControlContextKey struct{}

type soundCloudControlProvider interface {
	CanControl(context.Context, sound.Control) bool
	HandleControl(context.Context, sound.Control) (string, error)
}

func (e *Engine) soundCloudControlAvailable(ctx context.Context) bool {
	controller, ok := e.AirPlay.(AirPlayMediaController)
	return !ok || !controller.Active(ctx)
}

// Only semantic controls observed from Midi's local PlaybackControlCapability
// are translated. Analytics labels are deliberately not used as commands.
func soundCloudSemanticControl(event *common.TEvent) sound.Control {
	if event.GetName() != "@@mm_semantic_frame" || strings.TrimSpace(event.GetText()) != "" {
		return ""
	}
	raw := event.GetPayloadRaw()
	if len(raw) == 0 || len(raw) > 64<<10 {
		return ""
	}
	var data common.TSemanticFrameRequestData
	if (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(raw, &data) != nil || len(data.ProtoReflect().GetUnknown()) != 0 || data.Origin != nil || data.Params != nil || data.RequestParams != nil || data.OnChainFinish != nil {
		return ""
	}
	frame := data.GetTypedSemanticFrame()
	if frame == nil || !knownSoundCloudSemanticFields(frame.ProtoReflect()) {
		return ""
	}
	if c := frame.GetPlayerContinueSemanticFrame(); c != nil && c.PlayerType == nil {
		return sound.ControlResume
	}
	if c := frame.GetPlayerNextTrackSemanticFrame(); c != nil && !c.GetSetPause().GetBoolValue() {
		return sound.ControlNext
	}
	if c := frame.GetPlayerPrevTrackSemanticFrame(); c != nil && !c.GetSetPause().GetBoolValue() {
		return sound.ControlPrevious
	}
	return ""
}

func knownSoundCloudSemanticFields(message protoreflect.Message) bool {
	if len(message.GetUnknown()) != 0 {
		return false
	}
	known := true
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() || field.IsList() {
			known = false
		} else if field.Message() != nil {
			known = knownSoundCloudSemanticFields(value.Message())
		}
		return known
	})
	return known
}

// Native controls reserve music ordering without starting a conversational turn.
func (e *Engine) PrepareSoundCloudControl(ctx context.Context, control sound.Control) (context.Context, bool) {
	// Reserve arrival order before provider ownership can wait on its state lock.
	// Admission still uses the unsequenced observation so a stale sequence cannot
	// turn the provider's stale-request guard into a claim on foreign playback.
	sequenced := e.sequenceRequest(ctx)
	state := sound.CurrentPlayback(ctx)
	if ctx.Err() != nil || !state.Present || state.StreamID == "" || e.SoundCloud == nil {
		return ctx, false
	}
	provider, ok := e.SoundCloud.(soundCloudControlProvider)
	if !ok {
		return ctx, false
	}
	if !e.soundCloudControlAvailable(ctx) {
		return ctx, false
	}
	if !provider.CanControl(ctx, control) {
		return ctx, false
	}
	ctx = e.prepareComparisonInput(sequenced)
	return context.WithValue(e.ComparisonContext(ctx), soundCloudControlContextKey{}, true), true
}

func (e *Engine) HandleSoundCloudControl(ctx context.Context, control sound.Control) error {
	provider, ok := e.SoundCloud.(soundCloudControlProvider)
	if !ok || ctx.Err() != nil {
		return context.Canceled
	}
	if !e.soundCloudControlAvailable(ctx) {
		return context.Canceled
	}
	_, err := provider.HandleControl(ctx, control)
	return err
}

func (n *Native) soundCloudControl(req *request.TAliceRequestProto, h *header.TEventHeader) (bool, error) {
	command := soundCloudSemanticControl(req.GetRequest().GetEvent())
	handler, ok := n.resolver.(interface {
		PrepareSoundCloudControl(context.Context, sound.Control) (context.Context, bool)
		HandleSoundCloudControl(context.Context, sound.Control) error
	})
	id := h.GetMessageId()
	if !ok || command == "" || id == "" || len(id) > 256 || strings.TrimSpace(id) != id || strings.ContainsFunc(id, unicode.IsControl) {
		return false, nil
	}
	// Admission is serialized separately from the Native state mutex: provider
	// ownership checks must never run while that state mutex is held.
	n.soundControlMu.Lock()
	defer n.soundControlMu.Unlock()
	n.mu.Lock()
	if existing := n.requests[id]; existing != nil {
		own := existing.silentEvent && existing.route == "soundcloud" && existing.request.GetRequest().GetEvent().GetName() == "@@mm_semantic_frame"
		n.mu.Unlock()
		return own, nil
	}
	available := len(n.requests) < 128 && n.ctx.Err() == nil
	n.mu.Unlock()
	if !available {
		return false, nil
	}
	scope := "connection:" + n.connectionScope
	if device := req.GetApplication().GetDeviceId(); device != "" {
		scope = "device:" + device
	} else if uuid := req.GetApplication().GetUuid(); uuid != "" {
		scope = "uuid:" + uuid
	}
	ctx, cancel := context.WithTimeout(dialogue.WithSpeaker(n.ctx, scope), 90*time.Second)
	ctx = sound.WithPlaybackState(ctx, nativePlaybackState(req))
	ctx, owned := handler.PrepareSoundCloudControl(ctx, command)
	if !owned {
		cancel()
		return false, nil
	}
	if ctx.Err() != nil {
		cancel()
		return true, nil
	}
	p := &nativeRequest{id: id, header: proto.Clone(h).(*header.TEventHeader), request: proto.Clone(req).(*request.TAliceRequestProto), ctx: ctx, cancel: cancel, local: true, started: true, isText: true, silentEvent: true, route: "soundcloud"}
	p.ctx = sound.WithPlayer(ctx, func(ctx context.Context, r sound.Request) error { return n.queueSound(ctx, p, r) })
	n.mu.Lock()
	if n.requests[id] != nil || len(n.requests) >= 128 || n.ctx.Err() != nil {
		n.mu.Unlock()
		cancel()
		return true, nil
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
	slog.Info("native_soundcloud_control", "request", requestTag(id), "command", command)
	n.wg.Go(func() {
		if err := handler.HandleSoundCloudControl(p.ctx, command); err != nil {
			n.discardSound(p)
		}
		if p.ctx.Err() == nil {
			_ = n.reply(p, "")
		}
	})
	return true, nil
}
