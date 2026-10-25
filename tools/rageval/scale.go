package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The scale sweep: retrieval latency by SCOPE SIZE as well as by corpus size.
//
// ADR 0032 measured that a query costs what its own scope costs, not the
// corpus: one 50,000-chunk scope among fifty small ones dominated the p95 while
// the corpus-wide figure looked fine. A corpus-wide average hides that, so this
// sweep builds ONE database whose scopes have different, exact sizes (1,000,
// 20,000 and 50,000 chunks by default, plus a crowd of small ones that makes
// the corpus larger than any scope), then asks the same seeded questions of
// every size and of the corpus as a whole, and reports both: a row per scope
// size and a corpus-wide row, so the figure that hides the tail sits next to
// the ones that show it.
//
// The text is the worst case for BM25 (30 words a chunk from a 3,000-word Zipf
// vocabulary: a few words are in most chunks), the one of the benchmark of
// internal/rag/index and internal/rag/query, so the numbers are comparable with
// docs/efficiency/M2-09.md. Embeddings are the
// fake embedder's, at the dimension of Granite Embedding 97M (384), unless the
// run has a LocalAI embedder (--embedder localai): then every vector and every
// query embedding is the model's, the build's embedding stage and the "embed"
// stage of a query are real, and the block says so (embedder). A sweep with the
// fake measures the index, the vector scan, the fusion and the checks around the
// models, never a model.
//
// Each phase runs in its own child process (this very binary, re-executed) so
// that the peak memory of a phase is that phase's own, read from the operating
// system, and the memory of the build is not in the memory of a query.

const (
	scalePerParent     = 8   // children under one parent section
	scaleDocChunks     = 400 // children per document
	scaleVocab         = 3000
	scaleWordsPerChunk = 30
	scaleIDEvery       = 500 // every n-th chunk carries an exact identifier
	scaleWarmupQueries = 3
	scaleLongTerms     = 12
	scaleShortTerms    = 3
)

// Query kinds of the sweep.
const (
	scaleKindShort = "short" // three words
	scaleKindLong  = "long"  // twelve words, the worst case for the term cap
)

// ScaleConfig configures the sweep.
type ScaleConfig struct {
	// Sizes are the chunk counts of the scopes, one scope each (1000, 20000, 50000).
	Sizes []int
	// SmallScopes are extra scopes of the smallest size: the many-small-tenants
	// shape that makes the corpus larger than any scope.
	SmallScopes int
	// Queries is the number of timed questions per kind per scope size; Pooled
	// the number across the whole corpus.
	Queries int
	Pooled  int
	Mode    string
	Seed    uint64
	Query   query.Config
	// Embedder says which embedder builds the vectors and embeds the queries.
	Embedder embedderSpec
	// ReaderCacheKiB is the page cache of each reader connection of the query
	// phases, in KiB; 0 is the store's default (2000, the Lite configuration).
	ReaderCacheKiB int
	// PinScopes, when positive, asks the questions of a size's bucket of the
	// first PinScopes scopes of that size only, instead of rotating over all of
	// them: with one scope the working set is that scope's pages. 0 rotates. The
	// corpus-wide row always rotates over every scope.
	PinScopes int
	// InProcess runs the phases in this process instead of children (tests).
	InProcess bool
	// TempDir is where the database goes ("" is the system's).
	TempDir string
	// Progress receives a line per phase.
	Progress func(string)
}

