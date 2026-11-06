package soundcloud

import (
	"bytes"
	"cmp"
	"encoding/json/v2"
	"errors"
	"strconv"
)

type Kind string

const (
	TrackKind    Kind = "track"
	ArtistKind   Kind = "artist"
	AlbumKind    Kind = "album"
	PlaylistKind Kind = "playlist"
)

var ErrAuthorization = errors.New("SoundCloud: для избранного нужна авторизация пользователя")
var ErrWebChallenge = errors.New("SoundCloud: доступ веб-сессии отклонён; откройте SoundCloud в браузере и проверьте CAPTCHA или повторите вход")
var ErrCollectionBudget = errors.New("SoundCloud: превышен лимит проверки коллекций")

var ErrHydrationBudget = errors.New("SoundCloud: превышен лимит проверки доступности треков плейлиста")

var ErrUnavailable = errors.New("SoundCloud: трек недоступен для полного воспроизведения")

var ErrNoPlayableLikes = errors.New("SoundCloud: в избранном есть треки, но нет доступных для полного воспроизведения на колонке")

type Track struct {
	trackAuthorization                                                      string
	URN, Title, Artist, Uploader, UserURN, ArtworkURL, PermalinkURL, Access string
	DurationMS                                                              int64
	transcodings                                                            []transcoding
}
type transcoding struct {
	URL     string `json:"url"`
	Preset  string `json:"preset"`
	Quality string `json:"quality"`
	Snipped bool   `json:"snipped"`
	Format  struct {
		Protocol string `json:"protocol"`
		MIME     string `json:"mime_type"`
	} `json:"format"`
}
type catalogItem struct {
	PlaylistType       string `json:"playlist_type"`
	Type               string `json:"type"`
	Sharing            string `json:"sharing"`
	TrackAuthorization string `json:"track_authorization"`
	ID                 int64  `json:"id"`
	URN                string `json:"urn"`
	Kind               string `json:"kind"`
	Title              string `json:"title"`
	Username           string `json:"username"`
	Duration           int64  `json:"duration"`
	FullDuration       int64  `json:"full_duration"`
	Access             string `json:"access"`
	Policy             string `json:"policy"`
	Streamable         *bool  `json:"streamable"`
	Artwork            string `json:"artwork_url"`
	Avatar             string `json:"avatar_url"`
	Permalink          string `json:"permalink_url"`
	MetadataArtist     string `json:"metadata_artist"`
	IsAlbum            bool   `json:"is_album"`
	User               struct {
		ID       int64  `json:"id"`
		URN      string `json:"urn"`
		Username string `json:"username"`
		Avatar   string `json:"avatar_url"`
	} `json:"user"`
	Publisher struct {
		Artist string `json:"artist"`
	} `json:"publisher_metadata"`
	Media struct {
		Transcodings []transcoding `json:"transcodings"`
	} `json:"media"`
	Tracks []catalogItem `json:"tracks"`
	Track  *catalogItem  `json:"track"`
}

func (i catalogItem) urn(kind string) string {
	if i.URN != "" {
		return i.URN
	}
	if i.ID != 0 {
		return "soundcloud:" + kind + ":" + strconv.FormatInt(i.ID, 10)
	}
	return ""
}
func (i catalogItem) track() (Track, bool) {
	return i.trackForPrincipal(false)
}

// personalTrack is used only after authenticating the current account.
func (i catalogItem) personalTrack() (Track, bool) {
	return i.trackForPrincipal(true)
}

func (i catalogItem) trackForPrincipal(personal bool) (Track, bool) {
	if i.Track != nil {
		return i.Track.trackForPrincipal(personal)
	}
	if i.Sharing == "private" && !personal || i.Access == "blocked" || i.Access == "preview" || i.Policy == "BLOCK" || i.Policy == "SNIP" || (i.Streamable != nil && !*i.Streamable) {
		return Track{}, false
	}
	urn := i.urn("tracks")
	if urn == "" || i.Title == "" {
		return Track{}, false
	}
	user := i.User.URN
	if user == "" && i.User.ID != 0 {
		user = "soundcloud:users:" + strconv.FormatInt(i.User.ID, 10)
	}
	return Track{
		URN: urn, Title: i.Title,
		Artist:   cmp.Or(i.MetadataArtist, i.Publisher.Artist, i.User.Username),
		Uploader: i.User.Username, UserURN: user,
		ArtworkURL: cmp.Or(i.Artwork, i.User.Avatar), PermalinkURL: i.Permalink,
		Access: cmp.Or(i.Access, "playable"), DurationMS: cmp.Or(i.FullDuration, i.Duration),
		transcodings: i.Media.Transcodings, trackAuthorization: i.TrackAuthorization,
	}, true
}
func decodeItems(b []byte) ([]catalogItem, string, error) {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		var items []catalogItem
		e := json.Unmarshal(b, &items)
		return items, "", e
	}
	var page struct {
		Collection []catalogItem `json:"collection"`
		Next       string        `json:"next_href"`
	}
	e := json.Unmarshal(b, &page)
	return page.Collection, page.Next, e
}
