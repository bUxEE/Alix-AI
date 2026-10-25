package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// optFloat is a flag.Value for an optional float.
type optFloat struct{ v *float64 }

func (o *optFloat) String() string {
	if o == nil || o.v == nil {
		return ""
	}
	return fmt.Sprint(*o.v)
}

func (o *optFloat) Set(s string) error {
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil || f < 0 {
		return fmt.Errorf("%q is not a non-negative number", s)
	}
	o.v = &f
	return nil
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("decbench run", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgArg := fl.String("config", "all", "configuration: all or 1 to 5 (1 rules, 2 sub-1B scorer, 3 CLM-8B scorer, 4 direct controller, 5 scorer then controller)")
	scorer := fl.String("scorer", ScorerFake, "scorer: fake (scripted, synthetic), localai or clm (a running server on loopback)")
	corpus := fl.String("corpus", "", "fixture file (JSONL), e.g. tests/performance/decision/decision-v1.jsonl")
	out := fl.String("out", "", "output directory for the result file")
	profile := fl.String("profile", ProfileLite, "emulated hardware profile label: lite, balanced, high or 4g")
	artifact := fl.String("artifact", "", "scorer model file to hash into the result")
	endpoint := fl.String("endpoint", "", "scorer endpoint for localai or clm (default: the adapter's loopback default)")
	model := fl.String("model", "", "scorer model name for localai or clm")
	revision := fl.String("revision", "", "scorer model revision pin for localai or clm")
	timeoutMS := fl.Int64("timeout-ms", 250, "budget of one scorer call in milliseconds")
	minConf := fl.Float64("min-confidence", 0.85, "calibrated-confidence floor of the engine")
	var ctrlMS, audioMS, peakMB, residentMB, coldMS, warmMS optFloat
	fl.Var(&ctrlMS, "controller-ms", "measured per-call controller latency in ms, supplied from a live run (default: scripted)")
	fl.Var(&audioMS, "first-audio-ms", "measured first-audio time of the voice pipeline in ms, added to the first response")
	fl.Var(&peakMB, "peak-rss-mb", "measured peak RSS of the system in MB, supplied from a live run")
	fl.Var(&residentMB, "resident-model-mb", "measured resident model memory in MB, supplied from a live run")
	fl.Var(&coldMS, "cold-load-ms", "measured cold model load in ms, supplied from a live run")
	fl.Var(&warmMS, "warm-load-ms", "measured warm model load in ms, supplied from a live run")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: decbench run --config all|1|2|3|4|5 --scorer fake|localai|clm --corpus <jsonl> --out <dir> [flags]")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "decbench run: "+format+"\n", a...)
		return 2
	}
	if *corpus == "" || *out == "" || fl.NArg() != 0 {
		fl.Usage()
		return 2
	}
	switch *scorer {
	case ScorerFake, ScorerLocalAI, ScorerCLM:
	default:
		return usageErr("--scorer must be fake, localai or clm, got %q", *scorer)
	}
	prof, err := ParseProfile(*profile)
	if err != nil {
		return usageErr("%v", err)
	}
	specs, skipped, err := SelectConfigs(*cfgArg, *scorer)
	if err != nil {
		return usageErr("%v", err)
	}
	if *timeoutMS <= 0 || *minConf <= 0 || *minConf > 1 {
		return usageErr("--timeout-ms must be positive and --min-confidence in (0, 1]")
	}
	if *scorer != ScorerFake && *model == "" && usesScorer(specs) {
		return usageErr("--scorer %s needs --model", *scorer)
	}
	for _, s := range skipped {
		fmt.Fprintf(stderr, "decbench run: skipped %s\n", s)
	}

	c, err := LoadCorpus(*corpus)
	if err != nil {
		fmt.Fprintf(stderr, "decbench run: %v\n", err)
		return 1
	}
	opts := RunOptions{
		Configs: specs, Scorer: *scorer, Corpus: c, Profile: prof,
		Real:         RealScorerOptions{Endpoint: *endpoint, Model: *model, Revision: *revision, Timeout: time.Duration(*timeoutMS) * time.Millisecond},
		ControllerMS: ctrlMS.v, FirstAudioMS: audioMS.v, PeakRSSMB: peakMB.v, ResidentModelMB: residentMB.v,
		ColdLoadMS: coldMS.v, WarmLoadMS: warmMS.v,
		Timeout: time.Duration(*timeoutMS) * time.Millisecond, MinConfidence: *minConf,
	}
	if *artifact != "" {
		h, err := hashFile(*artifact)
		if err != nil {
			fmt.Fprintf(stderr, "decbench run: --artifact: %v\n", err)
			return 1
		}
		opts.ArtifactSHA256 = h
	}
	ctx := context.Background()
	if root, err := FindRepoRoot(); err == nil {
		opts.Git = CollectGit(ctx, root)
	}

	res, err := Run(ctx, opts)
	if err != nil {
		fmt.Fprintf(stderr, "decbench run: %v\n", err)
		return 1
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "decbench run: %v\n", err)
		return 1
	}
	data = append(data, '\n')

	// A run never writes a result its own check would reject.
	schema, err := LoadResultSchema("")
	if err != nil {
		fmt.Fprintf(stderr, "decbench run: %v\n", err)
		return 1
	}
	if problems := validateResultBytes(schema, data); len(problems) > 0 {
		fmt.Fprintf(stderr, "decbench run: the result is invalid (%s); nothing written\n", problems[0])
		return 1
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(stderr, "decbench run: %v\n", err)
		return 1
	}
	path := filepath.Join(*out, res.RunID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "decbench run: %v\n", err)
		return 1
	}
	summarize(stdout, res, path)
	return 0
}

func usesScorer(specs []ConfigSpec) bool {
	for _, s := range specs {
		if s.UsesScorer() {
			return true
		}
	}
	return false
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func pct(r Ratio) string {
	if r.Rate == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", *r.Rate*100, r.Passed, r.Total)
}

func ms(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", *p)
}

func summarize(w io.Writer, r *Result, path string) {
	tag := ""
	if r.Synthetic {
		tag = " SYNTHETIC (fake scorer: measures the harness, not a model)"
	}
	fmt.Fprintf(w, "decbench: %s  profile=%s emulated=%v scorer=%s%s\n", r.RunID, r.Profile, r.Emulated, r.Scorer.Kind, tag)
	for _, c := range r.Configurations {
		fmt.Fprintf(w, "  %d %-27s task %-16s ctrl-avoided %-16s first-response p50/p95/p99 %s/%s/%s ms (n=%d)\n",
			c.ID, c.Name, pct(c.Metrics.TaskCompletion), pct(c.Metrics.ControllerCallsAvoided),
			ms(c.Latency.FirstResponseMS.P50), ms(c.Latency.FirstResponseMS.P95), ms(c.Latency.FirstResponseMS.P99), c.Latency.FirstResponseMS.N)
	}
	fmt.Fprintf(w, "wrote %s\n", filepath.ToSlash(path))
}
