package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/readiness/report"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	corestore "github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

var bg = context.Background()

// rdyCanary stands for a secret value in a fixture. It is TEST-ONLY and not a real credential.
const rdyCanary = "TESTONLY-canary-0000000000000000"

type rdyRig struct {
	t   *testing.T
	db  *corestore.DB
	st  *RdyStore
	clk *clock.Fake
	gen *ids.Generator
}

func rdyNewRig(t *testing.T) *rdyRig {
	t.Helper()
	db := storetest.Open(t, nil)
	clk := clock.NewFake(time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC))
	return &rdyRig{t: t, db: db, st: RdyOpen(db, clk), clk: clk, gen: ids.NewGenerator(clk)}
}

func rdyWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const rdyOpenAPI = "openapi: 3.0.3\ninfo:\n  title: A\n  version: 1.0.0\npaths:\n  /slots:\n    get:\n      operationId: listSlots\n      responses:\n        \"200\":\n          description: ok\n"

// build scans root and builds a report with a fresh id and the next minute.
func (r *rdyRig) build(root string) readiness.Report {
	r.t.Helper()
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	r.clk.Advance(time.Minute)
	rep, err := report.RdyBuild(report.RdyBuildInput{Root: root, Scan: res, ReportID: report.RdyNewReportID(r.gen), GeneratedAt: r.clk.Now()})
	if err != nil {
		r.t.Fatalf("RdyBuild: %v", err)
	}
	return rep.Doc
}

func (r *rdyRig) save(key RdyKey, doc readiness.Report) RdySaved {
	r.t.Helper()
	s, err := r.st.Save(bg, key, doc)
	if err != nil {
		r.t.Fatalf("Save: %v", err)
	}
	return s
}

func rdyStatuses(doc readiness.Report) map[readiness.RequirementID]readiness.Status {
	m := map[readiness.RequirementID]readiness.Status{}
	for _, it := range doc.Checklist {
		m[it.RequirementID] = it.Status
	}
	return m
}

func rdyHas(cs []RdyChange, id readiness.RequirementID) bool {
	for _, c := range cs {
		if c.RequirementID == id {
			return true
		}
	}
	return false
}

