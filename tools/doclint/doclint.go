// doclint is the mechanical guard for the decision-layer documentation consistency rules of the owner's
// CLM-8B integration prompt (ADR 0029). The rules are table-driven: a new rule is one entry in Rules.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Finding is one rule violation.
type Finding struct {
	Path string
	Line int
	Rule string
	Msg  string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: [%s] %s", f.Path, f.Line, f.Rule, f.Msg) }

// Para is a block of text: a prose paragraph, one list item, a heading or a single table row.
type Para struct {
	Line  int
	Text  string
	Table bool
}

// Doc is a parsed markdown document. Path is slash-separated and relative to the repository root.
type Doc struct {
	Path  string
	Text  string
	Paras []Para
}

// Rule is one consistency check.
type Rule struct {
	ID    string
	Title string
	Check func(d Doc) []Finding
}

// Rules is the table of checks; add a rule by adding one line.
var Rules = []Rule{
	{"JEV", "the token JEV only in ADR 0029's historical note, never as a required component", checkJEV},
	{"CLM-QUALIFIERS", "a document that describes CLM states optional, bounded, non-generative and non-authoritative", checkQualifiers},
	{"CLM-CLAIM", "a CLM performance or safety claim is a vendor claim, a hypothesis, or conditioned by only if / until", checkClaims},
	{"CLM-DEFAULT", "no sentence makes CLM-8B an unconditional default", checkDefault},
}

// Tunables of the rules above, kept in one place.
var (
	jevRe       = regexp.MustCompile(`(?i)\bjev\b`)
	clmRe       = regexp.MustCompile(`\bCLM\b`)
	adrHistory  = "never required"
	adrFile     = "0029-decision-layer-clm-8b.md"
	describesAt = 3 // a document with fewer CLM paragraphs only names CLM (a table cell, a pointer) and is not describing it
	qualifiers  = []struct {
		Name string
		Re   *regexp.Regexp
	}{
		{"optional", regexp.MustCompile(`(?i)\boptional\b`)},
		{"bounded", regexp.MustCompile(`(?i)\bbounded\b`)},
		{"non-generative", regexp.MustCompile(`(?i)\bnon-?\s?generative\b`)},
		{"non-authoritative", regexp.MustCompile(`(?i)\bnon-?\s?authoritative\b`)},
	}
	claimRe   = regexp.MustCompile(`(?i)\b(faster|lower latency|safer|better|outperform\w*|sota)\b`)
	labelRe   = regexp.MustCompile(`(?i)\bvendor\b|\bhypothes[ie]s\b|\bonly if\b|\buntil\b`)
	defaultRe = regexp.MustCompile(`(?i)\b(by default|the default|default[- ]on|always on|always enabled|always active)\b`)
	// a sentence carrying one of these conditions or negations is not an unconditional default
	conditionRe = regexp.MustCompile(`(?i)\b(only if|unless|until|if|not|never|no|off|disabled|without|gate|before|once|except|cannot|nor|rejects?|rejected)\b`)
	listRe      = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)
	sentenceRe  = regexp.MustCompile(`[.!?]\s+`)
)

// Parse splits markdown into paragraphs, list items, headings and table rows.
func Parse(p, text string) Doc {
	d := Doc{Path: filepath.ToSlash(p), Text: text}
	var cur []string
	start := 0
	flush := func() {
		if len(cur) > 0 {
			d.Paras = append(d.Paras, Para{Line: start, Text: strings.Join(cur, " ")})
			cur = nil
		}
	}
	for i, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		n := i + 1
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "|"):
			flush()
			d.Paras = append(d.Paras, Para{Line: n, Text: line, Table: true})
		case strings.HasPrefix(line, "#"):
			flush()
			d.Paras = append(d.Paras, Para{Line: n, Text: line})
		case listRe.MatchString(raw):
			flush()
			cur, start = []string{line}, n
		default:
			if len(cur) == 0 {
				start = n
			}
			cur = append(cur, line)
		}
	}
	flush()
	return d
}

func sentences(s string) []string { return sentenceRe.Split(s, -1) }

func isADR(d Doc) bool { return path.Base(d.Path) == adrFile }

func checkJEV(d Doc) []Finding {
	var out []Finding
	for _, p := range d.Paras {
		if !jevRe.MatchString(p.Text) || (isADR(d) && p.Table) {
			continue // the ADR facts table quotes the vendor's own comparison text
		}
		for _, s := range sentences(p.Text) {
			if !jevRe.MatchString(s) || (isADR(d) && strings.Contains(strings.ToLower(s), adrHistory)) {
				continue
			}
			out = append(out, Finding{d.Path, p.Line, "JEV", "JEV may appear only in the historical note of ADR 0029 (it is never a required component)"})
			break
		}
	}
	return out
}

func clmParas(d Doc) []Para {
	var out []Para
	for _, p := range d.Paras {
		if clmRe.MatchString(p.Text) {
			out = append(out, p)
		}
	}
	return out
}

func checkQualifiers(d Doc) []Finding {
	ps := clmParas(d)
	if len(ps) < describesAt {
		return nil
	}
	var missing []string
	for _, q := range qualifiers {
		if !q.Re.MatchString(d.Text) {
			missing = append(missing, q.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Finding{{d.Path, ps[0].Line, "CLM-QUALIFIERS", "mentions CLM but never states: " + strings.Join(missing, ", ")}}
}

func checkClaims(d Doc) []Finding {
	var out []Finding
	for _, p := range clmParas(d) {
		if isADR(d) && p.Table {
			continue // quoted vendor benchmark text
		}
		if m := claimRe.FindString(p.Text); m != "" && !labelRe.MatchString(p.Text) {
			out = append(out, Finding{d.Path, p.Line, "CLM-CLAIM", fmt.Sprintf("%q claim about CLM is not labelled a vendor claim or hypothesis, nor conditioned with only if / until", m)})
		}
	}
	return out
}

func checkDefault(d Doc) []Finding {
	var out []Finding
	for _, p := range clmParas(d) {
		for _, s := range sentences(p.Text) {
			if clmRe.MatchString(s) && defaultRe.MatchString(s) && !conditionRe.MatchString(s) {
				out = append(out, Finding{d.Path, p.Line, "CLM-DEFAULT", "sentence makes CLM an unconditional default"})
				break
			}
		}
	}
	return out
}

// Lint runs every rule over one document.
func Lint(p, text string) []Finding {
	d := Parse(p, text)
	var out []Finding
	for _, r := range Rules {
		out = append(out, r.Check(d)...)
	}
	return out
}

// exempt reports whether a root-relative path is one of the owner's prompt files (root-level Alix_*Prompt.md).
func exempt(rel string) bool {
	ok, _ := path.Match("Alix_*Prompt.md", rel)
	return ok
}

// LintPaths lints every .md file under the given root-relative files or directories.
func LintPaths(root string, args []string) ([]Finding, error) {
	var files []string
	for _, a := range args {
		abs := filepath.Join(root, a)
		st, err := os.Stat(abs)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			files = append(files, filepath.ToSlash(filepath.Clean(a)))
			continue
		}
		err = filepath.WalkDir(abs, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() && (e.Name() == "node_modules" || e.Name() == ".git" || e.Name() == ".worktrees") {
				return filepath.SkipDir
			}
			if !e.IsDir() && strings.HasSuffix(p, ".md") {
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	var out []Finding
	for i, f := range files {
		if (i > 0 && files[i-1] == f) || exempt(f) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			return nil, err
		}
		out = append(out, Lint(f, string(b))...)
	}
	return out, nil
}
