package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Kind values of the backlog's last column. A kind may combine them with a
// plus sign: `automated+pentest`, `automated+human`.
const (
	kindAutomated = "automated"
	kindPentest   = "pentest"
	kindHuman     = "human"
)

// entry is one `package-dir: Name, Name` item of a Test(s) cell, or a bare
// evidence-file reference.
type entry struct {
	Dir      string   // package directory, or evidence file when Evidence is true
	Names    []string // Test*/Fuzz* names listed for the directory
	Evidence bool     // the entry names a report, not a Go package
}

// row is one SEC-NNN definition parsed from section 4 of the backlog.
type row struct {
	ID    string
	Sev   string
	Kind  string
	Owner string
	Tests []entry
}

// needsAutomated reports whether the row's kind includes automated tests.
func (r row) needsAutomated() bool { return strings.Contains(r.Kind, kindAutomated) }

// needsEvidence reports whether the row's kind includes a pentest or human part.
func (r row) needsEvidence() bool {
	return strings.Contains(r.Kind, kindPentest) || strings.Contains(r.Kind, kindHuman)
}

var (
	rowPattern  = regexp.MustCompile(`^\| (SEC-\d{3}) \|`)
	namePattern = regexp.MustCompile(`^((?:Test|Fuzz)[A-Za-z0-9_]*)\b`)
	backtick    = regexp.MustCompile("`([^`]+)`")
)

// columnCount is the fixed width of a section 4 row: ID, Threat, Ref,
// Component, Required test, Test(s), Owner, Sev, Kind.
const columnCount = 9

// backlog is the parsed file: the rows of section 4 and the evidence reports
// of section 6.2.
type backlog struct {
	Rows     []row
	Evidence map[string][]string // SEC id -> evidence files named in section 6.2
}

// parseBacklog reads the backlog file.
func parseBacklog(path string) (*backlog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseBacklogText(string(raw))
}

// parseBacklogText reads every `| SEC-NNN |` row and the section 6.2 evidence
// table. It fails on a row whose column count is wrong, on a duplicate id and on
// an empty result, so a reshaped file can never read as "nothing to check".
func parseBacklogText(text string) (*backlog, error) {
	b := &backlog{Evidence: map[string][]string{}}
	seen := map[string]bool{}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	inEvidence := false
	for n, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "### 6.2"):
			inEvidence = true
			continue
		case strings.HasPrefix(line, "#"):
			inEvidence = false
		}
		if inEvidence && strings.HasPrefix(line, "|") {
			cells := splitCells(line)
			if len(cells) >= 3 {
				files := backtick.FindAllStringSubmatch(cells[2], -1)
				for _, id := range idPattern.FindAllString(cells[1], -1) {
					for _, f := range files {
						b.Evidence[id] = append(b.Evidence[id], f[1])
					}
				}
			}
			continue
		}
		m := rowPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cells := splitCells(line)
		if len(cells) != columnCount {
			return nil, fmt.Errorf("backlog line %d (%s): %d columns, want %d", n+1, m[1], len(cells), columnCount)
		}
		if seen[m[1]] {
			return nil, fmt.Errorf("backlog line %d: %s is defined twice", n+1, m[1])
		}
		seen[m[1]] = true
		b.Rows = append(b.Rows, row{
			ID:    m[1],
			Owner: cells[6],
			Sev:   cells[7],
			Kind:  cells[8],
			Tests: parseTests(cells[5]),
		})
	}
	if len(b.Rows) == 0 {
		return nil, fmt.Errorf("no `| SEC-NNN |` rows found: the backlog format changed")
	}
	return b, nil
}

// splitCells splits a Markdown table row on its pipes, without the outer ones.
// A pipe inside backticks does not split.
func splitCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	var b strings.Builder
	inCode := false
	for _, r := range line {
		switch {
		case r == '`':
			inCode = !inCode
			b.WriteRune(r)
		case r == '|' && !inCode:
			cells = append(cells, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	return append(cells, strings.TrimSpace(b.String()))
}

// parseTests reads a Test(s) cell: entries separated by `;`, each
// `dir: Name, Name (new, see gap G1)` or a bare `file.md (human)`. Commas and
// semicolons inside parentheses do not split.
func parseTests(cell string) []entry {
	var out []entry
	for _, part := range splitTop(cell, ';') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		dir, rest, hasColon := strings.Cut(part, ":")
		dir = strings.TrimSpace(dir)
		if !hasColon || strings.Contains(dir, " ") {
			// `docs/evidence/x.md (human)` has no colon.
			first := strings.Fields(part)
			if len(first) > 0 && strings.HasSuffix(first[0], ".md") {
				out = append(out, entry{Dir: first[0], Evidence: true})
			}
			continue
		}
		if strings.HasSuffix(dir, ".md") {
			out = append(out, entry{Dir: dir, Evidence: true})
			continue
		}
		e := entry{Dir: dir}
		for _, tok := range splitTop(rest, ',') {
			tok = strings.TrimSpace(tok)
			if m := namePattern.FindStringSubmatch(tok); m != nil {
				e.Names = append(e.Names, m[1])
			}
		}
		out = append(out, e)
	}
	return out
}

// splitTop splits on sep outside parentheses.
func splitTop(s string, sep rune) []string {
	var parts []string
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '(':
			depth++
			b.WriteRune(r)
		case r == ')':
			if depth > 0 {
				depth--
			}
			b.WriteRune(r)
		case r == sep && depth == 0:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	return append(parts, b.String())
}
