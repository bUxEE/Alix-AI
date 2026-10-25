package repo

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/ids"
)

// The core mints IDs as "<prefix>_<ULID>" through internal/ids. The public
// contract for those IDs is the typedId definition in
// schemas/common/ids.schema.json, which is narrower than what internal/ids
// accepts (internal/ids allows "audit_evt" and a 32-character prefix; typedId
// allows a letter followed by 1 to 7 lower-case letters or digits). A prefix
// that internal/ids accepts but the schema rejects mints IDs that the public
// API schemas would then refuse, so the gate holds every prefix to the schema.

const (
	idsPackagePath = modulePath + "/internal/ids"
	typedIDSchema  = "urn:alixai:schema:common:ids:v1#/$defs/typedId"
)

// idSite is one place where internal/ code passes an ID prefix to internal/ids.
type idSite struct {
	Where    string   // file:line
	Expr     string   // the argument as written
	Prefixes []string // resolved constant values (several when a name is declared more than once)
	Err      error    // set when the argument is not a constant the scan can resolve
}

type constDecl struct {
	expr ast.Expr
	file *goSource
}

// idPackage is what the scan knows about one Go package of the module.
type idPackage struct {
	dir        string
	name       string
	consts     map[string][]constDecl // every const of that name, at any scope
	generators map[string]bool        // names declared as ids.Generator or set from ids.NewGenerator
}

type idIndex struct {
	pkgs map[string]*idPackage // by module-relative directory
}

// localName returns the identifier under which f imports the package at
// importPath, or "" when it does not.
func (ix *idIndex) localName(f *goSource, importPath string) string {
	for _, spec := range f.AST.Imports {
		p, _ := strconv.Unquote(spec.Path.Value)
		if p != importPath {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				return ""
			}
			return spec.Name.Name
		}
		if rest, ok := strings.CutPrefix(p, modulePath+"/"); ok {
			if pkg := ix.pkgs[rest]; pkg != nil {
				return pkg.name
			}
		}
		return path.Base(p)
	}
	return ""
}

// importedModuleDir maps the identifier name, as used in f, to the
// module-relative directory it imports, or "" for an identifier that is not
// an import of this module.
func (ix *idIndex) importedModuleDir(f *goSource, name string) string {
	for _, spec := range f.AST.Imports {
		p, _ := strconv.Unquote(spec.Path.Value)
		rest, ok := strings.CutPrefix(p, modulePath+"/")
		if !ok {
			continue
		}
		local := ""
		if spec.Name != nil {
			local = spec.Name.Name
		} else if pkg := ix.pkgs[rest]; pkg != nil {
			local = pkg.name
		} else {
			local = path.Base(rest)
		}
		if local == name {
			return rest
		}
	}
	return ""
}

// buildIDIndex indexes the constants and ID generators of the production
// (non-test) files. files must hold the whole module so a prefix declared in
// one package and used in another resolves.
func buildIDIndex(files []*goSource) *idIndex {
	ix := &idIndex{pkgs: map[string]*idPackage{}}
	for _, f := range files {
		if f.Test {
			continue
		}
		dir := path.Dir(f.Rel)
		pkg := ix.pkgs[dir]
		if pkg == nil {
			pkg = &idPackage{dir: dir, name: f.Package, consts: map[string][]constDecl{}, generators: map[string]bool{}}
			ix.pkgs[dir] = pkg
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			gd, ok := n.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				return true
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if len(vs.Values) != len(vs.Names) {
					continue // implicit repetition (iota): not a string prefix
				}
				for i, name := range vs.Names {
					pkg.consts[name.Name] = append(pkg.consts[name.Name], constDecl{expr: vs.Values[i], file: f})
				}
			}
			return true
		})
	}
	// Generators are collected in a second pass: it needs the import names.
	for _, f := range files {
		if f.Test {
			continue
		}
		ix.collectGenerators(f, ix.pkgs[path.Dir(f.Rel)])
	}
	return ix
}

