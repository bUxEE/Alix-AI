// Command llmbench replays recorded AlixAI-shaped chat turns against any
// OpenAI-compatible endpoint.
//
// Speed mode reports time-to-first-token, full-answer latency, and tokens/s at
// concurrency 1/4/16/64:
//
//	go run ./tools/llmbench run -endpoint URL -key SECRET [-model ID] [-out DIR]
//
// Quality mode evaluates an Italian corpus (minutes / governance / company data)
// with automatic scores (schema, verifier, facts, language); optional judge:
//
//	go run ./tools/llmbench quality -endpoint URL -key SECRET [-out DIR]
//	go run ./tools/llmbench run -mode quality -endpoint URL -key SECRET
//
// Do not run against production GPUs from CI; use the fake endpoint in tests
// or a lab host. Every capacity figure in docs/operations/hardware-and-pricing.md
// stays "estimate — run tools/llmbench to measure" until a speed report is filed.
// Italian quality stays "not yet measured" until a quality report is filed.
package main
