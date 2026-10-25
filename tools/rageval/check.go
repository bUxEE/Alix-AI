package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/rag/query"
)

// maxResultBytes bounds a result file that check will read.
const maxResultBytes = 64 << 20

func checkCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rageval check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rejectSynthetic := fs.Bool("reject-synthetic", false, "fail on results from the fake embedder (use for evidence that must come from a real model)")
	schemas := fs.String("schemas", "", "schemas directory (default: schemas/ beside go.mod)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: rageval check [-reject-synthetic] [-schemas dir] <dir or file>...")
		fmt.Fprintln(stderr, "  validates every *"+RAGExt+" result against rag-result.schema.json, re-derives its metrics")
		fmt.Fprintln(stderr, "  from the raw per-question records and checks it against its bench-envelope companion;")
		fmt.Fprintln(stderr, "  it fails when it finds no result")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	dir, err := FindSchemasDir(*schemas)
	if err != nil {
		fmt.Fprintf(stderr, "rageval check: %v\n", err)
		return 1
	}
	benchSchema, err := LoadBenchSchema(dir)
	if err != nil {
		fmt.Fprintf(stderr, "rageval check: %v\n", err)
		return 1
	}
	files, err := collectRAGFiles(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "rageval check: %v\n", err)
		return 1
	}
	failed := 0
	for _, f := range files {
		problems := checkRAGFile(benchSchema, f, *rejectSynthetic)
		if len(problems) == 0 {
			fmt.Fprintf(stdout, "PASS  %s\n", filepath.ToSlash(f))
			continue
		}
		failed++
		fmt.Fprintf(stdout, "FAIL  %s\n", filepath.ToSlash(f))
		for _, p := range problems {
			fmt.Fprintf(stdout, "      %s\n", p)
		}
	}
	if failed > 0 {
		fmt.Fprintf(stdout, "rageval check: FAIL - %d of %d result file(s) are invalid\n", failed, len(files))
		return 1
	}
	fmt.Fprintf(stdout, "rageval check: ok - %d result file(s)\n", len(files))
	return 0
}

// collectRAGFiles lists the results named by args: a file is taken as it is, a
// directory contributes every *.rag under it (hidden files and directories
// skipped). Finding none is an error, so an empty or mistyped directory cannot
// pass.
func collectRAGFiles(args []string) ([]string, error) {
	var files []string
	for _, a := range args {
		st, err := os.Stat(a)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			files = append(files, a)
			continue
		}
		err = filepath.WalkDir(a, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			name := d.Name()
			if d.IsDir() {
				if p != a && strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(name, RAGExt) && !strings.HasPrefix(name, ".") {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no result files (*%s) found under %s", RAGExt, strings.Join(args, ", "))
	}
	sort.Strings(files)
	return files, nil
}

func readCappedFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxResultBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResultBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxResultBytes)
	}
	return data, nil
}

// checkRAGFile validates one .rag file and the companion beside it.
func checkRAGFile(benchSchema *jsonschema.Schema, path string, rejectSynthetic bool) []string {
	data, err := readCappedFile(path)
	if err != nil {
		return []string{err.Error()}
	}
	var head struct {
		Companion struct {
			File string `json:"file"`
		} `json:"companion"`
	}
	_ = json.Unmarshal(data, &head)
	var companion []byte
	if head.Companion.File != "" && filepath.Base(head.Companion.File) == head.Companion.File {
		if companion, err = readCappedFile(filepath.Join(filepath.Dir(path), head.Companion.File)); err != nil {
			return []string{fmt.Sprintf("the companion %s cannot be read: %v", head.Companion.File, err)}
		}
	}
	problems := checkRAGBytes(data, companion, benchSchema)
	if rejectSynthetic {
		var r struct {
			Synthetic bool `json:"synthetic"`
		}
		if json.Unmarshal(data, &r) == nil && r.Synthetic {
			problems = append(problems, "synthetic result (fake embedder): it measures the pipeline and the harness, not a model")
		}
	}
	return problems
}

