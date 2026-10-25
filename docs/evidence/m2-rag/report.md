# M2-15: live RAG measurement with LocalAI embeddings (emulated Lite)

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Task M2-15. Written 2026-10-03. This is the live counterpart of M2-14 (`tools/rageval`, `docs/efficiency/M2-14.md`).

**Every figure is emulated.** One LocalAI v4.10.0 container capped by cgroup (Lite: 8 GiB, 4 CPUs, no swap, checked with `docker inspect`; section 3 and 10 also use the Balanced cap, 16 GiB and 8 CPUs) on an i7-10700K
dev box (8 cores, 16 threads, 64 GB, Windows 11, Docker Desktop, no GPU). It is not a certified machine (7.4), the host was shared with other work, and a
latency of a few milliseconds is indicative. Ranges are over repeated runs. Every number traces to a result file under `benchmarks/results/m2/` or `docs/evidence/m2-rag/` (the RSS figures to the CSVs in `rss/`, summed per two-second sample),
except the few hand measurements listed at the end of section 1. All result files pass `go run ./tools/rageval check`, and those under `benchmarks/results/m2` pass `go run ./tools/bench check`.

## 1. What ran

| | |
|---|---|
| Runtime | LocalAI v4.10.0, `docker.io/localai/localai:v4.10.0@sha256:0632c21d...`, `llama-cpp` backend, started with `compose.yaml` + `compose.lite.yaml` + the M0-13 spike override (`tests/live/spike/localai/compose.spike.yaml`, which adds the embedding model definition under its registry id). Nothing was downloaded: the files were already in `deploy/container/localai/data`. |
| Caps (docker inspect) | memory 8,589,934,592 B, memswap equal (no swap), 4.0 CPUs, `LOCALAI_MAX_ACTIVE_BACKENDS=2`, `LOCALAI_THREADS=4`. `docker stats` reported a limit of 8192 MiB in every result, which `bench check` compares with the profile. |
| Model | `granite-embedding-97m-multilingual-r2-q4_k_m`, Q4_K_M, 105,467,232 B. sha256 `355f1f30...965daf` on the host **and inside the container**, equal to the registry pin. 384 dimensions. |
| Corpus | `tests/performance/rag` (M2-14): 28 documents, 103 chunks, 50 questions (35 to answer, 15 to decline; 33 English, 17 Italian), unchanged. |
| Retrieval | The pipeline's own defaults (8 items, 3,000 estimated tokens, RRF k 60) unless a section says `--rag-caps lite` or `balanced` (the profile's package, section 3), reranking off unless stated, one reader. |
| Generator | The extractive, label-blind one of M2-14 (see 2, gap 4). |
| Tool | `rageval run --embedder localai ...` built from this branch while it was written: each result records the commit it ran at and `git.dirty: true`; the later edits added options and checks (the quantisation check, `--rag-caps`, tests), not a measured path. |

How the result files are labelled (read this before comparing them):

- `benchmarks/results/m2/hybrid`, `min-score-*`, `live-scale-*`, `quantization-live`, `lite-caps/hybrid`, `lite-caps/min-score-*`, `balanced/hybrid`, `balanced/min-score-*`, `balanced/live-scale-1k`: `provider: localai`, `synthetic: false`.
- `benchmarks/results/m2/fts`, `lite-caps/fts`, `balanced/fts`: `--embedder localai --mode fts` contacts no runtime, so the tool records it as `provider: fake`, `synthetic: true` (nothing real
  took part). Its retrieval and answer figures are real; they are the **no-model** figures.
- `benchmarks/results/m2/fake-hybrid` and `cache-experiment/*`: the fake embedder (hashed bag of words), `synthetic: true`. They measure the index, not a model.
- **Upgraded files.** The tool grew three descriptive keys while the runs went on (`quantization`, `config.rag_caps`, `config.query.max_parent_lookups` / `max_parent_toks`). The 52 results written before
  they existed were brought to the final format by `docs/evidence/m2-rag/upgrade-results.mjs`, which adds exactly the value that held (no quantisation check, the pipeline's own caps, the query defaults of 3 parent
  lookups of at most 768 tokens); no measured value, no companion `.json` and no digest was touched, and `rageval check` re-derives every metric from the raw records.
- `docs/evidence/m2-rag/rerank-synthetic-lexical`: Granite embeddings plus the harness's **lexical reranker** (a word-overlap score, no model). It is labelled
  `synthetic: true` in the `.rag` (and `components.reranker.synthetic`); its companion `.json` says `provider: localai` because the bench schema ties
  `synthetic` to the fake provider. It is kept out of `benchmarks/results/m2` for that reason.

**Hand measurements, not in a result file** (read once from a command, not part of any `.rag`):

