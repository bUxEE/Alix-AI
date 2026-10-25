// Package index is the scoped lexical index of the RAG pipeline: the FTS5/BM25
// store behind master plan 12.3 ("FTS indexing", "index generation activation")
// and 12.4 ("authorized FTS5/BM25 retrieval", "final authorization/version
// recheck"). It is SQLite module "ragindex" (migrations/0001_init.sql); only
// internal/store links the SQLite driver.
//
// # Isolation
//
// Every table carries application_id, tenant_id and workspace_id, the FTS5
// table included, and every statement filters on them (store.ScopedWhere).
// Search applies the scope in SQL, before ranking and before any chunk text is
// read; there is no post-filtering of results anywhere. Chunk text leaves the
// package only from a statement that repeats the whole visibility predicate
// (scope on the chunk and on its generation, the generation being the active
// one of its source, the chunk not tombstoned, not hidden), and every row that
// comes back is checked once more in Go: a row outside the caller's scope is
// an ErrScopeViolation that refuses the whole result. An id that exists only in
// another scope is reported exactly like one that does not exist.
//
// # Why one FTS5 table with a rowid range per scope
//
// M0-05 measured that a filter on an UNINDEXED FTS5 column is applied after
// the match: BM25 p95 reached about 200 ms at 100k rows, because every row that
// matches the term is found, scored and sorted whatever tenant owns it. Three
// shapes were considered:
//
//   - A FTS5 table per scope. Isolation is physical, but a table (and its
//     shadow tables) per workspace multiplies schema, migrations, WAL pages
//     and open cursors by the number of workspaces, and queries over a scope
//     change with its table name (SQL built from identifiers). Rejected for the
//     Lite profile, where hundreds of small workspaces are the normal case.
//   - A filter on the UNINDEXED columns or a join to a scope table, after the
//     match: the measured problem. Kept only as the control of the benchmark.
//   - One FTS5 table in which each (application, tenant, workspace) scope owns
//     a contiguous rowid range, rid = scope_no << 36 | seq. A query adds
//     "rowid >= lo AND rowid <= hi" to the MATCH; FTS5 pushes both bounds into
//     the posting-list scan, so rows of other scopes are not matched, scored
//     or sorted. Chosen. The range comes from ragindex_scope, which is itself
//     read under the scope predicate, so a caller cannot name a range.
//
// The range is the performance mechanism; the authorisation is the SQL join to
// ragindex_chunk and ragindex_generation under the scope predicate, which
// holds even if the range were wrong (TestScopeFilterAppliedInSQL runs a
// corpus in which a foreign tenant owns the 300 best matches, asserts the SQL
// carries the predicates before ORDER BY, and inspects the query plan for the
// FTS scan as the outer loop with both rowid bounds). The statement spells its
// joins CROSS JOIN: with a plain JOIN SQLite drives from the chunk table and
// runs one MATCH per chunk row, a query that does not finish at 100k rows.
//
// The FTS5 table is external-content over ragindex_chunk (one copy of the
// text), its columns are text, section, path and header (BM25 weights 1, 3,
// 1.5 and 2), and the scope and source columns are UNINDEXED labels that are
// never filtered on. Heading text lives only in a parent chunk, so each child
// is indexed with its section title, heading path and table header next to its
// own text. Citation spans (Hit.Span, Hit.Locator) are Unicode code points, as
// the chunker produces them; byte offsets never leave the chunker.
//
// # Generations
//
// A generation is one build of one source with one set of component versions
// (parser, chunker, OCR, embedder): building -> validating -> active ->
// superseded, with failed and invalidated as terminal states. BeginGeneration
// opens a build, PutDocument fills it, ValidateGeneration checks it against an
// Expectation and the full-text index itself, and ActivateGeneration flips the
// pointer in one transaction: the previous generation becomes superseded and
// the new one active, so readers see the old one or the new one, never both and
// never neither, and a crash before the commit leaves the old one serving. The
// pointer is a partial unique index (one active generation per source), so the
// database refuses a second one even through a bug. A generation that is still
// building is invisible to every query. The previous generation is retained
// (superseded) until compaction, which makes a rollback an ActivateGeneration
// of it.
//
// # Deletion
//
// DeleteDocument, DeleteSource, DeleteScope and DeleteTenant set tombstones in
// one transaction; every read predicate excludes a tombstoned row, so content
// is hidden at once and the same transaction bumps the scope's epoch, which
// changes every cache key. Compact later removes the rows physically, in
// bounded batches on the single writer connection (a row budget per call,
// short transactions, a grace period), and also retires failed, invalidated
// and surplus superseded generations and fails abandoned builds. The FTS5
// index entries and identifier rows follow their chunk through triggers.
//
// # Retrieval and exact identifiers
//
// Search reduces the query text to quoted terms: no FTS5 syntax typed by a
// user has any effect, stop words are dropped from OR expressions, and at most
// MaxQueryTerms terms are kept (BM25 scores every row that matches any term, so
// the query is bounded before it reaches SQLite). Part numbers, order codes and
// SKUs are recognised by shape alone (ident.go) and stored whole in
// ragindex_ident under a normalised key; the lookup is a B-tree range scan in
// the caller's scope, independent of the tokenizer and of corpus size. A chunk
// that holds an identifier of the query exactly gets Config.ExactIdentifierBoost
// added to its BM25 score once per distinct identifier, which outranks any
// text-only match; BM25 orders the rest. An identifier held by more than a
// hundred chunks of the scope is not selective and is left to BM25.
//
// # Recheck and cache keys
//
// Recheck is the final authorisation and version recheck of 12.4: in a fresh
// snapshot it confirms that each chunk is still in the scope, not tombstoned,
// in the generation serving its source and at the document version the caller
// saw. ParentOf expands a hit to its parent section under the same predicate.
// CacheKey binds a cache entry to the full authorisation scope (application,
// tenant, workspace, principal, roles, capability scopes), to the index Version
// (the scope's epoch and the serving generation of each source) and to the
// query material; Result.Version comes from the same snapshot as the hits, so
// results and version never disagree.
//
// # Measurements (dev box, 100k rows)
//
// The numbers below come from TestBenchScopedSearch (set
// ALIXAI_INDEX_BENCH_ROWS=100000; ALIXAI_INDEX_BENCH_DIST selects the scope
// distribution). Corpus: M0-05's, 30 words per chunk from a 3000-word
// vocabulary with a Zipf distribution, the worst case for BM25; 100 queries per
// kind, run by one reader and by four concurrent readers. Windows 11, Intel
// i7-10700K, Go 1.27, CGO_ENABLED=0, modernc.org/sqlite, default store profile
// (mmap off, 16 MB page cache). The box was shared with other builds, so two
// runs are given as a range; the figures are labelled dev box, neither
// emulated nor certified.
//
//	                     scoped (this package)           post-filter control (M0-05 way)
//	distribution  query  4 readers p50 / p95 (ms)        4 readers p50 / p95 (ms)
//	5 x 20k       AND2   32-35 / 113-143                  63 / 509
//	5 x 20k       OR16   215-283 / 279-418                897 / 1104
//	100 x 1k      AND2   8-11 / 17-25                     79 / 608
//	100 x 1k      OR16   40-51 / 56-104                   701 / 816
//	skewed (50k + 50 x 1k)  AND2  16-18 / 250-325         58 / 432
//	skewed                  OR16  409-536 / 511-721       907 / 1493
//
// AND2 is two head (most frequent) terms joined by AND; OR16 is sixteen Zipf
// terms joined by OR (the longest query the parser allows, and the
// worst case: BM25 scores nearly every chunk of the scope). The part-number
// query (OR8 plus an identifier) costs about 80 percent of OR16: the
// identifier lookup is one B-tree range scan per key and adds little.
//
// What the design buys is that a query costs what its own scope costs, not what
// the corpus costs: a workspace of 1k chunks answers in tens of milliseconds
// at 100k rows in the index, where the post-filter control takes hundreds, and
// the cost of one tenant no longer leaks into another's latency. What it does
// not buy is a cheaper BM25: a scope that itself holds 50k chunks, queried with
// head terms, scores every one of them (AND2 p95 250 to 325 ms with four
// readers, above M0-05's headline of about 200 ms, whose scopes held 20k), and
// the same corpus in one scope is slower still (AND2 p95 389 ms with one reader
// and 707 ms with four; OR16 p95 1025 ms and 986 ms). Mitigations are in the query, not in the schema (stop words,
// MaxQueryTerms, a result limit), and a deployment that holds a very large
// workspace should expect to raise its page cache or enable mmap, as M0-05
// measured (a third to a half faster, at +150 MiB of working set).
//
// Build rate with the FTS5 triggers, identifier rows and parent rows: 4.5k to
// 7.2k chunks per second across the runs (100k in 14 to 22 s, one transaction
// per 1000-chunk document), database size 75 MiB
// for 100k chunks of about 200 bytes. One FTS5 copy of the text is stored (the
// external-content table holds only the index).
//
// # Decisions the plan left open
//
//   - A rowid range per scope inside one FTS5 table, not a table per scope.
//     The trade-off: BM25's inverse document frequency and average length are
//     computed over the whole table, so the order of one tenant's results
//     shifts a little with the size and vocabulary of other tenants' corpora.
//     No text and no score crosses a scope (Hit.Score is relative to its own
//     result and is not exposed to clients), so this is not a content leak; a
//     table per scope would avoid it at the cost described above.
//   - Scope numbers are allocated once and never reused; a range holds 2^36
//     rows, and rowids are not reused inside it, so a stale index entry of a
//     deleted chunk cannot collide with a new one. Exhaustion is a typed
//     ErrResourceExhausted, not a wrap-around.
//   - The scope epoch (not wall time) is the "source version" of cache keys.
//   - Hidden chunks (SEC-012) are excluded by default and by the recheck.
//   - Identifier matching is by shape, deterministic, no model and no word list.
package index
