package source

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxPatternBytes bounds one glob. The package schema allows 256; the bound
// here only protects callers that build a Matcher from something else.
const maxPatternBytes = 1024

// Matcher applies the include and exclude globs of a source declaration to
// root-relative, slash-separated item keys.
//
// Pattern syntax is the one the package schema promises: a pattern is a list
// of segments separated by "/", matched against the segments of the key. Within
// a segment "*" matches any run of characters, "?" one character and [a-z] a
// class (path.Match). A segment that is exactly "**" matches zero or more whole
// segments, so "current/**/*.pdf" matches "current/a.pdf" and "**/draft-*"
// matches "draft-x" at the root. "*" also matches names that start with a dot:
// hidden files are not special. A trailing "/" is ignored.
//
// The rules fail closed:
//
//   - An item must match an include pattern itself. A pattern that names a
//     directory does not include its contents ("docs" does not include
//     "docs/a.md"; "docs/**" does).
//   - An exclude pattern that matches the item or any directory above it
//     removes the item, and exclusion always wins. "private" therefore
//     excludes everything under every directory named "private" that the
//     pattern reaches.
//   - With fold set (a case-insensitive filesystem) both sides are compared
//     case-insensitively, so a directory called "Private" cannot slip past the
//     exclude "private/**".
type Matcher struct {
	include []globPattern
	exclude []globPattern
	fold    bool
}

type globPattern struct {
	raw  string
	segs []string
}

// NewMatcher compiles the rules. It needs at least one include (a source with
// no include rule matches nothing, which is not a declaration) and rejects a
// pattern that is empty, absolute, contains a backslash, a "." or ".." segment
// or an empty interior segment, or has a malformed class such as "[a-".
func NewMatcher(include, exclude []string, foldCase bool) (*Matcher, error) {
	if len(include) == 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "a source needs at least one include rule")
	}
	m := &Matcher{fold: foldCase}
	var err error
	if m.include, err = compileGlobs(include, foldCase); err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	if m.exclude, err = compileGlobs(exclude, foldCase); err != nil {
		return nil, fmt.Errorf("exclude: %w", err)
	}
	return m, nil
}

func compileGlobs(patterns []string, fold bool) ([]globPattern, error) {
	out := make([]globPattern, 0, len(patterns))
	for _, raw := range patterns {
		g, err := compileGlob(raw, fold)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func compileGlob(raw string, fold bool) (globPattern, error) {
	bad := func(why string) (globPattern, error) {
		return globPattern{}, contracts.Newf(contracts.ErrValidationFailed, "glob %q: %s", raw, why)
	}
	p := strings.TrimSuffix(raw, "/")
	switch {
	case p == "":
		return bad("empty")
	case len(raw) > maxPatternBytes:
		return bad("too long")
	case !utf8.ValidString(raw):
		return bad("not UTF-8")
	case strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return bad("control character")
	case strings.ContainsRune(raw, '\\'):
		return bad("backslash")
	case p[0] == '/':
		return bad("absolute")
	}
	if fold {
		p = strings.ToLower(p)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch s {
		case "":
			return bad("empty segment")
		case ".", "..":
			return bad(`"." and ".." segments are not allowed`)
		}
		if _, err := path.Match(s, "x"); err != nil {
			return bad("malformed segment " + fmt.Sprintf("%q", s))
		}
	}
	return globPattern{raw: raw, segs: segs}, nil
}

func (m *Matcher) split(rel string) []string {
	if rel == "" || rel == "." {
		return nil
	}
	if m.fold {
		rel = strings.ToLower(rel)
	}
	return strings.Split(rel, "/")
}

// Included reports whether the item itself matches an include pattern.
func (m *Matcher) Included(rel string) bool {
	n := m.split(rel)
	if len(n) == 0 {
		return false
	}
	for _, g := range m.include {
		if matchSegments(g.segs, n) {
			return true
		}
	}
	return false
}

// Excluded reports whether an exclude pattern matches the item or any
// directory above it.
func (m *Matcher) Excluded(rel string) bool {
	n := m.split(rel)
	for i := 1; i <= len(n); i++ {
		for _, g := range m.exclude {
			if matchSegments(g.segs, n[:i]) {
				return true
			}
		}
	}
	return false
}

// Eligible reports whether the rules admit the item: included and not
// excluded.
func (m *Matcher) Eligible(rel string) bool {
	return m.Included(rel) && !m.Excluded(rel)
}

// PruneDir reports whether nothing below the directory can be eligible, so a
// walk need not enter it: the directory is excluded, or no include pattern
// can still match a path that starts with it. It never prunes the root.
func (m *Matcher) PruneDir(rel string) bool {
	n := m.split(rel)
	if len(n) == 0 {
		return false
	}
	if m.Excluded(rel) {
		return true
	}
	for _, g := range m.include {
		if canReach(g.segs, n) {
			return false
		}
	}
	return true
}

// matchSegments matches a whole key against a whole pattern. "**" is the only
// token that spans segments; the others match exactly one. The greedy
// backtrack-to-the-last-"**" form is linear in practice and cannot go
// exponential on a hostile pattern.
func matchSegments(p, n []string) bool {
	pi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && p[pi] == "**":
			star, mark = pi, ni
			pi++
		case pi < len(p) && segMatch(p[pi], n[ni]):
			pi++
			ni++
		case star >= 0:
			mark++
			ni, pi = mark, star+1
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == "**" {
		pi++
	}
	return pi == len(p)
}

func segMatch(pat, name string) bool {
	ok, err := path.Match(pat, name)
	return err == nil && ok
}

// canReach reports whether some key that starts with the directory segments n
// and has at least one more segment can match the pattern: the directory's
// segments are consumed by the pattern's head, and something is still left
// for a file name.
func canReach(p, n []string) bool {
	states := map[int]bool{}
	closure := func(i int) {
		for {
			states[i] = true
			if i < len(p) && p[i] == "**" {
				i++
				continue
			}
			return
		}
	}
	closure(0)
	for _, seg := range n {
		next := map[int]bool{}
		for i := range states {
			if i >= len(p) {
				continue
			}
			if p[i] == "**" {
				next[i] = true
				continue
			}
			if segMatch(p[i], seg) {
				next[i+1] = true
			}
		}
		states = map[int]bool{}
		for i := range next {
			closure(i)
		}
		if len(states) == 0 {
			return false
		}
	}
	for i := range states {
		if i < len(p) {
			return true
		}
	}
	return false
}
