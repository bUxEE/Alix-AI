package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/models"
)

// The shipped mix is valid and is the load the plan names: one interactive
// session of booking and RAG turns at a human pace, and continuous re-ingestion.
func TestShippedMix(t *testing.T) {
	path, err := FindDefaultMix(".")
	if err != nil {
		t.Fatal(err)
	}
	m, sum, err := LoadMix(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 64 || !m.Interactive.Enabled || !m.Ingestion.Enabled || m.Ingestion.Source != "clinic-policies" || m.Ingestion.Op != IngestRebuild {
		t.Errorf("mix %+v", m)
	}
	if m.Interactive.Think() < 5*time.Second {
		t.Errorf("think time %s is not a human pace", m.Interactive.Think())
	}
	var booking, rag int
	languages := map[string]bool{}
	for _, s := range m.Interactive.Steps {
		languages[s.Language] = true
		if s.Class == classBooking {
			booking++
		} else {
			rag++
		}
	}
	if booking == 0 || rag == 0 || !languages["en"] || !languages["it"] {
		t.Errorf("booking %d, rag %d, languages %v", booking, rag, languages)
	}
}

func TestMixValidate(t *testing.T) {
	good := testMix(true, true)
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Mix){
		"no load at all": func(m *Mix) { m.Interactive.Enabled, m.Ingestion.Enabled = false, false },
		"no steps":       func(m *Mix) { m.Interactive.Steps = nil },
		"duplicate step": func(m *Mix) { m.Interactive.Steps[1].ID = m.Interactive.Steps[0].ID },
		"bad class":      func(m *Mix) { m.Interactive.Steps[0].Class = "chat" },
		"bad language":   func(m *Mix) { m.Interactive.Steps[0].Language = "fr" },
		"empty text":     func(m *Mix) { m.Interactive.Steps[0].Text = "" },
		"bad step id":    func(m *Mix) { m.Interactive.Steps[0].ID = "Book Free" },
		"negative think": func(m *Mix) { m.Interactive.ThinkMS = -1 },
		"bad op":         func(m *Mix) { m.Ingestion.Op = "delete" },
		"no timeout":     func(m *Mix) { m.Ingestion.TimeoutMS = 0 },
		"bad source":     func(m *Mix) { m.Ingestion.Source = "../etc" },
		"bad mix id":     func(m *Mix) { m.ID = "" },
		"negative pause": func(m *Mix) { m.Ingestion.PauseMS = -5 },
	}
	for name, edit := range cases {
		m := testMix(true, true)
		edit(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// one half may be switched off
	if err := testMix(false, true).Validate(); err != nil {
		t.Errorf("ingestion only: %v", err)
	}
	if err := testMix(true, false).Validate(); err != nil {
		t.Errorf("interactive only: %v", err)
	}
}

func TestLoadMixIsStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, _, err := LoadMix(write("unknown.json", `{"id":"x","surprise":1}`)); err == nil {
		t.Error("an unknown member was accepted")
	}
	if _, _, err := LoadMix(write("two.json", `{"id":"x"} {"id":"y"}`)); err == nil {
		t.Error("two documents were accepted")
	}
	if _, _, err := LoadMix(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}

// Writing a result and checking it again is a round trip; editing it by hand is
// caught, by the recorded observations disagreeing with the verdict.
func TestCheckCommand(t *testing.T) {
	// The results are built from fixed observations, not from a timed run, so the
	// test does not depend on the speed of the machine.
	cfg := testConfig(models.ProfileLite)
	cfg.Duration, cfg.Warmup = 4*time.Second, time.Second
	build := func(obs Observations) *Result {
		return newResult(cfg, cfg.Rules(true), Deps{RunID: "run_TEST"}, time.Now(), cfg.Duration, obs)
	}
	res := build(goodObs())
	if !res.Passed() {
		t.Fatalf("the fixed observations do not pass: %v", res.Failures)
	}
	dir := t.TempDir()
	path, err := WriteResult(dir, res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ResultExt) || !strings.Contains(filepath.Base(path), "soak_lite_decision-off_") {
		t.Errorf("file name %s", filepath.Base(path))
	}

	check := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := checkCommand(args, &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, out := check(dir); code != 0 {
		t.Fatalf("check of a fresh result: %d\n%s", code, out)
	}
	if code, _ := check("-reject-synthetic", dir); code != 1 {
		t.Errorf("-reject-synthetic accepted a synthetic result: %d", code)
	}
	if code, _ := check("-require-pass", dir); code != 0 {
		t.Errorf("-require-pass on a passing result: %d", code)
	}

	tamper := func(name string, edit func(*Result)) string {
		var r Result
		data, _ := os.ReadFile(path)
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		edit(&r)
		out, _ := json.Marshal(&r)
		p := filepath.Join(t.TempDir(), name+ResultExt)
		if err := os.WriteFile(p, out, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]func(*Result){
		"swap hidden":          func(r *Result) { r.Observations.Samples[1].Mem.SwapMiB = 99 },
		"failure deleted":      func(r *Result) { r.Observations.Samples[1].Mem.UsedMiB = 8190; r.Verdict = verdictPass },
		"verdict flipped":      func(r *Result) { r.Verdict = verdictFail },
		"not emulated":         func(r *Result) { r.Emulated = false },
		"wrong limits":         func(r *Result) { r.ContainerLimits.MemMB = 16384 },
		"easier headroom rule": func(r *Result) { r.Rules.RequiredHeadroomMiB = 100 },
		"synthetic denied":     func(r *Result) { r.Synthetic = false },
		"kind changed":         func(r *Result) { r.Kind = "alixai.bench.result" },
		"turns deleted":        func(r *Result) { r.Observations.Turns = nil },
		"completed but short":  func(r *Result) { r.Observations.Completed, r.DurationMS = true, 1 },
		"scorer label changed": func(r *Result) { r.Decision.Axis = DecisionOn },
	}
	for name, edit := range cases {
		if code, out := check(tamper(strings.ReplaceAll(name, " ", "-"), edit)); code != 1 {
			t.Errorf("%s: check passed a tampered result\n%s", name, out)
		}
	}

	// a valid file of a failed run is valid, and -require-pass is how a gate refuses it
	swapped := goodObs()
	swapped.Samples[1].Mem.SwapMiB = 8
	failed := build(swapped)
	fdir := t.TempDir()
	if _, err := WriteResult(fdir, failed); err != nil {
		t.Fatal(err)
	}
	if code, out := check(fdir); code != 0 {
		t.Errorf("a failed run's file is valid: %d\n%s", code, out)
	}
	if code, out := check("-require-pass", fdir); code != 1 || !strings.Contains(out, FailSwapUsed) {
		t.Errorf("-require-pass on a failed run: %d\n%s", code, out)
	}
	// finding nothing is an error
	if code, _ := check(t.TempDir()); code != 1 {
		t.Errorf("an empty directory: %d", code)
	}
	if code, _ := check(); code != 2 {
		t.Errorf("no argument: %d", code)
	}
}

// writeFastMix writes a mix file whose ingestion settles at once, for the command line.
func writeFastMix(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(testMix(true, true))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "fast-mix.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommandLineFake(t *testing.T) {
	mix, out := writeFastMix(t), t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--fake", "--duration", "700ms", "--profile", "lite", "--out", out, "--mix", mix,
		"--sample-interval", "30ms", "--warmup", "100ms",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PASS") || !strings.Contains(stdout.String(), "SYNTHETIC") {
		t.Errorf("report:\n%s", stdout.String())
	}
	var chk bytes.Buffer
	if c := run(context.Background(), []string{"check", out}, &chk, &chk); c != 0 {
		t.Errorf("check exit %d\n%s", c, chk.String())
	}

	// a failing run exits 1 and still writes its result
	out2 := t.TempDir()
	stdout.Reset()
	code = run(context.Background(), []string{
		"run", "--fake", "--duration", "300ms", "--profile", "balanced", "--decision", "on", "--out", out2, "--mix", mix,
		"--sample-interval", "30ms", "--warmup", "50ms", "--max-turn-p95-ms", "0.0001",
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stdout.String(), FailTurnLatency) {
		t.Errorf("exit %d\n%s", code, stdout.String())
	}
	if files, _ := filepath.Glob(filepath.Join(out2, "*"+ResultExt)); len(files) != 1 || !strings.Contains(files[0], "decision-on") {
		t.Errorf("result files %v", files)
	}
}

func TestCommandLineUsageErrors(t *testing.T) {
	mix := writeFastMix(t)
	out := t.TempDir()
	cases := map[string][]string{
		"no flags":                      {},
		"no duration":                   {"--profile", "lite", "--out", out},
		"unknown profile":               {"--duration", "1m", "--profile", "huge", "--out", out},
		"warm-up as long as the run":    {"--duration", "1m", "--warmup", "1m", "--profile", "lite", "--out", out, "--mix", mix},
		"scorer on the 4 GB profile":    {"--duration", "1m", "--profile", "4g", "--decision", "on", "--out", out, "--mix", mix},
		"scorer on lite, not qualified": {"--duration", "1m", "--profile", "lite", "--decision", "on", "--out", out, "--mix", mix},
		"unknown decision":              {"--duration", "1m", "--profile", "lite", "--decision", "auto", "--out", out, "--mix", mix},
		"missing mix":                   {"--duration", "1m", "--profile", "lite", "--out", out, "--mix", filepath.Join(out, "nope.json")},
		"an unexpected argument":        {"--duration", "1m", "--profile", "lite", "--out", out, "--mix", mix, "extra"},
		"a live run with no identity":   {"--duration", "1m", "--profile", "lite", "--out", out, "--mix", mix},
		"a live run with no key":        {"--duration", "1m", "--profile", "lite", "--out", out, "--mix", mix, "--issuer", "i", "--audience", "a", "--key-id", "k", "--tenant", "t", "--workspace", "w", "--principal", "p", "--container", "c"},
		"containers and pids":           {"--duration", "1m", "--profile", "lite", "--out", out, "--mix", mix, "--issuer", "i", "--audience", "a", "--key-id", "k", "--tenant", "t", "--workspace", "w", "--principal", "p", "--issuer-key", "k.pem", "--container", "c", "--pid", "1"},
		"negative threshold":            {"--duration", "1m", "--profile", "lite", "--out", out, "--mix", mix, "--fake", "--max-swap-mib", "-1"},
		"error rate above one":          {"--duration", "1m", "--profile", "lite", "--out", out, "--mix", mix, "--fake", "--max-error-rate", "2"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr); code != 2 {
				t.Errorf("exit %d, want 2\n%s%s", code, stdout.String(), stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "usage:") {
		t.Errorf("help: %d %q", code, stdout.String())
	}
	if files, _ := filepath.Glob(filepath.Join(out, "*")); len(files) != 0 {
		t.Errorf("a refused run wrote %v", files)
	}
}
