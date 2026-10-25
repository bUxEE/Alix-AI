package scan

import (
	"strings"
	"testing"
)

// scanTestPEM is a private key block of TEST-ONLY lines, assembled at run time so no key-shaped literal sits in the
// source. The body lines are plain base64 that no other rule recognises.
func scanTestPEM(label string) (block string, body []string) {
	body = []string{"TESTONLYbase64bodyAAAA0123456789abcdef", "TESTONLYbase64bodyBBBB0123456789abcdef", "TESTONLYbase64tailCCCC=="}
	return "-----BEGIN " + label + "PRIVATE KEY-----\n" + strings.Join(body, "\n") + "\n-----END " + label + "PRIVATE KEY-----", body
}

// A private key block is removed whole: ScanRedact of a multi-line text loses the body as well as the header, and
// the text after the END marker is kept.
func TestRedactionRemovesAMultiLinePrivateKeyBlock(t *testing.T) {
	for _, label := range []string{"", "RSA ", "OPENSSH ", "ENCRYPTED "} {
		block, body := scanTestPEM(label)
		got := ScanRedact("before\n" + block + "\nafter")
		for _, b := range body {
			if strings.Contains(got, b) {
				t.Errorf("%q: the body line %q survived: %q", label, b, got)
			}
		}
		if strings.Contains(got, "PRIVATE KEY") || !strings.HasPrefix(got, "before\n"+ScanRedacted) || !strings.HasSuffix(got, "\nafter") {
			t.Errorf("%q: ScanRedact = %q", label, got)
		}
	}
	// No END: everything after the header is withheld.
	block, body := scanTestPEM("EC ")
	cut := block[:strings.Index(block, "-----END")]
	if got := ScanRedact("x\n" + cut + "\nmore text"); strings.Contains(got, body[0]) || strings.Contains(got, "more text") {
		t.Errorf("an unterminated block left %q", got)
	}
	// The END must be the block's own: an END of another label does not close it.
	if got := ScanRedact(strings.Replace(block, "-----END EC ", "-----END RSA ", 1) + "\ntail"); strings.Contains(got, "tail") || strings.Contains(got, body[2]) {
		t.Errorf("an END of another label closed the block: %q", got)
	}
}

// ScanRedactLines withholds every line of a private key block, from BEGIN through the matching END, or to the end
// of the input when there is no END; the lines around it are redacted as ScanRedact does each.
func TestRedactLinesWithholdsEveryLineOfAPrivateKeyBlock(t *testing.T) {
	block, body := scanTestPEM("RSA ")
	lines := append(append([]string{"# Setup", "password = hunter" + "2-testonly"}, strings.Split(block, "\n")...), "after the key", "-----END RSA PRIVATE KEY-----")
	got := ScanRedactLines(lines)
	if len(got) != len(lines) {
		t.Fatalf("%d lines, want %d", len(got), len(lines))
	}
	if got[0] != "# Setup" || got[1] != "password = "+ScanRedacted {
		t.Errorf("the lines before the key = %q", got[:2])
	}
	for i := 2; i < 2+len(body)+2; i++ {
		if got[i] != ScanRedacted {
			t.Errorf("line %d of the block = %q, want it withheld", i, got[i])
		}
	}
	if got[len(got)-2] != "after the key" {
		t.Errorf("the line after the END = %q, want it kept", got[len(got)-2])
	}
	// A stray END outside a block is ordinary text (it holds no secret).
	if got[len(got)-1] != "-----END RSA PRIVATE KEY-----" {
		t.Errorf("a stray END = %q", got[len(got)-1])
	}

	// Without an END every line to the end is withheld.
	open := []string{"intro", "-----BEGIN PRIVATE KEY-----", body[0], body[1], "not a key, but after an open block"}
	for i, l := range ScanRedactLines(open) {
		if want := i >= 1; (l == ScanRedacted) != want {
			t.Errorf("unterminated block, line %d = %q", i, l)
		}
	}
	// A one-line block with text after it on the same line, then another block opening on a later line.
	one := []string{"k: -----BEGIN EC PRIVATE KEY-----" + body[0] + "-----END EC PRIVATE KEY----- tail", "plain", "-----BEGIN DSA PRIVATE KEY-----", body[1], "-----END DSA PRIVATE KEY-----", "done"}
	want := []string{ScanRedacted, "plain", ScanRedacted, ScanRedacted, ScanRedacted, "done"}
	if got := ScanRedactLines(one); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ScanRedactLines = %q, want %q", got, want)
	}
}
