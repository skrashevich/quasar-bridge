package home

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"quasarbridge/internal/dialogue"

	"github.com/coder/websocket"
)

type AssistConfig struct {
	URL                  string `json:"url"`
	AgentID              string `json:"agent_id"`
	PipelineID           string `json:"pipeline_id"`
	DeviceID             string `json:"device_id"`
	Language             string `json:"language"`
	TimeoutMS            int    `json:"timeout_ms"`
	TTLSeconds           int    `json:"ttl_seconds"`
	ContinueConversation bool   `json:"continue_conversation"`
}

func (c *AssistConfig) Normalize() {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	c.AgentID = strings.TrimSpace(c.AgentID)
	c.PipelineID = strings.TrimSpace(c.PipelineID)
	c.DeviceID = strings.TrimSpace(c.DeviceID)
	c.Language = strings.TrimSpace(c.Language)
	if c.URL == "" {
		return
	}
	if c.Language == "" {
		c.Language = "ru"
	}
	if c.TimeoutMS == 0 {
		c.TimeoutMS = 20000
	}
	if c.TTLSeconds == 0 {
		c.TTLSeconds = 300
	}
}

func (c AssistConfig) Validate(token string) error {
	if c.URL == "" {
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("invalid Home Assistant URL")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("home assistant token is required")
	}
	if c.AgentID != "" && !agentIDPattern.MatchString(c.AgentID) {
		return errors.New("invalid Home Assistant agent id")
	}
	if c.PipelineID != "" && !agentIDPattern.MatchString(c.PipelineID) {
		return errors.New("invalid Home Assistant pipeline id")
	}
	if c.DeviceID != "" && !deviceIDPattern.MatchString(c.DeviceID) {
		return errors.New("invalid Home Assistant device id")
	}
	if !languagePattern.MatchString(c.Language) {
		return errors.New("invalid Home Assistant language")
	}
	if c.TimeoutMS < 1000 || c.TimeoutMS > 60000 || c.TTLSeconds < 30 || c.TTLSeconds > 3600 {
		return errors.New("invalid Home Assistant assist limits")
	}
	return nil
}

