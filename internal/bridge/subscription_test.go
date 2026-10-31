package bridge

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	application "github.com/freequasar/quasar-go-genproto/proto/alice/library/client/protos"
	subscription "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common/subscription_state"
	device "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	environment "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/common/environment_state"
	request "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/alice_request"
	update "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/capability_update_state"
	header "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/events/common/event_header"
	client "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	endpoint "github.com/freequasar/quasar-go-genproto/proto/alice/protos/endpoint"
	alicepro "github.com/freequasar/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/alice_pro"
	brick "github.com/freequasar/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/brick"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestSubscriptionMonitorReadsRequestStateWithoutMixingEndpoints(t *testing.T) {
	for _, rawState := range []bool{false, true} {
		t.Run(map[bool]string{false: "typed", true: "raw"}[rawState], func(t *testing.T) {
			var m SubscriptionMonitor
			s := m.NewSession(nil, nil, nil)
			defer s.Close()
			blocked, _ := anypb.New(&brick.TBrickCapability{State: &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_Brick}})
			unblocked, _ := anypb.New(&brick.TBrickCapability{State: &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_NotBrick}})
			ds := &device.TDeviceState{DeviceSubscriptionState: &subscription.TSubscriptionState{Subscription: subscription.TSubscriptionState_yandex_subscription}}
			env := &environment.TEnvironmentState{Endpoints: []*endpoint.TEndpoint{
				{Id: "a", Capabilities: []*anypb.Any{blocked}},
				{Id: "b", Capabilities: []*anypb.Any{unblocked}},
			}}
			r := &request.TAliceRequestProto_TRequest{}
			if rawState {
				dsRaw, _ := proto.Marshal(ds)
				envRaw, _ := proto.Marshal(env)
				r.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceStateRaw{DeviceStateRaw: dsRaw}
				r.EnvironmentStateType = &request.TAliceRequestProto_TRequest_EnvironmentStateRaw{EnvironmentStateRaw: envRaw}
			} else {
				r.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: ds}
				r.EnvironmentStateType = &request.TAliceRequestProto_TRequest_EnvironmentState{EnvironmentState: env}
			}
			msg := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Request: r}}}}}
			data, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Client(t.Context(), websocket.MessageBinary, data); err != nil {
				t.Fatal(err)
			}
			got := m.Snapshot()[0]
			if got.DeviceSubscription != "yandex_subscription" || len(got.Endpoints) != 2 || got.Endpoints[0].Status != "blocked_subscription" || got.Endpoints[1].Status != "subscription_unblocked" {
				t.Fatalf("snapshot=%+v", got)
			}
		})
	}
}

// Exercise the real proxy in both modes: a lock report must remain observable
// and reach upstream unchanged, including when Native handles the connection.
func TestProxySubscriptionMonitoringDoesNotBlockTraffic(t *testing.T) {
	for _, hybrid := range []bool{false, true} {
		t.Run(map[bool]string{false: "observe", true: "hybrid"}[hybrid], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer c.CloseNow()
				for {
					kind, data, err := c.Read(r.Context())
					if err != nil {
						return
					}
					if c.Write(r.Context(), kind, data) != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			var monitor SubscriptionMonitor
			p := &Proxy{Upstream: "ws" + strings.TrimPrefix(upstream.URL, "http"), Subscriptions: &monitor}
			if hybrid {
				p.NewSession = func(ctx context.Context, up, down Sender) Session { return NewNative(ctx, up, down, nil) }
			}
			server := httptest.NewServer(p)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			data := subscriptionFrame(t, &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_Brick}, true)
			if err := c.Write(ctx, websocket.MessageBinary, data); err != nil {
				t.Fatal(err)
			}
			_, got, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, got) {
				t.Fatal("lock report changed in transit")
			}
			state := monitor.Snapshot()
			if len(state) != 1 || len(state[0].Endpoints) != 1 || state[0].Endpoints[0].Status != "blocked_subscription" {
				t.Fatalf("snapshot=%+v", state)
			}
		})
	}
}