func (c ScaleConfig) check() error {
	if len(c.Sizes) == 0 {
		return errors.New("no scope size")
	}
	seen := map[int]bool{}
	for _, s := range c.Sizes {
		if s < 1 || s > 1_000_000 {
			return fmt.Errorf("scope size %d is outside 1..1000000", s)
		}
		if seen[s] {
			return fmt.Errorf("scope size %d is listed twice", s)
		}
		seen[s] = true
	}
	switch {
	case c.SmallScopes < 0 || c.SmallScopes > 1000:
		return errors.New("small scopes must be 0..1000")
	case c.Queries < 1 || c.Pooled < 1:
		return errors.New("the number of queries must be at least 1")
	case c.Mode != ModeFTS && c.Mode != ModeHybrid && c.Mode != ModeRerank:
		return fmt.Errorf("unknown mode %q", c.Mode)
	case c.PinScopes < 0 || c.PinScopes > 1000:
		return errors.New("pinned scopes must be 0..1000")
	case c.ReaderCacheKiB < 0 || c.ReaderCacheKiB > 1<<18:
		return errors.New("the reader cache must be 0..262144 KiB")
	}
	return c.Embedder.check(c.Mode)
}

// embedderSpec says which embedder a phase of the sweep uses. A phase runs in
// its own process, so the spec carries what that process needs to build it (a
// key is only ever a reference, env:NAME or file:PATH).
type embedderSpec struct {
	// Kind is fake, localai, or none (full-text only).
	Kind string `json:"kind"`
	// Model is the id the vector index records for the embedder.
	Model string `json:"model"`
	// RuntimeModel, Endpoint, APIKeyRef and TimeoutMS are for localai.
	RuntimeModel string `json:"runtime_model"`
	Endpoint     string `json:"endpoint"`
	APIKeyRef    string `json:"api_key_ref"`
	TimeoutMS    int64  `json:"timeout_ms"`
	// Dim is the dimension of the vectors: 384 for the fake, what the runtime
	// answered for localai.
	Dim int `json:"dim"`
}

func (e embedderSpec) check(mode string) error {
	switch {
	case mode == ModeFTS && e.Kind != "none":
		return fmt.Errorf("a full-text sweep has no embedder, not %q", e.Kind)
	case mode != ModeFTS && e.Kind != "fake" && e.Kind != "localai":
		return fmt.Errorf("a %s sweep needs the fake or the localai embedder, not %q", mode, e.Kind)
	case e.Kind != "none" && (e.Model == "" || e.Dim < 1):
		return errors.New("the embedder needs a model id and a dimension")
	}
	return nil
}

// scaleScope is one scope of the synthetic corpus.
type scaleScope struct {
	No     int `json:"no"`
	Chunks int `json:"chunks"`
}

// layout lists the scopes: one per size in the order given, then the small ones.
func (c ScaleConfig) layout() []scaleScope {
	var out []scaleScope
	for _, s := range c.Sizes {
		out = append(out, scaleScope{No: len(out), Chunks: s})
	}
	smallest := c.Sizes[0]
	for _, s := range c.Sizes {
		smallest = min(smallest, s)
	}
	for i := 0; i < c.SmallScopes; i++ {
		out = append(out, scaleScope{No: len(out), Chunks: smallest})
	}
	return out
}

func scaleScopeOf(no int) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_rageval",
		ApplicationID:    "app_rageval_scale",
		TenantID:         fmt.Sprintf("tn_s%04d", no),
		WorkspaceID:      "ws_main",
		PrincipalID:      "usr_scale",
		Roles:            []string{"member"},
		CapabilityScopes: []string{"rag.search"},
		SessionID:        "ses_rageval",
	}
}

// ---------------------------------------------------------------- results

// ScaleResult is the sweep's result.
type ScaleResult struct {
	Mode string `json:"mode"`
	// Isolated is true when every phase ran in its own process, so the memory
	// figures are each phase's own.
	Isolated bool `json:"isolated"`
	// Embedder is who made the vectors and embedded the queries: fake, localai,
	// or none for a full-text sweep.
	Embedder string `json:"embedder"`
	// ReaderCacheKiB is the page cache of each reader connection in the query
	// phases (2000 is the Lite configuration); PinScopes how many scopes of a
	// size its bucket asked (0: all of them, in rotation).
	ReaderCacheKiB int          `json:"reader_cache_kib"`
	PinScopes      int          `json:"pin_scopes"`
	Seed           uint64       `json:"seed"`
	Shape          ScaleShape   `json:"shape"`
	Build          ScaleBuild   `json:"build"`
	Buckets        []ScaleQuery `json:"buckets"`
	Pooled         ScaleQuery   `json:"pooled"`
	// TailHidden compares the corpus-wide p95 with the p95 of the largest scope.
	TailHidden ScaleTail `json:"tail_hidden"`
}

