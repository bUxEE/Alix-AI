package localai

import (
	"regexp"
	"strings"
)

// RuntimeVersion is what LocalAI's /version route reported.
type RuntimeVersion struct {
	// Version is the release token ("v4.10.0"), empty when the runtime
	// reported none (a source build) or something that is not a version.
	Version string
	// Commit is the source revision, empty when absent or malformed.
	Commit string
}

var (
	versionTokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	commitRE       = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

// sanitizeVersion returns s when it has the shape of a version token and ""
// otherwise. A server controls this text; it must never reach a report or a
// log line in any other shape.
func sanitizeVersion(s string) string {
	if versionTokenRE.MatchString(s) {
		return s
	}
	return ""
}

// ParseVersion splits LocalAI's version string, which has the form
// "<version> (<commit>)" (internal.PrintableVersion in v4.10.0; a source build
// has an empty version and reports " (<commit>)"). Anything that does not
// have the expected shape is dropped rather than passed on.
func ParseVersion(raw string) RuntimeVersion {
	raw = strings.TrimSpace(raw)
	var rv RuntimeVersion
	head, rest, hasCommit := strings.Cut(raw, "(")
	rv.Version = sanitizeVersion(strings.TrimSpace(head))
	if hasCommit {
		commit, _, _ := strings.Cut(rest, ")")
		if c := strings.TrimSpace(commit); commitRE.MatchString(c) {
			rv.Commit = c
		}
	}
	return rv
}

// normalizeVersion folds the cosmetic parts of a version for comparison with
// the qualified list: case and a leading "v".
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.ToLower(v), "v")
}

func (c Config) isQualified(version string) bool {
	if version == "" {
		return false
	}
	for _, q := range c.QualifiedVersions {
		if normalizeVersion(q) == normalizeVersion(version) {
			return true
		}
	}
	return false
}
