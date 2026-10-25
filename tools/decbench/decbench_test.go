package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const corpusPath = "../../tests/performance/decision/decision-v1.jsonl"

func loadTestCorpus(t *testing.T) *Corpus {
	t.Helper()
	c, err := LoadCorpus(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func fakeRun(t *testing.T, configs string, profile string) *Result {
	t.Helper()
	specs, _, err := SelectConfigs(configs, ScorerFake)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), RunOptions{Configs: specs, Scorer: ScorerFake, Corpus: loadTestCorpus(t), Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func wantNoProblems(t *testing.T, r *Result) {
	t.Helper()
	if ps := VerifyResult(r); len(ps) != 0 {
		t.Fatalf("a fresh result must verify: %v", ps)
	}
}

func TestFiveConfigurationsRunWithFakes(t *testing.T) {
	r := fakeRun(t, "all", ProfileLite)
	if len(r.Configurations) != 5 {
		t.Fatalf("got %d configurations, want 5", len(r.Configurations))
	}
	if !r.Synthetic || !r.Emulated || r.Scorer.Kind != ScorerFake || r.Controller.Source != ControllerScripted {
		t.Fatalf("a fake run is synthetic, emulated and scripted: %+v %+v", r.Scorer, r.Controller)
	}
	wantNoProblems(t, r)

	byID := map[int]Configuration{}
	var ids0 []string
	for i, c := range r.Configurations {
		byID[c.ID] = c
		var ids []string
		for _, cs := range c.Cases {
			ids = append(ids, cs.ID)
		}
		if i == 0 {
			ids0 = ids
		} else if !reflect.DeepEqual(ids0, ids) {
			t.Fatalf("configuration %d did not run the identical fixtures", c.ID)
		}
	}
	if len(ids0) != len(loadTestCorpus(t).Cases) {
		t.Fatalf("ran %d cases, the corpus holds %d", len(ids0), len(loadTestCorpus(t).Cases))
	}

	// Which components each configuration used.
	for _, id := range []int{1, 4} {
		if byID[id].ScorerCalls != 0 {
			t.Errorf("configuration %d must not call a scorer, called %d times", id, byID[id].ScorerCalls)
		}
	}
	for _, id := range []int{1, 2, 3} {
		if byID[id].ControllerCalls != 0 {
			t.Errorf("configuration %d has no controller, made %d calls", id, byID[id].ControllerCalls)
		}
	}
	if got := byID[4].ControllerCalls; got != len(ids0) {
		t.Errorf("direct controller routing calls the controller for every case: %d of %d", got, len(ids0))
	}
	if byID[4].Metrics.ControllerCallsAvoided.Passed != 0 {
		t.Error("direct controller routing avoids no controller call")
	}

	// Configurations 2, 3 and 5 get ACCEPTED scorer advice for in-distribution cases, and
	// the scorer is never consulted for an exact rule match.
	for _, id := range []int{2, 3, 5} {
		cfg := byID[id]
		accepted := 0
		for _, c := range cfg.Cases {
			if c.ScorerStatus == ScorerAccepted {
				accepted++
				if c.DecidedBy != ByScorer || c.Confidence == nil {
					t.Errorf("configuration %d case %s: accepted advice must be final and carry a confidence", id, c.ID)
				}
			}
			if c.DecidedBy == ByRules && c.ScorerCalled {
				t.Errorf("configuration %d case %s: the scorer was asked after an exact rule matched", id, c.ID)
			}
			if c.Category == CatParaphrase && c.ScorerStatus != ScorerAccepted {
				t.Errorf("configuration %d case %s: an in-distribution paraphrase must be accepted, got %s", id, c.ID, c.ScorerStatus)
			}
		}
		if accepted < 20 || cfg.ScorerCalls == 0 {
			t.Errorf("configuration %d: %d accepted selections and %d scorer calls", id, accepted, cfg.ScorerCalls)
		}
		if cfg.Cache.Requests != accepted || cfg.Cache.Hits != accepted {
			t.Errorf("configuration %d: warm replay of %d accepted requests hit %d/%d", id, accepted, cfg.Cache.Hits, cfg.Cache.Requests)
		}
	}

	// Configuration 5 asks the controller only after the scorer fell back (or the
	// candidate set was refused before it); every controller call there has a reason.
	cfg5 := byID[5]
	for _, c := range cfg5.Cases {
		if c.ControllerCalled && (c.ScorerStatus != ScorerFallback || c.FallbackReason == nil) {
			t.Errorf("case %s: the controller ran although the scorer had an accepted answer", c.ID)
		}
	}
	if cfg5.ControllerCalls == 0 || cfg5.ControllerCalls >= byID[4].ControllerCalls {
		t.Errorf("scorer-then-controller must call the controller sometimes but fewer times than direct routing: %d vs %d", cfg5.ControllerCalls, byID[4].ControllerCalls)
	}
	if cfg5.Metrics.GeneratedToksAvoided <= 0 {
		t.Error("configuration 5 avoids generated tokens")
	}

	// The untrusted document never leaves the harness.
	for _, cfg := range r.Configurations {
		for _, c := range cfg.Cases {
			if c.DocumentSent {
				t.Errorf("configuration %d case %s: untrusted document text reached a scorer request", cfg.ID, c.ID)
			}
		}
	}
	// A confident scripted miss is visible in configuration 5 but not in direct routing.
	if byID[5].Metrics.TaskCompletion.Passed >= byID[4].Metrics.TaskCompletion.Passed {
		t.Error("the scripted confident misses must cost configuration 5 some task completion")
	}
}

func TestRequestCarriesNoDocumentNoExpectationNoScript(t *testing.T) {
	c := loadTestCorpus(t)
	h := &harness{o: RunOptions{Timeout: 250 * 1e6}, clk: fakeClockForTest()}
	seen := 0
	for i := range c.Cases {
		cs := &c.Cases[i]
		b, err := json.Marshal(h.request(scopeOf(cs), cs))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if cs.Document != "" {
			seen++
			if strings.Contains(s, cs.Document) || strings.Contains(s, "IGNORE") || strings.Contains(s, "IGNORA") {
				t.Errorf("case %s: the document reached the request", cs.ID)
			}
		}
		for _, banned := range []string{"expect", "script", "attack_target"} {
			if strings.Contains(s, banned) {
				t.Errorf("case %s: the request mentions %q", cs.ID, banned)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no case carries a document")
	}
}

func TestSelectConfigsRejectsMismatchedScorer(t *testing.T) {
	for _, bad := range []struct{ arg, scorer string }{{"3", ScorerLocalAI}, {"2", ScorerCLM}, {"6", ScorerFake}, {"0", ScorerFake}, {"two", ScorerFake}} {
		if _, _, err := SelectConfigs(bad.arg, bad.scorer); err == nil {
			t.Errorf("--config %s with --scorer %s must be an error", bad.arg, bad.scorer)
		}
	}
	run, skipped, err := SelectConfigs("all", ScorerCLM)
	if err != nil || len(run) != 4 || len(skipped) != 1 {
		t.Fatalf("all with clm runs 1,3,4,5 and skips 2: %v %d %v", err, len(run), skipped)
	}
}

func TestBrierScoreAgainstHandComputed(t *testing.T) {
	// (0.9-1)^2 + (0.8-0)^2 + (0.6-1)^2 + (0.3-0)^2 = 0.01 + 0.64 + 0.16 + 0.09 = 0.9; / 4.
	ps := []CalPoint{{0.9, true}, {0.8, false}, {0.6, true}, {0.3, false}}
	got, ok := Brier(ps)
	if !ok || math.Abs(got-0.225) > 1e-12 {
		t.Fatalf("Brier = %v (ok=%v), want 0.225", got, ok)
	}
	if b, _ := Brier([]CalPoint{{1, true}, {0, false}}); b != 0 {
		t.Errorf("a perfect scorer has Brier 0, got %v", b)
	}
	if b, _ := Brier([]CalPoint{{1, false}}); b != 1 {
		t.Errorf("a certain miss has Brier 1, got %v", b)
	}
	if _, ok := Brier(nil); ok {
		t.Error("no points has no Brier score")
	}
}

func TestExpectedCalibrationErrorAgainstHandComputed(t *testing.T) {
	// Bin 9 holds [0.9, 1.0] (1 included): confidences 0.95, 1.0 and exactly 0.9, two of
	// three right: |2/3 - 0.95| = 0.283333. Bin 1 holds exactly 0.1 and 0.15, one of two
	// right: |0.5 - 0.125| = 0.375. Bin 0 holds 0.05, wrong: |0 - 0.05| = 0.05.
	// ECE = 3/6 * 0.283333 + 2/6 * 0.375 + 1/6 * 0.05 = 0.141667 + 0.125 + 0.008333 = 0.275.
	ps := []CalPoint{{0.95, true}, {1.0, true}, {0.9, false}, {0.1, false}, {0.15, true}, {0.05, false}}
	got, ok := ECE(ps)
	if !ok || math.Abs(got-0.275) > 1e-9 {
		t.Fatalf("ECE = %v (ok=%v), want 0.275", got, ok)
	}
	for c, want := range map[float64]int{0: 0, 0.0999: 0, 0.1: 1, 0.5: 5, 0.9: 9, 0.9999: 9, 1: 9} {
		if got := binOf(c); got != want {
			t.Errorf("binOf(%v) = %d, want %d", c, got, want)
		}
	}
	if e, _ := ECE([]CalPoint{{1, true}, {0, false}, {0.5, true}, {0.5, false}}); e > 1e-12 {
		t.Errorf("a calibrated scorer has ECE 0, got %v", e)
	}
	if _, ok := ECE(nil); ok {
		t.Error("no points has no ECE")
	}
	c := calibrationOf(ps)
	if c.N != 6 || c.Bins != 10 || c.Brier == nil || c.ECE == nil || *c.ECE != 0.275 {
		t.Errorf("calibrationOf = %+v", c)
	}
}

func TestPercentilesNearestRank(t *testing.T) {
	xs := make([]float64, 20)
	for i := range xs {
		xs[i] = float64(20 - i) // 20..1, deliberately unsorted
	}
	orig := append([]float64(nil), xs...)
	for pct, want := range map[int]float64{1: 1, 5: 1, 6: 2, 50: 10, 90: 18, 95: 19, 96: 20, 99: 20, 100: 20} {
		got, ok := Percentile(xs, pct)
		if !ok || got != want {
			t.Errorf("p%d = %v (ok=%v), want %v", pct, got, ok, want)
		}
	}
	if !reflect.DeepEqual(xs, orig) {
		t.Error("Percentile must not reorder its input")
	}
	if v, _ := Percentile([]float64{7}, 99); v != 7 {
		t.Errorf("one sample is every percentile, got %v", v)
	}
	for _, bad := range []int{0, -1, 101} {
		if _, ok := Percentile(xs, bad); ok {
			t.Errorf("p%d is not a percentile", bad)
		}
	}
	if _, ok := Percentile(nil, 50); ok {
		t.Error("no samples has no percentile")
	}

	s := Summarize(xs)
	if s.N != 20 || *s.P50 != 10 || *s.P95 != 19 || *s.P99 != 20 || *s.Max != 20 {
		t.Errorf("Summarize = %+v", s)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "mean") {
		t.Errorf("latency stats must not carry a mean: %s", b)
	}
	if e := Summarize(nil); e.N != 0 || e.P50 != nil || e.P99 != nil {
		t.Errorf("empty Stats must be all null: %+v", e)
	}
}

func TestCorpusValidAndBilingual(t *testing.T) {
	c := loadTestCorpus(t)
	perCat := map[string]map[string]int{}
	for _, cs := range c.Cases {
		if perCat[cs.Category] == nil {
			perCat[cs.Category] = map[string]int{}
		}
		perCat[cs.Category][cs.Language]++
		if strings.Contains(cs.Utterance+cs.Document, "@") {
			t.Errorf("case %s looks like it holds an address", cs.ID)
		}
	}
	for _, cat := range Categories {
		for _, lang := range Languages {
			if perCat[cat][lang] == 0 {
				t.Errorf("category %s has no %s case", cat, lang)
			}
		}
		if perCat[cat]["en"] != perCat[cat]["it"] {
			t.Errorf("category %s: %d en and %d it cases", cat, perCat[cat]["en"], perCat[cat]["it"])
		}
	}
	if len(perCat) != len(Categories) {
		t.Errorf("the corpus uses %d categories, expected %d", len(perCat), len(Categories))
	}

	// The fixtures are built from the signed contracts: every capability and every
	// contract a case names exists in that reference application's package.
	root := filepath.Join("..", "..", "examples", "reference-apps")
	for _, cs := range c.Cases {
		app, capID, _ := strings.Cut(cs.Contract, "/")
		if app != cs.Application {
			t.Errorf("case %s: contract %s is not of application %s", cs.ID, cs.Contract, cs.Application)
		}
		file := strings.NewReplacer(".", "-", "_", "-").Replace(capID)
		for _, p := range []string{
			filepath.Join(root, app, "package", "capabilities", file+".yaml"),
			filepath.Join(root, app, "package", "conversation-contracts", file+"."+cs.Language+".yaml"),
		} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("case %s: %v", cs.ID, err)
			}
		}
		for _, cd := range cs.Candidates {
			if strings.HasPrefix(cd.ID, "act:") || strings.HasPrefix(cd.ID, "rag.") || strings.Contains(cd.ID, "-synth-") {
				continue
			}
			p := filepath.Join(root, cs.Application, "package", "capabilities", strings.NewReplacer(".", "-", "_", "-").Replace(cd.ID)+".yaml")
			if _, err := os.Stat(p); err != nil {
				t.Errorf("case %s: candidate %s is not a capability of %s", cs.ID, cd.ID, cs.Application)
			}
		}
	}

	// The loader is strict.
	good := strings.Split(strings.TrimSpace(string(mustRead(t, corpusPath))), "\n")
	cases := map[string]string{
		"unknown key":    strings.Replace(good[0], `"id":`, `"surprise":1,"id":`, 1),
		"unknown lang":   strings.Replace(good[0], `"language":"en"`, `"language":"fr"`, 1),
		"unknown cat":    strings.Replace(good[0], `"category":"exact_command"`, `"category":"chat"`, 1),
		"not json":       "{",
		"missing pair":   good[0],
		"duplicate case": good[0] + "\n" + good[0],
	}
	for name, doc := range cases {
		if _, err := ParseCorpus([]byte(doc)); err == nil {
			t.Errorf("%s: the loader accepted a bad corpus", name)
		}
	}
	if _, err := ParseCorpus(nil); err == nil {
		t.Error("an empty corpus must be refused")
	}
	if _, err := LoadCorpus(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Error("a missing corpus must be an error")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func marshal(t *testing.T, r *Result) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestResultFileValidatesAndAggregatesRederive(t *testing.T) {
	schema, err := LoadResultSchema("")
	if err != nil {
		t.Fatal(err)
	}
	r := fakeRun(t, "all", ProfileBalanced)
	if ps := validateResultBytes(schema, marshal(t, r)); len(ps) != 0 {
		t.Fatalf("a fresh result must validate: %v", ps)
	}

	// Hand-edited aggregates are caught by re-deriving them from the cases.
	tamper := map[string]func(*Result){
		"task completion":      func(r *Result) { r.Configurations[1].Metrics.TaskCompletion.Passed++ },
		"flipped correct":      func(r *Result) { r.Configurations[0].Cases[0].Correct = !r.Configurations[0].Cases[0].Correct },
		"by category":          func(r *Result) { r.Configurations[2].ByCategory[0].Pass.Passed = 0 },
		"calibration":          func(r *Result) { r.Configurations[1].Calibration.ECE = round6(0.5) },
		"latency":              func(r *Result) { r.Configurations[3].Latency.FirstResponseMS.N-- },
		"scorer calls":         func(r *Result) { r.Configurations[1].ScorerCalls++ },
		"controller in rules":  func(r *Result) { r.Configurations[0].ControllerCalls = 1 },
		"cases differ":         func(r *Result) { r.Configurations[4].Cases[0].ID = "other" },
		"cases total":          func(r *Result) { r.Corpus.CasesTotal++ },
		"synthetic cleared":    func(r *Result) { r.Synthetic = false },
		"cache hits":           func(r *Result) { r.Configurations[1].Cache.Hits = r.Configurations[1].Cache.Requests + 1 },
		"memory reserve":       func(r *Result) { v := 99.0; r.Live.MemoryReserveMB = &v },
		"configuration name":   func(r *Result) { r.Configurations[0].Name = "clm-8b-scorer" },
		"documents sent":       func(r *Result) { r.Configurations[1].Cases[0].DocumentSent = true },
		"decided without call": func(r *Result) { r.Configurations[3].Cases[0].ControllerCalled = false },
	}
	for name, edit := range tamper {
		var cp Result
		if err := json.Unmarshal(marshal(t, r), &cp); err != nil {
			t.Fatal(err)
		}
		edit(&cp)
		if ps := validateResultBytes(schema, marshal(t, &cp)); len(ps) == 0 {
			t.Errorf("%s: the edit went unnoticed", name)
		}
	}

	// A result with a missing or empty place fails closed.
	var out bytes.Buffer
	if code := run([]string{"check", filepath.Join(t.TempDir(), "missing")}, &out, &out); code == 0 {
		t.Error("check of a missing path must fail")
	}
	if code := run([]string{"check", t.TempDir()}, &out, &out); code == 0 {
		t.Error("check of a directory with no result must fail")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"format_version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"check", bad}, &out, &out); code == 0 {
		t.Error("check of an invalid result must fail")
	}
}

func TestCLIRunThenCheck(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	args := []string{"run", "--config", "all", "--scorer", "fake", "--corpus", corpusPath, "--out", dir}
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("run exit %d: %s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "SYNTHETIC") {
		t.Errorf("a fake run must say it is synthetic:\n%s", out.String())
	}
	out.Reset()
	if code := run([]string{"check", dir}, &out, &errb); code != 0 {
		t.Fatalf("check exit %d: %s", code, out.String())
	}
	if code := run([]string{"check", "-reject-synthetic", dir}, &out, &errb); code == 0 {
		t.Error("-reject-synthetic must fail on a fake run")
	}
	for _, bad := range [][]string{
		{"run", "--config", "3", "--scorer", "localai", "--corpus", corpusPath, "--out", dir, "--model", "m"},
		{"run", "--config", "all", "--scorer", "bogus", "--corpus", corpusPath, "--out", dir},
		{"run", "--config", "all", "--scorer", "fake", "--corpus", corpusPath, "--out", dir, "--profile", "huge"},
		{"run", "--config", "2", "--scorer", "localai", "--corpus", corpusPath, "--out", dir},
		{"run", "--config", "all"},
		{"nonsense"},
	} {
		if code := run(bad, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
	if code := run([]string{"run", "--config", "all", "--scorer", "fake", "--corpus", filepath.Join(dir, "none.jsonl"), "--out", dir}, &out, &errb); code != 1 {
		t.Errorf("a missing corpus must fail the run, exit %d", code)
	}
}

func TestEmulatedFlagForLimitedProfiles(t *testing.T) {
	schema, err := LoadResultSchema("")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ProfileLite, ProfileBalanced, ProfileHigh, Profile4G} {
		if !IsEmulated(p) {
			t.Errorf("profile %s must be emulated", p)
		}
		r := fakeRun(t, "4", p)
		if !r.Emulated {
			t.Errorf("a run under profile %s must be flagged emulated", p)
		}
		limits, _ := LimitsFor(p)
		if r.ContainerLimits != limits {
			t.Errorf("profile %s: container limits %+v, want %+v", p, r.ContainerLimits, limits)
		}
		wantNoProblems(t, r)

		// There is no way to claim a certified result: the schema and the check both refuse.
		var cp Result
		if err := json.Unmarshal(marshal(t, r), &cp); err != nil {
			t.Fatal(err)
		}
		cp.Emulated = false
		if ps := validateResultBytes(schema, marshal(t, &cp)); len(ps) == 0 {
			t.Errorf("profile %s: emulated=false was accepted", p)
		}
		if ps := VerifyResult(&cp); len(ps) == 0 {
			t.Errorf("profile %s: VerifyResult accepted emulated=false", p)
		}
		cp.Emulated = true
		cp.ContainerLimits.MemMB *= 2
		if ps := VerifyResult(&cp); len(ps) == 0 {
			t.Errorf("profile %s: limits that are not the profile's were accepted", p)
		}
	}
	if IsEmulated("certified") {
		t.Error("an unknown profile is not a profile")
	}
	for in, want := range map[string]string{"lite": ProfileLite, "Balanced": ProfileBalanced, "high": ProfileHigh, "4g": Profile4G, "experimental-4g": Profile4G} {
		if got, err := ParseProfile(in); err != nil || got != want {
			t.Errorf("ParseProfile(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseProfile("huge"); err == nil {
		t.Error("an unknown profile must be an error")
	}
}

// ---- the default-enable gate ----

func f64(v float64) *float64 { return &v }

// gateBase is a baseline (layer off) with comparable numbers for every guarded metric.
func gateBase() GateInput {
	r := func(p, t int) Ratio { return newRatio(p, t) }
	return GateInput{
		Name: "baseline", RunID: "run_baseline", FirstResponseP95: f64(600), FirstResponseN: 86, MemoryReserveMB: f64(5000),
		Metrics: MetricSet{
			Cases: 86, TaskCompletion: r(86, 86), CitationCorrectness: r(9, 9), MultilingualQuality: r(43, 43), FailClosed: r(6, 6),
			UnauthorisedRejection: r(6, 6), InjectionResistance: r(6, 6), UntrustedDocumentHandling: r(4, 4), OutOfScopeRejection: r(6, 6),
			ControllerCallsAvoided: r(0, 86),
		},
	}
}

// gateGood is a candidate that improves latency and capacity and regresses nothing.
func gateGood() GateInput {
	g := gateBase()
	g.Name = "candidate"
	g.RunID = "run_candidate"
	g.FirstResponseP95 = f64(300)
	g.MemoryReserveMB = f64(5000)
	g.Metrics.ControllerCallsAvoided = newRatio(60, 86)
	g.Metrics.GeneratedToksAvoided = 1200
	return g
}

func TestDefaultEnableGateRefusesWhenAnyMetricRegresses(t *testing.T) {
	if d := DefaultEnableGate(gateBase(), gateGood()); !d.Enable || len(d.Regressed) != 0 || len(d.Unknown) != 0 {
		t.Fatalf("a candidate that improves and regresses nothing is enabled: %+v", d)
	}

	type row struct {
		name   string
		edit   func(*GateInput)
		reason string
	}
	less := func(m *MetricSet, pick func(*MetricSet) *Ratio) {
		r := pick(m)
		*r = newRatio(r.Passed-1, r.Total)
	}
	regressions := []row{
		{"task success", func(g *GateInput) { less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.TaskCompletion }) }, "task_completion"},
		{"citation correctness", func(g *GateInput) { less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.CitationCorrectness }) }, "citation_correctness"},
		{"multilingual quality", func(g *GateInput) { less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.MultilingualQuality }) }, "multilingual_quality"},
		{"fail-closed behaviour", func(g *GateInput) { less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.FailClosed }) }, "fail_closed"},
		{"memory reserve", func(g *GateInput) { g.MemoryReserveMB = f64(4999.9) }, "memory_reserve"},
		{"unauthorised rejection", func(g *GateInput) { less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.UnauthorisedRejection }) }, "unauthorised_rejection"},
		{"injection resistance", func(g *GateInput) { less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.InjectionResistance }) }, "injection_resistance"},
		{"untrusted documents", func(g *GateInput) {
			less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.UntrustedDocumentHandling })
		}, "untrusted_document_handling"},
		{"out-of-scope rejection", func(g *GateInput) { less(&g.Metrics, func(m *MetricSet) *Ratio { return &m.OutOfScopeRejection }) }, "out_of_scope_rejection"},
		{"slower p95", func(g *GateInput) { g.FirstResponseP95 = f64(601) }, "first_response_p95"},
	}
	for _, rw := range regressions {
		g := gateGood() // everything improves except the one metric the row regresses
		rw.edit(&g)
		d := DefaultEnableGate(gateBase(), g)
		if d.Enable {
			t.Errorf("%s: the gate enabled a candidate that regressed it: %+v", rw.name, d)
		}
		if !contains(d.Regressed, rw.reason) {
			t.Errorf("%s: regressions %v do not name %s", rw.name, d.Regressed, rw.reason)
		}
		if !strings.Contains(d.Reason(), rw.reason) {
			t.Errorf("%s: the reason %q does not name the metric", rw.name, d.Reason())
		}
	}

	// Unknown is not non-regression.
	unknown := []row{
		{"no citation cases", func(g *GateInput) { g.Metrics.CitationCorrectness = newRatio(0, 0) }, "citation_correctness"},
		{"different denominator", func(g *GateInput) { g.Metrics.TaskCompletion = newRatio(80, 80) }, "task_completion"},
		{"memory not measured", func(g *GateInput) { g.MemoryReserveMB = nil }, "memory_reserve"},
		{"latency not measured", func(g *GateInput) { g.FirstResponseP95 = nil }, "first_response_p95"},
		{"different sample count", func(g *GateInput) { g.FirstResponseN = 85 }, "first_response_p95"},
		{"synthetic evidence", func(g *GateInput) { g.Synthetic = true }, "synthetic"},
		{"scripted controller", func(g *GateInput) { g.ControllerScripted = true }, "scripted controller"},
	}
	for _, rw := range unknown {
		g := gateGood()
		rw.edit(&g)
		d := DefaultEnableGate(gateBase(), g)
		if d.Enable || !containsSub(d.Unknown, rw.reason) {
			t.Errorf("%s: want a refusal naming %q, got %+v", rw.name, rw.reason, d)
		}
		// The same holds when the baseline is the unknown side.
		d = DefaultEnableGate(g, gateGood())
		if d.Enable {
			t.Errorf("%s (baseline side): the gate enabled on unknown evidence", rw.name)
		}
	}

	// Memory is measured once per run: one run cannot tell the layer on from the layer off.
	oneRun := gateGood()
	oneRun.RunID = gateBase().RunID
	if d := DefaultEnableGate(gateBase(), oneRun); d.Enable || !containsSub(d.Unknown, "memory_reserve") {
		t.Errorf("baseline and candidate from the same run must not enable: %+v", d)
	}

	// Nothing improved: not enabled, however safe.
	same := gateBase()
	same.Name = "same"
	same.RunID = "run_same"
	if d := DefaultEnableGate(gateBase(), same); d.Enable || len(d.Improved) != 0 {
		t.Errorf("an unchanged candidate must not be enabled: %+v", d)
	}
	// Each of the three improvements alone is enough.
	lat := gateBase()
	lat.RunID, lat.FirstResponseP95 = "run_lat", f64(599)
	capa := gateBase()
	capa.RunID, capa.Metrics.GeneratedToksAvoided = "run_capa", 1
	safeCand := gateBase()
	safeCand.RunID = "run_safe"
	safeBase := gateBase()
	safeBase.Metrics.UnauthorisedRejection = newRatio(5, 6)
	for name, pair := range map[string][2]GateInput{
		"latency":  {gateBase(), lat},
		"capacity": {gateBase(), capa},
		"safety":   {safeBase, safeCand},
	} {
		if d := DefaultEnableGate(pair[0], pair[1]); !d.Enable || !contains(d.Improved, name) {
			t.Errorf("%s alone must enable: %+v", name, d)
		}
	}
}

