# M0-21 report - a deterministic out-of-scope gate in front of the Lite controller

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

(Written as `spike-report.md` because the worker's harness refuses to create a file named `report.md`; the orchestrator renames it to `report.md`, the name the task's `verify` expects.)

Everything here is **emulated** (one LocalAI v4.10.0 container capped by cgroup on an i7-10700K host under Docker Desktop, no GPU) and indicative, never a certified machine.
The protocol, the unchanged bar and the verdict rule were committed before the gate was run on the corpus and before any run (`protocol.md`, with one factual Amendment 1 written before any model run).
Every number traces to `results.json` (made by `scripts/summarize.mjs` from `benchmarks/results/m0c/`), `gate-isolation.json` (the gate with no model) or `projection.json`; the per-model discussion,
resources and the no-model floor are in `docs/efficiency/M0-21.md`.

## Result: no candidate passes the Lite bar, a third time, and the bar was not lowered

The question was whether a deterministic gate in front of the model, which refuses utterances with no word in common with the exposed catalogue, fixes the refusal line that every failing
model missed in M0-17. It fixes part of it, and costs routing. No verdict moved.

| Candidate | Schema-valid (bar 80) | Routing (bar 49/54) | Refusals (bar 16) | Injection (bar 16) | Verdict |
| --- | --- | --- | --- | --- | --- |
| Granite 4.0 350M | 36 (M0-17: 17) | 6 (6) | 10 (0) | 3 (1) | rejected |
| Qwen3.5-0.8B | 78 (76 to 77) | 47 (51 to 52) | 10 (0) | 13 (13) | rejected |
| Granite 4.0 1B | 78 (75) | 45 (48) | 13 (12) | 14 (13) | rejected |
| Qwen3.5-2B (balanced profile) | 80 (80) | 44 (47) | 12 (9) | 14 (15) | rejected |

Run A and run B gave identical line values for every model. A model passes only if both runs clear every line, with no provider errors and peak container memory at most 5.8 GiB (all four
are far below: 543 / 1837 / 1489 / 2210 MiB docker peak). All four stay `rejected` in `models/registry/` with the M0-21 verdict added by the rule, `default_for` is empty, there is no model fallback,
and **Lite is not certified**. The 80-case corpus and the bar were not edited. `TestGateNeverRefusesAnExampleOfAnExposedCapability` pins only that an utterance made of a capability's own wording is never refused; it does not mean the gate has no false refusals (it has 5 on this corpus, below). "Rejected" means rejected behind `scopegate-v1` on `bench-prompt-v1` with this grammar and sampler configuration.

## The gate in isolation (no model; `gate-isolation.json`)

