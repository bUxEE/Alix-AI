package main

import (
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/query"
)

// The full result of a run: the .rag file. It uses the field names and the
// rules of the bench envelope for everything the two share (run id, timestamp,
// git, host, profile, emulated, synthetic, container limits, provider, model,
// backend, corpus, statistics) and adds what a RAG run measures. It is
// described by rag-result.schema.json beside this file; `rageval check`
// validates it against that schema, re-derives every metric from the raw
// per-question records and checks it against its bench-envelope companion.
//
// Every key is always written (no omitempty) and a value that is unknown or
// does not apply is an explicit null, as in the bench envelope. Keys never
// contain the word "token" (they use toks), so that the log redactor, which
// masks any key containing it, can never mask a figure.

const (
	// RAGFormatVersion is result.format_version. 2 added container_memory,
	// latency.stages_ms (and each question's stages_ms), reranker.synthetic and
	// the scale block's embedder, reader cache and pinned scopes, and the
	// quantisation check (quantization).
	RAGFormatVersion = 2
	// RAGKind marks a file as a rageval result.
	RAGKind = "alixai.rageval.result"
	// RAGExt is the file extension of a full result. It is not .json on purpose:
	// `bench check` validates every *.json under a directory against the bench
	// schema, which a RAG result is not.
	RAGExt = ".rag"
)

// RAGResult is one rageval run.
type RAGResult struct {
	FormatVersion   int                `json:"format_version"`
	Kind            string             `json:"kind"`
	RunID           string             `json:"run_id"`
	Timestamp       string             `json:"timestamp"`
	DurationMS      float64            `json:"duration_ms"`
	Git             GitInfo            `json:"git"`
	Host            HostInfo           `json:"host"`
	Profile         string             `json:"profile"`
	Emulated        bool               `json:"emulated"`
	Synthetic       bool               `json:"synthetic"`
	ContainerLimits ContainerLimits    `json:"container_limits"`
	Isolation       Isolation          `json:"isolation"`
	Provider        ProviderInfo       `json:"provider"`
	Model           ModelInfo          `json:"model"`
	Backend         BackendInfo        `json:"backend"`
	Components      Components         `json:"components"`
	Corpus          RAGCorpus          `json:"corpus"`
	Config          RAGConfig          `json:"config"`
	Companion       CompanionRef       `json:"companion"`
	Metrics         Metrics            `json:"metrics"`
	ByKind          map[string]Metrics `json:"by_kind"`
	ByLanguage      map[string]Metrics `json:"by_language"`
	Modes           RetrievalModes     `json:"retrieval_modes"`
	Latency         RAGLatency         `json:"latency"`
	Ingestion       Ingestion          `json:"ingestion"`
	Index           IndexInfo          `json:"index"`
	Memory          RAGMemory          `json:"memory"`
	ContainerMemory MemoryInfo         `json:"container_memory"`
	Quantization    *Quantization      `json:"quantization"`
	Scale           *ScaleResult       `json:"scale"`
	Questions       []QuestionRecord   `json:"questions"`
}

// Isolation says what limits the run actually ran under. The profile label of
// a result is the profile the run STANDS FOR (as in bench: a result under lite
// is emulated, never certified); rageval runs natively on the host and applies
// no cgroup cap, and says so here.
type Isolation struct {
	CgroupLimitsApplied bool   `json:"cgroup_limits_applied"`
	Note                string `json:"note"`
}

// Components describes what answered each role.
type Components struct {
	Embedder  EmbedderInfo  `json:"embedder"`
	Generator GeneratorInfo `json:"generator"`
	Reranker  RerankerInfo  `json:"reranker"`
}

// EmbedderInfo is the embedder: the fake (hashed bag of words) or LocalAI.
type EmbedderInfo struct {
	Kind          string  `json:"kind"` // fake, localai, none
	Model         *string `json:"model"`
	Dimensions    *int    `json:"dimensions"`
	ReportedModel *string `json:"reported_model"`
}

// GeneratorInfo is the answer generator. The extractive one is the no-model
// baseline and is label-blind.
type GeneratorInfo struct {
	Kind         string  `json:"kind"` // extractive
	LabelBlind   bool    `json:"label_blind"`
	MinRatio     float64 `json:"min_ratio"`
	PromptID     string  `json:"prompt_id"`
	PromptSHA256 string  `json:"prompt_sha256"`
}

// RerankerInfo is the reranker, if any. The lexical one is the harness's: a word
// overlap score, no model, so it is always synthetic and a result that used it
// says so (no adapter serves the Reranker role, and no compact reranker is in
// the model registry).
type RerankerInfo struct {
	Kind      string `json:"kind"` // none, lexical
	Synthetic bool   `json:"synthetic"`
}

