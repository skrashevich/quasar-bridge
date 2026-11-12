package assistant

import (
	"errors"
	"strings"
	"unicode"
)

const maxMusicTitleRunes = 256

var (
	errMusicTitleNotFound  = errors.New("requested track title was not found on the website")
	errMusicTitleAmbiguous = errors.New("requested track title matches multiple audio URLs")
)

func normalizeMusicTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if r == 'ё' || r == 'Ё' {
			return 'е'
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, title)
	return strings.Join(strings.Fields(title), " ")
}

func selectMusicTitle(tracks []WebLink, title string) ([]WebLink, error) {
	want := normalizeMusicTitle(title)
	var match *WebLink
	for _, track := range tracks {
		if want == "" || normalizeMusicTitle(track.Title) != want {
			continue
		}
		if match != nil && match.URL != track.URL {
			return nil, errMusicTitleAmbiguous
		}
		if match == nil {
			match = new(track)
		}
	}
	if match == nil {
		return nil, errMusicTitleNotFound
	}
	return []WebLink{*match}, nil
}
