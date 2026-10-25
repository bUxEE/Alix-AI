package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRepo builds a throwaway checkout: go.mod, a backlog, and test files.
type fakeRepo struct {
	t    *testing.T
	root string
}

func newFakeRepo(t *testing.T, backlogRows string) *fakeRepo {
	t.Helper()
	r := &fakeRepo{t: t, root: t.TempDir()}
	r.write("go.mod", "module example.test/x\n\ngo 1.22\n")
	r.write("docs/security/test-backlog.md", backlogHeader+backlogRows)
	return r
}

const backlogHeader = `# backlog

## 4. Backlog

| ID | Threat | Ref | Component (control task) | Required test | Test(s) | Owner | Sev | Kind |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
`

func (r *fakeRepo) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// backlogRow builds a backlog row.
func backlogRow(id, tests, owner, sev, kind string) string {
	return "| " + id + " | threat | §1 | comp | required | " + tests + " | " + owner + " | " + sev + " | " + kind + " |\n"
}

// runTool runs the tool over the fake repo with the given pending.txt and flags.
func (r *fakeRepo) runTool(pending string, flags ...string) (code int, out, errs string) {
	r.t.Helper()
	old := pendingFile
	pendingFile = pending
	r.t.Cleanup(func() { pendingFile = old })
	var so, se bytes.Buffer
	code = run(append([]string{"-root", r.root}, flags...), &so, &se)
	return code, so.String(), se.String()
}

const goTest = "package x\n\nimport \"testing\"\n\n"

func TestToolCoveredByNameUnderTestsSecurity(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "tests/security/a: TestSecA", "M5-08", "High", "automated"))
	r.write("tests/security/a/a_test.go", goTest+"func TestSecA(t *testing.T) {}\n")
	if code, out, errs := r.runTool(""); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestToolMissingNameIsUncovered(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "tests/security/a: TestSecA", "M5-08", "High", "automated"))
	r.write("tests/security/a/a_test.go", goTest+"func TestSecOther(t *testing.T) {}\n")
	code, out, errs := r.runTool("")
	if code != 1 || !strings.Contains(errs, "SEC-001 has no test") || !strings.Contains(out, "UNCOVERED") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

// A legacy test elsewhere must not stand in for the cross-cutting test the
// backlog assigns to tests/security.
func TestToolLegacyTestDoesNotReplaceSecuritySuiteTest(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "internal/dialogue: TestLegacy; tests/security/a: TestSecA (new)", "M1-14, M5-08", "High", "automated"))
	r.write("internal/dialogue/d_test.go", goTest+"func TestLegacy(t *testing.T) {}\n")
	code, _, errs := r.runTool("")
	if code != 1 || !strings.Contains(errs, "SEC-001 has no test") {
		t.Fatalf("a legacy test replaced the security test: exit %d, %s", code, errs)
	}
	// Writing the test fixes it: a positive control for the rule above.
	r.write("tests/security/a/a_test.go", goTest+"func TestSecA(t *testing.T) {}\n")
	if code, out, errs := r.runTool(""); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestToolRowWithoutSecurityNamesAcceptsAnyResolvedTest(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "internal/dialogue: TestLegacy", "M1-14", "High", "automated"))
	r.write("internal/dialogue/d_test.go", goTest+"func TestLegacy(t *testing.T) {}\n")
	if code, out, errs := r.runTool(""); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestToolTagCoversOnlyTheFunctionItIsAbove(t *testing.T) {
	rows := backlogRow("SEC-001", "tests/security/a: TestSecGone (new)", "M5-08", "High", "automated")
	r := newFakeRepo(t, rows)
	// The tag sits above a helper that is not a test, then a blank line, then the test: not "directly above".
	r.write("tests/security/a/a_test.go", goTest+"// SEC-001\nfunc helper() {}\n\nfunc TestSecWrong(t *testing.T) {}\n")
	if code, _, errs := r.runTool(""); code != 1 || !strings.Contains(errs, "SEC-001 has no test") {
		t.Fatalf("a tag on the wrong function covered the id: exit %d, %s", code, errs)
	}
	r.write("tests/security/a/a_test.go", goTest+"// SEC-001\nfunc helper() {}\n\n// SEC-002, SEC-001\nfunc TestSecRight(t *testing.T) {}\n")
	if code, out, errs := r.runTool(""); code != 0 {
		t.Fatalf("a tagged test did not cover the id: exit %d\n%s\n%s", code, out, errs)
	}
}

