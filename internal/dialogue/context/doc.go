// Package dctx assembles the bounded model context of one dialogue turn (master plan
// 11.3, 17.2 and 7.5) and compacts the history that goes into it, deterministically,
// without a model.
//
// The directory is internal/dialogue/context; the package is named dctx so that code
// which imports it keeps the standard library's context package under its own name.
//
// # What it does
//
// The prompt of a turn is the sum of a fixed part (the trusted system frame and the
// output constraint, which the caller renders) and a history part that grows with the
// conversation unless something stops it. This package is that something:
//
//   - a Profile fixes a HARD token budget per deployment profile (lite, balanced,
//     experimental-4g, performance), a separate cap for everything derived from history,
//     and the counts and sizes of each history section;
//   - the session state is rendered compactly: the active goal with its visible slots,
//     the goals set aside (names only), the newest completed-goal capsules in full and
//     the older ones as one digest line;
//   - the replay window is not "the last N rows": rows are SELECTED. The newest rows,
//     the rows that produced a value the goal now holds and the rows that share words
//     with this turn's goal and message are kept, best first, up to a row count and a
//     token cap; the rest are not replayed;
//   - over budget, history is cut in a fixed order (the selected rows that are not among
//     the newest, then the digest and the oldest capsules, then the goals set aside, then
//     the newest rows themselves: the question being answered outlives an old completed
//     goal), then the offered choices, then the lowest-ranked capabilities, and if the
//     minimum still does not fit the build fails with the same typed budget violation the runtime boundary
//     raises (validation_failed, providers.IsBudgetViolation) and nothing is sent.
//
// Everything is deterministic: maps are rendered in sorted order, no time or random
// source is read, and the same input gives the same bytes (TestCapsulesDeterministic).
// There is no model summarisation; docs/efficiency/M3-03.md records why.
//
// # What it never does
//
//   - It never reads the transcript. The full transcript of a session lives in its own
//     module (internal/sessions, "transcript"), and the only history this package can
//     reach is the bounded recent window through TurnSource, an interface with one
//     method, RecentTurns. A test scans the package for any reference to the
//     transcript (TestOldTurnsNotReplayed).
//   - It never puts text a user, a host or a document wrote into a system message. All of
//     it is fenced as labelled data in a user or assistant message, with fence tags inside
//     the text neutralised and control and format characters removed (master plan 12.6).
//   - It never renders an identifier of the scope (tenant, workspace, principal,
//     session), a preparation ID, a receipt ID, a confirmation token or an idempotency
//     key: the model does not need them (rule 6).
//   - It shows a slot, a capsule fact or a capability only where the caller's Visible
//     function says the model may see it. Without that function nothing is shown
//     (fail closed).
//
// # Adopting it in the engine
//
// The package imports no part of internal/dialogue, so the engine can import it, and since
// M3-18 the engine builds its proposal prompt only through Builder.Build. The caller owns
// content (the frame text and the output constraint, as a Fixed function of the shortlist
// length) and the package owns layout, data flow and budget, which is how a shared component
// should be split. The estimated token counts are providers.Budget.EstimateChat, the same
// number providers.WithBudget enforces.
//
// The budgets of a Profile are the token caps of docs/operations/resource-budgets.md section
// 6.2 (M0-15), which owns them; the table in profile.go is a copy and a number is changed in
// the document first. Besides the prompt, output, shortlist, utterance and history caps the
// profile carries the output-constraint cap (enforced here: the builder lowers the shortlist
// until the constraint fits) and the response-prompt cap (Profile.ResponseBudget, which the
// engine builds its response prompt against).
package dctx
