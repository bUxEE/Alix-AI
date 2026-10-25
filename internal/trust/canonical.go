package trust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Limits of the canonical encoder.
const (
	// maxSafeInteger is 2^53-1, the largest integer every JSON implementation
	// reads back exactly (RFC 8785 section 3.2.2.3 and RFC 7493).
	maxSafeInteger = 1<<53 - 1
	// maxCanonicalDepth bounds the nesting of a canonicalised document so a
	// hostile input cannot exhaust the stack.
	maxCanonicalDepth = 32
)

// Canonicalize returns the canonical form of the JSON document raw: UTF-8,
// object members sorted by key, no insignificant whitespace.
//
// The form is the RFC 8785 (JSON Canonicalization Scheme) form restricted to
// what AlixAI signs. Differences from a general JSON library, all deliberate:
//
//   - duplicate object keys are an error, not "last one wins": two readers
//     must never disagree about what a signed document says;
//   - numbers must be integers with magnitude at most 2^53-1 (no fraction,
//     no exponent, no "-0"), so there is exactly one spelling and no float
//     formatting to get wrong;
//   - the input must be valid UTF-8 and one single JSON value with nothing
//     after it;
//   - strings are written with only the escapes RFC 8785 requires: \" \\
//     \b \f \n \r \t, and \u00xx (lower-case hex) for the other control
//     characters; everything else, including <, > & U+2028 and U+007F, is
//     written as is;
//   - keys are ordered by UTF-16 code units, as RFC 8785 specifies.
//
// A failure is contracts.ErrSchemaInvalid.
func Canonicalize(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, canonErr("input is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseCanonical(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, canonErr("data after the end of the JSON value")
	}
	var b bytes.Buffer
	b.Grow(len(raw))
	writeCanonical(&b, v)
	return b.Bytes(), nil
}

// CanonicalJSON returns the canonical form of v: v is marshalled with
// encoding/json (struct tags apply) and the result canonicalised by
// Canonicalize, so every restriction of Canonicalize applies to v too. Floats
// and integers above 2^53-1 are rejected. Strings must be valid UTF-8
// (encoding/json would otherwise replace the bad bytes silently).
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrSchemaInvalid, err, "trust: value cannot be marshalled to JSON")
	}
	return Canonicalize(raw)
}

func canonErr(format string, args ...any) error {
	return contracts.Newf(contracts.ErrSchemaInvalid, "trust: canonical JSON: "+format, args...)
}

// parseCanonical reads one value from the token stream. Objects become
// map[string]any (duplicates rejected), arrays []any, numbers json.Number
// that passed the integer rules.
func parseCanonical(dec *json.Decoder, depth int) (any, error) {
	if depth > maxCanonicalDepth {
		return nil, canonErr("nesting deeper than %d levels", maxCanonicalDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, canonErr("malformed JSON: %v", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, canonErr("malformed JSON: %v", err)
				}
				key, ok := kt.(string)
				if !ok {
					return nil, canonErr("object key is not a string")
				}
				if _, dup := obj[key]; dup {
					return nil, canonErr("duplicate object key %q", key)
				}
				val, err := parseCanonical(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil { // the closing brace
				return nil, canonErr("malformed JSON: %v", err)
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := parseCanonical(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // the closing bracket
				return nil, canonErr("malformed JSON: %v", err)
			}
			return arr, nil
		default:
			return nil, canonErr("unexpected %q", string(t))
		}
	case json.Number:
		if err := checkCanonicalInteger(t.String()); err != nil {
			return nil, err
		}
		return t, nil
	case string, bool, nil:
		return t, nil
	default:
		return nil, canonErr("unsupported JSON token %T", tok)
	}
}

// checkCanonicalInteger accepts -?(0|[1-9][0-9]*) with |n| <= 2^53-1 and
// rejects "-0".
func checkCanonicalInteger(lit string) error {
	digits := lit
	if len(digits) > 0 && digits[0] == '-' {
		digits = digits[1:]
	}
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return canonErr("number %s is not a canonical integer", lit)
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return canonErr("number %s is not an integer (fractions and exponents are not allowed)", lit)
		}
	}
	if lit == "-0" {
		return canonErr("number -0 is not allowed")
	}
	n, err := strconv.ParseInt(lit, 10, 64)
	if err != nil || n > maxSafeInteger || n < -maxSafeInteger {
		return canonErr("number %s is outside the safe integer range +-(2^53-1)", lit)
	}
	return nil
}

func writeCanonical(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortUTF16(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonicalString(b, k)
			b.WriteByte(':')
			writeCanonical(b, t[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, e)
		}
		b.WriteByte(']')
	case string:
		writeCanonicalString(b, t)
	case json.Number:
		b.WriteString(t.String())
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		// parseCanonical only produces the types above.
		panic(fmt.Sprintf("trust: writeCanonical: unexpected %T", v))
	}
}

const hexDigits = "0123456789abcdef"

func writeCanonicalString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		default:
			// Bytes >= 0x80 are the continuation of valid UTF-8 (checked on
			// entry) and are copied through unchanged.
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// sortUTF16 sorts keys by UTF-16 code units (RFC 8785 section 3.2.3). For
// keys made of BMP characters this equals byte order; they differ only for
// supplementary-plane characters against U+E000..U+FFFF.
func sortUTF16(keys []string) {
	if len(keys) < 2 {
		return
	}
	units := make(map[string][]uint16, len(keys))
	for _, k := range keys {
		units[k] = utf16.Encode([]rune(k))
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := units[keys[i]], units[keys[j]]
		for n := 0; n < len(a) && n < len(b); n++ {
			if a[n] != b[n] {
				return a[n] < b[n]
			}
		}
		return len(a) < len(b)
	})
}