// ScaleShape describes the synthetic corpus.
type ScaleShape struct {
	ScopeSizes      []int  `json:"scope_sizes"`
	SmallScopes     int    `json:"small_scopes"`
	Scopes          int    `json:"scopes"`
	CorpusChunks    int    `json:"corpus_chunks"`
	ChunksPerScope  []int  `json:"chunks_per_scope"`
	ChunksPerParent int    `json:"chunks_per_parent"`
	WordsPerChunk   int    `json:"words_per_chunk"`
	Vocabulary      int    `json:"vocabulary"`
	Text            string `json:"text"`
	EmbeddingDim    *int   `json:"embedding_dim"`
}

// ScaleBuild is the build phase: ingestion throughput and index size at scale.
type ScaleBuild struct {
	Chunks          int      `json:"chunks"`
	Vectors         int      `json:"vectors"`
	ElapsedMS       float64  `json:"elapsed_ms"`
	IndexingMS      float64  `json:"indexing_ms"`
	EmbeddingMS     float64  `json:"embedding_ms"`
	FinalizingMS    float64  `json:"finalizing_ms"`
	ChunksPerSecond *float64 `json:"chunks_per_second"`
	StoreUsedBytes  int64    `json:"store_used_bytes"`
	StoreDiskBytes  int64    `json:"store_disk_bytes"`
	BytesPerChunk   *float64 `json:"bytes_per_chunk"`
	RSSMeasured     bool     `json:"rss_measured"`
	PeakRSSMB       *float64 `json:"peak_rss_mb"`
}

// ScaleQuery is one row of the report: the queries asked of the scopes of one
// size (or of the whole corpus) and what they cost.
type ScaleQuery struct {
	Name string `json:"name"`
	// ScopeChunks is the size of the scopes asked (null for the corpus-wide row);
	// Scopes how many scopes the queries were spread over; CorpusChunks the
	// chunks of the whole database, which every row shares.
	ScopeChunks  *int `json:"scope_chunks"`
	Scopes       int  `json:"scopes"`
	CorpusChunks int  `json:"corpus_chunks"`
	// Samples are the timed latencies in ms per kind of question, in the order
	// asked; LatencyMS and ByKind are derived from them.
	SamplesMS map[string][]float64 `json:"samples_ms"`
	LatencyMS Stats                `json:"latency_ms"`
	ByKind    map[string]Stats     `json:"by_kind"`
	// FirstQueryMS is the first query of the process, cold (page cache and
	// vector cache empty); it is not in the samples.
	FirstQueryMS float64 `json:"first_query_ms"`
	// StageMeanMS is the mean of each stage of the pipeline over the samples.
	StageMeanMS map[string]float64 `json:"stage_mean_ms"`
	AvgItems    float64            `json:"avg_items"`
	Errors      int                `json:"errors"`
	Timeouts    int                `json:"timeouts"`
	// Degraded counts queries by degradation reason (none expected).
	Degraded    map[string]int `json:"degraded"`
	RSSMeasured bool           `json:"rss_measured"`
	RSSStartMB  *float64       `json:"rss_start_mb"`
	RSSAfterMB  *float64       `json:"rss_after_mb"`
	PeakRSSMB   *float64       `json:"peak_rss_mb"`
}

// ScaleTail says how much a corpus-wide figure hides: the p95 of the
// corpus-wide row, the p95 of the row of the largest scope, and their ratio.
type ScaleTail struct {
	PooledP95MS        *float64 `json:"pooled_p95_ms"`
	LargestScopeChunks int      `json:"largest_scope_chunks"`
	LargestScopeP95MS  *float64 `json:"largest_scope_p95_ms"`
	Ratio              *float64 `json:"ratio"`
}

