package source

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func mustMatcher(t *testing.T, include, exclude []string, fold bool) *Matcher {
	t.Helper()
	m, err := NewMatcher(include, exclude, fold)
	if err != nil {
		t.Fatalf("NewMatcher(%q, %q): %v", include, exclude, err)
	}
	return m
}

func TestGlobSemantics(t *testing.T) {
	// Patterns from the example packages (schemas/application-package/examples).
	m := mustMatcher(t,
		[]string{"current/**/*.pdf", "current/**/*.md", "*.txt", "deep/**"},
		[]string{"**/draft-*", "**/private/**"},
		false)

	cases := []struct {
		key          string
		included     bool
		excluded     bool
		wantEligible bool
	}{
		{"current/a.pdf", true, false, true},
		{"current/x/y/a.pdf", true, false, true},
		{"current/a.md", true, false, true},
		{"current/a.docx", false, false, false},
		{"other/a.pdf", false, false, false},
		{"a.pdf", false, false, false},
		{"notes.txt", true, false, true},
		{"sub/notes.txt", false, false, false}, // "*" is one segment
		{"deep/a", true, false, true},
		{"deep/x/y/z.bin", true, false, true},
		{"deep", true, false, true}, // "**" may match zero segments, so the pattern matches the name "deep"
		{"current/draft-x.pdf", true, true, false},
		{"current/sub/draft-y.pdf", true, true, false},
		{"draft-top.txt", true, true, false},
		{"current/draft-dir/a.pdf", true, true, false}, // an excluded directory excludes what is under it
		{"current/private/a.pdf", true, true, false},
		{"current/a/private/b/c/d.pdf", true, true, false},
		{"current/privates/a.pdf", true, false, true},
		{"current/x-draft-y.pdf", true, false, true},
	}
	for _, c := range cases {
		if got := m.Included(c.key); got != c.included {
			t.Errorf("Included(%q) = %v, want %v", c.key, got, c.included)
		}
		if got := m.Excluded(c.key); got != c.excluded {
			t.Errorf("Excluded(%q) = %v, want %v", c.key, got, c.excluded)
		}
		if got := m.Eligible(c.key); got != c.wantEligible {
			t.Errorf("Eligible(%q) = %v, want %v", c.key, got, c.wantEligible)
		}
	}
}

func TestGlobIncludeNamesMustMatchTheItself(t *testing.T) {
	m := mustMatcher(t, []string{"docs"}, nil, false)
	if m.Included("docs/a.md") {
		t.Error(`include "docs" must not include docs/a.md; only "docs/**" does`)
	}
	if !m.Included("docs") {
		t.Error(`include "docs" should match the name docs`)
	}
	// ...whereas an exclude naming a directory covers what is under it.
	m = mustMatcher(t, []string{"**"}, []string{"secret"}, false)
	if m.Eligible("secret/a.md") {
		t.Error(`exclude "secret" must remove secret/a.md`)
	}
	if !m.Eligible("x/secret/a.md") {
		t.Error(`exclude "secret" is anchored at the root; x/secret/a.md is not covered (use "**/secret")`)
	}
}

func TestGlobTrailingSlashAndClasses(t *testing.T) {
	m := mustMatcher(t, []string{"docs/**", "a[0-9]?.md", "*.PDF"}, []string{"tmp/"}, false)
	if !m.Eligible("docs/a.md") || !m.Eligible("a1x.md") || m.Eligible("aax.md") || m.Eligible("x.pdf") || !m.Eligible("x.PDF") {
		t.Error("class, case or ? handling is wrong")
	}
	m = mustMatcher(t, []string{"**"}, []string{"tmp/"}, false)
	if m.Eligible("tmp/a.md") {
		t.Error(`a trailing slash is ignored: "tmp/" excludes tmp and what is under it`)
	}
}

