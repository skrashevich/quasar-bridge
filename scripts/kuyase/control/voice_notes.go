package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"os"
)

const voiceNoteDescription = "Сохранить голосовую заметку в блокнот"

func voiceNoteScenario(ctx context.Context, base, fixtures, name, notesPath string) error {
	notes, err := os.OpenFile(notesPath, os.O_RDONLY|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("open voice notes: %w", err)
	}
	defer notes.Close()
	if _, err := notes.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	source, rule := "rule", "notes"
	want := counters{Voice: 1, TTS: 1}
	if name == "command_classifier" {
		source, rule = "classifier", ""
		want.LLM = 1
	}
	id := requestID(name)
	input := turn{name: name, device: name, answer: "Заметка сохранена.", want: want}
	if err := exchange(ctx, base, fixtures, input, id); err != nil {
		return err
	}
	added, err := io.ReadAll(notes)
	if err != nil {
		return err
	}
	line, ok := bytes.CutSuffix(added, []byte{'\n'})
	if !ok || bytes.IndexByte(line, '\n') >= 0 {
		return fmt.Errorf("expected exactly one new JSONL note, got %q", added)
	}
	var note struct {
		Version   int    `json:"version"`
		Text      string `json:"text"`
		Command   string `json:"command"`
		Source    string `json:"source"`
		Rule      string `json:"rule"`
		RequestID string `json:"request_id"`
		InputType string `json:"input_type"`
	}
	if err := json.Unmarshal(line, &note); err != nil {
		return fmt.Errorf("invalid voice note: %w", err)
	}
	if note.Version != 1 || note.Text != phrases[name] || note.Command != "voice_note" ||
		note.Source != source || note.Rule != rule || note.RequestID != id || note.InputType != "VoiceInput" {
		return fmt.Errorf("unexpected voice note for %s request %s: %s", source, id, line)
	}
	log.Printf("PASS %s: one voice_note entry for %s, stdout in AliceResponse and TtsGenerate", name, id)
	return nil
}
