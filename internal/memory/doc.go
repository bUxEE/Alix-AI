// Package memory is long-term memory per principal × tenant × workspace (ADR 0042).
//
// Items are curated facts the assistant may recall across sessions. Writes need
// consent: the host may create an active item on the user's explicit request
// (POST /v1/memory); the assistant may only propose (status proposed) through a
// schema-validated structured output, and only POST /v1/memory/{id}/confirm
// activates it — typed chat text never confirms. Every query is scoped; a foreign
// principal, tenant or workspace sees the same not-found as an absent id. Every
// write is audited. Active items enter context assembly fenced as untrusted data,
// counted in the budget and egress-guarded by their data class.
package memory
