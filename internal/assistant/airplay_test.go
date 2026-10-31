package assistant

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type fakeAirPlay struct {
	status     AirPlayStatus
	calls      []string
	controlErr error
	statusErr  error
}

func (f *fakeAirPlay) Status(context.Context) (AirPlayStatus, error) {
	f.calls = append(f.calls, "status")
	return f.status, f.statusErr
}
func (f *fakeAirPlay) Control(_ context.Context, action string) (string, error) {
	f.calls = append(f.calls, "control:"+action)
	return "Готово.", f.controlErr
}
func (f *fakeAirPlay) ControlMedia(_ context.Context, action string) (string, error) {
	f.calls = append(f.calls, "media:"+action)
	return "Готово.", f.controlErr
}

func TestAirPlayToolAvailabilityAndSchemas(t *testing.T) {
	names := []string{"get_airplay_status", "control_airplay", "control_airplay_playback", "set_airplay_volume"}
	for _, enabled := range []bool{false, true} {
		tools := &Tools{}
		if enabled {
			tools.AirPlay = &fakeAirPlay{}
		}
		definitions := tools.Definitions()
		if enabled && len(definitions) != len(names) || !enabled && len(definitions) != 0 {
			t.Fatalf("enabled=%v: definitions=%v", enabled, definitions)
		}
		for _, def := range definitions {
			if !slices.Contains(names, def.Function.Name) || def.Function.Description == "" {
				t.Fatalf("invalid definition: %+v", def)
			}
			parameters := def.Function.Parameters
			if parameters["type"] != "object" || parameters["additionalProperties"] != false {
				t.Fatalf("loose schema: %+v", parameters)
			}
			if def.Function.Name == "set_airplay_volume" {
				volume := parameters["properties"].(map[string]any)["volume"].(map[string]any)
				if volume["type"] != "integer" || volume["minimum"] != 0 || volume["maximum"] != 100 {
					t.Fatalf("invalid volume bounds: %v", volume)
				}
			}
		}
		if !enabled {
			for _, name := range names {
				if _, err := tools.Call(t.Context(), name, "{}"); err == nil {
					t.Fatalf("unavailable tool %s succeeded", name)
				}
			}
		}
	}
}

func TestAirPlayToolsDelegateValidatedActions(t *testing.T) {
	for _, tc := range []struct{ name, args, call string }{
		{"control_airplay", `{"action":"start"}`, "control:start"},
		{"control_airplay", `{"action":"stop"}`, "control:stop"},
		{"control_airplay_playback", `{"action":"pause"}`, "media:pause"},
		{"control_airplay_playback", `{"action":"resume"}`, "media:resume"},
		{"control_airplay_playback", `{"action":"stop"}`, "media:stop"},
		{"control_airplay_playback", `{"action":"next"}`, "media:next"},
		{"control_airplay_playback", `{"action":"previous"}`, "media:previous"},
		{"control_airplay_playback", `{"action":"restart"}`, "media:restart"},
		{"control_airplay_playback", `{"action":"volume_up"}`, "media:volume_up"},
		{"control_airplay_playback", `{"action":"volume_down"}`, "media:volume_down"},
		{"set_airplay_volume", `{"volume":0}`, "media:volume:0"},
		{"set_airplay_volume", `{"volume":45}`, "media:volume:45"},
		{"set_airplay_volume", `{"volume":100}`, "media:volume:100"},
	} {
		t.Run(tc.call, func(t *testing.T) {
			state := AirPlayStatus{Enabled: true, State: "running", Name: "Колонка", Connected: true, Playing: false, VolumeDB: -12}
			receiver := &fakeAirPlay{status: state}
			result, err := (&Tools{AirPlay: receiver}).Call(t.Context(), tc.name, tc.args)
			if err != nil || !reflect.DeepEqual(receiver.calls, []string{tc.call, "status"}) {
				t.Fatalf("calls=%v result=%v err=%v", receiver.calls, result, err)
			}
			raw, err := json.Marshal(result)
			var decoded struct {
				Message string        `json:"message"`
				Status  AirPlayStatus `json:"status"`
			}
			if err != nil || json.Unmarshal(raw, &decoded) != nil || decoded.Message != "Готово." || decoded.Status != state {
				t.Fatalf("lost real status: %s (%v)", raw, err)
			}
		})
	}
}

func TestAirPlayStatusDistinguishesConnectionAndPlayback(t *testing.T) {
	for _, state := range []AirPlayStatus{
		{Enabled: true, State: "stopped"},
		{Enabled: true, State: "running", Name: "Колонка"},
		{Enabled: true, State: "running", Connected: true, Playing: false, VolumeDB: -15},
		{Enabled: true, State: "running", Connected: true, Playing: true, VolumeDB: -5},
	} {
		receiver := &fakeAirPlay{status: state}
		result, err := (&Tools{AirPlay: receiver}).Call(t.Context(), "get_airplay_status", "{}")
		if err != nil || result != state || !reflect.DeepEqual(receiver.calls, []string{"status"}) {
			t.Fatalf("state=%+v result=%+v calls=%v err=%v", state, result, receiver.calls, err)
		}
	}
}

