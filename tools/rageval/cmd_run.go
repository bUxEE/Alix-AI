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
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/query"
)

// defaultEmbeddingModel is the Lite embedding model (ADR 0010).
const defaultEmbeddingModel = "granite-embedding-97m-multilingual-r2-q4_k_m"

// runOptions are the flags of `rageval run`.
type runOptions struct {
	embedder       string
	embeddingModel string
	localaiModel   string
	endpoint       string
	apiKeyRef      string
	corpus         string
	out            string
	profile        string
	mode           string
	k              string
	vectorMinScore float64
	warmup         int
	timeout        time.Duration
	scaleSizes     string
	smallScopes    int
	scaleQueries   int
	scalePooled    int
	scaleTimeout   time.Duration
	scaleInProcess bool
	// scaleReaderCacheKiB and scalePinScopes are the two knobs of the
	// reader-cache experiment (see ScaleConfig).
	// quantCheck also measures the recall of the int8 stored form against the exact
	// float scan on the model's own vectors (ADR 0011's bar; quant.go).
	quantCheck          bool
	quantDocs           string
	ragCaps             string
	scaleReaderCacheKiB int
	scalePinScopes      int
	// dockerContainer, when set, is a running container whose memory is sampled
	// with docker stats for the whole run (dockerBin is the CLI, dockerInterval
	// the sampling interval).
	dockerContainer string
	dockerBin       string
	dockerInterval  time.Duration
	seed            uint64
	schemas         string
	tmp             string
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	var o runOptions
	fs := flag.NewFlagSet("rageval run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.embedder, "embedder", "", "fake (hashed bag of words, measures the harness and the pipeline) or localai (a running LocalAI)")
	fs.StringVar(&o.embeddingModel, "embedding-model", "", "registry id of the embedding model (default for localai: "+defaultEmbeddingModel+")")
	fs.StringVar(&o.localaiModel, "localai-model", "", "name LocalAI knows the embedding model by (default: the registry id)")
	fs.StringVar(&o.endpoint, "endpoint", "", "LocalAI endpoint (default: $ALIXAI_LOCALAI_URL, else http://127.0.0.1:8080)")
	fs.StringVar(&o.apiKeyRef, "api-key-ref", "", "LocalAI API key reference, env:NAME or file:PATH (default: env:ALIXAI_LOCALAI_KEY when that variable is set); a key itself is never accepted")
	fs.StringVar(&o.corpus, "corpus", "", "corpus directory (corpus.json, questions.jsonl, docs/)")
	fs.StringVar(&o.out, "out", "", "directory for the result files")
	fs.StringVar(&o.profile, "profile", "lite", "hardware profile the run stands for: lite, balanced or 4g (experimental-4g); no cap is applied")
	fs.StringVar(&o.mode, "mode", ModeHybrid, "retrieval: fts (full-text only), hybrid (full-text and vectors) or rerank (hybrid and the harness's lexical reranker: no model, so the result is labelled synthetic)")
	fs.StringVar(&o.ragCaps, "rag-caps", CapsDefault, "RAG caps of a profile (resource-budgets 6.3): default (the pipeline's: 8 items, 3,000 tokens), lite (4 items, 1,000 tokens, 24 candidates a leg), balanced (6, 2,000, 48) or experimental-4g (3, 480, 12); the k of recall@k then stops at the items a package holds")
	fs.StringVar(&o.k, "k", "1,3,5,8", "cut-offs of recall@k and nDCG@k, each at most the items an evidence package holds")
	fs.Float64Var(&o.vectorMinScore, "vector-min-score", 0, "floor on the cosine score of a vector hit (query.vector_min_score); model-specific, calibrate before raising it")
	fs.IntVar(&o.warmup, "warmup", 1, "questions asked first and left out of every metric")
	fs.DurationVar(&o.timeout, "timeout", 2*time.Minute, "timeout of one request to LocalAI")
	fs.StringVar(&o.scaleSizes, "scale-sizes", "", "chunks per scope of the scale sweep, comma separated; none turns the sweep off (default: 1000,20000,50000 with the fake embedder; 1000 with localai, whose sweep embeds every chunk with the model)")
	fs.IntVar(&o.smallScopes, "scale-small-scopes", -1, "extra scopes of the smallest size, so the corpus is larger than any scope (default: 29, which makes the default corpus 100,000 chunks, with the fake embedder; 0 with localai)")
	fs.IntVar(&o.scaleQueries, "scale-queries", 30, "timed questions per kind (short, long) per scope size")
	fs.IntVar(&o.scalePooled, "scale-pooled", 200, "timed questions across the whole synthetic corpus")
	fs.DurationVar(&o.scaleTimeout, "scale-timeout", 30*time.Minute, "time allowed to one phase of the scale sweep")
	fs.BoolVar(&o.quantCheck, "quant-check", false, "also measure the recall@10 of the int8 stored form against the exact float scan on the embedder's own vectors (ADR 0011's 0.95 bar); needs hybrid or rerank")
	fs.StringVar(&o.quantDocs, "quant-docs", "", "with --quant-check: a directory of Markdown files whose chunks (up to 1,000) and section headings make one more, natural-language set")
	fs.IntVar(&o.scaleReaderCacheKiB, "scale-reader-cache-kib", 0, "page cache of each reader connection in the query phases of the sweep, in KiB (0: the store's default, 2000, the Lite configuration)")
	fs.IntVar(&o.scalePinScopes, "scale-pin-scopes", 0, "ask the questions of a size's bucket of its first N scopes only, instead of rotating over all of them (0: rotate); the corpus-wide row always rotates")
	fs.StringVar(&o.dockerContainer, "docker-container", "", "name of a running LocalAI container whose memory is sampled with docker stats for the whole run (cgroup usage, not VmRSS)")
	fs.StringVar(&o.dockerBin, "docker-bin", "", "container CLI for --docker-container: docker or podman, a bare name or an absolute path (default: docker)")
	fs.DurationVar(&o.dockerInterval, "docker-interval", 2*time.Second, "interval of the container memory samples")
	fs.BoolVar(&o.scaleInProcess, "scale-in-process", false, "run the scale phases in this process (their memory figures are then not their own)")
	fs.Uint64Var(&o.seed, "seed", 1, "seed of the synthetic scale corpus and its questions")
	fs.StringVar(&o.schemas, "schemas", "", "schemas directory (default: schemas/ beside go.mod)")
	fs.StringVar(&o.tmp, "tmp", "", "directory for the throwaway databases (default: the system's temporary directory)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: rageval run --embedder fake|localai --corpus <dir> --out <dir> [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "rageval run: "+format+"\n", a...)
		return 2
	}
	switch {
	case fs.NArg() > 0:
		return usageErr("unexpected argument %q", fs.Arg(0))
	case o.embedder != "fake" && o.embedder != "localai":
		return usageErr("--embedder must be fake or localai")
	case o.corpus == "":
		return usageErr("--corpus is required")
	case o.out == "":
		return usageErr("--out is required")
	case o.mode != ModeFTS && o.mode != ModeHybrid && o.mode != ModeRerank:
		return usageErr("--mode must be fts, hybrid or rerank")
	case o.warmup < 0 || o.smallScopes < -1 || o.scaleQueries < 1 || o.scalePooled < 1:
		return usageErr("--warmup and --scale-small-scopes must not be negative; --scale-queries and --scale-pooled must be positive")
	case o.timeout <= 0 || o.scaleTimeout <= 0 || o.dockerInterval <= 0:
		return usageErr("--timeout, --scale-timeout and --docker-interval must be positive")
	case o.scalePinScopes < 0 || o.scaleReaderCacheKiB < 0:
		return usageErr("--scale-pin-scopes and --scale-reader-cache-kib must not be negative")
	case o.quantDocs != "" && !o.quantCheck:
		return usageErr("--quant-docs belongs to --quant-check")
	case o.quantCheck && o.mode == ModeFTS:
		return usageErr("--quant-check measures an embedder's vectors: use --mode hybrid or rerank")
	}
	if o.embedder == "fake" && o.embeddingModel != "" && o.embeddingModel != fakeModelID {
		return usageErr("--embedding-model is for --embedder localai; the fake embedder's model is %q", fakeModelID)
	}
	if o.embedder == "localai" && o.embeddingModel == "" {
		o.embeddingModel = defaultEmbeddingModel
	}
	ks, err := parseInts(o.k)
	if err != nil || len(ks) == 0 {
		return usageErr("--k: %v", orElse(err, errors.New("no value")))
	}
	var capCfg query.Config = query.DefaultConfig()
	if err := applyRAGCaps(&capCfg, o.ragCaps); err != nil {
		return usageErr("%v", err)
	}
	kGiven := false
	fs.Visit(func(f *flag.Flag) { kGiven = kGiven || f.Name == "k" })
	if !kGiven {
		ks = capKs(ks, capCfg.MaxItems)
	}
	// The sweep's defaults depend on the embedder: the fake costs microseconds a
	// chunk, a model costs a good fraction of a second for a handful of them.
	sizesArg := strings.TrimSpace(o.scaleSizes)
	if sizesArg == "" {
		sizesArg = "1000,20000,50000"
		if o.embedder == "localai" {
			sizesArg = "1000"
		}
	}
	if o.smallScopes < 0 {
		o.smallScopes = 29
		if o.embedder == "localai" {
			o.smallScopes = 0
		}
	}
	var sizes []int
	if s := sizesArg; s != "none" {
		if sizes, err = parseInts(s); err != nil || len(sizes) == 0 {
			return usageErr("--scale-sizes: %v", orElse(err, errors.New("no value")))
		}
	}
	profile, err := ParseProfile(o.profile)
	if err != nil {
		return usageErr("%v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := execute(ctx, o, profile, ks, sizes, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "rageval run: %v\n", err)
		return 1
	}
	return 0
}

func orElse(err, alt error) error {
	if err != nil {
		return err
	}
	return alt
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", f)
		}
		out = append(out, n)
	}
	return out, nil
}

