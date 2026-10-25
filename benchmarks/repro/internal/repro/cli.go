package repro

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

// Exit codes shared by the three commands.
const (
	ExitOK        = 0
	ExitFailed    = 1 // a check failed or a result deviates beyond tolerance
	ExitUsageOrIO = 2 // bad flags, unreadable manifest, nothing to do
)

type commonFlags struct {
	root     string
	manifest string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.root, "root", "", "repository root (default: the directory with go.mod above the working directory)")
	fs.StringVar(&c.manifest, "manifest", "", "manifest file (default: <root>/"+ManifestPath+")")
}

func (c *commonFlags) load() (string, *Manifest, error) {
	root := c.root
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", nil, err
		}
		if root, err = FindRoot(wd); err != nil {
			return "", nil, err
		}
	}
	path := c.manifest
	if path == "" {
		path = filepath.Join(root, filepath.FromSlash(ManifestPath))
	}
	m, err := LoadManifest(path)
	if err != nil {
		return "", nil, err
	}
	return root, m, nil
}

// RunVerifyInputs is the verify-inputs command: it checks the corpus and model
// hashes, the runtime pin and the published results against the manifest, and
// runs no model.
func RunVerifyInputs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify-inputs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	modelsDir := fs.String("models-dir", "", "directory that may hold the GGUF files; every file found there is hashed")
	require := fs.Bool("require-models", false, "with -models-dir: a model file that is missing is a failure")
	matrix := fs.Bool("matrix", false, "also print the published results by hardware, model, quantization, backend, context and configuration")
	if err := fs.Parse(args); err != nil {
		return ExitUsageOrIO
	}
	if fs.NArg() != 0 || (*require && *modelsDir == "") {
		fmt.Fprintln(stderr, "usage: verify-inputs [-root dir] [-manifest file] [-models-dir dir [-require-models]] [-matrix]")
		return ExitUsageOrIO
	}
	root, m, err := cf.load()
	if err != nil {
		fmt.Fprintln(stderr, "verify-inputs:", err)
		return ExitUsageOrIO
	}
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		fmt.Fprintln(stderr, "verify-inputs: model registry:", err)
		return ExitUsageOrIO
	}
	checks := Verify(context.Background(), m, VerifyOptions{Root: root, Registry: reg, ModelsDir: *modelsDir, RequireModels: *require})
	for _, c := range checks {
		if c.OK() {
			fmt.Fprintf(stdout, "ok   %s: %s\n", c.Name, c.Detail)
		} else {
			fmt.Fprintf(stdout, "FAIL %s: %s\n", c.Name, strings.ReplaceAll(c.Err.Error(), "\n", "; "))
		}
	}
	if *matrix {
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, Matrix(m, root))
	}
	if n := Failed(checks); n > 0 {
		fmt.Fprintf(stdout, "\n%d of %d checks FAILED: the inputs are not the published ones\n", n, len(checks))
		return ExitFailed
	}
	fmt.Fprintf(stdout, "\nall %d checks hold: corpus checksums, model hashes, runtime pin and published results match manifest %s\n", len(checks), m.ID)
	return ExitOK
}

// Matrix lists the published results by hardware, model, quantization, backend,
// context and configuration (master plan 17.3).
func Matrix(m *Manifest, root string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-44s %-16s %-30s %-8s %-22s %-5s %s\n", "run", "profile (emulated)", "host", "quant", "backend", "ctx", "configuration")
	for _, r := range m.Runs {
		host, backend := "?", "?"
		if p, err := safePath(root, r.Result); err == nil {
			if res, err := LoadResult(p); err == nil {
				host = fmt.Sprintf("%s, %d threads, %d MiB, %s/%s", res.Host.CPUModel, res.Host.LogicalCores, res.Host.RAMMB, res.Host.OS, res.Host.Arch)
				backend = res.Backend.Runtime + " " + res.Backend.RuntimeVersion + " / " + res.Backend.Name
			}
		}
		md, _ := m.Model(r.Model)
		cfg := "A (model alone)"
		if r.Gate {
			cfg = "G (scopegate-v1 in front)"
		}
		fmt.Fprintf(&b, "%-44s %-16s %-30s %-8s %-22s %-5d %s, %s, %s\n", r.ID, r.Profile, host, md.Quantization, backend, m.Bench.ContextToks, cfg, m.Bench.Constraint, m.Bench.PromptID)
	}
	return b.String()
}

