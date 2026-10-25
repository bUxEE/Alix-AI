package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/readiness/assist"
	"github.com/buxee/alix-ai/internal/readiness/report"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// rdyCLICanary stands for a secret value. It is TEST-ONLY and not a real credential.
const rdyCLICanary = "TESTONLY-canary-5c1e9a7b3d204f68"

const rdyCLIClinic = "../readiness/testdata/clinic"

// rdyCLIBox is a throwaway deployment: a project directory, a data directory and a configuration, one process per call.
type rdyCLIBox struct {
	t       *testing.T
	root    string
	project string
	cfgPath string
	// sections registers the localai and inference sections on the configuration registry, as serve does.
	sections bool
}

func rdyCLINewBox(t *testing.T, config string) *rdyCLIBox {
	t.Helper()
	b := &rdyCLIBox{t: t, root: t.TempDir()}
	b.project = filepath.Join(b.root, "project")
	if err := os.CopyFS(b.project, os.DirFS(rdyCLIClinic)); err != nil {
		t.Fatal(err)
	}
	// a secret-looking file the scan must record as present and never read
	b.write("project/.env", "API_PASSWORD="+rdyCLICanary+"\n")
	b.cfgPath = filepath.Join(b.root, "alixai.yaml")
	b.write("alixai.yaml", fmt.Sprintf("data_dir: %q\n%s", filepath.ToSlash(filepath.Join(b.root, "data")), config))
	return b
}

func (b *rdyCLIBox) write(rel, content string) {
	b.t.Helper()
	p := filepath.Join(b.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		b.t.Fatal(err)
	}
}

// run executes one readiness command in its own configuration registry, like a process. Flags may be anywhere.
func (b *rdyCLIBox) run(args ...string) result {
	b.t.Helper()
	var out, errb bytes.Buffer
	reg := config.NewRegistry()
	if b.sections {
		reg.Register(localaiSectionName, &localaiSection)
		reg.Register(inferenceSectionName, &inferenceSection)
	}
	full := append([]string{args[0], args[1], "-config", b.cfgPath}, args[2:]...)
	code := Default.Run(context.Background(), full, &Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		LookupEnv: func(string) (string, bool) { return "", false }, Config: reg,
	})
	return result{code, out.String(), errb.String()}
}

