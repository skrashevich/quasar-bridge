package bridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"
	directives "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/directives"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"quasarbridge/internal/dialogue"
	"quasarbridge/internal/musicmatch"
	"quasarbridge/internal/sound"
	"quasarbridge/internal/wire"
)

const comparisonDeadline = 3 * time.Second
const comparisonMaxBytes = 2 << 20
const comparisonMaxFrames = 128

type comparisonResolver interface {
	PrepareMusicComparison(context.Context, string) (musicmatch.Prepared, error)
	ComparisonContext(context.Context) context.Context
	ComparisonCurrent(context.Context) bool
	PreferSoundCloud() bool
	AcceptYandex(context.Context) error
	DispatchSoundCloud(context.Context, func() error) error
	ArmMusicComparison(context.Context) bool
	ComparisonProtected(context.Context) bool
	MarkYandexAccepted(context.Context)
	YandexAcceptedAfter(context.Context) bool
}
type heldMusicFrame struct {
	kind websocket.MessageType
	data []byte
}
type nativeComparison struct {
	notify                 chan struct{}
	frames                 []heldMusicFrame
	bytes                  int
	inflightBytes          int
	ym                     *musicmatch.Candidate
	final, unsafe, decided bool
	winner                 string
	reason                 string
	cancel                 context.CancelFunc
	streams                map[uint32]bool
	aliases                map[string]bool
	serviceAliases         map[string]bool
	serviceStreams         map[uint32]bool
	finished               chan struct{}
}
type comparisonResult struct {
	prepared musicmatch.Prepared
	err      error
}

func (n *Native) beginMusicComparison(p *nativeRequest) error {
	resolver, ok := n.resolver.(comparisonResolver)
	if !ok || !resolver.ComparisonCurrent(p.ctx) || !resolver.ArmMusicComparison(p.ctx) {
		return context.Canceled
	}
	timeout := n.ComparisonTimeout
	if timeout <= 0 {
		timeout = comparisonDeadline
	}
	searchCtx, cancel := context.WithTimeout(p.ctx, timeout)
	stop := context.AfterFunc(resolver.ComparisonContext(p.ctx), cancel)
	c := &nativeComparison{notify: make(chan struct{}, 1), cancel: cancel, streams: map[uint32]bool{}, aliases: map[string]bool{}, serviceAliases: map[string]bool{}, serviceStreams: map[uint32]bool{}, finished: make(chan struct{})}
	n.mu.Lock()
	pending := 0
	for _, q := range n.requests {
		if q.comparison != nil && !q.comparison.decided {
			pending++
		}
	}
	if pending >= 8 {
		c.unsafe = true
	}
	p.comparison = c
	p.cloud = true
	p.route = "music_compare"
	n.mu.Unlock()
	ready := make(chan comparisonResult)
	n.wg.Go(func() {
		prepared, err := resolver.PrepareMusicComparison(searchCtx, p.text)
		if searchCtx.Err() != nil {
			if prepared != nil {
				prepared.Close()
			}
			return
		}
		select {
		case ready <- comparisonResult{prepared, err}:
		case <-searchCtx.Done():
			if prepared != nil {
				prepared.Close()
			}
		}
	})
	n.wg.Go(func() { defer stop(); defer cancel(); n.runMusicComparison(p, c, resolver, searchCtx, ready) })
	// Registration happens before delegation: even an immediate cloud response is held.
	n.mu.Lock()
	gated := p.gated
	control := bytes.Clone(p.continuation)
	input := bytes.Clone(p.input)
	n.mu.Unlock()
	var err error
	if gated {
		if control == nil {
			control, err = n.makeContinue(p)
		}
		if err == nil {
			err = n.up(p.ctx, websocket.MessageBinary, control)
		}
	} else {
		err = n.up(p.ctx, websocket.MessageBinary, input)
		if err == nil && control != nil {
			err = n.up(p.ctx, websocket.MessageBinary, control)
		}
	}
	if err != nil {
		cancel()
	}
	return err
}

