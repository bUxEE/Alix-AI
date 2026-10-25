// Package dataegress is the data-egress policy engine of External Provider Mode
// (additive plan section 6): the deterministic gate in front of every request to
// an external model provider. It decides what DATA may leave the deployment.
// internal/egress decides which network destinations may be dialled; the two are
// separate gates and are never merged. No model takes part in any decision here,
// and nothing written in a candidate's text (by a user, a document or a model) can
// change an outcome: decisions are made on metadata and policy data alone.
//
// External mode is off by default. A level with no policy is disabled, a RAG
// source with no policy is local_only, and the global kill switch denies every
// request of a deployment before any policy is read.
//
// # Policy store
//
// One policy per level (providers.EgressPolicy, schemas/providers/egress-policy)
// at deployment, application, tenant, workspace and source level, in module
// "dataegress" of the store. Every row carries the deployment, application, tenant
// and workspace of its key and every statement names them all; keys come from a
// verified contracts.Scope or an Operator, never from a request body. PutPolicy needs
// the write capability of the level (WriteCapability), and a policy that lets internal
// or confidential data leave also needs CapabilityApprove and records the writer's own
// approval. The deployment-level policy and the kill switch are operator paths
// (Operator): a context token cannot reach them.
//
// # Evaluation
//
// Service.Evaluate runs the checks in a fixed order and stops at the first failure:
// kill_switch, external_enabled (all four request levels have an enabled policy),
// profile_active (qualified), profile_permitted (role and permitted_scopes),
// provider_allowlist, model_allowlist, region (an unknown residency is denied, every
// processing region must be listed at every level), classification, source_mode and
// envelope. The most restrictive level wins everywhere: allowlists must list the
// provider at every level, classes must be listed at every level, and source modes
// (deny < local_only < allow_metadata < allow_excerpt) are a ceiling down the chain.
//
// Classification (additive plan 6.1): restricted is never egress-eligible and a
// restricted part fails the request; an empty or unknown class or category is the
// most restrictive (unclassified_data); confidential needs a per-source policy that
// lists it with an approval; credentials and raw database content are blocked
// categories, not classifications, and are removed. Content marked local-only is
// removed, and secrets, filesystem paths, database handles and MCP URLs found inside
// text are scrubbed (redact.go). RAG evidence is filtered by the effective source
// mode (envelope.go: bounded excerpts, metadata only, or nothing); evidence marked
// Required that policy keeps out denies the request with the source reason.
//
// # Decision, envelope and disclosure
//
// Every evaluation appends one dataegress.decision audit event, an
// providers.EgressDecision that holds no content, BEFORE the filtered
// providers.RequestEnvelope is returned; an append that fails denies the request.
// A denial is an error with code permission_denied whose Details name the rule
// ("rule", "check", "reason_code", "decision_id") and never carry content. A failure
// of the guard itself (store, audit) is a denial too, never "unavailable". Disclose
// computes the section 6.3 disclosure from the same checks.
//
// # Seam
//
// Guard is the method set the dialogue PromptGuard adapter calls. The adapter lives
// in internal/dialogue/extmode (X-12): this package cannot import internal/dialogue,
// which sits above it.
package dataegress
