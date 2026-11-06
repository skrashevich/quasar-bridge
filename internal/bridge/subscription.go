package bridge

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/coder/websocket"
	subscription "github.com/freequasar/quasar-go-genproto/proto/alice/megamind/protos/common/subscription_state"
	device "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/common/device_state"
	environment "github.com/freequasar/quasar-go-genproto/proto/alice/protos/api/alicekit/common/environment_state"
	endpoint "github.com/freequasar/quasar-go-genproto/proto/alice/protos/endpoint"
	alicepro "github.com/freequasar/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/alice_pro"
	brick "github.com/freequasar/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/brick"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"quasarbridge/internal/wire"
)

// SubscriptionMonitor reports the last explicit state on each active connection.
// It never queries billing services or changes the speaker's lock state.
// The zero value is ready to use.
type SubscriptionMonitor struct {
	mu       sync.Mutex
	next     uint64
	sessions map[uint64]*subscriptionSession
}

type SpeakerSubscription struct {
	Connection           uint64                 `json:"connection"`
	DeviceSubscription   string                 `json:"device_subscription"`
	AliceProSubscription string                 `json:"alice_pro_subscription"`
	Endpoints            []SubscriptionEndpoint `json:"endpoints"`
}

type SubscriptionEndpoint struct {
	EndpointID         string `json:"endpoint_id"`
	Status             string `json:"status"`
	BrickStatus        string `json:"brick_status"`
	SubscriptionMode   bool   `json:"subscription_mode"`
	SubscriptionStatus string `json:"subscription_status"`
}

type subscriptionSession struct {
	monitor              *SubscriptionMonitor
	id                   uint64
	up, down             Sender
	delegate             Session
	deviceID             string
	deviceSubscription   string
	aliceProSubscription string
	endpoints            map[string]SubscriptionEndpoint
}

func (m *SubscriptionMonitor) NewSession(up, down Sender, delegate Session) Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[uint64]*subscriptionSession)
	}
	m.next++
	s := &subscriptionSession{monitor: m, id: m.next, up: up, down: down, delegate: delegate, deviceSubscription: "unknown", aliceProSubscription: "unknown", endpoints: make(map[string]SubscriptionEndpoint)}
	m.sessions[s.id] = s
	return s
}

// Snapshot contains active connections only; reconnects start with unknown state.
func (m *SubscriptionMonitor) Snapshot() []SpeakerSubscription {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SpeakerSubscription, 0, len(m.sessions))
	for _, s := range m.sessions {
		state := SpeakerSubscription{Connection: s.id, DeviceSubscription: s.deviceSubscription, AliceProSubscription: s.aliceProSubscription, Endpoints: make([]SubscriptionEndpoint, 0, len(s.endpoints))}
		for _, ep := range s.endpoints {
			state.Endpoints = append(state.Endpoints, ep)
		}
		slices.SortFunc(state.Endpoints, func(a, b SubscriptionEndpoint) int { return cmp.Compare(a.EndpointID, b.EndpointID) })
		out = append(out, state)
	}
	slices.SortFunc(out, func(a, b SpeakerSubscription) int { return cmp.Compare(a.Connection, b.Connection) })
	return out
}

func (s *subscriptionSession) Client(ctx context.Context, kind websocket.MessageType, data []byte) error {
	f := wire.Decode(data, wire.ClientToServer)
	if e := f.Client.GetEvent(); e != nil {
		if update := e.GetCapabilityUpdateState(); update != nil {
			s.capability(update.GetEndpointId(), update.GetCapability(), update.GetAnyCapability())
			s.aliceProCapability(update.GetEndpointId(), update.GetCapability(), update.GetAnyCapability())
		}
		req := e.GetVoiceInput()
		if req == nil {
			req = e.GetTextInput()
		}
		if req != nil {
			r := req.GetRequest()
			ds := r.GetDeviceState()
			if raw := r.GetDeviceStateRaw(); len(raw) > 0 {
				ds = new(device.TDeviceState)
				if proto.Unmarshal(raw, ds) != nil {
					ds = nil
				}
			}
			deviceID := ds.GetDeviceId()
			applicationID := req.GetApplication().GetDeviceId()
			// Conflicting identities cannot establish ownership of account state.
			identityMatches := deviceID == "" || applicationID == "" || deviceID == applicationID
			if identityMatches {
				deviceID = cmp.Or(deviceID, applicationID)
				if deviceID != "" {
					s.bindDevice(deviceID)
				}
			} else {
				s.bindDevice("")
			}
			env := r.GetEnvironmentState()
			if raw := r.GetEnvironmentStateRaw(); len(raw) > 0 {
				env = new(environment.TEnvironmentState)
				if proto.Unmarshal(raw, env) != nil {
					env = nil
				}
			}
			if identityMatches {
				sub := ds.GetDeviceSubscriptionState()
				if sub == nil && deviceID != "" {
					for _, info := range env.GetDevices() {
						state := info.GetSpeakerDeviceState()
						appID, stateID := info.GetApplication().GetDeviceId(), state.GetDeviceId()
						if (appID != "" && appID != deviceID) || (stateID != "" && stateID != deviceID) {
							continue
						}
						if appID == deviceID || stateID == deviceID {
							sub = state.GetDeviceSubscriptionState()
							if sub != nil {
								break
							}
						}
					}
				}
				if sub != nil {
					s.deviceState(sub)
				}
			}
			for _, ep := range env.GetEndpoints() {
				for _, cap := range ep.GetCapabilities() {
					s.capability(ep.GetId(), cap, nil)
					if identityMatches && deviceID != "" {
						s.aliceProCapability(ep.GetId(), cap, nil)
					}
				}
				for _, cap := range ep.GetAnyCapabilities() {
					s.capability(ep.GetId(), nil, cap)
					if identityMatches && deviceID != "" {
						s.aliceProCapability(ep.GetId(), nil, cap)
					}
				}
			}
		}
	}
	if s.delegate != nil {
		return s.delegate.Client(ctx, kind, data)
	}
	if s.up != nil {
		return s.up(ctx, kind, data)
	}
	return nil
}

