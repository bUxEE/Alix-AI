package query

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Lexical is the part of the lexical index the pipeline uses. *index.Index
// implements it. Every method takes the caller's scope and applies it in the
// index, never in this package.
type Lexical interface {
	// Search is authorised BM25 retrieval with exact-identifier matching.
	Search(ctx context.Context, scope contracts.Scope, q index.Query) (index.Result, error)
	// Version reads the scope's version: its epoch and the generation serving
	// each source.
	Version(ctx context.Context, scope contracts.Scope) (index.Version, error)
	// ParentOf expands a hit to the section it sits under.
	ParentOf(ctx context.Context, scope contracts.Scope, h index.Hit, includeHidden bool) (index.Parent, bool, error)
	// Recheck is the final authorisation and version recheck of master plan 12.4.
	Recheck(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Verdict, error)
}

// Vectors is the search half of vector.VectorIndexProvider.
type Vectors interface {
	Search(ctx context.Context, scope contracts.Scope, q vector.Query) (vector.SearchResult, error)
}

var (
	_ Lexical = (*index.Index)(nil)
	_ Vectors = vector.VectorIndexProvider(nil)
)

// HitReader reads chunks by reference. The vector index returns chunk, generation
// and source IDs only, so a candidate that only the vector stage found has no
// text, span, document version or parent until the lexical index reads it.
//
// Hits returns, for each ref whose chunk is visible to the scope in this very
// read (in the scope, not tombstoned, in the generation serving its source, not
// hidden), the same index.Hit the lexical search would have returned for it,
// with Rank zero. It omits every other ref without saying why, and never
// returns a chunk of another scope: it must apply the same visibility rules as
// index.Search. A Ref with an empty DocumentVersion matches any version.
//
// *index.Index exports such a method (index.Hits, M2-16) and is used when
// Deps.Hits is nil and Deps.Lexical implements it; with no HitReader at all a
// vector-only candidate is dropped and the result is labelled
// reduced_semantic_recall. The final index.Recheck still runs on
// whatever a HitReader returns, so a reader that returned the wrong chunk
// could not put it into a package.
type HitReader interface {
	Hits(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Hit, error)
}

// SourceState is what the pipeline knows about one source at one moment. It is
// read twice per query: before retrieval, to decide which sources may be
// searched, and again just before the package is built, so a source revoked or
// removed while the query ran is gone from the result.
type SourceState struct {
	// DisplayName is the source's name as the requesting principal may see it.
	// Empty falls back to the source ID.
	DisplayName string
	// Authorised is false when the principal may no longer read the source: it
	// was revoked, or never granted. A source the catalog does not return is
	// treated exactly like Authorised false.
	Authorised bool
	// Availability says whether the principal can open the source now.
	// Removed drops the source's evidence; Unavailable keeps it, labelled. The
	// zero value is read as Unavailable: nothing is claimed as checked that was
	// not.
	Availability conversation.CitationAvailability
	// Staleness says whether the indexed version is the latest. The zero value
	// is read as Unknown.
	Staleness conversation.CitationStaleness
	// OpenRef is the opaque, permission-checked way to open the source. Nil
	// means "the source itself", resolved by source ID.
	OpenRef *conversation.CitationOpenRef
	// Egress and Classification are the source's data-egress mode and data
	// classification, copied unchanged from the source's declaration so a later
	// egress filter can act on the package without querying again. Nil means the
	// declaration states none; the package then carries the most restrictive
	// values (local_only, restricted), never a permissive default.
	Egress         *rag.SourceEgress
	Classification *rag.SourceClassification
}

// SourceCatalog is the deterministic authority on sources: which of them the
// principal may read, whether each can be opened, whether its indexed version
// is current, and what its declaration says about egress. The pipeline asks it
// about the sources of the scope's active index generations; it must apply the
// scope it is handed and may not widen it. An error is a failed check, never a
// grant: the query fails closed.
type SourceCatalog interface {
	Sources(ctx context.Context, scope contracts.Scope, sourceIDs []string) (map[string]SourceState, error)
}

// Quarantine is what the pipeline needs from the upgrade's quarantine of isolated
// derived indexes (internal/upgrade.Quarantine implements it; this package does
// not import the upgrade). Has reports whether an index is isolated: its damaged
// contents were dropped and it has not been rebuilt, so it would answer with
// nothing. The pipeline consults it once per query and never searches an isolated
// index: it labels the result instead (master plan 18).
type Quarantine interface {
	Has(name string) bool
}