// checkRAGBytes is the full check of one result: the schema, then every rule
// that needs arithmetic, then the companion.
func checkRAGBytes(rag, companion []byte, benchSchema *jsonschema.Schema) []string {
	schema, err := LoadRAGSchema()
	if err != nil {
		return []string{err.Error()}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(rag))
	if err != nil {
		return []string{fmt.Sprintf("not valid JSON: %v", err)}
	}
	if err := schema.Validate(inst); err != nil {
		return []string{"schema: " + firstProblemLines(err, 5)}
	}
	var res RAGResult
	dec := json.NewDecoder(bytes.NewReader(rag))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return []string{fmt.Sprintf("does not decode: %v", err)}
	}
	problems := verifyRAG(&res)
	return append(problems, verifyCompanion(&res, companion, benchSchema)...)
}

// validateCompanion validates a bench-envelope result against the bench schema.
func validateCompanion(schema *jsonschema.Schema, data []byte) []string {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return []string{fmt.Sprintf("not valid JSON: %v", err)}
	}
	if err := schema.Validate(inst); err != nil {
		return []string{"schema: " + firstProblemLines(err, 5)}
	}
	return nil
}

// verifyCompanion checks the bench-envelope result that goes with res: the
// schema (the part of bench check that is not arithmetic; the arithmetic is
// `bench check`'s, which the repository runs on the same files), the digest and
// run id it was written with, and that it is exactly the projection of res's
// questions.
func verifyCompanion(res *RAGResult, companion []byte, benchSchema *jsonschema.Schema) []string {
	if companion == nil {
		return []string{fmt.Sprintf("the companion %s is missing", res.Companion.File)}
	}
	if got := sha256Hex(companion); got != res.Companion.SHA256 {
		return []string{fmt.Sprintf("the companion %s has digest %s, the result says %s", res.Companion.File, got, res.Companion.SHA256)}
	}
	if problems := validateCompanion(benchSchema, companion); len(problems) > 0 {
		return prefixAll("companion: ", problems)
	}
	var got BenchResult
	dec := json.NewDecoder(bytes.NewReader(companion))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		return []string{fmt.Sprintf("companion does not decode: %v", err)}
	}
	want, err := buildCompanion(CompanionInput{
		RunID: res.RunID, Timestamp: res.Timestamp, DurationMS: res.DurationMS, Git: res.Git, Host: res.Host,
		Profile: models.Profile(res.Profile), Provider: res.Provider.Name, Model: res.Model, Backend: res.Backend,
		PromptBudgetToks: res.Config.Query.MaxToks, MaxOutputToks: res.Config.Answer.MaxOutputToks, TimeoutMS: res.Config.TimeoutMS,
		CorpusID: res.Corpus.ID, CorpusSHA256: res.Corpus.SHA256, PromptSHA256: res.Components.Generator.PromptSHA256,
		Warmup: res.Config.Warmup, WarmupFirstMS: res.Latency.WarmupFirstMS, Records: res.Questions, Memory: res.ContainerMemory,
	})
	if err != nil {
		return []string{"companion: " + err.Error()}
	}
	var ps []string
	if len(got.Cases) != res.Companion.Cases {
		ps = append(ps, fmt.Sprintf("companion holds %d cases, the result says %d", len(got.Cases), res.Companion.Cases))
	}
	if !reflect.DeepEqual(&got, want) {
		ps = append(ps, "companion is not the projection of the result's questions (a case, a metric or an envelope field differs)")
	}
	return ps
}

func prefixAll(prefix string, ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = prefix + p
	}
	return out
}

