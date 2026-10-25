# 0034 - Dialogue engine context budget: one profile, a bounded builder, responses outside it

- **Status:** accepted
- **Date:** 2026-10-03
- **Decided in task:** M3-18 (`internal/dialogue`, `internal/dialogue/context`)

## Context
The engine had six free-standing limits (utterance, prompt, proposal and response tokens, recent turns, turn length) while the bounded context builder (dctx, M3-03) carried the profile budgets
measured in M0-15. Two sets of numbers per profile drift apart, and a prompt built outside the builder is a prompt nobody bounds.

## Decision
- `Config.Profile` (a `dctx.Profile` value; zero means the Lite seed) REPLACES the six limits. The composition root resolves a profile name with `dctx.ProfileByName`; an invalid profile is an error from `New`.
- Every proposal prompt is built through `dctx.Builder.Build`. `Visible(capability, field)` comes from the turn's own policy decisions: shortlisted capabilities reuse the router's decisions, capabilities named only
  by the goal, set-aside goals or capsules cost one fresh `PhaseExpose` `Decide` each, undeclared or denied ones are never shown, and a nil `Visible` shows nothing.
- The response prompt stays assembled in `prompt.go` (there is no history to select) with its own fence; its budget is `Profile.ResponseBudget()` and, over it, the reply degrades to the deterministic template.
- The engine still clips an over-long user message in `admit` (the M1-14 behaviour "cut, not widened"); dctx's rejection is the backstop.
- `earlier_reply` joins the fence-break set, so a replayed assistant body cannot close its own fence (SEC-007 neighbourhood).

## Consequences
- The Lite proposal prompt at the plateau of a 500-turn conversation grows from 1,016 to 1,171 estimated tokens (+15%); real tokens measured 0.49 to 0.57 of the estimate for proposals (M0-15).
  Each state-only capability costs one policy record per turn (about 1.65 KB); on the 500-turn run policy records went from 950 to 1,000.
- No real-model measurement of the new context exists yet (the model was the fake); the real-token and latency figures are arithmetic and labelled estimates (docs/efficiency/M3-18.md).
- serve must select the profile and give the runtime wrapper `Profile.Budget()` (M3-23); until then every deployment runs the Lite seed.

## Reversal plan
Restore the six `Config` limits and the engine's own prompt assembly from the version before M3-18; `dctx` stays as a library. The recorded 30-turn replay (`TestExistingDialogueBehaviourUnchanged`) is the regression guard.
