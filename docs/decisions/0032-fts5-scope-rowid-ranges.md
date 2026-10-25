# 0032 — Authorised full-text retrieval: one FTS5 table, one rowid range per scope

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M2-06 (`internal/rag/index`)

## Context
Retrieval authorisation must be applied before any chunk text can reach a model (§10.2). M0-05 measured that filtering an
FTS5 table on an UNINDEXED scope column happens AFTER the MATCH, so every query pays for the whole corpus (p95 about 200 ms at
100k rows) and the filter sits on the wrong side of the ranking.

## Options
1. Post-filter on an UNINDEXED scope column (rejected: corpus-wide cost, authorisation after ranking).
2. One FTS5 table per scope (rejected: multiplies schema, WAL pages and cursors across workspaces — hurts the Lite profile).
3. **One external-content FTS5 table; each scope owns a contiguous rowid range** (`scope_no << 36 | seq`); the ranking query adds
   `rowid BETWEEN lo AND hi` to the MATCH, written with CROSS JOIN so the FTS scan stays the outer loop.

## Decision
Option 3. Scope and source stay as UNINDEXED labels and are re-checked: chunk text is read in a second statement repeating
the full predicate (scope, active generation, not tombstoned, not hidden) and every row is verified again in Go; a foreign row
refuses the whole result. Scope numbers and rowids are never reused; exhausting a range returns `resource_exhausted`.
Index generations (building, validating, active, superseded, failed, invalidated) flip in one transaction backed by a partial
unique index; a building generation is invisible. Tombstones hide content immediately and bump a scope epoch used as the cache
version; compaction is bounded and later. Exact identifiers (codes, ids, SKUs) are stored whole and found by B-tree range scan,
recognised by shape alone (no model, no word list).

## Measured (dev box, 100k rows, Zipf corpus, 4 readers; neither emulated nor certified)
Scoped versus the post-filter control, p50/p95 ms: 5x20k AND2 32-35/113-143 vs 63/509; 100x1k AND2 8-11/17-25 vs 79/608;
skewed (one 50k scope + 50x1k) AND2 16-18/250-325 vs 58/432, OR16 409-536/511-721 vs 907/1493. Build 4.5k-7.2k chunks/s; 100k
chunks = 75 MiB.

## Consequences
- A query now costs what its OWN scope costs, not the corpus. It does NOT make BM25 cheaper inside a large scope: a 50k-chunk
  scope gave AND2 p95 250-325 ms (389 ms single reader, 707 ms with four) and OR16 p95 about 1 s. Read "measured at 100k" as
  "the corpus-wide penalty is gone", not "large-scope BM25 is fast". Mitigations to design in M2-09: candidate caps, term caps (16),
  stop-word dropping, per-workspace corpus ceilings in the Lite profile, and measuring before promising latency.
- BM25 IDF and average length are shared across the table, so one tenant's ranking order shifts slightly with other tenants'
  corpora. No text or score crosses a scope, so this is not a content leak, but it is documented.

## Reversal plan
The layout is behind `internal/rag/index`. A table-per-scope or external-index provider can replace it without changing callers
(authorised search, recheck, parent expansion, cache key, tombstones); data is rebuildable from the sources.

## Open question recorded 2026-10-03 (M2-14)
The harness (`tools/rageval`, 100,000 chunks in 32 scopes) shows that a small scope is not free of the corpus: the same 1,000-chunk scope has p50 6.1 ms alone, 15.9 ms in a 30,000-chunk
corpus and 17.4 ms in a 100,000-chunk corpus. This partly contradicts the sentence "costs what its own scope costs, not the corpus", although it still holds against the post-filter
control (the cost is far below the whole-corpus scan). The hypothesis, not yet measured, is the 2 MB reader cache; M2-15 tests it. Latency by scope size is reported per scope, never as a
corpus-wide number: the corpus-wide p95 was 1.7 to 3.2 times lower than the 50k scope's p95 (a hidden tail).
