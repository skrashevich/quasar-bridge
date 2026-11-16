package main

import "testing"

func TestThinkingCueConfiguration(t *testing.T) {
	cfg, err := parseConfig([]byte(`{}`), false)
	if err != nil || cfg.LocalProgress.GPTCue {
		t.Fatal("cue must default off")
	}
	if _, err := parseConfig([]byte(`{"local_progress":{"gpt_cue":true}}`), false); err == nil {
		t.Fatal("accepted cue without progress")
	}
	cfg, err = parseConfig([]byte(`{"local_progress":{"enabled":true,"gpt_cue":true}}`), false)
	if err != nil || !cfg.LocalProgress.GPTCue {
		t.Fatalf("enabled cue rejected: %v", err)
	}
}
