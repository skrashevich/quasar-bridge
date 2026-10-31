package bridge

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"
	"uuid"

	"quasarbridge/internal/journal"
	"quasarbridge/internal/wire"
)

type turn struct {
	start time.Time
	route string
}
type conversation struct {
	writer       *journal.Writer
	initialRoute string
	id           string
	mu           sync.Mutex
	turns        map[string]turn
	aliases      map[string]string
}

func newConversation(w *journal.Writer) *conversation {
	return &conversation{writer: w, initialRoute: "yandex", id: uuid.NewV4().String(), turns: map[string]turn{}, aliases: map[string]string{}}
}
func journalID(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum[:16])
}
func (c *conversation) emit(id string, e journal.Event) {
	if !c.writer.Enabled() {
		return
	}
	c.mu.Lock()
	if alias := c.aliases[id]; alias != "" {
		id = alias
	}
	t := c.turns[id]
	c.mu.Unlock()
	e.Session = c.id
	e.Request = journalID(id)
	e.Route = t.route
	if !t.start.IsZero() {
		e.ElapsedMS = time.Since(t.start).Milliseconds()
	}
	c.writer.Record(e)
}
func (c *conversation) route(id, route string) {
	if !c.writer.Enabled() {
		return
	}
	c.mu.Lock()
	t := c.turns[id]
	t.route = route
	c.turns[id] = t
	c.mu.Unlock()
	c.emit(id, journal.Event{Event: "route"})
}
func (c *conversation) client(data []byte) {
	if !c.writer.Enabled() {
		return
	}
	f := wire.Decode(data, wire.ClientToServer)
	e := f.Client.GetEvent()
	if e == nil {
		return
	}
	req := e.GetTextInput()
	if req == nil {
		req = e.GetVoiceInput()
	}
	if req == nil {
		req = e.GetSpeechInput()
	}
	if req != nil {
		id := e.GetHeader().GetMessageId()
		c.mu.Lock()
		now := time.Now()
		for k, t := range c.turns {
			if now.Sub(t.start) > 90*time.Second {
				delete(c.turns, k)
			}
		}
		// Bound metadata even for endless connections. No transcript text is retained.
		if len(c.turns) >= 128 {
			clear(c.turns)
			clear(c.aliases)
		}
		for alias, target := range c.aliases {
			if _, ok := c.turns[target]; !ok {
				delete(c.aliases, alias)
			}
		}
		c.turns[id] = turn{start: now, route: c.initialRoute}
		if rid := req.GetHeader().GetRequestId(); rid != "" {
			c.aliases[rid] = id
		}
		c.mu.Unlock()
		c.emit(id, journal.Event{Event: "request", Kind: f.Kind(), Text: req.GetRequest().GetEvent().GetText()})
	} else if cancel := e.GetCancelRequest(); cancel != nil {
		c.emit(cancel.GetRequestMessageId(), journal.Event{Event: "cancel", Kind: f.Kind()})
	}
}
func (c *conversation) server(data []byte, delivered bool) {
	if !c.writer.Enabled() {
		return
	}
	f := wire.Decode(data, wire.ServerToClient)
	e := f.Server.GetEvent()
	if e == nil {
		return
	}
	id := e.GetHeader().GetRefMessageId()
	if id == "" {
		id = e.GetHeader().GetSubRequestRef().GetMessageId()
	}
	if id == "" {
		id = e.GetHeader().GetSubRequestRef().GetRequestId()
	}
	if asr := e.GetAsrResult(); asr != nil && asr.GetEndOfUtt() && !delivered {
		hypotheses := asr.GetRecognition()
		if len(hypotheses) > 1 {
			for i, hypothesis := range hypotheses[:min(len(hypotheses), asrHypothesisLimit)] {
				c.emit(id, journal.Event{Event: "asr_hypothesis", Kind: fmt.Sprintf("%s/%d", f.Kind(), i), Text: recognitionText(hypothesis)})
			}
		}
		c.emit(id, journal.Event{Event: "transcript", Kind: f.Kind(), Text: selectASRText(hypotheses)})
	}
	if d := e.GetDeferredAliceResponse(); d != nil {
		name := "progress"
		if d.GetIsLast() {
			name = "response"
		}
		c.emit(id, journal.Event{Event: name, Kind: f.Kind(), Text: d.GetBaseResponse().GetText(), Delivered: delivered})
	}
	if r := e.GetAliceResponse(); r != nil {
		if id == "" {
			id = r.GetHeader().GetRequestId()
		}
		var cards []string
		for _, card := range r.GetResponse().GetCards() {
			if text := card.GetText(); text != "" {
				cards = append(cards, text)
			}
		}
		name := "cloud_response"
		if delivered {
			name = "response"
			if r.GetResponse().GetIsStreaming() {
				name = "progress"
			}
		}
		c.emit(id, journal.Event{Event: name, Kind: f.Kind(), Text: strings.Join(cards, "\n"), Speech: r.GetVoiceResponse().GetOutputSpeech().GetText(), Delivered: delivered})
	}
}