// runEnv is everything the build of a result needs besides the corpus and the
// evaluation: it is what the command line decided.
type runEnv struct {
	opts        runOptions
	profile     models.Profile
	ks          []int
	qcfg        query.Config
	acfg        answer.Config
	ccfg        chunk.Config
	embedderKey string // fake | localai
	modelID     string // the id the index records
	provider    string
	model       resolved
	backend     BackendInfo
	embedder    providers.Embedder
	pinned      *pinnedModel
	gen         *extractiveGenerator
}

// execute wires the options to an evaluation and writes the result files.
func execute(ctx context.Context, o runOptions, profile models.Profile, ks, sizes []int, stdout, stderr io.Writer) error {
	schemasDir, err := FindSchemasDir(o.schemas)
	if err != nil {
		return err
	}
	benchSchema, err := LoadBenchSchema(schemasDir)
	if err != nil {
		return err
	}
	corpus, err := LoadCorpus(o.corpus)
	if err != nil {
		return err
	}

	env := &runEnv{
		opts: o, profile: profile, ks: ks, qcfg: query.DefaultConfig(), acfg: answer.DefaultConfig(),
		ccfg: chunk.DefaultConfig(), embedderKey: o.embedder, gen: newExtractiveGenerator(),
	}
	if err := applyRAGCaps(&env.qcfg, o.ragCaps); err != nil {
		return err
	}
	env.qcfg.VectorMinScore = o.vectorMinScore
	if err := env.qcfg.Validate(); err != nil {
		return fmt.Errorf("query configuration: %w", err)
	}

	// LocalAI is contacted only when a model takes part: with full-text only
	// retrieval nothing real is measured but the pipeline itself, and the result
	// is labelled as the fake's (synthetic) whatever --embedder says.
	var runtimeVersion, runtimeName string
	useLocalAI := o.embedder == "localai" && o.mode != ModeFTS
	switch {
	case !useLocalAI:
		env.provider, env.modelID = "fake", fakeModel
		env.embedder = newHashEmbedder()
		if env.model, err = ResolveModel("fake", fakeModelID, ""); err != nil {
			return err
		}
	default:
		env.provider, env.modelID = "localai", o.embeddingModel
		runtimeName = o.localaiModel
		if runtimeName == "" {
			runtimeName = o.embeddingModel
		}
		if env.model, err = ResolveModel("localai", o.embeddingModel, runtimeName); err != nil {
			return err
		}
		emb, version, err := localaiEmbedder(ctx, o, stderr)
		if err != nil {
			return err
		}
		env.embedder, runtimeVersion = emb, version
		env.pinned, _ = emb.(*pinnedModel)
	}
	if o.mode == ModeFTS {
		env.embedder = nil
	}
	env.backend = backendFor(env.provider, env.model.Entry, runtimeVersion)

	// The container's memory, from before the first question to after the last
	// phase: the baseline is the idle container with the model already loaded (the
	// probe embedding of localaiEmbedder loaded it).
	var memc *memCollector
	if o.dockerContainer != "" {
		src, err := newMemorySource(o.dockerContainer, o.dockerBin)
		if err != nil {
			return err
		}
		if memc, err = startMemory(ctx, src, o.dockerContainer, o.dockerInterval); err != nil {
			return err
		}
		defer memc.stop(ctx)
	}

	started := time.Now().UTC()
	root, _ := FindRepoRoot()
	git := GitInfo{}
	if root != "" {
		git = CollectGit(ctx, root)
	}
	host := CollectHost()

	progress := func(done, total int) {
		if done == 1 && memc != nil {
			memc.markMeasure() // the first question has been answered: the container is warm
		}
		if done == total || done%10 == 0 {
			fmt.Fprintf(stderr, "rageval: %d/%d questions\n", done, total)
		}
	}
	fmt.Fprintf(stderr, "rageval: corpus %s: %d questions, %d sources, %d documents\n",
		corpus.Manifest.ID, len(corpus.Questions), len(corpus.Manifest.Sources), corpus.Documents())
	ev, err := RunEval(ctx, corpus, EvalConfig{
		Mode: o.mode, K: ks, Query: env.qcfg, Ans: env.acfg,
		Embedder: env.embedder, Model: env.modelID, Gen: env.gen, Warmup: o.warmup, TempDir: o.tmp,
	}, progress)
	if err != nil {
		return err
	}
	var mem RAGMemory
	mem.Source, mem.Covers = "os-process", memoryCovers
	if rss, peak, ok := processMemory(); ok {
		mem.Measured, mem.RSSMB, mem.PeakRSSMB = true, round3(mib(rss)), round3(mib(peak))
	}

	var quant *Quantization
	if o.quantCheck {
		fmt.Fprintln(stderr, "rageval: quantisation check")
		if quant, err = runQuantCheck(ctx, env.embedder, env.provider, corpus, o.seed, o.quantDocs); err != nil {
			return fmt.Errorf("quantisation check: %w", err)
		}
	}

	var scale *ScaleResult
	if len(sizes) > 0 {
		embSpec := embedderSpec{Kind: "none"}
		switch {
		case o.mode == ModeFTS:
		case env.provider == "fake":
			embSpec = embedderSpec{Kind: "fake", Model: fakeModel, Dim: fakeDim}
		default:
			embSpec = embedderSpec{
				Kind: "localai", Model: o.embeddingModel, RuntimeModel: runtimeName, Endpoint: o.endpoint,
				APIKeyRef: o.apiKeyRef, TimeoutMS: o.timeout.Milliseconds(), Dim: env.pinned.Dimensions(),
			}
		}
		sctx, cancel := context.WithTimeout(ctx, o.scaleTimeout*time.Duration(len(sizes)+2))
		scale, err = RunScale(sctx, ScaleConfig{
			Sizes: sizes, SmallScopes: o.smallScopes, Queries: o.scaleQueries, Pooled: o.scalePooled,
			Mode: o.mode, Seed: o.seed, Query: env.qcfg, InProcess: o.scaleInProcess, TempDir: o.tmp,
			Embedder: embSpec, ReaderCacheKiB: o.scaleReaderCacheKiB, PinScopes: o.scalePinScopes,
			Progress: func(s string) { fmt.Fprintln(stderr, "rageval: "+s) },
		})
		cancel()
		if err != nil {
			return fmt.Errorf("scale sweep: %w", err)
		}
	}
	duration := time.Since(started)
	containerMem := noMemory()
	if memc != nil {
		containerMem = memc.stop(ctx)
	}

	runID := ids.NewGenerator(clock.Real{}).New("run")
	in := resultInput{
		env: env, corpus: corpus, eval: ev, scale: scale, mem: mem,
		runID: runID, started: started, duration: duration, git: git, host: host,
		runtimeName: runtimeName, containerMem: containerMem, quant: quant,
	}
	res, companion, err := buildResults(in)
	if err != nil {
		return err
	}

	// The tool never writes a result its own checks would refuse.
	files, err := encodeResults(res, companion, benchSchema)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return err
	}
	cpath, err := writeNew(o.out, files.stem+".json", files.companion)
	if err != nil {
		return err
	}
	rpath, err := writeNew(o.out, files.stem+RAGExt, files.rag)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, Summary(res))
	fmt.Fprintf(stdout, "result:    %s\nbench:     %s\n", filepath.ToSlash(rpath), filepath.ToSlash(cpath))
	return nil
}

