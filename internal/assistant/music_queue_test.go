package assistant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func musicFrame(bitrateIndex, rateIndex byte, marker byte) []byte {
	rates := [...]int{44100, 48000, 32000}
	bitrates := [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	frame := bytes.Repeat([]byte{marker}, 144000*bitrates[bitrateIndex]/rates[rateIndex])
	binary.BigEndian.PutUint32(frame, 0xfffb0000|uint32(bitrateIndex)<<12|uint32(rateIndex)<<10)
	return frame
}

func queueRequest(t *testing.T, queue *MusicQueue, method, raw string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, u.RequestURI(), nil)
	request.Header.Set("Range", "bytes=0-100")
	queue.ServeHTTP(recorder, request)
	return recorder
}

func TestMusicQueueStreamsTracksWithoutMetadata(t *testing.T) {
	first := musicFrame(9, 0, 0x11)
	second := musicFrame(11, 0, 0x22)
	xing := musicFrame(9, 0, 0)
	copy(xing[36:], "Xing")
	id3 := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 4, 1, 2, 3, 4}
	fetches := 0
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Timeout: time.Nanosecond, Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		fetches++
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) < time.Minute {
			t.Error("short web-reader timeout leaked into the music stream")
		}
		data := append(append(bytes.Clone(id3), xing...), first...)
		if r.URL.Path == "/two.mp3" {
			data = append(bytes.Clone(id3), second...)
		}
		if r.URL.RawQuery != "token=keep" {
			t.Error("source query was changed")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	queue, err := NewMusicQueue("http://127.0.0.1:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/one.mp3?token=keep"}, {URL: "https://audio.example/two.mp3?token=keep"}})
	if err != nil || fetches != 0 || !strings.HasPrefix(stream, "http://127.0.0.1:8443/music-stream/") {
		t.Fatalf("registration fetched audio or returned an invalid stream: %v", err)
	}
	if head := queueRequest(t, queue, http.MethodHead, stream); head.Code != http.StatusOK || head.Body.Len() != 0 || fetches != 0 {
		t.Fatal("HEAD downloaded audio")
	}
	response := queueRequest(t, queue, http.MethodGet, stream)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), append(bytes.Clone(first), second...)) || fetches != 2 {
		t.Fatalf("queue did not concatenate clean MP3 frames: status=%d bytes=%d fetches=%d", response.Code, response.Body.Len(), fetches)
	}
	if response.Header().Get("Content-Length") != "" || response.Header().Get("Accept-Ranges") != "none" || response.Header().Get("Content-Type") != "audio/mpeg" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("wrong streaming headers: %v", response.Header())
	}
	if current, err := queue.Step(stream, 30, 0); err != nil || current.Index != 1 {
		t.Fatalf("duration included stripped metadata frames: selection=%+v error=%v", current, err)
	}
}

