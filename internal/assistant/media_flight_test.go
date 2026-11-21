package assistant

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func readFlightFixture(t *testing.T, fragments ...string) mediaResult {
	t.Helper()
	var body strings.Builder
	body.WriteString(`<p>Album</p>`)
	for _, fragment := range fragments {
		frame, err := json.Marshal([]any{1, fragment})
		if err != nil {
			t.Fatal(err)
		}
		body.WriteString(`<script>self.__next_f.push(`)
		body.Write(frame)
		body.WriteString(`)</script>`)
	}
	return readMediaFixture(t, body.String(), "text/html")
}

func TestWebReaderFlightTextRecordWithoutNewlineBeforeModel(t *testing.T) {
	result := readFlightFixture(t, `a:T2,яb:{"title":"Stay or Leave","audioUrl":"https://cdn-o.suno.com/splash-page/clips/f0f00357-0cd7-4fd5-96e5-d9314439f4e5.mp3"}`+"\n")
	if len(result.Audio) != 1 || result.Audio[0].Title != "Stay or Leave" || result.Audio[0].URL != "https://cdn-o.suno.com/splash-page/clips/f0f00357-0cd7-4fd5-96e5-d9314439f4e5.mp3" {
		t.Fatalf("audio=%+v", result.Audio)
	}
}

func TestWebReaderFlightSkipsTextByByteLength(t *testing.T) {
	for _, textRecord := range []string{
		"a:T22,lyrics\nc:{\"audioUrl\":\"/fake.mp3\"}\n",
		"a:T1E,я\nz:{\"audioUrl\":\"/fake.mp3\"}\n",
		"a:T0,",
	} {
		t.Run(textRecord, func(t *testing.T) {
			result := readFlightFixture(t, textRecord+`b:{"title":"Real","audioUrl":"/real.mp3"}`+"\n")
			if len(result.Audio) != 1 || result.Audio[0].Title != "Real" || result.Audio[0].URL != "https://audio.example/real.mp3" {
				t.Fatalf("text body was parsed or framing lost: %+v", result.Audio)
			}
		})
	}
}

func TestWebReaderFlightTextFramingSpansNextFrames(t *testing.T) {
	result := readFlightFixture(t, "a:T", "1", "e,я\nz:{\"audioUrl\":", "\"/fake.mp3\"}\n", `b:{"title":"Real","audioUrl":"/real.mp3"}`+"\n")
	if len(result.Audio) != 1 || result.Audio[0].Title != "Real" || result.Audio[0].URL != "https://audio.example/real.mp3" {
		t.Fatalf("audio=%+v", result.Audio)
	}
}

func TestWebReaderFlightMalformedTextLengthStopsParsing(t *testing.T) {
	for _, header := range []string{
		"a:T,",
		"a:Txyz,",
		"a:T-1,",
		"a:T2\n",
		"a:T2:",
		"a:Tffff,",
		"a:T7fffffffffffffff,",
		"a:Tfffffffffffffffff,",
	} {
		t.Run(header, func(t *testing.T) {
			result := readFlightFixture(t, header+"lyrics\n"+`b:{"audioUrl":"/must-not-expose.mp3"}`+"\n")
			if len(result.Audio) != 0 {
				t.Fatalf("recovered inside malformed text record: %+v", result.Audio)
			}
		})
	}
}

func TestWebReaderFlightPreservesModelOrderAcrossTextAndHintRecords(t *testing.T) {
	stream := `1:{"title":"Before","audioUrl":"/before.mp3"}` + "\n" +
		`:HL["/style.css","style"]` + "\n" +
		`2:I["/ignored.mp3"]` + "\n" +
		`a:T2,яb:{"title":"After","audioUrl":"/after.mp3"}` + "\n" +
		`c:{"title":"Last","audioUrl":"/last.mp3"}` + "\n"
	result := readFlightFixture(t, stream)
	if len(result.Audio) != 3 ||
		result.Audio[0].Title != "Before" || result.Audio[0].URL != "https://audio.example/before.mp3" ||
		result.Audio[1].Title != "After" || result.Audio[1].URL != "https://audio.example/after.mp3" ||
		result.Audio[2].Title != "Last" || result.Audio[2].URL != "https://audio.example/last.mp3" {
		t.Fatalf("audio=%+v", result.Audio)
	}
}