// These tests catch default/absent fields being interpreted as payment evidence,
// TTL locks being called unpaid, and reports leaking between connections.
func TestSubscriptionMonitorClassifiesExplicitBrickState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *brick.TBrickCapability_TState
		want  string
	}{
		{"unknown", &brick.TBrickCapability_TState{}, "unknown"},
		{"retail", &brick.TBrickCapability_TState{BrickStatus: brick.TBrickCapability_NotBrick}, "not_subscription"},
		{"paid_subscription", &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_NotBrick}, "subscription_unblocked"},
		{"blocked_subscription", &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_Brick, SubscriptionStatus: brick.TBrickCapability_SubscriptionDisabled}, "blocked_subscription"},
		{"blocked_no_payment_evidence", &brick.TBrickCapability_TState{BrickStatus: brick.TBrickCapability_Brick}, "blocked"},
		{"ttl_is_not_nonpayment", &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_BrickByTTL}, "blocked_by_ttl"},
		{"disabled_alone_is_not_a_lock", &brick.TBrickCapability_TState{SubscriptionMode: true, SubscriptionStatus: brick.TBrickCapability_SubscriptionDisabled}, "subscription_unknown"},
		{"future_enum", &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: 99}, "subscription_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m SubscriptionMonitor
			s := m.NewSession(nil, nil, nil)
			defer s.Close()
			raw := subscriptionFrame(t, tc.state, false)
			s.Client(t.Context(), websocket.MessageBinary, raw)
			got := m.Snapshot()
			if len(got) != 1 || len(got[0].Endpoints) != 1 || got[0].Endpoints[0].Status != tc.want {
				t.Fatalf("snapshot=%+v", got)
			}
		})
	}
}

func TestSubscriptionMonitorPreservesTrafficAndRemovesClosedSessions(t *testing.T) {
	var m SubscriptionMonitor
	var sent []byte
	send := func(_ context.Context, _ websocket.MessageType, b []byte) error {
		sent = append([]byte(nil), b...)
		return nil
	}
	s := m.NewSession(send, send, nil)
	raw := subscriptionFrame(t, &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_Brick}, true)
	if err := s.Client(t.Context(), websocket.MessageBinary, raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, sent) {
		t.Fatal("client traffic changed")
	}
	if err := s.Server(t.Context(), websocket.MessageBinary, raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, sent) {
		t.Fatal("server traffic changed")
	}
	data, err := json.Marshal(m.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("secret-status-url")) || bytes.Contains(data, []byte("private-disabled-reason")) {
		t.Fatalf("private data in health: %s", data)
	}
	s.Close()
	if len(m.Snapshot()) != 0 {
		t.Fatal("closed session retained stale status")
	}
}

func TestSubscriptionMonitorDoesNotMixSessionsOrTreatTextAsEvidence(t *testing.T) {
	var m SubscriptionMonitor
	a := m.NewSession(nil, nil, nil)
	defer a.Close()
	b := m.NewSession(nil, nil, nil)
	defer b.Close()
	a.Client(t.Context(), websocket.MessageBinary, subscriptionFrame(t, &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_Brick}, false))
	b.Client(t.Context(), websocket.MessageText, []byte(`{"SubscriptionMode":true,"BrickStatus":"Brick"}`))
	got := m.Snapshot()
	if len(got) != 2 || got[0].Endpoints[0].Status != "blocked_subscription" || len(got[1].Endpoints) != 0 {
		t.Fatalf("mixed sessions: %+v", got)
	}
	// A later explicit unlock must replace the previous lock.
	a.Client(t.Context(), websocket.MessageBinary, subscriptionFrame(t, &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_NotBrick}, false))
	if m.Snapshot()[0].Endpoints[0].Status != "subscription_unblocked" {
		t.Fatal("unlock did not clear blocked status")
	}
}

func TestSubscriptionMonitorConcurrentSnapshots(t *testing.T) {
	var m SubscriptionMonitor
	raw := subscriptionFrame(t, &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_Brick}, false)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s := m.NewSession(nil, nil, nil)
			defer s.Close()
			for range 20 {
				s.Client(t.Context(), websocket.MessageBinary, raw)
				m.Snapshot()
			}
		})
	}
	wg.Wait()
}

func subscriptionFrame(t *testing.T, state *brick.TBrickCapability_TState, holder bool) []byte {
	t.Helper()
	state.StatusUrl = "https://example.invalid/secret-status-url"
	state.DisabledReason = "private-disabled-reason"
	cap := &brick.TBrickCapability{State: state}
	event := &update.TCapabilityUpdateState{EndpointId: "speaker-endpoint"}
	if holder {
		raw, err := proto.Marshal(cap)
		if err != nil {
			t.Fatal(err)
		}
		event.AnyCapability = &endpoint.TAnyCapabilityHolder{CapabilityType: endpoint.TCapability_BrickCapabilityType, RawCapability: raw}
	} else {
		any, err := anypb.New(cap)
		if err != nil {
			t.Fatal(err)
		}
		event.Capability = any
	}
	m := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{MessageId: "subscription-update"}, Event: &client.TClientMessage_TEvent_CapabilityUpdateState{CapabilityUpdateState: event}}}}
	raw, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if holder {
		raw = append([]byte("AAPI"), raw...)
	}
	return raw
}