func (s *subscriptionSession) bindDevice(id string) {
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if id == "" || id != s.deviceID {
		s.deviceID = id
		s.setDeviceSubscription("unknown")
		s.setAliceProSubscription("unknown")
	}
}

func (s *subscriptionSession) deviceState(sub *subscription.TSubscriptionState) {
	value := "unknown"
	switch sub.GetSubscription() {
	case subscription.TSubscriptionState_none:
		value = "none"
	case subscription.TSubscriptionState_yandex_subscription:
		value = "yandex_subscription"
	}
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	s.setDeviceSubscription(value)
}

// The caller holds monitor.mu; only safe status values enter these logs.
func (s *subscriptionSession) setDeviceSubscription(value string) {
	if s.deviceSubscription != value {
		s.deviceSubscription = value
		slog.Info("speaker_device_subscription_state", "connection", s.id, "status", value)
	}
}

func (s *subscriptionSession) setAliceProSubscription(value string) {
	if s.aliceProSubscription != value {
		s.aliceProSubscription = value
		slog.Info("speaker_alice_pro_subscription_state", "connection", s.id, "status", value)
	}
}

func (s *subscriptionSession) aliceProCapability(id string, cap *anypb.Any, holder *endpoint.TAnyCapabilityHolder) {
	var value alicepro.TAliceProCapability
	switch {
	case cap.MessageIs(&value):
		if cap.UnmarshalTo(&value) != nil {
			return
		}
	case holder.GetCapabilityType() == endpoint.TCapability_AliceProCapabilityType:
		if proto.Unmarshal(holder.GetRawCapability(), &value) != nil {
			return
		}
	default:
		return
	}
	if value.GetState() == nil {
		return
	}
	status := "unknown"
	switch value.GetState().GetSubscriptionStatus() {
	case alicepro.TAliceProCapability_SubscriptionActive:
		status = "active"
	case alicepro.TAliceProCapability_NoSubscription:
		status = "none"
	}
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if id != "" && id == s.deviceID {
		s.setAliceProSubscription(status)
	}
}

func (s *subscriptionSession) capability(id string, cap *anypb.Any, holder *endpoint.TAnyCapabilityHolder) {
	var value brick.TBrickCapability
	switch {
	case cap.MessageIs(&value):
		if cap.UnmarshalTo(&value) != nil {
			return
		}
	case holder.GetCapabilityType() == endpoint.TCapability_BrickCapabilityType:
		if proto.Unmarshal(holder.GetRawCapability(), &value) != nil {
			return
		}
	default:
		return
	}
	state := value.GetState()
	if state == nil {
		return
	}
	status := "unknown"
	if state.GetSubscriptionMode() {
		status = "subscription_unknown"
	}
	switch state.GetBrickStatus() {
	case brick.TBrickCapability_Brick:
		status = "blocked"
		if state.GetSubscriptionMode() {
			status = "blocked_subscription"
		}
	case brick.TBrickCapability_NotBrick:
		status = "not_subscription"
		if state.GetSubscriptionMode() {
			status = "subscription_unblocked"
		}
	case brick.TBrickCapability_BrickByTTL:
		status = "blocked_by_ttl"
	}
	// DisabledReason and StatusUrl may contain private text or account links.
	// They are deliberately excluded from both health responses and logs.
	report := SubscriptionEndpoint{EndpointID: id, Status: status, BrickStatus: state.GetBrickStatus().String(), SubscriptionMode: state.GetSubscriptionMode(), SubscriptionStatus: state.GetSubscriptionStatus().String()}
	s.monitor.mu.Lock()
	previous, exists := s.endpoints[id]
	s.endpoints[id] = report
	s.monitor.mu.Unlock()
	if !exists || previous != report {
		if status == "blocked_subscription" || status == "blocked_by_ttl" || status == "blocked" {
			slog.Warn("speaker_subscription_state", "connection", s.id, "status", status, "brick_status", report.BrickStatus, "subscription_mode", report.SubscriptionMode, "subscription_status", report.SubscriptionStatus)
		} else {
			slog.Info("speaker_subscription_state", "connection", s.id, "status", status)
		}
	}
}

func (s *subscriptionSession) Server(ctx context.Context, kind websocket.MessageType, data []byte) error {
	if s.delegate != nil {
		return s.delegate.Server(ctx, kind, data)
	}
	if s.down != nil {
		return s.down(ctx, kind, data)
	}
	return nil
}

func (s *subscriptionSession) Close() {
	s.monitor.mu.Lock()
	delete(s.monitor.sessions, s.id)
	s.monitor.mu.Unlock()
	if s.delegate != nil {
		s.delegate.Close()
	}
}
