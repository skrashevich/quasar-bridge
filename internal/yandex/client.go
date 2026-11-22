package yandex

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/coder/websocket"
)

type Client struct {
	endpoint, token, apiKey, appID, appVersion, timezone string
	http                                                 *http.Client
	mu                                                   sync.Mutex
	source                                               string
	accountID                                            uint64
	deviceID, dialogID, sessionID, previousID            string
	turn                                                 uint32
}

type envelope struct {
	Directive struct {
		Header struct {
			Name string `json:"name"`
			ID   string `json:"messageId"`
			Ref  string `json:"refMessageId"`
		} `json:"header"`
		Payload jsontext.Value `json:"payload"`
	} `json:"directive"`
}

func (c *Client) Query(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) {
		return "", errors.New("Yandex query must contain valid UTF-8 text")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil {
		return "", queryError(ctx, "Yandex text query canceled")
	}
	if c.source != "" {
		current, err := NewSpeaker(c.source, c.timezone)
		if err != nil {
			return "", err
		}
		if current.accountID != c.accountID {
			return "", errors.New("Yandex account changed; start a new dialog")
		}
		c.token, c.apiKey = current.token, current.apiKey
	}
	hc := http.DefaultClient
	if c.http != nil {
		hc = c.http
	}
	copyHTTP := *hc
	// Native credentials must never follow an HTTP redirect.
	copyHTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ws, _, err := websocket.Dial(ctx, c.endpoint, &websocket.DialOptions{HTTPClient: &copyHTTP, HTTPHeader: http.Header{"Authorization": {"OAuth " + c.token}}})
	if err != nil {
		return "", queryError(ctx, "Yandex text connection failed")
	}
	defer ws.CloseNow()
	ws.SetReadLimit(2 << 20)
	zone := c.timezone
	if zone == "" {
		zone = "Local"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return "", errors.New("invalid Yandex query timezone")
	}
	now := time.Now().In(location)
	if c.deviceID == "" {
		c.deviceID = strings.ReplaceAll(uuid.NewV4().String(), "-", "")
	}
	device := c.deviceID
	application := map[string]any{
		"app_id": c.appID, "app_version": c.appVersion, "platform": "Linux",
		"uuid": device, "device_id": device, "lang": "ru-RU", "timezone": zone,
		"client_time": now.Format(time.RFC3339), "timestamp": strconv.FormatInt(now.Unix(), 10),
	}
	sequence := 0
	send := func(namespace, name, id, ref string, payload any) error {
		sequence++
		header := map[string]any{"namespace": namespace, "name": name, "messageId": id, "seqNumber": sequence}
		if ref != "" {
			header["refMessageId"] = ref
		}
		body, err := json.Marshal(map[string]any{"event": map[string]any{"header": header, "payload": payload}})
		if err != nil {
			return errors.New("cannot encode Yandex text request")
		}
		if ws.Write(ctx, websocket.MessageText, body) != nil {
			return queryError(ctx, "cannot send Yandex text request")
		}
		return nil
	}
	if err = send("System", "SynchronizeState", uuid.NewV4().String(), "", map[string]any{
		"oauth_token": c.token, "auth_token": c.apiKey, "api_key": c.apiKey, "uuid": device,
		"application": application, "vins": map[string]any{"application": application},
		"supported_features": []string{}, "speechkitVersion": "1.0", "request": map[string]any{"experiments": []any{}},
	}); err != nil {
		return "", err
	}
	queryID := uuid.NewV4().String()
	sent := false
	answer := ""
	for {
		kind, data, err := ws.Read(ctx)
		if err != nil {
			return "", queryError(ctx, "Yandex text connection closed before a final answer")
		}
		if kind != websocket.MessageText {
			continue
		} // Discard audio; never play it.
		var message envelope
		if json.Unmarshal(data, &message) != nil {
			return "", errors.New("invalid Yandex text response")
		}
		header := message.Directive.Header
		switch header.Name {
		case "Ping":
			if err = send("System", "Pong", uuid.NewV4().String(), header.ID, map[string]any{}); err != nil {
				return "", err
			}
		case "SynchronizeStateResponse":
			if sent {
				continue
			}
			// A fresh dialog starts empty; follow-ups echo the server's context.
			queryHeader := map[string]any{"request_id": queryID, "dialog_id": c.dialogID, "sequence_number": c.turn + 1}
			if c.sessionID != "" {
				queryHeader["session_id"] = c.sessionID
			}
			if c.previousID != "" {
				queryHeader["prev_req_id"] = c.previousID
			}
			err = send("Vins", "TextInput", queryID, "", map[string]any{
				"application": application, "header": queryHeader,
				"request": map[string]any{"event": map[string]string{"type": "text_input", "text": text}, "voice_session": false,
					"additional_options": map[string]string{"oauth_token": c.token}, "experiments": []any{}, "environment_state": map[string]any{"endpoints": []any{}}},
				"topic": "general", "format": "audio/ogg;codecs=opus", "mime": "audio/ogg;codecs=opus",
			})
			if err != nil {
				return "", err
			}
			sent = true
		case "InvalidAuth":
			return "", errors.New("Yandex speaker authorization was rejected")
		case "EventException", "Error", "GoAway":
			return "", errors.New("Yandex text request was rejected")
		case "VinsResponse":
			if !sent || header.Ref != queryID {
				continue
			}
			part, streaming, err := responseText(message.Directive.Payload)
			if err != nil {
				return "", err
			}
			if part != "" {
				if strings.HasPrefix(part, answer) {
					answer = part
				} else if !strings.HasSuffix(answer, part) {
					answer = strings.TrimSpace(answer + " " + part)
				}
			}
			if streaming {
				continue
			}
			if answer == "" {
				return "", errors.New("Yandex returned no textual answer")
			}
			var state struct {
				Header struct {
					Dialog  string `json:"dialog_id"`
					Session string `json:"session_id"`
				} `json:"header"`
			}
			if json.Unmarshal(message.Directive.Payload, &state) != nil || len(state.Header.Dialog) > 65536 || len(state.Header.Session) > 65536 {
				return "", errors.New("invalid Yandex conversation context")
			}
			if state.Header.Dialog != "" {
				c.dialogID = state.Header.Dialog
			}
			if state.Header.Session != "" {
				c.sessionID = state.Header.Session
			}
			c.previousID = queryID
			c.turn++
			return answer, nil
		}
	}
}

func queryError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("Yandex text query: %w", err)
	}
	return errors.New(message)
}

func responseText(raw []byte) (string, bool, error) {
	type card struct {
		Text string `json:"text"`
	}
	type speech struct {
		Text string `json:"text"`
	}
	var payload struct {
		Response struct {
			Cards     []card `json:"cards"`
			Card      card   `json:"card"`
			Speech    speech `json:"output_speech"`
			Streaming bool   `json:"is_streaming"`
		} `json:"response"`
		Voice struct {
			Speech speech `json:"output_speech"`
		} `json:"voice_response"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "", false, errors.New("invalid Yandex answer")
	}
	var cards []string
	for _, card := range payload.Response.Cards {
		if text := strings.TrimSpace(card.Text); text != "" {
			cards = append(cards, text)
		}
	}
	text := strings.Join(cards, "\n")
	if text == "" {
		text = payload.Response.Card.Text
	}
	if text == "" {
		text = payload.Voice.Speech.Text
	}
	if text == "" {
		text = payload.Response.Speech.Text
	}
	return strings.TrimSpace(text), payload.Response.Streaming, nil
}
