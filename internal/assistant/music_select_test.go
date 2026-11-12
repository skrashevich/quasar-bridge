package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

const musicSelectionPage = `<script type="application/ld+json">[{"title":"Мы вместе","audioUrl":"https://svk.su/together.mp3"},{"title":"Нас нет","audioUrl":"https://svk.su/nas-net.mp3?original=yes"}]</script>`

func titleMusicTools(t *testing.T, body string) (*Tools, *[]string) {
	t.Helper()
	var fetched []string
	tools := musicTools(t, func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		if r.Method != http.MethodGet || r.URL.String() != "https://svk.su" {
			t.Fatalf("unexpected fetch: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	return tools, &fetched
}

func requireNoMusicQueue(t *testing.T, tools *Tools) {
	t.Helper()
	tools.Playback.queue.mu.Lock()
	defer tools.Playback.queue.mu.Unlock()
	if len(tools.Playback.queue.entries) != 0 {
		t.Fatal("registered a queue for an unsuccessful selection")
	}
}

func TestStartMusicSelectsRequestedTitle(t *testing.T) {
	for _, text := range []string{
		`включи трек "нас нет" с сайта svk.su`,
		`включи трек нас нет с сайта svk.su`,
		`Алиса, включи песню Нас нет с сайта svk точка су`,
		`Воспроизведи трек «Нас нет» с https://svk.su`,
		`Поставь трек “НАС НЕТ” с сайта svk.su`,
		`Включите песню 'нас нет' с сайта svk.su`,
		`включи трек «Нас, нёт!» с сайта svk.su`,
		`включи трек НАС — НЕТ с сайта svk.su`,
		`включи песню Нас нет на сайте svk точка су`,
		`включи трек "Нас нет" с сайта "svk.su"`,
		`включи пожалуйста трек "Нас нет" с сайта svk.su`,
		`включи мне трек "Нас нет" с сайта svk.su`,
		`«Алиса включи трек “Нас нет” с сайта svk.su»`,
		`включи трек "Нас   нет" с сайта svk.su`,
	} {
		t.Run(text, func(t *testing.T) {
			tools, fetched := titleMusicTools(t, musicSelectionPage)
			var played sound.Request
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
			message, err := tools.StartMusic(ctx, text)
			if err != nil || !strings.Contains(message, "Включаю") {
				t.Fatalf("message=%q err=%v", message, err)
			}
			tracks, err := tools.Playback.queue.Tracks(played.URL)
			if err != nil || len(tracks) != 1 || tracks[0].URL != "https://svk.su/nas-net.mp3?original=yes" || tracks[0].Title != "Нас нет" || played.Title != "Нас нет" {
				t.Fatalf("tracks=%+v played=%+v err=%v", tracks, played, err)
			}
			if len(*fetched) != 1 {
				t.Fatalf("repeated discovery or audio fetch: %v", *fetched)
			}
		})
	}
}

func TestStartMusicGenericCommandsRetainWholePlaylist(t *testing.T) {
	for _, text := range []string{
		"включи музыку с сайта svk.su", "включи радио с сайта svk.su",
		"включи трек с сайта svk.su", "включи песню на сайте svk.su", "включи трек https://svk.su",
	} {
		t.Run(text, func(t *testing.T) {
			tools, _ := titleMusicTools(t, musicSelectionPage)
			var played sound.Request
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
			message, err := tools.StartMusic(ctx, text)
			if err != nil || message != "Включаю музыку." {
				t.Fatalf("message=%q err=%v", message, err)
			}
			tracks, err := tools.Playback.queue.Tracks(played.URL)
			if err != nil || len(tracks) != 2 || tracks[0].Title != "Мы вместе" || tracks[1].Title != "Нас нет" {
				t.Fatalf("tracks=%+v err=%v", tracks, err)
			}
		})
	}
}

func TestStartMusicDomainTitleIsNotTheSource(t *testing.T) {
	for _, text := range []string{`включи трек "Love.com" с сайта svk.su`, `включи трек Love.com с сайта svk.su`, `включи трек Love.com с https://svk.su`} {
		t.Run(text, func(t *testing.T) {
			tools, fetched := titleMusicTools(t, `<a href="/other.mp3">Other</a><a href="/love.mp3">Love.com</a>`)
			var played sound.Request
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
			message, err := tools.StartMusic(ctx, text)
			if err != nil || !strings.Contains(message, "Включаю") {
				t.Fatalf("message=%q err=%v", message, err)
			}
			tracks, err := tools.Playback.queue.Tracks(played.URL)
			if err != nil || len(tracks) != 1 || tracks[0].URL != "https://svk.su/love.mp3" || tracks[0].Title != "Love.com" || len(*fetched) != 1 {
				t.Fatalf("tracks=%+v fetched=%v err=%v", tracks, *fetched, err)
			}
		})
	}
}

func TestStartMusicTitleEndingInSourcePrepositionIsNotShortened(t *testing.T) {
	for _, ending := range []string{"на", "с", "со"} {
		t.Run(ending, func(t *testing.T) {
			tools, _ := titleMusicTools(t, `<a href="/wrong.mp3">Нас нет</a><a href="/wanted.mp3">Нас нет `+ending+`</a>`)
			var played sound.Request
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
			message, err := tools.StartMusic(ctx, "включи трек Нас нет "+ending+" с сайта svk.su")
			if err != nil || !strings.Contains(message, "Включаю") {
				t.Fatalf("message=%q err=%v", message, err)
			}
			tracks, err := tools.Playback.queue.Tracks(played.URL)
			if err != nil || len(tracks) != 1 || tracks[0].URL != "https://svk.su/wanted.mp3" || tracks[0].Title != "Нас нет "+ending {
				t.Fatalf("title was shortened: tracks=%+v err=%v", tracks, err)
			}
		})
	}
}

func TestStartMusicMissingOrAmbiguousTitleDoesNotPlay(t *testing.T) {
	for _, tt := range []struct{ body, title, want string }{
		{musicSelectionPage, "Нас", "не наш"},
		{`<a href="/one.mp3">Нас нет</a><a href="/two.mp3">НАС — НЁТ!</a>`, "нас нет", "несколько"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			tools, fetched := titleMusicTools(t, tt.body)
			ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(context.Context, sound.Request) error { t.Fatal("played an unsuccessful selection"); return nil })
			message, err := tools.StartMusic(ctx, `включи трек "`+tt.title+`" с сайта svk.su`)
			if err != nil || !strings.Contains(strings.ToLower(message), tt.want) || len(*fetched) != 1 {
				t.Fatalf("message=%q err=%v fetched=%v", message, err, *fetched)
			}
			requireNoMusicQueue(t, tools)
		})
	}
}

func TestPlayMusicTitleUsesSameSelection(t *testing.T) {
	tools, fetched := titleMusicTools(t, musicSelectionPage)
	var played sound.Request
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
	result, err := tools.Call(ctx, "play_music", `{"source":"svk.su","title":"НАС, НЁТ!"}`)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	var report struct {
		Count int `json:"track_count"`
	}
	if err := json.Unmarshal(encoded, &report); err != nil || report.Count != 1 {
		t.Fatalf("result=%s err=%v", encoded, err)
	}
	tracks, err := tools.Playback.queue.Tracks(played.URL)
	if err != nil || len(tracks) != 1 || tracks[0].URL != "https://svk.su/nas-net.mp3?original=yes" || tracks[0].Title != "Нас нет" || len(*fetched) != 1 {
		t.Fatalf("tracks=%+v err=%v fetched=%v", tracks, err, *fetched)
	}
}

func TestPlayMusicTitleRejectsInvalidValuesBeforeFetching(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("invalid title fetched page"); return nil, nil })
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(context.Context, sound.Request) error { t.Fatal("invalid title played"); return nil })
	for _, value := range []string{`null`, `42`, `true`, `{}`, `[]`, `""`, `"   "`, `"!!!"`, `"` + strings.Repeat("я", 257) + `"`} {
		if _, err := tools.Call(ctx, "play_music", `{"source":"svk.su","title":`+value+`}`); err == nil {
			t.Fatalf("accepted title=%s", value)
		}
	}
	requireNoMusicQueue(t, tools)
}

