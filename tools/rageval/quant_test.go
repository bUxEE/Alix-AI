package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTopKOverlap(t *testing.T) {
	// Reference ranks 0, 1, 2, 3; the other ranks 0, 2, 3, 1: of the reference's
	// top two {0, 1} the other holds {0, 2}, so half.
	a := []float64{0.9, 0.8, 0.7, 0.1}
	b := []float64{0.9, 0.1, 0.8, 0.7}
	if got := topKOverlap(a, b, 2); got != 0.5 {
		t.Errorf("overlap@2 = %v, want 0.5", got)
	}
	if got := topKOverlap(a, a, 3); got != 1 {
		t.Errorf("a ranking against itself = %v, want 1", got)
	}
	if got := topKOverlap(a, b, 4); got != 1 {
		t.Errorf("k at the size of the set must be 1, got %v", got)
	}
}

// randomUnitish returns n vectors of dim dimensions of Gaussian components.
func randomVectors(rng *rand.Rand, n, dim int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		out[i] = v
	}
	return out
}

// TestQuantCompareOnRandomVectors: on vectors with no structure (ADR 0011's
// hardest case for quantisation) the int8 scan keeps the exact top ten, with a
// score error far below the gap between neighbours.
func TestQuantCompareOnRandomVectors(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	docs, queries := randomVectors(rng, 400, 384), randomVectors(rng, 30, 384)
	s, err := quantCompare("random", "gaussian", docs, queries)
	if err != nil {
		t.Fatal(err)
	}
	if s.Dim != 384 || s.Vectors != 400 || s.Queries != 30 || s.K != 10 || len(s.RecallAtK) != 30 {
		t.Fatalf("set %+v", s)
	}
	if s.Mean == nil || *s.Mean < quantBar || !s.MeetsBar {
		t.Errorf("mean recall@10 %v on unstructured vectors: below ADR 0011's bar of %v", s.Mean, quantBar)
	}
	if s.MaxAbsScoreErr > 0.02 || s.MeanAbsScoreErr <= 0 {
		t.Errorf("score error mean %v max %v", s.MeanAbsScoreErr, s.MaxAbsScoreErr)
	}
	// The aggregates are the ones the checker derives.
	if got := quantSummary(s); got.Perfect != s.Perfect || *got.Mean != *s.Mean || *got.Min != *s.Min {
		t.Errorf("summary %+v differs from %+v", got, s)
	}
}

// TestQuantCompareCanFail is the negative control: vectors whose only difference
// is far below one quantisation step (one large component, the rest at a
// thousandth of it) quantise to the same code, so the quantised scan cannot tell
// them apart and the check reports a recall far under the bar. A check that cannot
// fail would measure nothing.
func TestQuantCompareCanFail(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	mk := func(n int) [][]float32 {
		out := make([][]float32, n)
		for i := range out {
			v := make([]float32, 64)
			v[0] = 1
			for j := 1; j < len(v); j++ {
				v[j] = float32(rng.NormFloat64()) * 0.001
			}
			out[i] = v
		}
		return out
	}
	s, err := quantCompare("crushed", "one dominant component", mk(200), mk(20))
	if err != nil {
		t.Fatal(err)
	}
	if s.MeetsBar || *s.Mean > 0.5 {
		t.Errorf("mean recall@10 %v on vectors the int8 form cannot separate: the check did not notice", *s.Mean)
	}
}

