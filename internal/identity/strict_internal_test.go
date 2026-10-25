package identity

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseObjectIsStrict(t *testing.T) {
	ok := []string{
		`{}`,
		`{"a":1}`,
		` {"a" : "b" , "c":[1,2]} `,
		`{"a":{"b":{"c":null}}}`,
	}
	for _, in := range ok {
		if _, err := parseObject([]byte(in)); err != nil {
			t.Errorf("parseObject(%q) rejected: %v", in, err)
		}
	}
	bad := map[string]string{
		"empty":                  ``,
		"array":                  `[]`,
		"string":                 `"a"`,
		"null":                   `null`,
		"number":                 `1`,
		"unterminated":           `{"a":1`,
		"trailing object":        `{"a":1}{"b":2}`,
		"trailing comma":         `{"a":1,}`,
		"trailing text":          `{"a":1} x`,
		"duplicate":              `{"a":1,"a":2}`,
		"duplicate after escape": `{"a":1,"a":2}`,
		"unquoted key":           `{a:1}`,
		"invalid utf-8 in value": "{\"a\":\"\xff\"}",
		"invalid utf-8 in key":   "{\"\xc3\":1}",
		"lone byte order mark":   "\xef\xbb\xbf{}",
	}
	for name, in := range bad {
		if _, err := parseObject([]byte(in)); err == nil {
			t.Errorf("%s: parseObject(%q) accepted", name, in)
		}
	}
	// Member names are case-sensitive: "ALG" is not "alg".
	m, err := parseObject([]byte(`{"ALG":"x"}`))
	if err != nil || len(m) != 1 || m["alg"] != nil {
		t.Fatalf("case handling: %v %v", m, err)
	}
}

func TestJSONStringRefusesLenientConversions(t *testing.T) {
	good := map[string]string{`"abc"`: "abc", `""`: "", `"*"`: "*", `"é"`: "é"}
	for in, want := range good {
		got, ok := jsonString(json.RawMessage(in))
		if !ok || got != want {
			t.Errorf("jsonString(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		`null`, `1`, `true`, `[]`, `{}`, ``, `"`, `"abc`,
		`"\ud800"`, // a lone surrogate: the standard library would substitute U+FFFD
		`"a�b"`,    // an explicit replacement character
		"\"\xff\"", // invalid UTF-8
		`"\u00zz"`, // broken escape
	} {
		if s, ok := jsonString(json.RawMessage(in)); ok {
			t.Errorf("jsonString(%q) = %q, accepted", in, s)
		}
	}
}

func TestJSONStringListRefusesNullElements(t *testing.T) {
	if l, ok := jsonStringList(json.RawMessage(`["a","b"]`), 4); !ok || len(l) != 2 {
		t.Fatalf("good list: %v %v", l, ok)
	}
	for _, in := range []string{`null`, `"a"`, `{}`, `[null]`, `["a",null]`, `[1]`, `[["a"]]`, `["a","b","c"]`, `[`, ``} {
		if l, ok := jsonStringList(json.RawMessage(in), 2); ok {
			t.Errorf("jsonStringList(%q) = %v, accepted", in, l)
		}
	}
}

func TestNumericDateIsStrict(t *testing.T) {
	good := map[string]int64{"0": 0, "1": 1, "1790000000": 1790000000, "999999999999": 999999999999}
	for in, want := range good {
		if got, ok := jsonNumericDate(json.RawMessage(in)); !ok || got != want {
			t.Errorf("jsonNumericDate(%s) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{
		``, `-1`, `+1`, `1.0`, `1.5`, `1e3`, `1E3`, `01`, `00`, ` 1`, `1 `, `"1"`, `null`, `true`,
		`1000000000000`, // thirteen digits: beyond the bound
		`9223372036854775807`, `18446744073709551616`,
		strings.Repeat("9", 40),
	} {
		if got, ok := jsonNumericDate(json.RawMessage(in)); ok {
			t.Errorf("jsonNumericDate(%q) = %d, accepted", in, got)
		}
	}
}

func TestIdentifierGrammars(t *testing.T) {
	for _, s := range []string{"k", "key-1", "a.b_c-D", strings.Repeat("k", 64)} {
		if !validKeyID(s) {
			t.Errorf("validKeyID(%q) = false", s)
		}
	}
	for _, s := range []string{"", "a b", "a/b", "a:b", "é", strings.Repeat("k", 65)} {
		if validKeyID(s) {
			t.Errorf("validKeyID(%q) = true", s)
		}
	}
	for _, s := range []string{strings.Repeat("a", 16), strings.Repeat("a", 128), "abcdefgh-ijkl_mnop.q"} {
		if !validTokenID(s) {
			t.Errorf("validTokenID(%q) = false", s)
		}
	}
	for _, s := range []string{"", "short", strings.Repeat("a", 15), strings.Repeat("a", 129), "has space has space ", "uuid/with/slashes/aaaa"} {
		if validTokenID(s) {
			t.Errorf("validTokenID(%q) = true", s)
		}
	}
}

func TestSplitCompact(t *testing.T) {
	if h, p, s, ok := splitCompact("a.b.c"); !ok || h != "a" || p != "b" || s != "c" {
		t.Fatalf("splitCompact(a.b.c) = %q %q %q %v", h, p, s, ok)
	}
	for _, in := range []string{"", "a", "a.b", "a.b.c.d", ".b.c", "a..c", "a.b.", "a.b.c=", "a.b.c\n", "a.b.c d", "a.+.c", "a./.c", "ä.b.c"} {
		if _, _, _, ok := splitCompact(in); ok {
			t.Errorf("splitCompact(%q) accepted", in)
		}
	}
}