| Value | Command |
|---|---|
| Cold start, first embedding after the container started: **8.696 s** on the Lite cap, **8.745 s** on the Balanced cap | `curl -w %{time_total}` on the first `POST /v1/embeddings` after `stack.sh up lite` / `up balanced` |
| Container ready after about 27 to 30 s (Lite) and 36 s (Balanced) | `/readyz` polled every 3 s |
| Caps: Lite memory 8,589,934,592 B, memswap 8,589,934,592 B, 4,000,000,000 nano-CPUs; Balanced 17,179,869,184 B, 17,179,869,184 B, 8,000,000,000; image pinned by digest | `docker inspect alixai-localai --format ...` |
| sha256 of the model file `355f1f30...965daf`, on the host and inside the container, equal to the registry pin | `sha256sum` and `docker exec alixai-localai sha256sum /models/...` |
| CPU of the container during the 30,000-chunk build: 350.17%, 350.32%, 351.69% of a 400% cap (276.9, 278.4 and 279.9 MiB) | `docker stats --no-stream --format '{{.CPUPerc}} {{.MemUsage}}'`, three readings |

## 2. The five gaps of M2-14

| # | Gap | Outcome |
|---|---|---|
| 1 | No container memory | **Closed.** `--docker-container` (with `--docker-bin`, `--docker-interval`) samples `docker stats --no-stream` for the whole run, with the fixed argument vector and the name and CLI checks of `tools/bench`, into `container_memory` (the bench envelope's block, also copied into the companion). A container that is not under the profile's cap makes the run fail instead of writing a lite result. It is the cgroup figure (usage minus inactive cache), not VmRSS, so the resident set of the two LocalAI processes was sampled as well with the M0-13 sampler (`rss/labelled-runs.csv`, section 5). On this machine a bare `docker` is not found by Go's `exec`; pass `--docker-bin "C:\Program Files\Docker\Docker\resources\bin\docker.exe"`. |
| 2 | `--embedder localai --mode rerank` refused | **Reported, and allowed only as synthetic.** The registry holds no reranker (one qualified embedding model, four rejected controllers), the LocalAI adapter's five-path allowlist has no rerank route and was not widened. `--mode rerank` with localai now runs the harness's lexical reranker and the result says so (`components.reranker.synthetic`, top-level `synthetic`, the summary line, and `-reject-synthetic` refuses it). No compact reranker was measured. |
| 3 | The sweep never measured a model | **Closed.** Every labelled question now records the pipeline's timing per stage (`latency.stages_ms`; `embed` is the query-embedding latency), and the scale sweep embeds with the run's embedder (`scale.embedder` says which; with localai the default is one 1,000-chunk scope, because a model costs about 40 ms a chunk). Sections 4 and 8. |
| 4 | No LocalAI answer generator | **Not built, not measured.** No controller is qualified: all four candidates are `rejected` in the registry after M0-13 and M0-17 (`docs/efficiency/M0-17.md`). The answer-stage figures below therefore measure retrieval plus the claim check, as in M2-14. |
| 5 | First live use of `--embedder localai` | **Done.** One throwaway embedding first loads the model and teaches the run the dimension (384, now recorded in `components.embedder.dimensions`) and the name the runtime answers under. LocalAI reported `granite-embedding-97m-multilingual-r2-q4_k_m`: **equal to the registry id** (the spike's `models.yaml` names the model by it). It echoes the LocalAI model name, not the file name, so it does not identify the weights; the sha256 above does. |

Also added: `--quant-check` with `--quant-docs` (section 9), `--scale-reader-cache-kib` and `--scale-pin-scopes` (section 8), and `--rag-caps` (the evidence and candidate caps of a profile, section 3, because the
pipeline's defaults are not Lite's package). Result format 2 (`rag-result.schema.json` v2); no result of format 1 was ever committed. 20 new test functions (mock LocalAI on loopback, scripted memory source,
negative controls) and 5 new cases in the tamper table of `rageval check`.

## 3. Quality: FTS-only against hybrid against hybrid + rerank

35 answerable and 15 to-decline questions; one question is 2.9 points, so these are mechanism checks on a small corpus, not a quality claim. Identical in the
three hybrid runs (retrieval is deterministic).

| | FTS-only | **Hybrid**, Granite 97M (LocalAI) | Hybrid, fake embedder (same session) | Hybrid + lexical rerank (synthetic) |
|---|---|---|---|---|
| recall@1 / @3 / @5 / @8 | 0.900 / 0.900 / 0.900 / 0.900 | **0.900 / 0.943 / 0.943 / 0.971** | 0.871 / 0.900 / 0.929 / 0.929 | 0.829 / 0.886 / 0.929 / 0.971 |
| MRR, nDCG@5 | 0.914, 0.903 | **0.933, 0.930** | 0.906, 0.904 | 0.869, 0.879 |
| cross-language recall@1 / @5 / @8 (5 questions) | 0.3 / 0.3 / 0.3 | **0.3 / 0.6 / 0.8** | 0.3 / 0.5 / 0.5 | 0.2 / 0.5 / 0.8 |
| answers correct, answerable answered | 45/50, 31/35 | 45/50, 31/35 | 45/50, 31/35 | 45/50, 31/35 |
| unsupported answers, leaks, errors | 1/15, 0, 0 | 1/15, 0, 0 | 1/15, 0, 0 | 1/15, 0, 0 |
| Italian: correct, recall@5, MRR (17) | 15/17, 0.833, 0.833 | 15/17, 0.917, 0.887 | 15/17, 0.917, 0.850 | 15/17, 0.917, 0.872 |
| evidence package, mean estimated tokens | 247 | 365 | 446 | 398 |

- The real embedder helps where the model says it should: Italian and cross-language questions (recall@5 of the cross-language kind 0.3 to 0.6, recall@8 0.8; Italian
  MRR 0.833 to 0.887). It beats the fake embedder at every k (0.943 against 0.900 at @3), as it should. The gain is 1.5 questions' worth of recall on this corpus.
- **The answer figures do not move**, in any mode: the same four answerable questions are declined (`en-sem-01`, `en-xlang-01`, `it-xlang-01`, `it-xlang-02`) and
  the same one unanswerable question is answered (`en-none-01`). With the floor at 0.85 (section 7) the cross-language evidence is in the package for all five
  questions (recall@5 1.0) and the extractive generator, which picks sentences that share words with the question, still cannot use it. So retrieval is no
  longer what stops those four questions; the generator is, and a real controller is what would show whether the retrieval gain becomes answers (not measured, gap 4).
- **Lexical rerank is rejected**: it lowers recall@1 (0.900 to 0.829), MRR (0.933 to 0.869) and nDCG, as it did on the fake embedder in M2-14. It reorders by shared words and
  pushes labelled evidence down. It says nothing about a reranker model.
- Hybrid costs evidence tokens: 365 against 247, because vector candidates fill the package to its 8 items (4.5 items in FTS-only, 8.0 in hybrid). For the controllers
  M0-13 measured, about 3 s of every answer was the time to first token of a 330-token prompt on 4 CPUs, so evidence tokens are paid on every answer; section 7 shows a
  floor that removes most of them.

### The same comparison with each profile's package

The runs above use the pipeline's own package (8 items, 3,000 estimated tokens), which is the `performance` column of `docs/operations/resource-budgets.md` 6.3, not Lite's.
`--rag-caps` applies a profile's row of that table (items, evidence tokens, candidates a leg, parent expansion) and stops recall@k at the items the package holds. Lite: 4 items,
1,000 est. tokens, 24 candidates (`benchmarks/results/m2/lite-caps`); Balanced: 6 items, 2,000, 48, measured on the Balanced container (`benchmarks/results/m2/balanced`).
Latency is end to end p50 / p95 in ms, one value per run (three runs of hybrid).

| | FTS-only | Hybrid, floor 0 | Hybrid, floor 0.80 | Hybrid, floor 0.85 |
|---|---|---|---|---|
| **Lite** recall@1 / @3 / @4 | 0.900 / 0.900 / 0.900 | 0.900 / 0.943 / 0.943 | 0.900 / 0.943 / 0.943 | 0.900 / 1.000 / 1.000 |
| Lite MRR, nDCG@4 | 0.914, 0.903 | 0.929, 0.930 | 0.929, 0.930 | 0.952, 0.962 |
| Lite cross-language recall@4, Italian recall@4 | 0.3, 0.833 | 0.6, 0.917 | 0.6, 0.917 | 1.0, 1.000 |
| Lite evidence tokens (mean), items (mean) | 141, 2.80 | 166, 4.00 | 148, 3.34 | 147, 2.92 |
| Lite latency | 1.6 / 2.5 | 49.8 / 59.2; 47.2 / 54.3; 48.5 / 56.1 | 47.8 / 53.6 | 49.2 / 58.7 |
| Lite query embedding | none | 45.6 / 55.4; 43.1 / 50.6; 44.6 / 51.7 | 44.9 / 50.5 | 46.4 / 55.9 |
| **Balanced** recall@1 / @3 / @5 / @6 | 0.900 / 0.900 / 0.900 / 0.900 | 0.900 / 0.943 / 0.943 / 0.943 | 0.900 / 0.943 / 0.971 / 0.971 | 0.900 / 1.000 / 1.000 / 1.000 |
| Balanced MRR | 0.914 | 0.929 | 0.934 | 0.952 |
| Balanced evidence tokens, items | 205, 3.64 | 268, 6.00 | 227, 4.50 | 207, 3.78 |
| Balanced latency | 1.9 / 2.8 | 49.7 / 75.2; 48.7 / 55.3; 46.1 / 51.1 | 45.5 / 50.9 | 44.3 / 49.1 |
| Balanced query embedding | none | 44.5 / 70.2; 44.3 / 51.2; 42.5 / 47.5 | 42.8 / 48.3 | 42.1 / 47.7 |

Correct answers (45/50), declines and leaks are the same in every cell, as before. With Lite's four items the hybrid gain is smaller in MRR (0.914 to 0.929, against 0.933
with eight items) and the same in recall@3: the package, not the retrieval, is the limit. The floor matters less in a package of four items (11% fewer evidence tokens at 0.8,
against 22% in eight) but at 0.85 hybrid fits the package of FTS-only (147 against 141 tokens) with the best recall. Eight CPUs buy 5 to 10% on the embedding (42 to 44.5 ms
against 43 to 45.6): one request is not parallel work. The 75.2 / 70.2 ms p95 of the first Balanced run is a single slow query in that run.

## 4. Latency

End to end per labelled question (retrieval and the extractive answer stage), p50 / p95 in ms, ranges over three runs of the whole labelled set.

| | FTS-only | Hybrid, Granite (LocalAI, Lite cap) | Hybrid, fake (same session) |
|---|---|---|---|
| end to end | 1.9 to 2.0 / 3.0 to 3.1 | **49.4 to 49.7 / 54.0 to 60.5** | 4.1 / 5.2 (one run) |
| retrieval alone | 1.7 to 1.8 / 2.6 to 2.8 | 49.2 to 49.5 / 53.7 to 60.2 | 3.7 / 4.9 |
| **query embedding** (`stages_ms.embed`) | none | **45.0 to 45.3 / 49.6 to 55.0** (mean 45.5 to 46.7) | microseconds |
| answer stage (extractive) | 0.16 to 0.19 | 0.26 to 0.28 | 0.31 |

The model adds a constant of about 45 ms per query on four CPUs; everything else in the pipeline is what M2-14 measured with the fake. The pipeline's own stage
timings come from the Windows clock, which ticks about every 0.5 ms: stage figures under a millisecond are quantised.

Cold start (first request after the container started, backend start + model load + one embedding): **8.7 s** on the Lite cap and on the Balanced cap alike (M0-13: 7.3 s; the container reported ready after about 30 to 36 s). The 15-minute idle watchdog unloads
the model, so a Lite service that idles pays it again on the next question.

## 5. Memory

LocalAI, embedding model resident (the controller was not loaded). Two figures, because `docker stats` (cgroup usage minus inactive file cache, what `bench check` and
`container_memory` carry) is smaller than the resident set (what the budget table counts); both were taken, the second with the M0-13 sampler every two seconds
(`rss/labelled-runs.csv` for the Lite container, `rss/balanced-runs.csv` for the Balanced one; the unix times in them can be matched to `timestamp` and `duration_ms` of each result).

| Measure | Lite container (8 GiB, 4 CPUs) | Balanced container (16 GiB, 8 CPUs) |
|---|---|---|
| `docker stats`, short inputs (labelled corpus, sweeps, the 30,000-chunk build: 239 samples) | baseline 252 to 273 MiB, steady 272 to 279 MiB, **peak 283 MiB** | 264 to 268 MiB |
| `docker stats`, after the quantisation check embedded long chunks (the Lite-caps runs) | 367 to 368 MiB | n.m. |
| Resident set, both LocalAI processes, short inputs (the 30,000-chunk build: 347 samples) | min 418, median 445, **peak 454 MiB** (anonymous 221 to 251) | min 367 (just started), median 443, peak 452 MiB |
| of which the LocalAI service process (Go, no model) | 124 to 160 MiB, median 150 | 149 to 161, median 151 |
| of which the llama-cpp backend with the embedding model | **291 to 294 MiB** (anonymous 175 to 179, the mapped weights 116 file-backed) | 291 to 292 |
| Resident set after embedding long Markdown chunks (hundreds of tokens each, the quantisation check) | **peak 538 MiB**: the backend rose to **380 MiB** (anonymous 264), the service stayed at 156 | n.m. |
| `rageval` itself, labelled evaluation | 28 to 33 MiB (FTS-only 28 to 29, hybrid 32 to 33); the 30,000-chunk query phase 56 MiB | 33 MiB |

Against `docs/operations/resource-budgets.md` 4.1 (Lite: service 150, embedder backend at most 450): the backend fits with room (294 MiB with short texts, **380** with
long ones; M0-13 had 390 for a short session), the service peaks at 160, **10 MiB over its 150 row**. Memory grows with the length of what is embedded, so the long-input figure is the one to budget with.
The container never came near its cap, and `docker stats` reported the cap as 8192 MiB (16384 for Balanced), which `bench check` matches against the profile.
During the 30,000-chunk build the container used **350% of its 400% CPU cap** (three `docker stats` readings, 350 to 352%): the model saturates the four CPUs, so an ingestion in progress and an
interactive answer compete for them (R8: background work yields).

## 6. Ingestion and index size

Chunk + index + embed + validate + activate; the sandboxed parser and the job queue are not in it.

| | FTS-only | Hybrid, Granite (LocalAI) |
|---|---|---|
| labelled corpus, 103 chunks | 1,078 to 1,132 chunks/s (91 to 96 ms) | **30 to 32 chunks/s** (3.2 to 3.5 s; the embedding is 3.1 to 3.4 s of it); Balanced cap 39.4 |
| 1,000 chunks of the sweep's text, one scope | n.m. | **24.0 chunks/s** (41.7 s; 41.6 s of it embedding, 77 ms indexing); Balanced cap 29.4 chunks/s (34.0 s) |
| 30,000 chunks of the sweep's text, 30 scopes | fake 4,153 to 4,599 chunks/s (6.5 to 7.2 s) | **25.7 chunks/s** (1,168.6 s) |
| index, labelled corpus | 348,160 B (3,380 B/chunk) | 417,792 B (4,056 B/chunk), 100 vectors |
| index, 1,000-chunk scope | n.m. | 2,281,472 B used (2,281 B/chunk), 3.8 MB on disk with the log |

The model is the build: **32 to 42 ms a chunk on four CPUs** (32 ms for the labelled corpus's short chunks, 42 ms for the sweep's 30-word chunks, which are more
tokens), against 0.2 ms with the fake. Rebuilding an index from its chunks (ADR 0011) therefore costs about 42 s per 1,000 chunks, 17 minutes for the 25,000 vectors of
the experimental-4g ceiling and 2.3 hours for the 200,000-vector default ceiling (`vector.DefaultMaxVectorsPerWorkspace`), computed from the 1,000-chunk build, one
request at a time. The effect of larger batches was not measured.

## 7. `vector_min_score`, swept on the real embedder

Granite's cosine scores sit high: a floor up to 0.6 changes nothing on this corpus (every metric and the size of every package identical to floor 0) and the first item
is dropped at 0.7. One run per floor (retrieval is deterministic); latency is the same within noise (49 to 53 ms end to end).

| floor | recall@1 / @3 / @5 / @8 | MRR | nDCG@5 | correct | evidence tokens (mean) | items |
|---|---|---|---|---|---|---|
| FTS-only | 0.900 / 0.900 / 0.900 / 0.900 | 0.914 | 0.903 | 45/50 | 247 | 4.50 |
| 0 to 0.6 | 0.900 / 0.943 / 0.943 / 0.971 | 0.933 | 0.930 | 45/50 | 365 | 8.00 |
| 0.7 | 0.900 / 0.943 / 0.943 / 0.971 | 0.933 | 0.930 | 45/50 | 364 | 7.98 |
| 0.75 | 0.900 / 0.943 / 0.943 / 0.971 | 0.933 | 0.930 | 45/50 | 352 | 7.64 |
| **0.8** | 0.900 / 0.943 / 0.971 / 0.971 | 0.934 | 0.941 | 45/50 | **285** | 5.60 |
| 0.85 | 0.900 / 1.000 / 1.000 / 1.000 | 0.952 | 0.962 | 45/50 | 260 | 4.66 |
| 0.9 | 0.900 / 0.971 / 0.971 / 0.971 | 0.943 | 0.950 | 45/50 | 249 | 4.52 |

What it says: between 0.8 and 0.9 the floor **removes 22 to 32% of the evidence tokens without losing a point of any retrieval or answer metric**, and moves recall@5 up by
one cross-language question at 0.8 to 0.85 (not monotonic: 0.9 is lower than 0.85, which is what noise on one question looks like). At 0.9 the package is the size of
FTS-only's (249 against 247 tokens) with a higher recall. The default of the pipeline **stays 0**: 35 questions cannot fix a number, a floor is a threshold on one model's raw cosine (a Q8_0 file or
another model scores differently) and a larger corpus has more near-duplicates above it. **Candidate: 0.80 for Granite Embedding 97M Q4_K_M**, to be adopted as a property of
the embedding model (the registry entry) once a labelled corpus of a few hundred questions and a real controller confirm it.

## 8. Scale with a model, and the corpus-size effect on a small scope

M2-14 measured, with the fake embedder, that the same 1,000-chunk scope has p50 6.1 ms alone, 15.9 ms in a 30,000-chunk corpus and 17.4 ms in a 100,000-chunk corpus
(ADR 0032 open question), and put the 2 MB reader cache forward as the hypothesis. The sweep now embeds with the model when the run has one, and has two knobs
(`--scale-reader-cache-kib`, `--scale-pin-scopes`) to test it.

### 8.1 Does the real embedder change it? No: it adds a constant

Live, Lite cap, Granite via LocalAI; fake runs of the same session beside them. Same synthetic text (30 pseudo-words a chunk, the worst case for BM25), p50 / p95 in ms; stage
columns are means. Live rows are one run each (60 and 100 questions); fake rows are the range of three runs (100 questions).

| 1,000-chunk scope | p50 / p95, fake | p50 / p95, **live** | query embedding (mean), live | full-text stage, fake / live | vector stage, fake / live |
|---|---|---|---|---|---|
| alone (1,000 chunks) | 6.2 to 6.4 / 7.9 to 9.3 | **50.7 / 57.2** | **47.4** | 3.9 to 4.4 / 4.3 | 0.4 to 0.5 / 0.68 |
| in a 30,000-chunk corpus, questions rotating over 30 scopes | 10.1 to 10.7 / 14.1 to 15.7 | **54.3 / 68.4** | **50.4** | 7.6 to 8.3 / 9.7 | 0.9 to 1.2 / 1.29 |
| growth, alone to 30,000 | +3.7 to +4.5 ms | **+3.6 ms** | +3.0 | +3.2 to +4.4 / +5.4 | +0.4 to +0.8 / +0.6 |

The model adds the query embedding (47 to 50 ms, close to flat across corpus sizes) and nothing else: the growth of the small scope's cost with the corpus is the same 3.6 to 4.5 ms with the
real embedder as with the fake. In the 30,000-chunk corpus the whole labelled evaluation also ran, and the corpus-wide row (100 questions over the 30 scopes) is 52.5 / 64.2 ms. The
live full-text stage is about 1.5 ms above the fake's in the 30k corpus (9.7 against 7.6 to 8.3) for a reason this run does not explain (one run; the host had just been feeding the model).
**Computed, not measured:** at 20,000 and 50,000 chunks the M2-14 hybrid p50 (89 to 119 and 241 to 257 ms with the fake) plus the 47 to 50 ms embedding give about 136 to 169 and 288 to 307 ms;
the live 20k and 50k builds (about 13 and 32 minutes of embedding each) were not run.

Building the 30,000 chunks with the model took **1,168.6 s (19.5 minutes), 25.7 chunks/s**, 38.9 ms a chunk (embedding 1,163.1 s, indexing 3.6 s, finalising 1.7 s), 46.9 MiB peak for the building
process; with the fake it takes 6.5 to 7.2 s. The index is 61,726,720 B (2,058 B a chunk), **byte for byte the size the fake's index has**: the stored form does not depend on the embedder.
The container held 252 to 283 MiB (`docker stats`, 239 samples) throughout.

### 8.2 Is it the 2 MB reader cache? About a quarter of it, no more

Fake embedder (the model is not under test, and 8.1 shows it is a constant), the same scope of 1,000 chunks, three runs per arm (two for 100,000 chunks), 100 questions per run, or 1,200 for
the two "long" arms (so that each of the four pooled reader connections visits each scope many times and a larger cache has time to warm). Ranges over runs; stage columns are means.

| Arm | Corpus | Questions asked of | Reader cache | p50 | p95 | Full-text stage | Vector stage | Peak RSS |
|---|---|---|---|---|---|---|---|---|
| alone | 1,000 chunks | the only scope | 2 MB | 6.2 to 6.4 | 7.9 to 9.3 | 3.9 to 4.4 | 0.4 to 0.5 | 34.8 to 35.8 MiB |
| pinned | 5,000, 5 scopes | one scope | 2 MB | 6.6 to 7.0 | 8.9 to 9.3 | 4.5 to 4.7 | 0.4 to 0.5 | 35.5 to 35.8 MiB |
| pinned | 10,000, 10 scopes | one scope | 2 MB | 7.1 to 7.8 | 9.2 to 10.6 | 4.7 to 5.3 | 0.6 | 36.2 to 37.2 MiB |
| pinned | 30,000, 30 scopes | one scope | 2 MB | 8.6 to 9.7 | 12.0 to 16.1 | 6.4 to 7.3 | 0.5 to 0.6 | 38.5 to 39.2 MiB |
| pinned | 30,000, 30 scopes | one scope | **64 MiB** | 8.4 to 10.0 | 12.2 to 15.3 | 6.3 to 7.5 | 0.4 to 0.5 | 45.3 to 45.5 MiB |
| rotating | 30,000, 30 scopes | 30 scopes | 2 MB | 10.1 to 10.7 | 14.1 to 15.7 | 7.6 to 8.3 | 0.9 to 1.2 | 70.6 to 73.6 MiB |
| rotating | 30,000, 30 scopes | 30 scopes | **64 MiB** | 9.8 to 10.4 | 14.0 to 14.5 | 7.4 to 7.8 | 1.1 to 1.3 | 171.1 to 181.0 MiB |
| rotating, long | 30,000, 30 scopes | 30 scopes | 2 MB | **10.8 to 11.0** | 16.3 to 16.9 | 8.1 to 8.2 | 0.6 | 80.5 to 81.8 MiB |
| rotating, long | 30,000, 30 scopes | 30 scopes | **64 MiB** | **9.5 to 9.9** | 14.3 to 15.5 | 6.9 to 7.2 | 0.6 | 222.5 to 222.9 MiB |
| pinned | 100,000, 100 scopes | one scope | 2 MB | 12.5 to 13.3 | 23.2 to 23.5 | 11.5 to 11.6 | 0.4 to 0.5 | 41.1 to 42.1 MiB |
| rotating | 100,000, 100 scopes | 100 scopes | 2 MB | 14.2 to 14.9 | 23.6 to 25.7 | 12.8 to 13.4 | 1.4 | 84.4 to 89.3 MiB |

Reading it. This session's host was quieter than M2-14's: the same rotating 30k arm is 10 to 11 ms here against 15.9 there, so the gap to the lone scope is about 4.6 ms here, not 9.8.

- **A larger cache is worth about 1.2 ms in the rotating arm, once warm** (10.8 to 11.0 down to 9.5 to 9.9; full-text stage 8.1 to 8.2 down to 6.9 to 7.2), at a cost of about **140 MiB** of resident
  memory (80 to 223). With too few questions to warm it (100) it is worth nothing (10.1 to 10.7 against 9.8 to 10.4): a first version of this experiment, with 100 questions, would have rejected the
  hypothesis for the wrong reason.
- **Asking one scope instead of rotating is worth 1.4 to 1.8 ms** (10.1 to 10.9 down to 8.6 to 9.7): the vector stage halves (0.9 to 1.2 down to 0.5; the first visit of each scope loads its vector
  generation, and that cost is spread over the samples of a short run) and the full-text stage loses about 1 ms. In the long arms the vector stage is 0.6 for both caches, confirming it was a first-visit cost.
- **The rest does not depend on the cache.** With the working set pinned to a single scope, 2 MB and 64 MiB give the same p50 (8.6 to 9.7 against 8.4 to 10.0), and the full-text stage of the same
  scope grows with the corpus around it: **3.9 to 4.4 ms alone, 4.5 to 4.7 at 5,000 chunks, 4.7 to 5.3 at 10,000, 6.4 to 7.3 at 30,000, 11.5 at 100,000**, roughly 0.08 ms for every 1,000 chunks of
  other scopes. That is consistent with the full-text index being shared by the whole corpus and not with the page cache; it was not profiled further.

Verdict: the hypothesis is **partly supported and small**: about 1.2 of the 4.6 ms with a 64 MiB cache, 1.4 to 1.8 with a pinned working set, and about 2.8 ms remain with both. It does not justify raising the
Lite reader cache (4 readers x 64 MiB for 1.2 ms); ADR 0032's sentence "a query costs what its own scope costs, not the corpus" needs "plus about 0.08 ms per 1,000 chunks of other scopes in the full-text
stage". The 100,000-chunk point here is 100 scopes of 1,000 chunks, not M2-14's shape (32 scopes with a 20k and a 50k scope); its 14.2 to 14.9 ms against M2-14's 17.4 ms is the same load difference as above.

## 9. Quantisation on real vectors (ADR 0011's 0.95 bar)

`--quant-check --quant-docs docs/decisions`: for each question, the share of the ten nearest vectors by exact float32 cosine that the index's own int8 form (`vector.Quantise`, `UnitQuery`, `Score`,
without the index) also returns, with Granite's vectors. Run on the Lite container, one run (`benchmarks/results/m2/quantization-live`).