func (n *Native) runMusicComparison(p *nativeRequest, c *nativeComparison, r comparisonResolver, ctx context.Context, ready <-chan comparisonResult) {
	var sc musicmatch.Prepared
	scDone := false
	defer func() {
		if sc != nil {
			sc.Close()
		}
	}()
	for {
		n.mu.Lock()
		ym, final, unsafe := c.ym, c.final, c.unsafe
		n.mu.Unlock()
		if !r.ComparisonCurrent(p.ctx) || p.ctx.Err() != nil {
			n.cancelMusicComparison(p, c)
			return
		}
		if unsafe || scDone && final || scDone && sc == nil {
			choose := !unsafe && sc != nil && (ym == nil && final || ym != nil && musicmatch.PreferSoundCloud(p.text, *ym, sc.Candidate(), r.PreferSoundCloud()))
			n.finishMusicComparison(p, c, r, sc, choose)
			return
		}
		select {
		case result := <-ready:
			sc, scDone = result.prepared, true
			if result.err != nil && sc != nil {
				sc.Close()
				sc = nil
			}
		case <-c.notify:
		case <-ctx.Done():
			if !r.ComparisonCurrent(p.ctx) || p.ctx.Err() != nil {
				n.cancelMusicComparison(p, c)
				return
			}
			// A ready playable SC candidate is the available result when Yandex
			// exceeds the deadline. Unknown layouts still keep the normal cloud path.
			n.mu.Lock()
			ym, final, unsafe = c.ym, c.final, c.unsafe
			n.mu.Unlock()
			choose := !unsafe && sc != nil && (ym == nil || musicmatch.PreferSoundCloud(p.text, *ym, sc.Candidate(), r.PreferSoundCloud()))
			n.finishMusicComparison(p, c, r, sc, choose)
			return
		}
	}
}
func (n *Native) cancelMusicComparison(p *nativeRequest, c *nativeComparison) {
	n.mu.Lock()
	comparisonDecide(c, "canceled")
	c.frames = nil
	c.bytes = 0
	c.inflightBytes = 0
	p.canceled = true
	p.cancel()
	for id := range c.streams {
		n.blocked[id] = p
	}
	n.mu.Unlock()
	ctx, cancel := context.WithTimeout(n.ctx, time.Second)
	defer cancel()
	_ = n.sendCancel(ctx, p.id)
}
func (n *Native) finishMusicComparison(p *nativeRequest, c *nativeComparison, r comparisonResolver, sc musicmatch.Prepared, chooseSC bool) {
	err := r.DispatchSoundCloud(r.ComparisonContext(p.ctx), func() error {
		if !r.ComparisonCurrent(p.ctx) {
			return context.Canceled
		}
		if chooseSC {
			if err := sc.Commit(r.ComparisonContext(p.ctx)); err != nil {
				chooseSC = false
			} else {
				if !r.ComparisonCurrent(p.ctx) {
					n.discardSound(p)
					return context.Canceled
				}
				// Commit is local. The losing scenario is canceled only after SC is playable.
				n.mu.Lock()
				if c.unsafe {
					n.mu.Unlock()
					n.discardSound(p)
					chooseSC = false
				} else {
					comparisonDecide(c, "soundcloud")
					c.frames = nil
					c.bytes = 0
					p.local = true
					p.route = "soundcloud"
					if n.comparisonQuarantine == nil {
						n.comparisonQuarantine = map[string]*nativeRequest{}
					}
					n.comparisonQuarantine[dialogue.SpeakerKey(p.ctx)] = p
					for id := range c.streams {
						n.blocked[id] = p
					}
					n.mu.Unlock()
					if err := n.sendCancel(r.ComparisonContext(p.ctx), p.id); err != nil {
						n.discardSound(p)
						return err
					}
					if err := n.ack(p); err != nil {
						return err
					}
					if !r.ComparisonCurrent(p.ctx) {
						n.discardSound(p)
						return context.Canceled
					}
					return n.replyResponse(p, "", true)
				}
			}
		}
		// Stay pending while flushing. New upstream frames join the tail rather than
		// overtaking already held bytes. Native's total buffer cap remains enforced.
		for {
			n.mu.Lock()
			batch := c.frames
			c.inflightBytes += c.bytes
			c.frames = nil
			c.bytes = 0
			if len(batch) == 0 {
				comparisonDecide(c, "yandex")
				p.route = "yandex"
				n.mu.Unlock()
				return nil
			}
			n.mu.Unlock()
			for _, frame := range batch {
				if !r.ComparisonCurrent(p.ctx) {
					return context.Canceled
				}
				if comparisonAudioPlay(wire.Decode(frame.data, wire.ServerToClient).Server.GetEvent()) {
					if err := r.AcceptYandex(r.ComparisonContext(p.ctx)); err != nil {
						return err
					}
				}
				if err := n.down(r.ComparisonContext(p.ctx), frame.kind, frame.data); err != nil {
					return err
				}
				if comparisonAudioPlay(wire.Decode(frame.data, wire.ServerToClient).Server.GetEvent()) {
					r.MarkYandexAccepted(p.ctx)
				}
			}
			n.mu.Lock()
			c.inflightBytes = 0
			n.mu.Unlock()
		}
	})
	if err != nil {
		n.cancelMusicComparison(p, c)
	}
	winner := "yandex"
	if chooseSC {
		winner = "soundcloud"
	}
	n.mu.Lock()
	ym, unsupported, reason := c.ym, c.unsafe, c.reason
	n.mu.Unlock()
	ymScore, scScore := -10000, -10000
	if ym != nil {
		ymScore = musicmatch.Score(p.text, *ym)
	}
	if sc != nil {
		scScore = musicmatch.Score(p.text, sc.Candidate())
	}
	slog.Info("native_music_comparison", "winner", winner, "completed", err == nil, "unsupported_layout", unsupported, "reason", reason, "yandex_score", ymScore, "soundcloud_score", scScore)
}

