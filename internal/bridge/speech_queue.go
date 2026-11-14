package bridge

import (
	"context"
	"errors"
	"sync"
)

// speechQueue serializes synthesis so audio segments never overlap. An
// bounded queue permits synthesis and generation to overlap without allowing
// an unbounded backlog when the provider generates faster than speech.
type speechQueue struct {
	ctx    context.Context
	cancel context.CancelFunc
	items  chan string
	done   chan struct{}
	send   func(context.Context, string) error
	mu     sync.Mutex
	err    error
}

func newSpeechQueue(parent context.Context, send func(context.Context, string) error) *speechQueue {
	ctx, cancel := context.WithCancel(parent)
	q := &speechQueue{ctx: ctx, cancel: cancel, items: make(chan string, 8), done: make(chan struct{}), send: send}
	go q.run()
	return q
}

func (q *speechQueue) run() {
	defer close(q.done)
	for {
		select {
		case <-q.ctx.Done():
			q.setError(q.ctx.Err())
			return
		case text, ok := <-q.items:
			if !ok {
				return
			}
			if err := q.send(q.ctx, text); err != nil {
				q.setError(err)
				q.cancel()
				return
			}
		}
	}
}

func (q *speechQueue) setError(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err == nil {
		q.err = err
	}
}

func (q *speechQueue) error() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.err
}

// add and finish must be called serially by the producer.
func (q *speechQueue) add(text string) error {
	if err := q.ctx.Err(); err != nil {
		return err
	}
	select {
	case <-q.done:
		if err := q.error(); err != nil {
			return err
		}
		return errors.New("speech queue closed")
	case <-q.ctx.Done():
		return q.ctx.Err()
	case q.items <- text:
		return nil
	}
}

func (q *speechQueue) finish() error {
	close(q.items)
	<-q.done
	q.cancel()
	return q.error()
}
