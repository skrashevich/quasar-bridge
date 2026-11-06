package bridge

import (
	"context"
	"errors"
	"net/url"
	"strings"

	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	"github.com/skrashevich/quasar-go-genproto/proto/alice/protos/data/channel"
	"github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint"
	fileplayer "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/audio_file_player"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"quasarbridge/internal/sound"
)

type soundDirective = directives.TDirective

// Queue only on the requesting socket; the final answer owns delivery.
func (n *Native) queueSound(ctx context.Context, p *nativeRequest, request sound.Request) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.canceled || p.ctx.Err() != nil || n.requests[p.id] != p || p.soundSealed {
		return errors.New("speaker request is no longer active")
	}
	if p.soundRequest != nil {
		if *p.soundRequest == request {
			return nil
		}
		return errors.New("only one sound may be queued per speaker request")
	}
	directive, err := makeSoundDirective(request)
	if err != nil {
		return err
	}
	p.soundRequest = new(request)
	p.soundDirective = directive
	return nil
}

func (n *Native) discardSound(p *nativeRequest) {
	n.mu.Lock()
	defer n.mu.Unlock()
	p.soundDirective = nil
	p.soundSealed = true
}

func (n *Native) takeSound(p *nativeRequest) []*directives.TDirective {
	n.mu.Lock()
	p.soundSealed = true
	if p.canceled || p.ctx.Err() != nil || p.soundDirective == nil || p.soundRequest == nil {
		n.mu.Unlock()
		return nil
	}
	directive, request := p.soundDirective, *p.soundRequest
	p.soundDirective = nil
	comparison := p.comparison != nil
	n.mu.Unlock()
	if guard, ok := n.resolver.(interface{ ComparisonCurrent(context.Context) bool }); comparison && ok && !guard.ComparisonCurrent(p.ctx) {
		n.mu.Lock()
		p.soundSuperseded = true
		n.mu.Unlock()
		return nil
	}
	// Provider.Handle holds its own lock while invoking queueSound. Never acquire
	// that lock under Native.mu; this snapshot is immutable after sealing.
	if validator, ok := n.resolver.(interface {
		CanDeliver(context.Context, sound.Request) bool
	}); ok && !validator.CanDeliver(p.ctx, request) {
		n.mu.Lock()
		p.soundSuperseded = true
		n.mu.Unlock()
		return nil
	}
	return []*directives.TDirective{directive}
}

func (n *Native) hasSilentPlayback(p *nativeRequest) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return p.soundDirective != nil && p.soundRequest != nil && (p.soundRequest.URL != "" || p.soundRequest.Command != "")
}

