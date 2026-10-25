# Efficiency review — <task id>: <feature>

Every feature records this before it counts as done.

## Requirement
Exact behaviour, quality and safety requirement this feature must meet.

## No-model / deterministic option
Can code, a schema, a grammar, a lookup, a cache or an already-resident model do it? What was
tried, and what did it achieve? If this option is chosen, say so and skip to Decision.

## Candidates evaluated
Smallest first. For each: name/version, size, quantization, backend, peak RSS, latency
(p50/p95, first token where relevant), quality score on the fixed corpus, licence.

| Candidate | Size | Peak RSS | p50 / p95 | Quality | Licence | Verdict |
|---|---|---|---|---|---|---|

## Decision
The smallest candidate that passes, the larger ones rejected and why.

## Resource impact
CPU, RAM (steady and peak), storage, latency, concurrency, power where measurable. Mark numbers
from resource-limited containers as **emulated**.

## 4 GB profile impact
Preserves / improves / makes harder the 4 GB target (§7, rule 17), and why.

## Re-evaluation trigger
Date or event (new model release, provider change, new language) that reopens this review.