// Ensure ordinary dialogue does not clear a previously reported state.
func TestSubscriptionMonitorIgnoresMissingAndMalformedCapabilities(t *testing.T) {
	var m SubscriptionMonitor
	s := m.NewSession(nil, nil, nil)
	defer s.Close()
	s.Client(t.Context(), websocket.MessageBinary, subscriptionFrame(t, &brick.TBrickCapability_TState{SubscriptionMode: true, BrickStatus: brick.TBrickCapability_Brick}, false))
	ordinary := &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{}, Event: &client.TClientMessage_TEvent_TextInput{TextInput: &request.TAliceRequestProto{}}}}}
	raw, _ := proto.Marshal(ordinary)
	s.Client(t.Context(), websocket.MessageBinary, raw)
	s.Client(t.Context(), websocket.MessageBinary, []byte{0xff, 0, 1})
	if m.Snapshot()[0].Endpoints[0].Status != "blocked_subscription" {
		t.Fatal("missing state cleared a lock")
	}
}

// Real firmware puts the legacy subscription in EnvironmentState.Devices and
// Alice Pro in the local endpoint, alongside endpoints for other speakers.
func TestSubscriptionMonitorReadsLocalEnvironmentSubscription(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw, text bool
	}{
		{"typed_voice_any", false, false},
		{"typed_text_any", false, true},
		{"raw_voice_holder", true, false},
		{"raw_text_holder", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m SubscriptionMonitor
			var forwarded []byte
			s := m.NewSession(func(_ context.Context, _ websocket.MessageType, data []byte) error {
				forwarded = bytes.Clone(data)
				return nil
			}, nil, nil)
			defer s.Close()
			env := &environment.TEnvironmentState{
				Devices: []*environment.TEnvironmentDeviceInfo{
					subscriptionDevice("foreign", "", subscription.TSubscriptionState_yandex_subscription),
					subscriptionDevice("local", "", subscription.TSubscriptionState_none),
				},
				Endpoints: []*endpoint.TEndpoint{
					aliceProEndpoint(t, "foreign", alicepro.TAliceProCapability_NoSubscription, tc.raw),
					aliceProEndpoint(t, "local", alicepro.TAliceProCapability_SubscriptionActive, tc.raw),
				},
			}
			frame := subscriptionRequest(t, &device.TDeviceState{DeviceId: new("local")}, "local", env, tc.raw)
			if tc.text {
				var message client.TClientMessage
				if err := proto.Unmarshal(frame, &message); err != nil {
					t.Fatal(err)
				}
				event := message.GetEvent()
				event.Event = &client.TClientMessage_TEvent_TextInput{TextInput: event.GetVoiceInput()}
				frame = marshalSubscriptionMessage(t, &message)
			}
			if err := s.Client(t.Context(), websocket.MessageBinary, frame); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frame, forwarded) {
				t.Fatal("request changed in transit")
			}
			got := m.Snapshot()[0]
			if got.DeviceSubscription != "none" || len(got.Endpoints) != 0 || aliceProStatus(t, got) != "active" {
				t.Fatalf("snapshot=%+v Alice Pro=%q", got, aliceProStatus(t, got))
			}
		})
	}
}

func TestSubscriptionMonitorResetsConflictingRequesterIdentity(t *testing.T) {
	var m SubscriptionMonitor
	s := m.NewSession(nil, nil, nil)
	defer s.Close()
	env := &environment.TEnvironmentState{Endpoints: []*endpoint.TEndpoint{aliceProEndpoint(t, "local", alicepro.TAliceProCapability_SubscriptionActive, true)}}
	ds := &device.TDeviceState{DeviceId: new("local"), DeviceSubscriptionState: &subscription.TSubscriptionState{Subscription: subscription.TSubscriptionState_none}}
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, ds, "local", env, true))
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, &device.TDeviceState{DeviceId: new("other")}, "local", nil, true))
	s.Client(t.Context(), websocket.MessageBinary, aliceProUpdate(t, env.Endpoints[0]))
	got := m.Snapshot()[0]
	if got.DeviceSubscription != "unknown" || aliceProStatus(t, got) != "unknown" {
		t.Fatalf("conflicting requester kept previous subscription: %+v", got)
	}
}

