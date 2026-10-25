// Package modes is the mode manager and provider router of External Provider Mode (additive plan
// sections 2, 12 and 13). It decides WHICH provider handles a request and under what rules. It
// contains no HTTP and makes no model call: the caller hands it two functions (a local one and an
// external sender) and the router runs the gates around them. Nothing outside milestone X imports
// it except the composition roots.
//
// # Mode resolution
//
// The mode is Local (the default), Hybrid or External, set per deployment, application and tenant
// (ModeSource). The most restrictive level wins, ordered Local < Hybrid < External, and a level with
// no setting, or with a value that is not one of the three, counts as Local: external processing
// needs every one of the three levels to opt in, the same rule the data-egress chain applies. The
// global kill switch (dataegress) is read fresh on every resolution and again right before every
// external call; an engaged switch, or one that cannot be read, forces Local.
//
// # Routing
//
// Router.Execute picks a provider per role from the RolePolicy: an ordered list of candidate
// profiles and the conditions on which the LOCAL provider may serve instead. The decision-scorer
// role (router, master plan 8.5), the reranker, VAD and the embedder are never routed to an external
// provider; external roles are controller, realisation, vision, image generation, speech to text
// and text to speech. For an external call the gates run in this order, and each stops the request:
//
//	kill switch -> profile gate (providerreg.Check) -> egress guard (dataegress) -> budget
//	reservation (providerbudget) -> send -> settle
//
// A candidate is skipped for the next one only when it cannot serve the request (capability gate).
// An invalid credential, a quarantined or stale profile, denied egress and an exhausted budget end
// the request with their own typed outcome and never select another provider; only a local fallback
// the policy names for exactly that condition may serve, and the hop is recorded on the Outcome.
// A profile's own named fallback (providerreg.Fallback) is followed for unavailable, timeout,
// rate_limited and unsupported_modality only, once, and goes through every gate again.
//
// # Outcomes
//
// Every non-served result is a typed Kind (the section 13 table) and an error whose code is never
// "retry blindly": see Outcome and OutcomeOf. A network failure after bytes may have reached the
// provider, on a request that could cause a commit, is uncertain_outcome: the budget settles at the
// reserved bound, no retry and no fallback follow, and the caller must reconcile first.
//
// # Locality report
//
// Router.LocalityReport states for every capability whether it runs local, remote, is unavailable
// offline, or depends on a provider, and whether local mode alone would satisfy the required roles,
// so external mode cannot hide a failed local hardware gate.
package modes