func TestMusicQueueStepUsesPlayedOffsetAndStartsAtSelectedTrack(t *testing.T) {
	frames := map[string][]byte{
		"/one.mp3":   musicFrame(9, 1, 0x11),
		"/two.mp3":   musicFrame(11, 0, 0x22),
		"/three.mp3": musicFrame(9, 1, 0x33),
	}
	var fetched []string
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(frames[r.URL.Path]))}, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/one.mp3", Title: "First"}, {URL: "https://audio.example/two.mp3", Title: "Second"}, {URL: "https://audio.example/three.mp3", Title: "Third"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Step(stream, 25, 1); err == nil {
		t.Fatal("unknown track boundaries guessed before fetching audio")
	}
	queueRequest(t, queue, http.MethodGet, stream) // Entire queue prefetched; this does not advance playback.
	current, err := queue.Step(stream, 25, 0)
	if err != nil || current.Index != 1 || current.Title != "Second" || current.Count != 3 {
		t.Fatalf("download progress replaced played position: selection=%+v error=%v", current, err)
	}
	next, err := queue.Step(stream, 25, 1)
	if err != nil || next.Index != 2 || !strings.HasSuffix(next.URL, "?start=2") {
		t.Fatalf("wrong next selection: %+v error=%v", next, err)
	}
	fetched = nil
	response := queueRequest(t, queue, http.MethodGet, next.URL)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), frames["/three.mp3"]) || len(fetched) != 1 || fetched[0] != "/three.mp3" {
		t.Fatal("start endpoint fetched or replayed earlier tracks")
	}
	previous, err := queue.Step(next.URL, 0, -1)
	if err != nil || previous.Index != 1 {
		t.Fatalf("offset was not relative to the new stream start: %+v error=%v", previous, err)
	}
	for _, tc := range []struct {
		url      string
		position int64
		delta    int
	}{
		{stream, 0, -1}, {next.URL, 0, 1}, {stream, 1000, 0}, {stream, -1, 1}, {stream, 0, 2},
		{strings.Replace(stream, "localhost:8443", "foreign.example", 1), 0, 1},
	} {
		if _, err := queue.Step(tc.url, tc.position, tc.delta); err == nil {
			t.Errorf("invalid or out-of-range step accepted: %+v", tc)
		}
	}
	for _, query := range []string{"?start=-1", "?start=99", "?start=bad", "?start=1&start=2"} {
		if response := queueRequest(t, queue, http.MethodHead, stream+query); response.Code != http.StatusBadRequest {
			t.Errorf("invalid start accepted: %q status=%d", query, response.Code)
		}
	}
	resumed, err := queue.Resume(stream, 60)
	if err != nil || resumed.Index != 2 || !strings.Contains(resumed.URL, "offset_ms=9") {
		t.Fatalf("resume lost offset within current track: selection=%+v error=%v", resumed, err)
	}
	if current, err := queue.Step(resumed.URL, 0, 0); err != nil || current.Index != 2 || strings.Contains(current.URL, "offset_ms") {
		t.Fatalf("restart did not account for original URL offset: selection=%+v error=%v", current, err)
	}
}

func TestMusicQueueResumeSkipsWholeFrames(t *testing.T) {
	first, second, third := musicFrame(9, 1, 0x11), musicFrame(9, 1, 0x22), musicFrame(9, 1, 0x33)
	data := append(append(bytes.Clone(first), second...), third...)
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/test.mp3"}})
	if err != nil {
		t.Fatal(err)
	}
	queueRequest(t, queue, http.MethodGet, stream)
	selection, err := queue.Resume(stream, 30) // One complete 24ms frame, then 6ms into the second.
	if err != nil {
		t.Fatal(err)
	}
	response := queueRequest(t, queue, http.MethodGet, selection.URL)
	if !bytes.Equal(response.Body.Bytes(), append(bytes.Clone(second), third...)) {
		t.Fatalf("resume did not quantize to an MP3 frame boundary: bytes=%d", response.Body.Len())
	}
}

func TestMusicQueuePlayerTrackingQueryDoesNotChangeSelection(t *testing.T) {
	first, second := musicFrame(9, 1, 0x11), musicFrame(9, 1, 0x22)
	data := append(bytes.Clone(first), second...)
	fetches := 0
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		fetches++
		if r.URL.RawQuery != "source=keep" {
			t.Errorf("player tracking reached source or source query changed: %q", r.URL.RawQuery)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/test.mp3?source=keep"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		want  []byte
	}{
		{"?vsid=player-session", data},
		{"?start=0&offset_ms=30&vsid=player-session&unused=1", second},
	} {
		before := fetches
		if head := queueRequest(t, queue, http.MethodHead, stream+tc.query); head.Code != http.StatusOK || fetches != before {
			t.Fatalf("player tracking blocked HEAD or downloaded audio: status=%d", head.Code)
		}
		response := queueRequest(t, queue, http.MethodGet, stream+tc.query)
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), tc.want) {
			t.Fatalf("player tracking changed selection: status=%d bytes=%d want=%d", response.Code, response.Body.Len(), len(tc.want))
		}
	}
}

type musicSignalWriter struct {
	*httptest.ResponseRecorder
	ready chan struct{}
	once  sync.Once
}

func (w *musicSignalWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	w.once.Do(func() { close(w.ready) })
	return n, err
}

