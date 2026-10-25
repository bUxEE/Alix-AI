package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// listFlag is a flag that may be given several times.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// pidFlag is a repeatable process id.
type pidFlag []int

func (p *pidFlag) String() string {
	s := make([]string, len(*p))
	for i, v := range *p {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ",")
}

func (p *pidFlag) Set(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%q is not a process id", v)
	}
	*p = append(*p, n)
	return nil
}

// defaultOut is where results go when --out is not given: the repository's result directory.
const defaultOut = "benchmarks/results/soak"

// runOptions are the flags of `soak run`.
type runOptions struct {
	duration       time.Duration
	profile        string
	out            string
	decision       string
	decisionQual   bool
	mixPath        string
	sampleInterval time.Duration
	warmup         time.Duration
	warmupSet      bool
	fake           bool

	baseURL    string
	metricsURL string
	containers listFlag
	pids       pidFlag
	dockerBin  string

	issuerKey       string
	keyID           string
	issuer          string
	audience        string
	application     string
	tenant          string
	workspace       string
	principal       string
	ingestPrincipal string
	roles           string
	scopes          string

	th Thresholds
}

func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var o runOptions
	fs := flag.NewFlagSet("soak run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.DurationVar(&o.duration, "duration", 0, "how long to run, for example 24h or 168h (required)")
	fs.StringVar(&o.profile, "profile", "", "hardware profile the run stands for: lite, balanced or 4g (required)")
	fs.StringVar(&o.out, "out", defaultOut, "directory for the result file")
	fs.StringVar(&o.decision, "decision", DecisionOff, "decision-layer axis: off (scorer disabled) or on (scorer enabled; balanced, or lite with --decision-qualified)")
	fs.BoolVar(&o.decisionQual, "decision-qualified", false, "assert that a qualified sub-1B scorer is configured (lite only)")
	fs.StringVar(&o.mixPath, "mix", "", "load mix file (default tests/performance/soak/mix.json)")
	fs.DurationVar(&o.sampleInterval, "sample-interval", 10*time.Second, "time between samples (use 1m for a 7-day run)")
	fs.DurationVar(&o.warmup, "warmup", 0, "time before steady state, excluded from the headroom and role checks (default the smaller of 2m and a tenth of the duration)")
	fs.BoolVar(&o.fake, "fake", false, "run against the harness's own fakes (synthetic result): tries the harness, soaks nothing")

	fs.StringVar(&o.baseURL, "base-url", "http://127.0.0.1:7470", "the stack's API (plain http only to loopback)")
	fs.StringVar(&o.metricsURL, "metrics-url", "", "the operator's loopback /metrics URL: the probe then also sees the governor's reload counters")
	fs.Var(&o.containers, "container", "container to sample with docker stats (repeat for every container of the stack; use names)")
	fs.Var(&o.pids, "pid", "host process to sample from /proc, Linux only (repeat; instead of --container)")
	fs.StringVar(&o.dockerBin, "docker-bin", "docker", "docker or podman, a bare name or an absolute path")

	fs.StringVar(&o.issuerKey, "issuer-key", "", "Ed25519 private key (PKCS#8 PEM) the application's context tokens are signed with (no default)")
	fs.StringVar(&o.keyID, "key-id", "", "key id the issuer key is registered under (kid)")
	fs.StringVar(&o.issuer, "issuer", "", "token issuer (iss) of the application")
	fs.StringVar(&o.audience, "audience", "", "the stack's deployment id (aud)")
	fs.StringVar(&o.application, "application", "clinic-booking", "application id")
	fs.StringVar(&o.tenant, "tenant", "", "tenant id")
	fs.StringVar(&o.workspace, "workspace", "", "workspace id")
	fs.StringVar(&o.principal, "principal", "", "principal id of the interactive session")
	fs.StringVar(&o.ingestPrincipal, "ingest-principal", "", "principal that re-ingests (must hold manage_sources in rag.operators); default --principal")
	fs.StringVar(&o.roles, "roles", "receptionist", "roles of the principal, comma separated")
	fs.StringVar(&o.scopes, "scopes", "availability.list,patients.find,appointments.book", "capability scopes of the interactive tokens, comma separated")

	th := DefaultThresholds()
	fs.Float64Var(&th.MaxSwapMiB, "max-swap-mib", th.MaxSwapMiB, "swap that may be in use (a certified profile uses none)")
	fs.BoolVar(&th.AllowUnmeasuredSwap, "allow-unmeasured-swap", th.AllowUnmeasuredSwap, "pass although swap could not be read from every sample (recorded in the result)")
	fs.Uint64Var(&th.MaxReloads, "max-reloads", th.MaxReloads, "model reloads that may happen (resource-budgets R3.5: none)")
	fs.Float64Var(&th.MaxErrorRate, "max-error-rate", th.MaxErrorRate, "share of turns, and of ingestions, that may fail")
	fs.Float64Var(&th.MaxSampleErrorRate, "max-sample-error-rate", th.MaxSampleErrorRate, "share of memory or stack readings that may fail")
	fs.Float64Var(&th.MaxTurnP95MS, "max-turn-p95-ms", th.MaxTurnP95MS, "fail above this p95 turn latency (0: report only)")
	fs.IntVar(&th.MaxQueueDepth, "max-queue-depth", th.MaxQueueDepth, "fail when more jobs than this are pending at once (0: report only)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "warmup" {
			o.warmupSet = true
		}
	})
	o.th = th
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "soak: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	cfg, err := o.config()
	if err != nil {
		fmt.Fprintf(stderr, "soak: %v\n", err)
		return 2
	}
	deps, cfg, cleanup, err := o.build(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "soak: %v\n", err)
		return 2
	}
	defer cleanup()
	deps.Host, deps.Git = CollectHost(), CollectGit(ctx, ".")

	fmt.Fprintf(stderr, "soak: %s on profile %s, decision %s, mix %s; sampling every %s, warm-up %s\n",
		cfg.Duration, cfg.Profile, cfg.Decision, cfg.Mix.ID, cfg.SampleInterval, cfg.Warmup)
	res, err := Run(ctx, cfg, deps)
	if err != nil {
		fmt.Fprintf(stderr, "soak: the run did not start: %v\n", err)
		return 1
	}
	path, werr := WriteResult(o.out, res)
	fmt.Fprint(stdout, res.Report())
	if werr != nil {
		fmt.Fprintf(stderr, "soak: writing the result: %v\n", werr)
		return 1
	}
	fmt.Fprintf(stdout, "result %s\n", path)
	if !res.Passed() {
		return 1
	}
	return 0
}

