package sound

import "context"

type requestSequenceKey struct{}

// WithRequestSequence carries arrival order across ASR and routing. Music
// providers use it for playback supersession independently of dialogue turns.
func WithRequestSequence(ctx context.Context, sequence uint64) context.Context {
	return context.WithValue(ctx, requestSequenceKey{}, sequence)
}

// RequestSequence returns zero for unsequenced callers and lifecycle callbacks.
func RequestSequence(ctx context.Context) uint64 {
	sequence, _ := ctx.Value(requestSequenceKey{}).(uint64)
	return sequence
}
