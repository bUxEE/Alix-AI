# 0011 — Embedded vector-index implementation and supported corpus ceiling

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-16 (provisional default and ceiling); finalised in M2-07

## Context
§6 makes vector search a replaceable `VectorIndexProvider` with an embedded candidate first, and
§5.1 forbids an external vector database in the minimum deployment. §25 item 2 left the
implementation and the supported corpus ceiling open. The embedding model is Granite Embedding
97M Multilingual R2 (April 2026, Apache-2.0): **384 dimensions**, 32K context, about 97M
parameters, cosine similarity on normalised embeddings, no Matryoshka truncation (model card,
re-checked 2026-10-02).

Requirements the implementation must meet:

- **R1.** Scope is applied *before* candidate text can leave the provider: exact predicates on
  `application_id, tenant_id, workspace_id` plus the authorisation/source-version filter (§10.2,
  §12.4). Post-filtering a global top-k is not acceptable (SEC-021).
- **R2.** Pure Go, `CGO_ENABLED=0`, all release targets (ADR [0001](0001-go-core-service.md),
  [0009](0009-pure-go-sqlite-driver.md)).
- **R3.** Bounded RAM: the 4 GB direction (rule 17) counts every resident byte.
- **R4.** Derived and rebuildable: the index is regenerated from chunks by queued class-5 jobs
  (§12.3), and a delete removes vectors.
- **R5.** Embedding reuse must not become a cross-tenant existence oracle (SEC-023).

Embeddings of confidential text are themselves sensitive (they can leak content). Vectors get the
same scope columns, retention, backup and deletion rules as the text they came from.

The provisional version of this ADR (M0-16) set the bars this one is judged against: recall@10 of at
least 0.95 against brute force; p95 of at most 150 ms at the ceiling on emulated Lite (4 CPU); the
simplest implementation that meets them wins; and three overturn criteria (answered under
Decision).

## Options
**Measurement conditions (every number in this ADR).** Synthetic 384-dimensional unit vectors (the
dimension of Granite Embedding 97M), generated from a seed: *clustered* (Gaussian mixture, about 400
vectors per topic, queries are perturbed corpus points) and *uniform* (random on the sphere, which
has no neighbour structure and is the hardest case for quantisation). One search goroutine unless
stated, K = 10, 100 timed queries after one warm-up (p50 and p95 are over those), recall@10 over 50
queries against an independent float64 brute force. Every scenario runs in its own process, so the
RSS is that configuration's own, read from the OS (peak working set on Windows, `VmHWM` on Linux),
not from the Go runtime; it includes the roughly 10 MiB of the bare test process. The code is the
real `internal/store` with its default Lite pragmas (writer cache 8 MB, reader cache 2 MB x 4, mmap
off), `modernc.org/sqlite` v1.60.1 (SQLite 3.53.4, sqlite-vec v0.1.9), Go 1.27.0, `CGO_ENABLED=0`.
There are two environments, and **neither is certified**:

- **Dev box** — Intel i7-10700K, Windows 11, 64 GB RAM, NVMe, antivirus active (it inflates
  file-heavy paths) and shared with other build jobs, so its tails are noisy. No container limits,
  so not even emulated. Four complete runs: three on earlier commits and one on the final code, which differ
  only in the cache admission rule (it matters only when the cache is full), so the rows for two and
  three hot workspaces come from the final-code run alone, and the rows for the 250,000-vector
  deployment from the last two runs. A range is the minimum to the maximum over the runs. A fifth run
  was discarded because it overlapped with other jobs (deleting a 100,000-vector generation took 47 s
  against 1.0 to 1.8 s in the others).
- **Emulated Lite** — the same test binary cross-compiled for `linux/amd64` and run in a Docker
  container (the Docker Desktop WSL2 VM) limited to `--cpus=4 --memory=8g`, the Lite profile's
  8 GB / 4 CPU. Same CPU as the dev box, no antivirus, an ext4 file system. Two complete runs of the
  final code, same convention. A real Lite host is slower; the CPU limit constrains concurrent
  work, not a single searcher.

The harness is `internal/rag/vector/sqlitevec/bench_test.go`; reproduce with
`ALIXAI_VECTOR_BENCH=1 go test -run TestBenchMatrix -v -timeout 90m ./internal/rag/vector/sqlitevec/`.
Every row of every table, with ingest and delete figures, is in `docs/efficiency/M2-07.md`.

