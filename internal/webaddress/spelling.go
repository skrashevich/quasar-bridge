package webaddress

import (
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Character names are exact speech forms, not guesses about similar words.
// Common prepositions and conjunctions (с, со, а, и, о, у) are excluded so an
// address cannot consume the surrounding request.
var characterNames = map[string]string{
	"эй": "a", "би": "b", "бэ": "b", "си": "c", "цэ": "c",
	"ди": "d", "дэ": "d", "е": "e", "э": "e", "эф": "f",
	"джи": "g", "гэ": "g", "эйч": "h", "аш": "h", "ай": "i",
	"джей": "j", "йот": "j", "кей": "k", "ка": "k", "эл": "l", "эль": "l",
	"эм": "m", "эн": "n", "оу": "o", "пи": "p", "пэ": "p", "кью": "q", "ку": "q",
	"ар": "r", "эр": "r", "эс": "s", "ти": "t", "тэ": "t", "ю": "u",
	"ви": "v", "вэ": "v", "дабл ю": "w", "дабл-ю": "w", "даблю": "w",
	"экс": "x", "икс": "x", "уай": "y", "игрек": "y", "зи": "z", "зед": "z", "зэт": "z",
	"ноль": "0", "нуль": "0", "один": "1", "два": "2", "три": "3", "четыре": "4",
	"пять": "5", "шесть": "6", "семь": "7", "восемь": "8", "девять": "9", "дефис": "-",
}

var spokenCharacter = func() string {
	var names []string
	for _, name := range slices.Sorted(maps.Keys(characterNames)) {
		names = append(names, strings.ReplaceAll(regexp.QuoteMeta(name), " ", `\s+`))
	}
	return `(?:` + strings.Join(names, "|") + `|[a-z0-9])`
}()

var spokenLabel = `(?:` + spokenCharacter + `(?:\s+` + spokenCharacter + `)+|` + label + `)`

func spellLabel(label string) (string, bool) {
	words := strings.Fields(label)
	var out strings.Builder
	for i := 0; i < len(words); i++ {
		value, ok := characterNames[words[i]]
		if i+1 < len(words) {
			if pair, found := characterNames[words[i]+" "+words[i+1]]; found {
				value, ok = pair, true
				i++
			}
		}
		if !ok {
			word := words[i]
			if len(word) != 1 || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", rune(word[0])) {
				return "", false
			}
			value = word
		}
		out.WriteString(value)
	}
	return out.String(), len(words) > 0
}
