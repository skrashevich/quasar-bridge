package bridge

import (
	device "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	request "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	"google.golang.org/protobuf/proto"

	"quasarbridge/internal/sound"
)

func nativePlaybackState(req *request.TAliceRequestProto) sound.PlaybackState {
	r := req.GetRequest()
	state := r.GetDeviceState()
	if state == nil {
		raw := r.GetDeviceStateRaw()
		if len(raw) == 0 || len(raw) > 1<<20 {
			return sound.PlaybackState{}
		}
		state = new(device.TDeviceState)
		if proto.Unmarshal(raw, state) != nil {
			return sound.PlaybackState{}
		}
	}
	p := state.GetAudioPlayer()
	return sound.PlaybackState{Present: true, OffsetKnown: p != nil && p.OffsetMs != nil, StreamID: p.GetCurrentlyPlaying().GetStreamId(), OffsetMS: max(0, int64(p.GetOffsetMs())), PlayerState: p.GetPlayerState().String()}
}
