package repro

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

func repoRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := FindRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// publishedManifest returns a root that holds the published manifest and every input it names.
// Where the checkout carries the raw result files that is the repository root itself. The
// published source tree leaves the raw results out, so the tests then run on a copy of the
// root's inputs in which each run's result is rebuilt from testdata/result.json with the fields
// the manifest pins for that run, and the copied manifest pins the rebuilt files' hashes.
func publishedManifest(t testing.TB) (string, *Manifest) {
	t.Helper()
	root := repoRoot(t)
	m, err := LoadManifest(filepath.Join(root, filepath.FromSlash(ManifestPath)))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Runs {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(r.Result))); err != nil {
			return fixtureRoot(t, root, m)
		}
	}
	return root, m
}

// fixtureRoot builds the stand-in root described at publishedManifest.
func fixtureRoot(t testing.TB, root string, m *Manifest) (string, *Manifest) {
	t.Helper()
	template, err := os.ReadFile(filepath.Join("testdata", "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	m = cloneManifest(t, m)
	dst := t.TempDir()
	write := func(rel string, data []byte) {
		to := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{m.Runtime.Compose, m.Runtime.Override, m.Runtime.BackendGallery.Path}
	for _, c := range m.Corpora {
		files = append(files, c.Path)
	}
	for _, p := range m.Profiles {
		files = append(files, p.Compose)
	}
	for _, md := range m.Models {
		files = append(files, md.Definition)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		write(f, data)
	}
	for i, r := range m.Runs {
		md, _ := m.Model(r.Model)
		c, _ := m.Corpus(r.Corpus)
		var doc map[string]any
		if err := json.Unmarshal(template, &doc); err != nil {
			t.Fatal(err)
		}
		obj := func(k string) map[string]any { return doc[k].(map[string]any) }
		doc["run_id"] = "run_" + r.ID
		doc["profile"] = r.Profile
		obj("provider")["name"] = m.Bench.Provider
		obj("model")["registry_id"] = r.Model
		obj("model")["artifact_sha256"] = md.SHA256
		obj("model")["quantization"] = md.Quantization
		obj("backend")["name"] = m.Runtime.Backend
		obj("backend")["runtime_version"] = m.Runtime.Version
		obj("context")["configured_toks"] = m.Bench.ContextToks
		obj("corpus")["id"] = c.ID
		obj("corpus")["sha256"] = c.SHA256
		obj("corpus")["cases_total"] = c.Cases
		obj("corpus")["cases_run"] = c.Cases
		cfg := obj("config")
		cfg["role"] = m.Bench.Role
		cfg["constraint"] = m.Bench.Constraint
		cfg["max_toks"] = m.Bench.MaxToks
		cfg["percentile_method"] = m.Bench.PercentileMethod
		cfg["prompt"] = map[string]any{"id": m.Bench.PromptID, "sha256": m.Bench.PromptSHA256}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		write(r.Result, data)
		sum, err := FileSHA256(filepath.Join(dst, filepath.FromSlash(r.Result)))
		if err != nil {
			t.Fatal(err)
		}
		m.Runs[i].ResultSHA256 = sum
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(ManifestPath, data)
	if _, err := LoadManifest(filepath.Join(dst, filepath.FromSlash(ManifestPath))); err != nil {
		t.Fatalf("the stand-in manifest is invalid: %v", err)
	}
	return dst, m
}

func registryOrFatal(t testing.TB) *models.Registry {
	t.Helper()
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// copyInputs copies every repository file the manifest names into a fresh root, so a
// test can damage one input and watch exactly that check fail.
func copyInputs(t testing.TB, root string, m *Manifest) string {
	t.Helper()
	dst := t.TempDir()
	files := []string{m.Runtime.Compose, m.Runtime.Override, m.Runtime.BackendGallery.Path}
	for _, c := range m.Corpora {
		files = append(files, c.Path)
	}
	for _, p := range m.Profiles {
		files = append(files, p.Compose)
	}
	for _, md := range m.Models {
		files = append(files, md.Definition)
	}
	for _, r := range m.Runs {
		files = append(files, r.Result)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		to := filepath.Join(dst, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func failedNames(cs []Check) []string {
	var out []string
	for _, c := range cs {
		if !c.OK() {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestPublishedManifestIsValid(t *testing.T) {
	_, m := publishedManifest(t)
	if len(m.Runs) != 14 || len(m.Models) != 5 || len(m.Profiles) != 3 {
		t.Fatalf("manifest shape: %d runs, %d models, %d profiles", len(m.Runs), len(m.Models), len(m.Profiles))
	}
}

func TestVerifyPublishedInputsHold(t *testing.T) {
	root, m := publishedManifest(t)
	cs := Verify(context.Background(), m, VerifyOptions{Root: root, Registry: registryOrFatal(t)})
	if want := len(m.Corpora) + 1 + len(m.Profiles) + len(m.Models) + len(m.Runs); len(cs) != want {
		t.Fatalf("%d checks, want %d", len(cs), want)
	}
	for _, c := range cs {
		if !c.OK() {
			t.Errorf("%s: %v", c.Name, c.Err)
		}
	}
}

func TestVerifyDetectsTamperedInputs(t *testing.T) {
	root, m := publishedManifest(t)
	reg := registryOrFatal(t)
	run0 := m.Runs[0]
	tests := []struct {
		name   string
		mutate func(t *testing.T, dir string, m *Manifest)
		want   string
	}{
		{"corpus byte changed", func(t *testing.T, dir string, m *Manifest) {
			appendTo(t, filepath.Join(dir, filepath.FromSlash(m.Corpora[0].Path)), " ")
		}, "corpus routing-v0"},
		{"published result edited", func(t *testing.T, dir string, m *Manifest) {
			appendTo(t, filepath.Join(dir, filepath.FromSlash(run0.Result)), "\n")
		}, "run " + run0.ID},
		{"image digest differs", func(t *testing.T, dir string, m *Manifest) {
			m.Runtime.Image = strings.Replace(m.Runtime.Image, "sha256:0", "sha256:1", 1)
		}, "runtime"},
		{"backend gallery edited", func(t *testing.T, dir string, m *Manifest) {
			appendTo(t, filepath.Join(dir, filepath.FromSlash(m.Runtime.BackendGallery.Path)), "# x\n")
		}, "runtime"},
		{"model hash differs from the registry", func(t *testing.T, dir string, m *Manifest) {
			m.Models[0].SHA256 = strings.Repeat("0", 64)
		}, "model " + m.Models[0].ID},
		{"model definition hash differs", func(t *testing.T, dir string, m *Manifest) {
			p := filepath.Join(dir, filepath.FromSlash(m.Models[0].Definition))
			data, _ := os.ReadFile(p)
			bad := strings.Replace(string(data), m.Models[0].SHA256, strings.Repeat("a", 64), 1)
			if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "model " + m.Models[0].ID},
		{"profile limit differs", func(t *testing.T, dir string, m *Manifest) {
			p := m.Profiles["lite"]
			p.MemLimit = "9g"
			m.Profiles["lite"] = p
		}, "profile lite"},
		{"result measured on another prompt", func(t *testing.T, dir string, m *Manifest) {
			m.Bench.PromptSHA256 = strings.Repeat("b", 64)
		}, "run " + run0.ID},
		{"path leaves the root", func(t *testing.T, dir string, m *Manifest) {
			m.Corpora[0].Path = "../outside.jsonl"
		}, "corpus routing-v0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mc := cloneManifest(t, m)
			dir := copyInputs(t, root, mc)
			tc.mutate(t, dir, mc)
			got := failedNames(Verify(context.Background(), mc, VerifyOptions{Root: dir, Registry: reg}))
			if !contains(got, tc.want) {
				t.Fatalf("failed checks %v, want %q among them", got, tc.want)
			}
		})
	}
}

func TestVerifyModelFilesInModelsDir(t *testing.T) {
	root, m := publishedManifest(t)
	reg := registryOrFatal(t)
	md := m.Models[0]
	dir := t.TempDir()
	// A file with the right name and the wrong content must fail, an absent one is skipped
	// unless required.
	if err := os.WriteFile(filepath.Join(dir, md.Filename), []byte("not a model"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := Verify(context.Background(), m, VerifyOptions{Root: root, Registry: reg, ModelsDir: dir})
	if got := failedNames(cs); len(got) != 1 || got[0] != "model "+md.ID {
		t.Fatalf("wrong content: failed %v, want only the damaged model", got)
	}
	cs = Verify(context.Background(), m, VerifyOptions{Root: root, Registry: reg, ModelsDir: t.TempDir(), RequireModels: true})
	if got := failedNames(cs); len(got) != len(m.Models) {
		t.Fatalf("required but absent: %d models failed, want %d", len(got), len(m.Models))
	}
	if got := failedNames(Verify(context.Background(), m, VerifyOptions{Root: root, Registry: reg, ModelsDir: t.TempDir()})); len(got) != 0 {
		t.Fatalf("absent but not required failed: %v", got)
	}
}

func TestManifestRejectsUnknownKeysAndUnpinnedImage(t *testing.T) {
	root, _ := publishedManifest(t)
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ManifestPath)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(bytes.Replace(data, []byte(`"format_version": 1`), []byte(`"format_version": 1, "extra": true`), 1)); err == nil {
		t.Error("an unknown key was accepted")
	}
	unpinned := regexpReplace(t, string(data), `@sha256:[0-9a-f]{64}"`, `"`)
	if _, err := ParseManifest([]byte(unpinned)); err == nil || !strings.Contains(err.Error(), "pinned by digest") {
		t.Errorf("an image without a digest was accepted: %v", err)
	}
}

// loadPublished returns the first published result of the manifest, fresh each call.
func loadPublished(t testing.TB) (*Result, *Manifest) {
	t.Helper()
	root, m := publishedManifest(t)
	res, err := LoadResult(filepath.Join(root, filepath.FromSlash(m.Runs[0].Result)))
	if err != nil {
		t.Fatal(err)
	}
	return res, m
}

func flagged(fs []Finding, class Class) []string {
	var out []string
	for _, f := range fs {
		if f.Flagged && f.Class == class {
			out = append(out, f.Name)
		}
	}
	return out
}

func TestCompareIdenticalResultsDoNotDeviate(t *testing.T) {
	pub, m := loadPublished(t)
	rep, _ := loadPublished(t)
	fs := Compare(pub, rep, CompareOptions{Tol: m.Tolerances})
	if got := (Comparison{Findings: fs}).Flagged(); got != 0 {
		t.Fatalf("%d findings flagged for identical results: %+v", got, fs)
	}
	if len(flagged(fs, ClassQuality)) != 0 || len(fs) < 20 {
		t.Fatalf("expected the comparability, quality and performance rows, got %d", len(fs))
	}
}

func TestCompareFlagsQualityBeyondTolerance(t *testing.T) {
	pub, m := loadPublished(t)
	o := CompareOptions{Tol: m.Tolerances}

	within, _ := loadPublished(t)
	within.Metrics.SchemaValid = shifted(pub.Metrics.SchemaValid, +1) // one case of 80 is 0.0125
	if got := flagged(Compare(pub, within, o), ClassQuality); len(got) != 0 {
		t.Errorf("a one-case difference was flagged: %v", got)
	}

	beyond, _ := loadPublished(t)
	beyond.Metrics.SchemaValid = shifted(pub.Metrics.SchemaValid, +8) // 0.1
	fs := Compare(pub, beyond, o)
	if got := flagged(fs, ClassQuality); len(got) != 1 || got[0] != "schema_valid" {
		t.Errorf("flagged %v, want schema_valid only", got)
	}
	if (Comparison{Findings: fs}).Failed(false) != 1 {
		t.Error("a quality deviation must fail the comparison")
	}

	denom, _ := loadPublished(t)
	denom.Metrics.Pass.Total++
	if got := flagged(Compare(pub, denom, o), ClassQuality); !contains(got, "pass") {
		t.Errorf("a changed denominator was not flagged: %v", got)
	}

	errs, _ := loadPublished(t)
	errs.Errors.Count = 20
	if got := flagged(Compare(pub, errs, o), ClassQuality); !contains(got, "errors (rate of cases)") {
		t.Errorf("errors were not flagged: %v", got)
	}
}

func TestCompareRefusesDifferentInputs(t *testing.T) {
	pub, m := loadPublished(t)
	o := CompareOptions{Tol: m.Tolerances}
	tests := map[string]func(r *Result){
		"corpus sha256":         func(r *Result) { r.Corpus.SHA256 = strings.Repeat("c", 64) },
		"prompt sha256":         func(r *Result) { r.Config.Prompt.SHA256 = strings.Repeat("d", 64) },
		"model artifact sha256": func(r *Result) { r.Model.ArtifactSHA256 = strings.Repeat("e", 64) },
		"runtime version":       func(r *Result) { r.Backend.RuntimeVersion = "v9.9.9" },
		"context":               func(r *Result) { r.Context.ConfiguredToks = 2048 },
		"synthetic":             func(r *Result) { r.Synthetic = true },
	}
	for want, mut := range tests {
		t.Run(want, func(t *testing.T) {
			rep, _ := loadPublished(t)
			mut(rep)
			fs := Compare(pub, rep, o)
			if got := flagged(fs, ClassComparable); !contains(got, want) {
				t.Fatalf("flagged %v, want %q", got, want)
			}
			for _, f := range fs {
				if f.Class == ClassQuality || f.Class == ClassLatency {
					t.Fatalf("metrics were compared although the inputs differ: %+v", f)
				}
			}
		})
	}
}

func TestComparePerformanceIsReportedNotFailedUnlessStrict(t *testing.T) {
	pub, m := loadPublished(t)
	rep, _ := loadPublished(t)
	rep.Latency.TotalMS.P50 *= 3
	rep.Memory.PeakRSSMB *= 2
	fs := Compare(pub, rep, CompareOptions{Tol: m.Tolerances})
	if got := flagged(fs, ClassLatency); !contains(got, "latency total p50 (ms)") {
		t.Errorf("latency not flagged: %v", got)
	}
	if got := flagged(fs, ClassMemory); !contains(got, "memory peak (MB)") {
		t.Errorf("memory not flagged: %v", got)
	}
	c := Comparison{Findings: fs}
	if c.Failed(false) != 0 || c.Failed(true) == 0 {
		t.Errorf("performance deviation: failed %d (default) / %d (strict), want 0 / more", c.Failed(false), c.Failed(true))
	}
}

func TestCompareCommand(t *testing.T) {
	root, m := publishedManifest(t)
	ids := []string{m.Runs[0].ID, m.Runs[1].ID}
	dir := t.TempDir()
	for _, r := range m.Runs[:2] {
		to := filepath.Join(dir, r.ID)
		if err := os.MkdirAll(to, 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(r.Result)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, "result.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := RunCompare(append([]string{"-root", root}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	list := strings.Join(ids, ",")
	if code, out := run("-reproduced", dir, "-runs", list); code != ExitOK {
		t.Fatalf("the published results compared with themselves: exit %d\n%s", code, out)
	}
	if code, _ := run("-reproduced", dir, "-runs", list+","+m.Runs[2].ID); code != ExitFailed {
		t.Errorf("a missing reproduced result exited %d, want %d", code, ExitFailed)
	}
	if code, out := run("-reproduced", dir, "-runs", list+","+m.Runs[2].ID, "-allow-missing"); code != ExitOK {
		t.Errorf("-allow-missing exited %d\n%s", code, out)
	}
	if code, _ := run("-reproduced", dir, "-runs", "no-such-run"); code != ExitUsageOrIO {
		t.Errorf("an unknown run exited %d, want %d", code, ExitUsageOrIO)
	}
	// Damage one reproduced result: schema_valid collapses.
	damaged := filepath.Join(dir, ids[0], "result.json")
	var doc map[string]any
	data, _ := os.ReadFile(damaged)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["metrics"].(map[string]any)["schema_valid"] = map[string]any{"passed": 0, "total": 80, "rate": 0.0}
	data, _ = json.Marshal(doc)
	if err := os.WriteFile(damaged, data, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := run("-reproduced", dir, "-runs", list)
	if code != ExitFailed || !strings.Contains(out, "FLAG") {
		t.Errorf("a collapsed schema_valid rate exited %d without a FLAG\n%s", code, out)
	}
	if code, _ := run("-reproduced", dir, "-runs", list, "-tol-quality", "0.99"); code != ExitOK {
		t.Errorf("a wide -tol-quality still failed: exit %d", code)
	}
}

func TestRunPlanListsEveryRunOnce(t *testing.T) {
	root, m := publishedManifest(t)
	var out, errb bytes.Buffer
	if code := RunPlan([]string{"-root", root}, &out, &errb); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(m.Runs) {
		t.Fatalf("%d lines, want %d", len(lines), len(m.Runs))
	}
	for _, l := range lines {
		if n := len(strings.Split(l, "\t")); n != 16 {
			t.Fatalf("%d columns in %q, want 16", n, l)
		}
	}
	out.Reset()
	if code := RunPlan([]string{"-root", root, "-profile", "4g"}, &out, &errb); code != ExitOK || strings.Count(out.String(), "\n") != 4 {
		t.Errorf("-profile 4g: exit %d, output %q", code, out.String())
	}
	if code := RunPlan([]string{"-root", root, "-runs", "nope"}, &out, &errb); code != ExitUsageOrIO {
		t.Errorf("an unknown run exited %d", code)
	}
}

func TestVerifyInputsCommandOnPublishedInputs(t *testing.T) {
	root, _ := publishedManifest(t)
	var out, errb bytes.Buffer
	if code := RunVerifyInputs([]string{"-root", root}, &out, &errb); code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "FAIL") {
		t.Fatalf("output has a failure:\n%s", out.String())
	}
}

// --- helpers

func shifted(m Metric, by int) Metric {
	m.Passed += by
	r := float64(m.Passed) / float64(m.Total)
	m.Rate = &r
	return m
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func appendTo(t testing.TB, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func cloneManifest(t testing.TB, m *Manifest) *Manifest {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var c Manifest
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func regexpReplace(t testing.TB, s, pattern, repl string) string {
	t.Helper()
	return regexp.MustCompile(pattern).ReplaceAllString(s, repl)
}
