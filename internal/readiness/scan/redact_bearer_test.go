package scan

import (
	"strings"
	"testing"
)

// TestRedactionRemovesTheValueAfterABearerScheme: "Token: Bearer <value>" used to lose only the word
// Bearer and keep the credential after it.
func TestRedactionRemovesTheValueAfterABearerScheme(t *testing.T) {
	secret := "tk" + "_" + "0123456789abcdefghij" // TEST-ONLY fake value built at run time
	for _, line := range []string{
		"Token: Bearer " + secret,
		"authorization = Bearer " + secret,
		`"api_token": "Bearer ` + secret + `"`,
		"Authorization: Basic " + "dXNlcjpwYXNzd29yZDEyMzQ1",
	} {
		got := ScanRedact(line)
		if strings.Contains(got, secret) || strings.Contains(got, "dXNlcjpwYXNzd29yZDEyMzQ1") {
			t.Errorf("ScanRedact(%q) = %q still carries the credential", line, got)
		}
		if !strings.Contains(got, ScanRedacted) {
			t.Errorf("ScanRedact(%q) = %q has no marker", line, got)
		}
	}
}