func TestStoreKeepsHistoryAndDiffsTwoReports(t *testing.T) {
	r := rdyNewRig(t)
	root := t.TempDir()
	// First state: a compose file and a Dockerfile that agree, and a README.
	rdyWrite(t, root, "README.md", "# Clinic\n\nThe clinic exposes appointments over MCP.\n")
	rdyWrite(t, root, "Dockerfile", "FROM example.invalid/node@sha256:0000000000000000000000000000000000000000000000000000000000000000\nEXPOSE 8787\n")
	rdyWrite(t, root, "docker-compose.yml", "services:\n  app:\n    build: .\n    ports:\n      - \"127.0.0.1:8787:8787\"\n")
	key, err := RdyProjectKey(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := r.st.Latest(bg, key); err != nil || ok {
		t.Fatalf("an empty store has a latest report: %v %v", ok, err)
	}
	first := r.build(root)
	s1 := r.save(key, first)
	if s1.PreviousReportID != nil || s1.InputsChanged {
		t.Fatalf("first save: %+v", s1)
	}

	// Second state: the compose files go, an OpenAPI document arrives.
	for _, f := range []string{"Dockerfile", "docker-compose.yml"} {
		if err := os.Remove(filepath.Join(root, f)); err != nil {
			t.Fatal(err)
		}
	}
	rdyWrite(t, root, "api/openapi.yaml", rdyOpenAPI)
	second := r.build(root)
	s2 := r.save(key, second)
	if s2.PreviousReportID == nil || *s2.PreviousReportID != first.ReportID || !s2.InputsChanged {
		t.Fatalf("second save: %+v", s2)
	}

	// An unchanged project gives the same inputs hash and says so.
	third := r.build(root)
	if third.InputsSHA256 != second.InputsSHA256 {
		t.Fatal("an unchanged project changed its inputs hash")
	}
	s3 := r.save(key, third)
	if s3.PreviousReportID == nil || *s3.PreviousReportID != second.ReportID || s3.InputsChanged {
		t.Fatalf("third save: %+v", s3)
	}

	// History, newest first, with the chain of previous ids and the counts.
	hist, err := r.st.History(bg, key, 0)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history %v %v", hist, err)
	}
	if hist[0].ReportID != third.ReportID || hist[1].ReportID != second.ReportID || hist[2].ReportID != first.ReportID {
		t.Fatalf("history order %v", hist)
	}
	if hist[2].PreviousReportID != nil || *hist[1].PreviousReportID != first.ReportID || !reflect.DeepEqual(hist[1].Summary, second.Summary) || hist[0].InputsSHA256 != string(third.InputsSHA256) {
		t.Fatalf("history entries %+v", hist)
	}
	if one, _ := r.st.History(bg, key, 1); len(one) != 1 || one[0].ReportID != third.ReportID {
		t.Fatalf("limited history %+v", one)
	}

	latest, ok, err := r.st.Latest(bg, key)
	if err != nil || !ok || latest.ReportID != third.ReportID || len(latest.Findings) != len(third.Findings) {
		t.Fatalf("latest %v %v", ok, err)
	}
	if latest.PreviousReportID == nil || *latest.PreviousReportID != second.ReportID {
		t.Fatalf("the stored report does not follow the previous one: %v", latest.PreviousReportID)
	}
	if got, ok, err := r.st.Get(bg, key, first.ReportID); err != nil || !ok || got.ReportID != first.ReportID || got.Mode != readiness.ReportModeDeterministicOnly {
		t.Fatalf("get %v %v", ok, err)
	}

	// The diff between the first and the second report.
	d, err := r.st.Diff(bg, key, first.ReportID, second.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.InputsChanged || d.CatalogChanged || d.FromID != first.ReportID || d.ToID != second.ReportID {
		t.Fatalf("diff header %+v", d)
	}
	if !rdyHas(d.NewlyMet, "req.openapi.description") {
		t.Errorf("the OpenAPI document is not newly met: %+v", d.NewlyMet)
	}
	if !rdyHas(d.NewlyMissing, "req.deployment.compose_and_ports") {
		t.Errorf("the removed compose file is not newly missing: %+v", d.NewlyMissing)
	}
	// Against an oracle: every requirement lands in exactly one bucket, by its two statuses.
	before, after := rdyStatuses(first), rdyStatuses(second)
	n := d.Unchanged
	for id, to := range after {
		from := before[id]
		switch {
		case from == to:
		case to == readiness.StatusFound:
			if !rdyHas(d.NewlyMet, id) {
				t.Errorf("%s %s->%s not in NewlyMet", id, from, to)
			}
			n++
		case to == readiness.StatusMissing:
			if !rdyHas(d.NewlyMissing, id) {
				t.Errorf("%s %s->%s not in NewlyMissing", id, from, to)
			}
			n++
		default:
			if !rdyHas(d.Changed, id) {
				t.Errorf("%s %s->%s not in Changed", id, from, to)
			}
			n++
		}
	}
	if n != len(after) {
		t.Errorf("the diff accounts for %d of %d requirements", n, len(after))
	}
	for _, c := range append(append(append([]RdyChange{}, d.NewlyMet...), d.NewlyMissing...), d.Changed...) {
		if c.From == c.To || c.From != before[c.RequirementID] || c.To != after[c.RequirementID] || c.CheckID == "" {
			t.Errorf("change %+v", c)
		}
	}
	// Two reports of the same inputs differ in nothing.
	if same, err := r.st.Diff(bg, key, second.ReportID, third.ReportID); err != nil || !same.Empty() || same.Unchanged != len(after) {
		t.Fatalf("diff of identical states: %+v %v", same, err)
	}
	if dl, ok, err := r.st.DiffLatest(bg, key); err != nil || !ok || dl.FromID != second.ReportID || dl.ToID != third.ReportID {
		t.Fatalf("DiffLatest %+v %v %v", dl, ok, err)
	}
	// The same pair the other way round reverses met and missing.
	rev, _ := r.st.Diff(bg, key, second.ReportID, first.ReportID)
	if !rdyHas(rev.NewlyMissing, "req.openapi.description") || !rdyHas(rev.NewlyMet, "req.deployment.compose_and_ports") && !rdyHas(rev.Changed, "req.deployment.compose_and_ports") {
		t.Errorf("reverse diff: %+v", rev)
	}

	// Status view: the latest with what changed since the one before.
	sv, ok, err := r.st.StatusView(bg, key)
	if err != nil || !ok || sv.ReportID != third.ReportID || sv.PreviousReportID == nil || *sv.PreviousReportID != second.ReportID || sv.InputsChanged || !sv.Advisory {
		t.Fatalf("status view %+v %v %v", sv, ok, err)
	}
	for _, it := range sv.Items {
		if it.PreviousStatus == nil || it.Changed {
			t.Errorf("status item %+v", it)
		}
	}
	d2 := RdyStatusView(second, &first, r.clk.Now())
	changed := 0
	for _, it := range d2.Items {
		if it.Changed {
			changed++
		}
	}
	if !d2.InputsChanged || changed != len(d.NewlyMet)+len(d.NewlyMissing)+len(d.Changed) {
		t.Errorf("status view changes %d, diff %d", changed, len(d.NewlyMet)+len(d.NewlyMissing)+len(d.Changed))
	}

	// A second key never sees the first one's reports, and cannot diff them.
	other, _ := RdyAppKey("app_other")
	if _, ok, _ := r.st.Latest(bg, other); ok {
		t.Fatal("another key sees a report")
	}
	if _, ok, _ := r.st.Get(bg, other, first.ReportID); ok {
		t.Fatal("another key reads a report by id")
	}
	if _, err := r.st.Diff(bg, other, first.ReportID, second.ReportID); err == nil {
		t.Fatal("another key diffed reports it does not own")
	}
	if _, err := r.st.Diff(bg, key, first.ReportID, "rrpt_00000000000000000000000000"); err == nil {
		t.Fatal("a diff against an unknown report succeeded")
	}
	storetest.AssertScopedTables(t, r.db)
}

func TestHistoryIsBoundedAndTheOldestIsPruned(t *testing.T) {
	r := rdyNewRig(t)
	root := t.TempDir()
	rdyWrite(t, root, "README.md", "# Clinic\n\nThe clinic exposes appointments over MCP.\n")
	key, _ := RdyAppKey("app_clinic")
	base := r.build(root)
	var firstID common.TypedID
	for i := 0; i < RdyMaxHistory+3; i++ {
		d := base
		d.ReportID = report.RdyNewReportID(r.gen)
		if i == 0 {
			firstID = d.ReportID
		}
		s := r.save(key, d)
		if i >= RdyMaxHistory && s.Pruned != 1 {
			t.Fatalf("save %d pruned %d", i, s.Pruned)
		}
	}
	hist, err := r.st.History(bg, key, 0)
	if err != nil || len(hist) != RdyMaxHistory {
		t.Fatalf("history length %d %v", len(hist), err)
	}
	if _, ok, _ := r.st.Get(bg, key, firstID); ok {
		t.Fatal("the oldest report was kept")
	}
	// A repeated id is refused, not duplicated.
	d := base
	d.ReportID = hist[0].ReportID
	if _, err := r.st.Save(bg, key, d); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Fatalf("a repeated report id: %v", err)
	}
}