var (
	agentIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,80}$`)
	deviceIDPattern = regexp.MustCompile(`^[A-Fa-f0-9]{32}$`)
	languagePattern = regexp.MustCompile(`^[a-z]{2,3}(?:-[A-Za-z0-9]{2,8})?$`)
)

type Assist struct {
	Config AssistConfig
	Token  string
	HTTP   *http.Client
	now    func() time.Time
	mu     sync.Mutex
	turns  map[string]assistTurn

	// dialMu is taken only around dial. It is never acquired while sessMu is held.
	// writeMu covers id allocation and the socket write. Home Assistant closes
	// the connection when a command id is not greater than the last id it saw,
	// so those two steps cannot be split across ping, pipeline, and unsubscribe.
	dialMu  sync.Mutex
	writeMu sync.Mutex
	sessMu  sync.Mutex
	conn    *websocket.Conn
	gen     uint64
	done    chan struct{}
	nextID  int64
	waiters map[int64]chan pipelineMsg
}

type assistTurn struct {
	id string
	at time.Time
}

type assistReply struct {
	Speech   string
	Type     string
	Continue bool
}

func (a *Assist) Process(ctx context.Context, text string) (assistReply, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return assistReply{}, errors.New("empty home assistant request")
	}
	if err := ctx.Err(); err != nil {
		return assistReply{}, err
	}
	if utf8.RuneCountInString(text) > 1000 {
		text = string([]rune(text)[:1000])
	}
	timeout := time.Duration(a.Config.TimeoutMS) * time.Millisecond
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, gen, done, err := a.ensure(callCtx)
	if err != nil {
		return assistReply{}, transportErr(ctx)
	}
	events := make(chan pipelineMsg, 64)
	seconds := a.Config.TimeoutMS / 1000
	if seconds < 1 {
		seconds = 1
	}
	cmd := map[string]any{
		"type":        "assist_pipeline/run",
		"start_stage": "intent",
		"end_stage":   "intent",
		"input":       map[string]any{"text": text},
		"timeout":     seconds,
	}
	if a.Config.PipelineID != "" {
		cmd["pipeline"] = a.Config.PipelineID
	}
	if a.Config.DeviceID != "" {
		cmd["device_id"] = a.Config.DeviceID
	}
	if conversation := a.current(ctx); conversation != "" {
		cmd["conversation_id"] = conversation
	}
	id, err := a.writeCommand(callCtx, conn, gen, cmd, events)
	if err != nil {
		if errors.Is(err, errAssistSocketGone) {
			return assistReply{}, transportErr(ctx)
		}
		// The write may have reached Home Assistant.
		a.retire(gen, conn)
		return assistReply{}, errAssistUnknown
	}
	reply, conversationID, local, err := collectPipeline(callCtx, done, events)
	a.unbind(gen, id)
	if err != nil {
		if ctx.Err() != nil {
			a.unsubscribe(gen, conn, id)
			return assistReply{}, ctx.Err()
		}
		reason := "transport"
		if assist, ok := err.(assistFailure); ok {
			reason = assist.reason
		}
		switch reason {
		case "rejected":
			// The command was refused before it subscribed.
		case "pipeline", "no-intent", "ambiguous":
			a.unsubscribe(gen, conn, id)
		default:
			if errors.Is(err, context.DeadlineExceeded) {
				a.unsubscribe(gen, conn, id)
			} else {
				a.retire(gen, conn)
			}
		}
		slog.Warn("home_assist_failed", "reason", reason)
		if reason == "ambiguous" {
			return assistReply{}, errAssistUnknown
		}
		return assistReply{}, transportErr(ctx)
	}
	// The run has finished. Unsubscribe drops that subscription.
	// Sending it before run-end would cancel the pipeline.
	a.unsubscribe(gen, conn, id)
	attrs := []any{"response_type", reply.Type}
	if local != nil {
		attrs = append(attrs, "processed_locally", *local)
	}
	slog.Info("home_assist", attrs...)
	if reply.Type != "error" && conversationID != "" {
		a.remember(ctx, conversationID)
	}
	return reply, nil
}

// ensure returns the process-lifetime websocket, dialing only when there is none.
func (a *Assist) ensure(ctx context.Context) (*websocket.Conn, uint64, <-chan struct{}, error) {
	if conn, gen, done, ok := a.currentSocket(); ok {
		return conn, gen, done, nil
	}
	a.dialMu.Lock()
	defer a.dialMu.Unlock()
	if conn, gen, done, ok := a.currentSocket(); ok {
		return conn, gen, done, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, nil, err
	}
	conn, _, err := websocket.Dial(ctx, strings.TrimRight(a.Config.URL, "/")+"/api/websocket", &websocket.DialOptions{
		HTTPClient: &http.Client{CheckRedirect: refuseRedirect},
	})
	if err != nil {
		return nil, 0, nil, err
	}
	conn.SetReadLimit(1 << 20)
	if err := assistAuth(ctx, conn, a.Token); err != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		return nil, 0, nil, err
	}
	a.sessMu.Lock()
	a.gen++
	gen := a.gen
	done := make(chan struct{})
	a.conn = conn
	a.done = done
	a.waiters = map[int64]chan pipelineMsg{}
	a.sessMu.Unlock()
	slog.Info("home_assist_socket")
	go a.readLoop(gen, conn)
	go a.keepAlive(gen, conn, done)
	return conn, gen, done, nil
}

func (a *Assist) currentSocket() (*websocket.Conn, uint64, <-chan struct{}, bool) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.conn == nil {
		return nil, 0, nil, false
	}
	return a.conn, a.gen, a.done, true
}

// writeCommand allocates the id and writes it before any other command can
// take the next id. ch is bound first when the caller waits for the result.
func (a *Assist) writeCommand(ctx context.Context, conn *websocket.Conn, gen uint64, payload map[string]any, ch chan pipelineMsg) (int64, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	id := a.allocID()
	if ch != nil && !a.bind(gen, id, ch) {
		return 0, errAssistSocketGone
	}
	payload["id"] = id
	if err := writeJSON(ctx, conn, payload); err != nil {
		if ch != nil {
			a.unbind(gen, id)
		}
		return id, err
	}
	return id, nil
}

func (a *Assist) allocID() int64 {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	a.nextID++
	if a.nextID < 1 {
		a.nextID = 1
	}
	return a.nextID
}

func (a *Assist) bind(gen uint64, id int64, ch chan pipelineMsg) bool {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.gen != gen || a.conn == nil {
		return false
	}
	if a.waiters == nil {
		a.waiters = map[int64]chan pipelineMsg{}
	}
	a.waiters[id] = ch
	return true
}

func (a *Assist) unbind(gen uint64, id int64) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.gen != gen {
		return
	}
	delete(a.waiters, id)
}

func (a *Assist) readLoop(gen uint64, conn *websocket.Conn) {
	defer a.retire(gen, conn)
	for {
		var msg pipelineMsg
		if err := readJSON(context.Background(), conn, &msg); err != nil {
			return
		}
		if !a.deliver(gen, msg) {
			return
		}
	}
}

func (a *Assist) deliver(gen uint64, msg pipelineMsg) bool {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.gen != gen {
		return false
	}
	ch := a.waiters[msg.ID]
	if ch == nil {
		return true
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

func (a *Assist) retire(gen uint64, conn *websocket.Conn) {
	a.sessMu.Lock()
	if a.gen != gen || a.conn != conn {
		a.sessMu.Unlock()
		return
	}
	done := a.done
	a.conn = nil
	a.done = nil
	a.waiters = nil
	a.gen++
	a.sessMu.Unlock()
	if done != nil {
		close(done)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func (a *Assist) unsubscribe(gen uint64, conn *websocket.Conn, runID int64) {
	a.sessMu.Lock()
	alive := a.gen == gen && a.conn == conn
	a.sessMu.Unlock()
	if !alive || runID < 1 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.writeCommand(ctx, conn, gen, map[string]any{
		"type":         "unsubscribe_events",
		"subscription": runID,
	}, nil)
	if err != nil {
		a.retire(gen, conn)
	}
}

func (a *Assist) keepAlive(gen uint64, conn *websocket.Conn, done <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !a.ping(gen, conn, done) {
				a.retire(gen, conn)
				return
			}
		}
	}
}

func (a *Assist) ping(gen uint64, conn *websocket.Conn, done <-chan struct{}) bool {
	ch := make(chan pipelineMsg, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := a.writeCommand(ctx, conn, gen, map[string]any{"type": "ping"}, ch)
	if err != nil {
		return false
	}
	defer a.unbind(gen, id)
	select {
	case <-done:
		return false
	case msg, ok := <-ch:
		return ok && (msg.Type == "pong" || (msg.Type == "result" && msg.Success))
	case <-ctx.Done():
		return false
	}
}

func assistAuth(ctx context.Context, conn *websocket.Conn, token string) error {
	var hello struct {
		Type string `json:"type"`
	}
	if err := readJSON(ctx, conn, &hello); err != nil || hello.Type != "auth_required" {
		return errors.New("auth")
	}
	if err := writeJSON(ctx, conn, map[string]string{"type": "auth", "access_token": token}); err != nil {
		return err
	}
	if err := readJSON(ctx, conn, &hello); err != nil || hello.Type != "auth_ok" {
		return errors.New("auth")
	}
	return nil
}

type assistFailure struct{ reason string }

func (e assistFailure) Error() string { return e.reason }

type pipelineMsg struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Success bool   `json:"success"`
	Event   struct {
		Type string `json:"type"`
		Data struct {
			ProcessedLocally *bool `json:"processed_locally"`
			ConversationID   any   `json:"conversation_id"`
			IntentOutput     struct {
				ConversationID       any  `json:"conversation_id"`
				ContinueConversation bool `json:"continue_conversation"`
				Response             struct {
					ResponseType string `json:"response_type"`
					Speech       struct {
						Plain struct {
							Speech string `json:"speech"`
						} `json:"plain"`
					} `json:"speech"`
				} `json:"response"`
			} `json:"intent_output"`
		} `json:"data"`
	} `json:"event"`
}

type assistRun struct {
	reply          assistReply
	conversationID string
	local          *bool
	sawIntent      bool
	accepted       bool
}

func collectPipeline(ctx context.Context, done <-chan struct{}, events <-chan pipelineMsg) (assistReply, string, *bool, error) {
	var run assistRun
	for {
		select {
		case <-ctx.Done():
			if err := drainPipeline(events, &run); err != nil {
				return assistReply{}, "", nil, err
			}
			if run.sawIntent {
				return run.reply, run.conversationID, run.local, nil
			}
			if run.accepted {
				return assistReply{}, "", nil, assistFailure{"ambiguous"}
			}
			return assistReply{}, "", nil, ctx.Err()
		case <-done:
			if err := drainPipeline(events, &run); err != nil {
				return assistReply{}, "", nil, err
			}
			if run.sawIntent {
				return run.reply, run.conversationID, run.local, nil
			}
			if run.accepted {
				return assistReply{}, "", nil, assistFailure{"ambiguous"}
			}
			return assistReply{}, "", nil, assistFailure{"transport"}
		case msg, ok := <-events:
			if !ok {
				if run.sawIntent {
					return run.reply, run.conversationID, run.local, nil
				}
				if run.accepted {
					return assistReply{}, "", nil, assistFailure{"ambiguous"}
				}
				return assistReply{}, "", nil, assistFailure{"transport"}
			}
			if stop, err := applyPipeline(msg, &run); stop || err != nil {
				return run.reply, run.conversationID, run.local, err
			}
		}
	}
}

func drainPipeline(events <-chan pipelineMsg, run *assistRun) error {
	for {
		select {
		case msg, ok := <-events:
			if !ok {
				return nil
			}
			stop, err := applyPipeline(msg, run)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		default:
			return nil
		}
	}
}

func applyPipeline(msg pipelineMsg, run *assistRun) (bool, error) {
	switch msg.Type {
	case "result":
		// Home Assistant acknowledges the subscription before the pipeline runs.
		if !msg.Success {
			return true, assistFailure{"rejected"}
		}
		run.accepted = true
	case "event":
		switch msg.Event.Type {
		case "run-start":
			if id := jsonString(msg.Event.Data.ConversationID); id != "" {
				run.conversationID = id
			}
		case "intent-end":
			run.sawIntent = true
			if id := jsonString(msg.Event.Data.IntentOutput.ConversationID); id != "" {
				run.conversationID = id
			}
			run.local = msg.Event.Data.ProcessedLocally
			run.reply = assistReply{
				Speech:   spoken(msg.Event.Data.IntentOutput.Response.Speech.Plain.Speech),
				Type:     msg.Event.Data.IntentOutput.Response.ResponseType,
				Continue: msg.Event.Data.IntentOutput.ContinueConversation,
			}
			if run.reply.Type == "" {
				run.reply.Type = "query_answer"
			}
		case "run-end":
			if !run.sawIntent {
				return true, assistFailure{"no-intent"}
			}
			return true, nil
		case "error":
			if run.sawIntent {
				return true, nil
			}
			return true, assistFailure{"pipeline"}
		}
	}
	return false, nil
}

func jsonString(v any) string {
	s, _ := v.(string)
	return s
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

func readJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	_, b, err := conn.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func transportErr(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("Home Assistant transport failed")
}

// errAssistUnknown means the pipeline was accepted, or the command write may
// have arrived, and the outcome was not observed. A planner retry could run
// the same action twice.
var errAssistUnknown = errors.New("home assistant result is unknown")

var errAssistSocketGone = errors.New("home assistant socket is gone")

func (a *Assist) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Config.URL+"/api/", nil)
	if err != nil {
		return errors.New("invalid Home Assistant URL")
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{CheckRedirect: refuseRedirect}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Home Assistant transport failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Home Assistant HTTP %d", resp.StatusCode)
	}
	return nil
}

func (a *Assist) Active(ctx context.Context) bool {
	return a.current(ctx) != ""
}

func (a *Assist) Reset(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turns != nil {
		delete(a.turns, dialogue.SpeakerKey(ctx))
	}
}

func (a *Assist) current(ctx context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	turn, ok := a.turns[dialogue.SpeakerKey(ctx)]
	if !ok {
		return ""
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	if now.Sub(turn.at) >= time.Duration(a.Config.TTLSeconds)*time.Second {
		delete(a.turns, dialogue.SpeakerKey(ctx))
		return ""
	}
	return turn.id
}

func (a *Assist) remember(ctx context.Context, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turns == nil {
		a.turns = map[string]assistTurn{}
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	a.turns[dialogue.SpeakerKey(ctx)] = assistTurn{id: id, at: now}
}

func spoken(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > 1000 {
		text = string([]rune(text)[:1000])
	}
	return text
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("redirect")
}

// Gateway asks the configured Home Assistant assistant first. An assistant
// error, a refusal, or a failure before Home Assistant accepts the run falls
// back to the device planner. Once the run is accepted, a dropped socket or a
// timeout is not planned again: the pipeline may already have changed a device.
type Gateway struct {
	Assist  *Assist
	Devices interface {
		Handle(context.Context, string) (string, error)
	}
}

func (g *Gateway) Handle(ctx context.Context, text string) (string, error) {
	text, _, err := g.Turn(ctx, text)
	return text, err
}

// Turn returns the spoken answer. The bool is non-nil only for an Assist
// pipeline answer: false closes the speaker microphone. A nil bool leaves
// the caller's listen-after-reply policy unchanged, including planner fallback.
func (g *Gateway) Turn(ctx context.Context, text string) (string, *bool, error) {
	if g.Assist != nil {
		reply, err := g.Assist.Process(ctx, text)
		if err == nil && reply.Type != "error" {
			listen := g.keepListening(reply)
			return spokenAssist(reply), &listen, nil
		}
		if err != nil && (ctx.Err() != nil || errors.Is(err, errAssistUnknown)) {
			return "", nil, err
		}
		if g.Devices == nil {
			if err != nil {
				return "", nil, err
			}
			if reply.Speech != "" {
				listen := g.keepListening(reply)
				return reply.Speech, &listen, nil
			}
			return "", nil, errors.New("home assistant did not answer")
		}
	}
	if g.Devices == nil {
		return "", nil, errors.New("home assistant is not configured")
	}
	answer, err := g.Devices.Handle(ctx, text)
	return answer, nil, err
}

func (g *Gateway) keepListening(reply assistReply) bool {
	return g.Assist.Config.ContinueConversation && reply.Continue
}

func spokenAssist(reply assistReply) string {
	if reply.Speech == "" {
		return "Готово."
	}
	return reply.Speech
}

func (g *Gateway) Active(ctx context.Context) bool {
	return g != nil && g.Assist != nil && g.Assist.Active(ctx)
}

func (g *Gateway) Reset(ctx context.Context) {
	if g != nil && g.Assist != nil {
		g.Assist.Reset(ctx)
	}
}
