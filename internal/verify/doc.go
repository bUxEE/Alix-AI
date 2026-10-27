// Package verify is the last step of a turn (master plan 11.1, "verified natural response"): it
// decides whether the words a model wrote may be shown, or spoken (master plan 13), and writes the
// words itself when they may not.
//
// The model verbalises a DialogueDirective, and for an outcome the ActionReceipt the directive
// names. It cannot make a fact: the facts are the host's, the act and the receipt are the core's
// (master plan 3 rule 5; docs/integration/conversation-objects.md). So the response is untrusted
// text, and this package checks it against the objects it came from, with code and no second
// model (rule 16; docs/efficiency/M1-13.md):
//
//   - Verify checks a text. It passes only when the text states every fact the directive names in
//     must_state, states no number, date, time, amount, weekday or identifier the directive and
//     receipt do not carry, claims completion only against a committed receipt, and cites only
//     sources the directive lists.
//   - Render writes the deterministic response for a directive in a language (en, it): plain
//     phrasing, the facts from their canonical values, nothing else.
//   - Realise is what a turn calls: the model's text when it verifies, otherwise the template for
//     the same directive. Degraded is the template alone, for when no model is available. Both
//     verify the template before returning it, so an unverified template is never shown.
//
// # What a response may state
//
// The allowed set is built from the directive and the receipt: the typed facts (display forms
// too), and every string the core or the host wrote for the turn (field labels, choices,
// suggestions, permitted-action labels, issue messages, source names and claims). The same
// extractor reads those strings and the response, so a time inside a choice label or the number
// in a street address in a text fact is allowed, and the model may quote them.
//
// Values are compared after normalisation, never as spellings. Dates, times and numbers are read
// in the conventions of the response language, and an ambiguous reading keeps all its meanings:
//
//   - numbers: digits (also full-width and other scripts' digits), grouping and decimal
//     separators ("1,234.56" in English, "1.234,56" in Italian, no-break spaces), signs, percent,
//     ordinals, and English and Italian number words ("twenty-four", "ventiquattro");
//   - dates: "14 October 2026", "October 14th", "14 ottobre", "2026-10-14", "14/10/2026",
//     "14.10.2026"; in English "04/05/2026" is ambiguous and is accepted only when both readings
//     are allowed; in Italian it is the fourth of May. Weekdays must belong to the date they
//     qualify;
//   - times: "10:30", "10.30", "10h30", "10:30 pm", "alle 10", noon and midnight; a datetime fact
//     is compared in the IANA zone the host displays it in, never in UTC wording;
//   - money: an amount and a currency (symbol, ISO code or word) are one claim and must match a
//     money fact together;
//   - identifiers: exact, case-sensitive; a token that mixes letters and digits, a snake_case
//     token, a typed core ID, a URL or an e-mail address is an identifier, and an identifier the
//     directive does not carry is invented.
//
// Citations are written as "[1]" (the position in the directive's sources), "[src_...]" or a
// bare source ID, "(source: Name)", "[fonte: Name]", or a "Sources:" line or list of names. A
// source the directive marks removed cannot be cited.
//
// Text that contains invisible format or control characters (zero-width characters, bidirectional
// overrides, a byte order mark) is rejected: what a reader sees is not what was verified.
//
// # Completion
//
// A response claims something was done with a past participle or a completion phrase ("booked",
// "prenotato", "successfully", "all set"). That is allowed only when the receipt is committed and
// the directive names it. A negated mention ("was not booked", "non è stato prenotato"), a
// progressive one ("is being processed") and the host's own wording quoted verbatim (a source
// claim, a receipt error message, a label) are not claims.
//
// A host fact is data, whatever its value says: the clinic's prepare result for a cancellation
// carries appointment_status = "cancelled", and the confirmation that asks whether to go ahead
// must state it. So a completion word (or phrase) that is the whole value of a fact the template
// for the act states, stated as that fact ("Appointment status: cancelled": the fact's label, a
// colon, its value, the end of the clause), is data and not a claim. That is all it is. The same
// word anywhere else ("Your appointment is cancelled"), in a longer value, under another label,
// or in a fact the template does not state, is a claim and needs a committed receipt like any
// other; the rule needs no application to avoid words like "cancelled", "booked" or "paid" in its
// fact values. A host string of several words is quoted, as above, wherever it stands.
//
// # Limits
//
// The checks are lexical and bounded, by design: they do not judge meaning. What they do not
// check, and who does: whether a source really supports the claim a response attaches to it (the
// core's claim/source check, before the directive is built); whether a suggested next action is
// permitted (the policy gateway refuses it at prepare and commit); relative dates ("tomorrow"),
// ordinal words ("the third"), roman numerals, a bare domain name without a scheme, number words
// in times of day ("ten thirty"), "one", "un", "uno", "una" and Italian "sei" on their own (they
// are ordinary words), and a completion claim made with a verb the lexicon lacks. A boolean fact
// without a display form is checked only for a plain yes or no. Each of these fails towards the
// template or is covered by another gate; none can make the verifier accept a wrong fact, because
// facts are compared by value.
//
// Verify never logs and never returns the offending text of an identifier, citation or
// completion claim: violations carry a kind, the canonical form of a number, date, time or
// amount, and a fingerprint, so a result can be logged without echoing a secret the model
// repeated.
//
// # Analyst answers (ADR 0047 / ADR 0049)
//
// VerifyAnalyst / RealiseAnalyst check a model answer against an AnalystFactSet
// (datasets, derived facts, web citations with fetched text) and optional source
// spans / block plans. Numbers and named entities must trace to app or computed
// facts (with the usual format variants); web numbers must appear in the cited
// fetch; knowledge must not restate tenant-looking figures; forecasts need an
// estimate label and method. Failure yields the analyst template: a table of the
// final dataset plus a provenance block. NewAnalystStream verifies
// segment-by-segment (ADR 0046); a later failure replaces with that template.
//
// Reference tokens {{ref:<dataset_id>:<row>:<field>}} name a cell of a turn
// dataset so withheld values can appear in prose after verification. Tokens are
// validated then left in Text/ModelText; ResolveAnalystRefs formats the cell
// (language-aware) for the client. Incomplete tokens are held across stream
// segments so a half-resolved token is never emitted.
//
// The package imports only pkg/contracts and its generated subpackages (docs/architecture/module-map.md).
package verify
