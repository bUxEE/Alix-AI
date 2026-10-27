// Package scopegate is the deterministic out-of-scope gate that runs before the controller
// model (master plan 11.1, 8.3 step 2; M0-21).
//
// The controller model's weakest line on the Lite bar is refusal: asked for something the
// catalogue does not offer, small models answer with a capability anyway (M0-17). Whether an
// utterance has anything to do with the exposed capabilities is a question about words, and
// words need no model. The gate reads the utterance and the visible catalogue and answers
// one of three things: refuse (nothing in the utterance relates to any exposed capability,
// answer with a typed out-of-scope refusal and do not call the model), pass (some word
// relates, the controller decides) or abstain (the gate cannot judge, the controller
// decides).
//
// The gate only ever REMOVES a model call. It never selects a capability, never fills a
// field, never authorises anything and never grants what the model could not do: a pass and
// an abstain change nothing downstream (policy, the schema check and the validation still
// decide), and a refuse is the most restrictive answer. It fails toward the model on
// everything it cannot read, and toward refusal only for an empty catalogue.
//
// What it reads: the rules of the capability router's text matcher (internal/capabilities,
// M1-08: tokenising, accent folding, the stop words and the stem rule), extended to the
// whole wording of a catalogue (capability ids, descriptions, field names, enum values and
// the application's per-language terms). It reads a language only if the catalogue's own
// wording covers it. See Decide for the rules, in order.
//
// The package imports nothing outside the standard library (no provider, no adapter, no
// network): TestGateIsDeterministicAndNeedsNoModel enforces that.
//
// Placement (not wired in this task): after the engine's deterministic rules (help, cancel,
// restart, resume, reject, confirm, ordinal, reference), before the controller model call,
// and only when the session is not engaged. The bench puts it in front of the controller
// with `bench run --gate`.
//
// Analyst path (ADR 0047): when Request.AnalystActive is set and Domain is declared, the
// vocabulary also includes the package domain wording so an in-domain question that matches
// no capability is not refused here; the model-judged domain classifier decides. Clearly
// unrelated utterances (no overlap with catalogue or domain) are still refused.
package scopegate