func TestKeysAreStableAndHostilePathsAreHarmless(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	k1, err := RdyProjectKey(sub)
	if err != nil {
		t.Fatal(err)
	}
	for _, same := range []string{sub + string(filepath.Separator), filepath.Join(sub, "..", "proj"), filepath.Join(sub, ".")} {
		if k, err := RdyProjectKey(same); err != nil || k != k1 {
			t.Errorf("%q gives %q (%v), want %q", same, k, err, k1)
		}
	}
	if k2, _ := RdyProjectKey(filepath.Join(dir, "other")); k2 == k1 {
		t.Error("two directories share a key")
	}
	if !rdyProjectRE.MatchString(string(k1)) {
		t.Errorf("key shape %q", k1)
	}
	for _, bad := range []string{"", "   ", "a\x00b"} {
		if _, err := RdyProjectKey(bad); err == nil {
			t.Errorf("RdyProjectKey(%q) succeeded", bad)
		}
	}
	// Whatever the path is, the key is a hash: no part of the path survives in it.
	hostile := filepath.Join(dir, "x'; DROP TABLE readiness_reports;--\n"+strings.Repeat("é", 300))
	if k, err := RdyProjectKey(hostile); err != nil || !rdyProjectRE.MatchString(string(k)) {
		t.Errorf("hostile path key %q %v", k, err)
	}
	for _, bad := range []string{"", "a b", "x'; DROP TABLE readiness_reports;--", "../x", "app\n", strings.Repeat("a", 65), string(k1)} {
		if _, err := RdyAppKey(bad); err == nil {
			t.Errorf("RdyAppKey(%q) succeeded", bad)
		}
	}
	if k, err := RdyAppKey("app_crm-2.0"); err != nil || k != "app_crm-2.0" {
		t.Errorf("RdyAppKey: %q %v", k, err)
	}

	// A hostile directory name travels as display text only and is cleaned.
	r := rdyNewRig(t)
	root := t.TempDir()
	rdyWrite(t, root, "README.md", "# Clinic\n")
	res, _ := scan.ScanDir(root, scan.ScanOptions{})
	rep, err := report.RdyBuild(report.RdyBuildInput{Root: root, ScannedRoot: "/srv/\x00evil\n'); DROP TABLE x;--", Scan: res, ReportID: report.RdyNewReportID(r.gen), GeneratedAt: r.clk.Now()})
	if err != nil {
		t.Fatal(err)
	}
	r.save(k1, rep.Doc)
	got, _, _ := r.st.Latest(bg, k1)
	if strings.ContainsAny(got.ScannedRoot, "\x00\n") || !strings.Contains(got.ScannedRoot, "DROP TABLE x") {
		t.Errorf("scanned_root %q", got.ScannedRoot)
	}
	var n int
	if err := r.db.QueryRowContext(bg, "SELECT count(*) FROM readiness_reports").Scan(&n); err != nil || n != 1 {
		t.Fatalf("table state %d %v", n, err)
	}
	// An invalid key is refused everywhere.
	if _, err := r.st.Save(bg, "", rep.Doc); err == nil {
		t.Error("Save accepted an empty key")
	}
	if _, _, err := r.st.Latest(bg, "x'; --"); err == nil {
		t.Error("Latest accepted a malformed key")
	}
}