// The names the retrieval indexes have in the quarantine: the lexical index is the
// FTS5 table the upgrade finds in the database, the vector index is the one the
// upgrade wires in (upgrade.VectorIndexName). A test pins both against the upgrade.
const (
	LexicalIndexName = "ragindex_fts"
	VectorIndexName  = "vector_rows"
)

// Request is one question.
type Request struct {
	// Text is the question, as asked. It becomes EvidencePackage.query
	// unchanged, whatever a rewrite did to the text searched.
	Text string
	// SourceIDs restricts the search to these sources (a deterministic filter).
	// Empty means every source the principal may read. An ID the principal may
	// not read is ignored, exactly like one that does not exist.
	SourceIDs []string
	// MaxItems and MaxToks lower the package caps for this request; zero means
	// the configured value. Asking for more than the configuration allows is an
	// error, not a clamp.
	MaxItems int
	MaxToks  int
}

// Result is the outcome of one query.
type Result struct {
	// Package is the bounded evidence package. INTERNAL: it carries each
	// source's egress mode and classification and is never serialised to a
	// client.
	Package rag.EvidencePackage
	// Diagnostics describes how the answer was found, for logs, metrics and the
	// efficiency review. It holds no evidence text and no scores a client could
	// see.
	Diagnostics Diagnostics
}

// Mode names how a query was served.
type Mode string

const (
	// ModeHybrid ran the full-text and the vector stage.
	ModeHybrid Mode = "hybrid"
	// ModeFTSOnly ran the full-text stage alone, because no embedder was
	// available or the vector stage could not contribute. The package carries
	// the reduced_semantic_recall notice.
	ModeFTSOnly Mode = "fts_only"
	// ModeVectorOnly ran the vector stage alone, because the lexical index is
	// isolated (damaged and not yet rebuilt). Exact-identifier matches and
	// keyword recall are reduced; the package carries the reduced_semantic_recall
	// notice, the only reduced-recall label the package schema has.
	ModeVectorOnly Mode = "vector_only"
)

// Degradation reasons recorded in Diagnostics.Degraded. They are stable
// strings; none carries tenant data.
const (
	DegradedNoEmbedder         = "no_embedder"
	DegradedEmbedderDown       = "embedder_unavailable"
	DegradedEmbedFailed        = "embed_failed"
	DegradedNoVectorIndex      = "no_vector_index"
	DegradedVectorFailed       = "vector_search_failed"
	DegradedGenerationsSkipped = "vector_generations_skipped"
	DegradedModelMismatch      = "embedding_model_mismatch"
	DegradedNoHitReader        = "no_hit_reader"
	DegradedHydrateFailed      = "hydrate_failed"
	DegradedParentFailed       = "parent_expansion_failed"
	DegradedNoReranker         = "no_reranker"
	DegradedRerankFailed       = "rerank_failed"
	DegradedNoRewriter         = "no_rewriter"
	DegradedRewriteFailed      = "rewrite_failed"
	// DegradedLexicalIsolated and DegradedVectorIsolated: the index is in the
	// upgrade's quarantine, so it was not searched; its absence is not "no match".
	DegradedLexicalIsolated = "lexical_index_isolated"
	DegradedVectorIsolated  = "vector_index_isolated"
)

// Diagnostics describes one query.
type Diagnostics struct {
	Mode Mode
	// Degraded lists why a stage did not contribute, in the order it happened.
	Degraded []string
	// Isolated names the indexes that were in the quarantine when the query ran
	// and were therefore not searched (LexicalIndexName, VectorIndexName).
	Isolated []string
	// Version is the index version the query started from.
	Version index.Version

	// Durations of the stages. A stage that did not run is zero.
	Total, FTS, Embed, Vector, Hydrate, Expand, Rerank, Recheck, Rewrite time.Duration

	// Candidates.
	FTSCandidates    int // hits of the first full-text pass
	VectorCandidates int // hits of the vector stage
	Fused            int // distinct candidates after fusion
	Shortlisted      int // candidates that went on after the cut
	Rewrites         int // alternative queries run
	ParentLookups    int // sections read
	Expanded         int // items replaced by their section
	Reranked         bool

	// Dropped counts the candidates that did not reach the package, by reason.
	DroppedUnhydrated   int // vector-only candidates with no text to read
	DroppedUnauthorised int // source not authorised (before or at the final check)
	DroppedRecheck      int // failed the index's final authorisation and version recheck
	DroppedCovered      int // a section already included for a better child
	DroppedBudget       int // would not fit MaxItems or MaxToks
	DroppedInvalid      int // item that would not satisfy the package schema
}
