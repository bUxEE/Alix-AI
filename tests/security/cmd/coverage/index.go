package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// defect is one call of secutil.Known(t, "SEC-NNN", "ref", "why") inside a test:
// the test asserts the correct behaviour of a control that is wrong in code the
// security suite may not edit, and skips until it is fixed.
type defect struct {
	ID   string
	Ref  string
	Why  string
	Func string // "dir: Name" of the gated test
}

// testIndex records every top-level Test*/Fuzz* function of every _test.go
// file under the repository, keyed by package directory (slash-separated,
// relative to the root), every function that carries a `// SEC-NNN` tag in its
// own doc comment, and every defect-gated test.
type testIndex struct {
	funcs   map[string]map[string]bool // dir -> function name -> present
	tagged  map[string][]string        // SEC id -> "dir: Name"
	gated   map[string]bool            // "dir: Name" -> the test calls secutil.Known
	defects map[string][]defect        // SEC id -> gated tests
	files   int
}

var tagPattern = regexp.MustCompile(`^SEC-\d{3}\b`)
var idPattern = regexp.MustCompile(`SEC-\d{3}`)

// skipDir lists directories that never hold the project's own tests.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".worktrees", "vendor", "testdata", "dist", "bin", ".venv":
		return true
	}
	return false
}

// buildIndex walks root and parses every _test.go file. It fails when it finds
// none, so a wrong root can never read as "no tests exist".
func buildIndex(root string) (*testIndex, error) {
	ix := &testIndex{
		funcs:   map[string]map[string]bool{},
		tagged:  map[string][]string{},
		gated:   map[string]bool{},
		defects: map[string][]defect{},
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		ix.files++
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		dir := filepath.ToSlash(rel)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isTestName(fn.Name.Name) {
				continue
			}
			if ix.funcs[dir] == nil {
				ix.funcs[dir] = map[string]bool{}
			}
			ix.funcs[dir][fn.Name.Name] = true
			key := dir + ": " + fn.Name.Name
			for _, id := range docTags(fn.Doc) {
				ix.tagged[id] = append(ix.tagged[id], key)
			}
			for _, df := range knownCalls(fn) {
				df.Func = key
				ix.gated[key] = true
				ix.defects[df.ID] = append(ix.defects[df.ID], df)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ix.files == 0 {
		return nil, fmt.Errorf("no _test.go files under %s", root)
	}
	return ix, nil
}

func isTestName(n string) bool {
	return strings.HasPrefix(n, "Test") || strings.HasPrefix(n, "Fuzz")
}

// docTags returns the SEC ids tagged in a doc comment: a line whose text
// starts with `SEC-NNN` (the backlog's `// SEC-034` convention); several ids on
// one line (`// SEC-034, SEC-035`) all count.
func docTags(doc *ast.CommentGroup) []string {
	if doc == nil {
		return nil
	}
	var ids []string
	for _, c := range doc.List {
		text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		if !tagPattern.MatchString(text) {
			continue
		}
		ids = append(ids, idPattern.FindAllString(text, -1)...)
	}
	return ids
}

// knownCalls finds every `secutil.Known(t, "SEC-NNN", "ref", "why")` in fn,
// including inside closures (t.Run bodies). The id must be a string literal: a
// computed id cannot be audited, so it is not recognised and the test counts as
// ordinary coverage, which the caller's `Known` review would catch.
func knownCalls(fn *ast.FuncDecl) []defect {
	if fn.Body == nil {
		return nil
	}
	var out []defect
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Known" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "secutil" || len(call.Args) < 4 {
			return true
		}
		id, ref, why := stringLit(call.Args[1]), stringLit(call.Args[2]), stringLit(call.Args[3])
		if tagPattern.MatchString(id) && len(id) == 7 {
			out = append(out, defect{ID: id, Ref: ref, Why: why})
		}
		return true
	})
	return out
}

// stringLit returns the value of a string literal, or of literals joined with +.
func stringLit(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			return ""
		}
		return s
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return stringLit(x.X) + stringLit(x.Y)
		}
	case *ast.ParenExpr:
		return stringLit(x.X)
	}
	return ""
}

func (ix *testIndex) has(dir, name string) bool { return ix.funcs[dir][name] }

// repoRoot walks up from dir to the directory holding go.mod.
func repoRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory")
		}
		dir = parent
	}
}