// RunCompare is the compare command. With two file arguments it compares a
// published result with a reproduced one; with -reproduced it compares every
// run of the manifest with dir/<run id>/*.json.
func RunCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	reproduced := fs.String("reproduced", "", "directory with one subdirectory per run id, each holding the reproduced result (what reproduce.sh writes)")
	runs := fs.String("runs", "", "comma-separated run ids to compare (default: all)")
	allowMissing := fs.Bool("allow-missing", false, "a run without a reproduced result is not a failure (a partial reproduction)")
	strictPerf := fs.Bool("strict-perf", false, "latency and memory deviations fail the comparison (they are only reported by default: they depend on the host)")
	asJSON := fs.Bool("json", false, "write the findings as JSON")
	tolQ := fs.Float64("tol-quality", 0, "quality tolerance, absolute difference of a rate (default: the manifest's)")
	tolL := fs.Float64("tol-latency", 0, "latency tolerance, relative to the published value (default: the manifest's)")
	tolM := fs.Float64("tol-memory", 0, "memory tolerance, relative to the published value (default: the manifest's)")
	if err := fs.Parse(args); err != nil {
		return ExitUsageOrIO
	}
	usage := func() int {
		fmt.Fprintln(stderr, "usage: compare [flags] <published.json> <reproduced.json>\n       compare [flags] -reproduced <dir> [-runs id,id] [-allow-missing]")
		return ExitUsageOrIO
	}
	if (*reproduced == "") == (fs.NArg() == 0) || (fs.NArg() != 0 && fs.NArg() != 2) {
		return usage()
	}
	root, m, err := cf.load()
	if err != nil {
		fmt.Fprintln(stderr, "compare:", err)
		return ExitUsageOrIO
	}
	opts := CompareOptions{Tol: m.Tolerances, StrictPerf: *strictPerf}
	for _, t := range []struct {
		flagged bool
		v       float64
		dst     *float64
	}{{*tolQ != 0, *tolQ, &opts.Tol.QualityRate}, {*tolL != 0, *tolL, &opts.Tol.LatencyRel}, {*tolM != 0, *tolM, &opts.Tol.MemoryRel}} {
		if t.flagged {
			if t.v < 0 || t.v >= 1e6 {
				fmt.Fprintln(stderr, "compare: tolerances must be positive")
				return ExitUsageOrIO
			}
			*t.dst = t.v
		}
	}
	var cmps []Comparison
	if *reproduced == "" {
		pub, err := LoadResult(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(stderr, "compare:", err)
			return ExitUsageOrIO
		}
		rep, err := LoadResult(fs.Arg(1))
		if err != nil {
			fmt.Fprintln(stderr, "compare:", err)
			return ExitUsageOrIO
		}
		cmps = []Comparison{{RunID: filepath.Base(fs.Arg(1)), Findings: Compare(pub, rep, opts)}}
	} else {
		var only []string
		if *runs != "" {
			only = strings.Split(*runs, ",")
		}
		if cmps, err = CompareRuns(m, root, *reproduced, only, *allowMissing, opts); err != nil {
			fmt.Fprintln(stderr, "compare:", err)
			return ExitUsageOrIO
		}
		if len(cmps) == 0 {
			fmt.Fprintln(stderr, "compare: no run to compare")
			return ExitUsageOrIO
		}
	}
	failed := 0
	for _, c := range cmps {
		failed += c.Failed(opts.StrictPerf)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"within_tolerance": failed == 0, "tolerances": opts.Tol, "comparisons": cmps}); err != nil {
			fmt.Fprintln(stderr, "compare:", err)
			return ExitUsageOrIO
		}
	} else {
		for _, c := range cmps {
			fmt.Fprint(stdout, FormatComparison(c, opts.StrictPerf))
		}
		if failed == 0 {
			fmt.Fprintf(stdout, "\nwithin tolerance: %d run(s) (quality +-%.3f; latency +-%.0f%% and memory +-%.0f%% are reported, %s)\n",
				len(cmps), opts.Tol.QualityRate, opts.Tol.LatencyRel*100, opts.Tol.MemoryRel*100, map[bool]string{true: "and fail the comparison", false: "not failures"}[opts.StrictPerf])
		} else {
			fmt.Fprintf(stdout, "\n%d deviation(s) beyond tolerance: the published results did not reproduce\n", failed)
		}
	}
	if failed > 0 {
		return ExitFailed
	}
	return ExitOK
}

// RunPlan is the runplan command: one tab-separated line per run with everything
// reproduce.sh needs, so the script reads the manifest and never restates it.
func RunPlan(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runplan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf commonFlags
	cf.register(fs)
	profile := fs.String("profile", "", "only the runs of this profile (the published name or the CLI spelling, e.g. 4g)")
	runs := fs.String("runs", "", "comma-separated run ids (default: all)")
	if err := fs.Parse(args); err != nil {
		return ExitUsageOrIO
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: runplan [-root dir] [-manifest file] [-profile name] [-runs id,id]")
		return ExitUsageOrIO
	}
	_, m, err := cf.load()
	if err != nil {
		fmt.Fprintln(stderr, "runplan:", err)
		return ExitUsageOrIO
	}
	want := map[string]bool{}
	if *runs != "" {
		for _, id := range strings.Split(*runs, ",") {
			want[id] = false
		}
	}
	n := 0
	for _, r := range m.Runs {
		p := m.Profiles[r.Profile]
		if *profile != "" && *profile != r.Profile && *profile != p.CLI {
			continue
		}
		if len(want) > 0 {
			if _, ok := want[r.ID]; !ok {
				continue
			}
			want[r.ID] = true
		}
		md, _ := m.Model(r.Model)
		c, _ := m.Corpus(r.Corpus)
		b := m.Bench
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Model, p.CLI, r.Gate, md.Filename, md.Definition, c.Path, b.Constraint, b.ContextToks, b.MaxToks, b.Warmup, b.Timeout, b.SampleInterval,
			m.Runtime.Compose, p.Compose, m.Runtime.Override)
		n++
	}
	for id, found := range want {
		if !found {
			fmt.Fprintf(stderr, "runplan: run %q is not in the manifest\n", id)
			return ExitUsageOrIO
		}
	}
	if n == 0 {
		fmt.Fprintln(stderr, "runplan: no run selected")
		return ExitUsageOrIO
	}
	return ExitOK
}
