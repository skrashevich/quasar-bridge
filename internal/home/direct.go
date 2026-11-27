package home

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// NameIndexTTL bounds how often the in-memory device index is refreshed for
// direct commands (home_direct_commands).
const NameIndexTTL = 5 * time.Minute

var (
	directOnOffDomains = []string{"light", "switch", "fan"}
	directIndexDomains = []string{"light", "switch", "fan", "climate"}
)

type nameIndex struct {
	mu       sync.Mutex
	byName   map[string][]Entity
	entities []Entity
	built    time.Time
}

func (x *nameIndex) lookup(name string) ([]Entity, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil, false
	}
	return x.byName[name], true
}

func (x *nameIndex) allEntities() ([]Entity, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byName == nil || time.Since(x.built) > 3*NameIndexTTL {
		return nil, false
	}
	return x.entities, true
}

// RefreshNames reloads controllable entity names and aliases into memory so
// unambiguous direct commands can skip inventory and LLM planning.
func (c *Controller) RefreshNames(ctx context.Context) error {
	entities, err := (&Controller{Tools: c.Tools, Domains: directIndexDomains}).Entities(ctx)
	if err != nil {
		return err
	}
	c.setNameIndex(entities)
	return nil
}

func (c *Controller) setNameIndex(entities []Entity) {
	byName := map[string][]Entity{}
	for _, e := range entities {
		seen := map[string]bool{}
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if k := normName(n); k != "" && !seen[k] {
				seen[k] = true
				byName[k] = append(byName[k], e)
			}
		}
	}
	copied := slices.Clone(entities)
	c.names.mu.Lock()
	defer c.names.mu.Unlock()
	c.names.byName = byName
	c.names.entities = copied
	c.names.built = time.Now()
}

// normName lowercases, folds ё, keeps alphanumeric tokens, and drops a single
// trailing Russian vowel/ь/й on words of 4+ runes so spoken case forms match
// friendly names and aliases ("подсветку над столом" == "Подсветка над столом").
func normName(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, w := range words {
		if r := []rune(w); len(r) >= 4 && strings.ContainsRune("аяуюыиеоьй", r[len(r)-1]) {
			words[i] = string(r[:len(r)-1])
		}
	}
	return strings.Join(words, " ")
}