// config validates the flags that every run needs and builds the Config.
func (o *runOptions) config() (Config, error) {
	var missing []string
	if o.duration == 0 {
		missing = append(missing, "--duration")
	}
	if o.profile == "" {
		missing = append(missing, "--profile")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("%s required", strings.Join(missing, ", "))
	}
	p, err := ParseProfile(o.profile)
	if err != nil {
		return Config{}, err
	}
	mixPath := o.mixPath
	if mixPath == "" {
		if mixPath, err = FindDefaultMix("."); err != nil {
			return Config{}, err
		}
	}
	mix, sum, err := LoadMix(mixPath)
	if err != nil {
		return Config{}, err
	}
	warmup := o.warmup
	if !o.warmupSet {
		warmup = min(2*time.Minute, o.duration/10)
	}
	cfg := Config{
		Duration: o.duration, Profile: p, Decision: o.decision, DecisionQualified: o.decisionQual,
		SampleInterval: o.sampleInterval, Warmup: warmup, Mix: mix, MixSHA256: sum,
		Thresholds: o.th, Synthetic: o.fake,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// build wires the stack: the fakes, or the SDK driver, the memory source and
// the probe of a running one. cleanup releases what it started.
func (o *runOptions) build(cfg Config) (Deps, Config, func(), error) {
	if o.fake {
		f, err := newFakeStack(cfg.Profile, cfg.Mix, cfg.Decision)
		if err != nil {
			return Deps{}, cfg, nil, err
		}
		deps, err := f.Deps()
		if err != nil {
			f.Close()
			return Deps{}, cfg, nil, err
		}
		cfg.Target = TargetInfo{Driver: "fake", BaseURL: f.API.URL()}
		return deps, cfg, f.Close, nil
	}
	deps, err := o.buildLive(cfg)
	cfg.Target = TargetInfo{Driver: "sdk", BaseURL: o.baseURL}
	return deps, cfg, func() {}, err
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (o *runOptions) buildLive(cfg Config) (Deps, error) {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"--issuer-key", o.issuerKey}, {"--key-id", o.keyID}, {"--issuer", o.issuer}, {"--audience", o.audience},
		{"--tenant", o.tenant}, {"--workspace", o.workspace}, {"--principal", o.principal},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return Deps{}, fmt.Errorf("%s required for a running stack (the harness is a host application and mints context tokens)", strings.Join(missing, ", "))
	}
	if len(o.containers) > 0 && len(o.pids) > 0 {
		return Deps{}, errors.New("sample containers (--container) or host processes (--pid), not both")
	}
	var mem MemorySource
	var err error
	switch {
	case len(o.containers) > 0:
		mem, err = newDockerSource(o.dockerBin, o.containers)
	case len(o.pids) > 0:
		mem, err = newHostSource(o.pids)
	default:
		err = errors.New("--container or --pid is required: the harness samples the memory of the stack")
	}
	if err != nil {
		return Deps{}, err
	}

	key, err := LoadIssuerKey(o.issuerKey)
	if err != nil {
		return Deps{}, err
	}
	session, err := newSessionID()
	if err != nil {
		return Deps{}, err
	}
	ingestSession, err := newSessionID()
	if err != nil {
		return Deps{}, err
	}
	id := TokenIdentity{
		Issuer: o.issuer, Audience: o.audience, KeyID: o.keyID, Application: o.application,
		Tenant: o.tenant, Workspace: o.workspace, Principal: o.principal,
		Roles: splitList(o.roles), Scopes: splitList(o.scopes),
	}
	turnMinter, err := NewMinter(key, id, session)
	if err != nil {
		return Deps{}, err
	}
	ingestID := id
	if o.ingestPrincipal != "" {
		ingestID.Principal = o.ingestPrincipal
	}
	ingestMinter, err := NewMinter(key, ingestID, ingestSession)
	if err != nil {
		return Deps{}, err
	}
	turns, err := sdkgo.NewClient(sdkgo.ClientOptions{BaseURL: o.baseURL, GetContextToken: turnMinter.Provider()})
	if err != nil {
		return Deps{}, err
	}
	ingest, err := sdkgo.NewClient(sdkgo.ClientOptions{BaseURL: o.baseURL, GetContextToken: ingestMinter.Provider()})
	if err != nil {
		return Deps{}, err
	}
	drv, err := newSDKDriver(turns, ingest, session, cfg.Mix.Ingestion)
	if err != nil {
		return Deps{}, err
	}

	var probe StackProbe
	if o.metricsURL != "" {
		if probe, err = newMetricsProbe(o.metricsURL); err != nil {
			return Deps{}, err
		}
	} else {
		q, err := newOpsQueueProbe(o.baseURL, func(context.Context) (string, error) { return turnMinter.Mint("ops.queue.read") })
		if err != nil {
			return Deps{}, err
		}
		probe = readyzOpsProbe{roles: readyzProbe{c: turns}, queue: q}
	}
	return Deps{Mem: mem, Probe: probe, Driver: drv}, nil
}
