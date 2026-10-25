package apppkg

import (
	"strings"
	"testing"
)

// TestYAMLAuthoringRules: section 2 of docs/integration/application-package.md.
// A package file is plain data; the loader refuses what could mean something
// other than what it says. Each input is placed in a terminology file of an
// otherwise valid package, and the only finding must be yaml.invalid.
func TestYAMLAuthoringRules(t *testing.T) {
	good := "terms:\n- id: extra\n  labels:\n    en:\n      preferred: extra\n      definition: An extra term.\n"
	cases := []struct {
		name string
		body string
	}{
		{"anchor", "terms: &t\n- id: extra\n"},
		{"alias", "a: &x 1\nterms: *x\n"},
		{"merge key", "base: &b\n  id: extra\nterms:\n- <<: *b\n"},
		{"explicit tag", "terms: !!seq []\n"},
		{"custom tag", "terms: !local []\n"},
		{"duplicate key", "terms: []\nterms: []\n"},
		{"duplicate nested key", "terms:\n- id: a\n  id: b\n"},
		{"non-string key", "1: a\nterms: []\n"},
		{"complex key", "? [a, b]\n: c\nterms: []\n"},
		{"carriage return", "terms: []\r\n"},
		{"byte order mark", "\xef\xbb\xbfterms: []\n"},
		{"NUL byte", "terms: []\n\x00"},
		{"control character", "terms: []\n\x07"},
		{"invalid UTF-8", "terms: []\n# \xff\xfe\n"},
		{"two documents", "terms: []\n---\nterms: []\n"},
		{"empty file", ""},
		{"only a comment", "# nothing here\n"},
		{"unquoted date", "terms: []\nwhen: 2026-11-13\n"},
		{"unquoted timestamp", "terms: []\nwhen: 2026-11-13T10:00:00Z\n"},
		{"hexadecimal number", "terms: []\nn: 0x1F\n"},
		{"octal number", "terms: []\nn: 0o17\n"},
		{"integer beyond int64", "terms: []\nn: 99999999999999999999\n"},
		{"infinity", "terms: []\nn: .inf\n"},
		{"not a number", "terms: []\nn: .nan\n"},
		{"deep nesting", "terms: []\nn: " + strings.Repeat("[", 200) + strings.Repeat("]", 200) + "\n"},
		{"too many nodes", "terms: []\nn: [" + strings.Repeat("1,", 250000) + "1]\n"},
		{"file larger than the limit", "terms: []\n# " + strings.Repeat("a", 3<<20) + "\n"},
		{"expansion bomb", "a: &a [x, x, x, x, x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a]\nc: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b]\nterms: []\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := valid(t).set("terminology/bad.yaml", c.body)
			res := mustLoad(t, files.signed(t), loadOptions())
			requireCritical(t, res, "yaml.invalid")
			f, _ := find(res.Findings, "yaml.invalid")
			if f.Path != "terminology/bad.yaml" {
				t.Errorf("finding path = %q", f.Path)
			}
			if len(f.Message) > 400 {
				t.Errorf("finding message is not bounded: %d bytes", len(f.Message))
			}
		})
	}

	t.Run("a well-formed extra file is accepted", func(t *testing.T) {
		files := valid(t).set("terminology/extra.yaml", good)
		res := mustLoad(t, files.signed(t), loadOptions())
		requireCritical(t, res)
	})
	t.Run("quoted lookalikes stay strings", func(t *testing.T) {
		body := "terms:\n- id: extra\n  labels:\n    en:\n      preferred: \"2026-11-13\"\n      definition: \"yes\"\n    it:\n      preferred: \"0x1F\"\n      definition: \"no\"\n"
		res := mustLoad(t, valid(t).set("terminology/extra.yaml", body).signed(t), loadOptions())
		requireCritical(t, res)
		if got := res.Package.Terms["extra"].Labels["en"].Preferred; got != "2026-11-13" {
			t.Errorf("preferred = %q, want the quoted text", got)
		}
	})
}

// TestFindingTextIsBounded: a hostile file name or value cannot flood a
// finding, forge a second line or smuggle control characters into it.
func TestFindingTextIsBounded(t *testing.T) {
	long := strings.Repeat("a", 5000)
	for _, tc := range []struct{ name, in string }{
		{"long", long},
		{"newline", "a\nHIGH: forged finding"},
		{"escape", "a\x1b[31mred"},
		{"unicode line separator", "a b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := q(tc.in)
			if len(q) > 100 {
				t.Errorf("q is %d bytes", len(q))
			}
			if strings.ContainsAny(q, "\n\r\x1b") {
				t.Errorf("q kept a control character: %q", q)
			}
			p := clipPath(tc.in)
			if len(p) > 130 || strings.ContainsAny(p, "\n\r\x1b") {
				t.Errorf("clipPath = %q", p)
			}
		})
	}
}
