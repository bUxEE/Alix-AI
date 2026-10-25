package secretssec

import (
	"bytes"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
)

// The detectors of the repository secret scan (SEC-084). Each one is a pure
// function of a file's path and content, so each can be run against inline
// planted violations and inline clean text (TestSecSecretDetectorsFlagPlanted...)
// before it is trusted on the repository.
//
// A detector never reports the secret itself: a finding carries the path, the line,
// the detector and a masked excerpt, because a failing scan must not copy what it
// found into CI logs.

// Detector names.
const (
	detPrivateKey = "private-key-block"
	detAWSKey     = "aws-access-key-id"
	detGitHub     = "github-token"
	detGeneric    = "secret-assignment"
	detJWT        = "jwt"
	detBlob       = "secret-blob"
	detEnvFile    = "env-file"
	detBearer     = "bearer-literal"
)

// testOnlyMarker is the in-content declaration that a key or token is published
// on purpose and protects nothing. It must be in the content; a path allowlist
// is not accepted.
const testOnlyMarker = "TEST-ONLY"

// githubDocumentedExample is the token GitHub's own documentation prints to show the format.
const githubDocumentedExample = "ghp_16C7e42F292c6912E7710c838347Ae178B4a"

// minJWTSignature is the shortest signature a JWT that signs something has: HS256 is 43
// base64url characters, RS256 342. A JWT with a short, empty or illustrative signature is a
// sample of the format, not a credential.
const minJWTSignature = 32

// markerWindow is how many lines before a finding (and, for the head of a file,
// from its start) the marker may stand.
const markerWindow = 15

// finding is one suspected secret.
type finding struct {
	Path     string
	Line     int
	Detector string
	Excerpt  string // masked: never the whole value
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d  %s  %s", f.Path, f.Line, f.Detector, f.Excerpt)
}

// mask shows the first four bytes of a secret and its length.
func mask(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return fmt.Sprintf("<%d bytes>", len(s))
	}
	return fmt.Sprintf("%s... <%d bytes>", s[:4], len(s))
}

// entropy is the Shannon entropy of s in bits per byte.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]float64
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		if c > 0 {
			p := c / n
			h -= p * math.Log2(p)
		}
	}
	return h
}

var (
	// reKeyBlock is the opening line of a PEM private key (RSA, EC, OpenSSH, PGP,
	// PKCS#8).
	reKeyBlock = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	reAWS      = regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA|AIPA)[A-Z0-9]{16}\b`)
	reGitHub   = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`)
	// reJWT: a JWT header and payload are base64url JSON objects, so both start
	// with eyJ. Three bare dot-separated words would flag every Go selector chain.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.([A-Za-z0-9_-]*)`)
	// reAssign: name = "value" or name: value, where the name mentions a secret.
	// The name may be wrapped in quotes (JSON, YAML) and carry a suffix or prefix
	// (client_secret, ACCESS_TOKEN, db-password).
	reAssign        = regexp.MustCompile(`(?i)(?:(?:api|signing|encryption|access|auth)[_-]?key|secret|passw(?:or)?d|token|credential|private[_-]?key)[A-Za-z0-9_.-]{0,24}["']?\s*(?::=|=>|[:=])\s*["'\x60]?([^\s"'\x60,;]{20,})`)
	reBearer        = regexp.MustCompile(`(?i)authorization["']?\s*[:=]\s*["']?bearer\s+([A-Za-z0-9._~+/=-]{20,})`)
	reBlobValue     = regexp.MustCompile(`^(?:[0-9a-fA-F]{40,}|[A-Za-z0-9+/_-]{40,}={0,2})$`)
	reSelector      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	reSyntheticWord = regexp.MustCompile(`(?i)secret|fixture|canary|dummy|fake|sample|mock|notreal|not-a-`)
	reCounting      = regexp.MustCompile(`(?i)0123456789|123456789|abcdefghij|abcdef0123`)
	// placeholder text: a value that says it is not a secret.
	rePlaceholder = regexp.MustCompile(`(?i)change[-_ ]?me|replace|example|placeholder|your[-_]|dummy|redacted|sample|fixme|todo|x{6,}|\*{4,}|\$\{|\$\(|\{\{|<[^>]+>|%[sdvq]|\benv:|\bfile:|secret://|^\$[A-Za-z_]|\.\.\.`)
	// reCodeLike: a value that is an expression, a call or a path, not a literal.
	reCodeLike = regexp.MustCompile(`[(){}\[\]<>\x60|!@#$%^&*\\]|^[a-z][a-z0-9+.-]*://|^[./~]`)
)

