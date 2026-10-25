package assist

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/readiness/scan"
)

const (
	assistKindText   = "text"
	assistKindSecret = "secret"

	assistMaxLineRunes = 400
	assistMaxSplitLine = 2000 // the scanner's line cap: a longer line reads as empty
)

// assistView is one file as the model is shown it. Lines[i] is line i+1 of the file as sent
// (redacted, sanitised, neutralised, or the withheld marker); Flagged[i] says that line was
// withheld.
type assistView struct {
	Path    string
	SHA     string
	Total   int // lines in the whole file, by the scanner's rule
	Lines   []string
	Flagged []bool
	prio    int
}

// assistFS is the only way this package opens project files. The real one is confined to the
// scanned root with os.Root; a test wraps it to record every open.
type assistFS interface {
	Lstat(name string) (fs.FileInfo, error)
	Open(name string) (io.ReadCloser, error)
	Close() error
}

type assistRootFS struct{ root *os.Root }

func (r assistRootFS) Lstat(name string) (fs.FileInfo, error) {
	return r.root.Lstat(filepath.FromSlash(name))
}

func (r assistRootFS) Open(name string) (io.ReadCloser, error) {
	return r.root.Open(filepath.FromSlash(name))
}

func (r assistRootFS) Close() error { return r.root.Close() }

// assistOpenRoot applies the scanner's own refusals (a drive root, a system directory) and
// confines every later read to the root.
func assistOpenRoot(dir string) (assistFS, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("readiness assist: no directory named")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("readiness assist: cannot resolve the directory")
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, errors.New("readiness assist: cannot resolve the directory")
	}
	if why := scan.ScanRootRefusal(resolved); why != "" {
		return nil, fmt.Errorf("readiness assist: refused: %s", why)
	}
	r, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, errors.New("readiness assist: cannot open the directory")
	}
	return assistRootFS{root: r}, nil
}

var (
	assistDocExt    = map[string]bool{".md": true, ".markdown": true, ".mdx": true, ".rst": true, ".txt": true}
	assistDocDirs   = map[string]bool{"docs": true, "doc": true, "documentation": true, "wiki": true, "handbook": true, "guides": true, "guide": true}
	assistToolsRE   = regexp.MustCompile(`^\.?(mcp|tools?)([._-][a-z0-9._-]*)?\.(json|ya?ml|md)$`)
	assistAPINameRE = regexp.MustCompile(`^(openapi|swagger)[a-z0-9._-]*\.(json|ya?ml)$`)
	assistSkipRE    = regexp.MustCompile(`^(requirements[a-z0-9._-]*\.txt|licen[sc]e[a-z0-9._-]*|changelog[a-z0-9._-]*|notice[a-z0-9._-]*|robots\.txt|.*\.min\.[a-z]+)$`)
)

// assistPriority says whether a file is worth showing the model and how early: README, then
// documentation folders, API descriptions, tool lists, schema files, then other prose. Source code
// is never selected: the model reads what the project says about itself, and the rules read the
// code.
func assistPriority(p string, apiDocs map[string]bool) (int, bool) {
	base := strings.ToLower(path.Base(p))
	ext := path.Ext(base)
	if assistSkipRE.MatchString(base) {
		return 0, false
	}
	inDocs := false
	for _, seg := range strings.Split(strings.ToLower(path.Dir(p)), "/") {
		if assistDocDirs[seg] {
			inDocs = true
		}
	}
	switch {
	case strings.HasPrefix(base, "readme") && (assistDocExt[ext] || ext == ""):
		return 0, true
	case inDocs && assistDocExt[ext]:
		return 1, true
	case apiDocs[p] || assistAPINameRE.MatchString(base):
		return 2, true
	case assistToolsRE.MatchString(base):
		return 3, true
	case strings.HasSuffix(base, ".schema.json") || ext == ".proto" || ext == ".graphql" || ext == ".gql":
		return 4, true
	case assistDocExt[ext]:
		return 5, true
	}
	return 0, false
}

// assistSplit splits file content with the verifier's own line rule, so a line number the model
// cites is the number the verifier counts: \n separates, a CRLF is one separator, a final newline
// starts no line, and a line over the scanner's cap reads as empty.
func assistSplit(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	parts := strings.Split(string(content), "\n")
	if content[len(content)-1] == '\n' {
		parts = parts[:len(parts)-1]
	}
	for i, p := range parts {
		p = strings.TrimSuffix(p, "\r")
		if len(p) > assistMaxSplitLine {
			p = ""
		}
		parts[i] = p
	}
	return parts
}

