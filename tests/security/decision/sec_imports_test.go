package decisionsec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

const modulePath = "github.com/buxee/alix-ai/"

// The three trees of the scorer path (docs/architecture/decision-layer.md section 9) and the one place in them
// that may dial: the adapters' own client, which only ever talks to the endpoint the operator configured.
var (
	scorerTrees = []struct {
		dir      string
		minFiles int
	}{
		{"internal/decision", 12},
		{"adapters/decision", 12},
		{"internal/dialogue/decision", 6},
	}
	// networkAllowed are the directories where a network package may be imported.
	networkAllowed = []string{"adapters/decision/clm", "adapters/decision/localai", "adapters/decision/internal/wire"}
	// fsAllowed are the directories where the os package may be imported (never to write: see writeCalls).
	fsAllowed = []string{"adapters/decision/internal/wire"}
	// lowLevelAllowed are the directories where syscall may be imported.
	lowLevelAllowed = []string{"adapters/decision/internal/wire"}
)

// forbiddenEverywhere are packages the scorer path may not import at all: a database, a commit path, the
// application adapters, retrieval, and anything that runs code or loads it.
var forbiddenEverywhere = []string{
	"database/sql", "modernc.org/sqlite", "github.com/mattn/go-sqlite3", "go.mongodb.org", "github.com/lib/pq", "github.com/jackc/pgx",
	"os/exec", "plugin", "unsafe", "net/rpc", "debug/", "runtime/debug",
	modulePath + "internal/store", modulePath + "internal/actions", modulePath + "internal/adapters",
	modulePath + "internal/rag", modulePath + "internal/files", modulePath + "internal/sessions",
	modulePath + "internal/jobs", modulePath + "internal/history", modulePath + "internal/mcp",
	modulePath + "adapters/mcp", modulePath + "adapters/openapi", modulePath + "adapters/native",
	modulePath + "adapters/localai", modulePath + "adapters/external",
}

var networkPackages = []string{"net", "net/http", "net/url", "net/netip", "net/textproto", "net/smtp", "crypto/tls", "crypto/x509", "golang.org/x/net"}

// writeCalls are the functions of the os package (and its relatives) that change the file system.
var writeCalls = map[string]map[string]bool{
	"os": set("Create", "CreateTemp", "WriteFile", "OpenFile", "Mkdir", "MkdirAll", "MkdirTemp", "Remove", "RemoveAll", "Rename",
		"Symlink", "Link", "Chmod", "Chown", "Lchown", "Chtimes", "Truncate"),
	"io/ioutil":     set("WriteFile", "TempFile", "TempDir"),
	"syscall":       set("Open", "Creat", "Unlink", "Rmdir", "Rename", "Mkdir", "Symlink", "Link", "Chmod", "Truncate"),
	"path/filepath": set(),
}

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func underAny(dir string, roots []string) bool {
	for _, r := range roots {
		if dir == r || strings.HasPrefix(dir, r+"/") {
			return true
		}
	}
	return false
}

func matches(imp, forbidden string) bool {
	return imp == forbidden || strings.HasPrefix(imp, forbidden+"/") || (strings.HasSuffix(forbidden, "/") && strings.HasPrefix(imp, forbidden))
}

// checkSource reports what is wrong with one production source file that sits in directory dir (slash path from
// the repository root). src is nil to read the file named by filename.
func checkSource(dir, filename string, src any) []string {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.SkipObjectResolution)
	if err != nil {
		return []string{fmt.Sprintf("%s: does not parse: %v", filename, err)}
	}
	var bad []string
	local := map[string]string{} // local name -> import path, for the packages whose calls are checked
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		for _, fb := range forbiddenEverywhere {
			if matches(p, fb) {
				bad = append(bad, fmt.Sprintf("%s imports %s (forbidden on the scorer path)", filename, p))
			}
		}
		for _, n := range networkPackages {
			if p == n && !underAny(dir, networkAllowed) {
				bad = append(bad, fmt.Sprintf("%s imports %s outside the allowlisted client", filename, p))
			}
		}
		if p == "os" && !underAny(dir, fsAllowed) {
			bad = append(bad, fmt.Sprintf("%s imports os outside the allowlisted client", filename))
		}
		if p == "syscall" && !underAny(dir, lowLevelAllowed) {
			bad = append(bad, fmt.Sprintf("%s imports syscall outside the allowlisted client", filename))
		}
		if _, tracked := writeCalls[p]; tracked {
			name := path.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			local[name] = p
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if p, ok := local[id.Name]; ok && writeCalls[p][sel.Sel.Name] {
			bad = append(bad, fmt.Sprintf("%s calls %s.%s, which writes the file system", filename, p, sel.Sel.Name))
		}
		return true
	})
	return bad
}