// ---------------------------------------------------------------- child specs

// scaleSpec is what a child needs; it is passed as JSON.
type scaleSpec struct {
	DB     string       `json:"db"`
	Layout []scaleScope `json:"layout"`
	Mode   string       `json:"mode"`
	Seed   uint64       `json:"seed"`
	Query  query.Config `json:"query"`
	// Embedder and ReaderCacheKiB are the run's (see ScaleConfig).
	Embedder       embedderSpec `json:"embedder"`
	ReaderCacheKiB int          `json:"reader_cache_kib"`
	// Query phase only.
	Name    string   `json:"name"`
	Scopes  []int    `json:"scopes"`
	Kinds   []string `json:"kinds"`
	Queries int      `json:"queries"`
	// ScopeChunks is the size the bucket stands for; 0 means the corpus-wide row.
	ScopeChunks int `json:"scope_chunks"`
}

const childPrefix = "RAGEVAL-SCENARIO "

// childFunc runs one phase and returns its JSON result.
type childFunc func(ctx context.Context, role string, spec scaleSpec) (json.RawMessage, error)

// execChild re-executes this binary as `rageval scenario <role>` and sends the
// spec on its standard input (a command line is too short for the layout of a
// large corpus). The command is fixed: the tool's own path and one subcommand.
func execChild(ctx context.Context, role string, spec scaleSpec) (json.RawMessage, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, exe, "scenario", role)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(raw), &stdout, &stderr
	if err := cmd.Run(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return nil, fmt.Errorf("scenario %s: %w: %s", role, err, tail)
	}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, childPrefix) {
			return json.RawMessage(strings.TrimPrefix(line, childPrefix)), nil
		}
	}
	return nil, fmt.Errorf("scenario %s printed no result", role)
}

// inProcessChild runs the phase in this process.
func inProcessChild(ctx context.Context, role string, spec scaleSpec) (json.RawMessage, error) {
	v, err := runScenario(ctx, role, spec)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// maxSpecBytes bounds the spec a phase reads from its standard input.
const maxSpecBytes = 16 << 20

// scenarioMain is the entry of `rageval scenario`: it reads one phase's spec
// (JSON) from stdin, runs the phase and prints its result on one line.
func scenarioMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: rageval scenario build|query < spec.json")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxSpecBytes+1))
	if err != nil || len(raw) > maxSpecBytes {
		fmt.Fprintf(stderr, "rageval scenario: cannot read the spec: %v\n", orElse(err, errors.New("too large")))
		return 2
	}
	var spec scaleSpec
	if err := decodeStrict(raw, &spec); err != nil {
		fmt.Fprintf(stderr, "rageval scenario: bad spec: %v\n", err)
		return 2
	}
	v, err := runScenario(context.Background(), args[0], spec)
	if err != nil {
		fmt.Fprintf(stderr, "rageval scenario %s: %v\n", args[0], err)
		return 1
	}
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(stderr, "rageval scenario: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s%s\n", childPrefix, b)
	return 0
}

func runScenario(ctx context.Context, role string, spec scaleSpec) (any, error) {
	switch role {
	case "build":
		return scaleBuild(ctx, spec)
	case "query":
		return scaleQueries(ctx, spec)
	}
	return nil, fmt.Errorf("unknown role %q", role)
}

// ---------------------------------------------------------------- orchestration

