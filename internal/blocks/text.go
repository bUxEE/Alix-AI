package blocks

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// textKind says what a string is, which decides what it may contain.
type textKind int

const (
	// kindLine is a single line of model text (a title, a label, a table cell,
	// a chart value): no newline, no tab, no invisible character, no markup.
	kindLine textKind = iota
	// kindLines is multi-line model text that is not code (Markdown, a diagram
	// source): newlines and tabs allowed, no invisible character.
	kindLines
	// kindCode is a code listing: shown verbatim, so only what hides the
	// text's real shape is refused (controls, bidirectional overrides,
	// invisible characters).
	kindCode
	// kindHost is a string the core or the host wrote (a card title, a
	// citation's display name). It may carry any script's punctuation and
	// joiners, but not what changes how neighbouring text is displayed.
	kindHost
)

// textRule returns the rule s violates for its kind, or "" when it is fine.
// The message never quotes s.
func textRule(s string, kind textKind) Rule {
	if !utf8.ValidString(s) {
		return RuleBadEncoding
	}
	if r := runeRule(s, kind); r != "" {
		return r
	}
	if kind != kindLine {
		return ""
	}
	// Entity references are decoded before the checks so that "&lt;script&gt;"
	// and "&#106;avascript:" are judged by what they say, not by how they are
	// spelled. What decodes to a control or hidden character is refused too.
	decoded := s
	if strings.ContainsRune(s, '&') {
		decoded = html.UnescapeString(s)
		if decoded != s {
			if !utf8.ValidString(decoded) {
				return RuleBadEncoding
			}
			if r := runeRule(decoded, kind); r != "" {
				return r
			}
		}
	}
	return markupRule(decoded)
}

// runeRule checks the characters of s.
func runeRule(s string, kind textKind) Rule {
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			if kind == kindLine || kind == kindHost {
				return RuleControlChar
			}
		case unicode.IsControl(r), r == 0x2028, r == 0x2029:
			return RuleControlChar
		case kind == kindHost:
			if isBidiControl(r) || r == 0xFEFF || (r >= 0xE0000 && r <= 0xE007F) {
				return RuleInvisibleChar
			}
		case unicode.Is(unicode.Cf, r):
			// Zero-width characters, bidirectional controls, a byte order mark,
			// Unicode tag characters and the soft hyphen: what a reader sees is
			// not what was checked ("Trojan source", ASCII smuggling).
			return RuleInvisibleChar
		}
	}
	return ""
}

// isBidiControl reports the bidirectional embedding, override and isolate
// controls.
func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x061C
}

var (
	// reTagStart is the schema's plainText rule: a '<' followed by a letter,
	// '/', '!' or '?' starts an HTML tag, comment or declaration.
	reTagStart = regexp.MustCompile(`<[A-Za-z/!?]`)
	// reActiveScheme finds a script-bearing URL scheme followed by something
	// (a language name followed by a colon and a space is just a label).
	reActiveScheme = regexp.MustCompile(`(?i)(?:^|[^a-z0-9+.-])(?:javascript|vbscript|livescript):\S`)
	// reDataURL finds a data URL: a media type, or the empty one, after "data:".
	reDataURL = regexp.MustCompile(`(?i)(?:^|[^a-z0-9+.-])data:(?:[a-z0-9.+-]+/[a-z0-9.+-]+|;|,)`)
	// reFileScheme finds the schemes that name a local or opaque resource.
	reFileScheme = regexp.MustCompile(`(?i)(?:^|[^a-z0-9+.-])(?:file|blob|filesystem|ms-[a-z]+):\S`)
)

// markupRule checks decoded single-line model text for markup, URLs and
// expressions: none of it belongs in a title, a label or a value, and a
// renderer must never be one string away from a script.
func markupRule(s string) Rule {
	switch {
	case reTagStart.MatchString(s):
		return RuleMarkup
	case strings.Contains(s, "://"):
		return RuleScheme
	case reActiveScheme.MatchString(s), reDataURL.MatchString(s), reFileScheme.MatchString(s):
		return RuleScheme
	case isExpression(s):
		return RuleExpression
	}
	return ""
}

// isExpression reports a template or formula marker: the strings a renderer,
// an export or a spreadsheet may evaluate.
func isExpression(s string) bool {
	if hasTemplateMarker(s) {
		return true
	}
	t := strings.TrimLeft(s, " ")
	return strings.HasPrefix(t, "=") || strings.HasPrefix(t, "@")
}

// hasTemplateMarker reports the delimiters of a template language: the strings
// a templating layer interpolates.
func hasTemplateMarker(s string) bool {
	return strings.Contains(s, "{{") || strings.Contains(s, "}}") || strings.Contains(s, "${") ||
		strings.Contains(s, "{%") || strings.Contains(s, "<%") || strings.Contains(s, "#{")
}
