# llmbench

Replay recorded AlixAI-shaped turns (system prompt + tool schemas + RAG excerpts,
~3–6k input tokens, ~300 output) against any OpenAI-compatible endpoint.
Reports time-to-first-token, full-answer p50/p95, tokens/s per user and aggregate,
and the max concurrency whose full-answer p95 stays under a target.

## One-command run

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

## Corpus

Synthetic turns in [`testdata/corpus`](testdata/corpus): English and Italian, with tools and long RAG
excerpts. Not production data.

## Tests

```bash
go test ./tools/llmbench
```

Uses an in-process fake OpenAI-compatible streaming endpoint. **Do not** point CI at real GPUs.