// verifyRAG applies what the JSON Schema cannot express. It checks that the
// profile label is honest, that the counts agree, and that every aggregate in
// the file equals what its raw records say, so a result that was edited by
// hand, or written by a tool with a different idea of a metric, does not pass.
func verifyRAG(r *RAGResult) []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, fmt.Sprintf(format, args...)) }

	p := models.Profile(r.Profile)
	if limits, ok := limitsFor(p); !ok {
		add("profile %q is not a known profile", r.Profile)
	} else {
		if r.Emulated != isEmulated(p) {
			add("emulated is %v but results under profile %s are emulated (never certified)", r.Emulated, r.Profile)
		}
		if r.ContainerLimits != limits {
			add("container_limits %+v are not the limits of profile %s (%+v)", r.ContainerLimits, r.Profile, limits)
		}
	}
	if r.Components.Reranker.Synthetic != (r.Components.Reranker.Kind == "lexical") {
		add("the %s reranker is labelled synthetic: %v (the lexical one is a word-overlap score, no model; none is not synthetic)", r.Components.Reranker.Kind, r.Components.Reranker.Synthetic)
	}
	if want := isSynthetic(r.Provider.Name, r.Components.Reranker); r.Synthetic != want {
		add("synthetic is %v for provider %s and reranker %s (it is true for the fake embedder and for the lexical reranker)", r.Synthetic, r.Provider.Name, r.Components.Reranker.Kind)
	}
	if r.Components.Embedder.Kind != "none" && r.Components.Embedder.Kind != r.Provider.Name {
		add("the embedder is %s but the provider is %s", r.Components.Embedder.Kind, r.Provider.Name)
	}
	if (r.Config.Mode == ModeFTS) != (r.Components.Embedder.Kind == "none") {
		add("mode %s and embedder %s do not go together", r.Config.Mode, r.Components.Embedder.Kind)
	}
	if (r.Config.Mode == ModeRerank) != (r.Components.Reranker.Kind == "lexical") || r.Config.Query.Rerank != (r.Config.Mode == ModeRerank) {
		add("mode %s and the reranker (%s, rerank %v) do not go together", r.Config.Mode, r.Components.Reranker.Kind, r.Config.Query.Rerank)
	}
	{
		want := query.DefaultConfig()
		if err := applyRAGCaps(&want, r.Config.RAGCaps); err != nil {
			add("rag_caps: %v", err)
		} else if got := r.Config.Query; got.MaxItems != want.MaxItems || got.MaxToks != want.MaxToks || got.FTSCandidates != want.FTSCandidates ||
			got.VectorCandidates != want.VectorCandidates || got.ParentExpansion != want.ParentExpansion ||
			(want.ParentExpansion && (got.MaxParentLookups != want.MaxParentLookups || got.MaxParentToks != want.MaxParentToks)) {
			add("the query configuration is not that of the rag caps %q (max_items %d, max_toks %d, %d candidates a leg)", r.Config.RAGCaps, want.MaxItems, want.MaxToks, want.FTSCandidates)
		}
	}
	if !sort.IntsAreSorted(r.Config.K) {
		add("config.k is not ascending")
	}
	for i, k := range r.Config.K {
		if k > r.Config.Query.MaxItems {
			add("k %d is over the %d items of an evidence package", k, r.Config.Query.MaxItems)
		}
		if i > 0 && r.Config.K[i-1] == k {
			add("k %d is listed twice", k)
		}
	}
	if r.Config.Warmup == 0 && r.Latency.WarmupFirstMS != nil {
		add("latency.warmup_first_ms is set but no warm-up ran")
	}

	ps = append(ps, verifyQuestions(r)...)
	if len(ps) > 0 && len(r.Questions) == 0 {
		return ps
	}

	if want := computeMetrics(r.Questions, r.Config.K); !reflect.DeepEqual(r.Metrics, want) {
		add("metrics do not match the questions (recomputed correct %d of %d, leaks %d, errors %d)", want.Correct.Passed, want.Questions, want.Leaks, want.Errors)
	}
	if want := groupBy(r.Questions, r.Config.K, func(q QuestionRecord) string { return q.Kind }); !reflect.DeepEqual(r.ByKind, want) {
		add("by_kind does not match the questions")
	}
	if want := groupBy(r.Questions, r.Config.K, func(q QuestionRecord) string { return q.Language }); !reflect.DeepEqual(r.ByLanguage, want) {
		add("by_language does not match the questions")
	}
	if want := deriveLatency(r.Questions, r.Latency.WarmupFirstMS); !reflect.DeepEqual(r.Latency, want) {
		add("latency does not match the questions")
	}
	served, degraded := map[string]int{}, map[string]int{}
	for _, q := range r.Questions {
		if q.Status != StatusOK {
			continue
		}
		served[q.Mode]++
		for _, d := range q.Degraded {
			degraded[d]++
		}
	}
	if !reflect.DeepEqual(nonNilCounts(r.Modes.Served), served) || !reflect.DeepEqual(nonNilCounts(r.Modes.Degraded), degraded) {
		add("retrieval_modes do not match the questions")
	}
	if want := deriveIngestion(r.Ingestion); !reflect.DeepEqual(r.Ingestion, want) {
		add("ingestion elapsed time and rates do not match its stages")
	}
	if want := deriveIndex(r.Index); !reflect.DeepEqual(r.Index, want) {
		add("index bytes per chunk does not match its size")
	}
	if r.Index.Chunks != r.Ingestion.Chunks {
		add("index.chunks %d differs from ingestion.chunks %d", r.Index.Chunks, r.Ingestion.Chunks)
	}
	if r.Memory.Measured != (r.Memory.PeakRSSMB != nil && r.Memory.RSSMB != nil) {
		add("memory.measured does not match its figures")
	}
	if r.Memory.RSSMB != nil && r.Memory.PeakRSSMB != nil && *r.Memory.RSSMB > *r.Memory.PeakRSSMB {
		add("memory.rss_mb exceeds peak_rss_mb")
	}
	ps = append(ps, verifyContainerMemory(r.ContainerMemory, r.ContainerLimits)...)
	ps = append(ps, verifyQuantization(r.Quantization, r.Components.Embedder.Kind)...)
	if r.Scale != nil {
		ps = append(ps, verifyScale(r.Scale)...)
		if r.Scale.Embedder != r.Components.Embedder.Kind {
			add("scale: the sweep embedded with %s, the run's embedder is %s", r.Scale.Embedder, r.Components.Embedder.Kind)
		}
	}
	return ps
}