func (b *rdyCLIBox) mustRun(args ...string) result {
	b.t.Helper()
	r := b.run(args...)
	if r.code != ExitOK {
		b.t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

func rdyCLIFirstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// rdyCLIList is the JSON of `readiness list -format json`.
type rdyCLIListed struct {
	Key     string `json:"key"`
	Reports []struct {
		ReportID         string `json:"report_id"`
		PreviousReportID string `json:"previous_report_id"`
		Mode             string `json:"mode"`
		InputsSHA256     string `json:"inputs_sha256"`
		Summary          struct {
			Found, Partial, Missing, Unverified, Contradicted int64
		} `json:"summary"`
	} `json:"reports"`
}

func (b *rdyCLIBox) list(args ...string) rdyCLIListed {
	b.t.Helper()
	var l rdyCLIListed
	decode(b.t, b.mustRun(append([]string{"readiness", "list", "-format", "json"}, args...)...).stdout, &l)
	return l
}

// rdyCLITree hashes every file under root (relative slash path to content hash).
func rdyCLITree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			out[rel+"/"] = "dir"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReadinessScanProducesBilingualReportsAndStoresThem(t *testing.T) {
	b := rdyCLINewBox(t, "")

	en := b.mustRun("readiness", "scan", b.project, "-app", "clinic", "-lang", "en")
	if got := rdyCLIFirstLine(en.stdout); got != report.RdyAdvisoryLineEN {
		t.Fatalf("the first line of the English report = %q", got)
	}
	for _, want := range []string{"# Integration readiness report", "## Covered", "## Missing, and how to fix it", "## Contradictions", "## Could not be determined", "No model ran"} {
		if !strings.Contains(en.stdout, want) {
			t.Errorf("the English report lacks %q", want)
		}
	}
	it := b.mustRun("readiness", "scan", "-app", "clinic", "-lang", "it", b.project) // flags before the operand
	if got := rdyCLIFirstLine(it.stdout); got != report.RdyAdvisoryLineIT {
		t.Fatalf("the first line of the Italian report = %q", got)
	}
	for _, want := range []string{"# Rapporto di readiness dell'integrazione", "## Coperto", "## Mancante, e come rimediare", "## Contraddizioni", "## Non determinabile"} {
		if !strings.Contains(it.stdout, want) {
			t.Errorf("the Italian report lacks %q", want)
		}
	}

	js := b.mustRun("readiness", "scan", b.project, "-app", "clinic", "-format", "json")
	var doc struct {
		SchemaVersion int    `json:"schema_version"`
		ReportID      string `json:"report_id"`
		Advisory      bool   `json:"advisory"`
		Mode          string `json:"mode"`
		InputsSHA256  string `json:"inputs_sha256"`
		Summary       struct {
			Found int64 `json:"found"`
		} `json:"summary"`
	}
	decode(t, js.stdout, &doc)
	if doc.SchemaVersion != 1 || !doc.Advisory || doc.Mode != "deterministic_only" || doc.ReportID == "" || len(doc.InputsSHA256) != 64 || doc.Summary.Found == 0 {
		t.Fatalf("json report = %+v", doc)
	}

	// the three scans are stored, newest first, each pointing at the one before, with the same inputs hash
	l := b.list("-app", "clinic")
	if len(l.Reports) != 3 || l.Reports[0].ReportID != doc.ReportID {
		t.Fatalf("stored reports = %+v", l)
	}
	for i, r := range l.Reports[:2] {
		if r.PreviousReportID != l.Reports[i+1].ReportID || r.InputsSHA256 != doc.InputsSHA256 {
			t.Errorf("report %d = %+v: want it to follow %s with inputs %s", i, r, l.Reports[i+1].ReportID, doc.InputsSHA256)
		}
	}
	text := b.mustRun("readiness", "list", "-app", "clinic").stdout
	if !strings.Contains(text, doc.InputsSHA256) || !strings.Contains(text, doc.ReportID) {
		t.Errorf("the text list lacks the report id or its inputs hash:\n%s", text)
	}

	// show prints a stored report in either language, the latest or one by id
	shown := b.mustRun("readiness", "show", "-app", "clinic", "-lang", "it")
	if rdyCLIFirstLine(shown.stdout) != report.RdyAdvisoryLineIT || !strings.Contains(shown.stdout, doc.ReportID) {
		t.Errorf("show (latest, it) = %q", shown.stdout)
	}
	old := b.mustRun("readiness", "show", "-app", "clinic", "-report", l.Reports[2].ReportID, "-format", "json")
	if !strings.Contains(old.stdout, l.Reports[2].ReportID) {
		t.Errorf("show -report did not print the report asked for")
	}
	if r := b.run("readiness", "show", "-app", "clinic", "-report", "rrpt_00000000000000000000000000"); r.code != ExitError {
		t.Errorf("an unknown report id: exit %d", r.code)
	}

	// reports are kept per key: a project key does not see the application's reports
	if got := b.list(b.project); len(got.Reports) != 0 {
		t.Errorf("the directory key holds %d reports, want none", len(got.Reports))
	}

	// the secret file was recorded, never read: no output carries its value
	for name, s := range map[string]string{"en": en.stdout, "it": it.stdout, "json": js.stdout, "list": text, "stderr": en.stderr + it.stderr + js.stderr} {
		if strings.Contains(s, rdyCLICanary) {
			t.Errorf("the %s output carries a secret value", name)
		}
	}
}

func TestReadinessStatusShowsWhatChangedSinceTheLastScan(t *testing.T) {
	b := rdyCLINewBox(t, "")
	b.mustRun("readiness", "scan", b.project, "-app", "clinic")

	// nothing changed: the same report is shown, no new one is stored
	same := b.mustRun("readiness", "status", "-app", "clinic")
	if rdyCLIFirstLine(same.stdout) != report.RdyAdvisoryLineEN || !strings.Contains(same.stdout, "The files and the catalog are unchanged") {
		t.Fatalf("status of an unchanged tree:\n%s", same.stdout)
	}
	if strings.Contains(same.stderr, "changed since report") {
		t.Errorf("an unchanged tree must not rescan: %q", same.stderr)
	}
	for _, want := range []string{"## Summary", "### Missing (", "### Met ("} {
		if !strings.Contains(same.stdout, want) {
			t.Errorf("status lacks %q", want)
		}
	}
	first := b.list("-app", "clinic")
	if len(first.Reports) != 1 {
		t.Fatalf("an unchanged tree stored %d reports", len(first.Reports))
	}

	// remove the package: the checklist drops, the status rescans and says what changed
	if err := os.RemoveAll(filepath.Join(b.project, "package")); err != nil {
		t.Fatal(err)
	}
	after := b.mustRun("readiness", "status", "-app", "clinic", "-diff")
	if !strings.Contains(after.stderr, "changed since report "+first.Reports[0].ReportID) {
		t.Errorf("stderr = %q: want the rescan said", after.stderr)
	}
	for _, want := range []string{"The files or the catalog changed since the previous report", "## Changes since the previous report", "### Newly missing (", "[was: Met]"} {
		if !strings.Contains(after.stdout, want) {
			t.Errorf("status after the change lacks %q:\n%s", want, after.stdout)
		}
	}
	if strings.Contains(after.stdout, "### Newly missing (0)") {
		t.Errorf("removing the package must make something newly missing")
	}
	l := b.list("-app", "clinic")
	if len(l.Reports) != 2 || l.Reports[1].ReportID != first.Reports[0].ReportID || l.Reports[0].Summary.Found >= l.Reports[1].Summary.Found {
		t.Fatalf("stored reports = %+v", l)
	}
	if l.Reports[0].InputsSHA256 == l.Reports[1].InputsSHA256 {
		t.Errorf("the inputs hash did not change with the files")
	}

	// the same view in JSON and in Italian, then stable again
	var js struct {
		Rescanned bool `json:"rescanned"`
		Status    struct {
			InputsChanged bool `json:"inputs_changed"`
			Items         []struct {
				Status         string `json:"status"`
				PreviousStatus string `json:"previous_status"`
				Changed        bool   `json:"changed"`
			} `json:"items"`
		} `json:"status"`
		Diff struct {
			NewlyMissing []struct{ RequirementID string } `json:"newly_missing"`
		} `json:"diff"`
	}
	decode(t, b.mustRun("readiness", "status", "-app", "clinic", "-format", "json", "-diff").stdout, &js)
	changed := 0
	for _, it := range js.Status.Items {
		if it.Changed {
			changed++
		}
	}
	if js.Rescanned || !js.Status.InputsChanged || changed == 0 || len(js.Diff.NewlyMissing) == 0 {
		t.Errorf("json status = %+v", js)
	}
	itText := b.mustRun("readiness", "status", "-app", "clinic", "-lang", "it").stdout
	if rdyCLIFirstLine(itText) != report.RdyAdvisoryLineIT || !strings.Contains(itText, "## Requisiti") {
		t.Errorf("the Italian status = %q", itText)
	}
	if got := b.list("-app", "clinic"); len(got.Reports) != 2 {
		t.Errorf("status of an unchanged tree stored a report: %d", len(got.Reports))
	}

	// by directory (no -app): the same flow under the directory's own key
	b.mustRun("readiness", "scan", b.project)
	if r := b.run("readiness", "status", b.project); r.code != ExitOK || !strings.Contains(r.stdout, "unchanged") {
		t.Errorf("status by directory: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	// status needs a report, and a key
	if r := b.run("readiness", "status", "-app", "nothing-yet"); r.code != ExitError || !strings.Contains(r.stderr, "no readiness report") {
		t.Errorf("status with no report: exit %d\n%s", r.code, r.stderr)
	}
	if r := b.run("readiness", "status"); r.code != ExitUsage {
		t.Errorf("status with neither a directory nor -app: exit %d", r.code)
	}
}

func TestReadinessWorksWithNoModelAndSaysSo(t *testing.T) {
	// nothing configured: the deterministic leg runs alone, and the report and stderr say so
	plain := rdyCLINewBox(t, "")
	r := plain.mustRun("readiness", "scan", plain.project)
	if !strings.Contains(r.stdout, "No model ran") || !strings.Contains(r.stdout, "no model is configured") {
		t.Errorf("the report does not say no model ran:\n%s", r.stdout)
	}
	if !strings.Contains(r.stderr, "the model leg did not run (not_configured)") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if rdyCLIFirstLine(r.stdout) != report.RdyAdvisoryLineEN || !strings.Contains(r.stdout, "## Missing") {
		t.Errorf("the deterministic report is incomplete:\n%s", r.stdout)
	}

	// a model is available and -no-model is given: it is never called, and the output says it was disabled
	gen := fake.New()
	gen.On(fake.Any()).Reply(`{"proposals":[]}`)
	rdyCLIUseModel(t, &assist.AssistModel{Name: "local", ModelID: "fake-model-0", Local: true, Gen: gen})
	r = plain.mustRun("readiness", "scan", plain.project, "-no-model")
	if gen.Calls() != 0 {
		t.Errorf("-no-model still called the model %d times", gen.Calls())
	}
	if !strings.Contains(r.stderr, "disabled by -no-model") || !strings.Contains(r.stdout, "No model ran") {
		t.Errorf("-no-model output:\n%s\n%s", r.stdout, r.stderr)
	}

	// a model that is configured and fails: exit 0, the cause on stderr, the report deterministic and says why
	down := fake.New()
	down.On(fake.Any()).Fail(fake.FailUnavailable)
	rdyCLIUseModel(t, &assist.AssistModel{Name: "local", ModelID: "fake-model-0", Local: true, Gen: down})
	readmeOnly := rdyCLINewBox(t, "")
	if err := os.RemoveAll(filepath.Join(readmeOnly.project, "package")); err != nil {
		t.Fatal(err)
	}
	r = readmeOnly.mustRun("readiness", "scan", readmeOnly.project, "-format", "json")
	var doc struct {
		Mode     string `json:"mode"`
		ModelLeg struct {
			Status string `json:"status"`
		} `json:"model_leg"`
	}
	decode(t, r.stdout, &doc)
	if down.Calls() == 0 || doc.Mode != "deterministic_only" || doc.ModelLeg.Status != "unavailable" {
		t.Errorf("a failing model: calls %d, report %+v", down.Calls(), doc)
	}
	if !strings.Contains(r.stderr, "the model leg did not run (unavailable)") || !strings.Contains(r.stderr, "unavailable") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestReadinessNeverWritesOutsideTheDataDirAndTheNamedOutFile(t *testing.T) {
	b := rdyCLINewBox(t, "")
	b.write("out/keep.txt", "unrelated\n")
	outFile := filepath.Join(b.root, "out", "report.md")

	before := rdyCLITree(t, b.root)
	b.mustRun("readiness", "scan", b.project, "-app", "clinic", "-format", "both", "-out", outFile)
	b.mustRun("readiness", "status", "-app", "clinic")
	b.mustRun("readiness", "list", "-app", "clinic")
	b.mustRun("readiness", "show", "-app", "clinic")
	after := rdyCLITree(t, b.root)

	dataPrefix := "data/"
	for p, h := range after {
		if before[p] == h || strings.HasPrefix(p, dataPrefix) || p == "data/" {
			continue
		}
		if p != "out/report.md" {
			t.Errorf("%s was written outside the data directory and the named -out file", p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("%s was removed", p)
		}
	}
	// the one named file holds the JSON of a "both" run, owner-only where the OS has modes
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("the named file was not written: %v", err)
	}
	var doc struct {
		Advisory bool `json:"advisory"`
	}
	decode(t, string(raw), &doc)
	if !doc.Advisory {
		t.Errorf("-out file = %s", raw)
	}
	if fi, err := os.Stat(outFile); err == nil && os.PathSeparator == '/' && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("-out file mode = %v: want owner-only", fi.Mode().Perm())
	}
	if strings.Contains(string(raw), rdyCLICanary) {
		t.Errorf("-out file carries a secret value")
	}

	// a bad -out writes nothing at all and stops before the scan
	fresh := rdyCLINewBox(t, "")
	snap := rdyCLITree(t, fresh.root)
	for name, out := range map[string]string{
		"a directory":      filepath.Join(fresh.root, "project"),
		"a missing parent": filepath.Join(fresh.root, "nowhere", "r.md"),
	} {
		if r := fresh.run("readiness", "scan", fresh.project, "-out", out); r.code != ExitUsage {
			t.Errorf("-out %s: exit %d, want usage", name, r.code)
		}
	}
	// the read-only commands create no data directory
	for _, args := range [][]string{{"readiness", "list", "-app", "x"}, {"readiness", "show", "-app", "x"}, {"readiness", "status", "-app", "x"}} {
		fresh.run(args...)
	}
	if got := rdyCLITree(t, fresh.root); len(got) != len(snap) {
		t.Errorf("a refused or read-only command wrote files: before %d entries, after %d", len(snap), len(got))
	}
}

// A completed scan exits 0 whatever it finds, a scan error does not, and a malformed call is a usage error.
func TestReadinessExitStatusAndUsage(t *testing.T) {
	b := rdyCLINewBox(t, "")
	empty := filepath.Join(b.root, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := b.run("readiness", "scan", empty); r.code != ExitOK || !strings.Contains(r.stdout, "## Missing") {
		t.Errorf("an empty project finds nothing and is not an error: exit %d\n%s", r.code, r.stderr)
	}
	if r := b.run("readiness", "scan", filepath.Join(b.root, "no-such-dir")); r.code != ExitError {
		t.Errorf("a directory that is not there is a scan error: exit %d", r.code)
	}
	for _, args := range [][]string{
		{"readiness", "scan"},
		{"readiness", "scan", b.project, b.project},
		{"readiness", "scan", b.project, "-lang", "fr"},
		{"readiness", "scan", b.project, "-format", "xml"},
		{"readiness", "scan", b.project, "-app", "bad app"},
		{"readiness", "scan", b.project, "-model", "Not A Registry Id"},
		{"readiness", "scan", b.project, "-model", "granite-4.0-350m-q4_k_m"}, // nothing is configured: free text is never accepted
		{"readiness", "status", b.project, "-format", "both"},
		{"readiness", "list"},
	} {
		if r := b.run(args...); r.code != ExitUsage {
			t.Errorf("%v: exit %d, want usage\n%s", args[1:], r.code, r.stderr)
		}
	}
}

// TestAuditOutputLinksTheLatestReadinessReport walks the audit as in TestAuditCommandsEndToEnd: the audit says there is no
// readiness report, an operator scans the project under the application id, and the next audit links that report. The
// audit's own outcome and exit status do not change.
func TestAuditOutputLinksTheLatestReadinessReport(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t)
	transportsSwapDefault(t)
	e := newOpsEnv(t, "")
	signerPriv, signerPub, _ := e.genKey("audit-signer")
	e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", signerPub, "-operator", "root")
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys", "-operator", "root")
	e.mustRun("package", "install", "-operator", "root", filepath.Join(fixtures, "valid-minimal"))

	type auditOut struct {
		Outcome   string `json:"outcome"`
		Activated bool   `json:"activated"`
		Readiness *struct {
			Status       string `json:"status"`
			ReportID     string `json:"report_id"`
			InputsSHA256 string `json:"inputs_sha256"`
			Mode         string `json:"mode"`
			Note         string `json:"note"`
			Summary      *struct {
				Missing int64 `json:"missing"`
			} `json:"summary"`
		} `json:"readiness"`
	}
	audit := func() (auditOut, result) {
		res := e.run("audit", "run", "-no-activate", "-signing-key", "audit-signer="+signerPriv, "-operator", "root", "demo-library")
		var out auditOut
		decode(t, res.stdout, &out)
		return out, res
	}

	before, resBefore := audit()
	if before.Readiness == nil || before.Readiness.Status != "none" || !strings.Contains(before.Readiness.Note, "readiness scan") || !strings.Contains(before.Readiness.Note, "not an audit pass") {
		t.Fatalf("with no report the audit says so: %+v", before.Readiness)
	}

	// scan a project under the application id (the audit looks up that key and no other)
	project := filepath.Join(e.dir, "project")
	if err := os.CopyFS(project, os.DirFS(rdyCLIClinic)); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ReportID     string `json:"report_id"`
		InputsSHA256 string `json:"inputs_sha256"`
	}
	decode(t, e.mustRun("readiness", "scan", project, "-app", "demo-library", "-format", "json").stdout, &doc)
	// a report stored for another application is not linked
	e.mustRun("readiness", "scan", project, "-app", "other-app")

	after, resAfter := audit()
	got := after.Readiness
	if got == nil || got.Status != "linked" || got.ReportID != doc.ReportID || got.InputsSHA256 != doc.InputsSHA256 || got.Summary == nil || got.Mode != "deterministic_only" {
		t.Fatalf("the audit does not link the readiness report %+v: %+v", doc, got)
	}
	if !strings.Contains(got.Note, "not an audit pass") {
		t.Errorf("the link must say a readiness report is not an audit pass: %q", got.Note)
	}
	// independent: same outcome, same exit status, same activation as before the report existed
	if after.Outcome != before.Outcome || after.Activated != before.Activated || resAfter.code != resBefore.code {
		t.Errorf("the readiness report changed the audit: before %+v (exit %d), after %+v (exit %d)", before, resBefore.code, after, resAfter.code)
	}
}

func TestReadinessModelLegIsLabelledUnqualifiedUnlessTheRegistryQualifiesIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		qualified bool
		want      string
	}{{"candidate", false, "unqualified"}, {"qualified", true, "qualified"}} {
		t.Run(tc.name, func(t *testing.T) {
			gen := fake.New()
			gen.On(fake.Any()).Reply(`{"proposals":[]}`)
			rdyCLIUseModel(t, &assist.AssistModel{Name: "local", ModelID: "fake-model-0", Local: true, Qualified: tc.qualified, Gen: gen})
			readme := rdyCLINewBox(t, "")
			if err := os.RemoveAll(filepath.Join(readme.project, "package")); err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Mode  string `json:"mode"`
				Model struct {
					Qualification string `json:"qualification"`
					Label         string `json:"label"`
					Runtime       string `json:"runtime"`
				} `json:"model"`
				ModelLeg struct {
					Status string `json:"status"`
				} `json:"model_leg"`
			}
			decode(t, readme.mustRun("readiness", "scan", readme.project, "-format", "json").stdout, &doc)
			if gen.Calls() == 0 || doc.Mode != "deterministic_and_model" || doc.ModelLeg.Status != "ran" || doc.Model.Qualification != tc.want || doc.Model.Runtime != "local" {
				t.Errorf("calls %d, report %+v", gen.Calls(), doc)
			}
			if !strings.Contains(doc.Model.Label, tc.want) || !strings.Contains(doc.Model.Label, "advisory") {
				t.Errorf("label = %q", doc.Model.Label)
			}
			md := readme.mustRun("readiness", "scan", readme.project).stdout
			if !strings.Contains(md, "advisory. A model only proposes") {
				t.Errorf("the markdown does not label the model:\n%s", md)
			}
		})
	}
}

