package soundcloud

import (
	"sync"
	"testing"
)

func TestLiveDefaultPreservesPlaybackQueue(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "speaker", r)
	first := providerStart(t, p, ctx, r)
	p.SetDefault(false)
	if p.IsDefault() || p.CanHandle(ctx, "включи песню Second") || !p.CanHandle(ctx, "включи песню Second на SoundCloud") || !p.CanDeliver(ctx, first) {
		t.Fatal("default change affected explicit routing or current stream")
	}
	if _, err := p.HandleEvent(ctx, EventPrefix+"finished", eventPayload(first)); err != nil {
		t.Fatal(err)
	}
	if got := r.all(); len(got) != 2 || got[1].Title != "Second" {
		t.Fatal(got)
	}
	p.SetDefault(true)
	if !p.CanHandle(ctx, "включи песню First") {
		t.Fatal("live default not used")
	}
}

func TestLiveDefaultConcurrentRouting(t *testing.T) {
	p := providerFixture(t, nil)
	r := new(providerRecorder)
	ctx := providerContext(t, "speaker", r)
	providerStart(t, p, ctx, r)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			for range 20 {
				p.SetDefault(i%2 == 0)
				p.IsDefault()
				p.CanHandle(ctx, "включи песню First")
				p.Handle(ctx, "что сейчас играет на SoundCloud")
			}
		})
	}
	wg.Wait()
}
