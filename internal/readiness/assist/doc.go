// Package assist is the model-assisted leg of integration readiness discovery (master plan 9.5
// and 12.6, ADR 0037). A model reads bounded, redacted excerpts of the project's own prose
// (documentation, API descriptions, tool lists, schema files) and proposes findings; this package
// turns what it says into verify.RdyProposal values and nothing else. The model only proposes.
//
// # What the model is and is not
//
//   - Project content is untrusted data. It reaches the model in fenced blocks, one per file,
//     under a fixed template that says so; a line that reads as an instruction, carries a fence or
//     role marker or hides characters is withheld from the prompt and can never be cited.
//   - The model gets no tools: the request type has no place for one, and the template says it
//     cannot act. It answers in a closed JSON schema at temperature zero with a bounded output.
//   - Its reply is untrusted too. Every proposal is checked here before the verifier sees it: only
//     found or partial may be claimed, only for a requirement no rule has decided and a catalog
//     entry that lets a verified model finding stand, and only on lines the model was shown. A
//     proposal that tries to change or restate a rule's status, to claim anything else, to carry a
//     field outside the schema (a removal, say) or to rest on an instruction-like line is dropped
//     and counted as an attempted override. The count goes into the report.
//   - Whatever survives goes to report.RdyBuild, the only path to a report, where the verifier
//     re-reads the evidence, a quote that does not match makes the finding model_unverified, and a
//     rule's status stays final.
//
// # Where the model runs
//
// The model comes in through AssistModel, a thin wrapper over providers.TextGenerator that the CLI
// composition root fills. A local model needs nothing else. An external one is refused unless the
// operator enabled external mode, a guard (AssistEgressGate) was supplied, the project was given a
// class other than local_only and the guard lets the request out; a refusal is permission_denied,
// nothing is sent, and no other provider is tried. This package imports none of internal/modes,
// internal/dataegress or internal/providerreg (TestAssistImportsNoExternalModePackages).
//
// With no model, an unavailable model, a model that fails, or a refusal, the deterministic leg runs
// alone and the report says so (model_leg.status). The output of a leg that failed part-way is
// discarded whole. Any model is labelled "unqualified, advisory" unless the caller says the
// registry shows a qualified status; the Lite controller is uncertified (M0-17, M0-21, M5-09).
//
// Every package-level identifier here starts with assist or Assist.
package assist