func TestPlayMusicTitleRequiresSpeakerBeforeFetching(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("title fetched without speaker"); return nil, nil })
	if _, err := tools.Call(t.Context(), "play_music", `{"source":"svk.su","title":"Нас нет"}`); err == nil || !strings.Contains(err.Error(), "current speaker") {
		t.Fatalf("err=%v", err)
	}
}

func TestStartMusicNamedDirectAudioRequiresTitleMetadata(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("direct audio fetched"); return nil, nil })
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(context.Context, sound.Request) error { t.Fatal("guessed title from URL"); return nil })
	message, err := tools.StartMusic(ctx, `включи трек "Нас нет" с https://audio.example/nas-net.mp3`)
	if err != nil || !strings.Contains(strings.ToLower(message), "не наш") {
		t.Fatalf("message=%q err=%v", message, err)
	}
	requireNoMusicQueue(t, tools)
}

func TestStartMusicEmptyQuotedTitleDoesNotBecomePlaylist(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("empty title fetched website"); return nil, nil })
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(context.Context, sound.Request) error { t.Fatal("empty title played"); return nil })
	for _, text := range []string{`включи трек "" с сайта svk.su`, `включи трек «» с сайта svk.su`, `включи песню “ ” с сайта svk.su`, `включи трек "" svk.su`} {
		if _, err := tools.StartMusic(ctx, text); err == nil || !strings.Contains(err.Error(), "title") {
			t.Fatalf("accepted empty named request=%q err=%v", text, err)
		}
	}
	requireNoMusicQueue(t, tools)
}

