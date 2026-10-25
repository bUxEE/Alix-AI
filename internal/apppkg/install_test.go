package apppkg

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// harness is an installer on a throwaway database and data directory, with the
// published TEST-ONLY publisher key registered for demo-library.
type harness struct {
	in      *Installer
	db      *store.DB
	trust   *trust.Store
	dataDir string
	clk     *clock.Fake
}

func newHarness(t *testing.T, registerKey bool) *harness {
	t.Helper()
	db := storetest.Open(t, nil) // the default registry holds the trust and apppkg modules
	clk := clock.NewFake(tNow)
	ts := trust.NewStore(db, clk, trust.AllowTestOnlyKeys())
	if registerKey {
		if _, err := ts.RegisterKey(context.Background(), "demo-library", devkeys.KeyID, devkeys.PublicKey(), "test"); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	in, err := NewInstaller(InstallerOptions{
		DB:          db,
		Clock:       clk,
		DataDir:     dataDir,
		Verifier:    trust.NewVerifier(ts, clk),
		CoreVersion: "0.1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{in: in, db: db, trust: ts, dataDir: dataDir, clk: clk}
}

func (h *harness) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.QueryRowContext(context.Background(), `SELECT count(*) FROM apppkg_packages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// writeTree writes a package to a fresh directory so it can be installed with FromDir.
func writeTree(t *testing.T, files fs.FS) string {
	t.Helper()
	dir := t.TempDir()
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestInstallGoesToQuarantine (SEC-060): an install lands in the quarantined
// state, content-addressed under the data directory, and what is read back
// reports the stored state and exposes nothing.
// SEC-060
func TestInstallGoesToQuarantine(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()

	res := mustLoad(t, valid(t).signed(t), loadOptions())
	wantHash := res.ManifestHash

	got, err := h.in.Install(ctx, FromDir(validMinimalDir))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	rec := got.Record
	if !got.Created || rec.State != StateQuarantined {
		t.Fatalf("Created=%v State=%q, want a new quarantined record", got.Created, rec.State)
	}
	if rec.ManifestHash != wantHash || rec.ApplicationID != "demo-library" || rec.PackageVersion != "1.0.0" {
		t.Fatalf("record = %+v, want demo-library 1.0.0 at %s", rec, wantHash)
	}
	if rec.Path != "packages/demo-library/"+wantHash || rec.SignerKeyID != devkeys.KeyID || rec.Files != 24 || !rec.InstalledAt.Equal(tNow) {
		t.Fatalf("record details wrong: %+v", rec)
	}
	if rec.AuditReportID != "" {
		t.Fatalf("a fresh install carries no audit report: %q", rec.AuditReportID)
	}
	if h.rowCount(t) != 1 {
		t.Fatalf("want one row, have %d", h.rowCount(t))
	}

	// the files on disk are the verified bytes
	installed := filepath.Join(h.dataDir, filepath.FromSlash(rec.Path))
	want := readDirAll(t, validMinimalDir)
	have := readDirAll(t, installed)
	if len(have) != len(want) {
		t.Fatalf("installed %d files, want %d", len(have), len(want))
	}
	for name, b := range want {
		if !bytes.Equal(have[name], b) {
			t.Errorf("installed %s differs from the source", name)
		}
	}
	// no staging directory is left behind
	entries, _ := os.ReadDir(filepath.Join(h.dataDir, "packages", "demo-library"))
	if len(entries) != 1 || entries[0].Name() != wantHash {
		t.Fatalf("package directory holds %v, want only %s", entries, wantHash)
	}

	// reading it back reports the stored state, and a quarantined package is not active
	pkg, back, err := h.in.Open(ctx, "demo-library", wantHash)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if back.State != StateQuarantined || pkg.Status() != policy.StatusQuarantined {
		t.Fatalf("Open reports %q / %q, want quarantined", back.State, pkg.Status())
	}
	if pkg.Status() == policy.StatusActive {
		t.Fatal("a quarantined package must not be active")
	}
	if list, err := h.in.List(ctx, "demo-library"); err != nil || len(list) != 1 || list[0].ManifestHash != wantHash {
		t.Fatalf("List = %v, %v", list, err)
	}
	if list, _ := h.in.List(ctx, "another-app"); len(list) != 0 {
		t.Fatalf("List must filter on the application: %v", list)
	}

	t.Run("an archive of the same content installs to the same address", func(t *testing.T) {
		again, err := h.in.Install(ctx, FromArchive(bytes.NewReader(tarGz(t, valid(t).signed(t)))))
		if err != nil {
			t.Fatalf("Install archive: %v", err)
		}
		if again.Created || again.Record.ManifestHash != wantHash {
			t.Fatalf("Created=%v hash=%s, want the existing record", again.Created, again.Record.ManifestHash)
		}
		if h.rowCount(t) != 1 {
			t.Fatalf("a second install of the same bytes added a row")
		}
	})

	t.Run("installing again never resets a state", func(t *testing.T) {
		if err := h.in.Reject(ctx, "demo-library", wantHash, "operator rejected it"); err != nil {
			t.Fatal(err)
		}
		again, err := h.in.Install(ctx, FromDir(validMinimalDir))
		if err != nil {
			t.Fatal(err)
		}
		if again.Created || again.Record.State != StateRejected || again.Record.StatusReason != "operator rejected it" {
			t.Fatalf("a rejected package must stay rejected: %+v", again.Record)
		}
		pkg, _, err := h.in.Open(ctx, "demo-library", wantHash)
		if err != nil {
			t.Fatal(err)
		}
		if pkg.Status() == policy.StatusActive || pkg.Status() == policy.StatusQuarantined {
			t.Fatalf("a rejected package reports %q", pkg.Status())
		}
		if err := h.in.Reject(ctx, "demo-library", wantHash, "twice"); err == nil {
			t.Fatal("a rejected package cannot be rejected again")
		}
	})
}

// TestSec081InstallPermissions (SEC-081): the install tree is owner-only. On
// Windows the modes are advisory and the tree inherits the data directory's
// ACL (the same position as the key files of internal/trust).
// SEC-081
func TestSec081InstallPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not enforced on Windows; the install tree inherits the data directory ACL")
	}
	h := newHarness(t, true)
	got, err := h.in.Install(context.Background(), FromDir(validMinimalDir))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(h.dataDir, "packages")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		perm := info.Mode().Perm()
		switch {
		case d.IsDir() && perm != 0o700:
			t.Errorf("directory %s has mode %o, want 700", p, perm)
		case !d.IsDir() && perm != 0o600:
			t.Errorf("file %s has mode %o, want 600", p, perm)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = got
}

// TestInstallRefusals: nothing is written, and nothing is recorded, for a
// package that is unsigned, signed by a key nobody registered, or critical.
func TestInstallRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		register  bool
		src       func(t *testing.T) Source
		wantCheck string
		core      string
	}{
		{"unsigned", true, func(t *testing.T) Source { return FromFS(valid(t).fsys()) }, "signature.missing", ""},
		{"unknown publisher key", false, func(t *testing.T) Source { return FromFS(valid(t).signed(t)) }, "signature.invalid", ""},
		{"tampered", true, func(t *testing.T) Source {
			m := valid(t).signed(t)
			m["documentation/catalog/find-title.md"].Data = append(m["documentation/catalog/find-title.md"].Data, 'x')
			return FromFS(m)
		}, "signature.tampered", ""},
		{"core out of range", true, func(t *testing.T) Source { return FromFS(valid(t).signed(t)) }, "compat.core_out_of_range", "3.0.0"},
		{"critical cross-check", true, func(t *testing.T) Source {
			return FromFS(valid(t).remove(t, "conversation-contracts/reservations-create.it.yaml").signed(t))
		}, "contract.missing", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.register)
			if c.core != "" {
				in, err := NewInstaller(InstallerOptions{DB: h.db, Clock: h.clk, DataDir: h.dataDir, Verifier: h.in.opts.Verifier, CoreVersion: c.core})
				if err != nil {
					t.Fatal(err)
				}
				h.in = in
			}
			_, err := h.in.Install(ctx, c.src(t))
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Install error = %v, want *RefusedError", err)
			}
			if !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("code = %q, want validation_failed", contracts.CodeOf(err))
			}
			if !contains(ids(refused.Findings, contracts.SeverityCritical), c.wantCheck) {
				t.Fatalf("findings lack %s:\n%s", c.wantCheck, dump(refused.Findings))
			}
			if h.rowCount(t) != 0 {
				t.Fatalf("a refused package left a row")
			}
			if _, err := os.Stat(filepath.Join(h.dataDir, "packages")); err == nil {
				entries, _ := os.ReadDir(filepath.Join(h.dataDir, "packages", "demo-library"))
				if len(entries) != 0 {
					t.Fatalf("a refused package left files: %v", entries)
				}
			}
		})
	}
}

// TestInstallerNeedsATrustAnchor: an installer that cannot establish who signed
// a package does not exist.
func TestInstallerNeedsATrustAnchor(t *testing.T) {
	db := storetest.Open(t, nil)
	for name, o := range map[string]InstallerOptions{
		"no verifier":     {DB: db, DataDir: t.TempDir(), CoreVersion: "0.1.0"},
		"no database":     {DataDir: t.TempDir(), Verifier: newVerifierWithoutKeys(), CoreVersion: "0.1.0"},
		"no data dir":     {DB: db, Verifier: newVerifierWithoutKeys(), CoreVersion: "0.1.0"},
		"no core version": {DB: db, DataDir: t.TempDir(), Verifier: newVerifierWithoutKeys()},
	} {
		if _, err := NewInstaller(o); err == nil {
			t.Errorf("%s: NewInstaller must refuse", name)
		}
	}
}

// TestOpenReverifiesTheInstalledBytes: what is under the data directory is
// checked again, so a changed file, a replaced package or a key revoked since
// the install never yields a Package.
func TestOpenReverifiesTheInstalledBytes(t *testing.T) {
	ctx := context.Background()
	install := func(t *testing.T) (*harness, Record, string) {
		h := newHarness(t, true)
		got, err := h.in.Install(ctx, FromDir(validMinimalDir))
		if err != nil {
			t.Fatal(err)
		}
		return h, got.Record, filepath.Join(h.dataDir, filepath.FromSlash(got.Record.Path))
	}
	refusal := func(t *testing.T, err error, check string) {
		t.Helper()
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("Open error = %v, want *RefusedError", err)
		}
		if !contains(ids(refused.Findings, contracts.SeverityCritical), check) {
			t.Fatalf("findings lack %s:\n%s", check, dump(refused.Findings))
		}
	}

	t.Run("a file changed under the data directory", func(t *testing.T) {
		h, rec, dir := install(t)
		p := filepath.Join(dir, "documentation", "catalog", "find-title.md")
		if err := os.WriteFile(p, []byte("# changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := h.in.Open(ctx, rec.ApplicationID, rec.ManifestHash)
		refusal(t, err, "signature.tampered")
	})
	t.Run("a file added under the data directory", func(t *testing.T) {
		h, rec, dir := install(t)
		if err := os.WriteFile(filepath.Join(dir, "documentation", "planted.md"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := h.in.Open(ctx, rec.ApplicationID, rec.ManifestHash)
		refusal(t, err, "signature.tampered")
	})
	t.Run("another valid signed package put in its place", func(t *testing.T) {
		h, rec, dir := install(t)
		other := valid(t).edit(t, "alixai.yaml", "Demo Library Desk", "Demo Library Desk Two").signed(t)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		tree := writeTree(t, other)
		if err := os.Rename(tree, dir); err != nil {
			t.Fatal(err)
		}
		_, _, err := h.in.Open(ctx, rec.ApplicationID, rec.ManifestHash)
		refusal(t, err, "signature.tampered")
	})
	t.Run("the signing key was revoked after the install", func(t *testing.T) {
		h, rec, _ := install(t)
		if err := h.trust.RevokeKey(ctx, "demo-library", devkeys.KeyID, "compromised"); err != nil {
			t.Fatal(err)
		}
		_, _, err := h.in.Open(ctx, rec.ApplicationID, rec.ManifestHash)
		refusal(t, err, "signature.invalid")
	})
	t.Run("not installed", func(t *testing.T) {
		h := newHarness(t, true)
		_, _, err := h.in.Open(ctx, "demo-library", strings.Repeat("0", 64))
		if !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("Open error = %v, want ErrNotInstalled", err)
		}
	})
	t.Run("an orphan directory at the install path is replaced, never trusted", func(t *testing.T) {
		h := newHarness(t, true)
		res := mustLoad(t, valid(t).signed(t), loadOptions())
		orphan := filepath.Join(h.dataDir, "packages", "demo-library", res.ManifestHash)
		if err := os.MkdirAll(orphan, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(orphan, "alixai.yaml"), []byte("planted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := h.in.Install(ctx, FromDir(validMinimalDir))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := h.in.Open(ctx, "demo-library", got.Record.ManifestHash); err != nil {
			t.Fatalf("the planted directory was not replaced: %v", err)
		}
	})
}

// TestConcurrentInstallsOfOneContent: the same bytes installed from several
// goroutines are one record, whoever wins.
func TestConcurrentInstallsOfOneContent(t *testing.T) {
	h := newHarness(t, true)
	var wg sync.WaitGroup
	results := make([]*Installed, 6)
	errs := make([]error, len(results))
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = h.in.Install(context.Background(), FromDir(validMinimalDir))
		}()
	}
	wg.Wait()
	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("install %d: %v", i, errs[i])
		}
		if results[i].Created {
			created++
		}
		if results[i].Record.State != StateQuarantined {
			t.Errorf("install %d state %q", i, results[i].Record.State)
		}
	}
	if created != 1 || h.rowCount(t) != 1 {
		t.Fatalf("created %d times, %d rows; want exactly one of each", created, h.rowCount(t))
	}
}

// TestNoActivationPathInLoader (SEC-060): the loader and the installer have no
// way to make a package active. Only the integration audit (M1-17) does, and
// the database refuses an activation that carries no audit report.
// SEC-060
func TestNoActivationPathInLoader(t *testing.T) {
	t.Run("the source has no function that activates", func(t *testing.T) {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files := 0
		for _, pkg := range pkgs {
			for fname, f := range pkg.Files {
				files++
				ast.Inspect(f, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.FuncDecl:
						name := strings.ToLower(x.Name.Name)
						if strings.Contains(name, "activ") || strings.Contains(name, "promote") || strings.Contains(name, "setstatus") || strings.Contains(name, "setstate") {
							t.Errorf("%s: function %s looks like a way to change state", fname, x.Name.Name)
						}
					case *ast.SelectorExpr:
						if id, ok := x.X.(*ast.Ident); ok && id.Name == "policy" && x.Sel.Name == "StatusActive" {
							t.Errorf("%s: references policy.StatusActive", fname)
						}
					case *ast.BasicLit:
						if x.Kind != token.STRING {
							return true
						}
						s, _ := strconv.Unquote(x.Value)
						low := strings.ToLower(s)
						if strings.Contains(low, "'active'") || strings.Contains(low, "status = 'active") {
							t.Errorf("%s: SQL that mentions the active state: %q", fname, clipTest(s))
						}
						if strings.Contains(low, "update apppkg_packages") && !strings.Contains(low, "set status = 'rejected'") {
							t.Errorf("%s: an UPDATE other than the rejection: %q", fname, clipTest(s))
						}
					}
					return true
				})
			}
		}
		if files < 10 {
			t.Fatalf("scanned only %d source files; the scan is not looking at the package", files)
		}
	})

	t.Run("the only exported constants of a state", func(t *testing.T) {
		// Reading the active state is allowed (Open reports what the database
		// holds). Nothing else may name it as a value to write: the one place
		// the identifier appears in the package is its declaration and tests.
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
		if err != nil {
			t.Fatal(err)
		}
		var uses []string
		for _, pkg := range pkgs {
			for fname, f := range pkg.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name == "StateActive" && !strings.HasSuffix(fname, "install.go") {
						uses = append(uses, fname)
					}
					return true
				})
			}
		}
		sort.Strings(uses)
		if len(uses) != 0 {
			t.Errorf("StateActive is used outside install.go: %v", uses)
		}
	})

	t.Run("the database refuses what the loader would never do", func(t *testing.T) {
		h := newHarness(t, true)
		ctx := context.Background()
		got, err := h.in.Install(ctx, FromDir(validMinimalDir))
		if err != nil {
			t.Fatal(err)
		}
		rec := got.Record

		// a row cannot be inserted active
		_, err = h.db.ExecContext(ctx, `INSERT INTO apppkg_packages
 (application_id, manifest_hash, package_version, status, security_domain, signer_key_id, install_path, file_count, byte_count, warning_count, installed_at, status_changed_at, status_reason)
 VALUES ('demo-library', ?, '1.0.1', 'active', '', 'k', 'p', 1, 1, 0, 1, 1, 'x')`, strings.Repeat("a", 64))
		if err == nil {
			t.Fatal("an INSERT with status active must be refused")
		}
		// a quarantined package cannot be made active without an audit report
		if _, err := h.db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active' WHERE application_id = ? AND manifest_hash = ?`, rec.ApplicationID, rec.ManifestHash); err == nil {
			t.Fatal("an activation without an audit report must be refused")
		}
		if _, err := h.db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = '' WHERE application_id = ? AND manifest_hash = ?`, rec.ApplicationID, rec.ManifestHash); err == nil {
			t.Fatal("an activation with an empty audit report must be refused")
		}
		// rows are never deleted, and an installed package never changes identity
		if _, err := h.db.ExecContext(ctx, `DELETE FROM apppkg_packages`); err == nil {
			t.Fatal("a DELETE must be refused")
		}
		if _, err := h.db.ExecContext(ctx, `UPDATE apppkg_packages SET install_path = 'elsewhere'`); err == nil {
			t.Fatal("changing the install path must be refused")
		}
		after, err := h.in.Get(ctx, rec.ApplicationID, rec.ManifestHash)
		if err != nil || after.State != StateQuarantined {
			t.Fatalf("the package must still be quarantined: %+v, %v", after, err)
		}

		// the audit's path: an activation that carries its report is the one allowed change
		if _, err := h.db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rpt_test' WHERE application_id = ? AND manifest_hash = ?`, rec.ApplicationID, rec.ManifestHash); err != nil {
			t.Fatalf("the audit's activation must be possible: %v", err)
		}
		pkg, back, err := h.in.Open(ctx, rec.ApplicationID, rec.ManifestHash)
		if err != nil {
			t.Fatal(err)
		}
		if back.State != StateActive || back.AuditReportID != "rpt_test" || pkg.Status() != policy.StatusActive {
			t.Fatalf("Open must report the stored state: %+v status=%q", back, pkg.Status())
		}
		// the loader cannot reject or move an active package
		if err := h.in.Reject(ctx, rec.ApplicationID, rec.ManifestHash, "no"); err == nil {
			t.Fatal("Reject must only touch quarantined packages")
		}
		// a fresh load of the same bytes is quarantined whatever the database says
		res := mustLoad(t, valid(t).signed(t), loadOptions())
		if res.Package.Status() != policy.StatusQuarantined {
			t.Fatalf("Load reports %q, want quarantined", res.Package.Status())
		}
		// only one package of an application can be active
		other := valid(t).edit(t, "alixai.yaml", "Demo Library Desk", "Demo Library Desk Two").signed(t)
		g2, err := h.in.Install(ctx, FromFS(other))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rpt_2' WHERE manifest_hash = ?`, g2.Record.ManifestHash); err == nil {
			t.Fatal("two active packages of one application must be refused")
		}
		// rejected is final
		if err := h.in.Reject(ctx, g2.Record.ApplicationID, g2.Record.ManifestHash, "no"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'quarantined' WHERE manifest_hash = ?`, g2.Record.ManifestHash); err == nil {
			t.Fatal("a rejected package must keep its state")
		}
	})
}

func clipTest(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// TestMigrationScope: the table is application-wide on purpose, like the trust
// store's keys; the lint exempts it by name, and nothing else is exempt.
func TestMigrationScope(t *testing.T) {
	db := storetest.Open(t, nil)
	storetest.AssertScopedTables(t, db, "apppkg_packages", "trust_publisher_keys")
}

// readDirAll reads every file below dir, including signatures/.
func readDirAll(t testing.TB, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
