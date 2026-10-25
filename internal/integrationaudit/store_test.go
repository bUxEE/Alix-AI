package integrationaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/store/storetest"
)

// The tables are application-wide evidence, like the package and trust tables:
// they hold no tenant data and are exempt from the scope lint on purpose.
func TestTablesAreAppWideEvidence(t *testing.T) {
	h := newHarness(t)
	storetest.AssertScopedTables(t, h.db,
		"apppkg_packages", "trust_publisher_keys", "integrationaudit_runs", "integrationaudit_acceptances")

	h.install(validMinimalDir)
	run := h.audit(newStub(t, func(s *stub) { s.desc.Unmapped = []string{"x.op"} }))
	acceptAll(t, h, run)

	for _, q := range []string{
		`UPDATE integrationaudit_runs SET disposition = 'clear'`,
		`DELETE FROM integrationaudit_runs`,
		`UPDATE integrationaudit_acceptances SET operator_id = 'someone-else'`,
		`DELETE FROM integrationaudit_acceptances`,
	} {
		if _, err := h.db.ExecContext(ctx, q); err == nil {
			t.Errorf("%s: audit evidence is append-only", q)
		}
	}
	// a run must name a real disposition and a full digest
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO integrationaudit_runs (run_id, application_id, manifest_hash, report_sha256, disposition, issued_at, document)
		 VALUES ('run_x', 'a', ?, ?, 'maybe', 0, '{}')`, strings.Repeat("a", 64), strings.Repeat("b", 64)); err == nil {
		t.Errorf("an unknown disposition must be refused by the table")
	}
	// an acceptance with no operator or reason is refused by the table too
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO integrationaudit_acceptances (application_id, manifest_hash, finding_key, check_id, capability, operator_id, reason, run_id, accepted_at)
		 VALUES ('a', ?, ?, 'c', '', '', 'r', 'run_x', 0)`, strings.Repeat("a", 64), strings.Repeat("c", 16)); err == nil {
		t.Errorf("an acceptance without an operator must be refused by the table")
	}
}

// Nothing but this package sets a package active: no other non-test Go file of
// the repository writes the status 'active' of apppkg_packages.
func TestOnlyTheAuditActivates(t *testing.T) {
	roots := []string{"..", "../../cmd", "../../adapters", "../../pkg", "../../tools"}
	var scanned int
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a root that does not exist in this checkout
			}
			slash := filepath.ToSlash(path)
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "testdata" || d.Name() == "integrationaudit" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return nil
			}
			scanned++
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				low := strings.ToLower(s)
				if strings.Contains(low, "apppkg_packages") && strings.Contains(low, "update") && strings.Contains(low, "'active'") {
					t.Errorf("%s writes the active state of a package; only internal/integrationaudit may", slash)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files: the scan is not looking where it should", scanned)
	}
}

// internal/integrationaudit is tier 6 of docs/architecture/module-map.md. It
// does no network and runs no command: it reaches the application only through
// an adapter it is given.
func TestImportsStayWithinTheModuleMap(t *testing.T) {
	const modulePath = "github.com/buxee/alix-ai/"
	prod := map[string]bool{
		"pkg/contracts":                        true,
		"pkg/contracts/gen/actions":            true,
		"pkg/contracts/gen/applicationpackage": true,
		"internal/clock":                       true,
		"internal/ids":                         true,
		"internal/store":                       true,
		"internal/audit":                       true,
		"internal/trust":                       true,
		"internal/apppkg":                      true,
		"internal/policy":                      true,
		"internal/adapters":                    true,
		"internal/actions":                     true, // the prepare/confirm/commit coordinator the runtime probes (M1-18) run the application through
		// The RAG probes (M2-12, internal/integrationaudit/rag) run the real retrieval stack in a throwaway lab; module-map.md allows rag and providers.
		"internal/providers":             true,
		"internal/capabilities":          true, // the re-audit triggers (M5-10) refresh and read the live registry through their own interface; module-map.md allows it
		"internal/rag/answer":            true,
		"internal/rag/chunk":             true,
		"internal/rag/index":             true,
		"internal/rag/providers/fs":      true,
		"internal/rag/query":             true,
		"internal/rag/source":            true,
		"internal/rag/sources":           true,
		"pkg/contracts/gen/conversation": true,
		"pkg/contracts/gen/rag":          true,
	}
	testOnly := map[string]bool{
		"internal/store/storetest": true,
		"internal/capabilities":    true, // the registry the audit refreshes, through its own interface
		"tools/devkeys":            true,
		// M3-33: the ledger reference application is audited in process, as AlixAI reaches it: through its host bridge
		// on loopback and the real native adapter (probes/stepup_ledger_test.go). Tests only; nothing in production code.
		"adapters/native":                true,
		"examples/reference-apps/ledger": true,
		"internal/egress":                true,
		"pkg/bridge":                     true,
	}
	var files int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		isTest := strings.HasSuffix(path, "_test.go")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch p {
			case "net", "net/http", "os/exec", "plugin", "unsafe", "syscall":
				t.Errorf("%s imports %s: the audit does no network, shell-out or low-level work", path, p)
			}
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if rel == "internal/integrationaudit" {
				continue
			}
			if !prod[rel] && !(isTest && testOnly[rel]) {
				t.Errorf("%s imports %s, which module-map.md does not allow for internal/integrationaudit here", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("only %d files walked", files)
	}
}