A. **Exact flat scan, own code, in the core SQLite store (chosen).** Vectors are scalar-quantised to
   int8 (per-vector scale, clamped) and stored as BLOBs in a `WITHOUT ROWID` table whose primary key
   starts `(application_id, tenant_id, workspace_id, generation_id)`. A search reads exactly that key
   range (the query plan is asserted by `TestScanReadsOnlyTheScopeRange`), scores each vector against
   the float32 query and keeps a top-k heap. It is exact over the authorised set; the only recall loss
   is quantisation. Two read paths: **streamed** from SQLite on every query, or from an optional
   **resident cache** (the int8 codes of hot generations in memory, a byte-budgeted LRU, valid only
   when its revision equals the generation's revision read in the same snapshot as the scan). The
   cache has admission control: a generation that needs room is loaded only if every entry it would
   evict has not served a search since that generation last asked, otherwise it is streamed, so the
   cache is never slower than no cache however many generations are hot
   (`TestResidentCacheNeverThrashesUnderRotation`).
B. **sqlite-vec through `modernc.org/sqlite/vec`** (MIT/Apache-2.0, bundled in the driver we already
   use, pre-v1). Built and tested as a complete second provider
   (`internal/rag/vector/sqlitevec`, schema and vec0 operations in `internal/store/vec`) that passes
   the same contract suite. The provisional text said a vec0 partition key is one column; vec0 in
   fact accepts **four**, so the scope and the generation are partition keys and a KNN query that
   constrains all four reads only that partition (the numbers below show cost tracking the
   partition, not the table). Brute force, int8, cosine or L2, one dimension per table definition.
C. **Pure-Go HNSW** (`github.com/coder/hnsw`). Rejected, not benchmarked: it does not compile for
   `windows/amd64` or `windows/arm64` (`renameio.TempFile` undefined, verified 2026-10-02), which fails
   R2, and its CC0 licence is outside the allowlist of [0008](0008-apache-2-licence.md). Per-scope
   filtering in a graph (post-filter loses recall, per-scope graphs multiply memory) and deletes
   (tombstones or rebuilds) are also unsolved there. Kept as the ANN option for corpora above the
   ceiling, behind the same interface, with a successor that builds on Windows.
D. **Upstream SQLite Vec1** (v0.7, July 2026, not 1.0): C only, no pure-Go port, its authors report
   memory-safety bugs. Watch, do not adopt.
E. **External vector database or pgvector.** Violates §5.1 for the minimum profile; stays a valid
   second provider for large deployments, which is why `vectortest.RunProviderSuite` exists.
F. **LocalAI Stores.** Puts vectors inside the runtime, outside AlixAI's scope predicates, backup and
   deletion guarantees, and couples retrieval to one runtime (rule 11). Rejected.

### Measured, option A against option B
"A streamed" is the flat scan with the cache off, "A resident" the same scan with the default 64 MiB
cache. B is sqlite-vec int8 with cosine distance; "B L2" is the variant that scales every vector by
one corpus-wide constant, because vec0 has no per-vector scale. RSS "steady" is after the timed
queries, "peak" the OS maximum. Latencies are milliseconds.

**Dev box (Windows, four runs, see the conditions above):**

| Scenario | Implementation | p50 / p95 ms | recall@10 | RSS steady / peak MiB | database MiB |
|---|---|---:|---:|---:|---:|
| clustered, 10k chunks searched | A streamed (cache off) | 14-23 / 30-41 | 0.978 | 35-44 / 36-44 | 4.1 |
|  | A resident (64 MiB cache, default) | 5.3-5.6 / 6.8-14.6 | 0.978 | 39-41 / 39-41 | 4.1 |
|  | B sqlite-vec int8 cosine | 19-23 / 25-33 | 0.976 | 36-37 / 38 | 4.0 |
| clustered, 25k chunks searched | A streamed (cache off) | 56-117 / 81-245 | 0.978 | 39-40 / 40-41 | 12.1 |
|  | A resident (64 MiB cache, default) | 8.5-13.7 / 10-23 | 0.978 | 46-47 / 53-55 | 12.1 |
|  | B sqlite-vec int8 cosine | 29-55 / 38-109 | 0.970 | 45-46 / 46-47 | 10.9 |
| clustered, 50k chunks searched | A streamed (cache off) | 124-157 / 158-196 | 0.990 | 40 / 40-41 | 28.0 |
|  | A resident (64 MiB cache, default) | 14-26 / 16-31 | 0.990 | 56-57 / 71 | 28.0 |
|  | B sqlite-vec int8 cosine | 53-101 / 63-122 | 0.980 | 49-50 / 50-51 | 21.3 |
| clustered, 100k chunks searched | A streamed (cache off) | 205-395 / 287-655 | 0.976 | 39-40 / 41-46 | 55.9 |
|  | A resident (64 MiB cache, default) | 31-55 / 37-83 | 0.976 | 76-77 / 106-107 | 55.9 |
|  | B sqlite-vec int8 cosine | 135-211 / 229-290 | 0.976 | 56 / 58-59 | 45.6 |
|  | B sqlite-vec int8 L2 | 78-135 / 92-188 | 0.922 | 51 / 51-52 | 37.3 |
| clustered, 10k chunks searched + 90k in nine other workspaces | A streamed (cache off) | 13-24 / 20-31 | 0.978 | 40-42 / 41-42 | 54.8 |
|  | A resident (64 MiB cache, default) | 3.3-5.4 / 4.4-7.5 | 0.978 | 44-45 / 44-45 | 54.8 |
|  | B sqlite-vec int8 cosine | 12-22 / 14-28 | 0.976 | 52-53 / 54-56 | 47.5 |
| clustered, 100k chunks searched + 150k in nine other workspaces | A streamed (cache off) | 269-278 / 334-347 | 0.976 | 40-41 / 42-44 | 136.5 |
|  | A resident (64 MiB cache, default) | 34-37 / 40-44 | 0.976 | 77 / 107 | 136.5 |
|  | B sqlite-vec int8 cosine | 157-161 / 199 | 0.976 | 74-75 / 74-75 | 122.7 |
| clustered, 2 x 100k chunks, 2 workspaces searched in rotation | A resident (64 MiB cache, default) | 182 / 240 | 0.972 | 120 / 120 | 111.7 |
|  | A resident (128 MiB cache) | 35 / 63 | 0.972 | 119 / 162 | 111.7 |
| clustered, 3 x 100k chunks, 3 workspaces searched in rotation | A resident (64 MiB cache, default) | 218 / 356 | 0.976 | 122 / 122 | 167.5 |
| clustered, 100k chunks searched, 4 concurrent searchers | A streamed (cache off) | 283-667 / 326-1096 | 0.985 | 52-54 / 54-63 | 55.9 |
|  | A resident (64 MiB cache, default) | 29-58 / 35-88 | 0.985 | 77-78 / 106-107 | 55.9 |
|  | B sqlite-vec int8 cosine | 122-283 / 145-431 | 0.975 | 69-70 / 70-71 | 45.6 |

**Emulated Lite (Linux container, 4 CPU, 8 GB, two runs):**

| Scenario | Implementation | p50 / p95 ms | recall@10 | RSS steady / peak MiB | database MiB |
|---|---|---:|---:|---:|---:|
| clustered, 10k chunks searched | A streamed (cache off) | 16-22 / 24-35 | 0.978 | 31 / 31 | 4.1 |
|  | A resident (64 MiB cache, default) | 3.4-4.2 / 4.2-6.3 | 0.978 | 35-36 / 35-36 | 4.1 |
|  | B sqlite-vec int8 cosine | 10.7-12.5 / 17.2-18.1 | 0.976 | 30 / 30 | 4.0 |
| clustered, 25k chunks searched | A streamed (cache off) | 43-45 / 51-57 | 0.978 | 35-36 / 36 | 12.1 |
|  | A resident (64 MiB cache, default) | 8.8-9.1 / 10.8-11.8 | 0.978 | 42-43 / 49-50 | 12.1 |
|  | B sqlite-vec int8 cosine | 25-29 / 29-44 | 0.970 | 38-39 / 39-40 | 10.9 |
| clustered, 50k chunks searched | A streamed (cache off) | 84-89 / 95-123 | 0.990 | 36 / 36 | 28.0 |
|  | A resident (64 MiB cache, default) | 16.7-16.8 / 19-22 | 0.990 | 52 / 66-67 | 28.0 |
|  | B sqlite-vec int8 cosine | 52 / 61-72 | 0.980 | 42-43 / 43 | 21.3 |
| clustered, 100k chunks searched | A streamed (cache off) | 171-231 / 205-317 | 0.976 | 35-36 / 36 | 55.9 |
|  | A resident (64 MiB cache, default) | 32-38 / 39-47 | 0.976 | 72 / 101 | 55.9 |
|  | B sqlite-vec int8 cosine | 106-110 / 142-180 | 0.976 | 48 / 49 | 45.6 |
|  | B sqlite-vec int8 L2 | 59-62 / 82-99 | 0.922 | 48 / 48 | 37.3 |
| clustered, 10k chunks searched + 90k in nine other workspaces | A streamed (cache off) | 16.3-19.3 / 22-27 | 0.978 | 36 / 36-40 | 54.8 |
|  | A resident (64 MiB cache, default) | 3.7-3.9 / 5.0-6.1 | 0.978 | 40 / 40 | 54.8 |
|  | B sqlite-vec int8 cosine | 11.6 / 15.0-19.1 | 0.976 | 46 / 46 | 47.5 |
| clustered, 100k chunks searched + 150k in nine other workspaces | A streamed (cache off) | 180-187 / 230-245 | 0.976 | 36 / 36-37 | 136.5 |
|  | A resident (64 MiB cache, default) | 34-49 / 38-62 | 0.976 | 72-73 / 102 | 136.5 |
|  | B sqlite-vec int8 cosine | 106-123 / 131-166 | 0.976 | 67 / 67 | 122.7 |
| clustered, 2 x 100k chunks, 2 workspaces searched in rotation | A resident (64 MiB cache, default) | 155-166 / 195-247 | 0.972 | 114-115 / 114-115 | 111.7 |
|  | A resident (128 MiB cache) | 33-42 / 43-69 | 0.972 | 111 / 152 | 111.7 |
| clustered, 3 x 100k chunks, 3 workspaces searched in rotation | A resident (64 MiB cache, default) | 166-169 / 212-222 | 0.976 | 114-115 / 114-115 | 167.5 |
| clustered, 100k chunks searched, 4 concurrent searchers | A streamed (cache off) | 254-305 / 297-701 | 0.985 | 47-48 / 48-53 | 55.9 |
|  | A resident (64 MiB cache, default) | 46-51 / 59-65 | 0.985 | 72-73 / 101-102 | 55.9 |
|  | B sqlite-vec int8 cosine | 131-141 / 153-155 | 0.975 | 61-62 / 61-62 | 45.6 |

Write side, at the 100,000-vector workspace:

| Implementation | ingest, vectors/s | delete one 100k generation, s | longest wait of another writer during that delete, ms | first query, ms |
|---|---:|---:|---:|---:|
| A streamed (cache off) | 19277-25614 | 1.0-1.8 | 90-215 | 240-353 |
| A resident (64 MiB cache, default) | 16753-38093 | 1.0-1.7 | 86-173 | 228-466 |
| B sqlite-vec int8 cosine | 6271-8957 | 16.9-23.6 | 16945-23569 | 185-255 |
| B sqlite-vec int8 L2 | 16693-26996 | 17.8-25.4 | 17763-25373 | 92-196 |

Emulated Lite:

| Implementation | ingest, vectors/s | delete one 100k generation, s | longest wait of another writer during that delete, ms | first query, ms |
|---|---:|---:|---:|---:|
| A streamed (cache off) | 34380-38810 | 0.6-0.9 | 36-55 | 167-177 |
| A resident (64 MiB cache, default) | 33387-38572 | 0.7 | 42-49 | 231-265 |
| B sqlite-vec int8 cosine | 6779-7896 | 15.7-15.8 | 15722-15774 | 108-123 |
| B sqlite-vec int8 L2 | 24352-27977 | 11.5-12.5 | 11549-12551 | 59-66 |

What the numbers say:

1. **Recall is not the discriminator; everything shipped clears the 0.95 bar.** The int8 flat scan
   scores 0.976 to 0.990 on clustered data from 10k to 100k vectors and, on uniform random vectors (no
   neighbour structure, the hardest case for quantisation), 0.990 at 10k and 0.980 at 100k. sqlite-vec
   cosine scores 0.970 to 0.984 (uniform 0.984 and 0.976). Both lose recall only to int8 quantisation;
   A is exact over the authorised set. B's L2 variant has no per-vector scale, so it uses one
   corpus-wide constant and drops to 0.922 on clustered data (uniform 0.958 and 0.922): under the bar,
   rejected. These are synthetic vectors; recall on a real Granite-embedded corpus is owed to M2-15.
2. **The scope prefilter is also a performance property.** Ten thousand vectors searched beside
   90,000 in nine other workspaces cost the same as ten thousand alone, for every implementation
   (emulated p50: A resident 3.7 to 3.9 ms against 3.4 to 4.2; A streamed 16 to 19 against 16 to 22;
   B 11.6 against 10.7 to 12.5), and a 100,000-vector workspace beside 150,000 more costs what it costs
   alone, within the noise between runs (A resident 34 to 49 ms against 32 to 38; A streamed 180 to 187
   against 171 to 231; B 106 to 123 against 106 to 110). Cost tracks the scope, never the deployment:
   for A it is one primary-key range, for B the four partition keys.
3. **Latency against the bar (p95 at most 150 ms).** On emulated Lite at 100,000 vectors: A resident
   39 to 47 ms (38 to 62 beside 150,000 more vectors, 59 to 65 with four concurrent searchers) passes;
   A streamed 205 to 317 ms (230 to 245 beside 150,000 more, 297 to 701 with four searchers) fails;
   B cosine 142 to 180 ms (131 to 166 beside 150,000 more, 153 to 155 with four searchers) is on the
   line and over it in most runs. The dev box orders them the same way with a wider spread (A resident
   p95 37 to 83 ms; A streamed 287 to 655; B 229 to 290). With the cache off, A streamed passes up to
   25,000 vectors (p95 51 to 57 ms emulated) and is at 95 to 123 ms at 50,000;
   B passes to 50,000 (61 to 72 ms). The resident scan is 4 to 7x faster than the streamed scan
   (p50 32 to 38 ms against 171 to 231 at 100,000 emulated; 6 to 7x on the dev box), because SQLite's
   row decoding dominates the streamed one, and about 3x faster than B (106 to 110 ms). B is 1.5 to 2x
   faster than A streamed from 25,000 vectors up.
4. **Memory.** A streamed holds no vectors: RSS is 31 to 36 MiB on the emulated host at every size
   (35 to 46 on the dev box), about 10 MiB of which is the bare test process, the rest SQLite's bounded
   page caches. The resident cache adds 37 MiB steady at 100,000 vectors (72 against 35 to 36) and
   peaks at 101 MiB while loading (a one-off transient). That is about 0.38 KiB per hot vector steady;
   the cache budgets 456 bytes per vector when it decides what fits (384 code bytes, a scale, a norm,
   and 64 bytes reserved for the chunk id). B's vectors stay in SQLite pages: 48 to 49 MiB at 100,000
   (56 on the dev box), 13 MiB more than A streamed and with no dial to trade RAM for speed. The
   database is 55.9 MiB per 100,000 vectors for A (586 bytes a vector with the chunk id, the 32-byte
   reuse key and the scale) and 45.6 MiB for B.
5. **Loading and writing.** Nothing is loaded at process start. The first search of a hot generation
   pays the load: 231 to 265 ms at 100,000 vectors emulated (228 to 466 on the dev box), 24 to 26 ms
   at 10,000. A write to a generation changes its revision, so the next search reloads it; for a
   generation being ingested into, streaming is as good as the cache. A stores 33,000 to 39,000
   vectors a second emulated (17,000 to 38,000 on the dev box) and B 6,800 to 7,900 (6,300 to 9,000),
   so 100,000 vectors take 3 s against 13 to 15 s; embedding them is slower than either and is not
   measured here (no model in a hermetic task). Deleting a generation of 100,000 vectors takes A 0.6 to
   0.9 s emulated (1.0 to 1.8 s on the dev box) in bounded batches, the longest wait of any other
   writer being 36 to 55 ms (86 to 215 ms). B takes 15.7 to 15.8 s (16.9 to 23.6 s), in **one
   transaction**, so every other writer waits for all of it. That is a property of the B provider, not
   of vec0: batching it as A's is would cap the stall, but not the total work, which is 10 to 25x A's.
6. **Concurrency.** The store's reader pool is four wide. With four concurrent searchers at 100,000
   vectors, A resident is 46 to 51 ms p50 and 59 to 65 p95 emulated, A streamed 254 to 305 and 297 to
   701, B 131 to 141 and 153 to 155. The resident scan scales with the readers; the streamed scan
   contends on SQLite and does not.
7. **Cache sizing and the thrash case.** Hot vectors cost about 0.45 KiB each, so the default 64 MiB
   holds 147,000 vectors (one 100,000-vector workspace) and 128 MiB holds two. With plain LRU, two hot
   100,000-vector workspaces under 64 MiB evicted each other and every search paid a load on top of
   the scan: p50 379 ms, p95 624 ms on the emulated host, twice the cost of no cache (one run, before
   admission control). A first admission rule that gave each entry a few credits still churned with
   three hot generations (`TestResidentCacheNeverThrashesUnderRotation` was written against it and
   fails on it). The shipped rule loads a generation that needs room only if every entry it would
   evict has not served a search since that generation last asked: the first workspace stays resident
   while it is used, the others stream. Two hot 100,000-vector workspaces under 64 MiB: p50 155 to 166
   ms, p95 195 to 247 (a mix of searches around 35 ms and around 180 ms); three: p50 166 to 169, p95
   212 to 222; both no worse than the cache off. At 128 MiB both of two are resident: 33 to 42 ms p50
   and 43 to 69 p95.

## Decision
**Adopt option A as the embedded `VectorIndexProvider`:** the exact int8 flat scan over scope-clustered
rows in the core store (`vector.Flat`, package `internal/rag/vector`), with an optional,
admission-controlled resident cache (`rag_vector.cache_mib`, 64 in the Lite default). Option B
(`sqlitevec`, glue in `internal/store/vec`) stays built and contract-tested behind the same interface
and is **not linked into the service by default**; choosing it is a composition-root change
(`vec.Register`, then `sqlitevec.New`). Option C is not built. Why A, against the requirements and
the bars:

- **R1.** A search reads one primary-key range `(application, tenant, workspace, generation)`; there
  is no global candidate set to post-filter (`TestScopePrefilterBeforeSimilarity`,
  `TestScanReadsOnlyTheScopeRange`, and the contract suite's `ScopePrefilterBeforeSimilarity`, which
  `TestContractSuiteHasTeeth` proves a post-filtering provider fails). B passes the same case through its
  partition keys, so R1 does not separate them.
- **R2.** Pure Go on every target for both; A adds no dependency, B adds the driver's transpiled C
  extension (about 0.3 MB of binary) and registers it for every connection of the process.
- **R3.** A needs no resident copy; the cache is the one memory dial and is sized per profile.
- **Bars.** Recall 0.976 to 0.990 against 0.95. p95 at the ceiling: A resident 38 to 65 ms against
  150 ms (single searcher, beside 150,000 more vectors, four searchers), a 2.3x margin at worst
  (emulated). The simplest implementation that meets the bar is A; the cache
  (`cache.go`) is a small addition that buys a 4 to 7x speed-up for about 0.4 KiB per hot vector.

**The provisional overturn criteria, applied.**

- *"Switch to B if it is faster at equal recall with the scope mapping proven."* The scope mapping is
  proven (four partition keys; the contract suite passes; cost tracks the partition) and recall is
  equal (0.976 each at 100,000). B **is** faster than A *streamed*, by 1.5 to 2x, and about 3x slower than
  A resident. The criterion is judged against A as shipped for Lite, which is the cached one, and does
  not fire there. It does fire for the cache-off configuration, and we still do not switch: B is on
  the line at 100,000 vectors and over it in most runs (p95 131 to 180 ms), so what it buys is a
  cache-off ceiling of 50,000 instead of 25,000, not 100,000; a cache sized to the hot set
  beats B by 3x at every size; and B costs 4 to 6x slower ingestion (2 to 6x on the noisy dev box), 10 to 25x slower
  generation deletes (one transaction holding the single writer for 16 to 24 s, fixable by batching but not
  down to the flat scan's total work), one fixed dimension per table definition, unstable ordering of
  equal scores (`Info.StableTies` false), a process-wide extension that rules out the driver's
  pluggable page cache, and a pre-v1 extension on the production path. If the numbers later favour B
  (a batched delete, a 1.0 release) the matrix is one command to re-run (re-evaluation trigger in
  `docs/efficiency/M2-07.md`).
- *"Switch to a pure-Go ANN only if A or B miss the latency bar below the ceiling."* A with the
  cache meets it at the ceiling, so no ANN is needed below 100,000 vectors per workspace. Above
  the ceiling is the Balanced profile's business (C's successor or E, behind this interface).
- *"Lower the ceiling if emulated Lite misses the bar."* It does without the cache, so the cache-off
  ceiling is lower (below).

**Supported Lite corpus ceiling (§25 item 2).** Counts are vectors, one per chunk of an active
generation.

| Setting | Active vectors per workspace | Active vectors per deployment | Evidence, emulated Lite p95 |
|---|---:|---:|---|
| Lite default, `cache_mib` 64 | **100,000** | **250,000** | 39 to 47 ms at 100,000; 38 to 62 beside 150,000 more; 59 to 65 with four searchers |
| cache off (where the experimental-4g profile starts) | **25,000** | **250,000**, each searched workspace within 25,000 | 51 to 57 ms at 25,000; 95 to 123 at 50,000; 205 to 317 at 100,000 |

- `Upsert` refuses with `resource_exhausted` at twice the active ceiling, 200,000 per workspace and
  500,000 per deployment (`rag_vector.max_vectors_per_workspace`, `max_vectors_per_deployment`),
  because the count covers every generation including the one being rebuilt beside the active one. It
  never drops silently. A profile that lowers the ceiling lowers these values.
- The bar holds for a workspace that is resident or within 25,000 vectors. The default cache covers
  147,000 hot vectors; a deployment that searches more than that regularly sets `cache_mib` at 0.45
  KiB per vector (250,000 vectors: about 109 MiB, so 128), or accepts streamed latency for the
  overflow (p95 205 to 317 ms at 100,000, over the bar).
- The margins are 2.3 to 3.9x at 100,000 resident and 2.6x at 25,000 streamed, to absorb a host slower
  than this CPU. A Windows host with antivirus active streamed 25,000 vectors at p95 81 to 245 ms,
  which is why the table is stated for the emulated Linux host; Windows hosts should run with the
  cache on.
- Above the ceiling the documented path is a Balanced profile with an ANN or external provider (C's
  successor, E), proved equivalent by `vectortest.RunProviderSuite`.

**Embedding reuse (SEC-023, R5): the reuse key is scope, model and the exact embedding input.** This
deliberately differs from the task wording ("chunk content hash plus embedding model id").
`chunk.Chunk.ContentHash` (M2-05) covers `Text` only and carries no scope. Used as the key it would
(a) miss a heading path or table header added to the embedding input, reusing a stale vector, and
(b) turn reuse into a cross-tenant existence oracle: text that another tenant already holds would be
embedded without an embedder call, sharing a row and an id, so timing, call counts and equal ids
would reveal it. The key is `vector.InputKey`: SHA-256 over a domain tag, `application_id`,
`tenant_id`, `workspace_id`, the embedding model and the exact text sent to the embedder
(`EmbeddingText`: heading path, table header, text), every field length-prefixed so no two tuples
collide. Reuse happens **inside the index** (`Reuse` copies a stored vector between generations; the
vector never leaves the provider), only within the caller's scope and model, and only under the
**same chunk id** with an equal key. Identical content in another application, tenant or workspace
therefore has an unrelated key, finds nothing, is embedded afresh, shares no row, and leaves the
embedder call count, `Stats` and ids exactly as if the first scope did not exist (`Stats` never
reports another scope's size). Tests: `TestSecInputKeyIncludesScopeModelAndInput`,
`TestSecEmbeddingReuseDoesNotCrossTenants`, `TestReuseIsAnchoredToTheChunkAndTheKey`, and
`ReuseNeverCrossesScopes` in the contract suite every provider must pass.
Trade-offs accepted: identical text under a different chunk id (chunk ids are content and structure
hashes, `chk_` and `chp_`, so a paragraph that moves changes its id) is embedded again, and a change
to the embedding-input format or the model re-embeds everything. Anchoring reuse to the chunk id also
avoids an index on the hash, which measured 4x slower deletes and a 25% larger database. A chunker
change creates a new index generation (`1.0.0+<config fingerprint>`), and the unchanged chunks of the
old generation are found by id.

**Stored format: int8 only, which answers the float16 question the provisional ADR left to this
task.** Each vector is quantised with its own scale and stored with its reciprocal norm; the query
stays float32. float32 would cost 4x the memory (154 MB per 100,000 vectors) and the M0-16 scratch
run found it within 15% of int8 speed, so it buys no latency; float16 originals beside the codes
would add 77 MB per 100,000 vectors to allow a different quantiser later, which a rebuild from
chunks already provides. A future provider that wants float vectors re-embeds, through queued class-5
jobs. Recall is 0.976 to 0.990.

## Consequences
- **Easier.** One store, one backup, one scope model: vectors live in the same SQLite file as the
  text they came from, under the same retention, backup and deletion rules. No new dependency, no
  new process, nothing resident unless a search asked for it. The index is derived data and
  rebuildable. A deleted generation, source or scope is hidden at once and removed in bounded
  batches that never hold the single writer for seconds, and an interrupted delete is finished by
  repeating the call or by `Flat.ResumeDrops`. A future ANN or external provider proves equivalence
  with `vectortest.RunProviderSuite`, which has teeth (`TestContractSuiteHasTeeth` plants a post-filter,
  a cross-scope reuse and a delete that deletes nothing, and requires the suite to catch each).
- **Harder.** The scan is linear, so there is a hard ceiling (table above) and an operator duty:
  size `cache_mib` at about 0.45 KiB per hot vector or live with the cache-off ceiling. A write to a
  generation invalidates its cache entry, so the next search of it reloads (0.2 to 0.5 s at 100,000
  vectors). int8 storage means a provider that wants float vectors re-embeds. Deleted rows leave
  free pages in the database file until they are reused or the file is vacuumed, and the store does
  not set `secure_delete`, so deleted vectors are gone from every query but not physically erased;
  a hard erasure guarantee for a deleted tenant belongs to the store and the retention work, not to
  this index. (vec0 zeroes a deleted vector in place: `TestDeletedVectorIsErasedFromTheShadowTable`.)
- **Forbidden.** Post-filtering a global top-k; a vector index that cannot enforce scope before
  similarity; any cgo dependency; sharing one index across tenants without the scope predicate; a
  reuse path that matches on content without scope and model; linking `internal/store/vec` into the
  default service build (`TestProductionCodeStaysBehindTheStore`); a vec0 or other shadow table
  exempted from the scope lint other than through `vec.ShadowTables()`.
- **Carried to other tasks, not decided here.**
  - Recall@10 on a real corpus embedded with Granite Embedding 97M, and the embedder's throughput
    (so the time to rebuild an index from chunks): M2-14 builds the harness and M2-15 measures it live
    under emulated Lite. The 0.95 bar is re-asserted there; if real vectors miss it, the first remedy
    is a higher-precision stored format for the affected profile, behind the same interface.
  - Certified (not emulated) numbers on real Lite hardware and the 4 GB profile's full-workload RSS:
    M5. Until then every figure here is a dev-box or emulated figure.
  - Startup time: nothing is loaded at start; the first-search load is measured above. The sqlite-vec
    page-cache caveat of the provisional ADR is closed: the benchmark and the contract suite run the
    sqlite-vec provider on the real `store.Open` with its default pragmas (the store sets its cache
    through pragmas, not a registered page cache), and nothing conflicts.

## Reversal plan
All callers use `VectorIndexProvider` (`Upsert`, `Reuse`, `Search`, the four deletes, `Stats`).
Switching implementation is a configuration change plus a rebuild: vectors are derived data,
regenerated from stored chunks through queued embedding jobs, with unchanged chunks reusing
embeddings when the provider supports `Reuse`. To use the sqlite-vec candidate the composition root
calls `vec.Register` on the migration registry, opens the store and calls `sqlitevec.New`; nothing
else changes, and `vectortest.RunProviderSuite` proves the swap. Going back is the same in reverse:
the vec0 table is dropped with its module's tables and the flat tables are rebuilt from chunks. An
external provider (E) or a pure-Go ANN (C) is added by implementing the interface and passing the same
suite.

**Files: content-addressed storage (SEC-023, G7, M3-06).** The attachment store follows the same rule. A blob's key is
the SHA-256 over the length-prefixed application, tenant, workspace and content hash, in a per-tenant directory named by a hash of
application and tenant; no scope id and no plain content hash is ever a path. Identical content is deduplicated only inside one
workspace, so an identical upload in another tenant changes nothing observable (path, id, size or timing) for the first. The unit
test is `TestSecContentAddressIsPerTenant` in `internal/files`; the cross-tenant system test stays with M5-08.

## Sources (checked 2026-10-02)
- Driver and `vec` package: <https://pkg.go.dev/modernc.org/sqlite>, <https://gorse.io/posts/sqlite-vec>
- sqlite-vec: <https://github.com/asg017/sqlite-vec>; Vec1 status: <https://sqlite.org/forum/info/1d9852d4ca71e25e67aff87c0deaec9f894df046aaaa67c1b596520177bf2c5d>
- coder/hnsw: <https://github.com/coder/hnsw>
- Embedding model card: <https://huggingface.co/ibm-granite/granite-embedding-97m-multilingual-r2>
