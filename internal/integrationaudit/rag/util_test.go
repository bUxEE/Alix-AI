package rag

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rag/source"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// TestWitnessesAreNamesTheMatcherAdmits: the audit asks the core's own matcher about names the
// declared include rules could match; every witness of a pattern the matcher accepts is a name
// it includes, and the common shapes of a declaration yield one.
func TestWitnessesAreNamesTheMatcherAdmits(t *testing.T) {
	for _, pat := range []string{
		"**/*.md", "*.md", "docs/**", "docs/**/*.pdf", "current/**/*.pdf", "**", "a/b/c.txt", "report-?.csv",
		"[a-c]*.txt", "[^x]*.txt", "**/draft-*", "faq/*/index.md",
	} {
		m, err := source.NewMatcher([]string{pat}, nil, false)
		if err != nil {
			t.Fatalf("%s: %v", pat, err)
		}
		admitted := 0
		for _, w := range witnesses(pat) {
			if m.Included(w) {
				admitted++
			}
		}
		if admitted == 0 {
			t.Errorf("no witness of %q is a name the matcher admits: %v", pat, witnesses(pat))
		}
	}
}

// TestExcludeThatSwallowsTheIncludesIsSeen: the exclusion wins in the matcher, so a witness an
// exclusion removes is not eligible.
func TestExcludeThatSwallowsTheIncludesIsSeen(t *testing.T) {
	m, err := source.NewMatcher([]string{"**/*.md"}, []string{"**"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range witnesses("**/*.md") {
		if m.Eligible(w) {
			t.Errorf("%q is eligible although every name is excluded", w)
		}
	}
	m, err = source.NewMatcher([]string{"**/*.md"}, []string{"drafts/**"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, w := range witnesses("**/*.md") {
		ok = ok || m.Eligible(w)
	}
	if !ok {
		t.Errorf("an exclusion of one directory removed every witness")
	}
}

func TestFirstSentence(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		ok               bool
	}{
		{"skips the heading", "# Title\n\nFirst one. Second one.\n", "First one.", true},
		{"a version number does not end the sentence", "Version 1.2 of the policy applies. More.", "Version 1.2 of the policy applies.", true},
		{"no full stop: the whole line", "no stop here", "no stop here", true},
		{"only a heading", "# Title\n", "", false},
		{"blank", "  \n\n", "", false},
	} {
		got, ok := firstSentence(tc.text)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: firstSentence = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	long := strings.Repeat("word ", 200)
	if got, _ := firstSentence(long); utf8.RuneCountInString(got) > maxClaimRunes {
		t.Errorf("a claim of %d runes", utf8.RuneCountInString(got))
	}
}

func TestMarkersAreDistinctAndLettersOnly(t *testing.T) {
	a, b := &labPair{tenantRef: "t1", workspaceRef: "w1"}, &labPair{tenantRef: "t1", workspaceRef: "w2"}
	seen := map[string]bool{}
	for _, m := range []string{
		markerOf("s", "docs", a), markerOf("s", "docs", b), markerOf("s", "faq", a), markerOf("other", "docs", a),
	} {
		if seen[m] {
			t.Errorf("marker %s repeated", m)
		}
		seen[m] = true
		if !generatedID.MatchString(m) {
			t.Errorf("marker %q is not what the hygiene check looks for", m)
		}
		for _, r := range m {
			if r < 'a' || r > 'z' {
				t.Errorf("marker %q holds %q: a digit would make it an identifier of the index", m, r)
			}
		}
	}
	if d := synthetic(markerOf("s", "docs", a)); !strings.Contains(d.text, d.marker) || !strings.HasPrefix(d.version, "sha256:") {
		t.Errorf("synthetic document = %+v", d)
	}
}

func TestBindingConflicts(t *testing.T) {
	fixed := bindFixed("t-1")
	for _, tc := range []struct {
		b     ap.RAGSourceScopeBinding
		bound string
		want  bool
	}{
		{bindShared, "", false}, {bindShared, "t-1", true},
		{bindRequest, "t-1", false}, {bindRequest, "", true},
		{fixed, "t-1", false}, {fixed, "t-2", true}, {fixed, "", true},
	} {
		if got := bindingConflicts(tc.b, tc.bound); got != tc.want {
			t.Errorf("bindingConflicts(%s, %q) = %v, want %v", tc.b.Mode, tc.bound, got, tc.want)
		}
	}
}
