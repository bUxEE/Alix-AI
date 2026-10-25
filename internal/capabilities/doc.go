// Package capabilities is the capability registry and the deterministic-first
// router: the "capability shortlist" step of the turn pipeline (master plan 4.1,
// 11.1). Its job is to keep the prompt small and the model's options few.
//
// # Registry
//
// A Registry holds, per application, the capabilities of its active package. It
// is read from installed packages (apppkg) and only ever from the active state:
// a quarantined, rejected or superseded package registers nothing, so it exposes
// nothing. An entry is found by the application of the verified Scope and by
// nothing else; a capability ID is meaningful only inside its application.
//
// # Router
//
// A Router answers "which capabilities could this turn be about?" in the order
// of master plan 7.5, smallest first:
//
//  1. No model. The policy gateway's Expose decides what the caller may see
//     (token scope, roles, host rules, field projection). That set is the only
//     pool; nothing below can add to it.
//  2. Rules and lookups over that pool: the active goal and the next actions it
//     allows, then the package's terminology (the terms a capability's
//     conversation contract relies on, with their synonyms, per language) and
//     the words of the capability title. Text is lower-cased, accent-folded and
//     matched with a small stem rule that covers English and Italian inflection
//     without a dictionary. Evidence shared by many visible capabilities counts
//     for less than evidence one of them owns.
//  3. The smallest model, optional. If an Embedder exists and the rules are not
//     decisive, the cosine similarity between the utterance and each visible
//     capability's description adds evidence. With no embedder, or a failing one,
//     the result is the rules-only result.
//
// The result is a bounded shortlist (Config.MaxShortlist, at most
// ShortlistCeiling) of policy-visible capabilities, each carrying the allowed
// policy Decision and the reasons it matched, or an explicit out_of_scope
// outcome. A score ranks; it never authorises. Policy runs again at prepare and
// commit, and the optional decision scorer (milestone D) runs after this stage on
// the shortlist it returns and is independent of it.
//
// Capability examples (Capability.Examples) are test-only by the package schema
// and are never read here: they are not keywords and not embedding anchors.
//
// # Limits
//
// The rules match words, not meaning: a request that names an unavailable action
// on an available noun ("cancel my booking" when only "create booking" exists) is
// shortlisted on the noun. Deciding that the verb has no capability is the
// controller's job and the host's validation; the shortlist is a superset on
// purpose. Scripts written without spaces are matched character by character, so
// a label must be written as the user would write it.
package capabilities