// RAGCorpus identifies the corpus and counts what is in it.
type RAGCorpus struct {
	ID         string         `json:"id"`
	SHA256     string         `json:"sha256"`
	Files      int            `json:"files"`
	Bytes      int64          `json:"bytes"`
	Sources    int            `json:"sources"`
	Documents  int            `json:"documents"`
	Tenants    int            `json:"tenants"`
	Questions  int            `json:"questions"`
	Answerable int            `json:"answerable"`
	ToDecline  int            `json:"to_decline"`
	Languages  map[string]int `json:"languages"`
	Kinds      map[string]int `json:"kinds"`
}

// RAGConfig is how the run was configured.
type RAGConfig struct {
	Mode string `json:"mode"`
	// RAGCaps names the profile whose evidence and candidate caps the run used
	// (default: the pipeline's own).
	RAGCaps          string        `json:"rag_caps"`
	K                []int         `json:"k"`
	Query            QueryConfig   `json:"query"`
	Answer           AnswerConfig  `json:"answer"`
	Chunker          ChunkerConfig `json:"chunker"`
	Warmup           int           `json:"warmup_requests"`
	TimeoutMS        int64         `json:"timeout_ms"`
	PercentileMethod string        `json:"percentile_method"`
}

// QueryConfig is the retrieval configuration of the run (internal/rag/query).
type QueryConfig struct {
	RRFK             int     `json:"rrf_k"`
	MaxQueryTerms    int     `json:"max_query_terms"`
	FTSCandidates    int     `json:"fts_candidates"`
	VectorCandidates int     `json:"vector_candidates"`
	VectorMinScore   float64 `json:"vector_min_score"`
	Shortlist        int     `json:"shortlist"`
	MaxItems         int     `json:"max_items"`
	MaxToks          int     `json:"max_toks"`
	ParentExpansion  bool    `json:"parent_expansion"`
	MaxParentLookups int     `json:"max_parent_lookups"`
	MaxParentToks    int     `json:"max_parent_toks"`
	Rerank           bool    `json:"rerank"`
}

// AnswerConfig is the answer configuration of the run (internal/rag/answer).
type AnswerConfig struct {
	MaxOutputToks   int     `json:"max_output_toks"`
	MaxClaims       int     `json:"max_claims"`
	MaxCitations    int     `json:"max_citations"`
	MinCoverage     float64 `json:"min_coverage"`
	WindowSentences int     `json:"window_sentences"`
	MinClaimTerms   int     `json:"min_claim_terms"`
}

// ChunkerConfig is the chunker configuration (internal/rag/chunk).
type ChunkerConfig struct {
	ChildMinToks  int `json:"child_min_toks"`
	ChildMaxToks  int `json:"child_max_toks"`
	ParentMaxToks int `json:"parent_max_toks"`
}

func queryConfigInfo(c query.Config) QueryConfig {
	return QueryConfig{
		RRFK: c.RRFK, MaxQueryTerms: c.MaxQueryTerms, FTSCandidates: c.FTSCandidates,
		VectorCandidates: c.VectorCandidates, VectorMinScore: c.VectorMinScore, Shortlist: c.Shortlist,
		MaxItems: c.MaxItems, MaxToks: c.MaxToks, ParentExpansion: c.ParentExpansion,
		MaxParentLookups: c.MaxParentLookups, MaxParentToks: c.MaxParentToks, Rerank: c.Rerank,
	}
}

func answerConfigInfo(c answer.Config) AnswerConfig {
	return AnswerConfig{
		MaxOutputToks: c.MaxOutputToks, MaxClaims: c.MaxClaims, MaxCitations: c.MaxCitations, MinCoverage: c.MinCoverage,
		WindowSentences: c.WindowSentences, MinClaimTerms: c.MinClaimTerms,
	}
}

func chunkerConfigInfo(c chunk.Config) ChunkerConfig {
	return ChunkerConfig{ChildMinToks: c.ChildMinToks, ChildMaxToks: c.ChildMaxToks, ParentMaxToks: c.ParentMaxToks}
}

// CompanionRef names the bench-envelope result that goes with this one.
type CompanionRef struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Cases  int    `json:"cases"`
}

// RetrievalModes counts how the queries were served and why a stage did not
// contribute. In a hybrid run every query is hybrid; a query that fell back to
// full-text only says why.
type RetrievalModes struct {
	Served   map[string]int `json:"served"`
	Degraded map[string]int `json:"degraded"`
}

// RAGLatency is the latency of the labelled questions, per question, in ms:
// end to end (retrieval and answer), retrieval alone and the answer stage.
type RAGLatency struct {
	EndToEnd  Stats `json:"end_to_end_ms"`
	Retrieval Stats `json:"retrieval_ms"`
	Answer    Stats `json:"answer_ms"`
	// Stages is the latency of each stage of the retrieval pipeline over the
	// questions (the pipeline's own timing): stages.embed is the query-embedding
	// latency, the cost a model adds to every query.
	Stages        map[string]Stats `json:"stages_ms"`
	WarmupFirstMS *float64         `json:"warmup_first_ms"`
	ToksIn        int              `json:"toks_in"`
	ToksOut       int              `json:"toks_out"`
}

