package bridge

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	app "github.com/skrashevich/quasar-go-genproto/proto/alice/library/client/protos"
	asr "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result"
	recognition "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/asr_result/common"
	cancellation "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/cancel_request"
	header "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/journal"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/soundcloud"
	"quasarbridge/internal/wire"
)

type soundCloudHTTPTransport func(*http.Request) (*http.Response, error)

func (f soundCloudHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func soundCloudIntegrationProvider(t *testing.T, handler http.HandlerFunc) *soundcloud.Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	origin, _ := url.Parse(server.URL)
	hc := &http.Client{Transport: soundCloudHTTPTransport(func(r *http.Request) (*http.Response, error) {
		req := r.Clone(r.Context())
		req.URL = r.URL.Clone()
		req.URL.Scheme = origin.Scheme
		req.URL.Host = origin.Host
		return http.DefaultTransport.RoundTrip(req)
	})}
	cfg := soundcloud.Config{Enabled: true, Default: true, API: "official", AccessToken: "fixture"}
	c, err := soundcloud.NewClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := soundcloud.NewRelay(server.URL, hc)
	if err != nil {
		t.Fatal(err)
	}
	return soundcloud.NewProvider(cfg, c, relay)
}
func soundCloudCatalogFixture(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/streams") {
		fmt.Fprint(w, `{"hls_aac_160_url":"https://cf-media.sndcdn.com/test.m3u8"}`)
		return
	}
	fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000,"metadata_artist":"Artist"}]}`)
}
func soundCloudTextInput(id, text string) []byte {
	f := wire.Decode(soundInput(id), wire.ClientToServer)
	f.Client.GetEvent().GetTextInput().GetRequest().GetEvent().Text = new(text)
	return aa(f.Client)
}

type soundCloudDelayedDecision struct {
	*Engine
	entered, release chan struct{}
}

func (r *soundCloudDelayedDecision) Decide(ctx context.Context, text string) RouteDecision {
	d := r.Engine.Decide(ctx, text)
	if text == "включи First SoundCloud" {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	}
	return d
}
func TestNativeSoundCloudInformationDoesNotDiscardPlayback(t *testing.T) {
	for _, delay := range []string{"catalog", "decision"} {
		t.Run(delay, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			provider := soundCloudIntegrationProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if delay == "catalog" && !strings.HasSuffix(r.URL.Path, "/streams") {
					once.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				soundCloudCatalogFixture(w, r)
			})
			engine := &Engine{SoundCloud: provider, Dialogue: dialogue.New(dialogue.DefaultConfig())}
			var resolver Resolver = engine
			if delay == "decision" {
				resolver = &soundCloudDelayedDecision{Engine: engine, entered: entered, release: release}
			}
			n, _, down := soundSession(t, resolver)
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("play", "включи First SoundCloud")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("playback did not reach hold")
			}
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("info", "что играет в SoundCloud")); err != nil {
				close(release)
				t.Fatal(err)
			}
			// Information finishes while playback is independently pending.
			for {
				f := nextSoundFrame(t, down)
				if f.Server.GetEvent().GetAliceResponse() != nil {
					if f.Server.GetEvent().GetHeader().GetRefMessageId() != "info" {
						t.Fatal("unexpected response before release")
					}
					break
				}
			}
			close(release)
			ds := finalSoundDirectives(t, down)
			if len(ds) != 1 || ds[0].GetName() != "audio_play" {
				t.Fatal("valid playback was discarded after information")
			}
			n.wg.Wait()
		})
	}
}

func TestProxySoundCloudCallbacksDoNotJournalConversations(t *testing.T) {
	w := new(journal.Writer)
	cfg := journal.DefaultConfig()
	cfg.Enabled = true
	cfg.Path = filepath.Join(t.TempDir(), "conversation.jsonl")
	if err := w.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	var upstreamCount, observerCount atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, _, err := c.Read(r.Context())
			if err != nil {
				return
			}
			upstreamCount.Add(1)
		}
	}))
	t.Cleanup(upstream.Close)
	engine := &Engine{SoundCloud: soundCloudStub{can: true, handle: func(context.Context, string) (string, error) { return "", nil }, event: func(ctx context.Context, name string, _ map[string]any) (bool, error) {
		if name == "quasar_soundcloud_finished" {
			return true, sound.Play(ctx, sound.Request{URL: "https://audio.example/next.m3u8"})
		}
		return true, nil
	}}}
	natives := make(chan *Native, 1)
	p := &Proxy{Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http"), Journal: w, Observer: func(string, websocket.MessageType, []byte) { observerCount.Add(1) }, NewSession: func(ctx context.Context, up, down Sender) Session {
		n := NewNative(ctx, up, down, engine)
		natives <- n
		return n
	}}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); p.ServeHTTP(w, r) }))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	n := <-natives
	scoped := dialogue.WithSpeaker(t.Context(), "connection:"+n.connectionScope)
	prepared := engine.Prepare(scoped)
	_, _, ticket := engine.dialogue().Start(prepared)
	for i := range 4 {
		payload := soundCloudPayload()
		if i == 2 {
			payload = map[string]any{}
		}
		name := "quasar_soundcloud_started"
		if i == 3 {
			name = "quasar_soundcloud_finished"
		}
		b := soundCloudEventInput(fmt.Sprintf("callback-%d", i), name, i == 1, payload)
		if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f := wire.Decode(data, wire.ServerToClient)
			if r := f.Server.GetEvent().GetAliceResponse(); r != nil && (len(r.GetResponse().GetCards()) != 0 || r.GetVoiceResponse().GetShouldListen()) {
				t.Fatal("callback not silent")
			}
		}
		if i == 3 {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f := wire.Decode(data, wire.ServerToClient)
			if f.Kind() != "Push" || len(f.Server.GetEvent().GetPush().GetDirectives()) != 1 {
				t.Fatal("next track not sent by Push")
			}
		}
	}
	n.wg.Wait()
	if upstreamCount.Load() != 0 {
		t.Fatal("callback escaped upstream")
	}
	cancelFrame := aa(&client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "cancel"}, Event: &client.TClientMessage_TEvent_CancelRequest{CancelRequest: &cancellation.TCancelRequest{RequestMessageId: "callback-0"}}}}})
	if err := c.Write(ctx, websocket.MessageBinary, cancelFrame); err != nil {
		t.Fatal(err)
	}
	if !engine.dialogue().Current(prepared, ticket) {
		t.Fatal("lifecycle events changed dialogue generation")
	}
	// A real user music request must still be journaled and delivered normally.
	if err := c.Write(ctx, websocket.MessageBinary, soundCloudTextInput("user-music", "включи SoundCloud")); err != nil {
		t.Fatal(err)
	}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if wire.Decode(data, wire.ServerToClient).Server.GetEvent().GetAliceResponse() != nil {
			break
		}
	}
	c.CloseNow()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("proxy did not close")
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	var requestSeen, responseSeen bool
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var e journal.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.Request != journalID("user-music") {
			t.Fatalf("lifecycle callback leaked into conversation journal: %+v", e)
		}
		if e.Event == "request" {
			requestSeen = true
		}
		if e.Event == "response" && e.Delivered {
			responseSeen = true
		}
	}
	if !requestSeen || !responseSeen || observerCount.Load() != 6 {
		t.Fatalf("request=%v response=%v diagnostics=%d", requestSeen, responseSeen, observerCount.Load())
	}
}

func TestNativeSoundCloudObsoleteASRDoesNotReplaceNewPlayback(t *testing.T) {
	calls := new(atomic.Int32)
	provider := soundCloudIntegrationProvider(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); soundCloudCatalogFixture(w, r) })
	engine := &Engine{SoundCloud: provider}
	n, up, down := soundSession(t, engine)
	old := wire.Decode(soundInput("old-voice"), wire.ClientToServer)
	event := old.Client.GetEvent()
	req := event.GetTextInput()
	req.Request.Event.Text = nil
	event.Event = &client.TClientMessage_TEvent_VoiceInput{VoiceInput: req}
	if err := n.Client(t.Context(), websocket.MessageBinary, aa(old.Client)); err != nil {
		t.Fatal(err)
	}
	if f := nextSoundFrame(t, up); f.Kind() != "VoiceInput" {
		t.Fatal("old voice did not start ASR")
	}
	if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("new-play", "включи New SoundCloud")); err != nil {
		t.Fatal(err)
	}
	if ds := finalSoundDirectives(t, down); len(ds) != 1 {
		t.Fatal("new playback not delivered")
	}
	n.wg.Wait()
	before := calls.Load()
	result := aa(&server.TServerMessage{Message: &server.TServerMessage_Event{Event: &server.TServerMessage_TEvent{Header: &header.TEventHeader{RefMessageId: "old-voice"}, Event: &server.TServerMessage_TEvent_AsrResult{AsrResult: &asr.TAsrResult{EndOfUtt: new(true), Recognition: []*recognition.TAsrRecognition{{Normalized: new("включи Old SoundCloud")}}}}}}})
	if err := n.Server(t.Context(), websocket.MessageBinary, result); err != nil {
		t.Fatal(err)
	}
	n.wg.Wait()
	if calls.Load() != before {
		t.Fatal("obsolete ASR started a new catalog operation")
	}
	for len(down) > 0 {
		f := <-down
		for _, d := range f.Server.GetEvent().GetAliceResponse().GetResponse().GetDirectives() {
			if d.GetName() == "audio_play" {
				t.Fatal("obsolete ASR replaced newer playback")
			}
		}
	}
}

type soundCloudHoldHandler struct {
	SoundCloudProvider
	text             string
	entered, release chan struct{}
}

func (p *soundCloudHoldHandler) Handle(ctx context.Context, text string) (string, error) {
	if text == p.text {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return p.SoundCloudProvider.Handle(ctx, text)
}

type soundCloudDeliveryRecorder struct {
	*Engine
	mu        sync.Mutex
	questions []string
}

func (r *soundCloudDeliveryRecorder) RecordDelivered(ctx context.Context, question, answer string, final, action bool) {
	r.mu.Lock()
	if final {
		r.questions = append(r.questions, question)
	}
	r.mu.Unlock()
	r.Engine.RecordDelivered(ctx, question, answer, final, action)
}

func TestNativeSoundCloudSupersededQueuedDirectiveIsDiscardedAfterFocusRelease(t *testing.T) {
	for _, action := range []string{"включи First SoundCloud", "пауза SoundCloud", "стоп SoundCloud"} {
		t.Run(action, func(t *testing.T) {
			provider := soundCloudIntegrationProvider(t, soundCloudCatalogFixture)
			entered, releaseHandle := make(chan struct{}), make(chan struct{})
			blocked, releaseFocus := make(chan struct{}), make(chan struct{})
			var handleOnce, focusOnce sync.Once
			releaseH := func() { handleOnce.Do(func() { close(releaseHandle) }) }
			releaseF := func() { focusOnce.Do(func() { close(releaseFocus) }) }
			defer releaseH()
			defer releaseF()
			engine := &Engine{SoundCloud: &soundCloudHoldHandler{SoundCloudProvider: provider, text: action, entered: entered, release: releaseHandle}}
			recorder := &soundCloudDeliveryRecorder{Engine: engine}
			n, up, down := soundSession(t, recorder)
			send := n.down
			n.down = func(ctx context.Context, k websocket.MessageType, b []byte) error {
				f := wire.Decode(b, wire.ServerToClient)
				if c := f.Server.GetStreamControl(); c != nil && c.GetClose() != nil && c.GetMessageId() == "A" {
					close(blocked)
					select {
					case <-releaseFocus:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return send(ctx, k, b)
			}
			// Establish real ownership so pause/stop can use the concrete provider.
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("bootstrap", "включи Bootstrap SoundCloud")); err != nil {
				t.Fatal(err)
			}
			ds := finalSoundDirectives(t, down)
			if len(ds) != 1 {
				t.Fatal("bootstrap missing")
			}
			payload := ds[0].GetPayload().AsMap()["callbacks"].(map[string]any)["on_started"].(map[string]any)["payload"].(map[string]any)
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudEventInput("started", "quasar_soundcloud_started", false, payload)); err != nil {
				t.Fatal(err)
			}
			silentSoundCloudResponse(t, down)
			n.wg.Wait()
			n.ProgressAfter = time.Millisecond
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("A", action)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("A did not enter provider")
			}
			for {
				f := nextSoundFrame(t, up)
				if f.Kind() == "TtsGenerate" {
					feedSpeech(t, n, f.Client.GetEvent().GetHeader().GetMessageId(), 72, false)
					break
				}
			}
			releaseH()
			select {
			case <-blocked:
			case <-time.After(3 * time.Second):
				t.Fatal("A did not reach queued focus-release wait")
			}
			// A has queued its directive, but takeSound must validate after this wait.
			n.ProgressAfter = 0
			if err := n.Client(t.Context(), websocket.MessageBinary, soundCloudTextInput("B", "включи New SoundCloud")); err != nil {
				t.Fatal(err)
			}
			for {
				f := nextSoundFrame(t, down)
				if r := f.Server.GetEvent().GetAliceResponse(); r != nil && !r.GetResponse().GetIsStreaming() {
					if f.Server.GetEvent().GetHeader().GetRefMessageId() != "B" || len(r.GetResponse().GetDirectives()) != 1 {
						t.Fatal("newer playback missing")
					}
					break
				}
			}
			releaseF()
			for {
				f := nextSoundFrame(t, down)
				if r := f.Server.GetEvent().GetDeferredAliceResponse(); r != nil && r.GetIsLast() {
					if len(r.GetBaseResponse().GetDirectives()) != 0 {
						t.Fatalf("superseded %q directive delivered after newer music", action)
					}
					if r.GetBaseResponse().GetShouldListen() {
						t.Fatal("stale action reopened listening")
					}
					break
				}
			}
			n.wg.Wait()
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			for _, question := range recorder.questions {
				if question == action {
					t.Fatal("superseded action recorded as delivered")
				}
			}
		})
	}
}

type soundCloudCommitObserver struct {
	SoundCloudProvider
	committed chan struct{}
}

func (p *soundCloudCommitObserver) Handle(ctx context.Context, text string) (string, error) {
	answer, err := p.SoundCloudProvider.Handle(ctx, text)
	if text == "включи New SoundCloud" {
		close(p.committed)
	}
	return answer, err
}
func soundCloudDeviceFrame(data []byte) []byte {
	f := wire.Decode(data, wire.ClientToServer)
	req := f.Client.GetEvent().GetTextInput()
	req.Application = &app.TClientInfoProto{DeviceId: new("same-speaker")}
	return aa(f.Client)
}
func TestNativeSoundCloudDispatchOrdersAckAndResponseAcrossConnections(t *testing.T) {
	for _, action := range []string{"включи First SoundCloud", "пауза SoundCloud", "стоп SoundCloud", "callback", "callback_response"} {
		t.Run(action, func(t *testing.T) {
			provider := soundCloudIntegrationProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/streams") {
					soundCloudCatalogFixture(w, r)
					return
				}
				fmt.Fprint(w, `{"collection":[{"id":1,"title":"First","duration":120000},{"id":2,"title":"Second","duration":120000}]}`)
			})
			committed := make(chan struct{})
			engine := &Engine{SoundCloud: &soundCloudCommitObserver{SoundCloudProvider: provider, committed: committed}}
			recorder := &soundCloudDeliveryRecorder{Engine: engine}
			a, _, aDown := soundSession(t, recorder)
			b, _, bDown := soundSession(t, recorder)
			if err := a.Client(t.Context(), websocket.MessageBinary, soundCloudDeviceFrame(soundCloudTextInput("bootstrap", "включи Bootstrap SoundCloud"))); err != nil {
				t.Fatal(err)
			}
			ds := finalSoundDirectives(t, aDown)
			payload := ds[0].GetPayload().AsMap()["callbacks"].(map[string]any)["on_started"].(map[string]any)["payload"].(map[string]any)
			if err := a.Client(t.Context(), websocket.MessageBinary, soundCloudDeviceFrame(soundCloudEventInput("started", "quasar_soundcloud_started", false, payload))); err != nil {
				t.Fatal(err)
			}
			silentSoundCloudResponse(t, aDown)
			a.wg.Wait()
			ackBlocked, releaseAck := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(releaseAck) }) }
			defer release()
			send := a.down
			a.down = func(ctx context.Context, k websocket.MessageType, data []byte) error {
				f := wire.Decode(data, wire.ServerToClient)
				if ((f.Kind() == "InputStartAck" && action != "callback_response") || (f.Kind() == "AliceResponse" && action == "callback_response")) && f.Server.GetEvent().GetHeader().GetRefMessageId() == "A" {
					close(ackBlocked)
					select {
					case <-releaseAck:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return send(ctx, k, data)
			}
			input := soundCloudTextInput("A", action)
			if action == "callback" || action == "callback_response" {
				input = soundCloudEventInput("A", "quasar_soundcloud_finished", false, payload)
			}
			if err := a.Client(t.Context(), websocket.MessageBinary, soundCloudDeviceFrame(input)); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ackBlocked:
			case <-time.After(3 * time.Second):
				t.Fatal("A ack not reached")
			}
			if err := b.Client(t.Context(), websocket.MessageBinary, soundCloudDeviceFrame(soundCloudTextInput("B", "включи New SoundCloud"))); err != nil {
				t.Fatal(err)
			}
			select {
			case <-committed:
			case <-time.After(3 * time.Second):
				t.Fatal("B could not commit while A dispatch held")
			}
			select {
			case f := <-bDown:
				t.Errorf("newer response overtook blocked ACK: %s", f.Kind())
			case <-time.After(30 * time.Millisecond):
			}
			release()
			if ds := finalSoundDirectives(t, aDown); len(ds) != 0 {
				t.Error("A dispatched stale directive after its blocked ACK")
			}
			if ds := finalSoundDirectives(t, bDown); len(ds) != 1 || ds[0].GetName() != "audio_play" {
				t.Fatal("B playback not delivered")
			}
			a.wg.Wait()
			b.wg.Wait()
			if len(aDown) != 0 {
				t.Fatal("stale callback Push delivered after newer playback")
			}
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			for _, question := range recorder.questions {
				if question == action {
					t.Error("stale A recorded as delivered")
				}
			}
		})
	}
}
