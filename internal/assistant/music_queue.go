package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"
)

const musicQueueTTL = 2 * time.Hour
const musicTrackLimit int64 = 64 << 20
const musicQueueLimit int64 = 512 << 20
const musicBufferSize = 32 << 10

type musicQueueEntry struct {
	tracks   []WebLink
	expires  time.Time
	progress []musicTrackProgress
}

type musicTrackProgress struct {
	duration time.Duration
	complete bool
}

type MusicSelection struct {
	URL, Title   string
	Index, Count int
}

var (
	ErrMusicQueueStart      = errors.New("already at the first music track")
	ErrMusicQueueEnd        = errors.New("already at the last music track")
	ErrMusicPositionUnknown = errors.New("music position is beyond known track boundaries")
)

// MusicQueue serves bounded MP3 sequences without retaining audio in memory.
type MusicQueue struct {
	base    *url.URL
	prefix  string
	http    *http.Client
	mu      sync.Mutex
	entries map[string]musicQueueEntry
	active  chan struct{}
	now     func() time.Time
}

func NewMusicQueue(base string, reader *WebReader) (*MusicQueue, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") || reader == nil || reader.http == nil {
		return nil, errors.New("music base URL must be HTTP/HTTPS without credentials, query or fragment")
	}
	client := &http.Client{Transport: reader.http.Transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("music redirect limit exceeded")
		}
		if err := validateWebURL(req.URL); err != nil {
			return err
		}
		if reader.http.CheckRedirect != nil {
			return reader.http.CheckRedirect(req, via)
		}
		return nil
	}}
	return &MusicQueue{base: u, prefix: strings.TrimRight(u.Path, "/") + "/music-stream/", http: client, entries: map[string]musicQueueEntry{}, active: make(chan struct{}, 4), now: time.Now}, nil
}

func (q *MusicQueue) Register(tracks []WebLink) (string, error) {
	if len(tracks) == 0 || len(tracks) > 64 {
		return "", errors.New("music queue requires between 1 and 64 MP3 tracks")
	}
	for _, track := range tracks {
		u, err := url.Parse(track.URL)
		if err != nil || len(track.URL) > 4096 || validateWebURL(u) != nil || mediaExtension(track.URL) != ".mp3" {
			return "", errors.New("music queue tracks must be public HTTP/HTTPS MP3 URLs")
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	for id, entry := range q.entries {
		if !entry.expires.After(now) {
			delete(q.entries, id)
		}
	}
	if len(q.entries) >= 16 {
		return "", errors.New("music queue capacity reached")
	}
	id := uuid.NewV4().String() + ".mp3"
	q.entries[id] = musicQueueEntry{tracks: slices.Clone(tracks), expires: now.Add(musicQueueTTL), progress: make([]musicTrackProgress, len(tracks))}
	u := q.base.Clone()
	u.Path = q.prefix + id
	u.RawPath = ""
	return u.String(), nil
}

func (q *MusicQueue) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, q.prefix)
	q.mu.Lock()
	entry, exists := q.entries[id]
	if exists && !entry.expires.After(q.now()) {
		delete(q.entries, id)
		exists = false
	}
	q.mu.Unlock()
	if !ok || !exists || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	start, offset, err := musicQuery(r.URL.Query(), len(entry.tracks))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "none")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	select {
	case q.active <- struct{}{}:
		defer func() { <-q.active }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many active music streams", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), entry.expires)
	defer cancel()
	w.Header().Set("Trailer", "X-Quasar-Music-Error")
	out := bufio.NewWriterSize(w, musicBufferSize)
	remaining := musicQueueLimit
	started := false
	for i := start; i < len(entry.tracks); i++ {
		skip := time.Duration(0)
		if i == start {
			skip = offset
		}
		observe := func(duration time.Duration, complete bool) {
			q.mu.Lock()
			defer q.mu.Unlock()
			if current, exists := q.entries[id]; exists && !current.progress[i].complete {
				if duration > current.progress[i].duration || complete {
					current.progress[i] = musicTrackProgress{duration: duration, complete: complete}
				}
			}
		}
		used, err := q.streamTrack(ctx, w, out, entry.tracks[i].URL, remaining, &started, skip, observe)
		slog.Info("music_stream_track", "index", i+1, "total", len(entry.tracks), "bytes", used, "ok", err == nil)
		remaining -= used
		if err != nil {
			if !started {
				w.Header().Del("Trailer")
				http.Error(w, err.Error(), http.StatusBadGateway)
			} else {
				_ = out.Flush()
				w.Header().Set("X-Quasar-Music-Error", err.Error())
			}
			return
		}
	}
	_ = out.Flush()
}

