// Package loop is the bounded read-only analyst step loop of ADR 0047 (release 1.5).
//
// It is entered from dialogue.Engine.Turn only when the effective abilities include
// loop and the resolved controller is analyst_capable. Each iteration asks the
// controller for one constrained-JSON AnalystStepProposal, validates it, executes
// a read-only effect (or hands off answer / clarify / act), and feeds the next
// prompt. No write commits inside the loop: act ends the loop and returns a
// hand-off for the normal prepare → confirm → commit path.
//
// # Integrator wiring
//
// The composition root (or a later integration task) constructs a [Runner] with
// [Deps] that point at the sibling implementations:
//
//   - [Computer] — internal/analyst/compute
//   - [AnalystVerifier] — internal/verify (analyst entry)
//   - [WebSearcher] / [WebFetcher] — internal/analyst/web
//   - [DomainClassifier] — internal/analyst/domain
//   - [AbilityResolver] — internal/analyst/settings
//   - [CapabilityReader] — typically dialogue wrapping actions.Coordinator.Read
//   - [StepCaller] — typically dialogue wrapping ProviderSelector + PromptGuard
//   - [Budget] — reservation/settle against providerbudget (ADR 0046 accounting)
//   - [Progress] — publishes analyst_* progress stages (emit mapping is a sibling)
//
// With AbilityResolver nil or abilities.loop false the dialogue engine never
// calls this package, so Local mode stays byte-identical to 1.4.
//
// # Client capabilities
//
// [RememberClientCapabilities] stores the turn request's announcement under the
// turn id so the emit layer can read it via [ClientCapabilitiesFor] without
// importing internal/stream. Absent announcement means 1.4 emit surface.
package loop
