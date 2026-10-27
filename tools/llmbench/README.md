# llmbench

Replay recorded AlixAI-shaped turns against any OpenAI-compatible endpoint.

Two modes:

| Mode | Command | What it measures |
| --- | --- | --- |
| **Speed** (default) | `run` | TTFT, full-answer p50/p95, tokens/s, max concurrency under a latency target |
| **Quality** (Italian) | `quality` or `run -mode quality` | Schema validity, verifier pass/replace, fact/number accuracy, Italian language stay; optional LLM-as-judge |

## Speed run

With Performance vLLM listening on loopback (see `deploy/container/performance`):

```bash
go run ./tools/llmbench run \
  -endpoint http://127.0.0.1:8000/v1 \
  -key "$VLLM_API_KEY" \
  -model gpt-oss-20b \
  -out /tmp/llmbench-out \
  -target 8s
```

Writes `/tmp/llmbench-out/report.json` and `report.md`. Concurrencies: 1, 4, 16, 64.

Optional: `-cache-salt <secret>` sends vLLM `cache_salt` on every request (shared-tenant isolation).

Speed corpus: [`testdata/corpus`](testdata/corpus) (EN+IT, tools + long RAG). Not production data.

## Italian quality run

Evaluates meeting minutes / governance / company-data style prompts with **automatic** scores only
(no human labels; no invented latency numbers):

- JSON schema validity for TaskProposal / AnalystStepProposal (model-authority projection from `schemas/`)
- AlixAI `internal/verify` pass vs template-replace on realised answers (`kind=verify`)
- Fact/number accuracy against reference must-contain / must-numbers
- Italian language check (stop-word ratio; fail on English-dominant switch)
- Optional LLM-as-judge via `-judge-endpoint` (**off by default**)

```bash
go run ./tools/llmbench quality \
  -endpoint http://127.0.0.1:8000/v1 \
  -key "$VLLM_API_KEY" \
  -model gpt-oss-20b \
  -out /tmp/llmbench-quality

# equivalent:
go run ./tools/llmbench run -mode quality \
  -endpoint http://127.0.0.1:8000/v1 \
  -key "$VLLM_API_KEY" \
  -out /tmp/llmbench-quality
```

Corpus: [`testdata/quality/it`](testdata/quality/it) with reference answers. File `report.json` + `report.md` like the speed report.

Italian quality figures in ops docs stay **not yet measured** until a report from this mode is filed
(see `docs/operations/hardware-and-pricing.md`).

## Tests

```bash
go test ./tools/llmbench
```

Uses an in-process fake OpenAI-compatible endpoint. **Do not** point CI at real GPUs.