func TestToolPendingIdPassesUntilCoveredThenMustLeaveTheList(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "tests/security/a: TestSecA (new)", "M5-06", "Medium", "automated"))
	r.write("internal/other/o_test.go", goTest+"func TestOther(t *testing.T) {}\n")
	pending := "SEC-001 pending M5-06 the package has not landed\n"
	code, out, errs := r.runTool(pending)
	if code != 0 || !strings.Contains(out, "pending") {
		t.Fatalf("pending id failed the default run: exit %d\n%s\n%s", code, out, errs)
	}
	if code, _, errs := r.runTool(pending, "-strict"); code != 1 || !strings.Contains(errs, "SEC-001 is pending") {
		t.Fatalf("pending id passed -strict: exit %d, %s", code, errs)
	}
	// The test lands: the stale line is now the failure.
	r.write("tests/security/a/a_test.go", goTest+"func TestSecA(t *testing.T) {}\n")
	if code, _, errs := r.runTool(pending); code != 1 || !strings.Contains(errs, "remove it from pending.txt") {
		t.Fatalf("a covered id stayed on the pending list: exit %d, %s", code, errs)
	}
	if code, out, errs := r.runTool(""); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestToolPendingListIsValidated(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "tests/security/a: TestSecA", "M5-08", "High", "automated"))
	r.write("tests/security/a/a_test.go", goTest+"func TestSecA(t *testing.T) {}\n")
	for name, pending := range map[string]string{
		"unknown id":   "SEC-999 pending M5-06 reason\n",
		"no reason":    "SEC-001 pending M5-06\n",
		"bad kind":     "SEC-001 later M5-06 reason\n",
		"bad id":       "SEC-1 pending M5-06 reason\n",
		"listed twice": "SEC-001 pending M5-06 a\nSEC-001 pending M5-06 b\n",
	} {
		code, _, errs := r.runTool(pending)
		if code == 0 {
			t.Errorf("%s: accepted (%s)", name, errs)
		}
	}
}

func TestToolDefectGatedTestIsNeverCoverage(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "internal/audit: TestLegacy; tests/security/audit: TestSecTail (new)", "M1-15, M5-08", "High", "automated"))
	r.write("internal/audit/a_test.go", goTest+"func TestLegacy(t *testing.T) {}\n")
	r.write("tests/security/audit/a_test.go", goTest+
		"import \"example.test/x/tests/security/secutil\"\n\n// SEC-001\nfunc TestSecTail(t *testing.T) {\n\tsecutil.Known(t, \"SEC-001\", \"G6\", \"the head is not signed\")\n}\n")
	code, out, errs := r.runTool("")
	if code != 0 || !strings.Contains(out, "DEFECT SEC-001 (G6)") || !strings.Contains(out, "the head is not signed") {
		t.Fatalf("the defect was not listed: exit %d\n%s\n%s", code, out, errs)
	}
	if code, _, errs := r.runTool("", "-strict"); code != 1 || !strings.Contains(errs, "SEC-001 is DEFECT") {
		t.Fatalf("a defect passed -strict: exit %d, %s", code, errs)
	}
}

func TestToolDefectCallNamingUnknownIdFails(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "tests/security/a: TestSecA", "M5-08", "High", "automated"))
	r.write("tests/security/a/a_test.go", goTest+"import \"x/secutil\"\n\nfunc TestSecA(t *testing.T) {}\n\nfunc TestSecB(t *testing.T) { secutil.Known(t, \"SEC-777\", \"G1\", \"y\") }\n")
	if code, _, errs := r.runTool(""); code != 1 || !strings.Contains(errs, "SEC-777") {
		t.Fatalf("a defect for an id that is not in the backlog was accepted: exit %d, %s", code, errs)
	}
}

func TestToolPentestAndHumanRows(t *testing.T) {
	rows := backlogRow("SEC-001", "docs/security/pentest-internal/findings.md: recorded result (new)", "M5-12", "Low", "pentest") +
		backlogRow("SEC-002", "internal/x: TestX; docs/evidence/keys.md (human)", "M5-03", "High", "automated+human")
	r := newFakeRepo(t, rows+"\n### 6.2 Entries verified by a pentest or by human evidence\n\n| Kind | Entries | Evidence file |\n| --- | --- | --- |\n| pentest | SEC-001 | `docs/security/pentest-internal/findings.md` (M5-12) |\n| human | SEC-002 (key custody) | `docs/evidence/keys.md` |\n\n### 6.3 Totals\n")
	r.write("internal/x/x_test.go", goTest+"func TestX(t *testing.T) {}\n")

	// Evidence missing: SEC-001 is assigned (passes), SEC-002 is covered by its test but needs evidence for -strict.
	code, out, errs := r.runTool("")
	if code != 0 || !strings.Contains(out, "SEC-001  assigned") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if code, _, errs := r.runTool("", "-strict"); code != 1 || !strings.Contains(errs, "SEC-001 is assigned") || !strings.Contains(errs, "SEC-002: the pentest or human evidence is not recorded") {
		t.Fatalf("strict mode let missing evidence pass: exit %d, %s", code, errs)
	}

	// A report that exists but does not name the id is not evidence.
	r.write("docs/security/pentest-internal/findings.md", "# findings\n\nSEC-099 was probed.\n")
	if code, _, errs := r.runTool("", "-strict"); code != 1 || !strings.Contains(errs, "SEC-001 is assigned") {
		t.Fatalf("a report that does not name the id counted as evidence: exit %d, %s", code, errs)
	}

	// Recorded results clear both.
	r.write("docs/security/pentest-internal/findings.md", "# findings\n\nSEC-001: no finding.\n")
	r.write("docs/evidence/keys.md", "keys held offline (SEC-002)\n")
	if code, out, errs := r.runTool("", "-strict"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestToolPentestRowWithNoEvidenceFileIsUncovered(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "internal/x: TestX", "M5-12", "High", "pentest"))
	r.write("internal/x/x_test.go", goTest+"func TestX(t *testing.T) {}\n")
	if code, _, errs := r.runTool(""); code != 1 || !strings.Contains(errs, "SEC-001 has no test") {
		t.Fatalf("a pentest row with no report to own it passed: exit %d, %s", code, errs)
	}
}