// verifyQuantization re-derives every aggregate of the quantisation check from
// its per-question recalls.
func verifyQuantization(q *Quantization, embedder string) []string {
	if q == nil {
		return nil
	}
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, "quantization: "+fmt.Sprintf(format, args...)) }
	if q.Embedder != embedder || embedder == "none" {
		add("measured on the %s embedder, the run's is %s", q.Embedder, embedder)
	}
	if q.K != quantK || q.Bar != quantBar {
		add("k %d and bar %v are not ADR 0011's (%d, %v)", q.K, q.Bar, quantK, quantBar)
	}
	for _, s := range q.Sets {
		switch {
		case s.K != min(quantK, s.Vectors) || len(s.RecallAtK) != s.Queries || s.Vectors < 1 || s.Dim < 1:
			add("set %s: k %d, %d recalls for %d queries, %d vectors of %d dimensions do not add up", s.Name, s.K, len(s.RecallAtK), s.Queries, s.Vectors, s.Dim)
		case !reflect.DeepEqual(s, quantSummary(s)):
			add("set %s: mean, min, perfect and meets_bar do not match the per-question recalls", s.Name)
		}
		for i, r := range s.RecallAtK {
			if r < 0 || r > 1 {
				add("set %s: recall %v of question %d is outside 0..1", s.Name, r, i)
			}
		}
	}
	return ps
}

// verifyContainerMemory checks the sampled memory of the LocalAI container:
// either the "none" form, or readings whose order makes sense and a limit that
// is the profile's (bench check applies the same rule to the companion: a run
// labelled lite that did not run under the lite cap is not a lite result).
func verifyContainerMemory(m MemoryInfo, limits ContainerLimits) []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, "container_memory: "+fmt.Sprintf(format, args...)) }
	if !m.Sampled {
		if !reflect.DeepEqual(m, noMemory()) {
			add("not sampled, yet it is not the none form")
		}
		return ps
	}
	switch {
	case m.Source != MemSourceDocker || m.Container == nil || m.IntervalMS == nil:
		add("sampled, yet the source, the container or the interval is missing")
	case m.Samples < 1 || m.BaselineMB == nil || m.PeakRSSMB == nil || m.SteadyRSSMB == nil:
		add("sampled, yet it holds no reading")
	case *m.SteadyRSSMB > *m.PeakRSSMB || *m.BaselineMB > *m.PeakRSSMB:
		add("the baseline %.1f or the steady figure %.1f exceeds the peak %.1f", *m.BaselineMB, *m.SteadyRSSMB, *m.PeakRSSMB)
	}
	if m.LimitMBObserved == nil {
		add("no memory limit was observed, so nothing shows the container ran under the %d MiB of the profile", limits.MemMB)
	} else if !limitMatches(*m.LimitMBObserved, limits.MemMB) {
		add("the container's limit is %.0f MiB, the profile's is %d: the run was not under the limit it is labelled with", *m.LimitMBObserved, limits.MemMB)
	}
	return ps
}

