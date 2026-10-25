// Decision-layer client surface (advisory status only: provider, model, mode, fallback and counters; never an authorisation
// result). src/index.ts re-exports this file as the `decision` namespace, so everything below is `decision.<name>` to a host.
//
//   const client = decision.createDecisionClient({ baseUrl, getContextToken });
//   const status = await client.getStatus(); // needs the capability decision.status.read
//   show(status.mode, status.accepted_count, status.fallback_counts);
//
// The decision layer is optional, bounded, non-generative and non-authoritative: it scores candidates that were already
// authorised and never authorises, validates, prepares, confirms or commits anything. The status is ADVISORY operational
// information, and no member of it, no counter and no mode is permission to do anything: authority stays with the deterministic
// controller. A host tests against `decision.createFakeDecisionServer` (fixtures in `decision.decisionStatusFixture`).

export type { DecisionStatus, DecisionStatusFallbackCount, DecisionStatusMode, FallbackReason } from "./types.js";

export { createDecisionClient } from "./client.js";
export type { DecisionClient, DecisionClientOptions, DecisionContextTokenProvider, DecisionContextTokenRequest } from "./client.js";
export { FALLBACK_REASONS, isDecisionStatus } from "./guards.js";
export { DECISION_OPERATIONS, DECISION_STATUS_CAPABILITY } from "./operations.js";
export type { DecisionOperationId, DecisionOperationSpec } from "./operations.js";
export { createFakeDecisionServer, decisionStatusFixture } from "./testing.js";
export type { FakeDecisionServer, FakeDecisionServerOptions, RecordedDecisionRequest } from "./testing.js";
