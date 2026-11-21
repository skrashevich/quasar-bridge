package assistant

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode"
)

type ttsCatalogEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Each voice returned distinct, decodable Opus on repeated UniProxy
// TtsGenerate requests using <speaker voice="..."> with ru-RU.
var ttsVoiceCatalog = []ttsCatalogEntry{
	{ID: "alyss", Name: "Алисс"},
	{ID: "jane", Name: "Джейн"},
	{ID: "ermil", Name: "Ермил"},
	{ID: "zahar", Name: "Захар"},
	{ID: "kostya", Name: "Костя"},
	{ID: "oksana", Name: "Оксана"},
	{ID: "omazh", Name: "Омаж"},
}

func searchTTSCatalog(args string) ([]ttsCatalogEntry, error) {
	var request struct {
		Kind  string `json:"kind"`
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &request, json.RejectUnknownMembers(true)); err != nil {
		return nil, errors.New("invalid search_tts_catalog arguments")
	}
	var entries []ttsCatalogEntry
	switch request.Kind {
	case "voice":
		entries = ttsVoiceCatalog
	case "audio":
		entries = ttsSoundCatalog
	default:
		return nil, errors.New("search_tts_catalog kind must be voice or audio")
	}
	query := normalizeTTSName(request.Query)
	if query == "" {
		return nil, errors.New("search_tts_catalog requires a name or ID")
	}
	words := strings.Fields(query)
	id := strings.TrimSpace(request.Query)
	var exact, partial []ttsCatalogEntry
	for _, entry := range entries {
		if strings.EqualFold(id, entry.ID) {
			return []ttsCatalogEntry{entry}, nil
		}
		name := normalizeTTSName(entry.Name)
		if query == name {
			exact = append(exact, entry)
			continue
		}
		text := name + " " + normalizeTTSName(entry.ID)
		matches := true
		for _, word := range words {
			if !strings.Contains(text, word) {
				matches = false
				break
			}
		}
		if matches {
			partial = append(partial, entry)
		}
	}
	result := append(exact, partial...)
	return result[:min(8, len(result))], nil
}

func normalizeTTSName(text string) string {
	text = strings.ReplaceAll(strings.ToLower(text), "ё", "е")
	return strings.Join(strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}
