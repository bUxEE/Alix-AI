// Command llmbench replays recorded AlixAI-shaped chat turns against any
// OpenAI-compatible endpoint and reports time-to-first-token, full-answer
// latency, and tokens/s at concurrency 1/4/16/64.
//
//	go run ./tools/llmbench run -endpoint URL -key SECRET [-model ID] [-out DIR]
//
// Do not run against production GPUs from CI; use the fake endpoint in tests
// or a lab host. Every capacity figure in docs/operations/hardware-and-pricing.md
// stays "estimate — run tools/llmbench to measure" until a report from this tool
// is filed.
package main
