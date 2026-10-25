package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
)

// runOptions are the flags of `bench run`.
type runOptions struct {
	provider     string
	model        string
	profile      string
	corpus       string
	out          string
	role         string
	constraint   string
	prompt       string
	gate         bool
	stream       bool
	warmup       int
	limit        int
	maxToks      int
	promptBudget int
	timeout      time.Duration
	artifact     string
	localaiModel string
	endpoint     string
	apiKeyRef    string
	contextSize  int
	container    string
	dockerBin    string
	sampleEvery  time.Duration
	schemas      string
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	var o runOptions
	fs := flag.NewFlagSet("bench run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.provider, "provider", "", "fake (scripted, measures the harness only) or localai (a running LocalAI)")
	fs.StringVar(&o.model, "model", "", "model registry id (models/registry/<id>.yaml); the fake provider also accepts 'fake'")
	fs.StringVar(&o.profile, "profile", "", "hardware profile the run emulates: lite, balanced or 4g (experimental-4g)")
	fs.StringVar(&o.corpus, "corpus", "", "corpus file (JSONL)")
	fs.StringVar(&o.out, "out", "", "directory for the result file")
	fs.StringVar(&o.role, "role", "controller", "model role: controller or router")
	fs.StringVar(&o.constraint, "constraint", ConstraintSchema, "output constraint: schema (compact JSON Schema), grammar (GBNF) or none")
	fs.StringVar(&o.prompt, "prompt", PromptID, "prompt template: "+strings.Join(PromptIDs, " or ")+" (default: the newest; the older one stays reproducible)")
	fs.BoolVar(&o.gate, "gate", false, "put the deterministic out-of-scope gate (internal/dialogue/scopegate) in front of the model: a case it refuses is answered with the typed refusal and never sent (warm-up bypasses it)")
	fs.BoolVar(&o.stream, "stream", true, "stream responses (needed for time to first token)")
	fs.IntVar(&o.warmup, "warmup", 1, "requests sent first and left out of every metric (the first one loads the model)")
	fs.IntVar(&o.limit, "limit", 0, "run only the first N cases (0: all)")
	fs.IntVar(&o.maxToks, "max-toks", 256, "cap on generated tokens per request")
	fs.IntVar(&o.promptBudget, "prompt-budget", 4096, "cap on the estimated prompt of every request")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Minute, "timeout of one request")
	fs.StringVar(&o.artifact, "artifact", "", "path of the model file: its SHA-256 is computed and must equal the registry's pinned hash")
	fs.StringVar(&o.localaiModel, "localai-model", "", "name LocalAI knows the model by (default: the registry id)")
	fs.StringVar(&o.endpoint, "endpoint", "", "LocalAI endpoint (default: $ALIXAI_LOCALAI_URL, else http://127.0.0.1:8080)")
	fs.StringVar(&o.apiKeyRef, "api-key-ref", "", "LocalAI API key reference, env:NAME or file:PATH (default: env:ALIXAI_LOCALAI_KEY when that variable is set); a key itself is never accepted")
	fs.IntVar(&o.contextSize, "context-size", 0, "context window LocalAI was configured with, recorded in the result (default: the registry's budget)")
	fs.StringVar(&o.container, "docker-container", "", "sample this container's memory with docker stats while the run lasts")
	fs.StringVar(&o.dockerBin, "docker-bin", "", "the docker (or podman) CLI: a bare name or an absolute path (default: docker from PATH)")
	fs.DurationVar(&o.sampleEvery, "sample-interval", 2*time.Second, "interval between memory samples")
	fs.StringVar(&o.schemas, "schemas", "", "schemas directory (default: schemas/ beside go.mod)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: bench run --provider fake|localai --model <registry id> --profile lite|balanced|4g --corpus <file> --out <dir> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "bench run: "+format+"\n", a...)
		return 2
	}
	switch {
	case fs.NArg() > 0:
		return usageErr("unexpected argument %q", fs.Arg(0))
	case o.provider != "fake" && o.provider != "localai":
		return usageErr("--provider must be fake or localai")
	case o.model == "":
		return usageErr("--model is required")
	case o.corpus == "":
		return usageErr("--corpus is required")
	case o.out == "":
		return usageErr("--out is required")
	case o.role != string(providers.RoleController) && o.role != string(providers.RoleRouter):
		return usageErr("--role must be controller or router")
	case o.constraint != ConstraintSchema && o.constraint != ConstraintGrammar && o.constraint != ConstraintNone:
		return usageErr("--constraint must be schema, grammar or none")
	case !ValidPromptID(o.prompt):
		return usageErr("--prompt must be one of %s", strings.Join(PromptIDs, ", "))
	case o.warmup < 0 || o.limit < 0 || o.contextSize < 0:
		return usageErr("--warmup, --limit and --context-size must not be negative")
	case o.maxToks < 1 || o.promptBudget < 1 || o.timeout <= 0:
		return usageErr("--max-toks, --prompt-budget and --timeout must be positive")
	case o.container != "" && o.sampleEvery < 500*time.Millisecond:
		return usageErr("--sample-interval must be at least 500ms")
	}
	profile, err := ParseProfile(o.profile)
	if err != nil {
		return usageErr("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := execute(ctx, o, profile, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "bench run: %v\n", err)
		return 1
	}
	return 0
}

// execute wires the options to a Run and writes the result.
func execute(ctx context.Context, o runOptions, profile models.Profile, stdout, stderr io.Writer) error {
	schemasDir, err := FindSchemasDir(o.schemas)
	if err != nil {
		return err
	}
	proposal, err := LoadProposalSchema(schemasDir)
	if err != nil {
		return err
	}
	resultSchema, err := LoadResultSchema(schemasDir)
	if err != nil {
		return err
	}
	corpus, err := LoadCorpus(o.corpus)
	if err != nil {
		return err
	}

	runtimeName := ""
	if o.provider == "localai" {
		runtimeName = o.localaiModel
		if runtimeName == "" {
			runtimeName = o.model
		}
	}
	m, err := ResolveModel(ctx, o.provider, o.model, o.artifact, runtimeName)
	if err != nil {
		return err
	}
	if m.Entry != nil {
		want := models.RoleConversationalController
		if o.role == string(providers.RoleRouter) {
			want = models.RoleTypedRouting
		}
		if !m.Entry.HasRole(want) {
			fmt.Fprintf(stderr, "bench: warning: registry entry %s does not list the role %s\n", m.Entry.ID, want)
		}
	}

	var (
		gen            providers.TextGenerator
		runtimeVersion string
	)
	switch o.provider {
	case "fake":
		gen = newOracle(corpus, fake.WithModel(o.model))
	case "localai":
		client, version, err := connectLocalAI(ctx, o, runtimeName, stderr)
		if err != nil {
			return err
		}
		gen, runtimeVersion = client, version
	}

	var memory *MemoryPlan
	if o.container != "" {
		src, err := newDockerStats(o.container, o.dockerBin)
		if err != nil {
			return err
		}
		memory = &MemoryPlan{Source: src, Container: o.container, Interval: o.sampleEvery}
	}

	var configured *int
	if o.contextSize > 0 {
		configured = intPtr(o.contextSize)
	} else {
		configured = m.ContextBudget
	}

	git := GitInfo{}
	if root, err := FindRepoRoot(); err == nil {
		git = CollectGit(ctx, root)
	}

	cfg := RunConfig{
		Provider: o.provider, Gen: gen, Clock: clock.Real{},
		Corpus: corpus, Limit: o.limit, Warmup: o.warmup,
		Role: providers.Role(o.role), Constraint: o.constraint, Prompt: o.prompt, Gate: o.gate, Stream: o.stream,
		MaxToks: o.maxToks, PromptBudget: o.promptBudget, Timeout: o.timeout,
		Profile: profile, Model: m.Model, Backend: backendFor(o.provider, m.Entry, runtimeVersion),
		ContextConfigured: configured, NativeMax: m.NativeMax,
		Proposal: proposal, Memory: memory,
		Host: CollectHost(), Git: git,
		Progress: func(done, total int) {
			if done == total || done%10 == 0 {
				fmt.Fprintf(stderr, "bench: %d/%d cases\n", done, total)
			}
		},
	}
	res, err := Run(ctx, cfg)
	if err != nil {
		return err
	}
	data, err := marshalResult(res)
	if err != nil {
		return err
	}
	// The harness never writes a result its own check would refuse.
	if problems := validateResultBytes(resultSchema, data); len(problems) > 0 {
		return fmt.Errorf("internal error: the result does not pass its own check:\n  %s", strings.Join(problems, "\n  "))
	}
	path, err := writeResultFile(o.out, res, data)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, Summary(res))
	fmt.Fprintf(stdout, "result: %s\n", filepath.ToSlash(path))
	return nil
}

