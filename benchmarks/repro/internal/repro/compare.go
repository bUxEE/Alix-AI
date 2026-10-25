package repro

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Class says what a finding is about, and therefore what a deviation means.
type Class string

const (
	// ClassComparable findings say the two results were not measured on the same
	// inputs, so no metric of them may be compared. Always a failure.
	ClassComparable Class = "comparability"
	// ClassQuality findings compare pass rates and error rates. A deviation beyond
	// tolerance is a failure: the published quality did not reproduce.
	ClassQuality Class = "quality"
	// ClassLatency and ClassMemory depend on the host. They are always reported;
	// they fail the comparison only under StrictPerf.
	ClassLatency Class = "latency"
	ClassMemory  Class = "memory"
	// ClassMissing is a published run that has no reproduced result.
	ClassMissing Class = "missing"
)

// Finding is one compared quantity.
type Finding struct {
	Class      Class   `json:"class"`
	Name       string  `json:"name"`
	Published  string  `json:"published"`
	Reproduced string  `json:"reproduced"`
	Delta      float64 `json:"delta"`
	Limit      float64 `json:"limit"`
	Flagged    bool    `json:"flagged"`
}

// CompareOptions tune a comparison.
type CompareOptions struct {
	Tol Tolerances
	// StrictPerf makes latency and memory deviations fail the comparison.
	StrictPerf bool
}

// Comparison is the outcome for one run.
type Comparison struct {
	RunID    string    `json:"run_id"`
	Findings []Finding `json:"findings"`
}

// Failed counts the findings that fail the comparison.
func (c Comparison) Failed(strictPerf bool) int {
	n := 0
	for _, f := range c.Findings {
		if !f.Flagged {
			continue
		}
		if (f.Class == ClassLatency || f.Class == ClassMemory) && !strictPerf {
			continue
		}
		n++
	}
	return n
}

// Flagged counts every finding beyond tolerance, whatever its class.
func (c Comparison) Flagged() int {
	n := 0
	for _, f := range c.Findings {
		if f.Flagged {
			n++
		}
	}
	return n
}

// Compare judges a reproduced result against a published one. When the two were
// not measured on the same inputs it reports only that, because comparing their
// metrics would present a difference of setup as a difference of behaviour.
func Compare(pub, rep *Result, o CompareOptions) []Finding {
	var fs []Finding
	same := func(name string, p, r any) {
		ps, rs := fmt.Sprint(p), fmt.Sprint(r)
		fs = append(fs, Finding{Class: ClassComparable, Name: name, Published: ps, Reproduced: rs, Flagged: ps != rs})
	}
	same("corpus sha256", pub.Corpus.SHA256, rep.Corpus.SHA256)
	same("corpus cases run", pub.Corpus.CasesRun, rep.Corpus.CasesRun)
	same("prompt sha256", pub.Config.Prompt.SHA256, rep.Config.Prompt.SHA256)
	same("constraint", pub.Config.Constraint, rep.Config.Constraint)
	same("role", pub.Config.Role, rep.Config.Role)
	same("max_toks", pub.Config.MaxToks, rep.Config.MaxToks)
	same("context", pub.Context.ConfiguredToks, rep.Context.ConfiguredToks)
	same("model", pub.Model.RegistryID, rep.Model.RegistryID)
	same("model artifact sha256", pub.Model.ArtifactSHA256, rep.Model.ArtifactSHA256)
	same("profile", pub.Profile, rep.Profile)
	same("emulated", pub.Emulated, rep.Emulated)
	same("backend", pub.Backend.Name, rep.Backend.Name)
	same("runtime version", pub.Backend.RuntimeVersion, rep.Backend.RuntimeVersion)
	same("synthetic", pub.Synthetic, rep.Synthetic)
	if rep.Synthetic {
		// A synthetic result measures the harness; it never reproduces anything.
		fs = append(fs, Finding{Class: ClassComparable, Name: "reproduced result is synthetic", Published: "false", Reproduced: "true", Flagged: true})
	}
	if rep.Config.Limit != nil {
		fs = append(fs, Finding{Class: ClassComparable, Name: "reproduced run covers part of the corpus (limit)", Published: "none", Reproduced: fmt.Sprint(*rep.Config.Limit), Flagged: true})
	}
	for _, f := range fs {
		if f.Flagged {
			return fs
		}
	}

	pm, rm := pub.Metrics.named(), rep.Metrics.named()
	for i := range pm {
		fs = append(fs, compareMetric(pm[i].name, pm[i].m, rm[i].m, o.Tol.QualityRate))
	}
	pe, re := errRate(pub), errRate(rep)
	fs = append(fs, Finding{Class: ClassQuality, Name: "errors (rate of cases)",
		Published: fmt.Sprintf("%d", pub.Errors.Count), Reproduced: fmt.Sprintf("%d", rep.Errors.Count),
		Delta: re - pe, Limit: o.Tol.QualityRate, Flagged: math.Abs(re-pe) > o.Tol.QualityRate})

	rel := func(class Class, name string, p, r, limit float64) {
		f := Finding{Class: class, Name: name, Published: fmt.Sprintf("%.1f", p), Reproduced: fmt.Sprintf("%.1f", r), Limit: limit}
		if p <= 0 {
			f.Flagged = r > 0 // a published zero has no relative scale
		} else {
			f.Delta = (r - p) / p
			f.Flagged = math.Abs(f.Delta) > limit
		}
		fs = append(fs, f)
	}
	if pub.Latency.TotalMS.N > 0 && rep.Latency.TotalMS.N > 0 {
		rel(ClassLatency, "latency total p50 (ms)", pub.Latency.TotalMS.P50, rep.Latency.TotalMS.P50, o.Tol.LatencyRel)
		rel(ClassLatency, "latency total p95 (ms)", pub.Latency.TotalMS.P95, rep.Latency.TotalMS.P95, o.Tol.LatencyRel)
	}
	if pub.Latency.TTFTMS.N > 0 && rep.Latency.TTFTMS.N > 0 {
		rel(ClassLatency, "time to first output p50 (ms)", pub.Latency.TTFTMS.P50, rep.Latency.TTFTMS.P50, o.Tol.LatencyRel)
	}
	if pub.Latency.DecodeToksPerSecond > 0 && rep.Latency.DecodeToksPerSecond > 0 {
		rel(ClassLatency, "decode toks/s", pub.Latency.DecodeToksPerSecond, rep.Latency.DecodeToksPerSecond, o.Tol.LatencyRel)
	}
	if pub.Memory.Sampled && rep.Memory.Sampled {
		rel(ClassMemory, "memory peak (MB)", pub.Memory.PeakRSSMB, rep.Memory.PeakRSSMB, o.Tol.MemoryRel)
		rel(ClassMemory, "memory steady (MB)", pub.Memory.SteadyMB, rep.Memory.SteadyMB, o.Tol.MemoryRel)
	}
	return fs
}

