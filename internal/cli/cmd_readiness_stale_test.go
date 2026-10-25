package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/report"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	rdystore "github.com/buxee/alix-ai/internal/readiness/store"
	corestore "github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// rdyCLIStoreLegacy stores, as the latest report under key, a copy of it as a readiness release from before the rules version
// was recorded wrote it: no evaluator_version, and an inputs hash over the files, the catalog and the options only.
func rdyCLIStoreLegacy(t *testing.T, b *rdyCLIBox, app string) readiness.Report {
	t.Helper()
	ctx := context.Background()
	opts := corestore.FromConfig()
	opts.Path = filepath.Join(b.root, "data", corestore.DefaultFileName)
	db, err := corestore.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := rdystore.RdyOpen(db, clock.Real{})
	key, err := rdystore.RdyAppKey(app)
	if err != nil {
		t.Fatal(err)
	}
	cur, ok, err := st.Latest(ctx, key)
	if err != nil || !ok {
		t.Fatalf("no latest report: %v", err)
	}
	res, err := scan.ScanDir(b.project, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.CatalogLoad()
	if err != nil {
		t.Fatal(err)
	}
	lim, _ := json.Marshal(res.Limits)
	h := sha256.New()
	fmt.Fprintf(h, "files:%s\ncatalog:%d:%s\nlimits:%s\n", res.InputsSHA256, res.CatalogVersion, cat.SHA256(), lim)

	raw, err := json.Marshal(cur)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "evaluator_version")
	m["inputs_sha256"] = hex.EncodeToString(h.Sum(nil))
	m["report_id"] = string(report.RdyNewReportID(ids.NewGenerator(clock.Real{})))
	m["previous_report_id"] = string(cur.ReportID)
	if raw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	var old readiness.Report
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(ctx, key, old); err != nil {
		t.Fatalf("an old report cannot be stored: %v", err)
	}
	return old
}

// A report stored by an older readiness release is stale even when the files are unchanged: status says so, in both
// languages, evaluates the project again with the current rules and stores a new report. The old report stays readable.
func TestReadinessStatusReevaluatesAReportFromOlderRules(t *testing.T) {
	for _, c := range []struct{ lang, want string }{
		{"en", "older version of the readiness rules"},
		{"it", "versione precedente delle regole di readiness"},
	} {
		t.Run(c.lang, func(t *testing.T) {
			b := rdyCLINewBox(t, "")
			b.mustRun("readiness", "scan", b.project, "-app", "clinic", "-no-model")
			old := rdyCLIStoreLegacy(t, b, "clinic")

			r := b.mustRun("readiness", "status", "-app", "clinic", "-no-model", "-lang", c.lang)
			if !strings.Contains(r.stderr, "older readiness rules") || !strings.Contains(r.stderr, string(old.ReportID)) {
				t.Errorf("stderr does not say the report is from older rules: %q", r.stderr)
			}
			if !strings.Contains(r.stdout, c.want) {
				t.Errorf("status does not say the report came from older rules:\n%s", r.stdout)
			}
			l := b.list("-app", "clinic")
			if len(l.Reports) != 3 || l.Reports[1].ReportID != string(old.ReportID) || l.Reports[0].InputsSHA256 == string(old.InputsSHA256) {
				t.Fatalf("no new report was stored with a new inputs hash: %+v", l.Reports)
			}
			// the new report is stored with the current rules version
			var cur struct {
				EvaluatorVersion int64 `json:"evaluator_version"`
			}
			decode(t, b.mustRun("readiness", "show", "-app", "clinic", "-format", "json").stdout, &cur)
			if cur.EvaluatorVersion != report.RdyEvaluatorVersion {
				t.Errorf("the new report reads back with evaluator_version %d, want %d", cur.EvaluatorVersion, report.RdyEvaluatorVersion)
			}
			// the old report still reads, and a second status finds the new one current
			if s := b.mustRun("readiness", "show", "-report", string(old.ReportID), "-app", "clinic"); !strings.Contains(s.stdout, string(old.ReportID)) {
				t.Errorf("the old report cannot be shown:\n%s%s", s.stdout, s.stderr)
			}
			if again := b.mustRun("readiness", "status", "-app", "clinic", "-no-model"); strings.Contains(again.stderr, "changed since report") || strings.Contains(again.stderr, "older readiness rules") {
				t.Errorf("a current report is re-evaluated again: %q", again.stderr)
			}
		})
	}
}