func TestStoreRefusesAReportItCannotStandBehind(t *testing.T) {
	r := rdyNewRig(t)
	root := t.TempDir()
	rdyWrite(t, root, "README.md", "# Clinic\n\nThe clinic exposes appointments over MCP.\n")
	rdyWrite(t, root, "api/openapi.yaml", rdyOpenAPI)
	good := r.build(root)
	key, _ := RdyAppKey("app_clinic")
	covered := -1
	for i, it := range good.Checklist {
		if it.Status == readiness.StatusFound {
			covered = i
			break
		}
	}
	if covered < 0 {
		t.Fatal("no covered requirement in the fixture")
	}
	clone := func() readiness.Report {
		d := good
		d.Checklist = append([]readiness.ReportChecklistItem(nil), good.Checklist...)
		d.Findings = append([]readiness.Finding(nil), good.Findings...)
		return d
	}
	cases := map[string]func(*readiness.Report){
		"not advisory":                 func(d *readiness.Report) { d.Advisory = false },
		"summary that does not add up": func(d *readiness.Report) { d.Summary.Found++ },
		"a covered item with no finding behind it": func(d *readiness.Report) {
			it := d.Checklist[covered]
			it.FindingIDs = nil
			d.Checklist[covered] = it
		},
		"a covered item resting on a model_unverified finding": func(d *readiness.Report) {
			it := d.Checklist[covered]
			for _, id := range it.FindingIDs {
				for i, f := range d.Findings {
					if f.FindingID == id {
						// Unverified, with its reason, so the per-finding rules pass and only the
						// guard on the checklist entry can refuse the report.
						reason := readiness.Text("The evidence did not confirm.")
						f.Source, f.Status, f.Evidence, f.UnverifiedReason = readiness.SourceModelUnverified, readiness.StatusUnverified, nil, &reason
						d.Findings[i] = f
					}
				}
			}
		},
		"a found finding with no evidence": func(d *readiness.Report) {
			for i, f := range d.Findings {
				if f.Status == readiness.StatusFound {
					f.Evidence = nil
					d.Findings[i] = f
					return
				}
			}
		},
		"a path that leaves the root": func(d *readiness.Report) {
			for i, f := range d.Findings {
				if len(f.Evidence) > 0 {
					f.Evidence = append([]readiness.FindingEvidence(nil), f.Evidence...)
					f.Evidence[0].Path = "../../etc/passwd"
					d.Findings[i] = f
					return
				}
			}
		},
		"a model without the mode": func(d *readiness.Report) {
			d.Model = &readiness.ReportModel{ModelID: "m", Runtime: readiness.ReportModelRuntimeLocal, Qualification: readiness.ReportModelQualificationUnqualified}
		},
		"a not-applicable count that does not add up": func(d *readiness.Report) {
			n := *d.Summary.NotApplicable + 1
			d.Summary.NotApplicable = &n
		},
		"a not-applicable finding": func(d *readiness.Report) { d.Findings[0].Status = readiness.StatusNotApplicable },
		"a not-applicable item outside the transports": func(d *readiness.Report) {
			for i, it := range d.Checklist {
				if it.Status == readiness.StatusMissing && it.Category == readiness.CategoryDeployment {
					d.Checklist[i].Status, d.Summary.Missing = readiness.StatusNotApplicable, d.Summary.Missing-1
					n := *d.Summary.NotApplicable + 1
					d.Summary.NotApplicable = &n
					return
				}
			}
			t.Fatal("no missing deployment item in the fixture")
		},
		"a not-applicable transport with no transport met": func(d *readiness.Report) {
			for i, it := range d.Checklist {
				if it.RequirementID == "req.openapi.description" {
					d.Checklist[i].Status, d.Summary.Found, d.Summary.Missing = readiness.StatusMissing, d.Summary.Found-1, d.Summary.Missing+1
					d.Checklist[i].FindingIDs = nil
				}
			}
		},
		"a malformed id": func(d *readiness.Report) { d.ReportID = "not-an-id" },
		"a long text": func(d *readiness.Report) {
			t := readiness.Text(strings.Repeat("x", 1001))
			d.Findings[0].Summary = &t
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			d := clone()
			mut(&d)
			if _, err := r.st.Save(bg, key, d); err == nil {
				t.Fatal("Save accepted it")
			}
			if hist, _ := r.st.History(bg, key, 0); len(hist) != 0 {
				t.Fatal("a refused report was stored")
			}
		})
	}
	if _, err := r.st.Save(bg, key, good); err != nil {
		t.Fatalf("the unmodified report is refused: %v", err)
	}
}

