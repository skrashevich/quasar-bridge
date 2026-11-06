package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"unicode"
	"unicode/utf8"

	common "github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	"google.golang.org/protobuf/types/known/structpb"
	"quasarbridge/internal/sound"
)

const (
	nativeMusicCallbackPrefix     = "music_thin_client_on_"
	nativeMusicGetNext            = "@@mm_stack_engine_get_next"
	nativeMusicRecovery           = "music_thin_client_recovery"
	nativeSoundCloudContextFamily = "quasar-bridge/soundcloud/"
	nativeSoundCloudContextPrefix = nativeSoundCloudContextFamily + "v1."
)

type soundCloudProtocolIdentity struct {
	SessionID string `json:"session_id"`
	StreamID  string `json:"stream_id"`
	Token     string `json:"token"`
}

type soundCloudProtocolEvent struct {
	owned, prefetch, warmUp bool
	name                    string
	payload                 map[string]any
}

func validSoundCloudIdentityField(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func soundCloudNativeContext(request sound.Request) string {
	b, _ := json.Marshal(soundCloudProtocolIdentity{request.SessionID, request.StreamID, request.CallbackToken})
	return nativeSoundCloudContextPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeSoundCloudNativeContext(value string) map[string]any {
	if len(value) > 2048 || !strings.HasPrefix(value, nativeSoundCloudContextPrefix) {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, nativeSoundCloudContextPrefix))
	if err != nil {
		return nil
	}
	var identity soundCloudProtocolIdentity
	if json.Unmarshal(b, &identity) != nil || !validSoundCloudIdentityField(identity.SessionID) || !validSoundCloudIdentityField(identity.StreamID) || !validSoundCloudIdentityField(identity.Token) {
		return nil
	}
	return map[string]any{"session_id": identity.SessionID, "stream_id": identity.StreamID, "token": identity.Token}
}

// The client roundtrips this server-owned context. It contains only Bridge's
// bounded queue identity, never a Yandex account, token, or opaque cloud context.
func makeSoundCloudCallbacks(request sound.Request) map[string]any {
	callbacks := make(map[string]any, 4)
	for _, action := range []string{"started", "stopped", "finished", "failed"} {
		play := map[string]any{"context": soundCloudNativeContext(request), "contextItem": request.TrackID, "playId": request.StreamID, "trackId": request.TrackID, "aliceRequestId": request.StreamID}
		if action == "stopped" {
			play["trackLengthSeconds"] = float64(request.DurationMS) / 1000
		}
		callbacks["on_"+action] = map[string]any{"name": nativeMusicCallbackPrefix + action, "type": "server_action", "is_led_silent": true, "ignore_answer": true, "payload": map[string]any{"@scenario_name": "HollywoodMusic", "@request_id": request.StreamID, "callback_flow_type": "Old", "events": []any{map[string]any{"playAudioEvent": play}}}}
	}
	return callbacks
}

func makeSoundCloudContinuation(request sound.Request) (*soundDirective, error) {
	marker := soundCloudNativeContext(request)
	recovery := map[string]any{"name": nativeMusicRecovery, "type": "server_action", "ignore_answer": false, "is_led_silent": true, "payload": map[string]any{"@scenario_name": "HollywoodMusic", "@request_id": request.StreamID, "playback_context": map[string]any{"context": marker}}}
	payload, err := structpb.NewStruct(map[string]any{"@scenario_name": "HollywoodMusic", "@request_id": request.StreamID, "stack_product_scenario_name": "music", "stack_session_id": marker, "@recovery_params": map[string]any{}, "@recovery_callback": recovery})
	if err != nil {
		return nil, err
	}
	return &soundDirective{Type: "server_action", Name: nativeMusicGetNext, AnalyticsType: nativeMusicGetNext, IsLedSilent: true, PayloadType: &directives.TDirective_Payload{Payload: payload}}, nil
}

// Ownership scanning does not build an unbounded JSON object. It also runs
// when the payload is too large to accept, so its own token cannot go upstream.
func nativeMusicOwnMarker(event *common.TEvent) bool {
	if raw := event.GetPayloadRaw(); len(raw) != 0 {
		if bytes.Contains(raw, []byte(nativeSoundCloudContextFamily)) {
			return true
		}
		decoder := jsontext.NewDecoder(bytes.NewReader(raw))
		for {
			token, err := decoder.ReadToken()
			if err != nil {
				return false
			}
			if token.Kind() == '"' && strings.HasPrefix(token.String(), nativeSoundCloudContextFamily) {
				return true
			}
		}
	}
	p := event.GetPayload()
	if strings.HasPrefix(p.GetFields()["stack_session_id"].GetStringValue(), nativeSoundCloudContextFamily) {
		return true
	}
	if strings.HasPrefix(p.GetFields()["playback_context"].GetStructValue().GetFields()["context"].GetStringValue(), nativeSoundCloudContextFamily) {
		return true
	}
	for _, item := range p.GetFields()["events"].GetListValue().GetValues() {
		context := item.GetStructValue().GetFields()["playAudioEvent"].GetStructValue().GetFields()["context"].GetStringValue()
		if strings.HasPrefix(context, nativeSoundCloudContextFamily) {
			return true
		}
	}
	return false
}

func decodeSoundCloudProtocolEvent(event *common.TEvent) soundCloudProtocolEvent {
	name := event.GetName()
	if name != nativeMusicGetNext && !strings.HasPrefix(name, "music_thin_client_") {
		return soundCloudProtocolEvent{}
	}
	payload := soundCloudPayloadObject(event)
	if !nativeMusicOwnMarker(event) {
		return soundCloudProtocolEvent{}
	}
	result := soundCloudProtocolEvent{owned: true, prefetch: name == nativeMusicGetNext || name == nativeMusicRecovery}
	if payload == nil {
		return result
	}
	if result.prefetch {
		marker, _ := payload["stack_session_id"].(string)
		result.payload = decodeSoundCloudNativeContext(marker)
		result.warmUp = name == nativeMusicGetNext && event.GetIsWarmUp()
		return result
	}
	action := strings.TrimPrefix(name, nativeMusicCallbackPrefix)
	switch action {
	case "started", "stopped", "finished", "failed":
	default:
		return result
	}
	events, _ := payload["events"].([]any)
	if len(events) != 1 {
		return result
	}
	item, _ := events[0].(map[string]any)
	play, _ := item["playAudioEvent"].(map[string]any)
	marker, _ := play["context"].(string)
	identity := decodeSoundCloudNativeContext(marker)
	if identity == nil || play["playId"] != identity["stream_id"] {
		return result
	}
	result.name, result.payload = name, identity
	return result
}

// Warm-up is a correlated native queue request, not a conversation or an
// immediate player mutation. Only the final response may stage its next track.
func (e *Engine) PrefetchSoundCloud(ctx context.Context, sessionID, streamID, token string) (bool, error) {
	provider, ok := e.SoundCloud.(interface {
		Prefetch(context.Context, string, string, string) (bool, error)
	})
	if !ok || ctx.Err() != nil || !e.soundCloudControlAvailable(ctx) {
		return true, nil
	}
	return provider.Prefetch(ctx, sessionID, streamID, token)
}

func (e *Engine) SoundCloudPrefetchDelivered(ctx context.Context, request sound.Request) {
	if provider, ok := e.SoundCloud.(interface {
		PrefetchDelivered(context.Context, sound.Request)
	}); ok {
		provider.PrefetchDelivered(ctx, request)
	}
}
