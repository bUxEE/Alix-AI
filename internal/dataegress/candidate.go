package dataegress

import (
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PartKind is what a part of a message holds.
type PartKind string

// Part kinds. Media is carried by reference to a scoped file, never inline.
const (
	PartText  PartKind = "text"
	PartImage PartKind = "image"
	PartFile  PartKind = "file"
	PartAudio PartKind = "audio"
)

// Origin is the application, tenant and workspace a piece of content was read
// from. Retrieved evidence carries it so that one envelope can never mix tenants.
type Origin struct {
	ApplicationID string
	TenantID      string
	WorkspaceID   string
}

// Part is one piece of a candidate message with the internal metadata the guard
// decides on. The metadata is stripped before anything is sent. Every decision is
// made on the metadata and the policy; the text is data and never an instruction,
// so nothing written in it (by a user, a document or a model) can change an outcome.
type Part struct {
	Kind PartKind

	// Text is the content of a text part.
	Text string
	// FileID and SizeBytes name the scoped file a media part refers to.
	FileID    string
	SizeBytes int64

	// Category says what the data is: one of the sendable categories
	// (providers.SendCategory) or, for content that must be removed, one of the
	// blocked categories (providers.BlockedCategory: credential, raw_database_content,
	// filesystem_path, database_handle, mcp_url, local_only_field). A part whose
	// category is empty or unknown is unclassified and denies the request.
	Category string
	// Class is the data classification: public, internal, confidential or
	// restricted. Empty or unknown is the most restrictive and denies the request.
	Class string

	// SourceID names the RAG source of an excerpt or metadata part.
	SourceID string
	// Label is what may leave in place of the content when only metadata of the
	// source may (a title, an id, a status).
	Label string

	// LocalOnly marks a field that must never leave: the part is removed.
	LocalOnly bool
	// Required marks evidence the answer cannot be given without. If policy keeps it
	// out, the request is denied with the source reason, so the caller can report that
	// the selected provider cannot access the required evidence, instead of the
	// provider answering without it.
	Required bool
	// Origin, when set, must equal the request scope.
	Origin *Origin
}

// Message is one message of the candidate request.
type Message struct {
	Role  providers.RequestEnvelopeMessageRole
	Parts []Part
}

// Candidate is a request as the pipeline built it, before the guard: the verified
// scope, the provider profile it is destined for and the content with its metadata.
type Candidate struct {
	// Scope is the verified context of the request (additive plan 6.2 step 1).
	Scope contracts.Scope
	// Profile is the registry profile of the exact provider and model.
	Profile providers.ProviderProfile
	Role    providers.Role
	Channel providers.Channel
	TraceID string
	// ReservationID is the budget reservation held for the call, or empty.
	ReservationID string

	// Instructions are written by the core from fixed templates.
	Instructions string
	Messages     []Message
	// Tools are the allowlisted tool schemas the policy gateway exposed (the minimum
	// capability set, step 2). The model can only propose a call.
	Tools          []providers.RequestEnvelopeToolSpec
	ResponseFormat providers.RequestEnvelopeResponseFormat
	MaxOutputToks  int64
	TimeoutMS      int64
}

// PartOutcome says what became of one candidate part, so the caller can map the
// filtered envelope back onto the items it holds. Why is a fixed code, never text.
type PartOutcome struct {
	Message, Part int
	Kept          bool
	// EnvelopeMessage and EnvelopePart are the position in the envelope, or -1.
	EnvelopeMessage, EnvelopePart int
	// Why is empty for a part that was kept as it was, else why it was removed,
	// cut or rewritten (local_only_field, blocked_category, source_local_only,
	// source_denied, source_class_not_allowed, source_provider_not_allowed,
	// evidence_budget_exceeded, files_not_allowed, audio_not_allowed, empty_content, metadata_only, truncated, scrubbed).
	Why string
}

// Result is what the guard returns.
type Result struct {
	// Decision is the audited record of the decision. It is set for a denial too.
	Decision providers.EgressDecision
	// Envelope is the filtered request an adapter may send, or nil on a denial.
	Envelope *providers.RequestEnvelope
	// Disclosure is what the host may show before the data leaves (additive plan 6.3).
	Disclosure providers.EgressDisclosure
	// Parts says what became of every candidate part.
	Parts []PartOutcome
}