func TestGateRefusesEveryFakeRun(t *testing.T) {
	r := fakeRun(t, "all", ProfileHigh)
	b, err := gateInputOf(r, 4)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := gateInputOf(r, 5)
	d := DefaultEnableGate(b, c)
	if d.Enable || !containsSub(d.Unknown, "synthetic") {
		t.Fatalf("evidence from the fake scorer must never enable the layer: %+v", d)
	}
	// The scripted misses show up as regressions too, not only as missing evidence.
	if !contains(d.Regressed, "task_completion") {
		t.Errorf("the scripted confident misses must read as a task-completion regression: %+v", d)
	}
	if _, err := gateInputOf(r, 9); err == nil {
		t.Error("gateInputOf of an absent configuration must fail")
	}

	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := run([]string{"run", "--config", "all", "--scorer", "fake", "--corpus", corpusPath, "--out", dir}, &out, &errb); code != 0 {
		t.Fatal(errb.String())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	out.Reset()
	if code := run([]string{"gate", "--baseline", files[0], "--candidate", files[0]}, &out, &errb); code != 1 || !strings.Contains(out.String(), "do not enable") {
		t.Errorf("gate on a fake result: exit %d, %s", code, out.String())
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func containsSub(xs []string, s string) bool {
	for _, x := range xs {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

func TestSuppliedLiveNumbersAreDerivedNotInvented(t *testing.T) {
	specs, _, _ := SelectConfigs("all", ScorerFake)
	peak, resident, ctrl, audio := 1500.0, 6000.0, 700.0, 250.0
	r, err := Run(context.Background(), RunOptions{
		Configs: specs, Scorer: ScorerFake, Corpus: loadTestCorpus(t), Profile: ProfileLite,
		PeakRSSMB: &peak, ResidentModelMB: &resident, ControllerMS: &ctrl, FirstAudioMS: &audio,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantNoProblems(t, r)
	if r.Live.MemoryReserveMB == nil || *r.Live.MemoryReserveMB != 8192-1500-6000 {
		t.Errorf("memory reserve = %v, want %d", r.Live.MemoryReserveMB, 8192-1500-6000)
	}
	if r.Controller.Source != ControllerSupplied || *r.Controller.LatencyMS != 700 {
		t.Errorf("controller = %+v", r.Controller)
	}
	for _, c := range r.Configurations[3].Cases {
		if c.FirstResponseMS != c.DecisionMS+700 || c.FirstAudioMS == nil || *c.FirstAudioMS != c.FirstResponseMS+250 {
			t.Fatalf("case %s: first response %v / audio %v", c.ID, c.FirstResponseMS, c.FirstAudioMS)
		}
	}
	// Without supplied values everything the harness cannot measure stays null.
	plain := fakeRun(t, "all", ProfileLite)
	l := plain.Live
	if l.FirstAudioMS != nil || l.PeakRSSMB != nil || l.ResidentModelMB != nil || l.MemoryReserveMB != nil ||
		l.ColdLoadMS != nil || l.WarmLoadMS != nil || l.CacheEvictions != nil || l.ConcurrentWorkloads != nil {
		t.Errorf("a harness run must not invent live numbers: %+v", l)
	}
	if plain.Configurations[0].Latency.FirstAudioMS != nil {
		t.Error("first-audio latency is null unless supplied")
	}
}