| Vector set | Vectors, questions | Mean recall@10 | Worst question | Questions perfect | Mean / max score error | Bar 0.95 |
|---|---|---|---|---|---|---|
| labelled corpus (English and Italian, short chunks) against its 50 questions | 103, 50 | **0.978** | 0.9 | 39 of 50 | 0.00087 / 0.0037 | met |
| the 35 ADRs of `docs/decisions` (English prose and tables, long chunks) against 83 section headings | 807, 83 | **0.972** | 0.8 | 62 of 83 | 0.00085 / 0.0044 | met |
| the sweep's pseudo-word text, the hard case (the vectors of nonsense text sit close together) | 1,000, 100 | **0.922** | 0.7 | 37 of 100 | 0.00059 / 0.0033 | **missed** |

On natural language the int8 form keeps ADR 0011's bar (0.978 at 103 vectors, 0.972 at 807) with a score error under 0.005. On gibberish, whose neighbours are separated by less than the
quantisation error, it does not (0.922). The ADR's remedy for real vectors that miss the bar ("a higher-precision stored format for the affected profile, behind the same interface") is **not triggered by
natural language here**, but 807 vectors are not 25,000 or 200,000, where a larger share of the corpus sits within 0.005 of the tenth neighbour: the check should be repeated on a real corpus of at least
10,000 chunks (none exists in the repository). The negative control (`TestQuantCompareCanFail`) shows the check can fail.

