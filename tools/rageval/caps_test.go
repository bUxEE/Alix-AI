package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/query"
)

// TestRAGCapsAreTheBudgetTable pins the rows of docs/operations/resource-budgets.md
// 6.3 that the harness applies, and that each of them is a configuration the
// pipeline accepts.
func TestRAGCapsAreTheBudgetTable(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		items, toks, cands, lookups, p int
		parents                        bool
	}{
		{CapsLite, 4, 1000, 24, 1, 300, true},
		{CapsBalanced, 6, 2000, 48, 2, 500, true},
		{CapsMinimal, 3, 480, 12, 0, 0, false},
	} {
		c := query.DefaultConfig()
		if err := applyRAGCaps(&c, tc.name); err != nil {
			t.Fatal(err)
		}
		if c.MaxItems != tc.items || c.MaxToks != tc.toks || c.FTSCandidates != tc.cands || c.VectorCandidates != tc.cands || c.ParentExpansion != tc.parents {
			t.Errorf("%s: %+v", tc.name, c)
		}
		if tc.parents && (c.MaxParentLookups != tc.lookups || c.MaxParentToks != tc.p) {
			t.Errorf("%s: parent lookups %d of %d est", tc.name, c.MaxParentLookups, c.MaxParentToks)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("%s is not a valid configuration: %v", tc.name, err)
		}
	}
	d := query.DefaultConfig()
	c := d
	if err := applyRAGCaps(&c, CapsDefault); err != nil || !reflect.DeepEqual(c, d) {
		t.Errorf("default caps changed the configuration: %v", err)
	}
	if err := applyRAGCaps(&c, "huge"); err == nil {
		t.Error("an unknown profile was accepted")
	}
	if got := capKs([]int{1, 3, 5, 8}, 4); !reflect.DeepEqual(got, []int{1, 3, 4}) {
		t.Errorf("cut-offs under 4 items: %v", got)
	}
	if got := capKs([]int{1, 3, 5, 8}, 8); !reflect.DeepEqual(got, []int{1, 3, 5, 8}) {
		t.Errorf("cut-offs under 8 items: %v", got)
	}
}

// TestRunWithLiteCaps runs the tool with the Lite package: the cut-offs follow
// the four items, the result records which caps it used, and a configuration
// that is not the one of the named caps is caught by the checker.
func TestRunWithLiteCaps(t *testing.T) {
	dir, rag, _ := writeRun(t, "--rag-caps", "lite")
	var r RAGResult
	b, _ := os.ReadFile(rag)
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Config.RAGCaps != "lite" || r.Config.Query.MaxItems != 4 || r.Config.Query.MaxToks != 1000 || !reflect.DeepEqual(r.Config.K, []int{1, 3, 4}) {
		t.Errorf("config %+v", r.Config)
	}
	for _, q := range r.Questions {
		if len(q.Ranked) > 4 {
			t.Fatalf("question %s: %d items in a Lite package", q.ID, len(q.Ranked))
		}
	}
	if c, out := checkDir(t, dir); c != 0 {
		t.Fatalf("check: exit %d\n%s", c, out)
	}
	mutate(t, rag, func(m map[string]any) { at(m, "config", "query")["max_items"] = 8.0 })
	if c, out := checkDir(t, dir); c == 0 || !strings.Contains(out, "is not that of the rag caps") {
		t.Errorf("a Lite result with an eight-item configuration passed: exit %d\n%s", c, out)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--embedder", "fake", "--rag-caps", "huge", "--corpus", corpusDir, "--out", t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Errorf("an unknown --rag-caps exited %d, want 2", code)
	}
	// An explicit --k above the package is still refused, with the reason.
	stderr.Reset()
	if code := run([]string{"run", "--embedder", "fake", "--rag-caps", "lite", "--k", "1,5", "--scale-sizes", "none", "--corpus", corpusDir, "--out", t.TempDir()}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "k 5") {
		t.Errorf("--k 1,5 with the Lite package exited %d: %s", code, stderr.String())
	}
}