// productionFiles lists the non-test Go files under dir (slash path from the root).
func productionFiles(t *testing.T, root, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{} // directory -> files
	err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		out[filepath.ToSlash(rel)] = append(out[filepath.ToSlash(rel)], p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// moduleImports returns the module-local imports of the production files of dir, memoised.
type importGraph struct {
	t    *testing.T
	root string
	memo map[string][]string
}

func (g *importGraph) imports(dir string) []string {
	if v, ok := g.memo[dir]; ok {
		return v
	}
	own := map[string]bool{}
	for _, fn := range productionFiles(g.t, g.root, dir)[dir] {
		f, _ := parser.ParseFile(token.NewFileSet(), fn, nil, parser.ImportsOnly)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			own[p] = true
		}
	}
	out := make([]string, 0, len(own))
	for p := range own {
		out = append(out, p)
	}
	sort.Strings(out)
	g.memo[dir] = out
	return out
}

// reach walks the imports of dir transitively and returns, for every forbidden package reached, the chain that
// reaches it. edgeAllowed says which (importer, imported) module edges are deliberate and not followed.
func (g *importGraph) reach(dir string, followEdge func(from, to string) bool) map[string][]string {
	found := map[string][]string{}
	visited := map[string]bool{}
	var walk func(d string, chain []string)
	walk = func(d string, chain []string) {
		if visited[d] {
			return
		}
		visited[d] = true
		for _, p := range g.imports(d) {
			for _, fb := range forbiddenEverywhere {
				if matches(p, fb) {
					found[p] = append(append([]string(nil), chain...), d, p)
				}
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			next := strings.TrimPrefix(p, modulePath)
			if followEdge != nil && !followEdge(d, next) {
				continue
			}
			walk(next, append(append([]string(nil), chain...), d))
		}
	}
	walk(dir, nil)
	return found
}

// SEC-105
func TestSecDecisionScorerPackageCannotImportDBFilesystemNetworkOrCommit(t *testing.T) {
	root := secutil.RepoRoot(t)

	t.Run("no production file of the scorer path imports a database, a commit path, an adapter or a dialler", func(t *testing.T) {
		total := 0
		for _, tree := range scorerTrees {
			files := productionFiles(t, root, tree.dir)
			n := 0
			for dir, list := range files {
				for _, fn := range list {
					n++
					for _, msg := range checkSource(dir, fn, nil) {
						t.Error(msg)
					}
				}
			}
			if n < tree.minFiles {
				t.Fatalf("%s: scanned %d files, want at least %d (the walk is looking at nothing)", tree.dir, n, tree.minFiles)
			}
			total += n
		}
		t.Logf("scanned %d production files", total)
	})

	t.Run("the checker flags what it must: a database, a file write, a commit path and a dialler in the wrong place", func(t *testing.T) {
		cases := []struct {
			name, dir, src, want string
		}{
			{"database/sql", "internal/decision", `package x; import _ "database/sql"`, "database/sql"},
			{"sqlite driver", "internal/decision", `package x; import _ "modernc.org/sqlite"`, "modernc.org/sqlite"},
			{"store", "internal/dialogue/decision", `package x; import _ "github.com/buxee/alix-ai/internal/store"`, "internal/store"},
			{"commit path", "internal/decision", `package x; import _ "github.com/buxee/alix-ai/internal/actions"`, "internal/actions"},
			{"app adapter", "internal/decision", `package x; import _ "github.com/buxee/alix-ai/internal/adapters/fake"`, "internal/adapters"},
			{"mcp adapter", "adapters/decision/clm", `package x; import _ "github.com/buxee/alix-ai/adapters/mcp"`, "adapters/mcp"},
			{"localai adapter from the decision adapter", "adapters/decision/clm", `package x; import _ "github.com/buxee/alix-ai/adapters/localai"`, "adapters/localai"},
			{"retrieval", "internal/decision", `package x; import _ "github.com/buxee/alix-ai/internal/rag"`, "internal/rag"},
			{"exec", "internal/decision", `package x; import _ "os/exec"`, "os/exec"},
			{"net/http in the engine", "internal/decision", `package x; import _ "net/http"`, "outside the allowlisted client"},
			{"net in the dialogue layer", "internal/dialogue/decision", `package x; import _ "net"`, "outside the allowlisted client"},
			{"os in the engine", "internal/decision", `package x; import _ "os"`, "outside the allowlisted client"},
			{"write in the client", "adapters/decision/internal/wire", `package x; import "os"; func f() { _ = os.WriteFile("a", nil, 0o600) }`, "os.WriteFile"},
			{"aliased write", "adapters/decision/internal/wire", `package x; import o "os"; func f() { _, _ = o.Create("a") }`, "os.Create"},
			{"remove", "adapters/decision/internal/wire", `package x; import "os"; func f() { _ = os.RemoveAll("a") }`, "os.RemoveAll"},
			{"open with flags", "adapters/decision/internal/wire", `package x; import "os"; func f() { _, _ = os.OpenFile("a", os.O_WRONLY, 0) }`, "os.OpenFile"},
		}
		for _, c := range cases {
			got := strings.Join(checkSource(c.dir, "planted.go", c.src), "\n")
			if !strings.Contains(got, c.want) {
				t.Errorf("%s: planted source was not flagged (want %q), got %q", c.name, c.want, got)
			}
		}
		// Controls: the same checker lets the permitted shapes through.
		for _, c := range []struct{ name, dir, src string }{
			{"client dials", "adapters/decision/clm", `package x; import "net/http"; var _ = http.StatusOK`},
			{"client reads a key file", "adapters/decision/internal/wire", `package x; import "os"; func f() { _, _ = os.ReadFile("a"); _, _ = os.Open("a") }`},
			{"engine uses the standard library", "internal/decision", `package x; import ("context"; "sort"); var _ = context.Background; var _ = sort.Ints`},
		} {
			if got := checkSource(c.dir, "ok.go", c.src); len(got) != 0 {
				t.Errorf("control %s was flagged: %v", c.name, got)
			}
		}
	})

	t.Run("the adapters reach no database, commit path or application adapter even through their dependencies", func(t *testing.T) {
		g := &importGraph{t: t, root: root, memo: map[string][]string{}}
		for _, dir := range []string{"adapters/decision/clm", "adapters/decision/localai", "adapters/decision/internal/wire"} {
			if reached := g.reach(dir, nil); len(reached) != 0 {
				t.Errorf("%s reaches %v", dir, reached)
			}
			if n := len(g.imports(dir)); n < 3 {
				t.Fatalf("%s: read %d imports (the graph walk is looking at nothing)", dir, n)
			}
		}
	})

	t.Run("the engine reaches a database only through the audit appender it is handed and, for update verification, the trust key lookup", func(t *testing.T) {
		g := &importGraph{t: t, root: root, memo: map[string][]string{}}
		for _, dir := range []string{"internal/decision", "internal/decision/lifecycle", "internal/decision/fakescorer"} {
			// The one other edge is the manifest verifier of internal/decision/lifecycle (D-16): it imports
			// internal/trust for trust.Verifier and is handed a trust.KeyLookup, so scoring never touches the
			// key store; only the exact edge lifecycle -> trust is exempt, from nowhere else.
			via := g.reach(dir, func(from, to string) bool {
				return to != "internal/audit" && !(from == "internal/decision/lifecycle" && to == "internal/trust")
			})
			if len(via) != 0 {
				t.Errorf("%s reaches %v other than through internal/audit", dir, via)
			}
		}
		// Control: the audit edge is exactly where the database comes from, so the exemption is not hiding nothing.
		if direct := g.reach("internal/decision", nil); len(direct) == 0 {
			t.Fatal("control: following internal/audit reached no store package; the graph walk cannot see transitive imports")
		}
		// Control: the trust edge is exempt only from lifecycle; from the engine it is a finding.
		if via := g.reach("internal/decision/lifecycle", func(_, to string) bool { return to != "internal/audit" }); len(via) == 0 {
			t.Fatal("control: following internal/trust reached no store package, so the lifecycle exemption hides nothing")
		}
	})

	t.Run("the provider contract hands a scorer a scope and a bounded request, never a handle", func(t *testing.T) {
		ty := reflect.TypeOf((*decision.DecisionModelProvider)(nil)).Elem()
		var names []string
		for i := 0; i < ty.NumMethod(); i++ {
			names = append(names, ty.Method(i).Name)
		}
		if got := strings.Join(names, ","); got != "Manifest,ScoreDecision" {
			t.Fatalf("DecisionModelProvider methods = %s", got)
		}
		m, _ := ty.MethodByName("ScoreDecision")
		seen := map[reflect.Type]bool{}
		var walk func(rt reflect.Type, where string)
		walk = func(rt reflect.Type, where string) {
			if seen[rt] {
				return
			}
			seen[rt] = true
			switch rt.Kind() {
			case reflect.Func, reflect.Chan, reflect.UnsafePointer:
				t.Errorf("%s is a %s: a scorer would be handed a capability", where, rt.Kind())
			case reflect.Struct:
				if p := rt.PkgPath(); p != "" && !strings.HasPrefix(p, modulePath+"pkg/contracts") && p != "time" {
					t.Errorf("%s is a struct of %s: a scorer would be handed a handle", where, p)
				}
				for i := 0; i < rt.NumField(); i++ {
					walk(rt.Field(i).Type, where+"."+rt.Field(i).Name)
				}
			case reflect.Pointer, reflect.Slice, reflect.Array:
				walk(rt.Elem(), where)
			case reflect.Map:
				walk(rt.Key(), where)
				walk(rt.Elem(), where)
			}
		}
		for i := 0; i < m.Type.NumIn(); i++ {
			in := m.Type.In(i)
			if in.String() == "context.Context" {
				continue
			}
			walk(in, fmt.Sprintf("ScoreDecision argument %d (%s)", i, in))
		}
		if m.Type.NumIn() < 3 {
			t.Fatalf("ScoreDecision takes %d arguments; the walk saw nothing", m.Type.NumIn())
		}
	})
}