## 10. Recommendation for the defaults

The runs support **hybrid retrieval with the registry's Granite Embedding 97M as the default for Lite and for Balanced, with FTS-only as the fallback when the embedder is not resident**, and say what it costs.

**Lite (8 GiB, 4 CPUs; package of 4 items and 1,000 estimated tokens)**

1. Hybrid by default, `vector_min_score` 0 in code (as now), FTS-only as the degraded mode (`no_embedder`): what the resource budget (R2, 6.3) already says. Measured basis: no leak and no error in any mode; at
   Lite's package recall@3 0.900 to 0.943, MRR 0.914 to 0.929, cross-language recall 0.3 to 0.6, Italian recall 0.833 to 0.917; the answer stage does not move (extractive generator).
2. The price to budget: **+45 ms per query** (p95 50 to 56), a **resident 440 to 454 MiB** (backend 294, service 150), up to **540 MiB with long chunks** (backend 380), **8.7 s** to the first answer after
   the model unloads (so the embedder stays resident, as R3 already requires for Lite), and **24 to 32 chunks/s** to ingest (about 42 s per 1,000 chunks, 19.5 minutes per 30,000: embeddings belong on
   the bounded job queue, rule 13, and a model at 350% of a 400% CPU cap competes with answers). The service process peaks 10 MiB over its 150 MiB budget row; adjust the row.
