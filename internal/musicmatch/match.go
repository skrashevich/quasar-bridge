// Package musicmatch compares catalog metadata without transporting credentials or media URLs.
package musicmatch

import (
	"context"
	"strings"
	"unicode"
)

type Candidate struct {
	Title, Artist, Version string
	DurationMS             int64
}

type Prepared interface {
	Candidate() Candidate
	Commit(context.Context) error
	Close()
}

type Searcher interface {
	Prepare(context.Context, string) (Prepared, error)
}

func normalize(text string) string {
	text = strings.Map(func(r rune) rune {
		if r == 'ё' || r == 'Ё' {
			return 'е'
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, text)
	text = " " + strings.Join(strings.Fields(text), " ") + " "
	for from, to := range map[string]string{" ремикс ": " remix ", " кавер ": " cover ", " концертная версия ": " live ", " концерт ": " live ", " караоке ": " karaoke ", " инструментальная версия ": " instrumental "} {
		text = strings.ReplaceAll(text, from, to)
	}
	return strings.TrimSpace(text)
}

// Query removes a single spoken command prefix; callers may pass the original
// utterance to both provider preparation and cross-provider scoring.
func Query(text string) string {
	text = normalize(text)
	text = strings.TrimPrefix(text, "алиса ")
	for _, verb := range []string{"включи ", "поставь ", "сыграй ", "играй ", "запусти ", "play "} {
		if rest, ok := strings.CutPrefix(text, verb); ok {
			text = rest
			for _, kind := range []string{"песню ", "трек ", "композицию ", "song ", "track "} {
				if rest, ok := strings.CutPrefix(text, kind); ok {
					return rest
				}
			}
			break
		}
	}
	return text
}

// Score measures metadata agreement only; it makes no assumption about audio quality.
func Score(query string, c Candidate) int {
	q, title, artist := matchingMetadata(query, c)
	if q == "" || title == "" {
		return -10000
	}
	qwords := strings.Fields(q)
	all := " " + title + " " + artist + " "
	score := 0
	for _, word := range qwords {
		if strings.Contains(all, " "+word+" ") {
			score += 10
		} else {
			score -= 20
		}
	}
	switch {
	case q == title+" "+artist || q == artist+" "+title:
		score += 100
	case q == title:
		score += 90
	case strings.Contains(q, title) && artist != "" && strings.Contains(q, artist):
		score += 70
	case strings.Contains(title, q):
		score += 20
	}
	version := " " + normalize(c.Title+" "+c.Version) + " "
	for _, marker := range []string{"cover", "кавер", "remix", "ремикс", "live", "концерт", "karaoke", "караоке", "instrumental", "sped up", "slowed", "nightcore"} {
		if strings.Contains(version, " "+marker+" ") {
			if strings.Contains(" "+q+" ", " "+marker+" ") {
				score += 35
			} else {
				score -= 45
			}
		}
	}
	return score
}

func PreferSoundCloud(query string, ym, sc Candidate, preferSCOnTie bool) bool {
	a, b := Score(query, ym), Score(query, sc)
	return b > a || b == a && preferSCOnTie
}

// ExactMatch accepts a whole normalized title, optionally accompanied by the
// artist. It deliberately excludes fuzzy/artist-only matches and an unrequested
// alternate recording even when its version is stored outside the title.
func ExactMatch(query string, c Candidate) bool {
	q, title, artist := matchingMetadata(query, c)
	if q == "" || title == "" || !(q == title || artist != "" && (q == title+" "+artist || q == artist+" "+title)) {
		return false
	}
	version := " " + normalize(c.Title+" "+c.Version) + " "
	for _, marker := range []string{"cover", "кавер", "remix", "ремикс", "live", "концерт", "karaoke", "караоке", "instrumental", "sped up", "slowed", "nightcore"} {
		if strings.Contains(version, " "+marker+" ") && !strings.Contains(" "+q+" ", " "+marker+" ") {
			return false
		}
	}
	return true
}

// SameArtist compares only normalized artist metadata, without aliases or history.
func SameArtist(a, b string) bool {
	a, b = normalize(a), normalize(b)
	return a != "" && a == b
}

func matchingMetadata(query string, c Candidate) (q, title, artist string) {
	q, title, artist = Query(query), normalize(c.Title), normalize(c.Artist)
	if artist != "" {
		if rest, ok := strings.CutPrefix(title, artist+" "); ok {
			title = rest
		} else if index, rest, ok := strings.Cut(title, " "); ok && len(index) <= 3 && strings.IndexFunc(index, func(r rune) bool { return !unicode.IsDigit(r) }) == -1 {
			if rest, ok := strings.CutPrefix(rest, artist+" "); ok {
				title = rest
			}
		}
	}
	return q, title, artist
}

// ExplicitArtistMatch requires the utterance to name both the whole title and
// its artist, preserving the same recording/version exclusions as ExactMatch.
func ExplicitArtistMatch(query string, c Candidate) bool {
	q, title, artist := matchingMetadata(query, c)
	return artist != "" && title != "" && ExactMatch(query, c) && (q == title+" "+artist || q == artist+" "+title)
}