// scanContent runs every content detector over one file.
func scanContent(rel string, data []byte) []finding {
	if !mayHoldSecret(data) {
		return nil
	}
	var out []finding
	text := string(data)
	lines := strings.Split(text, "\n")

	// Private key blocks span lines, so they are found on the whole text.
	for _, loc := range reKeyBlock.FindAllStringIndex(text, -1) {
		if keyBodyLen(text[loc[1]:]) < minKeyBody {
			continue // a marker with no key behind it: a pattern, a check, a truncated fixture
		}
		line := 1 + strings.Count(text[:loc[0]], "\n")
		if markedTestOnly(lines, line) {
			continue
		}
		out = append(out, finding{rel, line, detPrivateKey, mask(text[loc[0]:loc[1]])})
	}

	for i, ln := range lines {
		n := i + 1
		if len(ln) > 8192 {
			ln = ln[:8192] // minified blobs: the head of a line is enough to see an assignment
		}
		if m := reAWS.FindString(ln); m != "" && !strings.Contains(m, "EXAMPLE") {
			out = append(out, finding{rel, n, detAWSKey, mask(m)})
		}
		if m := reGitHub.FindString(ln); m != "" && m != githubDocumentedExample {
			out = append(out, finding{rel, n, detGitHub, mask(m)})
		}
		// The remaining detectors read values a fixture may legitimately hold: one that says
		// TEST-ONLY next to it, in the content, is not a finding.
		if marked := markedTestOnly(lines, n); !marked {
			if m := reJWT.FindStringSubmatch(ln); m != nil && len(m[1]) >= minJWTSignature {
				out = append(out, finding{rel, n, detJWT, mask(m[0])})
			}
			if m := reBearer.FindStringSubmatch(ln); m != nil && secretLike(m[1], 3.0, true) {
				out = append(out, finding{rel, n, detBearer, mask(m[1])})
			}
			for _, m := range reAssign.FindAllStringSubmatch(ln, 4) {
				val := m[1]
				if reBlobValue.MatchString(val) && secretLike(val, 3.0, false) {
					out = append(out, finding{rel, n, detBlob, mask(val)})
					continue
				}
				if secretLike(val, 3.5, true) {
					out = append(out, finding{rel, n, detGeneric, mask(val)})
				}
			}
		}
	}
	return out
}

// secretLike decides whether a literal value looks like a real secret: long
// enough, not a placeholder, not code, high enough entropy and (for needMix) a
// mix of letters and digits, which random keys have and identifiers do not.
func secretLike(val string, minEntropy float64, needMix bool) bool {
	if len(val) < 20 || strings.Contains(val, githubDocumentedExample) || rePlaceholder.MatchString(val) || reCodeLike.MatchString(val) || reSelector.MatchString(val) || synthetic(val) {
		return false
	}
	if entropy(val) < minEntropy {
		return false
	}
	if needMix {
		hasDigit, hasLetter := false, false
		for _, r := range val {
			switch {
			case r >= '0' && r <= '9':
				hasDigit = true
			case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
				hasLetter = true
			}
		}
		if !hasDigit || !hasLetter {
			return false
		}
	}
	return true
}

// synthetic reports whether a value declares itself made up: it names itself a secret or a
// fixture, counts (0123456789, abcdef0123) or reads as a phrase of words. A random credential
// does none of these, and a fake one written for a redaction test almost always does.
func synthetic(val string) bool {
	if reSyntheticWord.MatchString(val) || reCounting.MatchString(val) {
		return true
	}
	words := 0
	for _, seg := range strings.FieldsFunc(val, func(r rune) bool { return r == '-' || r == '_' }) {
		if len(seg) >= 3 && singleCaseLetters(seg) {
			words++
		}
	}
	return words >= 3
}

// singleCaseLetters reports whether s is all letters and all lower case, all upper case or
// capitalised: a word, not a random run.
func singleCaseLetters(s string) bool {
	lower, upper := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z':
			lower++
		case c >= 'A' && c <= 'Z':
			upper++
		default:
			return false
		}
	}
	return upper == 0 || lower == 0 || (upper == 1 && s[0] >= 'A' && s[0] <= 'Z')
}

// mayHoldSecret is the cheap pre-filter: binary content (a NUL in the head) is
// never scanned.
func mayHoldSecret(data []byte) bool {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	return !bytes.Contains(head, []byte{0})
}

// minKeyBody is the shortest base64 body that can be a private key: an Ed25519
// PKCS#8 body is 64 characters, RSA and EC are longer. A shorter run behind a
// marker is a truncated or illustrative block.
const minKeyBody = 40

// keyBodyLen counts the base64 characters that follow a BEGIN line, looking
// through line ends and the escape sequences \n and \r (a key kept in a string).
func keyBodyLen(rest string) int {
	if len(rest) > 8192 {
		rest = rest[:8192]
	}
	n := 0
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '\r' || c == '\n' || c == ' ' || c == '\t' || c == '"' || c == '\'' || c == '+' || c == '\x60':
			continue
		case c == '\\' && i+1 < len(rest) && (rest[i+1] == 'n' || rest[i+1] == 'r'):
			i++
			continue
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=':
			n++
			continue
		}
		return n // the END marker, a header line or anything that is not base64
	}
	return n
}

// markedTestOnly reports whether the TEST-ONLY marker stands in the content
// within markerWindow lines before line (1-based), on the line itself, or in the
// first markerWindow lines of the file (a key file's header).
func markedTestOnly(lines []string, line int) bool {
	lo := line - 1 - markerWindow
	if lo < 0 {
		lo = 0
	}
	for i := lo; i < line && i < len(lines); i++ {
		if strings.Contains(lines[i], testOnlyMarker) {
			return true
		}
	}
	for i := 0; i < markerWindow && i < len(lines); i++ {
		if strings.Contains(lines[i], testOnlyMarker) {
			return true
		}
	}
	return false
}

// isEnvFileName reports whether a path is an environment file that must not be
// committed: .env, *.env and .env.* — except the *.env.example templates.
func isEnvFileName(rel string) bool {
	base := path.Base(rel)
	switch {
	case strings.HasSuffix(base, ".example") || strings.HasSuffix(base, ".sample") || strings.HasSuffix(base, ".template"):
		return false
	case base == ".env", strings.HasSuffix(base, ".env"), strings.HasPrefix(base, ".env."):
		return true
	}
	return false
}

func sortFindings(f []finding) {
	sort.Slice(f, func(i, j int) bool {
		if f[i].Path != f[j].Path {
			return f[i].Path < f[j].Path
		}
		if f[i].Line != f[j].Line {
			return f[i].Line < f[j].Line
		}
		return f[i].Detector < f[j].Detector
	})
}
