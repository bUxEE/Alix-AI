# M0-17 report - controller re-measurement with a grammar-constrained gateway and bench-prompt-v1

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Everything here is **emulated** (one LocalAI v4.10.0 container capped by cgroup on an i7-10700K host under Docker Desktop, no GPU) and indicative, never a certified machine.
The protocol, the pass bar and the verdict rule were committed before the first run (`protocol.md`, commit 4ad3442). Every number traces to `results.json` (made by
`scripts/summarize.mjs` from `benchmarks/results/m0b/`); the per-model discussion, the no-model floor, repeatability and resource figures are in `docs/efficiency/M0-17.md`.

## Result: no candidate passes the Lite bar, again, and the bar was not lowered

The question was whether the two defects of the M0-13 experiment (the gateway's `json_schema` was silently ignored by LocalAI v4.10.0, and `bench-prompt-v0` defined only one intent) explained
the failure. They did not: grammar plus prompt v1 moved some lines a lot and no verdict.

| Candidate | Schema-valid (bar 80) | Routing (bar 49/54) | Refusals (bar 16) | Injection (bar 16) | Verdict |
| --- | --- | --- | --- | --- | --- |
| no-model keyword router (floor) | n/a | 33 | 12 | 14 | not a pass |
| Granite 4.0 350M | 17 | 6 | 0 | 1 | rejected |
| Qwen3.5-0.8B | 76 to 77 | 51 to 52 | 0 | 13 | rejected |
| Granite 4.0 1B | 75 | 48 | 12 | 13 | rejected |
| Qwen3.5-2B (balanced profile) | 80 | 47 | 9 | 15 | rejected |

A model passes only if both runs (A and B) clear every line, with no provider errors and peak container memory at most 5.8 GiB. All four stay `rejected` in `models/registry/` with the M0-17 data,
`default_for` is empty, there is no model fallback, and **Lite is not certified**. "Rejected" means rejected on `bench-prompt-v1` with this grammar and this sampler configuration, not for all prompts.

Pattern: every failing model fails on **refusals**, not routing (Qwen3.5-0.8B routes 51 to 52 of 54 and refuses 0 of 16). That points at the next lever: a deterministic out-of-scope gate before the model.

## What changed in the product (stands regardless of the verdict)
- `bench-prompt-v1` (`--prompt`, default for new runs; the v0 hash still equals the recorded one).
- The gateway constrains the controller request with a GBNF grammar (`proposeBuildChat`, digest pinned equal to the bench builder) and validates after decoding (`proposeDecode`): any non-stop finish,
  including an answer cut at the token cap, is a typed `schema_invalid` at stage `truncated`, never a partial proposal.
- `scripts/` (run matrix, summarize, apply-registry) so the registry statuses follow from the rule only.

## Findings
1. LocalAI applies `presence_penalty 1.5` to the Qwen family and draws a random seed at each model load; both are recorded in `protocol.md` and `run-manifest.jsonl`.
2. Repeatability is the opposite of M0-15: A against B differ in 0 of 80 outputs for both Granite models and in 14 of 80 (Qwen3.5-0.8B) and 18 of 80 (Qwen3.5-2B); no line of the bar flipped, so no verdict depends on it.
   The cause is not found (hypotheses only: prompt-prefix cache reuse for Granite, the presence penalty for Qwen).
3. The first Qwen3.5-0.8B session lost its container after case 54 of run A (26 provider errors, the container vanished while the agent was paused). It is kept as `A-aborted` and was re-measured in a new cold session, as the protocol requires for provider errors that are not the model's.

## Deviations and gaps
- `routes_dev.go` still calls `buildChat` (json_schema) and reads `res.JSON`, and `spike_test.go` asserts json_schema: the dev route is not on the grammar yet (follow-up, below).
- Larger models were not tried: a larger model must first earn its memory against the 5.8 GiB line, and the two cheapest levers (grammar, intent definitions) are now exhausted for these candidates.
- CPU and power were not measured; latency is indicative because other agents' builds shared the host.
- This file was written by the orchestrator: the worker's harness refuses to create a file named `report.md`, so it did not route around that guard.

## Decision for the owner
Lite has no certified controller. Options, none of which lowers the bar: (a) add a deterministic out-of-scope gate before the model and re-measure the same four candidates (cheapest, targets the failing line);
(b) measure a larger model on the Balanced profile and keep Lite on deterministic routing plus typed forms; (c) route Lite to an external provider through milestone X (opt-in, disabled by default).
