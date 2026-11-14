package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

func TestPlayMusicMatchesFullMetadataBeforeDisplayTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, title, prefix string
	}{
		{"161 characters", strings.Repeat("я", 160) + "б", `<audio src="/wanted.mp3"></audio>`},
		{"256 characters", strings.Repeat("я", 255) + "б", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.prefix + `<a href="/other.mp3">` + strings.Repeat("я", 160) + `в</a><a href="/wanted.mp3">` + tc.title + `</a>`
			tools, fetched := titleMusicTools(t, body)
			var played sound.Request
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, request sound.Request) error {
				played = request
				return nil
			})
			args, err := json.Marshal(map[string]string{"source": "svk.su", "title": tc.title})
			if err != nil {
				t.Fatal(err)
			}
			result, err := tools.Call(ctx, "play_music", string(args))
			if err != nil {
				t.Fatalf("complete metadata title was not selected: %v", err)
			}
			tracks, err := tools.Playback.queue.Tracks(played.URL)
			if err != nil || len(tracks) != 1 || tracks[0].URL != "https://svk.su/wanted.mp3" || len(*fetched) != 1 {
				t.Fatalf("selection lost its source: tracks=%+v, fetched=%v, err=%v", tracks, *fetched, err)
			}
			const displayLength = 160
			wantDisplay := strings.Repeat("я", displayLength)
			if played.Title != wantDisplay || tracks[0].Title != wantDisplay {
				t.Fatalf("unbounded playback title: played=%+v, tracks=%+v", played, tracks)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				FirstTrack string `json:"first_track"`
			}
			if err := json.Unmarshal(encoded, &report); err != nil || report.FirstTrack != wantDisplay {
				t.Fatalf("unbounded tool result: %s, err=%v", encoded, err)
			}
			encoded, err = json.Marshal(tracks)
			if err != nil {
				t.Fatal(err)
			}
			var publicTracks []struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			}
			if err := json.Unmarshal(encoded, &publicTracks, json.RejectUnknownMembers(true)); err != nil || len(publicTracks) != 1 || publicTracks[0].Title != wantDisplay {
				t.Fatalf("queue JSON leaked matching metadata: %s, err=%v", encoded, err)
			}
		})
	}
}

func TestPlayMusicDoesNotSelectTruncatedMetadataPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name, title, requested string
	}{
		{"160-character display prefix", strings.Repeat("я", 160) + "б", strings.Repeat("я", 160)},
		{"160-character prefix of overlong metadata", strings.Repeat("я", 256) + "б", strings.Repeat("я", 160)},
		{"256-character prefix of overlong metadata", strings.Repeat("я", 256) + "б", strings.Repeat("я", 256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools, fetched := titleMusicTools(t, `<a href="/wrong.mp3">`+tc.title+`</a>`)
			played := false
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(context.Context, sound.Request) error {
				played = true
				return nil
			})
			args, err := json.Marshal(map[string]string{"source": "svk.su", "title": tc.requested})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tools.Call(ctx, "play_music", string(args)); !errors.Is(err, errMusicTitleNotFound) || played || len(*fetched) != 1 {
				t.Fatalf("truncated prefix was selected: played=%t, fetched=%v, err=%v", played, *fetched, err)
			}
			requireNoMusicQueue(t, tools)
		})
	}
}
