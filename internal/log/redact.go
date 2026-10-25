package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted replaces every value the redaction layer removes.
const Redacted = "[REDACTED]"

// sensitiveFragments are matched as substrings of a normalised key (lower
// case, "_" and "-" removed), so "api_key", "X-Api-Key", "apiKey",
// "access_token" and "client_secret" all match. The list is token, secret,
// password, authorization, api_key and cookie plus their close relatives.
var sensitiveFragments = []string{
	"token", "secret", "password", "passwd", "authorization", "apikey", "cookie",
	"privatekey", "credential",
}

// Redactor decides what must never reach a log line. It is immutable and safe
// for concurrent use.
type Redactor struct {
	extra map[string]struct{}
}

// NewRedactor returns a Redactor that, besides the built-in sensitive-key
// rule, redacts any key equal to one of extraKeys (compared case-insensitively
// and ignoring "_" and "-").
func NewRedactor(extraKeys []string) *Redactor {
	r := &Redactor{extra: make(map[string]struct{}, len(extraKeys))}
	for _, k := range extraKeys {
		if n := normalizeKey(k); n != "" {
			r.extra[n] = struct{}{}
		}
	}
	return r
}

func normalizeKey(k string) string {
	return strings.Map(func(c rune) rune {
		switch {
		case c == '_' || c == '-' || c == ' ':
			return -1
		case c >= 'A' && c <= 'Z':
			return c - 'A' + 'a'
		}
		return c
	}, k)
}

// SensitiveKey reports whether values logged under key must be redacted.
func (r *Redactor) SensitiveKey(key string) bool {
	n := normalizeKey(key)
	if n == "" {
		return false
	}
	if _, ok := r.extra[n]; ok {
		return true
	}
	for _, f := range sensitiveFragments {
		if strings.Contains(n, f) {
			return true
		}
	}
	return false
}

const keyFragmentRE = `(?:token|secret|passw(?:or)?d|authorization|api[_\-]?key|cookie|credential|private[_\-]?key)`

// stringPatterns scrub secrets embedded in free text. Order matters: the
// header and bearer rules run before the generic key=value rule so that a
// credential scheme ("Bearer", "Basic") is not mistaken for the value.
var stringPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// "Authorization: ...", "Cookie: ...", "Set-Cookie: ...", "X-Api-Key: ..." to end of line.
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|(?:set-)?cookie|x-api-key)(\s*:\s*)[^\r\n]+`), "${1}${2}" + Redacted},
	// Bearer tokens anywhere in a string.
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9\-._~+/]+=*`), "${1}" + Redacted},
	// JSON members: "api_key": "...".
	{regexp.MustCompile(`(?i)("[\w.\-]*` + keyFragmentRE + `[\w.\-]*"\s*:\s*)("(?:[^"\\]|\\.)*"|[^\s,}\]]+)`), `${1}"` + Redacted + `"`},
	// key=value pairs, e.g. URL query strings: ?token=..., password=...
	{regexp.MustCompile(`(?i)\b([\w.\-]*` + keyFragmentRE + `[\w.\-]*)(\s*=\s*)("[^"]*"|'[^']*'|(?:(?:bearer|basic|digest|negotiate|token)\s+)?[^\s&,;"']+)`), "${1}${2}" + Redacted},
	// JSON web tokens.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]*`), Redacted},
	// Credentials in URLs: scheme://user:password@host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^\s/@:]+:[^\s/@]+@`), "${1}" + Redacted + "@"},
	// PEM private keys, through the end marker or, if truncated, the end of the string.
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), Redacted},
}

// String removes secrets embedded in s: bearer tokens, JWTs, credential
// header lines, key=value and JSON members whose key looks sensitive, URL
// userinfo and PEM private keys. Text without any of those is returned as is.
func (r *Redactor) String(s string) string {
	for _, p := range stringPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// RedactString applies the built-in string patterns, with no configured keys.
func RedactString(s string) string { return defaultRedactor.String(s) }

var defaultRedactor = NewRedactor(nil)

// replaceAttr is the slog.HandlerOptions.ReplaceAttr hook. The JSON handler
// calls it for every non-group attribute: the message, attributes added with
// Logger.With, attributes inside groups (groups holds the open group names)
// and per-record attributes, after any LogValuer has been resolved.
func (r *Redactor) replaceAttr(groups []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindTime:
		if len(groups) == 0 && a.Key == slog.TimeKey {
			a.Value = slog.TimeValue(a.Value.Time().UTC())
			return a
		}
	case slog.KindAny:
		switch a.Value.Any().(type) {
		case slog.Level, *slog.Source:
			if len(groups) == 0 {
				return a // the handler's own level and source attributes
			}
		}
	}

	if r.SensitiveKey(a.Key) {
		a.Value = slog.StringValue(Redacted)
		return a
	}
	for _, g := range groups {
		if r.SensitiveKey(g) {
			a.Value = slog.StringValue(Redacted)
			return a
		}
	}
	a.Value = r.value(a.Value)
	return a
}

func (r *Redactor) value(v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(r.String(v.String()))
	case slog.KindAny:
		switch x := v.Any().(type) {
		case nil:
			return v
		case error:
			return slog.StringValue(r.String(x.Error()))
		}
		return r.anyValue(v.Any())
	}
	return v // numbers, bools, durations and times cannot embed a secret
}

// anyValue redacts an arbitrary value (a map of headers, a struct, a slice).
// It works on the JSON form of the value, which is exactly what the JSON
// handler would have written: struct tags, maps and nesting are all honoured.
// A value that cannot be marshalled is dropped rather than risk logging it raw.
func (r *Redactor) anyValue(x any) slog.Value {
	b, err := json.Marshal(x)
	if err != nil {
		return slog.StringValue(Redacted + " (unserialisable value)")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep large integers exact
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return slog.StringValue(Redacted + " (unserialisable value)")
	}
	return slog.AnyValue(r.walk(generic))
}

func (r *Redactor) walk(x any) any {
	switch t := x.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			if r.SensitiveKey(k) {
				out[k] = Redacted
			} else {
				out[k] = r.walk(v)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = r.walk(v)
		}
		return out
	case string:
		return r.String(t)
	}
	return x
}