func makeSoundDirective(request sound.Request) (*directives.TDirective, error) {
	if request.Command != "" {
		if (request.Command != "pause" && request.Command != "stop") || request.URL != "" || request.Sound != "" || request.StreamID != "" || request.OffsetMS != 0 {
			return nil, errors.New("invalid playback control")
		}
		payload, _ := structpb.NewStruct(map[string]any{})
		return &directives.TDirective{Type: "client_action", Name: "audio_stop", AnalyticsType: "audio_stop", IsLedSilent: true, PayloadType: &directives.TDirective_Payload{Payload: payload}}, nil
	}
	if request.URL != "" && request.Sound == "" {
		maxOffset := int64(2 * 60 * 60 * 1000)
		if request.Provider == "SoundCloud" && request.DurationMS > 0 {
			maxOffset = min(request.DurationMS, 24*60*60*1000)
		}
		if request.OffsetMS < 0 || request.OffsetMS > maxOffset {
			return nil, errors.New("invalid playback offset")
		}
		u, err := url.Parse(request.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return nil, errors.New("invalid sound URL")
		}
		format, kind := "MP3", "Track"
		if strings.HasSuffix(strings.ToLower(u.Path), ".m3u8") {
			format, kind = "HLS", "FmRadio"
		}
		if request.Format != "" {
			format = request.Format
		}
		if request.Kind != "" {
			kind = request.Kind
		}
		if request.DurationMS < 0 {
			return nil, errors.New("invalid playback duration")
		}
		id := request.StreamID
		if id == "" {
			id = newID()
		}
		stream := map[string]any{"id": id, "url": request.URL, "format": format, "type": kind, "offset_ms": request.OffsetMS}
		if request.DurationMS > 0 {
			stream["duration_ms"] = request.DurationMS
		}
		metadata := map[string]any{"title": request.Title}
		data := map[string]any{"stream": stream, "metadata": metadata, "screen_type": "Default", "set_pause": false}
		if request.Provider == "SoundCloud" {
			if request.SessionID == "" || request.CallbackToken == "" || request.StreamID == "" {
				return nil, errors.New("incomplete SoundCloud session")
			}
			metadata["subtitle"] = request.Subtitle
			metadata["art_image_url"] = request.ArtworkURL
			metadata["hide_progress_bar"] = false
			music := map[string]any{"id": request.TrackID, "type": kind, "description": request.PermalinkURL, "repeat_mode": request.RepeatMode, "shuffled": request.Shuffle}
			if request.PreviousTrackID != "" {
				music["prev_track_info"] = map[string]any{"id": request.PreviousTrackID, "type": kind}
			}
			if request.NextTrackID != "" {
				music["next_track_info"] = map[string]any{"id": request.NextTrackID, "type": kind}
			}
			metadata["glagol_metadata"] = map[string]any{"music_metadata": music}
			data["screen_type"] = "Music"
			data["provider_name"] = "SoundCloud"
			data["scenario_meta"] = map[string]any{"@scenario_name": "SoundCloud", "owner": "quasar_bridge", "content_id": request.TrackID, "session_id": request.SessionID}
			callbacks := make(map[string]any, 4)
			for _, name := range []string{"started", "stopped", "finished", "failed"} {
				callbacks["on_"+name] = map[string]any{"name": nativeSoundCloudPrefix + name, "type": "server_action", "is_led_silent": true, "ignore_answer": false, "payload": map[string]any{"session_id": request.SessionID, "stream_id": id, "token": request.CallbackToken}}
			}
			data["callbacks"] = callbacks
		}
		payload, err := structpb.NewStruct(data)
		if err != nil {
			return nil, errors.New("cannot encode sound directive")
		}
		return &directives.TDirective{Type: "client_action", Name: "audio_play", AnalyticsType: "audio_play", IsLedSilent: true, PayloadType: &directives.TDirective_Payload{Payload: payload}}, nil
	}
	var stored fileplayer.TAudioFilePlayerCapability_EStoredSound
	switch request.Sound {
	case "notification":
		stored = fileplayer.TAudioFilePlayerCapability_NotificationSound
	case "success":
		stored = fileplayer.TAudioFilePlayerCapability_AliceIoTSoundsDone
	case "error":
		stored = fileplayer.TAudioFilePlayerCapability_SpotterError
	case "timer":
		stored = fileplayer.TAudioFilePlayerCapability_Timer
	default:
		return nil, errors.New("unknown sound")
	}
	if request.URL != "" {
		return nil, errors.New("supply exactly one sound or audio URL")
	}
	payload, err := proto.Marshal(&fileplayer.TAudioFilePlayerCapability_TLocalAudioFilePlayDirective{Name: newID(), Channel: channel.EDirectiveChannel_Dialog, PlayTimes: 1, Sound: &fileplayer.TAudioFilePlayerCapability_TLocalAudioFilePlayDirective_SoundToPlay{SoundToPlay: stored}})
	if err != nil {
		return nil, errors.New("cannot encode sound directive")
	}
	return &directives.TDirective{Type: "client_action", Name: "local_audio_file_play", AnalyticsType: "local_audio_file_play", DirectiveType: endpoint.TCapability_LocalAudioFilePlayDirectiveType, IsLedSilent: true, PayloadType: &directives.TDirective_PayloadRaw{PayloadRaw: payload}}, nil
}
