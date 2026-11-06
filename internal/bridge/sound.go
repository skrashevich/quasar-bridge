package bridge

import (
	"context"
	"errors"
	"net/url"
	"strings"

	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	"github.com/skrashevich/quasar-go-genproto/proto/alice/protos/data/channel"
	endpoint "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint"
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
	defer n.mu.Unlock()
	p.soundSealed = true
	if p.canceled || p.ctx.Err() != nil || p.soundDirective == nil {
		return nil
	}
	directive := p.soundDirective
	p.soundDirective = nil
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
		if request.OffsetMS < 0 || request.OffsetMS > 2*60*60*1000 {
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
		id := request.StreamID
		if id == "" {
			id = newID()
		}
		payload, err := structpb.NewStruct(map[string]any{
			"stream":      map[string]any{"id": id, "url": request.URL, "format": format, "type": kind, "offset_ms": request.OffsetMS},
			"metadata":    map[string]any{"title": request.Title},
			"screen_type": "Default",
			"set_pause":   false,
		})
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
