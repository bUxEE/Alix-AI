package verify

import "testing"

func TestRandomLettersAreIdentifierLike(t *testing.T) {
	for w, want := range map[string]bool{
		"UbgFXUxjcxDQdzzlvsWgCw": true,  // a base64 nonce with no digit
		"qZxWvUtSrQpOnMlK":       true,  // the shortest flagged length
		"Internationalization":   false, // a long word
		"INTERNATIONALIZATION":   false, // shouted
		"PostgreSQL":             false, // too short to judge
		"JavaScriptDeveloper":    false, // a compound changes case a few times
		"Rossi":                  false,
	} {
		if got := randomLetters(w); got != want {
			t.Errorf("randomLetters(%q) = %v, want %v", w, got, want)
		}
	}
}