func TestStartMusicQuotedAudioURLRemainsGenericSource(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("direct URL fetched"); return nil, nil })
	var played sound.Request
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(_ context.Context, r sound.Request) error { played = r; return nil })
	for _, text := range []string{`включи трек "https://audio.example/live.m3u8"`, `включи трек с сайта «https://audio.example/live.m3u8»`} {
		message, err := tools.StartMusic(ctx, text)
		if err != nil || message != "Включаю музыку." || played.URL != "https://audio.example/live.m3u8" {
			t.Fatalf("message=%q played=%+v err=%v", message, played, err)
		}
	}
}

func TestPlayMusicTitleLengthUsesCharacters(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("direct URL fetched"); return nil, nil })
	ctx := sound.WithPlayer(dialogue.WithSpeaker(t.Context(), "speaker"), func(context.Context, sound.Request) error { t.Fatal("guessed direct title"); return nil })
	args, err := json.Marshal(map[string]string{"source": "https://audio.example/live.m3u8", "title": strings.Repeat("界", 256)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Call(ctx, "play_music", string(args)); !errors.Is(err, errMusicTitleNotFound) {
		t.Fatalf("256 character title was rejected before selection: %v", err)
	}
	requireNoMusicQueue(t, tools)
}

func TestStartMusicTitleKeepsNegationAndCurrentCommandGuards(t *testing.T) {
	tools := musicTools(t, func(*http.Request) (*http.Response, error) { t.Fatal("noncommand fetched"); return nil, nil })
	for _, text := range []string{`Не включи трек "Нас нет" с сайта svk.su`, `Вчера я просил включи трек "Нас нет" с сайта svk.su`, `Как включить песню "Нас нет" с сайта svk.su?`} {
		if tools.CanStartMusic(text) {
			t.Fatalf("accepted noncommand=%q", text)
		}
	}
}