// collectGenerators records the names that hold an *ids.Generator in f's
// package: struct fields, parameters and variables declared with that type,
// and names assigned from ids.NewGenerator(...). A call x.ids.New("job") in
// another file of the package is then recognised by the last name of its
// receiver.
func (ix *idIndex) collectGenerators(f *goSource, pkg *idPackage) {
	idsName := ix.localName(f, idsPackagePath)
	if idsName == "" {
		return
	}
	isGenType := func(e ast.Expr) bool {
		if star, ok := e.(*ast.StarExpr); ok {
			e = star.X
		}
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Generator" {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == idsName
	}
	isNewGen := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewGenerator" {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == idsName
	}
	nameOf := func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.Ident:
			return v.Name
		case *ast.SelectorExpr:
			return v.Sel.Name
		}
		return ""
	}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Field:
			if isGenType(v.Type) {
				for _, name := range v.Names {
					pkg.generators[name.Name] = true
				}
			}
		case *ast.KeyValueExpr:
			if isNewGen(v.Value) {
				if k := nameOf(v.Key); k != "" {
					pkg.generators[k] = true
				}
			}
		case *ast.AssignStmt:
			for i, rhs := range v.Rhs {
				if i < len(v.Lhs) && isNewGen(rhs) {
					if k := nameOf(v.Lhs[i]); k != "" {
						pkg.generators[k] = true
					}
				}
			}
		case *ast.ValueSpec:
			if v.Type != nil && isGenType(v.Type) {
				for _, name := range v.Names {
					pkg.generators[name.Name] = true
				}
			}
			for i, val := range v.Values {
				if i < len(v.Names) && isNewGen(val) {
					pkg.generators[v.Names[i].Name] = true
				}
			}
		}
		return true
	})
}

// resolve evaluates e to its string constant values. It handles string
// literals, parentheses, "+" concatenation, string(...) conversions, a
// constant of the same package and pkg.Const of another package of this
// module. Everything else (a parameter, a call, a field) is an error.
func (ix *idIndex) resolve(f *goSource, e ast.Expr, depth int) ([]string, error) {
	if depth > 8 {
		return nil, fmt.Errorf("constant chain too deep")
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return nil, fmt.Errorf("not a string literal")
		}
		s, err := strconv.Unquote(v.Value)
		return []string{s}, err
	case *ast.ParenExpr:
		return ix.resolve(f, v.X, depth+1)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return nil, fmt.Errorf("operator %s is not a constant concatenation", v.Op)
		}
		l, err := ix.resolve(f, v.X, depth+1)
		if err != nil {
			return nil, err
		}
		r, err := ix.resolve(f, v.Y, depth+1)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, a := range l {
			for _, b := range r {
				out = append(out, a+b)
			}
		}
		return out, nil
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "string" && len(v.Args) == 1 {
			return ix.resolve(f, v.Args[0], depth+1)
		}
		return nil, fmt.Errorf("a call is not a constant")
	case *ast.Ident:
		pkg := ix.pkgs[path.Dir(f.Rel)]
		return ix.resolveConst(pkg, v.Name, depth)
	case *ast.SelectorExpr:
		x, ok := v.X.(*ast.Ident)
		if !ok {
			return nil, fmt.Errorf("not a package-qualified constant")
		}
		dir := ix.importedModuleDir(f, x.Name)
		if dir == "" {
			return nil, fmt.Errorf("%s is not an import of this module", x.Name)
		}
		return ix.resolveConst(ix.pkgs[dir], v.Sel.Name, depth)
	}
	return nil, fmt.Errorf("%T is not a constant expression", e)
}

func (ix *idIndex) resolveConst(pkg *idPackage, name string, depth int) ([]string, error) {
	if pkg == nil {
		return nil, fmt.Errorf("package not found")
	}
	decls := pkg.consts[name]
	if len(decls) == 0 {
		return nil, fmt.Errorf("%s is not a string constant of %s", name, pkg.dir)
	}
	var out []string
	for _, d := range decls {
		vals, err := ix.resolve(d.file, d.expr, depth+1)
		if err != nil {
			return nil, fmt.Errorf("constant %s: %w", name, err)
		}
		out = append(out, vals...)
	}
	return out, nil
}

