package scan

import (
	"bytes"
	"path"
	"regexp"
	"strings"
)

// ScanLineCount is the line-counting rule evidence spans use: lines are separated by \n, a CRLF
// counts as one separator, and a final newline does not start a new line. An empty file has no
// lines, so it can never be cited. A verifier must count the same way.
func ScanLineCount(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	n := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		n++
	}
	return n
}

// scanMaxLineBytes is the longest line the detectors examine; minified bundles and data blobs
// are skipped line by line rather than fed to a pattern.
const scanMaxLineBytes = 2000

// scanLines splits a file into lines with the ScanLineCount rule; a line over the cap is empty.
func scanLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	parts := strings.Split(string(content), "\n")
	if content[len(content)-1] == '\n' {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		p = strings.TrimSuffix(p, "\r")
		if len(p) > scanMaxLineBytes {
			p = ""
		}
		parts[i] = p
	}
	return parts
}

func scanExt(p string) string { return strings.ToLower(path.Ext(p)) }

func scanBase(p string) string { return strings.ToLower(path.Base(p)) }

func scanIsDocText(p string) bool {
	switch scanExt(p) {
	case ".md", ".markdown", ".mdx", ".rst", ".txt":
		return true
	}
	return false
}

func scanIsCode(p string) bool {
	switch scanExt(p) {
	case ".ts", ".tsx", ".js", ".mjs", ".cjs", ".jsx", ".py", ".go", ".java", ".kt", ".cs", ".rb", ".php", ".rs":
		return true
	}
	return false
}

func scanIsConfig(p string) bool {
	switch scanExt(p) {
	case ".json", ".yaml", ".yml", ".toml":
		return true
	}
	return false
}

func scanIsYAML(p string) bool {
	e := scanExt(p)
	return e == ".yaml" || e == ".yml"
}

// scanInDir reports whether the path lies inside a directory called name, at any depth.
func scanInDir(p, name string) bool {
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if strings.EqualFold(seg, name) {
			return true
		}
	}
	return false
}

// scanUnder reports whether p is below the directory prefix ("" is the root).
func scanUnder(p, prefix string) bool {
	if prefix == "" {
		return true
	}
	return strings.HasPrefix(p, prefix+"/")
}

// scanJSONDepthOK checks the nesting depth of a JSON document without building it, so a
// hostile document can never make the decoder recurse deeply. It also fails on an unterminated
// string or on unbalanced brackets.
func scanJSONDepthOK(data []byte, maxDepth int) bool {
	depth := 0
	inStr, esc := false, false
	for _, c := range data {
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return !inStr && depth == 0
}

// scanIndent is the number of leading spaces; a tab line returns -1 (YAML forbids it).
func scanIndent(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	if n < len(line) && line[n] == '\t' {
		return -1
	}
	return n
}

func scanBlankOrComment(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

// scanTopKey returns the value of a top-level (column 0) YAML key and its 1-based line.
func scanTopKey(lines []string, key string) (string, int, bool) {
	prefix := key + ":"
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			v := strings.TrimSpace(strings.TrimPrefix(l, prefix))
			return strings.Trim(v, `"'`), i + 1, true
		}
	}
	return "", 0, false
}

var scanCapSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// scanSlug lower-cases and collapses separators, for matching a capability id to a file name.
func scanSlug(s string) string {
	return strings.Trim(scanCapSlugRE.ReplaceAllString(strings.ToLower(s), "-"), "-")
}