func TestQuantCompareRefusesBadInput(t *testing.T) {
	good := [][]float32{{1, 2, 3}, {3, 2, 1}}
	for name, tc := range map[string]struct{ docs, qs [][]float32 }{
		"no documents":         {nil, good},
		"no queries":           {good, nil},
		"mixed dimensions":     {good, [][]float32{{1, 2}}},
		"an all-zero document": {[][]float32{{0, 0, 0}, {1, 2, 3}}, good},
		"a NaN query":          {good, [][]float32{{float32(math.NaN()), 1, 1}}},
	} {
		if _, err := quantCompare(name, "x", tc.docs, tc.qs); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestRunQuantCheckOnTheLabelledCorpus runs both sets with the fake embedder
// (microseconds a text) and checks the shape; the figures of a real model are
// the live run's.
func TestRunQuantCheckOnTheLabelledCorpus(t *testing.T) {
	corpus := loadTestCorpus(t)
	q, err := runQuantCheck(t.Context(), newHashEmbedder(), "fake", corpus, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if q.Embedder != "fake" || q.K != 10 || q.Bar != 0.95 || len(q.Sets) != 2 {
		t.Fatalf("quantisation %+v", q)
	}
	a, b := q.Sets[0], q.Sets[1]
	if a.Name != "labelled-corpus" || a.Queries != len(corpus.Questions) || a.Vectors < 100 || a.Dim != fakeDim {
		t.Errorf("labelled set %+v", a)
	}
	if b.Name != "synthetic-scale-text" || b.Vectors != 1000 || b.Queries != 100 {
		t.Errorf("synthetic set %+v", b)
	}
	if ps := verifyQuantization(q, "fake"); len(ps) != 0 {
		t.Errorf("a fresh check does not verify: %v", ps)
	}
	if ps := verifyQuantization(q, "localai"); len(ps) == 0 {
		t.Error("a check of the fake's vectors in a localai run verified")
	}
	q.Sets[0].Perfect++
	if ps := verifyQuantization(q, "fake"); len(ps) == 0 || !strings.Contains(ps[0], "do not match") {
		t.Errorf("a tampered count verified: %v", ps)
	}
}

// TestLocalAIQuantCheckEndToEnd: the flag through the command line against the
// mock, the result passes its check, and a hand-edited recall is caught.
func TestLocalAIQuantCheckEndToEnd(t *testing.T) {
	var calls atomic.Int64
	srv := mockLocalAI(t, &calls)
	code, dir, r, stderr := runMockedLocalAI(t, srv, "--scale-sizes", "none", "--quant-check")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if r.Quantization == nil || r.Quantization.Embedder != "localai" || len(r.Quantization.Sets) != 2 {
		t.Fatalf("quantization %+v", r.Quantization)
	}
	if c, out := checkDir(t, dir); c != 0 {
		t.Fatalf("check: exit %d\n%s", c, out)
	}
	mutate(t, ragFile(t, dir), func(m map[string]any) {
		s := at(m, "quantization")["sets"].([]any)[0].(map[string]any)
		s["recall_at_k"].([]any)[0] = 0.0
	})
	if c, out := checkDir(t, dir); c == 0 || !strings.Contains(out, "do not match the per-question recalls") {
		t.Errorf("a hand-edited recall passed: exit %d\n%s", c, out)
	}
	// And the flag is refused where there is no model.
	if code := run([]string{"run", "--embedder", "localai", "--mode", "fts", "--quant-check", "--corpus", corpusDir, "--out", t.TempDir()}, os.Stdout, os.Stderr); code != 2 {
		t.Errorf("--quant-check with --mode fts exited %d, want 2", code)
	}
}

// TestQuantDocsSet: a directory of Markdown becomes a third, natural-language
// set (its chunks and its headings), by file name, bounded; a directory with too
// little in it is refused.
func TestQuantDocsSet(t *testing.T) {
	dir := t.TempDir()
	var sb strings.Builder
	for f := 0; f < 3; f++ {
		sb.Reset()
		for s := 0; s < 8; s++ {
			fmt.Fprintf(&sb, "## Section %d of file %d\n\n", s, f)
			for p := 0; p < 3; p++ {
				fmt.Fprintf(&sb, "Paragraph %d of section %d in file %d says that the retention period is %d days and the owner is team %d.\n\n", p, s, f, 10*s+p, f)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.md", f)), []byte(sb.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("not markdown"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Each section is one chunk: 24 in all, bounded by the cap and by the files.
	texts, queries, err := docsQuantInputs(t.Context(), dir, 12, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) != 12 || len(queries) != 12 {
		t.Fatalf("%d texts and %d queries", len(texts), len(queries))
	}
	q, err := runQuantCheck(t.Context(), newHashEmbedder(), "fake", loadTestCorpus(t), 1, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sets) != 3 || q.Sets[1].Name != "markdown-docs" || q.Sets[1].Vectors != 24 {
		t.Errorf("sets %+v", q.Sets)
	}
	if ps := verifyQuantization(q, "fake"); len(ps) != 0 {
		t.Errorf("does not verify: %v", ps)
	}
	if _, _, err := docsQuantInputs(t.Context(), t.TempDir(), 40, 100); err == nil {
		t.Error("an empty directory was accepted")
	}
}