func TestSubscriptionMonitorConflictClearsUnboundLegacySubscription(t *testing.T) {
	var m SubscriptionMonitor
	s := m.NewSession(nil, nil, nil)
	defer s.Close()
	ds := &device.TDeviceState{DeviceSubscriptionState: &subscription.TSubscriptionState{Subscription: subscription.TSubscriptionState_yandex_subscription}}
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, ds, "", nil, true))
	if got := m.Snapshot()[0].DeviceSubscription; got != "yandex_subscription" {
		t.Fatalf("explicit unbound subscription=%q", got)
	}
	// Ordinary missing identity still preserves an explicit legacy observation.
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, nil, "", nil, true))
	if got := m.Snapshot()[0].DeviceSubscription; got != "yandex_subscription" {
		t.Fatalf("missing identity cleared subscription=%q", got)
	}
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, &device.TDeviceState{DeviceId: new("other")}, "local", nil, true))
	if got := m.Snapshot()[0].DeviceSubscription; got != "unknown" {
		t.Fatalf("conflict retained unbound subscription=%q", got)
	}
}

func TestSubscriptionMonitorAttributesEnvironmentDevice(t *testing.T) {
	for _, tc := range []struct {
		name, rootID, applicationID, environmentID, environmentStateID string
		rootSubscription                                               *subscription.TSubscriptionState
		want                                                           string
	}{
		{name: "application_match", rootID: "local", environmentID: "local", want: "none"},
		{name: "state_match", rootID: "local", environmentStateID: "local", want: "none"},
		{name: "application_fallback", applicationID: "local", environmentID: "local", want: "none"},
		{name: "foreign_device", rootID: "local", environmentID: "foreign", want: "unknown"},
		{name: "empty_environment_identity", rootID: "local", want: "unknown"},
		{name: "empty_request_identity", environmentID: "local", want: "unknown"},
		{name: "conflicting_environment_identity", rootID: "local", environmentID: "local", environmentStateID: "foreign", want: "unknown"},
		{name: "conflicting_request_identity", rootID: "local", applicationID: "foreign", environmentID: "local", want: "unknown"},
		{name: "explicit_root_precedence", rootID: "local", environmentID: "local", rootSubscription: &subscription.TSubscriptionState{Subscription: subscription.TSubscriptionState_yandex_subscription}, want: "yandex_subscription"},
		{name: "explicit_root_unknown_precedence", rootID: "local", environmentID: "local", rootSubscription: &subscription.TSubscriptionState{Subscription: 99}, want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m SubscriptionMonitor
			s := m.NewSession(nil, nil, nil)
			defer s.Close()
			ds := &device.TDeviceState{DeviceId: new(tc.rootID), DeviceSubscriptionState: tc.rootSubscription}
			env := &environment.TEnvironmentState{Devices: []*environment.TEnvironmentDeviceInfo{subscriptionDevice(tc.environmentID, tc.environmentStateID, subscription.TSubscriptionState_none)}}
			if err := s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, ds, tc.applicationID, env, true)); err != nil {
				t.Fatal(err)
			}
			if got := m.Snapshot()[0].DeviceSubscription; got != tc.want {
				t.Fatalf("device_subscription=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubscriptionMonitorAliceProUpdatesAreScopedAndExplicit(t *testing.T) {
	var m SubscriptionMonitor
	s := m.NewSession(nil, nil, nil)
	defer s.Close()
	// An update cannot establish the requester's identity by itself.
	active := aliceProEndpoint(t, "local", alicepro.TAliceProCapability_SubscriptionActive, true)
	s.Client(t.Context(), websocket.MessageBinary, aliceProUpdate(t, active))
	if got := aliceProStatus(t, m.Snapshot()[0]); got != "unknown" {
		t.Fatalf("unattributed Alice Pro=%q", got)
	}
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, &device.TDeviceState{DeviceId: new("local")}, "", nil, false))
	for _, tc := range []struct {
		name string
		ep   *endpoint.TEndpoint
		want string
	}{
		{"active", active, "active"},
		{"foreign", aliceProEndpoint(t, "foreign", alicepro.TAliceProCapability_NoSubscription, true), "active"},
		{"empty_id", aliceProEndpoint(t, "", alicepro.TAliceProCapability_NoSubscription, true), "active"},
		{"malformed", &endpoint.TEndpoint{Id: "local", AnyCapabilities: []*endpoint.TAnyCapabilityHolder{{CapabilityType: endpoint.TCapability_AliceProCapabilityType, RawCapability: []byte{255}}}}, "active"},
		{"missing_state", &endpoint.TEndpoint{Id: "local", AnyCapabilities: []*endpoint.TAnyCapabilityHolder{{CapabilityType: endpoint.TCapability_AliceProCapabilityType}}}, "active"},
		{"none", aliceProEndpoint(t, "local", alicepro.TAliceProCapability_NoSubscription, false), "none"},
		{"future", aliceProEndpoint(t, "local", 99, true), "unknown"},
		{"active_again", active, "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.Client(t.Context(), websocket.MessageBinary, aliceProUpdate(t, tc.ep))
			if got := aliceProStatus(t, m.Snapshot()[0]); got != tc.want {
				t.Fatalf("Alice Pro=%q, want %q", got, tc.want)
			}
		})
	}
	// Neither missing nor malformed state clears explicit observations.
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, nil, "", nil, true))
	s.Client(t.Context(), websocket.MessageBinary, []byte{255})
	if got := aliceProStatus(t, m.Snapshot()[0]); got != "active" {
		t.Fatalf("missing state cleared Alice Pro: %q", got)
	}
	other := m.NewSession(nil, nil, nil)
	defer other.Close()
	if got := aliceProStatus(t, m.Snapshot()[1]); got != "unknown" {
		t.Fatalf("session inherited Alice Pro=%q", got)
	}
	// A changed requester must not inherit the previous speaker's state.
	s.Client(t.Context(), websocket.MessageBinary, subscriptionRequest(t, &device.TDeviceState{DeviceId: new("other")}, "", nil, true))
	s.Client(t.Context(), websocket.MessageBinary, aliceProUpdate(t, active))
	if got := aliceProStatus(t, m.Snapshot()[0]); got != "unknown" {
		t.Fatalf("changed identity inherited Alice Pro=%q", got)
	}
}