func TestNoFileContentOrSecretReachesTheDatabase(t *testing.T) {
	r := rdyNewRig(t)
	root := t.TempDir()
	// A line nothing cites, a line a finding cites, and a secret file.
	const uncited = "Internal note: the Zanzibar rollout window is every second Tuesday."
	rdyWrite(t, root, "README.md", "# Clinic\n\n"+uncited+"\nThe clinic exposes appointments over MCP.\n")
	rdyWrite(t, root, "api/openapi.yaml", rdyOpenAPI+"# api_key: "+rdyCanary+"\n")
	rdyWrite(t, root, ".env", "API_PASSWORD="+rdyCanary+"\n")
	key, _ := RdyProjectKey(root)
	doc := r.build(root)

	// A producer that let a secret into a summary is scrubbed on the way in.
	s := readiness.Text("found password = " + rdyCanary + " in the file")
	doc.Findings = append([]readiness.Finding(nil), doc.Findings...)
	doc.Findings[0].Summary = &s
	r.save(key, doc)
	got, _, _ := r.st.Latest(bg, key)
	if strings.Contains(string(*got.Findings[0].Summary), rdyCanary) || !strings.Contains(string(*got.Findings[0].Summary), scan.ScanRedacted) {
		t.Fatalf("summary was not scrubbed: %q", *got.Findings[0].Summary)
	}

	// Neither the canary nor the uncited line is in the database file or its write-ahead log.
	for i, p := range []string{r.db.Path(), r.db.Path() + "-wal"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			if i == 0 {
				t.Fatalf("the database file cannot be read, so nothing was checked: %v", err)
			}
			continue
		}
		for _, secret := range []string{rdyCanary, "Zanzibar", "second Tuesday"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("%s holds %q", filepath.Base(p), secret)
			}
		}
	}
	var raw string
	if err := r.db.QueryRowContext(bg, "SELECT document FROM readiness_reports LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "Zanzibar") || strings.Contains(raw, rdyCanary) {
		t.Fatal("the stored document holds file content")
	}
}

