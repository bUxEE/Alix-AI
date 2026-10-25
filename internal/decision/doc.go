// Package decision is the provider-neutral heart of the optional decision layer
// (master plan 8.5, docs/architecture/decision-layer.md section 4, ADR 0029).
//
// It GUARDS a scorer; it is not one. A scorer proposes: it scores a bounded set of
// candidates that deterministic code already authorised, and the result is ADVISORY.
// Nothing in this package authorises, validates, prepares, confirms, commits, sends,
// deletes, discloses or changes anything, lowers risk or decides who is authorised.
//
// The flow of Engine.Score, every step fail-closed with a typed fallback reason:
//
//	scope and request binding        -> tenant_mismatch
//	request shape                    -> ood_unsupported_shape
//	candidate guard (empty, oversized, duplicate, stale, cross-tenant, unauthorised)
//	                                 -> candidate_set_rejected (+ a RejectCode)
//	deadline, cache lookup, audit "invoked"
//	provider call (synchronous, bounded by the request deadline and Config.Timeout)
//	acceptance gates                 -> malformed_output, tenant_mismatch, model_mismatch,
//	                                    ood_unsupported_shape, calibration_failed, low_confidence
//	audit "accepted" or "fallback"
//
// The outcome is an Advice. A selection becomes usable only through Engine.Resolve, which
// re-resolves every selected candidate id through the deterministic CandidateResolver
// (the adapter, wired by dialogue integration in D-04); a refusal is policy_conflict.
//
// The package takes the clock, id generator, resolver and audit appender from its
// caller and starts no goroutines. A provider must honour its context; the engine
// bounds the call with a context deadline and also checks the measured latency.
package decision
