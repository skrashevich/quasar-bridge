package bridge

import (
	"bytes"
	"testing"

	"github.com/coder/websocket"
	group "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/message_group"
	"google.golang.org/protobuf/encoding/protowire"
	"quasarbridge/internal/musicmatch"
	"quasarbridge/internal/wire"
)

func TestNativeMusicComparisonPreservesOrDropsGroupedYandexResponse(t *testing.T) {
	for _, tc := range []struct {
		name, yandexTitle, soundCloudTitle string
		wantSoundCloud                     bool
	}{
		{"Yandex wins", "Гусары орать", "Другая песня", false},
		{"SoundCloud wins", "Другая песня", "Гусары орать", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &comparisonProvider{candidate: musicmatch.Candidate{Title: tc.soundCloudTitle}}
			n, up, down := comparisonRig(t, p)
			comparisonStart(t, n, up, "grouped-song")
			frame := wire.Decode(comparisonCloud("grouped-song", tc.yandexTitle, "Artist"), wire.ServerToClient)
			// The ordinary header owns this response. The new group contains
			// multiple references and must not change that existing ownership.
			frame.Server.MessageGroup = &group.TMessageGroupInfo{SubRequestRefs: []*group.TMessageGroupInfo_TSubRequestRef{
				{MessageId: "grouped-song", RequestId: "request-grouped-song"},
				{MessageId: "other-request", RequestId: "request-other"},
			}}
			unknownEnvelope := protowire.AppendVarint(protowire.AppendTag(nil, 19000, protowire.VarintType), 37)
			unknownGroup := protowire.AppendBytes(protowire.AppendTag(nil, 19001, protowire.BytesType), []byte("opaque-group-field"))
			unknownDirective := protowire.AppendBytes(protowire.AppendTag(nil, 19002, protowire.BytesType), []byte("opaque-directive-field"))
			frame.Server.ProtoReflect().SetUnknown(unknownEnvelope)
			frame.Server.MessageGroup.ProtoReflect().SetUnknown(unknownGroup)
			frame.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives()[0].ProtoReflect().SetUnknown(unknownDirective)
			original := aa(frame.Server)
			decoded := wire.Decode(original, wire.ServerToClient)
			if len(decoded.Server.GetMessageGroup().GetSubRequestRefs()) != 2 || !bytes.Equal(decoded.Server.ProtoReflect().GetUnknown(), unknownEnvelope) || !bytes.Equal(decoded.Server.GetMessageGroup().ProtoReflect().GetUnknown(), unknownGroup) {
				t.Fatal("grouped response fixture lost its new or unknown fields")
			}
			if err := n.Server(t.Context(), websocket.MessageBinary, original); err != nil {
				t.Fatal(err)
			}
			winner := comparisonWinner(t, down)
			n.wg.Wait()
			if !tc.wantSoundCloud {
				if !bytes.Equal(winner, original) || p.committed.Load() != 0 || p.accepted.Load() != 1 {
					t.Fatal("Yandex winner did not preserve the complete grouped response")
				}
				return
			}
			selected := wire.Decode(winner, wire.ServerToClient).Server
			directives, _, _ := comparisonDirectives(selected.GetEvent())
			if p.committed.Load() != 1 || p.accepted.Load() != 0 || selected.GetMessageGroup() != nil || len(directives) != 1 || directives[0].GetPayload().GetFields()["stream"].GetStructValue().GetFields()["url"].GetStringValue() != "https://audio.example/sc.m3u8" {
				t.Fatal("grouped Yandex response changed or contaminated the SoundCloud winner")
			}
			// A repeated grouped response from the losing request must remain
			// suppressed even after the coordinator has finished.
			if err := n.Server(t.Context(), websocket.MessageBinary, original); err != nil {
				t.Fatal(err)
			}
			select {
			case extra := <-down:
				t.Fatalf("losing grouped output leaked: %s", wire.Decode(extra, wire.ServerToClient).Kind())
			default:
			}
		})
	}
}