3. Candidate to carry in the **registry entry** of this model, not in code: `vector_min_score` 0.80 (evidence tokens down 11% at Lite's package and 22% at eight items, no metric lower on this corpus). 0.85 is
   better here and is one question; adopt a value only after a labelled corpus of a few hundred questions and a real controller confirm it.
4. Reranker **off**: none is qualified and none could be served (no adapter, no registry entry); the harness's lexical reranker lowers recall@1 by 0.07 and stays a harness tool.
5. Reader cache stays at 2 MB (a 64 MiB one buys 1.2 ms for 140 MiB of resident memory); the per-workspace corpus ceilings of M2-09 are what bound the p95 (the full-text stage grows with the corpus).

**Balanced (16 GiB, 8 CPUs; package of 6 items and 2,000 estimated tokens)**: the same defaults on the same model. Measured on the Balanced cap: embedding 42 to 44.5 ms (5 to 10% under Lite's), ingestion 29.4 to 39.4
chunks/s (+22 to +31%), resident set 443 / 452 MiB. A larger embedding model ("qualified larger replacement", 8.2) and a compact reranker are the only candidates that could change this and neither was evaluated; do not
enable a reranker by default until one passes, with an adapter, on this corpus.

## 11. Measurements for ADR 0011 and ADR 0032

- **ADR 0011, recall on a real corpus embedded with Granite and the 0.95 bar:** recall@10 of the int8 form against exact is 0.978 (103 vectors) and 0.972 (807 vectors) on natural language, 0.922 on 1,000
  pseudo-word vectors; the bar is met on natural language and its remedy is not triggered; repeat at 10,000+ chunks (section 9).
- **ADR 0011, embedder throughput and the time to rebuild an index from its chunks:** 24.0 to 25.7 chunks/s at Lite (1,000 and 30,000 chunks of 30 words; 30 to 32 for the labelled corpus's short chunks), 29.4 to 39.4
  at Balanced: about 42 s per 1,000 chunks, 19.5 minutes per 30,000, and (computed) 17 minutes for 25,000 and 2.3 hours for 200,000 vectors, one request at a time. The 30,000-chunk index is the same 61,726,720 B
  with the model and with the fake: the stored form does not depend on the embedder.
- **ADR 0011, retrieval quality:** hybrid over FTS-only on the labelled corpus is +0.04 recall@3 to +0.07 recall@8 and +0.02 MRR; larger on Italian and cross-language questions; none on answers (extractive generator).
- **ADR 0032, the reader-cache hypothesis:** partly supported, small: about 1.2 ms of 4.6 with a 64 MiB cache and 1.4 to 1.8 ms with a pinned working set; the remainder is the full-text stage growing with
  the corpus, about 0.08 ms per 1,000 chunks of other scopes (section 8.2). The real embedder does not change it (section 8.1).

## 12. Not measured, and limits

- **A controller-generated answer**: no qualified controller, so citation precision, unsupported-answer rate and fact accuracy are those of the extractive generator in every mode; the retrieval gain is not shown to become answers.
- **A reranker model**: none available. **A larger embedding model, Q8_0 against Q4_K_M** (M0-13: no gain): not run.
- **20,000 and 50,000-chunk scopes with the model** (computed from the fake plus the embedding constant), **more than one request at a time**, an embedder under concurrent load or beside the controller (M0-15), the
  experimental-4g cap, CPU or power beyond the one `docker stats` reading.
- **Quality is a mechanism check**: 35 answerable questions, 15 to decline, 5 cross-language; one question is 2.9 points. Every sweep over a floor or a cache is on one corpus.
- **The Windows clock** quantises the pipeline's own stage timings to about 0.5 ms; the harness's end-to-end figures use the performance counter.
- **Shared host**: other processes ran on the machine; ranges are over repeated runs, a few milliseconds are not a difference.
- **Emulated, never certified**: a cgroup cap on a larger machine, Docker Desktop on Windows.

## 13. Reproducing

```
# from the worktree root, with the stack up (stack.sh up lite); the key is in the environment, never on a command line
export ALIXAI_LOCALAI_KEY=...
rageval run --embedder localai --mode hybrid --corpus tests/performance/rag --out <dir> \
  --docker-container alixai-localai --docker-bin "C:\Program Files\Docker\Docker\resources\bin\docker.exe" --scale-sizes none
rageval run ... --vector-min-score 0.8            # the floor sweep
rageval run ... --rag-caps lite --profile lite    # Lite's package (Balanced: --rag-caps balanced --profile balanced, on the Balanced container)
rageval run ... --scale-sizes 1000 --scale-small-scopes 29   # the live 30,000-chunk sweep, about 20 minutes
rageval run ... --quant-check --quant-docs docs/decisions
rageval run --embedder fake --scale-sizes 1000 --scale-small-scopes 29 --scale-reader-cache-kib 65536 --scale-pin-scopes 1 ...   # the cache arms
go run ./tools/rageval check benchmarks/results/m2 docs/evidence/m2-rag
go run ./tools/bench check benchmarks/results/m2
```
