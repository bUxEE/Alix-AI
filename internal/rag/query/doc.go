// Package query is the authorised hybrid retrieval pipeline of master plan 12.4:
// it turns one question and a verified scope into a bounded EvidencePackage and
// nothing else. It does not generate, verify claims or cite (M2-10); it opens no
// network connection and knows no external provider.
//
//	question + scope
//	  -> deterministic filters (scope, readable sources, exact identifiers, term cap)
//	  -> authorised FTS5/BM25      \  side by side
//	  -> authorised vector search  /
//	  -> Reciprocal Rank Fusion (k = 60)
//	  -> optional parent expansion (bounded)
//	  -> optional reranking (Reranker role, off in Lite)
//	  -> final authorisation and version recheck
//	  -> bounded EvidencePackage
//
// # Authority
//
// The scope is applied inside the stores, before ranking: the lexical index
// searches the caller's rowid range and the vector index reads the caller's
// generations, so a chunk of another scope is never a candidate. This package
// repeats the check at every hand-over and never relies on a collaborator
// having done it: a hit of a source the principal may not read is dropped at
// fusion, a chunk the vector stage found is read back under the caller's scope
// before it has any text, a reader that names another source for it is refused,
// and the last step before the package is built is index.Recheck (scope,
// tombstone, serving generation, document version, hidden text) plus a fresh read
// of the source catalog. A source revoked, removed or re-indexed while the query
// runs is gone from the package (SEC-028). A failed read is an error, never an
// empty package: an empty package means "nothing authorised matched".
//
// The model roles hold no authority. The embedder sees the question; the reranker
// sees the question and at most RerankCandidates chunk texts that are already
// authorised, and can only reorder them; the rewriter sees the question alone and
// returns search words that go through the same authorised full-text stage. An
// exact-identifier match is never moved behind a fused or reranked one.
//
// # Degradation
//
// Every optional stage degrades to the deterministic one beneath it and says so:
// with no usable Embedder or vector index the result is FTS-only and the package
// carries the reduced_semantic_recall notice; a missing, unhealthy, slow or
// misbehaving reranker leaves the fused order; a failed rewrite leaves the first
// pass. Result.Diagnostics lists why, in stable strings that hold no tenant data.
//
// # Budgets
//
// Nothing is unbounded and nothing is clamped silently: Config validates every
// number. The candidate lists, the shortlist, the query terms, the parent
// sections read, the reranker's input, the rewrites, the package items and its
// estimated tokens, and the time of every stage and of the query are capped. The
// full-text stage is the one that can be slow inside a large scope (ADR 0032:
// BM25 scores every row that matches any term), so its terms and candidates are
// capped before the index runs and its deadline is typed: a timeout is an error,
// not an empty answer. docs/efficiency/M2-09.md has the measurements.
//
// # Seams
//
// Lexical, Vectors and HitReader are what the pipeline needs from the indexes;
// SourceCatalog is the deterministic authority on sources, implemented by the
// composition that owns the policy gateway and the source registry. Each item of
// the package carries its source's data-egress mode and classification, copied
// unchanged (the most restrictive values when the declaration states none), so a
// later data-egress filter can drop or shorten a passage without querying again.
// Those two values are internal and are never part of any client type.
//
// There is no cache in this package. A cache of results must be keyed by
// index.CacheKey over Result.Diagnostics.Version, which binds it to the scope,
// the principal, its roles, the epoch and the active generations.
package query
