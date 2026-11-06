package bridge

import (
	"context"
	"net/http"
	"net/url"
	"quasarbridge/internal/journal"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Proxy struct {
	Journal       *journal.Writer
	Subscriptions *SubscriptionMonitor
	Upstream      string
	Observer      func(direction string, kind websocket.MessageType, data []byte)
	NewSession    func(context.Context, Sender, Sender) Session
}

type Sender func(context.Context, websocket.MessageType, []byte) error
type Session interface {
	Client(context.Context, websocket.MessageType, []byte) error
	Server(context.Context, websocket.MessageType, []byte) error
	Close()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(p.Upstream)
	if err != nil || u.Host == "" {
		http.Error(w, "invalid upstream", http.StatusBadGateway)
		return
	}
	if r.URL.RawQuery != "" {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += r.URL.RawQuery
	}
	headers := r.Header.Clone()
	for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol"} {
		headers.Del(key)
	}
	var protocols []string
	for value := range strings.SplitSeq(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if value = strings.TrimSpace(value); value != "" {
			protocols = append(protocols, value)
		}
	}
	dialCtx, dialCancel := context.WithTimeout(r.Context(), 15*time.Second)
	up, _, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{HTTPHeader: headers, Subprotocols: protocols})
	dialCancel()
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer up.CloseNow()
	var selected []string
	if proto := up.Subprotocol(); proto != "" {
		selected = []string{proto}
	}
	down, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: selected, OriginPatterns: []string{u.Hostname()}})
	if err != nil {
		return
	}
	defer down.CloseNow()
	down.SetReadLimit(8 << 20)
	up.SetReadLimit(8 << 20)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	var session Session
	var trace *conversation
	downSend := Sender(down.Write)
	if p.Journal != nil {
		trace = newConversation(p.Journal)
		if p.NewSession != nil {
			trace.initialRoute = ""
		}
		downSend = func(ctx context.Context, k websocket.MessageType, data []byte) error {
			if err := down.Write(ctx, k, data); err != nil {
				return err
			}
			trace.server(data, true)
			return nil
		}
	}
	if p.NewSession != nil {
		session = p.NewSession(ctx, up.Write, downSend)
		if n, ok := session.(*Native); ok && trace != nil {
			n.OnRoute = trace.route
		}
	}
	if p.Subscriptions != nil {
		session = p.Subscriptions.NewSession(up.Write, downSend, session)
	}
	if session != nil {
		defer session.Close()
	}
	copyMessages := func(dst, src *websocket.Conn, direction string) {
		defer cancel()
		for {
			kind, data, err := src.Read(ctx)
			if err != nil {
				return
			}
			if trace != nil {
				if direction == "client" {
					trace.client(data)
				} else {
					trace.server(data, false)
				}
			}
			if p.Observer != nil {
				p.Observer(direction, kind, data)
			}
			if session != nil {
				if direction == "client" {
					err = session.Client(ctx, kind, data)
				} else {
					err = session.Server(ctx, kind, data)
				}
				if err != nil {
					return
				}
				continue
			}
			send := Sender(dst.Write)
			if direction == "server" {
				send = downSend
			}
			if err := send(ctx, kind, data); err != nil {
				return
			}
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { copyMessages(up, down, "client") })
	wg.Go(func() { copyMessages(down, up, "server") })
	wg.Wait()
}
