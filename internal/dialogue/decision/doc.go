// Package decision wires the optional decision layer (master plan 8.5, 11.1;
// docs/architecture/decision-layer.md) into the dialogue pipeline through the seams
// internal/dialogue already exposes: the DecisionStage, the control Detectors and the
// Turner wrapper below. The dialogue core is not edited.
//
// The layer is OPTIONAL, BOUNDED, NON-GENERATIVE and NON-AUTHORITATIVE. What it does:
//
//   - exact rules first: an utterance that is exactly a shortlisted capability's id or
//     title is routed with no scorer at all (ExactCapability), and the exact control
//     phrases of the core's rule detectors still win over everything;
//   - Stage (dialogue.DecisionStage): builds the bounded candidate set from the policy
//     exposed shortlist, calls the scorer through internal/decision.Engine (guard, gates,
//     cache, audit), re-resolves the selected id through the Resolver (the real policy
//     gateway over the active package) and returns a dialogue.DecisionResult. A selection
//     only narrows the capabilities the controller is offered; the proposal, validation,
//     entity resolution, prepare, confirmation and commit are the normal pipeline;
//   - Detectors (dialogue.Detectors): the same rule detectors for help, cancel, reject
//     and confirm, extended with a scorer that is consulted only on a rule miss, only for a
//     short utterance and only among the acts the session state permits (with "ordinary
//     request" always one of them). Reject, cancel while a preparation is pending,
//     restart, resume and correct stay rules only: advice never declines a preparation;
//   - Turner (Layer.Wrap): binds the verified scope to the turn so the detectors, which
//     receive no scope, can call the engine, and counts the controller calls and generated
//     tokens a turn avoided from the turn's own Trace.
//
// Every failure, every gate and every disabled or unavailable layer ends in a typed
// fallback reason and the unchanged controller path. Nothing here imports a database,
// the RAG package, the action coordinator, an adapter or the network, and nothing here
// can authorise, validate, prepare, confirm, commit, lower a risk level or write prose.
package decision
