package localaisec

import (
	"sort"
	"strings"
)

// requiredValue stands in for a value that a required substitution (${VAR:?message}) leaves to the operator: the
// file cannot say what it will be, only that startup fails without one.
const requiredValue = "\x00required\x00"

// expand returns every value s can take after Compose-style variable substitution, given the values an operator
// gets by copying the .env.example next to the file (example). The rules judge each of them: a default that
// weakens a rule counts as the weakened value, and so does a value the example file sets.
//
//	${V}        the example's value, or empty
//	${V:-d}     d, and the example's value when it is non-empty
//	${V-d}      d, and the example's value when it is set
//	${V:?msg}   requiredValue (the operator supplies it)
//	${V:+x}     x, and empty
//	$$          a literal dollar sign
func expand(s string, example map[string]string) []string {
	out := []string{""}
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			j := strings.IndexByte(s[i:], '$')
			if j < 0 {
				j = len(s) - i
			}
			out = appendAll(out, []string{s[i : i+j]})
			i += j
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			out = appendAll(out, []string{"$"})
			i += 2
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := matchBrace(s, i+1)
			if end < 0 {
				out = appendAll(out, []string{s[i:]})
				break
			}
			out = appendAll(out, substitute(s[i+2:end], example))
			i = end + 1
			continue
		}
		j := i + 1
		for j < len(s) && (s[j] == '_' || (s[j] >= '0' && s[j] <= '9') || (s[j] >= 'A' && s[j] <= 'Z') || (s[j] >= 'a' && s[j] <= 'z')) {
			j++
		}
		if j == i+1 {
			out = appendAll(out, []string{"$"})
			i++
			continue
		}
		out = appendAll(out, []string{example[s[i+1:j]]})
		i = j
	}
	return dedupe(out)
}

// matchBrace returns the index of the brace closing the one at open, or -1.
func matchBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// substitute resolves the inside of one ${...}.
func substitute(body string, example map[string]string) []string {
	i := 0
	for i < len(body) && (body[i] == '_' || (body[i] >= '0' && body[i] <= '9') || (body[i] >= 'A' && body[i] <= 'Z') || (body[i] >= 'a' && body[i] <= 'z')) {
		i++
	}
	name, rest := body[:i], body[i:]
	op, arg := "", ""
	for _, cand := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, cand) {
			op, arg = cand, rest[len(cand):]
			break
		}
	}
	exVal, exSet := example[name]
	switch op {
	case "":
		return []string{exVal}
	case ":-":
		out := expand(arg, example)
		if exSet && exVal != "" {
			out = append(out, exVal)
		}
		return dedupe(out)
	case "-":
		out := expand(arg, example)
		if exSet {
			out = append(out, exVal)
		}
		return dedupe(out)
	case ":?", "?":
		return []string{requiredValue}
	case ":+", "+":
		return dedupe(append([]string{""}, expand(arg, example)...))
	}
	return []string{exVal}
}

// appendAll is the product of the alternatives so far with the ones for the next piece, kept small: a value with
// that many substitutions is not a setting.
func appendAll(prefixes, next []string) []string {
	if len(prefixes)*len(next) > 64 {
		next = next[:1]
	}
	out := make([]string, 0, len(prefixes)*len(next))
	for _, p := range prefixes {
		for _, n := range next {
			if p == requiredValue || n == requiredValue {
				out = append(out, requiredValue)
				continue
			}
			out = append(out, p+n)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// parseEnvLines reads env-file lines: KEY=VALUE, an optional leading "export", one layer of quotes around the value.
// Comments and blank lines are skipped.
func parseEnvLines(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

// splitWords splits a command line the way a unit file or a shell would for the cases a deployment file uses: spaces
// separate words, single and double quotes group.
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	has := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote, has = c, true
		case c == ' ' || c == '\t':
			if has || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteByte(c)
		}
	}
	if has || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