// comparisonServer runs before the normal Native state machine and all down
// writes. It never rewrites a cloud frame. Only fully correlated layouts may
// yield a SoundCloud winner; an ambiguous music action forces normal Yandex.
func (n *Native) comparisonServer(ctx context.Context, k websocket.MessageType, data []byte, f wire.Frame) (bool, error) {
	n.mu.Lock()
	var p *nativeRequest
	var stream uint32
	if f.Server != nil {
		if e := f.Server.GetEvent(); e != nil {
			p, _ = n.findLocked(e.GetHeader())
			if p == nil {
				for _, id := range []string{e.GetHeader().GetRefMessageId(), e.GetHeader().GetSubRequestRef().GetMessageId(), e.GetHeader().GetSubRequestRef().GetRequestId()} {
					if q := n.comparisonAliases[id]; q != nil {
						p = q
						break
					}
				}
			}
			if p == nil {
				p = n.comparisonPayloadOwnerLocked(e)
			}
		} else if control := f.Server.GetStreamControl(); control != nil {
			stream = uint32(control.GetStreamId())
			// Voice upload termination belongs to the input transport, even when
			// its MessageId differs from the request event ID. Known output and
			// loser streams take precedence if a stream ID has been reused.
			outputKnown := n.comparisonStreams[stream] != nil || n.streams[stream] != nil || n.blocked[stream] != nil || n.tts[control.GetMessageId()] != nil
			var input *nativeRequest
			requestMatch := false
			for _, q := range n.requests {
				if id := control.GetMessageId(); id != "" && (q.id == id || q.request.GetHeader().GetRequestId() == id) {
					requestMatch = true
				}
				if !q.isText && !q.canceled && q.ctx.Err() == nil && q.header.GetStreamId() != 0 && q.header.GetStreamId() == control.GetStreamId() {
					input = q
				}
			}
			action, reason := "other", ""
			if close := control.GetClose(); close != nil {
				action, reason = "close", close.GetReason().String()
			} else if control.GetSpotterEnd() != nil {
				action = "spotter_end"
			}
			inputTag := ""
			if input != nil {
				inputTag = requestTag(input.id)
			}
			slog.Info("native_music_stream_control", "stream", control.GetStreamId(), "action", action, "close_reason", reason, "request_match", requestMatch, "input_stream_match", input != nil, "input_request", inputTag, "output_known", outputKnown)
			if input != nil && !outputKnown && (control.GetClose() != nil || control.GetSpotterEnd() != nil) {
				n.mu.Unlock()
				return false, nil
			}
			p = n.requests[control.GetMessageId()]
			if p == nil {
				p = n.comparisonStreams[stream]
			}
		}
	} else if len(data) >= 4 {
		stream = binary.BigEndian.Uint32(data)
		p = n.comparisonStreams[stream]
		if p == nil && n.streams[stream] != nil {
			n.mu.Unlock()
			return false, nil
		}
	}
	if p == nil || p.comparison == nil {
		if p == nil {
			if e := f.Server.GetEvent(); e != nil && e.GetPush() != nil && !comparisonHasMusic(e) {
				for _, q := range n.requests {
					if q.comparison != nil {
						if len(q.comparison.serviceAliases) >= 64 {
							n.mu.Unlock()
							n.cancel()
							return true, errors.New("music comparison service limit")
						}
						if id := e.GetHeader().GetMessageId(); id != "" && len(id) <= 256 {
							q.comparison.serviceAliases[id] = true
						}
					}
				}
				n.mu.Unlock()
				return false, nil
			}
			for _, q := range n.requests {
				if q.comparison == nil {
					continue
				}
				c := q.comparison
				if e := f.Server.GetEvent(); e != nil && e.GetTtsSpeak() != nil && c.serviceAliases[e.GetHeader().GetRefMessageId()] {
					if len(c.serviceStreams) >= 64 {
						n.mu.Unlock()
						n.cancel()
						return true, errors.New("music comparison service stream limit")
					}
					c.serviceStreams[uint32(e.GetHeader().GetStreamId())] = true
					n.mu.Unlock()
					return false, nil
				}
				if c.serviceStreams[stream] {
					n.mu.Unlock()
					return false, nil
				}
			}
		}
		if p != nil {
			if e := f.Server.GetEvent(); e != nil && e.GetTtsSpeak() != nil {
				stream = uint32(e.GetHeader().GetStreamId())
			}
			delete(n.comparisonStreams, stream)
			delete(n.blocked, stream)
			// A positively correlated newer normal request is the explicit
			// handover path. Quarantine applies to unknown or older output only.
			if q := n.comparisonQuarantine[dialogue.SpeakerKey(p.ctx)]; q != nil && sound.RequestSequence(p.ctx) < sound.RequestSequence(q.ctx) {
				if comparisonHasMusic(f.Server.GetEvent()) || f.Server.GetEvent().GetTtsSpeak() != nil {
					if f.Server.GetEvent().GetTtsSpeak() != nil {
						stream = uint32(f.Server.GetEvent().GetHeader().GetStreamId())
						if err := n.comparisonStreamLocked(q, stream); err != nil {
							n.mu.Unlock()
							n.cancel()
							return true, err
						}
					}
					n.mu.Unlock()
					return true, nil
				}
			}
			n.mu.Unlock()
			return false, nil
		}
		// An uncorrelated music Push cannot leak while SC remains a possible winner.
		// Replay existing Yandex data first, then let the normal frame through.
		unknownMedia := f.Server == nil && len(data) >= 4 || f.Server != nil && (comparisonHasMusic(f.Server.GetEvent()) || f.Server.GetEvent().GetTtsSpeak() != nil || f.Server.GetStreamControl() != nil)
		if unknownMedia {
			var pending []*nativeRequest
			stalePending := false
			for _, q := range n.requests {
				if q.comparison != nil && !q.comparison.decided {
					q.comparison.unsafe = true
					q.comparison.reason = "uncorrelated_music"
					if r, ok := n.resolver.(comparisonResolver); ok && r.ComparisonCurrent(q.ctx) {
						pending = append(pending, q)
					} else {
						stalePending = true
					}
					comparisonNotify(q.comparison)
				} else if q.comparison != nil && q.comparison.winner == "yandex" && q.comparison.reason == "uncorrelated_music" {
					if r, ok := n.resolver.(comparisonResolver); ok && r.ComparisonCurrent(q.ctx) {
						pending = append(pending, q)
					}
				}
			}
			if len(pending) == 0 {
				for key, q := range n.comparisonQuarantine {
					if r, ok := n.resolver.(comparisonResolver); ok {
						if q.ctx.Err() != nil || r.YandexAcceptedAfter(q.ctx) {
							delete(n.comparisonQuarantine, key)
							continue
						}
						if err := n.comparisonAliasLocked(q, f.Server.GetEvent().GetHeader().GetMessageId()); err != nil {
							n.mu.Unlock()
							n.cancel()
							return true, err
						}
						if e := f.Server.GetEvent(); e != nil && e.GetTtsSpeak() != nil {
							stream = uint32(e.GetHeader().GetStreamId())
						}
						if stream != 0 {
							if err := n.comparisonStreamLocked(q, stream); err != nil {
								n.mu.Unlock()
								n.cancel()
								return true, err
							}
						}
						n.mu.Unlock()
						return true, nil
					}
				}
			}
			n.mu.Unlock()
			if len(pending) == 0 && stalePending {
				return true, nil
			}
			for _, q := range pending {
				select {
				case <-q.comparison.done():
				case <-q.ctx.Done():
					return true, nil
				}
				if r, ok := n.resolver.(comparisonResolver); ok && !r.ComparisonCurrent(q.ctx) {
					return true, nil
				}
				n.mu.Lock()
				winner := q.comparison.winner
				n.mu.Unlock()
				if winner != "yandex" {
					return true, nil
				}
			}
			if len(pending) > 0 {
				q := pending[0]
				if r, ok := n.resolver.(comparisonResolver); ok {
					return true, r.DispatchSoundCloud(r.ComparisonContext(q.ctx), func() error {
						if !r.ComparisonCurrent(q.ctx) {
							return nil
						}
						if comparisonAudioPlay(f.Server.GetEvent()) {
							if err := r.AcceptYandex(r.ComparisonContext(q.ctx)); err != nil {
								return err
							}
						}
						err := n.down(r.ComparisonContext(q.ctx), k, data)
						if err == nil && comparisonAudioPlay(f.Server.GetEvent()) {
							r.MarkYandexAccepted(q.ctx)
						}
						return err
					})
				}
			}
			return false, nil
		}
		n.mu.Unlock()
		return false, nil
	}
	c := p.comparison
	if e := f.Server.GetEvent(); e != nil {
		for _, id := range []string{e.GetHeader().GetMessageId(), e.GetAliceResponse().GetHeader().GetResponseId()} {
			if err := n.comparisonAliasLocked(p, id); err != nil {
				n.mu.Unlock()
				n.cancel()
				return true, err
			}
		}
	}
	if e := f.Server.GetEvent(); e != nil && e.GetTtsSpeak() != nil {
		stream = uint32(e.GetHeader().GetStreamId())
	}
	if f.Server != nil && f.Server.GetStreamControl() != nil || f.Server != nil && f.Server.GetEvent().GetTtsSpeak() != nil || stream != 0 {
		if err := n.comparisonStreamLocked(p, stream); err != nil {
			n.mu.Unlock()
			n.cancel()
			return true, err
		}
	}
	if c.decided {
		winner := c.winner
		n.mu.Unlock()
		if winner != "yandex" {
			return true, nil
		}
		if r, ok := n.resolver.(comparisonResolver); ok {
			return true, r.DispatchSoundCloud(r.ComparisonContext(p.ctx), func() error {
				if !r.ComparisonCurrent(p.ctx) {
					return nil
				}
				if comparisonAudioPlay(f.Server.GetEvent()) {
					if err := r.AcceptYandex(r.ComparisonContext(p.ctx)); err != nil {
						return err
					}
				}
				err := n.down(r.ComparisonContext(p.ctx), k, data)
				if err == nil && comparisonAudioPlay(f.Server.GetEvent()) {
					r.MarkYandexAccepted(p.ctx)
				}
				return err
			})
		}
		return true, n.down(ctx, k, data)
	}
	if n.comparisonStreams == nil {
		n.comparisonStreams = map[uint32]*nativeRequest{}
	}
	if stream != 0 {
		c.streams[stream] = true
		n.comparisonStreams[stream] = p
	}
	if f.Server != nil {
		if e := f.Server.GetEvent(); e != nil {
			if e.GetTtsSpeak() != nil {
				stream = uint32(e.GetHeader().GetStreamId())
				c.streams[stream] = true
				n.comparisonStreams[stream] = p
				p.ttsStarted = true
			}
			ds, final, known := comparisonDirectives(e)
			if !known {
				c.unsafe = true
			}
			for _, d := range ds {
				slog.Info("native_music_directive", "type", comparisonSafeName(d.GetType()), "name", comparisonSafeName(d.GetName()))
				if d.GetType() == "client_action" && d.GetName() == "audio_play" {
					candidate, valid := comparisonCandidate(d)
					if !valid {
						c.unsafe = true
						c.reason = "invalid_candidate"
					} else if c.ym == nil {
						c.ym = &candidate
					} else if c.ym.Title != candidate.Title || c.ym.Artist != candidate.Artist || c.ym.Version != candidate.Version {
						c.unsafe = true
						c.reason = "multiple_candidates"
					}
				} else if !comparisonSafeDirective(d) {
					c.unsafe = true
					c.reason = "unknown_directive"
				}
			}
			if final && (c.ym != nil || n.comparisonRefusal(e)) {
				c.final = true
			}
		}
	}
	total := 0
	for _, q := range n.requests {
		if q.comparison != nil {
			total += q.comparison.bytes + q.comparison.inflightBytes
		}
	}
	if len(c.frames) >= comparisonMaxFrames || total+len(data) > comparisonMaxBytes {
		c.unsafe = true
		c.reason = "buffer_limit"
		comparisonNotify(c)
		n.mu.Unlock()
		// Backpressure is bounded by the comparison coordinator; do not append the
		// overflowing frame. Its bytes follow the original buffered batch on fallback.
		select {
		case <-c.done():
		case <-p.ctx.Done():
			return true, nil
		}
		return n.comparisonServer(ctx, k, data, f)
	}
	c.frames = append(c.frames, heldMusicFrame{k, bytes.Clone(data)})
	c.bytes += len(data)
	comparisonNotify(c)
	n.mu.Unlock()
	return true, nil
}
func comparisonNotify(c *nativeComparison) {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}
func comparisonSafeName(value string) string {
	if len(value) > 100 {
		return "unknown"
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_:@.-", r)) {
			return "unknown"
		}
	}
	return value
}

