// The decision-layer types of the client: the generated ones (schemas/decision/decision-status.schema.json), re-exported so a host
// imports them from one place.
//
// ADVISORY, NOT AN AUTHORISATION. The decision layer is optional, bounded, non-generative and non-authoritative: it scores a
// short list of candidates that were already authorised and never authorises, validates, prepares, confirms or commits anything.
// A DecisionStatus is operational information about the layer (provider, model, mode, counters). It carries no score, threshold,
// gate, candidate or tenant datum, and no counter in it is permission to do anything. Authority stays with the deterministic
// controller and the application's own checks.

export type { DecisionStatus, DecisionStatusFallbackCount, DecisionStatusMode, FallbackReason } from "../gen/decision/index.js";
