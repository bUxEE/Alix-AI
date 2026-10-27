package loop

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// AbilityFlags is the effective per-ability set after deployment ∧ application ∧
// tenant intersection and analyst_capable gating.
type AbilityFlags struct {
	Loop         bool
	Compute      bool
	Forecasts    bool
	Presentation bool
	Drafts       bool
	Knowledge    bool
	WebSearch    bool
	WebFetch     bool
}

// Effective is what [AbilityResolver] returns for a turn.
type Effective struct {
	// Enabled is true when the master switch and loop can run.
	Enabled bool
	// AnalystCapable mirrors the resolved controller profile flag.
	AnalystCapable bool
	Abilities      AbilityFlags
	// DisabledReasons maps ability id → stable reason codes (deployment_off, …).
	DisabledReasons map[an.AbilityID][]an.AbilitiesReasonListItem
	// Wanted is true when deployment/application/tenant would enable the analyst
	// path but the controller is not analyst_capable (typed reason for the single path).
	Wanted bool
	// WantedReason is set when Wanted is true (usually model_not_analyst_capable).
	WantedReason contracts.ErrCode
}

// AbilityResolver computes the effective ability set for a scope. Nil on Deps
// means analyst is off (1.4 path).
type AbilityResolver interface {
	Resolve(ctx context.Context, scope contracts.Scope, analystCapable bool) (Effective, error)
}

// DomainClassifier decides in/out of domain for an analyst turn. Nil skips the
// check (integrator wires the real one; tests may supply a fake).
type DomainClassifier interface {
	Classify(ctx context.Context, scope contracts.Scope, utterance string) (an.DomainClassification, error)
}

// ComputeResult is what [Computer] returns on success.
type ComputeResult struct {
	Dataset *an.Dataset
	Facts   []an.DerivedFact
}

// Computer runs one deterministic compute op. Typed refusals use contracts.Error.
type Computer interface {
	Run(ctx context.Context, scope contracts.Scope, op an.ComputeOp, datasets map[an.DatasetID]an.Dataset) (ComputeResult, error)
}

// SearchHit is one web search result (untrusted).
type SearchHit struct {
	URL     string
	Title   string
	Snippet string
}

// SearchResult is a web search response.
type SearchResult struct {
	Hits []SearchHit
}

// SearchInput is one web_query search call.
type SearchInput struct {
	Query    string
	TurnID   string
	Datasets []an.Dataset // datasets already read/computed this turn (confidential check)
}

// WebSearcher runs a web_query search. Nil when web_search is off.
type WebSearcher interface {
	Search(ctx context.Context, scope contracts.Scope, in SearchInput) (SearchResult, error)
}

// FetchResult is a fetched page (untrusted, fenced by the loop).
type FetchResult struct {
	Citation an.WebCitation
	Text     string
}

// FetchInput is one fetch of a prior search hit.
type FetchInput struct {
	URL    string
	Index  int
	TurnID string
}

// WebFetcher fetches an allow-listed URL from a prior search hit.
type WebFetcher interface {
	Fetch(ctx context.Context, scope contracts.Scope, in FetchInput) (FetchResult, error)
}

// TurnLifecycle is notified when an analyst turn ends so adapters can drop
// per-turn state (search hits, budgets). Optional.
type TurnLifecycle interface {
	EndTurn(turnID string)
}

// FactSet is the turn's verified fact pool for answer verification.
type FactSet struct {
	Datasets  map[an.DatasetID]an.Dataset
	Facts     []an.DerivedFact
	Citations []an.WebCitation
	Fetched   map[string]string // citation id → fetched text
}

// VerifyAnswerInput is what [AnalystVerifier] receives for an answer step.
type VerifyAnswerInput struct {
	Scope       contracts.Scope
	TextPlan    string
	Blocks      []an.StepProposalBlockPlan
	SourceSpans []an.SourceSpan
	Facts       FactSet
	Stream      bool
	Language    string // BCP-47 tag the turn is answering in (en, it, …)
}

// VerifiedSegment is one incremental emission during verified streaming.
type VerifiedSegment struct {
	Text    string
	Replace bool
	Reason  string
}

// VerifyAnswerResult is a verified answer (or template fallback).
type VerifyAnswerResult struct {
	Text             string
	Verified         bool
	Blocks           []conversation.RichBlock
	CoreMaterialised []bool // parallel to Blocks; set only by the analyst materialiser
	Segments         []VerifiedSegment
}

// AnalystVerifier checks answer text/blocks against the fact set (sibling: verify).
type AnalystVerifier interface {
	VerifyAnswer(ctx context.Context, in VerifyAnswerInput) (VerifyAnswerResult, error)
}

// ReadRequest is a capability read proposed by the model.
type ReadRequest struct {
	CapabilityID string
	Arguments    map[string]any
}

// ReadResult is a dataset produced by a governed read.
type ReadResult struct {
	Dataset an.Dataset
}

// CapabilityReader performs shortlist + policy + tenant scope + adapter Read.
// Implementations must never write. Cross-tenant scope violations are typed errors.
type CapabilityReader interface {
	Read(ctx context.Context, scope contracts.Scope, req ReadRequest) (ReadResult, error)
}

// StepCallInput is one constrained model call for a step proposal.
type StepCallInput struct {
	Scope   contracts.Scope
	Items   []PromptItem
	Schema  []byte // JSON Schema for AnalystStepProposal (abilities-filtered)
	TraceID string
}

// PromptItem is one fenced or trusted message part for the step caller.
type PromptItem struct {
	Role      string // system | user | assistant
	Trusted   bool
	Content   string
	SourceID  string
	DataClass an.DataClass
	LocalOnly bool
}

// StepCallResult is the raw model output for one step.
type StepCallResult struct {
	Raw    []byte
	TokIn  int
	TokOut int
}

// StepCaller issues one constrained-JSON AnalystStepProposal call through the
// existing provider + PromptGuard path. The loop never sends restricted fields.
type StepCaller interface {
	Call(ctx context.Context, in StepCallInput) (StepCallResult, error)
}

// BudgetReservation is one reserved model-call budget unit.
type BudgetReservation struct {
	ID string
}

// BudgetActual is settled usage.
type BudgetActual struct {
	TokIn, TokOut int
	Cancelled     bool
	Failed        bool
}

// Budget reserves and settles per model call (ADR 0046 accounting). Nil skips
// reservation (tests / Local without providerbudget).
type Budget interface {
	Reserve(ctx context.Context, scope contracts.Scope, estToks int) (BudgetReservation, error)
	Settle(ctx context.Context, scope contracts.Scope, r BudgetReservation, a BudgetActual) error
}

// Progress publishes 1.5 analyst_* stages. The emit sibling maps them for old clients.
type Progress interface {
	Publish(ctx context.Context, scope contracts.Scope, turnID string, stage conversation.ProgressEventStage)
}

// IDSource mints dataset / fact / citation / trace ids. Nil uses a local counter (tests).
type IDSource interface {
	New(prefix string) string
}
