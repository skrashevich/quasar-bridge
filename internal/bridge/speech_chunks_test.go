package bridge

import (
	"reflect"
	"testing"
)

func TestSpeechChunksAcrossDeltas(t *testing.T) {
	var s speechChunks
	var got []string
	for _, delta := range []string{"Первая", " фраза.", " Вторая ", "фраза! ", "Остаток"} {
		got = append(got, s.add(delta)...)
	}
	if !reflect.DeepEqual(got, []string{"Первая фраза.", "Вторая фраза!"}) {
		t.Fatalf("chunks = %#v", got)
	}
	if got := s.flush(); got != "Остаток" {
		t.Fatalf("remainder = %q", got)
	}
}