// RunScale runs the sweep: one build, one query phase per scope size, one
// corpus-wide query phase.
func RunScale(ctx context.Context, cfg ScaleConfig) (*ScaleResult, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	run := childFunc(execChild)
	if cfg.InProcess {
		run = inProcessChild
	}
	note := cfg.Progress
	if note == nil {
		note = func(string) {}
	}
	dbPath, cleanup, err := newTempDB(cfg.TempDir)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	layout := cfg.layout()
	base := scaleSpec{DB: dbPath, Layout: layout, Mode: cfg.Mode, Seed: cfg.Seed, Query: cfg.Query,
		Embedder: cfg.Embedder, ReaderCacheKiB: cfg.ReaderCacheKiB}
	corpusChunks := 0
	perScope := make([]int, len(layout))
	for i, s := range layout {
		corpusChunks += s.Chunks
		perScope[i] = s.Chunks
	}
	res := &ScaleResult{
		Mode: cfg.Mode, Isolated: !cfg.InProcess, Seed: cfg.Seed, Embedder: cfg.Embedder.Kind,
		ReaderCacheKiB: effectiveReaderCacheKiB(cfg.ReaderCacheKiB), PinScopes: cfg.PinScopes,
		Shape: ScaleShape{
			ScopeSizes: append([]int{}, cfg.Sizes...), SmallScopes: cfg.SmallScopes, Scopes: len(layout),
			CorpusChunks: corpusChunks, ChunksPerScope: perScope, ChunksPerParent: scalePerParent,
			WordsPerChunk: scaleWordsPerChunk, Vocabulary: scaleVocab,
			Text: "seeded Zipf pseudo-words, 30 a chunk from a 3000-word vocabulary (the worst case for BM25), eight chunks to a parent section",
		},
	}

	if cfg.Embedder.Kind != "none" {
		d := cfg.Embedder.Dim
		res.Shape.EmbeddingDim = &d
	}
	note(fmt.Sprintf("scale: building %d scopes, %d chunks", len(layout), corpusChunks))
	raw, err := run(ctx, "build", base)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &res.Build); err != nil {
		return nil, fmt.Errorf("reading the build result: %w", err)
	}
	if res.Build.Chunks != corpusChunks {
		return nil, fmt.Errorf("the build holds %d chunks, the layout asks for %d", res.Build.Chunks, corpusChunks)
	}

	sizes := append([]int{}, cfg.Sizes...)
	sort.Ints(sizes)
	for _, size := range sizes {
		spec := base
		spec.Name, spec.ScopeChunks = fmt.Sprintf("scope-%d", size), size
		spec.Kinds, spec.Queries = []string{scaleKindShort, scaleKindLong}, cfg.Queries
		for _, s := range layout {
			if s.Chunks == size {
				spec.Scopes = append(spec.Scopes, s.No)
			}
		}
		if cfg.PinScopes > 0 && len(spec.Scopes) > cfg.PinScopes {
			spec.Scopes = spec.Scopes[:cfg.PinScopes]
		}
		note(fmt.Sprintf("scale: querying scopes of %d chunks (%d scopes, %d queries a kind)", size, len(spec.Scopes), cfg.Queries))
		var q ScaleQuery
		if raw, err = run(ctx, "query", spec); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("reading the result for %d: %w", size, err)
		}
		q.CorpusChunks = corpusChunks
		res.Buckets = append(res.Buckets, q)
	}

	spec := base
	spec.Name, spec.ScopeChunks = "corpus-wide", 0
	spec.Kinds, spec.Queries = []string{scaleKindShort, scaleKindLong}, (cfg.Pooled+1)/2
	for _, s := range layout {
		spec.Scopes = append(spec.Scopes, s.No)
	}
	note(fmt.Sprintf("scale: querying the whole corpus (%d queries)", cfg.Pooled))
	if raw, err = run(ctx, "query", spec); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &res.Pooled); err != nil {
		return nil, fmt.Errorf("reading the corpus-wide result: %w", err)
	}
	res.Pooled.CorpusChunks = corpusChunks
	res.TailHidden = tailHidden(res)
	return res, nil
}

// tailHidden compares the corpus-wide p95 with the largest scope's.
func tailHidden(r *ScaleResult) ScaleTail {
	t := ScaleTail{PooledP95MS: r.Pooled.LatencyMS.P95}
	for _, b := range r.Buckets {
		if b.ScopeChunks != nil && *b.ScopeChunks >= t.LargestScopeChunks {
			t.LargestScopeChunks, t.LargestScopeP95MS = *b.ScopeChunks, b.LatencyMS.P95
		}
	}
	if t.PooledP95MS != nil && t.LargestScopeP95MS != nil && *t.PooledP95MS > 0 {
		t.Ratio = round3(*t.LargestScopeP95MS / *t.PooledP95MS)
	}
	return t
}

