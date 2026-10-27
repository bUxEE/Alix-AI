// Package dialogue is the turn pipeline of master plan 11.1: the engine that takes
// one authenticated message, or one structured input from the client, and returns a
// DialogueDirective, a PreparedAction or an ActionReceipt together with a verified
// natural response. It is wired from the merged components and decides nothing a
// model could decide wrongly: the model proposes a typed task and phrases a
// directive, and everything it writes is validated or verified before it is used
// (master plan 3 rules 5 to 8 and 14).
//
//	scope and rate checks
//	  -> active goal and state lookup               (internal/sessions)
//	  -> deterministic rules for exact cases        (help; cancel; restart; resume; reject; confirm; relative references; a zero-field read; choices)
//	  -> optional analyst step loop (ADR 0047)      (internal/analyst/loop; only when abilities.loop and analyst_capable)
//	  -> capability shortlist                       (internal/capabilities, policy-exposed only)
//	  -> optional decision stage                    (DecisionStage seam, advisory)
//	  -> bounded context assembly                   (internal/dialogue/context, profile token budget)
//	  -> constrained TaskProposal                   (model call 1, only when needed)
//	  -> schema validation                          (embedded model-only TaskProposal schema)
//	  -> entity resolution                          (adapter Read, never the model)
//	  -> host reauthorisation and validation        (internal/actions Read / Prepare)
//	  -> DialogueDirective or PreparedAction
//	  -> confirmation                               (structured client input, never read from text)
//	  -> idempotent commit                          (internal/actions Commit, the only path to a write)
//	  -> ActionReceipt
//	  -> verified natural response                  (model call 2, internal/verify, else the template)
//
// # Entry point
//
// Engine.Turn takes the verified Scope and a TurnInput. Exactly one of Text, ChoiceID,
// Confirm or Reject is set. A text turn runs the whole pipeline. A choice selects one of
// the host's candidates (no model reads it). A confirmation presents the single-use
// token the client received with the PreparedAction; the token goes from the Result to
// the client and nowhere else, and a text message can never confirm anything.
//
// An error means the turn could not be run: no valid scope, an invalid input, an
// unsupported language, a rate limit, a session that is not the presented context, a
// store that failed before anything was decided. Everything else is a Result. A refusal
// or a failure is a directive (refuse, report_failure) with a typed Code and a response
// that says so; it is never a success-looking one. A model that is unavailable, slow or
// malformed yields report_failure with nothing executed.
//
// # What the model sees
//
// A short system frame written by the core (the application, the output rules, the
// date, the shortlisted capabilities with their policy-visible fields only, the terms
// they use), then the session state (the active goal, the goals set aside and the
// completed-goal capsules, each shown only where the policy gateway's decision for its
// capability lets the model see it), the replayed turns and the user's message, each
// fenced as labelled data in a non-system message (master plan 12.6). A system message is
// only ever core text. Never: a context token, a credential, a confirmation secret, an
// idempotency key, the principal, tenant or workspace identifiers, a goal, preparation or
// receipt ID, another tenant's text, the full transcript, or an unbounded tool list.
//
// The proposal prompt is assembled by internal/dialogue/context (package dctx) within the
// token budget of the deployment's profile (Config.Profile, Lite by default), which is
// checked before the runtime is called. The builder replays the newest rows and the rows
// that share words with the turn, never "the last N", caps everything derived from history
// separately, and over budget cuts in a fixed order (the less relevant rows, the capsule
// digest and capsules, the goals set aside, the newest rows, the offered choices, the
// lowest-ranked capabilities); if the minimum does not fit nothing is sent
// (validation_failed, not retryable). The response prompt has its own, smaller cap from the
// same profile.
//
// The model's output is untrusted. It must be one JSON document that satisfies the
// model-authority subset of the TaskProposal schema (trace_id and every other property
// the core owns are removed from the grammar; the core attaches its own trace ID), the
// capability must be on the shortlist the model was offered, the policy gateway must
// allow it, and every value is checked against the capability's own field definitions.
// A rejected output is schema_invalid and no part of it is used. The response is the
// model's text only if internal/verify accepts it against the directive (and, for an
// outcome, the receipt, which is always passed); otherwise it is the deterministic
// template for the same directive, and the template alone when no model is available.
//
// # Seams
//
// Each seam has a default that keeps the pipeline what it is without the feature behind
// it, so Local mode behaves exactly as before.
//
//   - DecisionStage (master plan 8.5, optional, advisory): called after the shortlist and
//     before context assembly with the bounded, policy-authorised candidate set. It
//     returns a selected candidate or a typed FallbackReason. A valid selection narrows the
//     capabilities the controller is offered; nothing else changes: the proposal, the
//     policy gateway, prepare, confirm, commit and the verifier all still run, and every
//     selected ID is still resolved by the deterministic adapter. A selection outside the
//     candidate set, a malformed result, a panic or a timeout is a fallback. The default,
//     NoopDecisionStage, always falls back. This package imports no decision package; the
//     decision milestone implements the stage in internal/dialogue/decision.
//   - PromptGuard and ProviderSelector (external provider mode additive plan): the selector
//     picks the provider for a role (LocalSelector returns the one local provider wrapped
//     in the request budget); the guard sees every built prompt, with each context item's
//     internal metadata (source ID, DataClass, local-only), after it is built and before it
//     is sent, and returns the request that may be sent or a typed denial. The metadata is
//     stripped when the ChatRequest is made. The default guard, LocalOnlyGuard, allows the
//     local provider and denies any other, so no context leaves without a real guard and
//     there is no hidden fallback. This package imports no external-mode package.
//   - RateLimiter: a sliding-window limiter per principal (WindowLimiter), in memory.
//   - EntityResolver: resolves a mention to the host's record IDs and a data version. The
//     default, AdapterResolver, is an adapter Read (see entity.go for the convention).
//   - PackageSource: the typed package a turn is served from. capabilities.Registry does not
//     export its compiled package, so the composition root backs this with the same source
//     it gives the registry and the coordinator; the engine checks it is the verified
//     application's, active, and the content the registry routes against.
//
//   - Detectors (master plan 8.5): one small ControlDetector per bounded control decision
//     (help, cancel, restart, resume, reject, confirm, correct). The defaults, RuleDetectors,
//     are exact whole-utterance phrase tables in English and Italian and need no model; the
//     optional decision layer may supply its own for any of them and so pre-empt the
//     controller for these decisions. A detector only says what an utterance IS. The engine
//     checks that the session is in a state where the transition applies and takes it itself,
//     and no transition reachable from a detector can prepare, confirm, commit or call a
//     tool. A detection that does not fit the state, or a detector that panics, is ignored.
//
// # State
//
// Durable (internal/sessions): the active goal and the goals set aside, each with its typed
// slots and where every value came from, its resolved entities with the data version each
// relied on, its pending clarification and the reference (with the expiry) of its prepared
// action; a summary of the last directive; the records the last results presented; compact
// capsules of completed goals; and a bounded window of recent turns. A turn starts with
// Store.Reconcile (Open, plus the token it carries: a permission the token no longer grants
// discards the goals that relied on it) and State.Expire (an idle goal, a suspended goal, a
// question past its timeout, a preparation past its own expiry, an old result). Expired state
// is discarded, not reused. Every preparation a state change leaves without a goal is
// invalidated through the coordinator before the turn is saved; if that fails, the turn fails
// and the state is not saved, so a preparation is never left confirmable behind a session that
// forgot it.
//
// # Multi-turn behaviour
//
// Every transition below is a deterministic function of the state and of typed input. The
// model only proposes an intent (TaskProposal.intent: task, correction, cancel, help,
// out_of_scope) and values; what an intent means is decided here.
//
//   - Correction. A value that differs from one already collected replaces it and is recorded
//     as a correction, whatever intent the model gave it (a correction cue in the words, or
//     the intent, also reopens a finished goal instead of starting a new one). A preparation
//     built on the old values is voided first (field_corrected), and the goal is prepared
//     again. A correction for a capability the active goal is not on corrects nothing: it is a
//     plain request for that capability.
//   - Cancel. An exact "cancel", "never mind", "annulla" (or the intent) rejects the pending
//     preparation through the coordinator, forgets the question and ends the goal into a
//     capsule that holds no value; "cancel everything" ends the set-aside goals too. A typed
//     "no" while a preparation awaits confirmation is the same. If the preparation cannot be
//     rejected, nothing is dropped and the turn reports a failure.
//   - Confirm. A typed "yes" never confirms anything: the pending request is shown again, with
//     no new secret. A confirmation is only TurnInput.Confirm.
//   - Restart. "Start over" begins the goal again from nothing (same capability, fresh goal,
//     no slots, entities or question, preparation voided) and asks for what it needs.
//   - Topic switch and resume. A request for another capability sets the active goal aside
//     (suspended with its slots, entities and question; its preparation is voided, so a
//     confirmation cannot be answered while the user is elsewhere) and a finished goal becomes
//     a capsule. "Resume", "go back" (or "go back to the <goal>") brings the goal back and
//     asks the question it was waiting on or prepares it afresh; "continue" does so only when
//     no goal is in progress. Resume is a rule only: the TaskProposal schema has no such intent.
//   - Nested goals. A lookup (a level 0 read) asked while the goal waits for an answer is a
//     side trip: the goal is nested, the lookup is its child, and when the child finishes
//     (or is cancelled) the parent is active again with its question still pending. Anything
//     else is a topic switch.
//   - Relative references. "The second one", "that patient", "same time tomorrow" are resolved
//     from the state by rules (refs.go), never by a model: an ordinal indexes the choices just
//     offered, else the records the latest result presented in order; a demonstrative is the
//     one record of the field's entity type the state holds (ambiguous, so unresolved, after a
//     list); "same time <day>" is the time of day of the latest date and time the state holds,
//     on that day, in UTC. A reference that is the whole utterance is applied without the
//     model; one inside a longer request overrides what the model proposed for that field. A
//     reference the state cannot resolve to exactly one answer is left alone, never guessed.
//

// # Limits, stated
//
// The engine does not call a model to repair a malformed proposal (one call, validated).
// Free text that matches no capability is refused by the router without a model, so a
// request the package's terminology and titles do not name is out of scope until the
// optional embedder or the decision stage says otherwise; only the exact control phrases of
// the rule detectors (help, cancel, restart, resume, a typed yes or no) are answered without
// one. A phrase inside a longer sentence is the controller's: "no, make it 5" is a correction,
// not a rejection. Dates are UTC, so "tomorrow" is the next UTC day. Literal field defaults and context defaults are the host's business:
// the engine sends only what was proposed and validated. Dates in the prompt are UTC.
//
// # Audit
//
// Policy decisions and the action lifecycle are recorded by the gateway and the
// coordinator, each fail closed. The engine adds one dialogue.turn event per turn
// (identifiers, act, code, call sizes; never text), written before the turn's state is
// saved and fail closed like the others: a turn that cannot be audited is not
// acknowledged. If a commit may already have happened, Turn returns the Result with its
// receipt together with the error, because the receipt has its own write-ahead audit and
// must reach the caller. With no Appender in Deps no event is written.
package dialogue
