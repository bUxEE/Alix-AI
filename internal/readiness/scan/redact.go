package scan

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ScanRedacted replaces every secret-looking value in a string. It is applied to every excerpt,
// summary and detail before it is stored, so a report, a model prompt or a log never carries a
// credential that a readable file happened to hold. Over-redaction is the safe direction.
const ScanRedacted = "[REDACTED]"

var (
	// scanSecretAssignRE matches key = value and key: value where the key names a secret.
	scanSecretAssignRE = regexp.MustCompile(`(?i)(["']?[A-Za-z0-9_.\-]*(?:pass(?:word|wd|phrase)?|secret|token|api[_-]?key|apikey|private[_-]?key|credential|authorization|auth[_-]?key|access[_-]?key|signing[_-]?key|bearer)[A-Za-z0-9_.\-]*["']?)(\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s,;}\])]+)`)
	scanTokenShapes    = []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`),
		regexp.MustCompile(`sk_(?:live|test)_[A-Za-z0-9]{12,}`),
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`xox[abprs]-[A-Za-z0-9\-]{10,}`),
		regexp.MustCompile(`AIza[0-9A-Za-z_\-]{30,}`),
		regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]*`),
		regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=\-]{8,}`),
		regexp.MustCompile(`(?i)\bBasic\s+[A-Za-z0-9+/=]{12,}`),
	}
	scanURLUserinfoRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s:@]+:[^/\s@]+@`)
	// scanPEMBeginRE is the first line of a private key block; the label (RSA, EC, OPENSSH, ENCRYPTED...) names its END.
	scanPEMBeginRE = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]*)PRIVATE KEY-----`)
)

// scanKeepSuffixes are key endings that make a value a reference or a setting, not a secret.
var scanKeepSuffixes = []string{
	"ref", "env", "file", "path", "name", "id", "url", "header", "type", "mode", "kind", "policy", "required",
	"seconds", "days", "hours", "ms", "count", "length", "limit", "tokens", "scheme", "field", "claim",
}

var scanRefPrefixes = []string{"cred:", "env:", "secret:", "vault:", "ref:", "${", "$(", "$", "process.env", "os.environ", "<", "{{", "[redacted]", "***", "..."}

func scanLooksLikeReference(key, value string) bool {
	k := strings.ToLower(strings.Trim(key, `"'`))
	for _, suf := range scanKeepSuffixes {
		if strings.HasSuffix(k, suf) {
			return true
		}
	}
	v := strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`))
	switch v {
	case "", "null", "none", "true", "false", "~", "required", "optional", "string", "number", "integer", "boolean", "object":
		return true
	}
	for _, p := range scanRefPrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

// scanPEMEnd is the END marker of the private key block whose BEGIN marker matched loc in s.
func scanPEMEnd(s string, loc []int) string {
	return "-----END " + s[loc[2]:loc[3]] + "PRIVATE KEY-----"
}

// scanRedactPEM replaces every private key block of s, from its BEGIN marker through the END marker of the same
// label, with [REDACTED]. A block with no END is redacted to the end of s: the body of a key is base64, which no
// other rule recognises, so it is never left behind its header.
func scanRedactPEM(s string) string {
	if !strings.Contains(s, "PRIVATE KEY-----") {
		return s
	}
	var b strings.Builder
	for {
		loc := scanPEMBeginRE.FindStringSubmatchIndex(s)
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:loc[0]])
		b.WriteString(ScanRedacted)
		end := scanPEMEnd(s, loc)
		rest := s[loc[1]:]
		i := strings.Index(rest, end)
		if i < 0 {
			return b.String()
		}
		s = rest[i+len(end):]
	}
}

// ScanRedactLines redacts the lines of one text as ScanRedact does each, and withholds whole every line of a
// private key block: the BEGIN line, the base64 body and the END line of the same label, or every line to the end
// when there is no END. A caller that redacts a file line by line uses it, not ScanRedact per line, which sees the
// header of a key but not the body below it.
func ScanRedactLines(lines []string) []string {
	out := make([]string, len(lines))
	end := "" // the END marker of the block that is open, "" outside one
	for i, l := range lines {
		inKey := end != "" // the line starts inside a block: its body or its END line
		rest := l
		for {
			if end != "" {
				j := strings.Index(rest, end)
				if j < 0 {
					break
				}
				rest, end = rest[j+len(end):], ""
				continue
			}
			loc := scanPEMBeginRE.FindStringSubmatchIndex(rest)
			if loc == nil {
				break
			}
			inKey, end, rest = true, scanPEMEnd(rest, loc), rest[loc[1]:]
		}
		if inKey {
			out[i] = ScanRedacted
		} else {
			out[i] = ScanRedact(l)
		}
	}
	return out
}

// ScanRedact returns s with secret-looking values replaced by [REDACTED]. A private key block is removed whole,
// across lines, from its BEGIN marker through its END marker (or to the end of s).
func ScanRedact(s string) string {
	if s == "" {
		return s
	}
	s = scanRedactPEM(s)
	// Token shapes first: "Token: Bearer <value>" must lose the value after the scheme, which the
	// key = value rule below would otherwise leave behind (it takes "Bearer" as the value).
	for _, re := range scanTokenShapes {
		s = re.ReplaceAllString(s, ScanRedacted)
	}
	s = scanSecretAssignRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := scanSecretAssignRE.FindStringSubmatch(m)
		if len(sub) != 4 || scanLooksLikeReference(sub[1], sub[3]) {
			return m
		}
		return sub[1] + sub[2] + ScanRedacted
	})
	return scanURLUserinfoRE.ReplaceAllString(s, "${1}"+ScanRedacted+"@")
}

// scanExcerpt makes one redacted, bounded, single-line excerpt.
func scanExcerpt(line string, max int) string {
	line = strings.TrimSpace(line)
	line = ScanRedact(line)
	if utf8.RuneCountInString(line) > max {
		r := []rune(line)
		line = string(r[:max]) + "..."
	}
	var b strings.Builder
	for _, r := range line {
		if r < 0x20 && r != '\t' {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
