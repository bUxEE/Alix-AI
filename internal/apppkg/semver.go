package apppkg

import (
	"regexp"
	"strings"
)

// semverRE is defs.schema.json semver: semantic version 2.0.0 with optional
// pre-release and build metadata.
var semverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// semver is a parsed semantic version. Build metadata is dropped: it takes no
// part in precedence.
type semver struct {
	major, minor, patch string
	pre                 []string
}

func parseSemver(s string) (semver, bool) {
	if len(s) > 128 {
		return semver{}, false
	}
	m := semverRE.FindStringSubmatch(s)
	if m == nil {
		return semver{}, false
	}
	v := semver{major: m[1], minor: m[2], patch: m[3]}
	if m[5] != "" {
		v.pre = strings.Split(m[5], ".")
	}
	return v, true
}

// compareSemver orders two versions by SemVer 2.0.0 precedence: -1, 0 or 1.
func compareSemver(a, b semver) int {
	for _, p := range [][2]string{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if c := compareNumeric(p[0], p[1]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1 // a release is higher than any of its pre-releases
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		xn, yn := isNumeric(x), isNumeric(y)
		switch {
		case xn && yn:
			if c := compareNumeric(x, y); c != 0 {
				return c
			}
		case xn:
			return -1 // numeric identifiers are lower than alphanumeric ones
		case yn:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

// compareNumeric compares two non-negative decimal strings without leading
// zeros, of any length.
func compareNumeric(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// CoreVersionOrDev returns v when it is a semantic version and DevCoreVersion
// otherwise: a development build of the binary carries no release version, and
// the loader must still be given a core version to check packages against.
func CoreVersionOrDev(v string) string {
	if _, ok := parseSemver(v); ok {
		return v
	}
	return DevCoreVersion
}