func errRate(r *Result) float64 {
	if r.Corpus.CasesRun <= 0 {
		return 0
	}
	return float64(r.Errors.Count) / float64(r.Corpus.CasesRun)
}

func compareMetric(name string, p, r Metric, tol float64) Finding {
	f := Finding{Class: ClassQuality, Name: name, Published: metricText(p), Reproduced: metricText(r), Limit: tol}
	switch {
	case p.Total != r.Total || (p.Rate == nil) != (r.Rate == nil):
		// Same corpus, so the denominator must agree; if it does not, the scoring differs.
		f.Flagged = true
	case p.Rate != nil:
		f.Delta = *r.Rate - *p.Rate
		f.Flagged = math.Abs(f.Delta) > tol
	}
	return f
}

func metricText(m Metric) string {
	if m.Rate == nil {
		return fmt.Sprintf("n/a (0/%d)", m.Total)
	}
	return fmt.Sprintf("%d/%d (%.3f)", m.Passed, m.Total, *m.Rate)
}

// CompareRuns compares every run of the manifest (or those in only) with the
// result found in dir/<run id>/, the layout reproduce.sh writes. A published
// result whose hash differs from the manifest is refused rather than compared:
// the baseline must be the published one.
func CompareRuns(m *Manifest, root, dir string, only []string, allowMissing bool, o CompareOptions) ([]Comparison, error) {
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	known := map[string]bool{}
	for _, r := range m.Runs {
		known[r.ID] = true
	}
	for _, id := range sortedSet(want) {
		if !known[id] {
			return nil, fmt.Errorf("run %q is not in the manifest", id)
		}
	}
	var out []Comparison
	for _, r := range m.Runs {
		if len(want) > 0 && !want[r.ID] {
			continue
		}
		cmp := Comparison{RunID: r.ID}
		if _, _, err := readPinned(root, r.Result, r.ResultSHA256); err != nil {
			return nil, fmt.Errorf("run %s: published result: %w", r.ID, err)
		}
		pp, _ := safePath(root, r.Result)
		pub, err := LoadResult(pp)
		if err != nil {
			return nil, err
		}
		repPath, err := findOneResult(filepath.Join(dir, r.ID))
		switch {
		case errors.Is(err, os.ErrNotExist):
			cmp.Findings = []Finding{{Class: ClassMissing, Name: "no reproduced result", Published: r.Result, Reproduced: "none", Flagged: !allowMissing}}
			out = append(out, cmp)
			continue
		case err != nil:
			return nil, fmt.Errorf("run %s: %w", r.ID, err)
		}
		rep, err := LoadResult(repPath)
		if err != nil {
			return nil, err
		}
		cmp.Findings = Compare(pub, rep, o)
		out = append(out, cmp)
	}
	return out, nil
}

// findOneResult returns the single *.json file of dir.
func findOneResult(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		if st, err := os.Stat(dir); err != nil {
			return "", err
		} else if !st.IsDir() {
			return "", fmt.Errorf("%s is not a directory", dir)
		}
		return "", fmt.Errorf("%s holds no result (*.json): %w", dir, os.ErrNotExist)
	case 1:
		return matches[0], nil
	}
	return "", fmt.Errorf("%s holds %d results, want exactly one", dir, len(matches))
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FormatComparison renders one comparison as text. Flagged rows start with FLAG.
func FormatComparison(c Comparison, strictPerf bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s\n", c.RunID)
	for _, f := range c.Findings {
		mark := "  ok  "
		if f.Flagged {
			mark = " FLAG "
			if (f.Class == ClassLatency || f.Class == ClassMemory) && !strictPerf {
				mark = " note "
			}
		}
		switch f.Class {
		case ClassComparable, ClassMissing:
			if f.Flagged {
				fmt.Fprintf(&b, "%s%-13s %s: published %s, reproduced %s\n", mark, f.Class, f.Name, f.Published, f.Reproduced)
			}
		case ClassQuality:
			fmt.Fprintf(&b, "%s%-13s %-24s published %-18s reproduced %-18s delta %+.3f (limit %.3f)\n", mark, f.Class, f.Name, f.Published, f.Reproduced, f.Delta, f.Limit)
		default:
			fmt.Fprintf(&b, "%s%-13s %-30s published %-10s reproduced %-10s delta %+.1f%% (limit %.0f%%)\n", mark, f.Class, f.Name, f.Published, f.Reproduced, f.Delta*100, f.Limit*100)
		}
	}
	return b.String()
}