// connectLocalAI builds the adapter, probes it and returns it with the runtime
// version it reported. A runtime that is not ready, or that lacks the model, is
// an error before any case is sent: a run against nothing measures nothing.
func connectLocalAI(ctx context.Context, o runOptions, runtimeName string, stderr io.Writer) (*localai.Client, string, error) {
	endpoint := o.endpoint
	if endpoint == "" {
		endpoint = os.Getenv("ALIXAI_LOCALAI_URL")
	}
	keyRef := o.apiKeyRef
	if keyRef == "" {
		if _, ok := os.LookupEnv("ALIXAI_LOCALAI_KEY"); ok {
			keyRef = "env:ALIXAI_LOCALAI_KEY"
		}
	}
	client, err := localai.New(localai.Config{
		Endpoint:       endpoint,
		APIKeyRef:      keyRef,
		Models:         localai.ModelSet{Controller: runtimeName, Router: runtimeName},
		RequestTimeout: o.timeout,
	})
	if err != nil {
		return nil, "", err
	}
	rep, err := client.Health(ctx)
	if err != nil {
		return nil, "", err
	}
	switch rep.Status {
	case providers.HealthOK:
	case providers.HealthDegraded:
		fmt.Fprintf(stderr, "bench: warning: LocalAI is degraded: %s\n", rep.Reason)
	default:
		return nil, "", fmt.Errorf("LocalAI is not ready: %s (is the model installed under the name %q? use --localai-model)", rep.Reason, runtimeName)
	}
	return client, rep.Version, nil
}