// assistSelect chooses the files to show and reads each into a view. It goes only through the
// scan's own inventory, never opens a secret-looking file (the rule is the first check, before
// any open), re-hashes every file and drops one that no longer has the scanned hash.
func (e *assistEngine) assistSelect(fsys assistFS, res *scan.ScanResult) []*assistView {
	secret := map[string]bool{}
	for _, p := range res.SecretFiles {
		secret[p] = true
	}
	apiDocs := map[string]bool{}
	for _, d := range res.Inventory.OpenAPI {
		apiDocs[d.Path] = true
	}
	type cand struct {
		f    scan.ScanFile
		prio int
	}
	var cands []cand
	for _, f := range res.Files {
		if e.rules.skipSecret(f.Path, f.Kind, secret) {
			continue
		}
		if f.Kind != assistKindText || f.SHA256 == "" || f.Size <= 0 || !assistSafePath(f.Path) {
			continue
		}
		prio, ok := assistPriority(f.Path, apiDocs)
		if !ok {
			continue
		}
		cands = append(cands, cand{f: f, prio: prio})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].prio != cands[j].prio {
			return cands[i].prio < cands[j].prio
		}
		return cands[i].f.Path < cands[j].f.Path
	})
	maxBytes := res.Limits.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	var views []*assistView
	for _, c := range cands {
		if len(views) >= e.cfg.Budget.MaxFiles {
			break
		}
		if v := e.assistRead(fsys, c.f, maxBytes); v != nil {
			v.prio = c.prio
			views = append(views, v)
		}
	}
	return views
}

// assistRead reads one recorded file for the prompt, or returns nil when it cannot be shown: a
// link on the path, a hash that no longer matches, a binary file, nothing readable.
func (e *assistEngine) assistRead(fsys assistFS, f scan.ScanFile, maxBytes int64) *assistView {
	segs := strings.Split(f.Path, "/")
	for i := range segs {
		info, err := fsys.Lstat(strings.Join(segs[:i+1], "/"))
		if err != nil || info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return nil
		}
		if i == len(segs)-1 && !info.Mode().IsRegular() {
			return nil
		}
		if i < len(segs)-1 && !info.IsDir() {
			return nil
		}
	}
	rc, err := fsys.Open(f.Path)
	if err != nil {
		return nil
	}
	defer rc.Close()
	buf, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil || int64(len(buf)) > maxBytes {
		return nil
	}
	sum := sha256.Sum256(buf)
	sha := hex.EncodeToString(sum[:])
	if sha != f.SHA256 {
		return nil
	}
	head := buf
	if len(head) > 4096 {
		head = head[:4096]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil
	}
	lines := assistSplit(buf)
	if len(lines) > e.cfg.Budget.MaxLinesPerFile {
		lines = lines[:max(e.cfg.Budget.MaxLinesPerFile, 0)]
	}
	// Redacted as one text, not line by line: a private key block is withheld from its BEGIN line through its END
	// line, body included, which a line on its own cannot tell apart from any other base64.
	masked := scan.ScanRedactLines(lines)
	v := &assistView{Path: f.Path, SHA: sha, Total: scan.ScanLineCount(buf)}
	budget := e.cfg.Budget.MaxFileBytes
	shown := false
	for i, raw := range lines {
		redacted := masked[i]
		text, hidden := assistSanitize(redacted)
		text = strings.TrimRight(text, " ")
		if utf8.RuneCountInString(text) > assistMaxLineRunes {
			text = string([]rune(text)[:assistMaxLineRunes])
		}
		flagged := e.rules.flag(text, hidden) || e.rules.redactedLine(redacted != raw)
		if flagged {
			text = assistWithheld
		} else {
			text = e.rules.neutralize(text)
			if strings.TrimSpace(text) != "" {
				shown = true
			}
		}
		if len(v.Lines) > 0 && budget-len(text)-8 < 0 {
			break
		}
		budget -= len(text) + 8
		v.Lines = append(v.Lines, text)
		v.Flagged = append(v.Flagged, flagged)
	}
	if !shown {
		return nil
	}
	return v
}

// assistBlock renders one file as a fenced, numbered block of untrusted data.
func assistBlock(v *assistView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<project_file path=%q>\n", v.Path)
	for i, l := range v.Lines {
		fmt.Fprintf(&b, "%d| %s\n", i+1, l)
	}
	b.WriteString("</project_file>")
	return b.String()
}
