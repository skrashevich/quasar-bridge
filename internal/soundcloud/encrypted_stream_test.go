package soundcloud

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func encryptedStreamCandidate(path, protocol, preset string, snipped bool) transcoding {
	t := transcoding{URL: "https://api-v2.soundcloud.com/" + path, Preset: preset, Snipped: snipped}
	t.Format.Protocol, t.Format.MIME = protocol, "audio/mp4"
	return t
}

func encryptedAACVariants() []transcoding {
	var candidates []transcoding
	for _, protocol := range []string{"cbc-encrypted-hls", "ctr-encrypted-hls"} {
		for _, preset := range []string{"aac_160", "aac_96", "abr_sq"} {
			candidates = append(candidates, encryptedStreamCandidate(protocol+"/"+preset, protocol, preset, false))
		}
	}
	return candidates
}

func TestEncryptedAACVariantsAreNotResolved(t *testing.T) {
	requests := 0
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		fmt.Fprint(w, `{"url":"https://cf-media.sndcdn.com/unsupported.m3u8"}`)
	})
	client, _ := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
	for _, candidate := range encryptedAACVariants() {
		upstream, err := client.Stream(t.Context(), Track{URN: "soundcloud:tracks:1", transcodings: []transcoding{candidate}})
		if !errors.Is(err, ErrUnavailable) || upstream != "" || requests != 0 || streamRank(candidate) != 0 {
			t.Fatal("unsupported encrypted stream was fetched", upstream, err, requests)
		}
	}
}

func TestOrdinaryHLSRemainsAvailableBesideEncryptedAndPreviewVariants(t *testing.T) {
	requests := 0
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/ordinary96" || r.Header.Get("Authorization") != "OAuth web-token" {
			t.Error("stream selection resolved unsupported media or lost authorization")
		}
		fmt.Fprint(w, `{"url":"https://cf-media.sndcdn.com/ordinary.m3u8"}`)
	})
	client, _ := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
	candidates := append(encryptedAACVariants(), encryptedStreamCandidate("preview", "hls", "aac_160", true), encryptedStreamCandidate("ordinary96", "hls", "aac_96", false))
	upstream, err := client.Stream(t.Context(), Track{URN: "soundcloud:tracks:1", transcodings: candidates})
	if err != nil || upstream != "https://cf-media.sndcdn.com/ordinary.m3u8" || requests != 1 {
		t.Fatal("ordinary full AAC HLS compatibility changed", upstream, err, requests)
	}
}

func TestLikedEncryptedAACIsNotReportedAsEmptyLibrary(t *testing.T) {
	track := catalogItem{ID: 1, Title: "Liked full recording", FullDuration: 236069, Policy: "MONETIZE", Streamable: new(true)}
	track.Media.Transcodings = encryptedAACVariants()
	requests := 0
	hc := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/me":
			fmt.Fprint(w, `{"id":42}`)
		case "/users/42/track_likes":
			body, err := json.Marshal(map[string]any{"collection": []any{map[string]any{"track": track}}})
			if err != nil {
				t.Error(err)
				return
			}
			w.Write(body)
		default:
			t.Error("encrypted liked track initiated unsupported stream work", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client, _ := NewClient(Config{API: "web", WebAccessToken: "web-token", ClientID: "public"}, hc)
	tracks, err := client.Likes(t.Context(), 10)
	if !errors.Is(err, ErrNoPlayableLikes) || len(tracks) != 0 || requests != 2 {
		t.Fatal("nonempty unsupported likes were reported as empty or playable", err, len(tracks), requests)
	}
}