// deriveLatency derives the latency block from the records alone.
func deriveLatency(recs []QuestionRecord, warmupFirst *float64) RAGLatency {
	var e2e, ret, ans []float64
	byStage := map[string][]float64{}
	l := RAGLatency{WarmupFirstMS: warmupFirst, Stages: map[string]Stats{}}
	for _, r := range recs {
		if r.Status != StatusOK || r.LatencyMS == nil || r.RetrievalLatencyMS == nil || r.AnswerLatencyMS == nil {
			continue
		}
		e2e = append(e2e, *r.LatencyMS)
		ret = append(ret, *r.RetrievalLatencyMS)
		ans = append(ans, *r.AnswerLatencyMS)
		for name, v := range r.StagesMS {
			byStage[name] = append(byStage[name], v)
		}
		l.ToksIn += r.ToksIn
		l.ToksOut += r.ToksOut
	}
	l.EndToEnd, l.Retrieval, l.Answer = Summarize(e2e), Summarize(ret), Summarize(ans)
	for name, v := range byStage {
		l.Stages[name] = Summarize(v)
	}
	return l
}

// Ingestion is the cost of building the labelled corpus's index: chunk, index,
// embed (through the vector pipeline), validate and activate. The sandboxed
// parser and the job queue of the real ingestion pipeline are not in it (the
// corpus is Markdown, already text), so it is chunk + index + embed
// throughput, not end-to-end ingestion throughput.
type Ingestion struct {
	Covers       string  `json:"covers"`
	Documents    int     `json:"documents"`
	Builds       int     `json:"builds"`
	Parents      int     `json:"parents"`
	Chunks       int     `json:"chunks"`
	BytesIn      int64   `json:"bytes_in"`
	ChunkingMS   float64 `json:"chunking_ms"`
	IndexingMS   float64 `json:"indexing_ms"`
	EmbeddingMS  float64 `json:"embedding_ms"`
	FinalizingMS float64 `json:"finalizing_ms"`
	// ElapsedMS is the sum of the four stages.
	ElapsedMS          float64  `json:"elapsed_ms"`
	ChunksPerSecond    *float64 `json:"chunks_per_second"`
	DocumentsPerSecond *float64 `json:"documents_per_second"`
	MiBPerSecond       *float64 `json:"mib_per_second"`
	Embedded           int      `json:"embedded"`
	Reused             int      `json:"reused"`
	EmbedCalls         int      `json:"embed_calls"`
}

const ingestionCovers = "chunk + index + embed + validate + activate; the sandboxed parser and the job queue are not in it"

// deriveIngestion fills ElapsedMS and the rates from the counts and the stage
// times, so that a result and its checker use one definition.
func deriveIngestion(in Ingestion) Ingestion {
	in.ElapsedMS = *round3(in.ChunkingMS + in.IndexingMS + in.EmbeddingMS + in.FinalizingMS)
	in.ChunksPerSecond, in.DocumentsPerSecond, in.MiBPerSecond = nil, nil, nil
	if in.ElapsedMS > 0 {
		sec := in.ElapsedMS / 1000
		in.ChunksPerSecond = round3(float64(in.Chunks) / sec)
		in.DocumentsPerSecond = round3(float64(in.Documents) / sec)
		in.MiBPerSecond = round3(float64(in.BytesIn) / (1 << 20) / sec)
	}
	return in
}

// IndexInfo is the size of the labelled corpus's index. StoreUsed is the pages
// in use: the text, the full-text index, the parents, the vectors and the
// bookkeeping tables around them. StoreDisk is the files on disk, which are
// larger while the write-ahead log has not been folded back.
type IndexInfo struct {
	StoreUsedBytes int64    `json:"store_used_bytes"`
	StoreDiskBytes int64    `json:"store_disk_bytes"`
	Chunks         int      `json:"chunks"`
	Vectors        int      `json:"vectors"`
	BytesPerChunk  *float64 `json:"bytes_per_chunk"`
}

func deriveIndex(in IndexInfo) IndexInfo {
	in.BytesPerChunk = nil
	if in.Chunks > 0 {
		in.BytesPerChunk = round3(float64(in.StoreUsedBytes) / float64(in.Chunks))
	}
	return in
}

// RAGMemory is the process memory of the labelled evaluation: the corpus load,
// the ingestion and every question. It is the operating system's figure for
// this process (resident set and its high-water mark), not a container's. The
// scale phases each report their own, in scale.
type RAGMemory struct {
	Measured  bool     `json:"measured"`
	Source    string   `json:"source"`
	RSSMB     *float64 `json:"rss_mb"`
	PeakRSSMB *float64 `json:"peak_rss_mb"`
	Covers    string   `json:"covers"`
}

const memoryCovers = "the process that ingested the labelled corpus and answered every question; the scale phases are in scale"
