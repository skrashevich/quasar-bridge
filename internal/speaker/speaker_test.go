package speaker

import (
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/common"
	"github.com/skrashevich/quasar-go-genproto/proto/alice/megamind/protos/guest"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	"google.golang.org/protobuf/proto"
)

func near(got, want float64) bool { return math.Abs(got-want) < 1e-3 }

func fixture(t *testing.T) *client.TClientMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/matcheduser-frame.hex")
	if err != nil {
		t.Fatal(err)
	}
	data, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		t.Fatal(err)
	}
	var msg client.TClientMessage
	if err := proto.Unmarshal(data[4:], &msg); err != nil {
		t.Fatal(err)
	}
	return &msg
}

// withClassification returns the captured frame with its classification replaced.
func withClassification(t *testing.T, bc *common.TBiometryClassification) *client.TClientMessage {
	t.Helper()
	msg := fixture(t)
	msg.GetEvent().GetMatchedUser().GetRequest().GetEvent().BiometryClassification = bc
	return msg
}

func score(class, tag string, p float64) *common.TBiometryClassification_TClassificationResult {
	return &common.TBiometryClassification_TClassificationResult{ClassName: &class, Tag: &tag, Confidence: &p}
}

func TestParse_CapturedFrame(t *testing.T) {
	got, ok := Parse(fixture(t))
	if !ok {
		t.Fatal("no classification parsed")
	}
	if got.Age != "adult" || !near(got.AgeConfidence, 0.9999507) {
		t.Fatalf("age=%q %v", got.Age, got.AgeConfidence)
	}
	if got.Gender != "male" || !near(got.GenderConfidence, 0.9867575) {
		t.Fatalf("gender=%q %v", got.Gender, got.GenderConfidence)
	}
	if got.RequestID != "28630110-2f70-4054-823d-c89697a269c5" || got.Guest {
		t.Fatalf("request=%q guest=%v", got.RequestID, got.Guest)
	}
}

func TestParse_ScoresOnlyUsesArgmax(t *testing.T) {
	bc := &common.TBiometryClassification{Scores: []*common.TBiometryClassification_TClassificationResult{
		score("child", "children", 0.50), score("adult", "children", 0.98),
		score("female", "gender", 0.86), score("male", "gender", 0.68),
	}}
	got, ok := Parse(withClassification(t, bc))
	if !ok || got.Age != "adult" || got.Gender != "female" || !near(got.GenderConfidence, 0.86) {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
}

func TestParse_Absent(t *testing.T) {
	for name, msg := range map[string]*client.TClientMessage{
		"no matched user": {},
		"no classifier":   withClassification(t, nil),
		"empty":           withClassification(t, &common.TBiometryClassification{}),
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := Parse(msg); ok {
				t.Fatalf("unexpected %+v", got)
			}
		})
	}
}

func TestParse_GuestStatus(t *testing.T) {
	msg := fixture(t)
	opts := msg.GetEvent().GetMatchedUser().GetRequest().GetAdditionalOptions().GetGuestUserOptions()
	opts.Status = guest.TGuestOptions_Match.Enum()
	if got, ok := Parse(msg); !ok || !got.Guest {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
}

func TestSentence(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	adultMale := Context{Age: "adult", AgeConfidence: 0.9999, Gender: "male", GenderConfidence: 0.9868}
	for _, tc := range []struct {
		name string
		cfg  func(*Config)
		in   Context
		want string
	}{
		{"both high", nil, adultMale, "Сейчас говорит: взрослый, мужчина. Уверенность: высокая."},
		{"gender below threshold", nil, Context{Age: "child", AgeConfidence: 0.95, Gender: "male", GenderConfidence: 0.68}, "Сейчас говорит: ребёнок. Уверенность: высокая."},
		{"medium is the weakest mentioned", nil, Context{Age: "adult", AgeConfidence: 0.99, Gender: "female", GenderConfidence: 0.8}, "Сейчас говорит: взрослый, женщина. Уверенность: средняя."},
		{"nothing passes", nil, Context{Age: "adult", AgeConfidence: 0.5, Gender: "male", GenderConfidence: 0.6}, ""},
		{"gender only", nil, Context{Gender: "male", GenderConfidence: 0.95}, "Сейчас говорит: мужчина. Уверенность: высокая."},
		{"stricter threshold", func(c *Config) { c.MinConfidence = 0.95 }, Context{Age: "adult", AgeConfidence: 0.99, Gender: "male", GenderConfidence: 0.9}, "Сейчас говорит: взрослый. Уверенность: высокая."},
		{"guest", nil, Context{Age: "adult", AgeConfidence: 0.99, Guest: true}, "Сейчас говорит: взрослый (гостевой профиль). Уверенность: высокая."},
		{"guest disabled", func(c *Config) { c.IncludeGuestStatus = false }, Context{Age: "adult", AgeConfidence: 0.99, Guest: true}, "Сейчас говорит: взрослый. Уверенность: высокая."},
		{"unknown placeholder", func(c *Config) { c.PromptTemplate = "Говорит {{age}}{{nope}}." }, adultMale, "Говорит взрослый."},
		{"disabled", func(c *Config) { c.Enabled = false }, adultMale, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			if tc.cfg != nil {
				tc.cfg(&c)
			}
			if got := c.Sentence(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestStore(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewStore()
	s.now = func() time.Time { return now }
	s.Record(Context{RequestID: "a", Age: "adult"})
	if got, ok := s.Lookup("a"); !ok || got.Age != "adult" {
		t.Fatalf("exact lookup failed: %+v %v", got, ok)
	}
	if _, ok := s.Lookup("other"); !ok {
		t.Fatal("fresh latest entry should be the fallback")
	}
	now = now.Add(FallbackAge + time.Second)
	if _, ok := s.Lookup("other"); ok {
		t.Fatal("stale entry used as fallback")
	}
	if _, ok := s.Lookup("a"); !ok {
		t.Fatal("exact match should live for the full TTL")
	}
	now = now.Add(TTL)
	if _, ok := s.Lookup("a"); ok {
		t.Fatal("expired entry returned")
	}
}

func TestStore_EvictsOldest(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewStore()
	s.now = func() time.Time { return now }
	for i := range MaxEntries + 5 {
		now = now.Add(time.Millisecond)
		s.Record(Context{RequestID: string(rune('A' + i)), Age: "adult"})
	}
	if len(s.entries) != MaxEntries {
		t.Fatalf("entries=%d", len(s.entries))
	}
	if _, ok := s.entries["A"]; ok {
		t.Fatal("oldest entry survived")
	}
}