func TestMusicQueueStepWorksBeforeFirstTrackDownloadCompletes(t *testing.T) {
	input, source := io.Pipe()
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		var body io.ReadCloser = input
		if r.URL.Path == "/two.mp3" {
			body = io.NopCloser(bytes.NewReader(musicFrame(9, 1, 0x22)))
		}
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/one.mp3"}, {URL: "https://audio.example/two.mp3"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	response := &musicSignalWriter{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		queue.ServeHTTP(response, httptest.NewRequest(http.MethodGet, stream, nil).WithContext(ctx))
	}()
	t.Cleanup(func() { cancel(); _ = source.Close(); _ = input.Close(); <-done })
	if _, err := source.Write(musicFrame(9, 1, 0x11)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-response.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("first frame was not delivered")
	}
	if next, err := queue.Step(stream, 10, 1); err != nil || next.Index != 1 {
		t.Fatalf("next waited for whole first track: selection=%+v error=%v", next, err)
	}
	if _, err := queue.Step(stream, 30, 0); !errors.Is(err, ErrMusicPositionUnknown) {
		t.Fatalf("unknown position beyond scanned prefix was guessed: %v", err)
	}
}

func TestMusicQueueLimitsAndExpiration(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	for _, base := range []string{"", "file:///tmp", "https://user:secret@example.org", "https://example.org/?x=1", "https://example.org/#part"} {
		if _, err := NewMusicQueue(base, reader); err == nil {
			t.Errorf("invalid base accepted: %q", base)
		}
	}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tracks := range [][]WebLink{nil, {{URL: "file:///tmp/test.mp3"}}, {{URL: "http://" + net.IPv4(10, 0, 0, 1).String() + "/test.mp3"}}, {{URL: "https://audio.example/test.wav"}}, make([]WebLink, 65)} {
		if _, err := queue.Register(tracks); err == nil {
			t.Error("invalid track selection accepted")
		}
	}
	now := time.Now()
	queue.now = func() time.Time { return now }
	var oldest string
	for range 16 {
		oldest, err = queue.Register([]WebLink{{URL: "https://audio.example/test.mp3"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := queue.Register([]WebLink{{URL: "https://audio.example/test.mp3"}}); err == nil {
		t.Fatal("unbounded queue registry")
	}
	now = now.Add(2*time.Hour + time.Second)
	if response := queueRequest(t, queue, http.MethodHead, oldest); response.Code != http.StatusNotFound {
		t.Fatal("expired queue remained available")
	}
	if _, err := queue.Register([]WebLink{{URL: "https://audio.example/test.mp3"}}); err != nil {
		t.Fatal("expired queues retained capacity")
	}
}

func TestMusicQueueTracksSnapshot(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http.Transport = roundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("reading registered source tracks must not fetch pages or audio")
		return nil, nil
	})
	queue, err := NewMusicQueue("https://bridge.example", reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	queue.now = func() time.Time { return now }
	want := []WebLink{
		{URL: "https://audio.example/one.mp3?version=first&token=one", Title: "First track"},
		{URL: "https://audio.example/two.mp3?version=second&token=two", Title: "Second track"},
	}
	stream, err := queue.Register(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := queue.Tracks(stream)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("source tracks=%+v error=%v", got, err)
	}
	got[0].URL, got[0].Title = "https://other.example/replaced.mp3", "Replaced title"
	if again, err := queue.Tracks(stream); err != nil || !slices.Equal(again, want) {
		t.Fatalf("returned slice changed the queue: tracks=%+v error=%v", again, err)
	}
	if remaining, err := queue.Tracks(stream + "?start=1"); err != nil || !slices.Equal(remaining, want[1:]) {
		t.Fatalf("wrong remaining source tracks: tracks=%+v error=%v", remaining, err)
	}
	for _, invalid := range []string{
		strings.Replace(stream, "bridge.example", "other.example", 1),
		stream + "?start=2",
		"https://bridge.example/music-stream/missing.mp3",
	} {
		if tracks, err := queue.Tracks(invalid); err == nil || len(tracks) != 0 {
			t.Fatalf("invalid stream returned sources: tracks=%+v error=%v", tracks, err)
		}
	}
	now = now.Add(musicQueueTTL + time.Second)
	if tracks, err := queue.Tracks(stream); err == nil || len(tracks) != 0 {
		t.Fatalf("expired stream returned sources: tracks=%+v error=%v", tracks, err)
	}
}

func TestMusicQueueRejectsNonMP3Tracks(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		data := musicFrame(9, 0, 0x11)
		if r.URL.Path == "/two.mp3" {
			data = []byte("this file is not MPEG Layer III audio")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/one.mp3"}, {URL: "https://audio.example/two.mp3"}})
	if err != nil {
		t.Fatal(err)
	}
	response := queueRequest(t, queue, http.MethodGet, stream)
	if !bytes.Equal(response.Body.Bytes(), musicFrame(9, 0, 0x11)) || !strings.Contains(response.Result().Trailer.Get("X-Quasar-Music-Error"), "MPEG Layer III") {
		t.Fatalf("non-MP3 audio concatenated without a clear error: bytes=%d trailers=%v", response.Body.Len(), response.Result().Trailer)
	}
}

func TestMusicQueuePreservesMP3RateAndChannelChanges(t *testing.T) {
	first := musicFrame(9, 1, 0x11)
	second := musicFrame(11, 0, 0x22)
	second[3] = 0xc0 // A valid mono MPEG frame between stereo tracks.
	third := musicFrame(9, 1, 0x33)
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		data := first
		switch r.URL.Path {
		case "/two.mp3":
			data = second
		case "/three.mp3":
			data = third
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/one.mp3"}, {URL: "https://audio.example/two.mp3"}, {URL: "https://audio.example/three.mp3"}})
	if err != nil {
		t.Fatal(err)
	}
	response := queueRequest(t, queue, http.MethodGet, stream)
	want := append(append(bytes.Clone(first), second...), third...)
	if !bytes.Equal(response.Body.Bytes(), want) || response.Result().Trailer.Get("X-Quasar-Music-Error") != "" {
		t.Fatalf("valid changing MP3 format truncated: bytes=%d want=%d trailer=%v", response.Body.Len(), len(want), response.Result().Trailer)
	}
}

func TestMusicQueueConcurrentStreamsBounded(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	started, release := make(chan struct{}, 4), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	reader.http = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		select {
		case <-release:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(musicFrame(9, 0, 0x11)))}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/test.mp3"}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { queueRequest(t, queue, http.MethodGet, stream) })
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("stream did not start")
		}
	}
	if response := queueRequest(t, queue, http.MethodGet, stream); response.Code != http.StatusServiceUnavailable {
		t.Fatal("more than four downloads accepted")
	}
	once.Do(func() { close(release) })
	wg.Wait()
}

func TestMusicQueueCanceledRequestDoesNotFetch(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	reader.http = &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("canceled request downloaded audio")
		return nil, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := queue.Register([]WebLink{{URL: "https://audio.example/test.mp3"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := httptest.NewRequest(http.MethodGet, stream, nil).WithContext(ctx)
	queue.ServeHTTP(httptest.NewRecorder(), request)
}

func TestMusicQueueByteBudgetDoesNotForwardOverflowProbe(t *testing.T) {
	reader := NewWebReader(DefaultConfig())
	data := append(musicFrame(9, 0, 0x11), musicFrame(9, 0, 0x22)...)
	reader.http = &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})}
	queue, err := NewMusicQueue("http://localhost:8443", reader)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	out := bufio.NewWriter(response)
	started := false
	_, err = queue.streamTrack(t.Context(), response, out, "https://audio.example/test.mp3", 500, &started, 0, nil)
	if flushErr := out.Flush(); flushErr != nil {
		t.Fatal(flushErr)
	}
	if err == nil || !strings.Contains(err.Error(), "limit") || response.Body.Len() > 500 || response.Body.Len() < len(musicFrame(9, 0, 0x11)) {
		t.Fatalf("overflow probe reached player or missing limit error: bytes=%d error=%v", response.Body.Len(), err)
	}
}