// resultFiles are the two encoded files of a run.
type resultFiles struct {
	stem      string
	rag       []byte
	companion []byte
}

// encodeResults encodes the companion and the full result, links them (the
// companion's name and digest go into the full result) and refuses a pair that
// does not pass its own checks: the bench schema for the companion, and the
// whole of `rageval check` for the full result.
func encodeResults(res *RAGResult, companion *BenchResult, benchSchema *jsonschema.Schema) (resultFiles, error) {
	companionBytes, err := marshalIndented(companion)
	if err != nil {
		return resultFiles{}, err
	}
	if problems := validateCompanion(benchSchema, companionBytes); len(problems) > 0 {
		return resultFiles{}, fmt.Errorf("internal error: the bench-envelope result does not pass the bench schema:\n  %s", strings.Join(problems, "\n  "))
	}
	stem := resultStem(res)
	res.Companion = CompanionRef{File: stem + ".json", SHA256: sha256Hex(companionBytes), Cases: len(companion.Cases)}
	ragBytes, err := marshalIndented(res)
	if err != nil {
		return resultFiles{}, err
	}
	if problems := checkRAGBytes(ragBytes, companionBytes, benchSchema); len(problems) > 0 {
		return resultFiles{}, fmt.Errorf("internal error: the result does not pass its own check:\n  %s", strings.Join(problems, "\n  "))
	}
	return resultFiles{stem: stem, rag: ragBytes, companion: companionBytes}, nil
}

var fileNameUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

// resultStem names a result after its provider, model, mode, profile and run
// id, with nothing a file system could object to.
func resultStem(r *RAGResult) string {
	part := func(s string) string { return fileNameUnsafe.ReplaceAllString(strings.ToLower(s), "_") }
	return fmt.Sprintf("rag_%s_%s_%s_%s_%s", part(r.Provider.Name), part(r.Model.RegistryID), part(r.Config.Mode), part(r.Profile), part(r.RunID))
}

func marshalIndented(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("cannot encode the result: %w", err)
	}
	return append(b, '\n'), nil
}

// writeNew writes data into dir as a new file and returns its path. It writes a
// temporary file and renames it, so a reader never sees a half-written result,
// and it never overwrites an existing file.
func writeNew(dir, name string, data []byte) (string, error) {
	final := filepath.Join(dir, name)
	if _, err := os.Lstat(final); err == nil {
		return "", fmt.Errorf("%s already exists", final)
	}
	tmp, err := os.CreateTemp(dir, ".rageval-*.tmp")
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

// resultInput is what buildResults assembles a result from.
type resultInput struct {
	env         *runEnv
	corpus      *Corpus
	eval        *EvalResult
	scale       *ScaleResult
	mem         RAGMemory
	runID       string
	started     time.Time
	duration    time.Duration
	git         GitInfo
	host        HostInfo
	runtimeName string
	// containerMem is the sampled memory of the LocalAI container (the "none"
	// form when the run sampled none).
	containerMem MemoryInfo
	quant        *Quantization
}

// buildResults assembles the full result and its bench-envelope companion. The
// companion's file name and digest are filled in by the caller, once the
// companion has been encoded.
func buildResults(in resultInput) (*RAGResult, *BenchResult, error) {
	env, c, ev := in.env, in.corpus, in.eval
	limits, _ := limitsFor(env.profile)
	in.containerMem = companionMemory(in.containerMem)
	timestamp := in.started.Format(time.RFC3339)
	promptSHA := env.gen.FrameSHA256()
	if promptSHA == "" {
		promptSHA = sha256Hex(nil)
	}

	companion, err := buildCompanion(CompanionInput{
		RunID: in.runID, Timestamp: timestamp, DurationMS: *round3(durMS(in.duration)),
		Git: in.git, Host: in.host, Profile: env.profile, Provider: env.provider,
		Model: env.model.Model, Backend: env.backend,
		PromptBudgetToks: env.qcfg.MaxToks, MaxOutputToks: env.acfg.MaxOutputToks,
		TimeoutMS: env.acfg.GenerateTimeout.Milliseconds(),
		CorpusID:  c.Manifest.ID, CorpusSHA256: c.SHA256, PromptSHA256: promptSHA,
		Warmup: env.opts.warmup, WarmupFirstMS: ev.WarmupFirstMS, Records: ev.Records, Memory: in.containerMem,
	})
	if err != nil {
		return nil, nil, err
	}

	answerable := 0
	kinds := map[string]int{}
	for _, q := range c.Questions {
		kinds[q.Kind]++
		if q.Outcome == OutcomeAnswer {
			answerable++
		}
	}
	emb := EmbedderInfo{Kind: "none"}
	switch {
	case env.opts.mode == ModeFTS:
	case env.provider == "fake":
		d := fakeDim
		emb = EmbedderInfo{Kind: "fake", Model: strPtr(fakeModel), Dimensions: &d}
	default:
		emb = EmbedderInfo{Kind: "localai", Model: strPtr(env.modelID)}
		if env.pinned != nil {
			if r := env.pinned.Reported(); r != "" {
				emb.ReportedModel = strPtr(r)
			}
			if d := env.pinned.Dimensions(); d > 0 {
				emb.Dimensions = &d
			}
		}
	}
	reranker := RerankerInfo{Kind: "none"}
	if env.opts.mode == ModeRerank {
		reranker = RerankerInfo{Kind: "lexical", Synthetic: true}
	}
	qcfg := env.qcfg
	if env.opts.mode == ModeRerank {
		qcfg.Rerank = true
	}

	res := &RAGResult{
		FormatVersion: RAGFormatVersion, Kind: RAGKind,
		RunID: in.runID, Timestamp: timestamp, DurationMS: *round3(durMS(in.duration)),
		Git: in.git, Host: in.host,
		Profile: string(env.profile), Emulated: isEmulated(env.profile),
		Synthetic:       isSynthetic(env.provider, reranker),
		ContainerLimits: limits,
		Isolation: Isolation{
			CgroupLimitsApplied: false,
			Note:                isolationNote(in.containerMem),
		},
		Provider: ProviderInfo{Name: env.provider}, Model: env.model.Model, Backend: env.backend,
		Components: Components{
			Embedder: emb,
			Generator: GeneratorInfo{
				Kind: "extractive", LabelBlind: true, MinRatio: env.gen.minRatio,
				PromptID: "rag-answer", PromptSHA256: promptSHA,
			},
			Reranker: reranker,
		},
		Corpus: RAGCorpus{
			ID: c.Manifest.ID, SHA256: c.SHA256, Files: c.Files, Bytes: c.Bytes,
			Sources: len(c.Manifest.Sources), Documents: c.Documents(), Tenants: len(c.Manifest.Tenants),
			Questions: len(c.Questions), Answerable: answerable, ToDecline: len(c.Questions) - answerable,
			Languages: c.Languages(), Kinds: kinds,
		},
		Config: RAGConfig{
			Mode: env.opts.mode, RAGCaps: capsName(env.opts.ragCaps), K: env.ks, Query: queryConfigInfo(qcfg), Answer: answerConfigInfo(env.acfg),
			Chunker: chunkerConfigInfo(env.ccfg), Warmup: env.opts.warmup,
			TimeoutMS: env.acfg.GenerateTimeout.Milliseconds(), PercentileMethod: PercentileMethod,
		},
		Metrics:    computeMetrics(ev.Records, env.ks),
		ByKind:     groupBy(ev.Records, env.ks, func(r QuestionRecord) string { return r.Kind }),
		ByLanguage: groupBy(ev.Records, env.ks, func(r QuestionRecord) string { return r.Language }),
		Modes:      RetrievalModes{Served: nonNilCounts(ev.ModeCounts), Degraded: nonNilCounts(ev.DegradedCount)},
		Latency:    deriveLatency(ev.Records, ev.WarmupFirstMS),
		Ingestion: deriveIngestion(Ingestion{
			Covers: ingestionCovers, Documents: ev.Ingest.Docs, Builds: ev.Builds, Parents: ev.Ingest.Parents,
			Chunks: ev.Ingest.Children, BytesIn: ev.Ingest.Bytes,
			ChunkingMS: *round3(durMS(ev.Ingest.Chunking)), IndexingMS: *round3(durMS(ev.Ingest.Indexing)),
			EmbeddingMS: *round3(durMS(ev.Ingest.Embedding)), FinalizingMS: *round3(durMS(ev.Ingest.Finalizing)),
			Embedded: ev.Ingest.Embedded, Reused: ev.Ingest.Reused, EmbedCalls: ev.Ingest.EmbedCalls,
		}),
		Index: deriveIndex(IndexInfo{
			StoreUsedBytes: ev.Store.Used, StoreDiskBytes: ev.Store.Disk, Chunks: ev.Ingest.Children, Vectors: ev.Vectors,
		}),
		Memory:          in.mem,
		ContainerMemory: in.containerMem,
		Quantization:    in.quant,
		Scale:           in.scale,
		Questions:       ev.Records,
	}
	return res, companion, nil
}

// isSynthetic says whether a result is not wholly a model's: the fake embedder
// or the lexical reranker (a word-overlap score) took part.
func isSynthetic(provider string, r RerankerInfo) bool { return provider == "fake" || r.Synthetic }

const isolationNoContainer = "the run is native on the host and applies no cgroup cap: the profile is the one the result stands for (emulated, never certified), and the host above is what actually ran it"

// isolationNote says what limited the run. rageval itself is never capped; when
// a container was sampled, its own cap is in container_memory.limit_mb_observed.
func isolationNote(m MemoryInfo) string {
	if m.Source != MemSourceDocker {
		return isolationNoContainer
	}
	return "rageval runs natively on the host and applies no cgroup cap; the LocalAI container named in container_memory ran under the cap it reports (limit_mb_observed), which is the profile's: the profile is the one the result stands for (emulated, never certified), and the host above is what actually ran it"
}

func nonNilCounts(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

// Summary renders the headline numbers of a result for the terminal.
func Summary(r *RAGResult) string {
	var sb strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&sb, format+"\n", args...) }
	label := ""
	switch {
	case r.Provider.Name == "fake":
		label = "  [synthetic: fake embedder, measures the pipeline and the harness, not a model]"
	case r.Synthetic:
		label = "  [synthetic reranker: the lexical word-overlap reranker is not a model]"
	}
	w("run %s%s", r.RunID, label)
	w("  provider %s  model %s  profile %s (emulated: %v)  mode %s  corpus %s (%d questions)",
		r.Provider.Name, r.Model.RegistryID, r.Profile, r.Emulated, r.Config.Mode, r.Corpus.ID, r.Corpus.Questions)
	m := r.Metrics
	for i, k := range m.Retrieval.K {
		w("  recall@%-2d %s   nDCG@%-2d %s", k, fmtNum6(m.Retrieval.RecallAtK[i]), k, fmtNum6(m.Retrieval.NDCGAtK[i]))
	}
	w("  MRR %s over %d answerable questions", fmtNum6(m.Retrieval.MRR), m.Retrieval.Questions)
	w("  answered %s   correct %s   fact accuracy %s", fmtRatio(m.Answers.Answerable), fmtRatio(m.Answers.AnswerCorrect), fmtRatio(m.Answers.FactAccuracy))
	w("  citation precision %s   citation labels %s", fmtRatio(m.Answers.CitationPrecision), fmtRatio(m.Answers.CitationLabelAccuracy))
	w("  unsupported answers %s   declines correct %s   leaks %d   errors %d", fmtRatio(m.Answers.UnsupportedAnswerRate), fmtRatio(m.Answers.DeclineCorrect), m.Leaks, m.Errors)
	w("  latency p50/p95 %s / %s ms (end to end)   retrieval p50/p95 %s / %s ms",
		fmtNum(r.Latency.EndToEnd.P50), fmtNum(r.Latency.EndToEnd.P95), fmtNum(r.Latency.Retrieval.P50), fmtNum(r.Latency.Retrieval.P95))
	w("  ingestion %s chunks/s (%d chunks, %d documents; parser excluded)   index %d bytes (%s bytes/chunk)",
		fmtNum(r.Ingestion.ChunksPerSecond), r.Ingestion.Chunks, r.Ingestion.Documents, r.Index.StoreUsedBytes, fmtNum(r.Index.BytesPerChunk))
	if r.Memory.Measured {
		w("  memory (process) peak %s MiB", fmtNum(r.Memory.PeakRSSMB))
	}
	if q := r.Quantization; q != nil {
		for _, s := range q.Sets {
			w("  quantisation (int8 vs exact float, recall@%d, bar %.2f) %s: mean %s, min %s, %d of %d questions perfect (%d vectors, max score error %.4f)",
				s.K, q.Bar, s.Name, fmtNum6(s.Mean), fmtNum6(s.Min), s.Perfect, s.Queries, s.Vectors, s.MaxAbsScoreErr)
		}
	}
	if cm := r.ContainerMemory; cm.Sampled {
		w("  container %s (docker stats, cgroup usage) baseline %s / steady %s / peak %s MiB over %d samples",
			*cm.Container, fmtNum(cm.BaselineMB), fmtNum(cm.SteadyRSSMB), fmtNum(cm.PeakRSSMB), cm.Samples)
	}
	if e := r.Latency.Stages["embed"]; r.Components.Embedder.Kind == "localai" {
		w("  query embedding p50/p95 %s / %s ms (the model's share of every query)", fmtNum(e.P50), fmtNum(e.P95))
	}
	if s := r.Scale; s != nil {
		w("  scale sweep (%s, embedder %s, %d scopes, %d chunks; reader cache %d KiB, pin %d; isolated: %v)",
			s.Mode, s.Embedder, s.Shape.Scopes, s.Shape.CorpusChunks, s.ReaderCacheKiB, s.PinScopes, s.Isolated)
		for _, b := range s.Buckets {
			w("    scope %-6s p50/p95 %s / %s ms   peak %s MiB", fmt.Sprint(*b.ScopeChunks), fmtNum(b.LatencyMS.P50), fmtNum(b.LatencyMS.P95), fmtNum(b.PeakRSSMB))
		}
		w("    corpus-wide    p50/p95 %s / %s ms   (the largest scope's p95 is %sx the corpus-wide p95)",
			fmtNum(s.Pooled.LatencyMS.P50), fmtNum(s.Pooled.LatencyMS.P95), fmtNum(s.TailHidden.Ratio))
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

func fmtNum6(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", *p)
}