// readinessSection is the text of a status section from its heading to the next one.
func readinessSection(out, heading string) string {
	i := strings.Index(out, heading)
	if i < 0 {
		return ""
	}
	s := out[i:]
	if k := strings.Index(s[1:], "\n#"); k >= 0 {
		s = s[:k+1]
	}
	return s
}

// The clinic is integrated over MCP, so its native adapter requirement is not applicable: status and list say so in
// both languages and never list it as missing.
func TestReadinessStatusListsOtherTransportsAsNotApplicable(t *testing.T) {
	b := rdyCLINewBox(t, "")
	b.mustRun("readiness", "scan", b.project, "-app", "clinic")
	for _, c := range []struct{ lang, missing, na string }{{"en", "### Missing (", "### Not applicable (1)"}, {"it", "### Mancante (", "### Non applicabile (1)"}} {
		out := b.mustRun("readiness", "status", "-app", "clinic", "-lang", c.lang).stdout
		na, missing := readinessSection(out, c.na), readinessSection(out, c.missing)
		if !strings.Contains(na, "req.native.adapter") || missing == "" || strings.Contains(missing, "req.native.adapter") {
			t.Errorf("%s: req.native.adapter is not listed as not applicable only:\n%s", c.lang, out)
		}
	}
	if text := b.mustRun("readiness", "list", "-app", "clinic").stdout; !strings.Contains(text, "not applicable 1") {
		t.Errorf("the list does not count the not-applicable requirement:\n%s", text)
	}
}
