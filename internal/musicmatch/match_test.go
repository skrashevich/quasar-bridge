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
