package assistant

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/sound"
)

type musicBinding struct {
	streamID    string
	url         string
	title       string
	queued      bool
	paused      bool
	offsetMS    int64
	offsetKnown bool
}

type musicPlaybackEntry struct {
	active  *musicBinding
	pending *musicBinding
	at      time.Time
}

// MusicPlayback binds music commands to a speaker-reported stream identity.
type MusicPlayback struct {
	queue   *MusicQueue
	mu      sync.Mutex
	entries map[string]*musicPlaybackEntry
	now     func() time.Time
}

func NewMusicPlayback(queue *MusicQueue) *MusicPlayback {
	return &MusicPlayback{queue: queue, entries: map[string]*musicPlaybackEntry{}, now: time.Now}
}

func (m *MusicPlayback) Start(ctx context.Context, tracks []WebLink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := dialogue.SpeakerKey(ctx)
	if m == nil || key == "" || !sound.HasPlayer(ctx) {
		return errors.New("play_music requires a current speaker request")
	}
	if len(tracks) == 0 {
		return errors.New("music requires at least one track")
	}
	queued := mediaExtension(tracks[0].URL) == ".mp3"
	stream := tracks[0].URL
	isolatedQueue, isolated := ctx.Value(isolatedMusicQueueKey{}).(*MusicQueue)
	if queued {
		queue := m.queue
		if isolated {
			queue = isolatedQueue
		}
		if queue == nil {
			return errors.New("music queue is unavailable")
		}
		var err error
		stream, err = queue.Register(tracks)
		if err != nil {
			return err
		}
	} else if len(tracks) != 1 {
		return errors.New("music queues require MP3 tracks")
	}
	request := sound.Request{URL: stream, StreamID: sound.NewStreamID(), Title: tracks[0].Title}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := sound.Play(ctx, request); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if isolated {
		return nil
	}
	m.pruneLocked()
	e := m.entries[key]
	if e == nil {
		if len(m.entries) >= 128 {
			var oldest string
			var at time.Time
			for key, entry := range m.entries {
				if at.IsZero() || entry.at.Before(at) {
					oldest, at = key, entry.at
				}
			}
			delete(m.entries, oldest)
		}
		e = new(musicPlaybackEntry)
		m.entries[key] = e
	}
	e.pending = &musicBinding{streamID: request.StreamID, url: stream, title: request.Title, queued: queued, offsetMS: -1}
	e.at = m.now()
	return nil
}

func (m *MusicPlayback) pruneLocked() {
	now := m.now()
	for key, entry := range m.entries {
		if now.Sub(entry.at) >= musicQueueTTL {
			delete(m.entries, key)
		}
	}
}

// currentLocked never infers which track is playing from an HTTP download.
func (m *MusicPlayback) currentLocked(ctx context.Context) (*musicPlaybackEntry, *musicBinding) {
	key := dialogue.SpeakerKey(ctx)
	state := sound.CurrentPlayback(ctx)
	if key == "" || !state.Present || ctx.Err() != nil {
		return nil, nil
	}
	e := m.entries[key]
	if e == nil || m.now().Sub(e.at) >= musicQueueTTL {
		return nil, nil
	}
	if state.StreamID != "" {
		if e.pending != nil && e.pending.streamID == state.StreamID {
			e.active, e.pending = e.pending, nil
		}
		if e.active == nil || e.active.streamID != state.StreamID {
			return nil, nil
		}
		if state.PlayerState == "Playing" || state.PlayerState == "Buffering" {
			e.active.paused = false
		}
		if (!e.active.paused || state.PlayerState == "Paused") && state.OffsetKnown {
			e.active.offsetMS = state.OffsetMS
			e.active.offsetKnown = state.OffsetMS >= 0
		}
	} else if e.active == nil || !e.active.paused {
		return nil, nil
	}
	e.at = m.now()
	return e, e.active
}

func (m *MusicPlayback) Active(ctx context.Context) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, binding := m.currentLocked(ctx)
	return binding != nil
}

func (m *MusicPlayback) Control(ctx context.Context, action string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m == nil {
		return "На этой колонке сейчас нет моей музыкальной очереди.", nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, binding := m.currentLocked(ctx)
	if binding == nil {
		return "На этой колонке сейчас нет моей музыкальной очереди.", nil
	}
	if action == "pause" || action == "stop" {
		if err := sound.Play(ctx, sound.Request{Command: action}); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		binding.paused = true
		slog.Info("music_control", "action", action, "offset_ms", binding.offsetMS, "ok", true)
		if action == "stop" {
			return "Музыка остановлена.", nil
		}
		return "Музыка на паузе.", nil
	}
	request := sound.Request{URL: binding.url, Title: binding.title, StreamID: sound.NewStreamID()}
	var selection MusicSelection
	known := sound.CurrentPlayback(ctx).OffsetKnown
	if binding.paused {
		known = binding.offsetKnown
	}
	if !known || binding.offsetMS < 0 {
		return "Не удалось определить позицию трека. Попробуйте ещё раз.", nil
	}
	if binding.queued {
		if m.queue == nil {
			return "Музыкальная очередь больше недоступна.", nil
		}
		var err error
		switch action {
		case "next":
			selection, err = m.queue.Step(binding.url, binding.offsetMS, 1)
		case "previous", "prev":
			selection, err = m.queue.Step(binding.url, binding.offsetMS, -1)
		case "restart":
			selection, err = m.queue.Step(binding.url, binding.offsetMS, 0)
		case "resume":
			selection, err = m.queue.Resume(binding.url, binding.offsetMS)
		default:
			return "", errors.New("unknown music control action")
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrMusicQueueStart):
				return "Это первый трек в очереди.", nil
			case errors.Is(err, ErrMusicQueueEnd):
				return "Это последний трек в очереди.", nil
			case errors.Is(err, ErrMusicPositionUnknown):
				return "Не удалось определить позицию трека. Попробуйте ещё раз.", nil
			default:
				return "Музыкальная очередь больше недоступна. Включите музыку заново.", nil
			}
		}
		request.URL, request.Title = selection.URL, selection.Title
	} else {
		switch action {
		case "next", "previous", "prev":
			return "В этом потоке нельзя переключать треки.", nil
		case "resume":
			request.OffsetMS = binding.offsetMS
		case "restart":
		default:
			return "", errors.New("unknown music control action")
		}
	}
	if err := sound.Play(ctx, request); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	e.pending = &musicBinding{streamID: request.StreamID, url: request.URL, title: request.Title, queued: binding.queued, offsetMS: -1}
	e.at = m.now()
	fields := []any{"action", action, "offset_ms", binding.offsetMS, "ok", true}
	if selection.Count > 0 {
		fields = append(fields, "track_index", selection.Index+1, "track_total", selection.Count)
	}
	slog.Info("music_control", fields...)
	switch action {
	case "next":
		return "Включаю следующий трек.", nil
	case "previous", "prev":
		return "Включаю предыдущий трек.", nil
	case "restart":
		return "Включаю трек с начала.", nil
	default:
		return "Продолжаю воспроизведение.", nil
	}
}