// marshalResult renders a result as indented JSON.
func marshalResult(res *Result) ([]byte, error) {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("cannot encode the result: %w", err)
	}
	return append(data, '\n'), nil
}

var fileNameUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// resultFileName names a result file after its provider, model, profile and
// run id, with nothing a file system could object to (no colon, no separator).
func resultFileName(res *Result) string {
	part := func(s string) string { return fileNameUnsafe.ReplaceAllString(strings.ToLower(s), "_") }
	return fmt.Sprintf("%s_%s_%s_%s.json", part(res.Provider.Name), part(res.Model.RegistryID), part(res.Profile), part(res.RunID))
}

// writeResultFile writes data into dir (created if needed) as a new file and
// returns its path. It writes a temporary file and renames it, so a reader
// never sees a half-written result, and it never overwrites an existing file.
func writeResultFile(dir string, res *Result, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := filepath.Join(dir, resultFileName(res))
	if _, err := os.Lstat(final); err == nil {
		return "", fmt.Errorf("%s already exists", final)
	}
	tmp, err := os.CreateTemp(dir, ".bench-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		return "", errors.Join(werr, cerr)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return final, nil
}

// Summary renders the headline numbers of a result for the terminal.
func Summary(res *Result) string {
	var sb strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&sb, format+"\n", args...) }
	label := ""
	if res.Synthetic {
		label = "  [synthetic: fake provider, measures the harness only]"
	}
	w("run %s%s", res.RunID, label)
	w("  model %s  provider %s  profile %s (emulated: %v)  corpus %s (%d cases)",
		res.Model.RegistryID, res.Provider.Name, res.Profile, res.Emulated, res.Corpus.ID, res.Corpus.CasesRun)
	m := res.Metrics
	w("  schema valid         %s", fmtRatio(m.SchemaValid))
	w("  capability routing   %s", fmtRatio(m.CapabilityRouting))
	w("  intent               %s", fmtRatio(m.Intent))
	w("  field exact match    %s", fmtRatio(m.FieldExactMatch))
	w("  field value accuracy %s", fmtRatio(m.FieldValueAccuracy))
	w("  catalogue conformance %s", fmtRatio(m.CatalogueConformity))
	w("  refusal correctness  %s", fmtRatio(m.RefusalCorrectness))
	w("  injection resistance %s", fmtRatio(m.InjectionResistance))
	w("  pass                 %s", fmtRatio(m.Pass))
	w("  latency p50/p95      %s / %s ms   ttft p50 %s ms   %s tok/s",
		fmtNum(res.Latency.TotalMS.P50), fmtNum(res.Latency.TotalMS.P95), fmtNum(res.Latency.TTFTMS.P50), fmtNum(res.Latency.ToksPerSecond))
	if res.Memory.Sampled {
		w("  memory (docker)      peak %s MiB   steady %s MiB", fmtNum(res.Memory.PeakRSSMB), fmtNum(res.Memory.SteadyRSSMB))
	}
	if res.Errors.Count > 0 {
		w("  errors               %d of %d cases %v", res.Errors.Count, len(res.Cases), res.Errors.ByCode)
	}
	return sb.String()
}

func fmtRatio(r Ratio) string {
	if r.Rate == nil {
		return fmt.Sprintf("%d/%d (n/a)", r.Passed, r.Total)
	}
	return fmt.Sprintf("%d/%d (%.1f%%)", r.Passed, r.Total, *r.Rate*100)
}

func fmtNum(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", *p)
}
