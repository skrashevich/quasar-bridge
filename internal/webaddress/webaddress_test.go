package webaddress

import "testing"

func TestNormalizeWrittenAndSpokenAddresses(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"включи музыку с сайта abc.su/music", "включи музыку с сайта https://abc.su/music"},
		{"Открой https://example.org/Music?x=1", "Открой https://example.org/Music?x=1"},
		{"открой abc.ru/песни", "открой https://abc.ru/%D0%BF%D0%B5%D1%81%D0%BD%D0%B8"},
		{"Включи музыку с сайта абв точка су слэш music", "Включи музыку с сайта https://abv.su/music"},
		{"поставь музыку с бгд dot com slash music", "поставь музыку с https://bgd.com/music"},
		{"включи аудио с сайта абв точка с ю слеш Music", "включи аудио с сайта https://abv.su/Music"},
		{"с сайта абв.зу/music", "с сайта https://abv.su/music"},
		{"с сайта абв.zu/music", "с сайта https://abv.su/music"},
		{"открой пример.рф/Музыка", "открой https://xn--e1afmkfd.xn--p1ai/%D0%9C%D1%83%D0%B7%D1%8B%D0%BA%D0%B0"},
		{"на сайте пример точка рф слеш music", "на сайте https://xn--e1afmkfd.xn--p1ai/music"},
		{"открой пример.com/music", "открой https://xn--e1afmkfd.com/music"},
		{"с сайта www точка abc точка org слеш audio слеш Track.mp3", "с сайта https://www.abc.org/audio/Track.mp3"},
		{"ссылка abc.su/music, спасибо", "ссылка https://abc.su/music, спасибо"},
		{"почта user@example.org", "почта user@example.org"},
		{"число 3.14", "число 3.14"},
		{"проиграй файл Track.mp3", "проиграй файл Track.mp3"},
		{"включи музыку с сайта", "включи музыку с сайта"},
		{"адрес абв точка где то", "адрес абв точка где то"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := Normalize(tc.input); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if got := Normalize(tc.want); got != tc.want {
				t.Fatalf("normalization is not idempotent: %q", got)
			}
		})
	}
}

func TestMusicRequestNeedsPlaybackMusicAndWebsite(t *testing.T) {
	for _, text := range []string{
		"включи музыку с сайта abc.su/music",
		"проиграй песни со страницы https://example.org/audio",
		"Воспроизведи аудио абв точка су слэш music",
		"поставь музыку с сайта",
	} {
		if !MusicRequest(text) {
			t.Errorf("missed explicit website playback: %q", text)
		}
	}
	for _, text := range []string{
		"включи музыку", "включи радио", "включи песню",
		"какая музыка на сайте abc.su/music", "прочитай сайт abc.su/music",
		"не включай музыку с сайта abc.su/music", "не надо воспроизводить музыку с сайта abc.su/music",
		"как включить музыку на сайте abc.su/music", "объясни, как включить музыку с сайта abc.su/music",
	} {
		if MusicRequest(text) {
			t.Errorf("unexpected playback request: %q", text)
		}
	}
}

func TestNormalizeSpelledLatinDomainLabels(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Включи музыку с сайта эс вэ ка точка эс ю", "Включи музыку с сайта https://svk.su"},
		{"открой эй би си точка ком", "открой https://abc.com"},
		{"прочитай би би си точка си оу точка ю кей", "прочитай https://bbc.co.uk"},
		{"с сайта эф оу оу точка орг слэш Music слэш Track.MP3?x=One", "с сайта https://foo.org/Music/Track.MP3?x=One"},
		{"с сайта эй дефис би два точка ком", "с сайта https://a-b2.com"},
		{"с сайта дабл ю дабл ю дабл ю точка эй би си точка ком", "с сайта https://www.abc.com"},
		{"открой www.example.org и эй би си точка ком", "открой https://www.example.org и https://abc.com"},
		{"на сайте пример точка рф слеш Музыка", "на сайте https://xn--e1afmkfd.xn--p1ai/%D0%9C%D1%83%D0%B7%D1%8B%D0%BA%D0%B0"},
		{"открой эй би си точка рф", "открой https://abc.xn--p1ai"},
		{"с сайта abc точка nonsense", "с сайта abc точка nonsense"},
		{"с сайта свк точка слух", "с сайта свк точка слух"},
		{"не сайт: эй дефис точка ком", "не сайт: эй дефис точка ком"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := Normalize(tc.input); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if got := Normalize(tc.want); got != tc.want {
				t.Fatalf("not idempotent: %q", got)
			}
		})
	}
}

func TestCanonicalSourceRequiresOneReliableWholeAddress(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"example.org", "https://example.org"},
		{"эй би си точка орг", "https://abc.org"},
		{"https://example.org/Music/Track.MP3?x=One", "https://example.org/Music/Track.MP3?x=One"},
		{"пример точка рф", "https://xn--e1afmkfd.xn--p1ai"},
	} {
		if got, ok := CanonicalSource(tc.input); !ok || got != tc.want {
			t.Errorf("%q got=%q ok=%v want=%q", tc.input, got, ok, tc.want)
		}
	}
	for _, input := range []string{
		"abc точка слух", "abc точка nonsense", "https://abc.nonsense",
		"example.org спасибо", "prefix example.org", "example.org/music слово",
		"https://user@example.org", "http://127.0.0.1/music", "http://192.168.1.1/music",
		"http://localhost/music", "https://example.org/#music", "https://example.org/#",
		"https://bad..example.org/music",
	} {
		if got, ok := CanonicalSource(input); ok {
			t.Errorf("accepted incomplete or unreliable source %q as %q", input, got)
		}
	}
}

func TestDomainStemPreservesSpellingWithoutGuessingSuffix(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"альфа точка слух", "alfa"},
		{"эс вэ ка точка слух", "svk"},
		{"эй би си точка орг слэш Music", "abc"},
		{"https://bbc.co.uk/music?x=1", "bbc.co"},
		{"пример точка рф", "xn--e1afmkfd"},
	} {
		if got, ok := DomainStem(tc.input); !ok || got != tc.want {
			t.Errorf("%q got=%q ok=%v want=%q", tc.input, got, ok, tc.want)
		}
	}
	for _, input := range []string{"foo", "foo.org other", "http://127.0.0.1", "https://user@example.org"} {
		if got, ok := DomainStem(input); ok {
			t.Errorf("accepted incomplete source %q as %q", input, got)
		}
	}
}
