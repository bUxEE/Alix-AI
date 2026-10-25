// Command providercompare compares the external provider adapters (OpenAI, Anthropic, Gemini,
// Meta) on the shared corpora and measures the connected-mode footprint of the control plane under
// the experimental 4 GB profile (additive plan section 17 item 11; task X-23).
//
//	go run ./tools/providercompare                      # the comparison report, to standard output
//	go run ./tools/providercompare --out report.md      # ... to a file
//	go run ./tools/providercompare footprint --result-dir benchmarks/results/x --evidence footprint.json
//	go run ./tools/providercompare labels report.md     # check that every table row is labelled
//
// Nothing here calls a provider. Every adapter dials a loopback stub (127.0.0.1) that replays
// recorded wire fixtures after an injected delay, so every number is labelled `recorded` (read from
// a repository artefact) or `emulated` (produced by the stub, the emulated container or computed on
// an assumed turn), and a figure that needs a real key is `not measured` (X-H02).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func main() { os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr)) }

func cli(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "footprint":
			return footprintCmd(args[1:], stdout, stderr)
		case "labels":
			return labelsCmd(args[1:], stdout, stderr)
		case "report":
			args = args[1:]
		}
	}
	return reportCmd(args, stdout, stderr)
}

func reportCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "repository root (default: the directory holding go.mod above the working directory)")
	out := fs.String("out", "", "write the report here (default: standard output)")
	fpPath := fs.String("footprint", "", "a footprint evidence file written by `footprint --evidence` to include in section 3")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	r := *root
	if r == "" {
		var err error
		if r, err = repoRoot(); err != nil {
			fmt.Fprintf(stderr, "providercompare: %v\n", err)
			return 1
		}
	}
	cmp, err := Build(context.Background(), r)
	if err != nil {
		fmt.Fprintf(stderr, "providercompare: %v\n", err)
		return 1
	}
	var fp *Footprint
	if *fpPath != "" {
		b, err := os.ReadFile(*fpPath)
		if err == nil {
			fp = &Footprint{}
			err = json.Unmarshal(b, fp)
		}
		if err != nil {
			fmt.Fprintf(stderr, "providercompare: cannot read the footprint evidence: %v\n", err)
			return 1
		}
	}
	md := Render(cmp, fp, loadCoreIdle(r))
	if problems := CheckLabels(md); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, "providercompare: label check:", p)
		}
		return 1
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			fmt.Fprintf(stderr, "providercompare: %v\n", err)
			return 1
		}
		return 0
	}
	_, _ = io.WriteString(stdout, md)
	return 0
}

func labelsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: providercompare labels <report.md>")
		return 2
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "providercompare: %v\n", err)
		return 1
	}
	problems := CheckLabels(string(b))
	for _, p := range problems {
		fmt.Fprintln(stdout, p)
	}
	if len(problems) > 0 {
		return 1
	}
	fmt.Fprintln(stdout, "every table row carries a label")
	return 0
}

func footprintCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("footprint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o FootprintOptions
	var baseMS, jitterMS, sampleMS int
	resultDir := fs.String("result-dir", "", "directory for the bench-compatible result file (required)")
	evidence := fs.String("evidence", "", "file for the footprint evidence JSON (required)")
	fs.StringVar(&o.Root, "root", "", "repository root (default: found from the working directory)")
	fs.IntVar(&baseMS, "rtt-base-ms", 120, "injected network delay per call, milliseconds")
	fs.IntVar(&jitterMS, "rtt-jitter-ms", 80, "extra injected delay per call, uniform in [0, jitter), milliseconds")
	fs.Int64Var(&o.Seed, "seed", 20261008, "seed of the injected jitter")
	fs.IntVar(&o.Warmup, "warmup", 4, "warm-up requests, left out of every metric")
	fs.IntVar(&o.Limit, "limit", 0, "run only the first N routing-v0 cases (0: all)")
	fs.IntVar(&o.Workers, "workers", 8, "concurrent workers of the load phase (0 skips it)")
	fs.IntVar(&o.CallsPerWorker, "calls", 50, "calls per worker in the load phase")
	fs.IntVar(&sampleMS, "sample-ms", 250, "memory sampling interval, milliseconds")
	fs.StringVar(&o.CoreBin, "core-bin", "", "path of the alixai binary to start as `alixai serve` beside the harness (optional)")
	fs.StringVar(&o.WorkDir, "workdir", "", "writable directory for the core's configuration and data (needed with --core-bin)")
	fs.StringVar(&o.GitSHA, "git-sha", "", "40-hex commit the binaries were built from (the container has no git)")
	dirty := fs.Bool("git-dirty", false, "the tree was dirty when built")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *resultDir == "" || *evidence == "" || (o.CoreBin != "" && o.WorkDir == "") {
		fmt.Fprintln(stderr, "usage: providercompare footprint --result-dir D --evidence F [--core-bin B --workdir W] [flags]")
		return 2
	}
	o.LatencyBase = time.Duration(baseMS) * time.Millisecond
	o.LatencyJitter = time.Duration(jitterMS) * time.Millisecond
	o.SampleEvery = time.Duration(sampleMS) * time.Millisecond
	o.GitDirty = dirty
	res, fp, err := RunFootprint(context.Background(), o)
	if err != nil {
		fmt.Fprintf(stderr, "providercompare: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(*resultDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "providercompare: %v\n", err)
		return 1
	}
	name := fmt.Sprintf("fake_connected-stub_%s_%s.json", res.Profile, res.RunID)
	if err := writeJSON(filepath.Join(*resultDir, name), res, os.O_EXCL); err != nil {
		fmt.Fprintf(stderr, "providercompare: %v\n", err)
		return 1
	}
	if err := writeJSON(*evidence, fp, 0); err != nil {
		fmt.Fprintf(stderr, "providercompare: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s and %s (pass %d/%d, errors %d)\n", filepath.ToSlash(filepath.Join(*resultDir, name)), filepath.ToSlash(*evidence),
		res.Metrics.Pass.Passed, res.Metrics.Pass.Total, res.Errors.Count)
	return 0
}

func writeJSON(path string, v any, extraFlag int) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|extraFlag, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
