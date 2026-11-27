package home

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// CatalogTTL controls how often the in-memory index of controllable HA devices
// and rooms is refreshed for device-aware routing.
const CatalogTTL = 5 * time.Minute

var catalogDomains = []string{"light", "switch", "fan", "media_player", "climate", "cover", "vacuum"}

type deviceCatalog struct {
	mu       sync.Mutex
	entities []Entity
	areas    []string
	summary  string
	built    time.Time
}

func (c *deviceCatalog) snapshot() (entities []Entity, areas []string, summary string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.built.IsZero() || time.Since(c.built) > 3*CatalogTTL {
		return nil, nil, "", false
	}
	return c.entities, c.areas, c.summary, true
}

// RefreshCatalog reloads the names, aliases, and rooms of controllable HA
// devices into memory so routing can distinguish real devices from media titles.
func (c *Controller) RefreshCatalog(ctx context.Context) error {
	entities, err := (&Controller{Tools: c.Tools, Domains: catalogDomains}).Entities(ctx)
	if err != nil {
		return err
	}
	c.setCatalog(entities)
	return nil
}

func (c *Controller) setCatalog(entities []Entity) {
	var areas []string
	seenArea := map[string]bool{}
	for _, e := range entities {
		if a := strings.TrimSpace(e.Area); a != "" && !seenArea[a] {
			seenArea[a] = true
			areas = append(areas, a)
		}
	}
	slices.Sort(areas)
	copied := slices.Clone(entities)
	summary := buildCatalogSummary(copied, areas)
	c.catalog.mu.Lock()
	defer c.catalog.mu.Unlock()
	c.catalog.entities = copied
	c.catalog.areas = areas
	c.catalog.summary = summary
	c.catalog.built = time.Now()
}

// normCatalogName folds ё, splits into lowercase alphanumeric tokens, and strips
// a single trailing Russian vowel/ь/й on words of 4+ runes for case-form matching.
func normCatalogName(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, w := range words {
		if r := []rune(w); len(r) >= 4 && strings.ContainsRune("аяуюыиеоьй", r[len(r)-1]) {
			words[i] = string(r[:len(r)-1])
		}
	}
	return strings.Join(words, " ")
}

func catalogRoomWords(s string) []string {
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
	return out
}

func containsWordSeq(hay, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(hay) {
		return false
	}
	for i := 0; i <= len(hay)-len(needle); i++ {
		if slices.Equal(hay[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

// genericHomeWords lists whole-word normalized (normCatalogName) smart-home nouns.
// Whole-word comparison avoids substring collisions with song/artist titles:
// "батарейка" -> "батарейк" != "батаре"; "Светлана" -> "светлан" != "свет".
var genericHomeWords = map[string]bool{
	"свет": true, "освещени": true, "ламп": true, "лампочк": true, "люстр": true,
	"подсветк": true, "светильник": true, "ночник": true, "бра": true, "торшер": true,
	"лент": true, "гирлянд": true, "розетк": true, "выключател": true, "реле": true,
	"рубильник": true, "штор": true, "жалюз": true, "карниз": true,
	"кондиционер": true, "термостат": true, "обогревател": true, "обогрев": true,
	"батаре": true, "радиатор": true, "вентилятор": true, "вытяжк": true,
	"увлажнител": true, "очистител": true, "пылесос": true,
	"телевизор": true, "приставк": true, "проектор": true, "тв": true, "tv": true,
	"колонк": true, "котел": true, "котл": true, "отоплени": true,
	"водонагревател": true, "бойлер": true, "горяч": true,
	"датчик": true, "температур": true, "влажност": true, "климат": true,
}

// HomeHint reports whether the live HA catalog is loaded, whether text mentions
// any real device/alias/room or generic smart-home noun, and returns a compact
// UTF-8-safe catalog summary for the routing classifier.
func (c *Controller) HomeHint(text string) (known bool, matches bool, summary string) {
	entities, areas, summary, ok := c.catalog.snapshot()
	if !ok {
		return false, false, ""
	}
	normWords := strings.Fields(normCatalogName(text))
	for _, w := range normWords {
		if genericHomeWords[w] {
			return true, true, summary
		}
	}
	rWords := catalogRoomWords(text)
	for _, area := range areas {
		if aw := catalogRoomWords(area); len(aw) > 0 && containsWordSeq(rWords, aw) {
			return true, true, summary
		}
	}
	for _, e := range entities {
		for _, n := range append([]string{e.Name}, e.Aliases...) {
			nw := strings.Fields(normCatalogName(n))
			if len(nw) == 0 || (len(nw) == 1 && len([]rune(nw[0])) < 2) {
				continue
			}
			if containsWordSeq(normWords, nw) {
				return true, true, summary
			}
		}
	}
	return true, false, summary
}

func buildCatalogSummary(entities []Entity, areas []string) string {
	const maxRunes = 900
	var b strings.Builder
	runeCount := 0
	appendStr := func(s string) bool {
		s = strings.ToValidUTF8(s, "")
		n := len([]rune(s))
		if runeCount+n > maxRunes {
			return false
		}
		b.WriteString(s)
		runeCount += n
		return true
	}
	if len(areas) > 0 && appendStr("Комнаты: ") {
		for i, a := range areas {
			sep := ""
			if i > 0 {
				sep = ", "
			}
			if !appendStr(sep + a) {
				break
			}
		}
		appendStr(". ")
	}
	var devs []string
	seen := map[string]bool{}
	for _, e := range entities {
		name := strings.TrimSpace(e.Name)
		if name == "" && len(e.Aliases) == 0 {
			continue
		}
		label := name
		if len(e.Aliases) > 0 {
			if label != "" {
				label += " (" + strings.Join(e.Aliases, ", ") + ")"
			} else {
				label = strings.Join(e.Aliases, ", ")
			}
		}
		if !seen[label] {
			seen[label] = true
			devs = append(devs, label)
		}
	}
	if len(devs) > 0 && appendStr("Устройства: ") {
		for i, d := range devs {
			sep := ""
			if i > 0 {
				sep = ", "
			}
			if !appendStr(sep + d) {
				break
			}
		}
		appendStr(".")
	}
	return strings.TrimSpace(b.String())
}
