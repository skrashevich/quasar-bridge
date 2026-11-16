package bridge

// thinkingCue appends original pre-encoded tones to the existing Dialog stream.
// The encoder is a build-time tool only. No URL, file player or model tool is
// involved. Preserve the provider's stream headers, sequence and granule clock.
// Called under Native.mu, once, after discarded setup speech has completed.
func (o *oggSpeech) thinkingCue() []byte {
	if len(o.head) < 19 || o.head[18] != 0 || o.head[9] < 1 || o.head[9] > 2 {
		return nil // Only standard mono/stereo Opus channel mapping is supported.
	}
	out := o.flushPending(0, 0)
	for _, packet := range thinkingCuePackets {
		o.granule += 960 // Every generated packet is a 20ms frame at 48kHz.
		out = append(out, o.page(packet, 0, o.granule)...)
	}
	return out
}