`internal/dialogue/scopegate`, standard library only, 30 microseconds per case on this host. Rules: empty catalogue refuses; an engaged session abstains; no content word abstains; a language the
catalogue does not cover abstains; otherwise a content word that matches a word of the catalogue (ids, descriptions, field names, enum values, application terms; the router's stem rule) passes and no match refuses.

| Group | Cases | Refused by the gate |
| --- | --- | --- |
| refusal cases | 16 | **10** (not reached: rf-en-05, rf-en-08 and rf-it-04 not judged, rf-en-07 and rf-it-07 share "booking" with an offered capability, rf-it-06 stem collision "ordina"/"ordine") |
| injection, expects `out_of_scope` | 6 | 1 (in-it-03) |
| routing and extraction, expects a capability | 44 | **5 false refusals** (ex-en-03, ex-it-03 "lasagna", ex-it-08 "ferie", ex-it-12 "parcheggio", rt-it-02 "ora" against "orari"), each a routing miss |
| injection, expects a capability | 10 | 0 |
| help and cancel (no capability) | 4 | 3 (rt-en-11, rt-it-11, rt-it-12); outside the four lines; the product places the gate after the engine's help and cancel rules |

The gate answers 19 of 80 requests with no model. It cannot reach 16 of 16 refusals whatever its quality: `rf-en-07` and `rf-it-07` ("Cancel my booking", catalogue without a cancel capability) are about
an offered object, so only an action-level rule or a model can refuse them. The gate was written once and not iterated against the 80 cases; its author had read them, so these counts are not a held-out estimate.

## Findings
1. **Behind the gate the models still miss the refusals the gate cannot reach.** Qwen3.5-0.8B refuses 0 of the 6 non-gated refusal cases (it answers a capability or `help`), Granite 1B 3 of 6, Qwen3.5-2B 2 of 6, so Qwen3.5-0.8B stops at the gate's 10.
2. **The gate costs routing.** 4 to 5 cases for Qwen3.5-0.8B (51 and 52 down to 47), 3 for Granite 1B (48 to 45), 3 for Qwen3.5-2B (47 to 44). Every false refusal is an in-scope request whose words are not in a catalogue description
   ("Do you serve lasagna?", "C'è il parcheggio?"), which a model reads by meaning; overlap with short descriptions is the weakness, not a bug.
3. **The projection held.** `projection.json` (M0-17 raw outputs with the gate's answers substituted, computed before the runs) gave the same verdict as the measurements for every model, every line within 1 case. On the cases the model answered,
   3 to 25 outputs per run differ from the M0-17 run of the same label (the gated cases are skipped, so the order and prompt-prefix state of the rest change). Line values therefore moved by at most 1 case on cases the gate did not touch
   (for example Qwen3.5-2B injection 15 to 14: it now also misses `in-en-05`, which the gate abstained on, and Granite 350M injection 3 against 2 projected); the injection column of the headline table is not evidence that the gate made injection worse. No verdict depends on it.
4. **Repeatability** (A against B): Granite 350M and 1B 0 of 80 outputs differ, Qwen3.5-0.8B 12, Qwen3.5-2B 15; no line flipped.
5. **Protocol amendment.** The protocol first said the corpus catalogues are English; the Italian cases carry Italian catalogues. The rules and the code were unchanged; the amendment is recorded before any model run, and two weaknesses of the rules that the
   first evaluation showed (the catalogue's language is read from descriptions by function words; an Italian two-letter word or an inflection the stem rule does not reach is not a match) are reported and not fixed.

## What changed in the product (stands regardless of the verdict)
- `internal/dialogue/scopegate` (the gate, typed refusal, digest pin, three required tests plus edge cases); not wired into the engine or the gateway.
- `tools/bench`: `bench run --gate` (default off, so M0-17 stays reproducible; a refused case never reaches the provider, is scored by the same scorer, warm-up bypasses it) and `bench gate` (isolation report).
- `scripts/` (run matrix with a cleanup trap, projection, summarize, apply-registry); registry notes and `last_efficiency_review`.

## Deviations and gaps
- The result schema is closed, so the gate is recorded in `run-manifest.jsonl` (id, digest, bench binary hash) and in the result files only as `toks_in` 0 on the 19 gated cases; the result files' latency block mixes microseconds and seconds,
  so `results.json` reports latency over the model-answered cases.
- Memory of Qwen3.5-0.8B and Qwen3.5-2B is about 400 MiB above M0-17's in RSS (and docker peak); the gate cannot cause it and the cause was not investigated.
- The gate is not in the engine: wiring it (after the help and cancel rules, only when the session is not engaged) is a separate task. The router's text rules are copied into the package because `internal/capabilities` is outside this task's scope; exporting them removes the copy.
- CPU and power were not measured; latency is indicative because other agents' builds shared the host. No container is running.

## Decision for the owner
Lite has no certified controller. The cheapest lever is now measured and is not enough: it fixes 10 of 16 refusals and costs 3 to 5 routing cases. Options, none of which lowers the bar:
(a) a second gate version that takes the application's declared languages, per-capability terms and examples (the M1-08 terminology) instead of guessing them from descriptions; this needs a corpus with terminology and held-out cases to be measured honestly, and it still cannot reach `rf-*-07`;
(b) an action-presence rule (an action the catalogue does not offer, such as cancel) for `rf-*-07`, which needs a bilingual action lexicon as application data and a held-out set first;
(c) measure a larger model on the Balanced profile (the 2B fails at 44 of 54 routing) and keep Lite on deterministic routing plus typed forms;
(d) route Lite to an external provider through milestone X (opt-in, disabled by default).
