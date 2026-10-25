// Package hybrid is the Hybrid production mode of External Provider Mode (additive plan phase D,
// section 2.3; ADR 0023 and ADR 0027). Identity, scope, application state, deterministic routing,
// the conversational controller and RAG stay local. The one job that may leave is language
// realisation, and only from a DialogueDirective (and ActionReceipt) that already passed local
// verification, in the facts-only envelope of ADR 0023.
//
// # One turn
//
//	verify.Degraded(directive, receipt)      the directive is usable, and the template exists
//	mode, class, source gates                Hybrid only; sensitive or unclassified stays local
//	modes.Router (kill switch, profile gate, egress guard, budget, send)
//	verify.Realise(text, directive, receipt) the external text is untrusted until it verifies
//
// The user always gets correct facts: external text that verifies, else the local model's text
// that verifies, else the deterministic template of M1-13. A failed verification of external text
// goes to the template, never to a second external call and never to another provider.
//
// # What leaves
//
// The envelope holds the instructions this package writes, one dialogue_directive part (the act,
// the facts, what must be stated, questions, suggestions, permitted next actions, no ids), one
// action_receipt part when there is a receipt, and one rag_excerpt part per cited source with
// Required set. The egress guard keeps a source's claims only for allow_excerpt, only its name for
// allow_metadata, and denies the whole request for a local_only or denied source: a turn that
// involves such a source is realised locally. A turn whose class is above internal, or whose class
// is not stated (Config.FactClass is empty by default, as extmode's DirectiveClass is), never
// reaches the guard: Hybrid keeps it local.
//
// # Roles
//
// Only the realisation role is routed by this package. The controller, the decision scorer, embeddings,
// reranking, VAD, local speech to text and text to speech are pinned local by the policy this
// package gives its own router, whatever the operator's policy lists. In any mode but Hybrid
// realisation is local.
//
// # Fallback and records
//
// Outage (unavailable, timeout, rate limited) falls back to the local model only when the operator's
// policy names it; RealisationPolicy builds such a policy. Every local hop and every template
// fallback is appended to the audit log as a hybrid.transition event with codes only, and returned
// on the Realised so the host can show it.
//
// # Cost and usage
//
// Each Realised carries the usage of its external call (toks_in, toks_out, integer micro-units,
// currency, reservation id, latency). Hybrid.Report adds the running totals per tenant and the
// provider usage export of internal/providerbudget for the host's own dashboards. There is no UI.
//
// # Limits, stated
//
// The composition root must wrap its dialogue runtime's policy with ControllerLocalPolicy, which strips
// only the controller and only under Hybrid, so the extmode controller route stays local there while
// External mode and the other external roles (vision, image generation) are untouched. Wiring this package into the engine's response
// step is a composition-root change outside this package.
package hybrid
