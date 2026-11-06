package bridge

import (
	"context"
	"quasarbridge/internal/dialogue"
)

type soundCloudDispatchGate struct {
	token chan struct{}
	refs  int
}

// DispatchSoundCloud orders final response batches for one speaker across its
// connections. Reservations may proceed concurrently; state locks are never
// held while waiting for the gate or writing a response.
func (e *Engine) DispatchSoundCloud(ctx context.Context, send func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := dialogue.SpeakerKey(ctx)
	e.dispatchMu.Lock()
	if e.dispatches == nil {
		e.dispatches = make(map[string]*soundCloudDispatchGate)
	}
	gate := e.dispatches[key]
	if gate == nil {
		gate = &soundCloudDispatchGate{token: make(chan struct{}, 1)}
		e.dispatches[key] = gate
	}
	gate.refs++
	e.dispatchMu.Unlock()
	defer func() {
		e.dispatchMu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(e.dispatches, key)
			if len(e.dispatches) == 0 {
				e.dispatches = nil
			}
		}
		e.dispatchMu.Unlock()
	}()
	select {
	case gate.token <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate.token }()
	if err := ctx.Err(); err != nil {
		return err
	}
	return send()
}

func (n *Native) dispatchSound(p *nativeRequest, send func() error) error {
	n.mu.Lock()
	own := p.silentEvent || p.route == "soundcloud" || (p.soundRequest != nil && p.soundRequest.Provider == "SoundCloud")
	n.mu.Unlock()
	if dispatcher, ok := n.resolver.(interface {
		DispatchSoundCloud(context.Context, func() error) error
	}); ok && own {
		return dispatcher.DispatchSoundCloud(p.ctx, send)
	}
	return send()
}