// scanIDPrefixes finds every call that passes an ID prefix to internal/ids in
// the production files of internal/ (not internal/ids itself, whose own tests
// deliberately use prefixes outside the schema):
//
//	ids.New(p)                 package function
//	ids.Valid(id, p)           package function, prefix is the second argument
//	<generator>.New(p)         a value holding an *ids.Generator, recognised by
//	                           the name it is declared or assigned under in the
//	                           package, or by ids.NewGenerator(...).New(p)
func scanIDPrefixes(files []*goSource) []idSite {
	ix := buildIDIndex(files)
	var sites []idSite
	for _, f := range files {
		if f.Test || !under(f.Rel, "internal/") || under(f.Rel, "internal/ids/") {
			continue
		}
		pkg := ix.pkgs[path.Dir(f.Rel)]
		idsName := ix.localName(f, idsPackagePath)
		ast.Inspect(f.AST, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			arg := -1
			switch sel.Sel.Name {
			case "Valid":
				if x, ok := sel.X.(*ast.Ident); ok && idsName != "" && x.Name == idsName {
					arg = 1
				}
			case "New":
				switch x := sel.X.(type) {
				case *ast.Ident:
					if (idsName != "" && x.Name == idsName) || pkg.generators[x.Name] {
						arg = 0
					}
				case *ast.SelectorExpr:
					if pkg.generators[x.Sel.Name] {
						arg = 0
					}
				case *ast.CallExpr:
					if inner, ok := x.Fun.(*ast.SelectorExpr); ok && inner.Sel.Name == "NewGenerator" {
						if y, ok := inner.X.(*ast.Ident); ok && idsName != "" && y.Name == idsName {
							arg = 0
						}
					}
				}
			}
			if arg < 0 {
				return true
			}
			site := idSite{Where: f.Fset.Position(call.Pos()).String()}
			if arg >= len(call.Args) {
				site.Err = fmt.Errorf("call has no prefix argument")
				sites = append(sites, site)
				return true
			}
			site.Expr = types.ExprString(call.Args[arg])
			site.Prefixes, site.Err = ix.resolve(f, call.Args[arg], 0)
			sites = append(sites, site)
			return true
		})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Where < sites[j].Where })
	return sites
}

// typedIDChecker returns the function that judges a prefix: internal/ids must
// accept it and must mint an ID that validates against typedId of the common
// ids schema, read from the repository.
func typedIDChecker(t testing.TB, root string) func(prefix string) error {
	t.Helper()
	sch := compileSchema(t, root, typedIDSchema)
	gen := ids.NewGenerator(nil)
	return func(prefix string) (err error) {
		if !ids.ValidPrefix(prefix) {
			return fmt.Errorf("internal/ids itself rejects the prefix")
		}
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("minting panicked: %v", r)
			}
		}()
		id := gen.New(prefix)
		if verr := sch.Validate(id); verr != nil {
			return fmt.Errorf("minted ID %q does not match typedId in schemas/common/ids.schema.json: %v", id, verr)
		}
		return nil
	}
}

var (
	modFilesOnce sync.Once
	modFiles     []*goSource
	modFilesErr  error
)

// repoProductionSources parses (in full) every non-test Go file of the checkout once.
func repoProductionSources(t testing.TB) []*goSource {
	t.Helper()
	root, files := repoRoot(t), repoFiles(t)
	modFilesOnce.Do(func() {
		modFiles, modFilesErr = loadGoSources(root, files, parser.SkipObjectResolution, func(rel string) bool {
			return !strings.HasSuffix(rel, "_test.go")
		})
	})
	if modFilesErr != nil {
		t.Fatalf("%v", modFilesErr)
	}
	return modFiles
}

func TestIDPrefixesMatchSchemaGrammar(t *testing.T) {
	root := repoRoot(t)
	check := typedIDChecker(t, root)
	sites := scanIDPrefixes(repoProductionSources(t))

	// Positive controls: the scan must see the core's call sites, and resolve
	// constants declared in the same package and in another package.
	if len(sites) < 15 {
		t.Fatalf("found only %d id-minting call sites in internal/; the scan is not seeing them", len(sites))
	}
	seen := map[string]bool{}
	for _, s := range sites {
		for _, p := range s.Prefixes {
			seen[p] = true
		}
	}
	for _, want := range []string{"job", "aud", "src", "prep", "rcpt"} {
		if !seen[want] {
			t.Errorf("the scan never resolved the core prefix %q (resolved: %s)", want, setString(seen))
		}
	}

	t.Logf("%d id-minting call sites in internal/, prefixes: %s", len(sites), setString(seen))

	for _, s := range sites {
		if s.Err != nil {
			t.Errorf("%s: the id prefix %s is not a constant the gate can resolve (%v); declare it as a string const so it can be checked against the typedId grammar", s.Where, s.Expr, s.Err)
			continue
		}
		for _, p := range s.Prefixes {
			if err := check(p); err != nil {
				t.Errorf("%s: id prefix %q: %v", s.Where, p, err)
			}
		}
	}
}

