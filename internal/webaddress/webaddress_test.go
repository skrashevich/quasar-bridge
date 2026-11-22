package webaddress

import (
	"strings"
	"testing"
)

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

func TestNormalizeSplitSpokenSiteNames(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Включи музыку с сайта rp fm точка ru", "Включи музыку с сайта https://rpfm.ru"},
		{"Включи музыку с сайта рп фм точка ру", "Включи музыку с сайта https://rpfm.ru"},
		{"Открой сайт r p f m dot ru", "Открой сайт https://rpfm.ru"},
		{"На сайте b b c точка com", "На сайте https://bbc.com"},
		{"С сайта rp fm точка ru слэш Music/Live.mp3?x=1", "С сайта https://rpfm.ru/Music/Live.mp3?x=1"},
		{"С сайта www точка rp fm точка ru", "С сайта https://www.rpfm.ru"},
		{"Включи песню «Не надо» с сайта рп фм точка ру", "Включи песню «Не надо» с сайта https://rpfm.ru"},
		{"Не включай музыку с сайта rp fm точка ru", "Не включай музыку с сайта https://rpfm.ru"},
		{"с сайта мой fm точка ru", "с сайта мой fm точка ru"},
		{"с сайта в fm точка ru", "с сайта в fm точка ru"},
		{"с сайта любимый fm точка ru", "с сайта любимый fm точка ru"},
		{"с сайта rp, fm точка ru", "с сайта rp, fm точка ru"},
		{"с сайта «rp» fm точка ru", "с сайта «rp» fm точка ru"},
		{"с сайта www точка ab cd точка ru", "с сайта www точка ab cd точка ru"},
		{"с сайта www точка rpf m точка ru", "с сайта www точка rpf m точка ru"},
		{"с сайта www точка my fm точка ru", "с сайта www точка my fm точка ru"},
		{"с сайта www точка мой fm точка ru", "с сайта www точка мой fm точка ru"},
		{"с сайта r p f m x точка ru", "с сайта r p f m x точка ru"},
		{"с сайта rpf m точка ru", "с сайта rpf m точка ru"},
		{"с сайта rp fm", "с сайта rp fm"},
		{"rp fm точка ru", "rp fm точка ru"},
		{"Включи музыку с fm точка ru", "Включи музыку с https://fm.ru"},
		{"Включи песню «rp fm точка ru» с сайта b b c точка com", "Включи песню «rp fm точка ru» с сайта https://bbc.com"},
		{"с сайта fm точка ru и tv точка com", "с сайта https://fm.ru и https://tv.com"},
		{"с сайта rp fm точка ru и с сайта b b c точка com", "с сайта https://rpfm.ru и с сайта https://bbc.com"},
		{"с сайта https://rpfm.ru/Music?q=rp+fm", "с сайта https://rpfm.ru/Music?q=rp+fm"},
		{"на сайте пример точка рф слеш Music", "на сайте https://xn--e1afmkfd.xn--p1ai/Music"},
		{"почта user@mail.example.org", "почта user@mail.example.org"},
		{"foo_bar.example.org", "foo_bar.example.org"},
		{strings.Repeat("a", 64) + ".example.org", strings.Repeat("a", 64) + ".example.org"},
		{"foo_https://example.org", "foo_https://example.org"},
		{"123https://example.org", "123https://example.org"},
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
	if MusicRequest("Не включай музыку с сайта rp fm точка ru") {
		t.Fatal("split address erased command negation")
	}
	if !MusicRequest("Включи песню «Не надо» с сайта rp fm точка ru") {
		t.Fatal("split address turned a track title into command negation")
	}
}
