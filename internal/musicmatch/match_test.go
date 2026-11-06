package musicmatch

import "testing"

func TestPreferSoundCloudMatchesBeforeSourcePreference(t *testing.T) {
	for _, tc := range []struct {
		name, query      string
		ym, sc           Candidate
		preference, want bool
	}{
		{"exact SC", "Гусары орать", Candidate{Title: "Песня гусар"}, Candidate{Title: "Гусары, Орать!", Artist: "svk"}, false, true},
		{"exact YM over cover", "Queen Bohemian Rhapsody", Candidate{Title: "Bohemian Rhapsody", Artist: "Queen"}, Candidate{Title: "Bohemian Rhapsody (cover)", Artist: "Another"}, true, false},
		{"same song SC preference", "Гусары орать", Candidate{Title: "Гусары, Орать!", Artist: "svk"}, Candidate{Title: "Гусары, Орать!", Artist: "svk"}, true, true},
		{"same song YM preference", "Гусары орать", Candidate{Title: "Гусары, Орать!", Artist: "svk"}, Candidate{Title: "Гусары, Орать!", Artist: "svk"}, false, false},
		{"requested remix", "Song remix", Candidate{Title: "Song", Artist: "Artist"}, Candidate{Title: "Song (remix)", Artist: "Artist"}, false, true},
		{"plain song over remix", "Song", Candidate{Title: "Song", Artist: "Artist"}, Candidate{Title: "Song (remix)", Artist: "Artist"}, true, false},
		{"artist embedded in upload title", "Queen Bohemian Rhapsody", Candidate{Title: "Bohemian Rhapsody", Artist: "Queen"}, Candidate{Title: "Queen - Bohemian Rhapsody", Artist: "Queen"}, true, true},
		{"track index and artist embedded", "svk Не спать", Candidate{Title: "Не спать", Artist: "svk"}, Candidate{Title: "03 svk - Не спать!", Artist: "svk"}, true, true},
		{"Russian remix requested", "Song ремикс", Candidate{Title: "Song (Remix)"}, Candidate{Title: "Song"}, true, false},
		{"English remix requested", "Song remix", Candidate{Title: "Song"}, Candidate{Title: "Song (ремикс)"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreferSoundCloud(tc.query, tc.ym, tc.sc, tc.preference); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestScoreUsesSpokenSongQuery(t *testing.T) {
	c := Candidate{Title: "Гусары, Орать!", Artist: "svk"}
	if Score("Алиса, включи песню Гусары орать", c) != Score("Гусары орать", c) {
		t.Fatal("spoken intent changed candidate score")
	}
}

func TestExactMatchRequiresWholeTitleAndRequestedVersion(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		candidate   Candidate
		want        bool
	}{
		{"title punctuation", "Включи Нас нет", Candidate{Title: "Нас, нет!", Artist: "Performer"}, true},
		{"artist before title", "поставь Performer Нас нет", Candidate{Title: "Нас нет", Artist: "Performer"}, true},
		{"artist after title", "запусти Нас нет Performer", Candidate{Title: "Нас нет", Artist: "Performer"}, true},
		{"upload prefix and track number", "включи Нас нет", Candidate{Title: "03 Performer - Нас нет!", Artist: "Performer"}, true},
		{"normalized artist", "включи семен песня", Candidate{Title: "Песня", Artist: "Семён"}, true},
		{"partial title", "включи Нас", Candidate{Title: "Нас нет", Artist: "Performer"}, false},
		{"artist only", "включи Performer", Candidate{Title: "Нас нет", Artist: "Performer"}, false},
		{"unrequested remix title", "включи Song", Candidate{Title: "Song (Remix)", Artist: "Artist"}, false},
		{"unrequested remix field", "включи Song", Candidate{Title: "Song", Artist: "Artist", Version: "Remix"}, false},
		{"unrequested cover field", "включи Artist Song", Candidate{Title: "Song", Artist: "Artist", Version: "cover"}, false},
		{"requested localized remix", "включи Melody ремикс", Candidate{Title: "Melody (Remix)", Artist: "Artist"}, true},
		{"different artist words", "включи Other Song", Candidate{Title: "Song", Artist: "Artist"}, false},
		{"empty query", "", Candidate{Title: "Song", Artist: "Artist"}, false},
		{"empty title", "включи Artist", Candidate{Artist: "Artist"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExactMatch(tc.query, tc.candidate); got != tc.want {
				t.Fatalf("ExactMatch=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestScorePreservesMetadataRankingValues(t *testing.T) {
	for _, tc := range []struct {
		query     string
		candidate Candidate
		want      int
	}{
		{"Song", Candidate{Title: "Song", Artist: "Artist"}, 100},
		{"Artist Song", Candidate{Title: "03 Artist - Song!", Artist: "Artist"}, 120},
		{"Song", Candidate{Title: "Song", Artist: "Artist", Version: "Remix"}, 55},
		{"Song remix", Candidate{Title: "Song (ремикс)", Artist: "Artist"}, 145},
	} {
		if got := Score(tc.query, tc.candidate); got != tc.want {
			t.Fatalf("Score(%q)=%d want=%d", tc.query, got, tc.want)
		}
	}
}

func TestExplicitArtistMatchRequiresWholeTitleAndArtist(t *testing.T) {
	for _, tc := range []struct {
		query string
		c     Candidate
		want  bool
	}{
		{"включи Performer Wanted", Candidate{Title: "Wanted", Artist: "Performer"}, true},
		{"включи Wanted Performer", Candidate{Title: "03 Performer - Wanted", Artist: "Performer"}, true},
		{"включи Wanted", Candidate{Title: "Wanted", Artist: "Performer"}, false},
		{"включи Performer", Candidate{Title: "Wanted", Artist: "Performer"}, false},
		{"включи Other Wanted", Candidate{Title: "Wanted", Artist: "Performer"}, false},
		{"включи Performer Wanted", Candidate{Title: "Wanted", Artist: "Performer", Version: "Remix"}, false},
	} {
		if got := ExplicitArtistMatch(tc.query, tc.c); got != tc.want {
			t.Fatalf("ExplicitArtistMatch(%q)=%v want=%v", tc.query, got, tc.want)
		}
	}
}