func foldAdjectiveWords(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	raw := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := make([]string, 0, len(raw))
	for _, w := range raw {
		if len([]rune(w)) >= 4 {
			for _, suffix := range []string{"ая", "яя", "ое", "ее", "ой", "ей", "ую", "юю", "ом", "ем", "ые", "ие"} {
				if strings.HasSuffix(w, suffix) && len([]rune(w))-len([]rune(suffix)) >= 3 {
					w = strings.TrimSuffix(w, suffix)
					break
				}
			}
		}
		if r := []rune(w); len(r) >= 4 && strings.ContainsRune("аяуюыиеоьй", r[len(r)-1]) {
			w = string(r[:len(r)-1])
		}
		if w != "" {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// directCommand splits "Включи подсветку над столом." into ("turn_on", "подсветк над стол").
func directCommand(text string) (service, name string, ok bool) {
	trimmed := strings.TrimSpace(strings.ToLower(text))
	trimmed = strings.TrimPrefix(trimmed, "алиса, ")
	trimmed = strings.TrimPrefix(trimmed, "алиса ")
	verb, rest, _ := strings.Cut(strings.TrimSpace(trimmed), " ")
	svc := map[string]string{"включи": "turn_on", "выключи": "turn_off"}[verb]
	if name := normName(rest); svc != "" && name != "" {
		return svc, name, true
	}
	return "", "", false
}

func (c *Controller) directOnOffPlan(text string) (Plan, Entity, bool) {
	service, name, ok := directCommand(text)
	if !ok {
		return Plan{}, Entity{}, false
	}
	matches, ok := c.names.lookup(name)
	if !ok {
		return Plan{}, Entity{}, false
	}
	var direct []Entity
	for _, m := range matches {
		domain, _, _ := strings.Cut(m.ID, ".")
		if slices.Contains(directOnOffDomains, domain) && m.State != "unavailable" && m.State != "unknown" {
			direct = append(direct, m)
		}
	}
	if len(direct) != 1 {
		return Plan{}, Entity{}, false
	}
	e := direct[0]
	domain, _, _ := strings.Cut(e.ID, ".")
	return Plan{Actions: []Action{{Domain: domain, Service: service, EntityID: e.ID, Data: map[string]any{}}}}, e, true
}

// setpointCommand parses commands like "сделай колонку 41",
// "поставь котёл на 22 градуса", "установи горячую воду 40".
// Relative changes ("теплее на 2 градуса") return ok=false for the LLM planner.
func setpointCommand(text string) (target string, temp float64, ok bool) {
	s := strings.ReplaceAll(strings.ToLower(text), "ё", "е")
	raw := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != ','
	})
	for i := range raw {
		raw[i] = strings.Trim(raw[i], ".,")
	}
	raw = slices.DeleteFunc(raw, func(w string) bool { return w == "" })
	for len(raw) > 0 && (raw[0] == "алиса" || raw[0] == "пожалуйста") {
		raw = raw[1:]
	}
	for len(raw) > 0 && raw[len(raw)-1] == "пожалуйста" {
		raw = raw[:len(raw)-1]
	}
	if len(raw) < 3 {
		return "", 0, false
	}
	switch raw[0] {
	case "сделай", "поставь", "установи", "выстави", "задай":
	default:
		return "", 0, false
	}
	rest := raw[1:]
	for _, w := range rest {
		switch w {
		case "теплее", "холоднее", "выше", "ниже", "больше", "меньше", "плюс", "минус", "прибавь", "убавь":
			return "", 0, false
		}
	}
	numIdx := -1
	var parsed float64
	for i, w := range rest {
		clean := strings.Trim(w, ".,")
		if clean == "" {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(clean, ",", "."), 64)
		if err == nil {
			if numIdx >= 0 {
				return "", 0, false
			}
			numIdx = i
			parsed = v
		}
	}
	if numIdx < 0 || parsed < 5 || parsed > 85 {
		return "", 0, false
	}
	var nameWords []string
	for i, w := range rest {
		if i == numIdx {
			continue
		}
		nw := normName(w)
		if nw == "" {
			continue
		}
		if nw == "на" || nw == "до" || nw == "в" || nw == "во" || nw == "температур" ||
			strings.HasPrefix(nw, "градус") || nw == "цельси" || nw == "c" || nw == "с" {
			continue
		}
		nameWords = append(nameWords, nw)
	}
	target = strings.Join(nameWords, " ")
	if target == "" {
		return "", 0, false
	}
	return target, parsed, true
}

func matchSetpointEntity(text string, entities []Entity) (Plan, Entity, bool) {
	target, temp, ok := setpointCommand(text)
	if !ok {
		return Plan{}, Entity{}, false
	}
	targetKey := foldAdjectiveWords(target)
	var exact []Entity
	seen := map[string]bool{}
	for _, e := range entities {
		// Powered-off climate entities stay on the LLM path so it can emit turn_on + set_temperature.
		if !strings.HasPrefix(e.ID, "climate.") || e.State == "unavailable" || e.State == "unknown" || e.State == "off" || seen[e.ID] {
			continue
		}
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			if normName(n) == target || (targetKey != "" && foldAdjectiveWords(n) == targetKey) {
				seen[e.ID] = true
				exact = append(exact, e)
				break
			}
		}
	}
	if len(exact) == 1 {
		e := exact[0]
		return Plan{Actions: []Action{{Domain: "climate", Service: "set_temperature", EntityID: e.ID, Data: map[string]any{"temperature": temp}}}}, e, true
	}
	if len(exact) == 0 && (targetKey == "колонк" || targetKey == "газов колонк" || targetKey == "водонагревател" || targetKey == "вод" || targetKey == "горяч вод") {
		var dhw []Entity
		for _, e := range entities {
			if !strings.HasPrefix(e.ID, "climate.") || e.State == "unavailable" || e.State == "unknown" || e.State == "off" {
				continue
			}
			hay := strings.ToLower(e.ID + " " + e.Name + " " + strings.Join(e.Aliases, " "))
			if strings.Contains(hay, "dhw") || strings.Contains(hay, "горяч") || strings.Contains(hay, "колонк") || strings.Contains(hay, "водонагрев") {
				dhw = append(dhw, e)
			}
		}
		if len(dhw) == 1 {
			e := dhw[0]
			return Plan{Actions: []Action{{Domain: "climate", Service: "set_temperature", EntityID: e.ID, Data: map[string]any{"temperature": temp}}}}, e, true
		}
	}
	return Plan{}, Entity{}, false
}

func (c *Controller) directSetpointPlan(text string) (Plan, Entity, bool) {
	entities, ok := c.names.allEntities()
	if !ok {
		return Plan{}, Entity{}, false
	}
	return matchSetpointEntity(text, entities)
}
