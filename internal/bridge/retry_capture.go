package bridge

import (
	"context"
	"strings"

	response "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_response"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/encoding/protowire"
)

func (n *Native) captureDelivered(p *nativeRequest, text string, final, action, local bool) {
	recorder, ok := n.resolver.(interface {
		RecordDelivered(context.Context, string, string, bool, bool)
	})
	if !ok {
		return
	}
	n.mu.Lock()
	if p.canceled || p.ctx.Err() != nil || p.local != local || p.soundSuperseded {
		n.mu.Unlock()
		return
	}
	question, ctx := p.text, p.ctx
	action = action || (p.route != "" && p.route != "llm" && p.route != "yandex")
	n.mu.Unlock()
	recorder.RecordDelivered(ctx, question, text, final, action)
}
func cloudResponseText(r *response.TAliceResponseProto) string {
	if text := r.GetVoiceResponse().GetOutputSpeech().GetText(); text != "" {
		return text
	}
	var text []string
	for _, c := range r.GetResponse().GetCards() {
		if c.GetText() != "" {
			text = append(text, c.GetText())
		}
	}
	return strings.Join(text, " ")
}
func (n *Native) captureCloud(p *nativeRequest, e *server.TServerMessage_TEvent) {
	if r := e.GetAliceResponse(); r != nil {
		n.captureDelivered(p, cloudResponseText(r), !r.GetResponse().GetIsStreaming(), !canReplaceResponse(r), false)
	}
	if r := e.GetDeferredAliceResponse(); r != nil {
		base := r.GetBaseResponse()
		if base == nil {
			return
		}
		text := base.GetText()
		if text == "" {
			unknown := base.ProtoReflect().GetUnknown()
			for len(unknown) > 0 {
				num, typ, size := protowire.ConsumeTag(unknown)
				if size < 0 {
					break
				}
				unknown = unknown[size:]
				if num == 2 && typ == protowire.BytesType {
					value, n := protowire.ConsumeString(unknown)
					if n > 0 {
						text = value
					}
					break
				}
				size = protowire.ConsumeFieldValue(num, typ, unknown)
				if size < 0 {
					break
				}
				unknown = unknown[size:]
			}
		}
		action := false
		for _, d := range base.GetDirectives() {
			if d.GetType() != "client_action" || d.GetName() != "tts_play_placeholder" {
				action = true
			}
		}
		n.captureDelivered(p, text, r.GetIsLast(), action, false)
	}
}
