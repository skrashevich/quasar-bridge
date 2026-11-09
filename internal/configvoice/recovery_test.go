package configvoice

import "testing"

// An unanchored match could reboot a speaker when discussing or negating recovery.
func TestRecoveryActionRequiresAnExplicitWholeCommand(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"Алиса, аварийно переведи квазар бридж в режим observe, пожалуйста!", "observe"},
		{"Переключи quasar-bridge в observe", "observe"},
		{"переведи qwazar bridge в режим наблюдения", "observe"},
		{"аварийный откат квазар бридж", "rollback"},
		{"Выполни аварийный rollback quasar bridge", "rollback"},
		{"переведи квазар блич в режим rollback", "rollback"},
		{"не переводи квазар бридж в observe", ""},
		{"не выполни аварийный rollback quasar bridge", ""},
		{"как переключить quasar bridge в observe", ""},
		{"скажи переведи quasar bridge в observe", ""},
		{"«переведи quasar bridge в observe»", ""},
		{"\"аварийный откат квазар бридж\"", ""},
		{"найди в истории аварийный откат квазар бридж", ""},
		{"переведи quasar bridge в observe и выключи свет", ""},
		{"переведи телевизор в observe", ""},
		{"переведи в observe", ""},
		{"откати обновление quasar bridge", ""},
		{"перезапусти quasar bridge", ""},
		{"переведи quasar bridge в hybrid", ""},
		{"", ""},
	} {
		if got := RecoveryAction(tc.text); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.text, got, tc.want)
		}
	}
}
