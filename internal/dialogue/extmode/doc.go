// Package extmode wires External Provider Mode (additive plan sections 7 to 10) into the dialogue
// turn pipeline (master plan 11.1) through the two seams M1-14 exposes, without editing the
// dialogue core. internal/dialogue and internal/rag import no external-mode package; this
// subpackage and the composition root are where internal/dataegress, internal/modes and
// internal/providerreg meet the engine.
//
// # What it provides
//
// Runtime implements both dialogue.ProviderSelector and dialogue.PromptGuard, and is also the
// text generator the selector hands the engine for an external route:
//
//	Select -> the mode of the scope (modes.Resolver, the kill switch included): Local, or a role
//	          with no external route, returns the local provider unchanged. Otherwise the engine
//	          gets the external route, marked not-local.
//	Guard  -> for the external route only: the prompt items are mapped onto a dataegress
//	          Candidate with the registry profile (never a client value), evaluated, and the
//	          items that survive are returned. local_only items, credentials and everything the
//	          policy keeps out are gone before the budget check and before any send.
//	Chat   -> modes.Router.Execute: kill switch, profile gate, the egress guard again, budget
//	          reservation, then the Sender. A request that was not released by Guard is refused.
//
// With external mode off (the default) none of this runs: Select returns what the local selector
// returns and the Guard is never reached for a non-local provider.
//
// # Authority
//
// The provider is an inference backend. Its output is untrusted: a tool-call proposal becomes
// the text of a TaskProposal and goes through the engine's own schema, shortlist, policy, entity
// and host-reauthorisation path, so there is no code path from a provider to an adapter (the
// Sender interface has no adapter in reach). A malformed tool call keeps the capability and drops
// the arguments, so the engine asks the user for the missing fields; an unknown tool is an
// unknown capability and is refused. Tools are small allowlisted schemas built from the
// capabilities and visible fields the engine offered for the turn.
//
// # Evidence
//
// Knowledge answers a question from the sources the principal may read. In an external route the
// retrieved excerpts are evaluated by the egress guard first: a local_only or denied source is
// withheld, and if the best evidence is one the provider may not receive the result is the typed
// no_supported_evidence with reason egress_restricted (the selected provider cannot access the
// required evidence) and nothing is sent. Citations come from the verified answerer, never from
// the provider.
//
// # Native provider features
//
// Provider-native agents, hosted memory, file search and server-side tool loops are disabled for
// every consequential operation (anything that may propose an action, or offers tools). For other
// calls they stay off unless the operator listed the feature as a classified convenience feature.
//
// # Limits, stated
//
// The dialogue core marks the directive of a response prompt restricted and local-only, so by
// default an external provider never realises a response: the deterministic template is used.
// Config.DirectiveClass opts a deployment into sending the directive, at the class it names.
// User-authored context (the message, earlier turns and the session slots) is classed confidential
// by the core; it is mapped to internal for the egress decision, because it is what the principal
// told the assistant, and the operator's internal-class approval is what lets it leave.
package extmode