func TestAirPlayToolsRejectInvalidArgumentsBeforeControl(t *testing.T) {
	for name, arguments := range map[string][]string{
		"get_airplay_status":       {`null`, `[]`, `{"action":"start"}`},
		"control_airplay":          {`null`, `[]`, `{}`, `{"action":null}`, `{"action":"pause"}`, `{"action":"start","volume":10}`, `{"action":"start","action":"stop"}`},
		"control_airplay_playback": {`null`, `{}`, `{"action":"start"}`, `{"action":"volume:10"}`, `{"action":"next","url":"http://example.org"}`},
		"set_airplay_volume":       {`null`, `[]`, `{}`, `{"volume":null}`, `{"volume":-1}`, `{"volume":101}`, `{"volume":4.5}`, `{"volume":"50"}`, `{"volume":20,"action":"start"}`, `{"volume":0,"volume":99}`},
	} {
		for _, args := range append(arguments, "", "{", "{} {}") {
			t.Run(name+args, func(t *testing.T) {
				receiver := &fakeAirPlay{}
				result, err := (&Tools{AirPlay: receiver}).Call(t.Context(), name, args)
				if err == nil || result != nil || len(receiver.calls) != 0 {
					t.Fatalf("invalid args called receiver: calls=%v result=%v err=%v", receiver.calls, result, err)
				}
			})
		}
	}
}

func TestAirPlayToolsPropagateFailuresAndCancellation(t *testing.T) {
	failure := errors.New("sender does not support remote control")
	for _, tc := range []struct{ name, args string }{
		{"control_airplay", `{"action":"start"}`},
		{"control_airplay_playback", `{"action":"next"}`},
		{"set_airplay_volume", `{"volume":40}`},
	} {
		receiver := &fakeAirPlay{controlErr: failure}
		result, err := (&Tools{AirPlay: receiver}).Call(t.Context(), tc.name, tc.args)
		if !errors.Is(err, failure) || result != nil || len(receiver.calls) != 1 {
			t.Fatalf("failure reported success: %v %v %v", result, err, receiver.calls)
		}
	}
	receiver := &fakeAirPlay{statusErr: failure}
	for _, tc := range []struct{ name, args string }{
		{"get_airplay_status", `{}`},
		{"control_airplay", `{"action":"start"}`},
	} {
		if result, err := (&Tools{AirPlay: receiver}).Call(t.Context(), tc.name, tc.args); !errors.Is(err, failure) || result != nil {
			t.Fatalf("status failure reported success: %v %v", result, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receiver = &fakeAirPlay{}
	if _, err := (&Tools{AirPlay: receiver}).Call(ctx, "control_airplay", `{"action":"start"}`); !errors.Is(err, context.Canceled) || len(receiver.calls) != 0 {
		t.Fatalf("cancelled call reached receiver: %v %v", receiver.calls, err)
	}
}

func TestAgentAirPlayToolRoundTrip(t *testing.T) {
	receiver := &fakeAirPlay{status: AirPlayStatus{Enabled: true, State: "running", Connected: true}}
	round := 0
	agent := Agent{Tools: &Tools{AirPlay: receiver}, MaxRounds: 2, MaxCalls: 2, Client: chatFunc(func(_ context.Context, messages []llm.Message, definitions []llm.Tool) (llm.Message, error) {
		round++
		if round == 1 {
			if len(definitions) != 4 || !strings.Contains(messages[0].Content, "AirPlay") {
				t.Fatal("AirPlay capabilities missing from model context")
			}
			return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "pause", Type: "function", Function: llm.FunctionCall{Name: "control_airplay_playback", Arguments: `{"action":"pause"}`}}}}, nil
		}
		last := messages[len(messages)-1]
		if last.Role != "tool" || last.ToolCallID != "pause" || !strings.Contains(last.Content, `"connected":true`) || !strings.Contains(last.Content, `"playing":false`) {
			t.Fatalf("model did not receive actual paused status: %+v", last)
		}
		return llm.Message{Role: "assistant", Content: "Пауза."}, nil
	})}
	answer, err := agent.Complete(t.Context(), []llm.Message{{Role: "user", Content: "поставь AirPlay на паузу"}}, false)
	if err != nil || answer != "Пауза." || round != 2 || !reflect.DeepEqual(receiver.calls, []string{"media:pause", "status"}) {
		t.Fatalf("answer=%q calls=%v rounds=%d err=%v", answer, receiver.calls, round, err)
	}
}
