package speaker

import (
	"errors"
	"regexp"
	"strings"
)

// DefaultTemplate is used when prompt_template is empty.
const DefaultTemplate = "Сейчас говорит: {{age}}{{gender}}{{guest}}. Уверенность: {{confidence}}."

// Config is the optional speaker_context block of config.json.
type Config struct {
	Enabled            bool    `json:"enabled"`
	PromptTemplate     string  `json:"prompt_template"`
	MinConfidence      float64 `json:"min_confidence"`
	IncludeGuestStatus bool    `json:"include_guest_status"`
}

func DefaultConfig() Config {
	return Config{PromptTemplate: DefaultTemplate, MinConfidence: 0.7, IncludeGuestStatus: true}
}

func (c Config) Validate() error {
	if c.MinConfidence < 0.7 || c.MinConfidence > 1 {
		return errors.New("speaker_context.min_confidence must be between 0.7 and 1")
	}
	return nil
}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z_]+)\s*\}\}`)

var (
	ages    = map[string]string{"adult": "взрослый", "child": "ребёнок"}
	genders = map[string]string{"male": "мужчина", "female": "женщина"}
)

// confidenceLabel maps a probability to the spoken level; lower values are not mentioned.
func confidenceLabel(p float64) string {
	switch {
	case p >= 0.9:
		return "высокая"
	case p >= 0.7:
		return "средняя"
	}
	return ""
}

// Sentence renders the single prompt sentence. It returns an empty string when
// nothing passes the threshold, so the prompt stays unchanged.
func (c Config) Sentence(s Context) string {
	if !c.Enabled {
		return ""
	}
	var age, gender string
	level := 1.0
	if label, ok := ages[s.Age]; ok && s.AgeConfidence >= c.MinConfidence && confidenceLabel(s.AgeConfidence) != "" {
		age, level = label, min(level, s.AgeConfidence)
	}
	if label, ok := genders[s.Gender]; ok && s.GenderConfidence >= c.MinConfidence && confidenceLabel(s.GenderConfidence) != "" {
		gender, level = label, min(level, s.GenderConfidence)
	}
	if age == "" && gender == "" {
		return ""
	}
	if age != "" && gender != "" {
		gender = ", " + gender
	}
	guest := ""
	if c.IncludeGuestStatus && s.Guest {
		guest = " (гостевой профиль)"
	}
	values := map[string]string{"age": age, "gender": gender, "guest": guest, "confidence": confidenceLabel(level)}
	template := c.PromptTemplate
	if strings.TrimSpace(template) == "" {
		template = DefaultTemplate
	}
	// Unknown substitutions become empty strings instead of breaking the prompt.
	out := placeholder.ReplaceAllStringFunc(template, func(m string) string {
		return values[placeholder.FindStringSubmatch(m)[1]]
	})
	return strings.Join(strings.Fields(out), " ")
}