func subscriptionDevice(appID, stateID string, status subscription.TSubscriptionState_ESubscriptionType) *environment.TEnvironmentDeviceInfo {
	return &environment.TEnvironmentDeviceInfo{Application: &application.TClientInfoProto{DeviceId: new(appID)}, OneOfStates: &environment.TEnvironmentDeviceInfo_SpeakerDeviceState{SpeakerDeviceState: &device.TDeviceState{DeviceId: new(stateID), DeviceSubscriptionState: &subscription.TSubscriptionState{Subscription: status}}}}
}

func subscriptionRequest(t *testing.T, ds *device.TDeviceState, appID string, env *environment.TEnvironmentState, raw bool) []byte {
	t.Helper()
	r := &request.TAliceRequestProto_TRequest{}
	if raw {
		r.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceStateRaw{DeviceStateRaw: marshalSubscriptionMessage(t, ds)}
		r.EnvironmentStateType = &request.TAliceRequestProto_TRequest_EnvironmentStateRaw{EnvironmentStateRaw: marshalSubscriptionMessage(t, env)}
	} else {
		r.DeviceStateType = &request.TAliceRequestProto_TRequest_DeviceState{DeviceState: ds}
		r.EnvironmentStateType = &request.TAliceRequestProto_TRequest_EnvironmentState{EnvironmentState: env}
	}
	return marshalSubscriptionMessage(t, &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{}, Event: &client.TClientMessage_TEvent_VoiceInput{VoiceInput: &request.TAliceRequestProto{Application: &application.TClientInfoProto{DeviceId: new(appID)}, Request: r}}}}})
}

func aliceProEndpoint(t *testing.T, id string, status alicepro.TAliceProCapability_ESubscriptionStatus, holder bool) *endpoint.TEndpoint {
	t.Helper()
	cap := &alicepro.TAliceProCapability{State: &alicepro.TAliceProCapability_TState{SubscriptionStatus: status}}
	ep := &endpoint.TEndpoint{Id: id}
	if holder {
		ep.AnyCapabilities = []*endpoint.TAnyCapabilityHolder{{CapabilityType: endpoint.TCapability_AliceProCapabilityType, RawCapability: marshalSubscriptionMessage(t, cap)}}
	} else {
		value, err := anypb.New(cap)
		if err != nil {
			t.Fatal(err)
		}
		ep.Capabilities = []*anypb.Any{value}
	}
	return ep
}

func aliceProUpdate(t *testing.T, ep *endpoint.TEndpoint) []byte {
	t.Helper()
	update := &update.TCapabilityUpdateState{EndpointId: ep.GetId()}
	if len(ep.Capabilities) > 0 {
		update.Capability = ep.Capabilities[0]
	}
	if len(ep.AnyCapabilities) > 0 {
		update.AnyCapability = ep.AnyCapabilities[0]
	}
	return marshalSubscriptionMessage(t, &client.TClientMessage{Message: &client.TClientMessage_Event{Event: &client.TClientMessage_TEvent{Header: &header.TEventHeader{}, Event: &client.TClientMessage_TEvent_CapabilityUpdateState{CapabilityUpdateState: update}}}})
}

func marshalSubscriptionMessage(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func aliceProStatus(t *testing.T, state SpeakerSubscription) string {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	status, _ := fields["alice_pro_subscription"].(string)
	return status
}