func (n *Native) comparisonRefusal(e *server.TServerMessage_TEvent) bool {
	text := ""
	if r := e.GetAliceResponse(); r != nil {
		text = cloudResponseText(r)
	}
	if r := e.GetDeferredAliceResponse(); r != nil {
		text = r.GetBaseResponse().GetText()
	}
	text = strings.ToLower(text)
	for _, phrase := range []string{"не нашла", "не нашел", "не нашёл", "не нашлось", "не удалось найти", "не могу найти", "ничего не найдено"} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func comparisonDirectives(e *server.TServerMessage_TEvent) ([]*directives.TDirective, bool, bool) {
	if r := e.GetAliceResponse(); r != nil {
		return r.GetResponse().GetDirectives(), !r.GetResponse().GetIsStreaming(), true
	}
	if r := e.GetDeferredAliceResponse(); r != nil {
		return r.GetBaseResponse().GetDirectives(), r.GetIsLast(), true
	}
	if r := e.GetPush(); r != nil {
		return r.GetDirectives(), false, true
	}
	return nil, false, true
}
func comparisonHasMusic(e *server.TServerMessage_TEvent) bool {
	if e == nil {
		return false
	}
	ds, _, _ := comparisonDirectives(e)
	for _, d := range ds {
		if d.GetName() == "audio_play" || strings.Contains(d.GetName(), "music_thin_client") || strings.HasPrefix(d.GetName(), "@@mm_stack") {
			return true
		}
	}
	return false
}
func comparisonAudioPlay(e *server.TServerMessage_TEvent) bool {
	if e == nil {
		return false
	}
	ds, _, _ := comparisonDirectives(e)
	for _, d := range ds {
		if d.GetName() == "audio_play" {
			return true
		}
	}
	return false
}
func comparisonSafeDirective(d *directives.TDirective) bool {
	return d.GetType() == "client_action" && d.GetName() == "tts_play_placeholder" || d.GetType() == "server_action" && d.GetName() == "@@mm_stack_engine_get_next" || d.GetType() == "uniproxy_action" && d.GetName() == "update_memento"
}
func comparisonPayload(d *directives.TDirective) map[string]any {
	if p := d.GetPayload(); p != nil {
		return p.AsMap()
	}
	raw := d.GetPayloadRaw()
	if len(raw) == 0 || len(raw) > 256<<10 {
		return nil
	}
	var p map[string]any
	if json.Unmarshal(raw, &p) == nil {
		return p
	}
	var typed structpb.Struct
	if err := proto.Unmarshal(raw, &typed); err == nil {
		return typed.AsMap()
	}
	return nil
}
func comparisonCandidate(d *directives.TDirective) (musicmatch.Candidate, bool) {
	p := comparisonPayload(d)
	m, _ := p["metadata"].(map[string]any)
	s, _ := p["stream"].(map[string]any)
	title, _ := m["title"].(string)
	artist, _ := m["subtitle"].(string)
	id, _ := s["id"].(string)
	duration, _ := s["duration_ms"].(float64)
	if title == "" || len(title) > 4096 || len(artist) > 4096 || id == "" {
		return musicmatch.Candidate{}, false
	}
	return musicmatch.Candidate{Title: title, Artist: artist, DurationMS: int64(max(0, min(duration, float64(24*time.Hour/time.Millisecond))))}, true
}
func (n *Native) comparisonPayloadOwnerLocked(e *server.TServerMessage_TEvent) *nativeRequest {
	ds, _, _ := comparisonDirectives(e)
	var match *nativeRequest
	for _, d := range ds {
		p := comparisonPayload(d)
		for _, object := range []map[string]any{p, comparisonMap(p["scenario_meta"])} {
			id, _ := object["@request_id"].(string)
			if id == "" {
				continue
			}
			for _, q := range n.requests {
				if q.comparison != nil && (q.id == id || q.request.GetHeader().GetRequestId() == id) {
					if match != nil && match != q {
						return nil
					}
					match = q
				}
			}
		}
	}
	return match
}
func comparisonMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func (n *Native) comparisonAliasLocked(p *nativeRequest, id string) error {
	if id == "" {
		return nil
	}
	if len(id) > 256 {
		return errors.New("music comparison alias limit")
	}
	if n.comparisonAliases == nil {
		n.comparisonAliases = map[string]*nativeRequest{}
	}
	if !p.comparison.aliases[id] && (len(p.comparison.aliases) >= 64 || len(n.comparisonAliases) >= 1024) {
		return errors.New("music comparison alias limit")
	}
	p.comparison.aliases[id] = true
	n.comparisonAliases[id] = p
	return nil
}

func (n *Native) comparisonStreamLocked(p *nativeRequest, stream uint32) error {
	if n.comparisonStreams == nil {
		n.comparisonStreams = map[uint32]*nativeRequest{}
	}
	if !p.comparison.streams[stream] && (len(p.comparison.streams) >= 64 || len(n.comparisonStreams) >= 1024) {
		return errors.New("music comparison stream limit")
	}
	p.comparison.streams[stream] = true
	n.comparisonStreams[stream] = p
	return nil
}

// done is initialized lazily under Native.mu and closed once decision is final.
func (c *nativeComparison) done() <-chan struct{} { return c.finished }

func (n *Native) comparisonDeliveryContext(p *nativeRequest) context.Context {
	if p.comparison != nil {
		if r, ok := n.resolver.(comparisonResolver); ok {
			return r.ComparisonContext(p.ctx)
		}
	}
	return p.ctx
}

func (n *Native) sendCloudMusic(ctx context.Context, k websocket.MessageType, data []byte, p *nativeRequest, e *server.TServerMessage_TEvent) error {
	if r, ok := n.resolver.(comparisonResolver); ok && r.ComparisonProtected(p.ctx) && comparisonHasMusic(e) {
		return r.DispatchSoundCloud(p.ctx, func() error {
			if !r.ComparisonCurrent(p.ctx) {
				return nil
			}
			if comparisonAudioPlay(e) {
				if err := r.AcceptYandex(r.ComparisonContext(p.ctx)); err != nil {
					return err
				}
			}
			err := n.down(ctx, k, data)
			if err == nil && comparisonAudioPlay(e) {
				r.MarkYandexAccepted(p.ctx)
			}
			return err
		})
	}
	return n.down(ctx, k, data)
}
func comparisonDecide(c *nativeComparison, winner string) {
	if !c.decided {
		c.decided = true
		c.winner = winner
		close(c.finished)
	}
}
