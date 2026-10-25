// Package verify is the evidence verifier of integration readiness discovery (master plan 9.5,
// ADR 0037): the one authority that decides whether a finding stands. A model only proposes; this
// package re-opens every cited file under the scanned root, re-hashes it, re-reads the cited line
// span and, for a model proposal, checks that the quoted text is really there. A finding whose
// evidence does not confirm is demoted to unverified with a reason from a closed vocabulary, a
// finding with no evidence can never be found, partial or contradicted, and evidence outside the
// root, outside the scan's own inventory or on a secret file is refused without the file being
// opened.
//
// The origin of a finding is decided by which slice it arrived in (deterministic findings from the
// scanner, proposals from a model), never by the Source field it carries, and the category and
// check id are taken from the catalog, never from the proposal. The package imports only the
// standard library, the generated readiness types, the catalog and the scanner (for the line rule
// and the redactor); it has no network and no process execution.
//
// Every package-level identifier here starts with rdy or Rdy.
package verify

import "github.com/buxee/alix-ai/pkg/contracts/gen/readiness"

// RdyReason is why a finding or one of its evidence entries did not confirm. The vocabulary is
// closed and carries no free text, so no raw error, absolute path or file content reaches a report.
type RdyReason string

// The reasons.
const (
	RdyReasonNoEvidence       RdyReason = "no_evidence"
	RdyReasonTooMuchEvidence  RdyReason = "too_much_evidence"
	RdyReasonOutsideRoot      RdyReason = "path_outside_root"
	RdyReasonNotScanned       RdyReason = "file_not_scanned"
	RdyReasonSecretFile       RdyReason = "secret_file"
	RdyReasonLink             RdyReason = "link_refused"
	RdyReasonUnreadable       RdyReason = "file_unreadable"
	RdyReasonTooLarge         RdyReason = "file_over_size_cap"
	RdyReasonChanged          RdyReason = "file_changed"
	RdyReasonSpan             RdyReason = "span_not_in_file"
	RdyReasonQuoteMissing     RdyReason = "quote_missing"
	RdyReasonQuoteMismatch    RdyReason = "quote_not_in_span"
	RdyReasonClaimNotAllowed  RdyReason = "claim_not_allowed_for_a_model"
	RdyReasonUnknownReq       RdyReason = "unknown_requirement"
	RdyReasonDuplicate        RdyReason = "duplicate_proposal"
	RdyReasonTooManyProposals RdyReason = "too_many_proposals"
)

// Valid reports whether r is one of the closed vocabulary.
func (r RdyReason) Valid() bool { _, ok := rdyReasonText[r]; return ok }

// Text is the English sentence recorded as the finding's unverified reason.
func (r RdyReason) Text() string {
	if t, ok := rdyReasonText[r]; ok {
		return t
	}
	return "The evidence did not confirm."
}

var rdyReasonText = map[RdyReason]string{
	RdyReasonNoEvidence:       "The finding cites no evidence, so it cannot stand as covered.",
	RdyReasonTooMuchEvidence:  "The finding cites more evidence entries than a finding may carry.",
	RdyReasonOutsideRoot:      "The cited path is not a plain path inside the scanned directory.",
	RdyReasonNotScanned:       "The cited file is not one the scan recorded and read.",
	RdyReasonSecretFile:       "The cited file looks like a secret file, which readiness never reads.",
	RdyReasonLink:             "The cited path goes through a link, which readiness never follows.",
	RdyReasonUnreadable:       "The cited file could not be read again.",
	RdyReasonTooLarge:         "The cited file is over the size cap, so it was not re-read.",
	RdyReasonChanged:          "The cited file no longer has the hash it had when it was scanned.",
	RdyReasonSpan:             "The cited line span does not exist in the file.",
	RdyReasonQuoteMissing:     "A model proposal must quote the text it relies on, and none was quoted.",
	RdyReasonQuoteMismatch:    "The quoted text is not in the cited line span.",
	RdyReasonClaimNotAllowed:  "A model may only propose found or partial, never another status.",
	RdyReasonUnknownReq:       "The proposal names a requirement the catalog does not have.",
	RdyReasonDuplicate:        "The proposal repeats an earlier one.",
	RdyReasonTooManyProposals: "The proposal is past the number of proposals a scan accepts.",
}

// rdyOrigin is which slice a finding arrived in.
type rdyOrigin int

const (
	rdyOriginRule rdyOrigin = iota + 1
	rdyOriginModel
)

// RdyVerified is a finding after verification. Only the verifier makes one: its fields are not
// exported, so nothing downstream can mark a finding confirmed. Finding carries the verified
// status; Claimed keeps what the finding said before verification, for the record only.
type RdyVerified struct {
	finding   readiness.Finding
	claimed   readiness.Status
	origin    rdyOrigin
	confirmed bool
	reasons   []RdyReason
}

// Finding is the finding with its status as the verifier decided it.
func (v RdyVerified) Finding() readiness.Finding { return v.finding }

// Claimed is the status the finding claimed before verification. A report never rests on it.
func (v RdyVerified) Claimed() readiness.Status { return v.claimed }

// Confirmed reports that every claim the finding makes held up against the files.
func (v RdyVerified) Confirmed() bool { return v.confirmed }

// FromModel reports whether the finding was a model proposal.
func (v RdyVerified) FromModel() bool { return v.origin == rdyOriginModel }

// Reasons are why the finding did not confirm, sorted, empty when it did.
func (v RdyVerified) Reasons() []RdyReason { return append([]RdyReason(nil), v.reasons...) }

// RdyDropped is a proposal that was not turned into a finding at all.
type RdyDropped struct {
	Index  int
	Reason RdyReason
}