type mp3Format struct {
	version  uint32
	rate     int
	channels int
}

func (q *MusicQueue) streamTrack(ctx context.Context, w http.ResponseWriter, out *bufio.Writer, raw string, remaining int64, started *bool, skip time.Duration, observe func(time.Duration, bool)) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, errors.New("music stream canceled or expired")
	}
	u, err := url.Parse(raw)
	if err != nil || validateWebURL(u) != nil {
		return 0, errors.New("music track URL is blocked")
	}
	if remaining <= 0 {
		return 0, errors.New("music queue exceeds byte limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, errors.New("invalid music track URL")
	}
	req.Header.Set("User-Agent", "QuasarBridge/0.3 (music stream)")
	req.Header.Set("Accept", "audio/mpeg,*/*;q=0.5")
	resp, err := q.http.Do(req)
	if err != nil {
		return 0, errors.New("music track request failed or target blocked")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, errors.New("music track returned an unsuccessful HTTP status")
	}
	limit := min(musicTrackLimit, remaining)
	bounded := &io.LimitedReader{R: resp.Body, N: limit + 1}
	input := bufio.NewReaderSize(bounded, musicBufferSize)
	// Each MP3 frame carries its rate and channel mode. Validate every track's
	// first audio frame, but let the decoder follow valid changes at boundaries.
	frame, _, err := readMusicStart(input)
	used := func() int64 { return limit + 1 - bounded.N }
	if err != nil {
		return used(), err
	}
	consumed := used() - int64(input.Buffered())
	if consumed > limit {
		return used(), errors.New("music track or queue exceeds byte limit")
	}
	if !*started {
		w.WriteHeader(http.StatusOK)
		*started = true
	}
	framed := &musicFrameWriter{out: out, skip: skip, observe: observe}
	if _, err := framed.Write(frame); err != nil {
		return used(), errors.New("music client disconnected")
	}
	if err := out.Flush(); err != nil {
		return used(), errors.New("music client disconnected")
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if _, err := io.CopyBuffer(framed, io.LimitReader(input, limit-consumed), make([]byte, musicBufferSize)); err != nil {
		return used(), errors.New("music audio transfer failed")
	}
	var probe [1]byte
	if n, err := input.Read(probe[:]); n > 0 {
		return used(), errors.New("music track or queue exceeds byte limit")
	} else if err != nil && !errors.Is(err, io.EOF) {
		return used(), errors.New("music audio transfer failed")
	}
	if err := framed.finish(); err != nil {
		return used(), err
	}
	if err := out.Flush(); err != nil {
		return used(), errors.New("music client disconnected")
	}
	return used(), nil
}

func readMusicStart(input *bufio.Reader) ([]byte, mp3Format, error) {
	for {
		prefix, err := input.Peek(3)
		if err != nil {
			return nil, mp3Format{}, errors.New("music track contains no MP3 audio")
		}
		if string(prefix) != "ID3" {
			break
		}
		tag, err := input.Peek(10)
		if err != nil {
			return nil, mp3Format{}, errors.New("invalid MP3 ID3 header")
		}
		var size int64
		for _, b := range tag[6:10] {
			if b&128 != 0 {
				return nil, mp3Format{}, errors.New("invalid MP3 ID3 size")
			}
			size = size<<7 | int64(b)
		}
		size += 10
		if tag[3] == 4 && tag[5]&16 != 0 {
			size += 10
		}
		if size > musicTrackLimit {
			return nil, mp3Format{}, errors.New("MP3 metadata exceeds track limit")
		}
		if _, err := io.CopyN(io.Discard, input, size); err != nil {
			return nil, mp3Format{}, errors.New("incomplete MP3 metadata")
		}
	}
	frame, format, err := readMusicFrame(input)
	if err != nil {
		return nil, mp3Format{}, err
	}
	header := binary.BigEndian.Uint32(frame)
	side := 32
	if format.version != 3 {
		side = 17
	}
	if format.channels == 1 {
		side = 17
		if format.version != 3 {
			side = 9
		}
	}
	offset := 4 + side
	if header&(1<<16) == 0 {
		offset += 2
	}
	if len(frame) >= offset+4 && (string(frame[offset:offset+4]) == "Xing" || string(frame[offset:offset+4]) == "Info") {
		return readMusicFrame(input)
	}
	return frame, format, nil
}

func readMusicFrame(input *bufio.Reader) ([]byte, mp3Format, error) {
	first, err := input.Peek(4)
	if err != nil {
		return nil, mp3Format{}, errors.New("incomplete MP3 frame")
	}
	header := binary.BigEndian.Uint32(first)
	size, format, err := musicFrameInfo(header)
	if err != nil {
		return nil, mp3Format{}, err
	}
	frame := make([]byte, size)
	if _, err := io.ReadFull(input, frame); err != nil {
		return nil, mp3Format{}, errors.New("incomplete MP3 frame")
	}
	return frame, format, nil
}

func musicFrameInfo(header uint32) (int, mp3Format, error) {
	version, layer, rateIndex, bitrateIndex := header>>19&3, header>>17&3, header>>10&3, header>>12&15
	if header>>21 != 0x7ff || version == 1 || layer != 1 || rateIndex == 3 || bitrateIndex == 0 || bitrateIndex == 15 {
		return 0, mp3Format{}, errors.New("music queue requires MPEG Layer III audio")
	}
	rates := [...]int{44100, 48000, 32000}
	bitrate := [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[bitrateIndex]
	rate, factor := rates[rateIndex], 144000
	if version != 3 {
		bitrate = [...]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bitrateIndex]
		rate /= 2
		factor = 72000
		if version == 0 {
			rate /= 2
		}
	}
	channels := 2
	if header>>6&3 == 3 {
		channels = 1
	}
	return factor*bitrate/rate + int(header>>9&1), mp3Format{version: version, rate: rate, channels: channels}, nil
}

func musicQuery(query url.Values, count int) (int, time.Duration, error) {
	// Players may add their own tracking fields. They do not select tracks and
	// are never copied to source requests; only our selection fields are parsed.
	for _, key := range []string{"start", "offset_ms"} {
		if values, exists := query[key]; exists && len(values) != 1 {
			return 0, 0, errors.New("invalid music stream selection")
		}
	}
	start, offset := int64(0), int64(0)
	var err error
	if raw := query.Get("start"); raw != "" {
		start, err = strconv.ParseInt(raw, 10, 32)
	}
	if err != nil || start < 0 || start >= int64(count) {
		return 0, 0, errors.New("invalid music stream start index")
	}
	if raw := query.Get("offset_ms"); raw != "" {
		offset, err = strconv.ParseInt(raw, 10, 64)
	}
	if err != nil || offset < 0 || offset > musicQueueTTL.Milliseconds() {
		return 0, 0, errors.New("invalid music stream offset")
	}
	return int(start), time.Duration(offset) * time.Millisecond, nil
}

func (q *MusicQueue) locate(raw string, positionMS int64) (string, musicQueueEntry, int, time.Duration, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != q.base.Scheme || u.Host != q.base.Host || u.User != nil || u.Fragment != "" || positionMS < 0 || positionMS > musicQueueTTL.Milliseconds() {
		return "", musicQueueEntry{}, 0, 0, errors.New("invalid current music stream or position")
	}
	id, ok := strings.CutPrefix(u.Path, q.prefix)
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, exists := q.entries[id]
	if !ok || !exists || !entry.expires.After(q.now()) {
		return "", musicQueueEntry{}, 0, 0, errors.New("music queue is unavailable or expired")
	}
	start, offset, err := musicQuery(u.Query(), len(entry.tracks))
	if err != nil {
		return "", musicQueueEntry{}, 0, 0, err
	}
	remaining := offset + time.Duration(positionMS)*time.Millisecond
	if remaining > musicQueueTTL {
		return "", musicQueueEntry{}, 0, 0, errors.New("music position exceeds queue limit")
	}
	for i := start; i < len(entry.tracks); i++ {
		progress := entry.progress[i]
		if remaining == 0 || remaining < progress.duration {
			return id, entry, i, remaining, nil
		}
		if !progress.complete {
			return "", musicQueueEntry{}, 0, 0, ErrMusicPositionUnknown
		}
		remaining -= progress.duration
	}
	return "", musicQueueEntry{}, 0, 0, ErrMusicQueueEnd
}

// Tracks returns a copy of the remaining registered sources without fetching audio.
func (q *MusicQueue) Tracks(streamURL string) ([]WebLink, error) {
	_, entry, index, _, err := q.locate(streamURL, 0)
	if err != nil {
		return nil, err
	}
	return slices.Clone(entry.tracks[index:]), nil
}

func (q *MusicQueue) selection(id string, entry musicQueueEntry, index int, offset time.Duration) MusicSelection {
	u := q.base.Clone()
	u.Path, u.RawPath = q.prefix+id, ""
	query := url.Values{"start": {strconv.Itoa(index)}}
	if offset > 0 {
		query.Set("offset_ms", strconv.FormatInt(offset.Milliseconds(), 10))
	}
	u.RawQuery = query.Encode()
	return MusicSelection{URL: u.String(), Title: entry.tracks[index].Title, Index: index, Count: len(entry.tracks)}
}

func (q *MusicQueue) Step(streamURL string, positionMS int64, delta int) (MusicSelection, error) {
	if delta < -1 || delta > 1 {
		return MusicSelection{}, errors.New("invalid music step")
	}
	id, entry, index, _, err := q.locate(streamURL, positionMS)
	if err != nil {
		return MusicSelection{}, err
	}
	index += delta
	if index < 0 {
		return MusicSelection{}, ErrMusicQueueStart
	}
	if index >= len(entry.tracks) {
		return MusicSelection{}, ErrMusicQueueEnd
	}
	return q.selection(id, entry, index, 0), nil
}

func (q *MusicQueue) Resume(streamURL string, positionMS int64) (MusicSelection, error) {
	id, entry, index, offset, err := q.locate(streamURL, positionMS)
	if err != nil {
		return MusicSelection{}, err
	}
	return q.selection(id, entry, index, offset), nil
}

// Observe emitted frames before a downstream write can block on player buffering.
type musicFrameWriter struct {
	out            io.Writer
	skip, duration time.Duration
	observe        func(time.Duration, bool)
	pending        []byte
	unknown        bool
}

func (w *musicFrameWriter) Write(data []byte) (int, error) {
	if w.unknown {
		if w.duration < w.skip {
			return 0, ErrMusicPositionUnknown
		}
		return w.out.Write(data)
	}
	w.pending = append(w.pending, data...)
	for len(w.pending) >= 4 {
		if bytes.HasPrefix(w.pending, []byte("TAG")) && len(w.pending) <= 128 {
			break
		}
		size, format, err := musicFrameInfo(binary.BigEndian.Uint32(w.pending))
		if err != nil {
			w.unknown = true
			if w.duration < w.skip {
				return 0, ErrMusicPositionUnknown
			}
			_, err = w.out.Write(w.pending)
			w.pending = nil
			if err != nil {
				return 0, err
			}
			return len(data), nil
		}
		if len(w.pending) < size {
			break
		}
		samples := 1152
		if format.version != 3 {
			samples = 576
		}
		w.duration += time.Second * time.Duration(samples) / time.Duration(format.rate)
		if w.observe != nil {
			w.observe(w.duration, false)
		}
		if w.duration > w.skip {
			if _, err := w.out.Write(w.pending[:size]); err != nil {
				return 0, err
			}
		}
		w.pending = w.pending[size:]
	}
	return len(data), nil
}

func (w *musicFrameWriter) finish() error {
	if w.duration < w.skip {
		return ErrMusicPositionUnknown
	}
	complete := !w.unknown && (len(w.pending) == 0 || (len(w.pending) == 128 && bytes.HasPrefix(w.pending, []byte("TAG"))))
	if len(w.pending) > 0 {
		if _, err := w.out.Write(w.pending); err != nil {
			return err
		}
	}
	if w.observe != nil {
		w.observe(w.duration, complete)
	}
	return nil
}