func TestGlobFoldCase(t *testing.T) {
	sensitive := mustMatcher(t, []string{"**"}, []string{"private/**"}, false)
	folded := mustMatcher(t, []string{"**"}, []string{"private/**"}, true)
	if !sensitive.Eligible("Private/a.md") {
		t.Error("a case-sensitive matcher keeps Private/a.md")
	}
	if folded.Eligible("Private/a.md") || folded.Eligible("PRIVATE/A.md") {
		t.Error("a folding matcher must exclude Private/a.md: a case-insensitive filesystem would otherwise bypass the rule")
	}
	inc := mustMatcher(t, []string{"Docs/**/*.MD"}, nil, true)
	if !inc.Eligible("docs/x/a.md") || !inc.Eligible("DOCS/X/A.Md") {
		t.Error("a folding include should match any case")
	}
}

func TestPruneDir(t *testing.T) {
	m := mustMatcher(t, []string{"current/**/*.pdf", "notes/*.md", "**/keep/*.txt"}, []string{"**/private/**", "build"}, false)
	cases := []struct {
		dir  string
		want bool // pruned
	}{
		{"", false},
		{"current", false},
		{"current/a/b", false},
		{"current/private", true},
		{"current/a/private", true},
		{"notes", false},
		{"notes/sub", false}, // "**/keep/*.txt" can still reach below any directory
		{"archive", false},
		{"build", true},
		{"build/out", true},
		{"x/keep", false},
	}
	for _, c := range cases {
		if got := m.PruneDir(c.dir); got != c.want {
			t.Errorf("PruneDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
	direct := mustMatcher(t, []string{"notes/*.md"}, nil, false)
	if !direct.PruneDir("notes/sub") || direct.PruneDir("notes") {
		t.Error("notes/*.md reaches only the direct children of notes")
	}
	narrow := mustMatcher(t, []string{"docs/**/*.md"}, nil, false)
	for _, dir := range []string{"archive", "docs2", "x/docs"} {
		if !narrow.PruneDir(dir) {
			t.Errorf("PruneDir(%q) should prune: no include pattern can reach below it", dir)
		}
	}
	for _, dir := range []string{"docs", "docs/a", "docs/a/b"} {
		if narrow.PruneDir(dir) {
			t.Errorf("PruneDir(%q) pruned a directory the include reaches", dir)
		}
	}
	// PruneDir never says "keep" for a directory that cannot hold an eligible
	// file: agree with Eligible on a sample.
	for _, dir := range []string{"docs", "docs/a", "archive"} {
		hasEligible := narrow.Eligible(dir+"/x.md") || narrow.Eligible(dir+"/y/z.md")
		if narrow.PruneDir(dir) && hasEligible {
			t.Errorf("PruneDir(%q) pruned a directory that holds eligible files", dir)
		}
	}
}

func TestNewMatcherRejectsBadRules(t *testing.T) {
	if _, err := NewMatcher(nil, nil, false); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("no include rule: %v", err)
	}
	bad := []string{
		"", "/", "/abs/*.md", `a\b`, "a/../b", "../a", "a/./b", "a//b", "a[", "[a-", "a" + string(rune(1)), "\xff", strings.Repeat("a", maxPatternBytes+1),
	}
	for _, p := range bad {
		if _, err := NewMatcher([]string{p}, nil, false); err == nil {
			t.Errorf("include %q accepted", p)
		}
		if _, err := NewMatcher([]string{"**"}, []string{p}, false); err == nil {
			t.Errorf("exclude %q accepted", p)
		}
	}
}

// A hostile pattern must not take exponential time.
func TestGlobCannotGoExponential(t *testing.T) {
	pat := strings.Repeat("**/a/", 30) + "z"
	m := mustMatcher(t, []string{pat}, nil, false)
	name := strings.Repeat("a/", 60) + "q"
	done := make(chan bool, 1)
	go func() { done <- m.Included(name) }()
	select {
	case got := <-done:
		if got {
			t.Error("matched")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("matching took too long")
	}
}

func TestMatcherEmptyKey(t *testing.T) {
	m := mustMatcher(t, []string{"**"}, nil, false)
	if m.Included("") || m.Eligible("") || m.PruneDir("") {
		t.Error("the root itself is never an item and is never pruned")
	}
	var ce *contracts.Error
	if _, err := NewMatcher([]string{"a["}, nil, false); !errors.As(err, &ce) || ce.Code != contracts.ErrValidationFailed {
		t.Errorf("a malformed class is a validation error: %v", err)
	}
}
