package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/buxee/alix-ai/internal/readiness/catalog"
)

// A report records the rules version that made it, and that version is part of its inputs hash: a report written before the
// version was recorded counts as version 1 and does not hash like a current one over the same files.
func TestReportRecordsTheRulesVersionInItsInputs(t *testing.T) {
	root, res := rdyProject(t)
	doc := rdyBuild(t, root, res, nil).Doc
	if doc.EvaluatorVersion == nil || *doc.EvaluatorVersion != RdyEvaluatorVersion || RdyReportEvaluatorVersion(doc) != RdyEvaluatorVersion {
		t.Fatalf("evaluator_version = %v, want %d", doc.EvaluatorVersion, RdyEvaluatorVersion)
	}
	cat, err := catalog.CatalogLoad()
	if err != nil {
		t.Fatal(err)
	}
	// the inputs hash a release without the rules version wrote
	lim, _ := json.Marshal(res.Limits)
	h := sha256.New()
	fmt.Fprintf(h, "files:%s\ncatalog:%d:%s\nlimits:%s\n", res.InputsSHA256, res.CatalogVersion, cat.SHA256(), lim)
	if string(doc.InputsSHA256) == hex.EncodeToString(h.Sum(nil)) {
		t.Error("the inputs hash does not depend on the rules version")
	}
	old := doc
	old.EvaluatorVersion = nil
	if RdyReportEvaluatorVersion(old) != 1 {
		t.Errorf("a report without a rules version is version %d, want 1", RdyReportEvaluatorVersion(old))
	}
}