func TestADamagedRowIsAnErrorNotAReport(t *testing.T) {
	r := rdyNewRig(t)
	root := t.TempDir()
	rdyWrite(t, root, "README.md", "# Clinic\n")
	key, _ := RdyAppKey("app_clinic")
	doc := r.build(root)
	r.save(key, doc)
	for name, stmt := range map[string]string{
		"an unknown member": `UPDATE readiness_reports SET document = json_set(document, '$.surprise', 1)`,
		"a swapped hash":    `UPDATE readiness_reports SET inputs_sha256 = '` + strings.Repeat("0", 64) + `'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.db.ExecContext(bg, stmt); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.st.Latest(bg, key); err == nil {
				t.Fatal("a damaged row was returned as a report")
			}
		})
		// Restore for the next case.
		if _, err := r.db.ExecContext(bg, `DELETE FROM readiness_reports`); err != nil {
			t.Fatal(err)
		}
		doc.ReportID = report.RdyNewReportID(r.gen)
		r.save(key, doc)
	}
}

func TestAHostileProjectCannotMakeItsReportUnstorable(t *testing.T) {
	r := rdyNewRig(t)
	root := t.TempDir()
	long := strings.Repeat("a", 1500)
	api := func(path, verb string) string {
		return strings.Join([]string{"openapi: 3.0.3", "info:", "  title: A", "  version: 1.0.0", "paths:", "  /" + path + ":", "    " + verb + ":", "      operationId: " + long, "      responses:", "        \"200\":", "          description: ok"}, "\n") + "\n"
	}
	rdyWrite(t, root, "api/a.yaml", api(long+"1", "get"))
	rdyWrite(t, root, "api/b.yaml", api(long+"2", "post"))
	doc := r.build(root)
	conflicted := false
	for _, it := range doc.Checklist {
		conflicted = conflicted || it.Status == readiness.StatusContradicted
	}
	if !conflicted {
		t.Skip("the scanner did not read the long operation ids, so there is no conflict to bound")
	}
	key, _ := RdyProjectKey(root)
	r.save(key, doc)
}

// ---- the model leg (M8-04) ----

func TestModelLegAndLabelSurviveTheStoreAndAreValidated(t *testing.T) {
	r := rdyNewRig(t)
	root := t.TempDir()
	rdyWrite(t, root, "README.md", "# Clinic\n\nThe clinic exposes appointments over MCP.\n")
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("0f", 32)
	build := func(mut func(*report.RdyBuildInput)) readiness.Report {
		r.clk.Advance(time.Minute)
		in := report.RdyBuildInput{Root: root, Scan: res, ReportID: report.RdyNewReportID(r.gen), GeneratedAt: r.clk.Now()}
		mut(&in)
		rep, err := report.RdyBuild(in)
		if err != nil {
			t.Fatalf("RdyBuild: %v", err)
		}
		return rep.Doc
	}
	ran := build(func(in *report.RdyBuildInput) {
		in.Model = &report.RdyModel{ModelID: "lite-controller-0", Runtime: readiness.ReportModelRuntimeLocal}
		in.ModelLeg = &report.RdyModelLeg{Status: readiness.ReportModelLegStatusRan, PromptTemplateSHA256: digest, Calls: 1, Received: 3, Dropped: 2, AttemptedOverrides: 2}
	})
	key, _ := RdyAppKey("app_clinic")
	r.save(key, ran)
	got, ok, err := r.st.Latest(bg, key)
	if err != nil || !ok {
		t.Fatalf("Latest: %v %v", ok, err)
	}
	if got.ModelLeg == nil || got.ModelLeg.DroppedProposals != 2 || got.ModelLeg.AttemptedOverrides != 2 || string(*got.ModelLeg.PromptTemplateSHA256) != digest ||
		got.Model.Label == nil || *got.Model.Label != readiness.ReportModelLabelUnqualifiedAdvisory {
		t.Fatalf("the stored report lost the leg: %+v", got.ModelLeg)
	}

	notRun := build(func(in *report.RdyBuildInput) {
		in.ModelLeg = &report.RdyModelLeg{Status: readiness.ReportModelLegStatusRefused}
	})
	r.save(key, notRun)

	// The store refuses a leg that contradicts the rest of the report.
	bad := map[string]func(*readiness.Report){
		"a label that disagrees with the qualification": func(d *readiness.Report) {
			m := *d.Model
			l := readiness.ReportModelLabelQualifiedAdvisory
			m.Label = &l
			d.Model = &m
		},
		"a leg that ran in a deterministic_only report": func(d *readiness.Report) {
			d.Mode, d.Model = readiness.ReportModeDeterministicOnly, nil
		},
		"a leg that did not run in a model report": func(d *readiness.Report) {
			l := *d.ModelLeg
			l.Status = readiness.ReportModelLegStatusFailed
			d.ModelLeg = &l
		},
		"more overrides than drops": func(d *readiness.Report) {
			l := *d.ModelLeg
			l.AttemptedOverrides = l.DroppedProposals + 1
			d.ModelLeg = &l
		},
		"a negative count": func(d *readiness.Report) {
			l := *d.ModelLeg
			l.ProposalsReceived = -1
			d.ModelLeg = &l
		},
		"a malformed digest": func(d *readiness.Report) {
			l := *d.ModelLeg
			s := readiness.SHA256("XYZ")
			l.PromptTemplateSHA256 = &s
			d.ModelLeg = &l
		},
		"a leg that ran without a digest": func(d *readiness.Report) {
			l := *d.ModelLeg
			l.PromptTemplateSHA256 = nil
			d.ModelLeg = &l
		},
		"an unknown status": func(d *readiness.Report) {
			l := *d.ModelLeg
			l.Status = "certified"
			d.ModelLeg = &l
		},
	}
	for name, mut := range bad {
		t.Run(name, func(t *testing.T) {
			d := build(func(in *report.RdyBuildInput) {
				in.Model = &report.RdyModel{ModelID: "lite-controller-0", Runtime: readiness.ReportModelRuntimeLocal}
				in.ModelLeg = &report.RdyModelLeg{Status: readiness.ReportModelLegStatusRan, PromptTemplateSHA256: digest, Calls: 1, Received: 3, Dropped: 2, AttemptedOverrides: 1}
			})
			mut(&d)
			if _, err := r.st.Save(bg, key, d); err == nil {
				t.Fatal("Save accepted it")
			}
		})
	}
}