func TestToolStaleNamesAreReportedAndFailStrict(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "internal/x: TestX, TestRenamedAway", "M1-01", "High", "automated"))
	r.write("internal/x/x_test.go", goTest+"func TestX(t *testing.T) {}\n")
	code, out, errs := r.runTool("")
	if code != 0 || !strings.Contains(out, "1 listed test names do not exist yet") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if code, _, errs := r.runTool("", "-strict"); code != 1 || !strings.Contains(errs, "TestRenamedAway") {
		t.Fatalf("a stale name passed -strict: exit %d, %s", code, errs)
	}
}

func TestToolRefusesAReshapedBacklog(t *testing.T) {
	for name, text := range map[string]string{
		"eight columns":  "| SEC-001 | threat | §1 | comp | required | internal/x: TestX | M1-01 | High |\n",
		"duplicate id":   backlogRow("SEC-001", "internal/x: TestX", "M1-01", "High", "automated") + backlogRow("SEC-001", "internal/x: TestX", "M1-01", "High", "automated"),
		"no rows at all": "no table here\n",
	} {
		r := newFakeRepo(t, text)
		r.write("internal/x/x_test.go", goTest+"func TestX(t *testing.T) {}\n")
		if code, _, errs := r.runTool(""); code != 2 || errs == "" {
			t.Errorf("%s: exit %d, %q; want 2 with a message", name, code, errs)
		}
	}
}

func TestToolFailsWhenThereIsNothingToScan(t *testing.T) {
	r := newFakeRepo(t, backlogRow("SEC-001", "internal/x: TestX", "M1-01", "High", "automated"))
	if code, _, errs := r.runTool(""); code != 2 || !strings.Contains(errs, "no _test.go files") {
		t.Fatalf("a repository with no tests passed: exit %d, %s", code, errs)
	}
}

func TestParseTestsCell(t *testing.T) {
	got := parseTests("internal/identity: TestA, TestB; tests/security/identity: TestSecC (new, see section 7 gap G1, and more); " +
		"adapters/mcp: TestD; internal/apppkg: (extend) tar.gz traversal; docs/security/f.md: recorded result (new); docs/evidence/k.md (human)")
	type want struct {
		dir      string
		names    []string
		evidence bool
	}
	wants := []want{
		{"internal/identity", []string{"TestA", "TestB"}, false},
		{"tests/security/identity", []string{"TestSecC"}, false},
		{"adapters/mcp", []string{"TestD"}, false},
		{"internal/apppkg", nil, false},
		{"docs/security/f.md", nil, true},
		{"docs/evidence/k.md", nil, true},
	}
	if len(got) != len(wants) {
		t.Fatalf("got %d entries %+v, want %d", len(got), got, len(wants))
	}
	for i, w := range wants {
		g := got[i]
		if g.Dir != w.dir || g.Evidence != w.evidence || strings.Join(g.Names, ",") != strings.Join(w.names, ",") {
			t.Errorf("entry %d = %+v, want %+v", i, g, w)
		}
	}
}

// The real backlog parses: every id of section 4 is read, with nine columns each.
func TestRealBacklogParses(t *testing.T) {
	root, err := repoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseBacklog(filepath.Join(root, filepath.FromSlash(defaultBacklog)))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Rows) < 90 {
		t.Fatalf("parsed %d rows, the backlog has 97", len(b.Rows))
	}
	if len(b.Evidence["SEC-095"]) == 0 || len(b.Evidence["SEC-096"]) == 0 {
		t.Fatalf("section 6.2 evidence files not read: %v", b.Evidence)
	}
}
