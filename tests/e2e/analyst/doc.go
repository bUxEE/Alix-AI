// Package analyst is the serve-level exit gate for ADR 0047 analyst turns (release 1.5).
//
// It composes the public API the way `alixai serve` / wireV1 does (same constructors,
// scripted fake controller, fake host application with company read capabilities, fake
// SearXNG-shaped search), with dialogue.analyst enabled. Defaults-off (analyst disabled)
// keeps the 1.4 single-capability path.
//
// Scenarios covered:
//  1. out-of-domain ("Persian cat") → typed refusal, no read/search/answer model essay
//  2. eligibility question → read + search + fetch + criteria table + not-legal-advice
//  3. revenue forecast → read + compute + chart/provenance, estimate labelled
//  4. invented number → text_replaced / template; computed number survives
//  5. old client (no announcement) events validate against schemas/_frozen/v1; 1.5 client gets new stages/blocks
//  6. abilities: deployment/application/tenant narrowing; non-analyst_capable; GET /v1/me/analyst
//  7. tenant isolation of reads/datasets
//  8. injection in host record / web page → no write, no foreign read, no out-of-domain answer
//  9. budget/cancel/caps → settled defined outcomes
//
// 10. analyst off → existing 1.4 serve behaviour unchanged
package analyst