func setString(m map[string]bool) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestIDPrefixGrammarRejectsOutOfGrammarPrefixes shows the checker is not
// vacuous: it passes the schema's own example prefixes and rejects the ones
// that internal/ids would mint but the public schema refuses.
func TestIDPrefixGrammarRejectsOutOfGrammarPrefixes(t *testing.T) {
	check := typedIDChecker(t, repoRoot(t))
	for _, p := range []string{"ses", "prep", "rcpt", "src", "job", "aud", "achk", "pdec", "ab", "abcdefgh", "a1b2c3d4"} {
		if err := check(p); err != nil {
			t.Errorf("prefix %q should be accepted: %v", p, err)
		}
	}
	for _, p := range []string{
		"audit_evt", // accepted by internal/ids, not by typedId
		"a",         // too short for typedId
		"abcdefghi", // nine characters
		"Job",
		"1ab",
		"ab-c",
		"",
	} {
		if err := check(p); err == nil {
			t.Errorf("prefix %q should be rejected", p)
		}
	}
}

// TestIDPrefixScannerOnInlineSources exercises every shape the scan claims to
// understand, on in-memory sources.
func TestIDPrefixScannerOnInlineSources(t *testing.T) {
	const idsImp = `import "` + idsPackagePath + `"`
	srcs := map[string]string{
		"internal/a/a.go": `package a
` + idsImp + `
const (
	Good = "good"
	Bad  = "Bad_Prefix"
	cat  = "ca" + "t"
)
type S struct {
	gen *ids.Generator
}
func (s *S) mint() {
	_ = s.gen.New("fromfield")
	_ = ids.New(Good)
	_ = ids.New(Bad)
	_ = ids.New(cat)
	_ = ids.New(string(Good))
	_ = ids.Valid("x", "validarg")
	_ = ids.NewGenerator(nil).New("chained")
}
func param(p string) { _ = ids.New(p) }
func assigned() {
	g := ids.NewGenerator(nil)
	_ = g.New("viaassign")
}
`,
		"internal/a/other.go": `package a
func (s *S) more() { _ = s.gen.New("otherfile") }
`,
		"internal/b/b.go": `package b
import (
	"` + modulePath + `/internal/a"
	aa "` + modulePath + `/internal/a"
	` + idsImp[len("import "):] + `
)
func f() { _ = ids.New(a.Good); _ = ids.New(aa.Bad) }
`,
		"internal/c/c.go": `package c
import idpkg "` + idsPackagePath + `"
type T struct{ idg idpkg.Generator }
func (t *T) f() { _ = t.idg.New("aliased") }
`,
		// None of these is an ID minted through internal/ids.
		"internal/d/d.go": `package d
type foo struct{}
func (foo) New(string) {}
func f(x foo) { x.New("NotAnID"); foo{}.New("AlsoNot") }
`,
		// Excluded: internal/ids itself and test files.
		"internal/ids/ids.go": `package ids
type Generator struct{}
func (Generator) New(p string) string { return New("Excluded") }
func New(p string) string { return p }
`,
		"internal/e/e_test.go": `package e
` + idsImp + `
func TestX() { _ = ids.New("audit_evt") }
`,
		// Outside internal/: not this rule's scope.
		"tools/t/t.go": `package t
` + idsImp + `
func f() { _ = ids.New("ToolsPrefix") }
`,
	}
	var files []*goSource
	for rel, src := range srcs {
		f, err := parseGoSource(rel, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		files = append(files, f)
	}

	got := map[string]bool{}
	unresolved := 0
	for _, s := range scanIDPrefixes(files) {
		if s.Err != nil {
			unresolved++
			continue
		}
		for _, p := range s.Prefixes {
			got[p] = true
		}
	}
	want := []string{
		"fromfield", "good", "Bad_Prefix", "cat", "validarg", "chained", "viaassign",
		"otherfile", "aliased",
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("prefix %q was not found (found: %s)", p, setString(got))
		}
	}
	if len(got) != len(want) {
		t.Errorf("found %s, want exactly %s", setString(got), strings.Join(want, ","))
	}
	if unresolved != 1 {
		t.Errorf("unresolved sites = %d, want 1 (the parameter)", unresolved)
	}
	for _, bad := range []string{"NotAnID", "AlsoNot", "Excluded", "audit_evt", "ToolsPrefix"} {
		if got[bad] {
			t.Errorf("prefix %q should not have been reported", bad)
		}
	}
}
