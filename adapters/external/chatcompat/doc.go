// Package chatcompat implements a shared OpenAI-compatible Chat Completions HTTP
// client for third-party hosts (OpenRouter, xAI, Moonshot, hosted Llama, …).
// Provider-specific packages set the provider id, default origin and profile;
// this package holds the wire format, egress route guard and error mapping.
//
// It is disabled by default and absent from a binary built with -tags noexternal
// (ADR-0022); only internal/cli/providers_external.go builds it into serve.
package chatcompat
