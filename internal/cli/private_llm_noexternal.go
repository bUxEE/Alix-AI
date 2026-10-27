//go:build noexternal

package cli

// Private LLM wiring lives in private_llm_wire.go (!noexternal). The local-only
// binary must not import modes/providerreg through this package.