// verifyQuestions checks the records one by one and the counts of the corpus
// block against them.
func verifyQuestions(r *RAGResult) []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, fmt.Sprintf(format, args...)) }

	c := r.Corpus
	if c.Questions != len(r.Questions) {
		add("corpus.questions is %d but the result holds %d questions", c.Questions, len(r.Questions))
	}
	answerable, kinds, langs := 0, map[string]int{}, map[string]int{}
	seen := map[string]bool{}
	for _, q := range r.Questions {
		if seen[q.ID] {
			add("question %s appears twice", q.ID)
		}
		seen[q.ID] = true
		kinds[q.Kind]++
		langs[q.Language]++
		if q.Outcome == OutcomeAnswer {
			answerable++
		}
		ps = append(ps, verifyQuestion(q)...)
	}
	if c.Answerable != answerable || c.ToDecline != len(r.Questions)-answerable {
		add("corpus.answerable %d and to_decline %d do not match the questions (%d answerable)", c.Answerable, c.ToDecline, answerable)
	}
	if !reflect.DeepEqual(c.Kinds, kinds) {
		add("corpus.kinds does not match the questions")
	}
	if !reflect.DeepEqual(c.Languages, langs) {
		add("corpus.languages does not match the questions")
	}
	return ps
}

// verifyQuestion checks the internal consistency of one record and re-derives
// its derived flags (leak, correct) from its parts.
func verifyQuestion(q QuestionRecord) []string {
	var ps []string
	add := func(format string, args ...any) {
		ps = append(ps, fmt.Sprintf("question %s: ", q.ID)+fmt.Sprintf(format, args...))
	}

	if q.FactsHit > q.FactsTotal {
		add("facts_hit %d exceeds facts_total %d", q.FactsHit, q.FactsTotal)
	}
	for i, hits := range q.Ranked {
		for _, e := range hits {
			if e >= q.Expected {
				add("ranked item %d hits entry %d of %d", i+1, e, q.Expected)
			}
		}
	}
	if q.Outcome == OutcomeDecline && q.Expected != 0 {
		add("a question to decline has %d evidence entries", q.Expected)
	}
	if wantLeak := q.ForbiddenSourceHits > 0 || q.ForbiddenHits > 0; q.Leak != wantLeak {
		add("leak is %v but forbidden_hits %d and forbidden_source_hits %d say %v", q.Leak, q.ForbiddenHits, q.ForbiddenSourceHits, wantLeak)
	}
	if q.Status != StatusOK {
		if q.Correct {
			add("correct is true for a question that ended in an error")
		}
		return ps
	}
	if q.Declined && (q.Claims != 0 || len(q.Citations) != 0) {
		add("declined, yet it holds %d claims and %d citations", q.Claims, len(q.Citations))
	}
	if !q.Declined && (q.Claims < 1 || len(q.Citations) < 1) {
		add("answered, yet it holds %d claims and %d citations", q.Claims, len(q.Citations))
	}
	var want bool
	switch q.Outcome {
	case OutcomeDecline:
		want = q.Declined && !q.Leak
	default:
		anyCorrect := false
		for _, c := range q.Citations {
			anyCorrect = anyCorrect || c.Correct
		}
		want = !q.Declined && q.FactsHit == q.FactsTotal && !q.Leak && anyCorrect && (q.LabelOK == nil || *q.LabelOK)
	}
	if q.Correct != want {
		add("correct is %v but its parts say %v", q.Correct, want)
	}
	return ps
}

