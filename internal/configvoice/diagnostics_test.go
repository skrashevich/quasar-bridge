package configvoice

import "testing"

func TestDiagnosticsCommandAcceptsOnlyWholeWebInterfaceCommands(t *testing.T) {
	for _, tc := range []struct {
		text    string
		enabled bool
	}{
		{"Алиса, включи веб-интерфейс", true},
		{"Алиса, выключи веб-интерфейс", false},
		{"включи веб интерфейс", true},
		{"выключи веб интерфейс", false},
		{"отключи веб-интерфейс", false},
		{"ВКЛЮЧИ ВЕБИНТЕРФЕЙС", true},
		{"Алиса включи веб интерфейс, пожалуйста!", true},
		{"  Пожалуйста, отключи веб интерфейс.  ", false},
		{"Алиса, пожалуйста включи веб-интерфейс", true},
	} {
		enabled, ok := DiagnosticsCommand(tc.text)
		if !ok || enabled != tc.enabled {
			t.Errorf("%q: got (%t, %t), want (%t, true)", tc.text, enabled, ok, tc.enabled)
		}
	}
}

func TestDiagnosticsCommandRejectsDiscussionAndOtherActions(t *testing.T) {
	for _, text := range []string{
		"не включи веб-интерфейс",
		"не включай веб-интерфейс",
		"Алиса, не выключи веб-интерфейс",
		"включи не веб интерфейс",
		"как включить веб-интерфейс",
		"скажи включи веб-интерфейс",
		"найди в истории включи веб-интерфейс",
		"«включи веб-интерфейс»",
		`"выключи веб-интерфейс"`,
		"включи веб интерфейс и выключи свет",
		"включи веб-интерфейс, потом включи свет",
		"включи свет и веб интерфейс",
		"включи веб интерфейс телевизора",
		"выключи веб интерфейс роутера",
		"открой веб-интерфейс",
		"включи веб камеру",
		"выключи интерфейс",
		"включи его",
		"выключи",
		"да",
		"",
	} {
		if enabled, ok := DiagnosticsCommand(text); ok || enabled {
			t.Errorf("%q: accepted unrelated or indirect request (%t, %t)", text, enabled, ok)
		}
	}
}