// ---------------------------------------------------------------- the synthetic corpus

func scaleVocabulary() []string {
	vocab := make([]string, scaleVocab)
	for i := range vocab {
		vocab[i] = pseudoWord(i)
	}
	return vocab
}

// pseudoWord is a pronounceable, unique, three-syllable word for i.
func pseudoWord(i int) string {
	const cons, vow = "bdfgklmnprstvz", "aeiou"
	var b strings.Builder
	for k := 0; k < 3; k++ {
		s := i % 70
		i /= 70
		b.WriteByte(cons[s%14])
		b.WriteByte(vow[s/14])
	}
	return b.String()
}

func newZipf(seed1, seed2 uint64) *rand.Zipf {
	rng := rand.New(rand.NewPCG(seed1, seed2))
	return rand.NewZipf(rng, 1.2, 4, uint64(scaleVocab-1))
}

// scaleDocument builds one document of exactly n children, grouped under parent
// sections of scalePerParent, with the words drawn from zipf. Every
// scaleIDEvery-th child of the scope carries an exact identifier.
func scaleDocument(zipf *rand.Zipf, vocab []string, scopeNo int, doc string, first, n int) chunk.Result {
	var res chunk.Result
	words := func(k int) string {
		var b strings.Builder
		for i := 0; i < k; i++ {
			b.WriteString(vocab[zipf.Uint64()])
			b.WriteByte(' ')
		}
		return strings.TrimSpace(b.String())
	}
	off := 0
	for p := 0; p*scalePerParent < n; p++ {
		pid := fmt.Sprintf("chp_s%d_%s_%04d", scopeNo, doc, p)
		section := "Section " + vocab[zipf.Uint64()]
		start := off
		var texts, kids []string
		for j := 0; j < scalePerParent && p*scalePerParent+j < n; j++ {
			i := p*scalePerParent + j
			text := words(scaleWordsPerChunk)
			if (first+i)%scaleIDEvery == 0 {
				text += fmt.Sprintf(" PN-%d-%d", scopeNo, first+i)
			}
			runes := len([]rune(text))
			sum := sha256.Sum256([]byte(text))
			id := fmt.Sprintf("chk_s%d_%s_%05d", scopeNo, doc, i)
			res.Children = append(res.Children, chunk.Chunk{
				ID: id, ParentID: pid, Kind: chunk.KindParagraph, Ordinal: j,
				Text: text, Section: section, Path: []string{section}, Span: chunk.Span{Start: off, End: off + runes},
				ByteSpan:    chunk.Span{Start: off, End: off + len(text)},
				ContentHash: hex.EncodeToString(sum[:]), EstToks: chunk.EstimateTokens(text),
			})
			texts = append(texts, text)
			kids = append(kids, id)
			off += runes + 2
		}
		ptext := strings.Join(texts, "\n\n")
		res.Parents = append(res.Parents, chunk.Chunk{
			ID: pid, Kind: chunk.KindSection, Ordinal: p, Text: ptext, Section: section, Path: []string{section},
			Span: chunk.Span{Start: start, End: off - 2}, EstToks: chunk.EstimateTokens(ptext), ChildIDs: kids,
		})
	}
	return res
}

// ---------------------------------------------------------------- build phase

// effectiveReaderCacheKiB is the page cache a reader connection gets: the
// flag's value, or the store's default when it is 0.
func effectiveReaderCacheKiB(kib int) int {
	if kib == 0 {
		return store.DefaultReaderCacheKiB
	}
	return kib
}