// verifyScale re-derives every statistic of the sweep from its samples.
func verifyScale(s *ScaleResult) []string {
	var ps []string
	add := func(format string, args ...any) { ps = append(ps, "scale: "+fmt.Sprintf(format, args...)) }

	sum := 0
	for _, n := range s.Shape.ChunksPerScope {
		sum += n
	}
	if s.Shape.Scopes != len(s.Shape.ChunksPerScope) || s.Shape.CorpusChunks != sum {
		add("shape: %d scopes and %d corpus chunks do not match the %d sizes listed (%d chunks)", s.Shape.Scopes, s.Shape.CorpusChunks, len(s.Shape.ChunksPerScope), sum)
	}
	if s.Build.Chunks != s.Shape.CorpusChunks {
		add("the build holds %d chunks, the shape says %d", s.Build.Chunks, s.Shape.CorpusChunks)
	}
	if (s.Mode == ModeFTS) != (s.Embedder == "none") || (s.Embedder != "none" && s.Embedder != "fake" && s.Embedder != "localai") {
		add("a %s sweep with the embedder %q", s.Mode, s.Embedder)
	}
	if (s.Embedder == "none") != (s.Shape.EmbeddingDim == nil) {
		add("the embedder %s and the embedding dimension %v do not go together", s.Embedder, s.Shape.EmbeddingDim)
	}
	if s.Mode != ModeFTS && s.Build.Vectors != s.Build.Chunks {
		add("the build holds %d vectors for %d chunks", s.Build.Vectors, s.Build.Chunks)
	}
	if want := deriveScaleBuild(s.Build); !reflect.DeepEqual(s.Build, want) {
		add("build rates do not match its elapsed time and size")
	}
	sizes := map[int]int{}
	for _, n := range s.Shape.ChunksPerScope {
		sizes[n]++
	}
	for _, b := range s.Buckets {
		if b.ScopeChunks == nil {
			add("bucket %s has no scope size", b.Name)
			continue
		}
		want := sizes[*b.ScopeChunks]
		if s.PinScopes > 0 {
			want = min(want, s.PinScopes)
		}
		if want != b.Scopes {
			add("bucket %s samples %d scopes of %d chunks, expected %d (the shape has %d, pinned to %d)", b.Name, b.Scopes, *b.ScopeChunks, want, sizes[*b.ScopeChunks], s.PinScopes)
		}
		ps = append(ps, verifyScaleQuery(b, s.Shape.CorpusChunks)...)
	}
	if s.Pooled.ScopeChunks != nil {
		add("the corpus-wide row has a scope size")
	}
	if s.Pooled.Scopes != s.Shape.Scopes {
		add("the corpus-wide row spans %d scopes, the shape has %d", s.Pooled.Scopes, s.Shape.Scopes)
	}
	ps = append(ps, verifyScaleQuery(s.Pooled, s.Shape.CorpusChunks)...)
	if want := tailHidden(s); !reflect.DeepEqual(s.TailHidden, want) {
		add("tail_hidden does not match the rows")
	}
	return ps
}

func verifyScaleQuery(q ScaleQuery, corpusChunks int) []string {
	var ps []string
	add := func(format string, args ...any) {
		ps = append(ps, fmt.Sprintf("scale: row %s: ", q.Name)+fmt.Sprintf(format, args...))
	}
	if q.CorpusChunks != corpusChunks {
		add("corpus_chunks is %d, the shape says %d", q.CorpusChunks, corpusChunks)
	}
	var all []float64
	byKind := map[string]Stats{}
	for _, kind := range sortedKeys(q.SamplesMS) {
		byKind[kind] = Summarize(q.SamplesMS[kind])
		all = append(all, q.SamplesMS[kind]...)
	}
	if !reflect.DeepEqual(q.ByKind, byKind) {
		add("by_kind does not match the samples")
	}
	if !reflect.DeepEqual(q.LatencyMS, Summarize(all)) {
		add("latency_ms does not match the samples")
	}
	if q.RSSMeasured != (q.PeakRSSMB != nil) {
		add("rss_measured does not match the figures")
	}
	if q.RSSAfterMB != nil && q.PeakRSSMB != nil && *q.RSSAfterMB > *q.PeakRSSMB {
		add("rss_after_mb exceeds peak_rss_mb")
	}
	return ps
}

// deriveScaleBuild fills the build's rates from its counts, elapsed time and
// size, so the writer and the checker share one definition.
func deriveScaleBuild(b ScaleBuild) ScaleBuild {
	b.ChunksPerSecond, b.BytesPerChunk = nil, nil
	if b.ElapsedMS > 0 {
		b.ChunksPerSecond = round3(float64(b.Chunks) / (b.ElapsedMS / 1000))
	}
	if b.Chunks > 0 {
		b.BytesPerChunk = round3(float64(b.StoreUsedBytes) / float64(b.Chunks))
	}
	return b
}
