package rag

import (
	"encoding/json"
	"strings"
)

// jsonOf renders v as JSON text.
func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// witnesses returns names a glob pattern of a source declaration could match, so that the
// audit can ask the core's own matcher whether a declared include rule admits anything and
// whether the declared exclude rules leave it standing. A segment "**" stands for one
// segment in the first witness and for none in the second; "*" and "?" stand for "x"; a
// character class stands for its first character. The result is a set of candidates, not a
// promise: the caller keeps only the ones the matcher itself accepts.
func witnesses(pattern string) []string {
	segs := strings.Split(strings.TrimSuffix(pattern, "/"), "/")
	build := func(star string) string {
		var out []string
		for _, s := range segs {
			if s == "**" {
				if star != "" {
					out = append(out, star)
				}
				continue
			}
			out = append(out, witnessSegment(s))
		}
		return strings.Join(out, "/")
	}
	a, b := build("w"), build("")
	if a == b || b == "" {
		return []string{a}
	}
	return []string{a, b}
}

func witnessSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '*', '?':
			b.WriteByte('x')
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				b.WriteByte('x')
				continue
			}
			class := s[i+1 : i+end]
			negated := strings.HasPrefix(class, "^")
			class = strings.TrimPrefix(class, "^")
			switch {
			case negated:
				// any character the class does not hold
				pick := byte('x')
				for _, c := range []byte("xyzqw0") {
					if !strings.ContainsRune(class, rune(c)) {
						pick = c
						break
					}
				}
				b.WriteByte(pick)
			case class == "" || class[0] == '\\':
				b.WriteByte('x')
			default:
				b.WriteByte(class[0])
			}
			i += end
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// containsFold reports whether s holds sub, ignoring case.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// collapse replaces every run of white space by one space and trims the ends, so that a
// preview can be compared with the original whatever the line breaks of either.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
