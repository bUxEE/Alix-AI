package assist

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/internal/readiness/report"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

var assistTestClock = clock.NewFake(time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC))

// assistCanary stands for a secret value. It is TEST-ONLY and not a real credential.
const assistCanary = "TESTONLY-canary-7f3a9c1e5b2d4086"

func assistScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: "app_readiness", TenantID: "ten_operator", WorkspaceID: "wsp_operator",
		PrincipalID: "prn_operator", Roles: []string{"operator"}, CapabilityScopes: []string{"readiness.scan"}, SessionID: "ses_test",
	}
}

// assistProject writes files into a temporary directory and scans it.
func assistProject(t *testing.T, files map[string]string) (string, *scan.ScanResult) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res, err := scan.ScanDir(root, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return root, res
}

func assistLocal(f *fake.Provider) *AssistModel {
	return &AssistModel{Name: "fake", ModelID: "fake-model-0", Local: true, Gen: f}
}

func assistExternal(f *fake.Provider) *AssistModel {
	return &AssistModel{Name: "hosted", ModelID: "hosted-model-0", Local: false, Gen: f}
}

// assistReply builds a reply document from items.
func assistReplyOf(items ...map[string]any) string {
	if items == nil {
		items = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"proposals": items})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// assistProp is one well-formed proposal item citing one span.
func assistProp(ids []string, category, status, path string, start, end int, quote string) map[string]any {
	return map[string]any{
		"requirement_ids": ids,
		"category":        category,
		"status":          status,
		"claim":           "The project describes this.",
		"evidence": []map[string]any{
			{"path": path, "line_start": start, "line_end": end, "quote": quote},
		},
		"remediation": "Document it in one place.",
	}
}

// assistRecFS wraps the real confined file system and records every open and lstat.
type assistRecFS struct {
	assistFS
	mu      sync.Mutex
	touched []string
}

func (r *assistRecFS) Lstat(name string) (fs.FileInfo, error) {
	r.mu.Lock()
	r.touched = append(r.touched, name)
	r.mu.Unlock()
	return r.assistFS.Lstat(name)
}

func (r *assistRecFS) Open(name string) (io.ReadCloser, error) {
	r.mu.Lock()
	r.touched = append(r.touched, name)
	r.mu.Unlock()
	return r.assistFS.Open(name)
}

func (r *assistRecFS) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.touched...)
}

// assistRunRec runs the engine with the given rules over a recording file system.
func assistRunRec(t *testing.T, root string, res *scan.ScanResult, cfg AssistConfig, rules assistRules) (*AssistOutcome, *assistRecFS) {
	t.Helper()
	real, err := assistOpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	rec := &assistRecFS{assistFS: real}
	out, err := assistRun(context.Background(), assistScope(), root, res, cfg, rules, rec)
	if err != nil {
		t.Fatalf("assistRun: %v", err)
	}
	return out, rec
}

// assistBuild builds the report for an outcome the way the CLI will.
func assistBuild(t *testing.T, root string, res *scan.ScanResult, out *AssistOutcome) *report.RdyReport {
	t.Helper()
	in := report.RdyBuildInput{
		Root: root, Scan: res,
		ReportID: report.RdyNewReportID(ids.NewGenerator(assistTestClock)), GeneratedAt: assistTestClock.Now(),
	}
	if out != nil {
		out.Apply(&in)
	}
	r, err := report.RdyBuild(in)
	if err != nil {
		t.Fatalf("RdyBuild: %v", err)
	}
	return r
}

// assistDeterministic is the report of the same project built with no model at all.
func assistDeterministic(t *testing.T, root string, res *scan.ScanResult) *report.RdyReport {
	t.Helper()
	return assistBuild(t, root, res, nil)
}

func assistStatuses(doc readiness.Report) map[readiness.RequirementID]readiness.Status {
	m := map[readiness.RequirementID]readiness.Status{}
	for _, it := range doc.Checklist {
		m[it.RequirementID] = it.Status
	}
	return m
}

// assistPromptTexts is everything that reached the model, across all calls.
func assistPromptTexts(f *fake.Provider) string {
	var b strings.Builder
	for _, p := range f.Prompts() {
		for _, s := range p.Texts() {
			b.WriteString(s)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func assistJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assistSameOutcome reports the differences between two reports' verdicts: checklist, summary and
// findings. A model that only proposes leaves all three untouched unless a proposal is verified.
func assistSameOutcome(a, b readiness.Report) []string {
	var out []string
	if assistMarshal(a.Checklist) != assistMarshal(b.Checklist) {
		out = append(out, "the checklist differs")
	}
	if assistMarshal(a.Summary) != assistMarshal(b.Summary) {
		out = append(out, "the summary differs")
	}
	if assistMarshal(a.Findings) != assistMarshal(b.Findings) {
		out = append(out, "the findings differ")
	}
	return out
}

func assistMarshal(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func mustCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.CatalogLoad()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func assistEqualStatuses(a, b map[readiness.RequirementID]readiness.Status) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
