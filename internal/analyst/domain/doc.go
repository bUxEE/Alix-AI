// Package domain is the model-judged domain classifier for analyst turns (ADR 0047).
//
// Scopegate remains the cheap hard refusal for clearly unrelated utterances. When the
// analyst path is active and the application package declares assistant.domain, this
// package asks a constrained-JSON classifier whether the user utterance is in_domain or
// out_of_domain. Out of domain yields a typed polite refusal with no read and no search.
//
// The integrator wires a Classifier with a Model that returns constrained JSON; tests use
// a fake. Classification never trusts retrieved record text or web pages as instructions:
// only the user utterance and the declared domain are judged.
package domain
