package bridge

import (
	"context"
	"errors"
	"strings"
	"sync"

	"quasarbridge/internal/llm"
	"quasarbridge/internal/reply"
)

// speechStream belongs to a single resolver call. Its producer is the SSE
// reader; synthesis runs in a bounded, ordered queue.
type speechStream struct {
	n       *Native
	p       *nativeRequest
	stop    func()
	stopped sync.Once
	chunks  speechChunks
	queue   *speechQueue
	seen    bool
	emitted bool
}

func (s *speechStream) stopProgress() { s.stopped.Do(s.stop) }

func (s *speechStream) enqueue(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := llm.ValidateAnswer(text); err != nil {
		return err
	}
	if s.queue == nil {
		s.stopProgress()
		if err := s.n.startDeferred(s.p); err != nil {
			s.p.cancel()
			return err
		}
		s.queue = newSpeechQueue(s.p.ctx, func(ctx context.Context, part string) error {
			return s.n.synthesizeSegment(ctx, s.p, part)
		})
	}
	if err := s.queue.add(text); err != nil {
		return err
	}
	s.emitted = true
	return nil
}

func (s *speechStream) delta(text string) error {
	s.seen = true
	for _, chunk := range s.chunks.add(text) {
		if err := s.enqueue(chunk); err != nil {
			return err
		}
	}
	return nil
}

// finish is called once after the model loop has stopped producing deltas.
// If there were no deltas the caller should use the ordinary complete reply.
func (s *speechStream) finish(resolveErr error) (bool, error) {
	s.stopProgress()
	if resolveErr == nil && s.seen {
		if tail := s.chunks.flush(); tail != "" {
			if err := s.enqueue(tail); err != nil {
				resolveErr = err
			}
		}
	}
	if s.queue == nil {
		return false, resolveErr
	}
	// A provider error must not cancel an in-flight TTS request: wait for
	// already accepted segments before ending the shared Ogg stream. Request
	// cancellation still propagates immediately through the queue's context.
	queueErr := s.queue.finish()
	if resolveErr == nil {
		resolveErr = queueErr
	}
	if s.p.ctx.Err() != nil {
		return true, s.p.ctx.Err()
	}
	if !s.emitted {
		return false, errors.Join(resolveErr, errors.New("empty streamed response"))
	}
	if resolveErr != nil {
		// Speech already played cannot be retracted; do not replay a complete
		// answer or claim that an interrupted answer finished successfully.
		s.n.discardSound(s.p)
	}
	s.n.mu.Lock()
	started := s.p.audioStarted
	s.n.mu.Unlock()
	if !started {
		s.p.cancel()
		return true, errors.Join(resolveErr, errors.New("stream audio did not start"))
	}
	if err := s.n.deferredPart(s.p, "", true); err != nil {
		return true, errors.Join(resolveErr, err)
	}
	audioErr := s.n.finishSilentAudio(s.p)
	if audioErr == nil && resolveErr == nil {
		reply.Delivered(s.p.ctx)
	}
	return true, errors.Join(resolveErr, audioErr)
}
