package webaddress

import "testing"

func TestMusicRequestSeparatesTitleAndCommandNegation(t *testing.T) {
	for _, text := range []string{
		`Включи песню «Не надо» с сайта svk.su`,
		`Включи песню Не надо с сайта svk.su`,
		`Проиграй трек "Не нужно" с сайта svk.su`,
		`Поставь песню Не хочу с сайта svk.su`,
		`Мне включи радио с сайта svk.su`,
	} {
		if !MusicRequest(text) {
			t.Errorf("affirmative request rejected: %s", text)
		}
	}
	for _, text := range []string{
		`Не включай песню «Не надо» с сайта svk.su`,
		`Включи музыку, но песню не надо с сайта svk.su`,
		`Включи радио, но трек не нужно с сайта svk.su`,
		`Включи песню «Не надо» с сайта svk.su, но не включай`,
		`Включи песню «Не надо», не нужно, с сайта svk.su`,
	} {
		if MusicRequest(text) {
			t.Errorf("negated request accepted: %s", text)
		}
	}
}
