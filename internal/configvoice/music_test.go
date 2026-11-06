package configvoice

import "testing"

func TestMusicDefault(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"включи SoundCloud по умолчанию", "soundcloud"},
		{"Алиса, используй Саундклауд по умолчанию, пожалуйста", "soundcloud"},
		{"сделай SoundCloud музыкальным сервисом по умолчанию", "soundcloud"},
		{"установи саунд клауд по умолчанию", "soundcloud"},
		{"верни Яндекс Музыку по умолчанию", "yandex"},
		{"используй Яндекс Музыку по умолчанию", "yandex"},
		{"какой музыкальный сервис по умолчанию?", "status"},
		{"включи Spotify по умолчанию", "unsupported"},
		{"не включай SoundCloud по умолчанию", ""},
		{"как включить SoundCloud по умолчанию", ""},
		{"можешь включить SoundCloud по умолчанию?", ""},
		{"включи песню SoundCloud по умолчанию", ""},
		{"включи песню «SoundCloud по умолчанию»", ""},
		{"включи песню Гусары на SoundCloud", ""},
		{"сделай свет по умолчанию", ""},
	} {
		if got := MusicDefault(tc.text); got != tc.want {
			t.Errorf("%q=%q want %q", tc.text, got, tc.want)
		}
	}
}