// scaleStack opens the database of the sweep with the embedder the spec names.
func scaleStack(ctx context.Context, spec scaleSpec) (*Stack, providers.Embedder, error) {
	so := StackOptions{Path: spec.DB, VectorCeilingScale: true, ReaderCacheKiB: spec.ReaderCacheKiB}
	var emb providers.Embedder
	if spec.Mode != ModeFTS {
		var err error
		if emb, err = newSpecEmbedder(ctx, spec.Embedder); err != nil {
			return nil, nil, err
		}
		so.Embedder, so.Model = emb, spec.Embedder.Model
	}
	s, err := OpenStack(ctx, so)
	return s, emb, err
}

func scaleBuild(ctx context.Context, spec scaleSpec) (ScaleBuild, error) {
	var out ScaleBuild
	stack, _, err := scaleStack(ctx, spec)
	if err != nil {
		return out, err
	}
	defer stack.Close()
	vocab := scaleVocabulary()

	started := time.Now()
	var total IngestResult
	for _, sc := range spec.Layout {
		scope := scaleScopeOf(sc.No)
		zipf := newZipf(spec.Seed+1, uint64(sc.No)+1)
		var docs []index.Document
		for d, left := 0, sc.Chunks; left > 0; d++ {
			n := min(left, scaleDocChunks)
			doc := fmt.Sprintf("d%04d", d)
			docs = append(docs, index.Document{
				ID: doc, Version: "v1",
				Chunks: scaleDocument(zipf, vocab, sc.No, doc, d*scaleDocChunks, n),
			})
			left -= n
		}
		r, err := stack.IngestChunked(ctx, scope, newSourceID(), docs)
		if err != nil {
			return out, fmt.Errorf("scope %d: %w", sc.No, err)
		}
		if r.Children != sc.Chunks {
			return out, fmt.Errorf("scope %d holds %d chunks, the layout asks for exactly %d", sc.No, r.Children, sc.Chunks)
		}
		total.Add(r)
	}
	elapsed := time.Since(started)

	out.Chunks = total.Children
	out.ElapsedMS = *round3(durMS(elapsed))
	out.IndexingMS = *round3(durMS(total.Indexing))
	out.EmbeddingMS = *round3(durMS(total.Embedding))
	out.FinalizingMS = *round3(durMS(total.Finalizing))
	out.Vectors = total.Embedded + total.Reused
	sz, err := stack.StoreSize(ctx)
	if err != nil {
		return out, err
	}
	out.StoreUsedBytes, out.StoreDiskBytes = sz.Used, sz.Disk
	if _, peak, ok := processMemory(); ok {
		out.RSSMeasured, out.PeakRSSMB = true, round3(mib(peak))
	}
	return deriveScaleBuild(out), nil
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

// ---------------------------------------------------------------- query phase

// scaleQuestions are the seeded questions of one kind.
func scaleQuestions(kind string, n int, seed uint64) []string {
	vocab := scaleVocabulary()
	zipf := newZipf(seed+7, uint64(len(kind)))
	terms := scaleShortTerms
	if kind == scaleKindLong {
		terms = scaleLongTerms
	}
	out := make([]string, n)
	for i := range out {
		seen := map[string]bool{}
		var parts []string
		for len(parts) < terms {
			w := vocab[zipf.Uint64()]
			if !seen[w] {
				seen[w] = true
				parts = append(parts, w)
			}
		}
		out[i] = strings.Join(parts, " ")
	}
	return out
}

func scaleQueries(ctx context.Context, spec scaleSpec) (ScaleQuery, error) {
	out := ScaleQuery{
		Name: spec.Name, Scopes: len(spec.Scopes),
		SamplesMS: map[string][]float64{}, ByKind: map[string]Stats{},
		StageMeanMS: map[string]float64{}, Degraded: map[string]int{},
	}
	if spec.ScopeChunks > 0 {
		sc := spec.ScopeChunks
		out.ScopeChunks = &sc
	}
	if len(spec.Scopes) == 0 || len(spec.Kinds) == 0 || spec.Queries < 1 {
		return out, errors.New("a query phase needs scopes, kinds and a query count")
	}
	rssStart, _, rssOK := processMemory()

	stack, emb, err := scaleStack(ctx, spec)
	if err != nil {
		return out, err
	}
	defer stack.Close()
	reg := providers.NewRegistry(clock.Real{})
	deps := query.Deps{Lexical: stack.Ix, Catalog: openCatalog{}}
	qcfg := spec.Query
	if spec.Mode != ModeFTS {
		if err := reg.Register(providers.RoleEmbedder, emb); err != nil {
			return out, err
		}
		deps.Providers, deps.EmbeddingModel, deps.Vectors = reg, spec.Embedder.Model, stack.Flat
	}
	if spec.Mode == ModeRerank {
		if err := reg.Register(providers.RoleReranker, lexicalReranker{}); err != nil {
			return out, err
		}
		qcfg.Rerank = true
	}
	pipe, err := query.New(deps, qcfg)
	if err != nil {
		return out, err
	}

	ask := func(scopeNo int, text string) (query.Result, time.Duration, error) {
		w := startWatch()
		r, err := pipe.Query(ctx, scaleScopeOf(scopeNo), query.Request{Text: text})
		return r, w.elapsed(), err
	}

	// Warm-up: the first query of the process is cold and reported on its own;
	// the next ones bring the page cache and the vector cache to the state of a
	// running service. None of them is in the samples.
	warm := scaleQuestions(scaleKindShort, scaleWarmupQueries, spec.Seed+99)
	for i, text := range warm {
		_, d, err := ask(spec.Scopes[i%len(spec.Scopes)], text)
		if err != nil && contracts.CodeOf(err) != contracts.ErrTimeout {
			return out, fmt.Errorf("warm-up query: %w", err)
		}
		if i == 0 {
			out.FirstQueryMS = *round3(durMS(d))
		}
	}

	var items, answered int
	stages := map[string]time.Duration{}
	for _, kind := range spec.Kinds {
		qs := scaleQuestions(kind, spec.Queries, spec.Seed)
		for i, text := range qs {
			scopeNo := spec.Scopes[i%len(spec.Scopes)]
			r, d, err := ask(scopeNo, text)
			if err != nil {
				if ctx.Err() != nil {
					return out, ctx.Err()
				}
				out.Errors++
				if contracts.CodeOf(err) == contracts.ErrTimeout {
					out.Timeouts++
				}
				continue
			}
			out.SamplesMS[kind] = append(out.SamplesMS[kind], *round3(durMS(d)))
			items += len(r.Package.Items)
			answered++
			for _, g := range r.Diagnostics.Degraded {
				out.Degraded[g]++
			}
			for name, v := range map[string]time.Duration{
				"fts": r.Diagnostics.FTS, "embed": r.Diagnostics.Embed, "vector": r.Diagnostics.Vector,
				"hydrate": r.Diagnostics.Hydrate, "expand": r.Diagnostics.Expand, "rerank": r.Diagnostics.Rerank,
				"recheck": r.Diagnostics.Recheck,
			} {
				stages[name] += v
			}
		}
	}
	finishScaleQuery(&out, stages, items, answered)
	if rss, peak, ok := processMemory(); ok && rssOK {
		out.RSSMeasured = true
		out.RSSStartMB, out.RSSAfterMB, out.PeakRSSMB = round3(mib(rssStart)), round3(mib(rss)), round3(mib(peak))
	}
	return out, nil
}

// finishScaleQuery derives the statistics of a row from its samples.
func finishScaleQuery(q *ScaleQuery, stages map[string]time.Duration, items, answered int) {
	var all []float64
	for _, kind := range sortedKeys(q.SamplesMS) {
		s := q.SamplesMS[kind]
		q.ByKind[kind] = Summarize(s)
		all = append(all, s...)
	}
	q.LatencyMS = Summarize(all)
	if answered > 0 {
		q.AvgItems = *round3(float64(items) / float64(answered))
		for name, d := range stages {
			q.StageMeanMS[name] = *round3(durMS(d) / float64(answered))
		}
	}
}
