package sdkgo

import (
	"fmt"
	"slices"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Wire types of the /v1 API that the OpenAPI document defines (schemas/api/openapi.yaml and its fragments)
// rather than a JSON Schema, so they are not in pkg/contracts/gen. They are written by hand and kept in step
// with the document by contract_test.go. Member names are the wire's (snake_case via the json tags).
// Everything that has a JSON Schema (TurnEvent, PreparedAction, ActionReceipt, Choice, the RAG search and
// answer documents, SourceStatus, IndexGeneration, Citation) is the generated type, not a copy.

// ---- sessions ----

// PendingField says where a collected field of the pending goal came from.
type PendingField struct {
	Name string `json:"name"`
	// Origin is user_turn, correction, entity_resolution or choice.
	Origin string `json:"origin"`
}

// PendingQuestion is a question the dialogue waits to have answered.
type PendingQuestion struct {
	Field string `json:"field"`
	// AnswerKind is text, choice or entity.
	AnswerKind string           `json:"answer_kind"`
	Choices    []actions.Choice `json:"choices,omitempty"`
}

// PendingState is what the active goal of a session waits for: a confirmation of a prepared action, an
// answer to a clarification, or nothing blocking. Collected values are never returned, only field names. The
// confirmation secret is never part of it: it cannot be recovered after the turn that issued it.
type PendingState struct {
	// Kind is confirmation, clarification or goal.
	Kind            string                  `json:"kind"`
	CapabilityID    string                  `json:"capability_id"`
	Phase           string                  `json:"phase"`
	CollectedFields []PendingField          `json:"collected_fields"`
	MissingFields   []string                `json:"missing_fields,omitempty"`
	Question        *PendingQuestion        `json:"question,omitempty"`
	Preparation     *actions.PreparedAction `json:"preparation,omitempty"`
	ExpiresAt       *time.Time              `json:"expires_at,omitempty"`
}

func (p *PendingState) validate() error {
	if !slices.Contains([]string{"confirmation", "clarification", "goal"}, p.Kind) || p.CapabilityID == "" || p.Phase == "" {
		return fmt.Errorf("pending state: kind, capability_id and phase are required")
	}
	return nil
}

// Session is a session with its structured pending state.
type Session struct {
	SessionID string `json:"session_id"`
	// Status is active or ended.
	Status    string        `json:"status"`
	TurnCount int           `json:"turn_count"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Pending   *PendingState `json:"pending"`
}

func (s *Session) validate() error {
	if s.SessionID == "" || !slices.Contains([]string{"active", "ended"}, s.Status) || s.TurnCount < 0 {
		return fmt.Errorf("session: session_id, status and turn_count are required")
	}
	if s.Pending != nil {
		return s.Pending.validate()
	}
	return nil
}

// SessionEnded is the answer to ending a session.
type SessionEnded struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

func (s *SessionEnded) validate() error {
	if s.SessionID == "" || s.Status != "ended" {
		return fmt.Errorf("session ended: session_id and status ended are required")
	}
	return nil
}

type pendingResponse struct {
	Pending *PendingState `json:"pending"`
}

func (p *pendingResponse) validate() error {
	if p.Pending != nil {
		return p.Pending.validate()
	}
	return nil
}

// ---- turns ----

// MaxTurnAttachments is the most files one text turn may name.
const MaxTurnAttachments = 4

// TurnInput is what a user can send in one turn: exactly one of a text, a choice, a confirmation or a
// rejection. A confirmation is a structured input from the client UI, {preparation_id, token}; no text can
// ever confirm anything. Build one with TextTurn, ChoiceTurn, ConfirmTurn or RejectTurn.
//
// A text turn may name the files the user uploaded for it (WithAttachments: the file ids POST /v1/files returned,
// at most MaxTurnAttachments). The core resolves them together under the turn's scope before anything runs: a file
// that is another tenant's, principal's or session's, one deleted, one expired and one that never existed all fail
// the whole turn with the same scope_violation, no model is called and no turn is recorded.
type TurnInput struct {
	Text         string             `json:"text,omitempty"`
	ChoiceID     string             `json:"choice_id,omitempty"`
	Confirmation *ConfirmationInput `json:"confirmation,omitempty"`
	Reject       *RejectionInput    `json:"reject,omitempty"`
	// Language is a BCP 47 tag the core supports; it applies to a text or a choice only.
	Language string `json:"language,omitempty"`
	// Attachments are the ids of the uploaded files that belong to a text turn, at most MaxTurnAttachments.
	Attachments []string `json:"attachments,omitempty"`
}

// ConfirmationInput presents the single-use confirmation secret of one preparation.
type ConfirmationInput struct {
	PreparationID string `json:"preparation_id"`
	Token         string `json:"token"`
}

// RejectionInput declines one preparation.
type RejectionInput struct {
	PreparationID string `json:"preparation_id"`
}

// TextTurn is a user message.
func TextTurn(text string) TurnInput { return TurnInput{Text: text} }

// ChoiceTurn selects one of the choices of the pending clarification.
func ChoiceTurn(choiceID string) TurnInput { return TurnInput{ChoiceID: choiceID} }

// ConfirmTurn confirms a prepared action through the conversation.
func ConfirmTurn(preparationID, token string) TurnInput {
	return TurnInput{Confirmation: &ConfirmationInput{PreparationID: preparationID, Token: token}}
}

// RejectTurn declines a prepared action through the conversation.
func RejectTurn(preparationID string) TurnInput {
	return TurnInput{Reject: &RejectionInput{PreparationID: preparationID}}
}

// WithLanguage returns the input with a language tag.
func (t TurnInput) WithLanguage(tag string) TurnInput {
	t.Language = tag
	return t
}

// WithAttachments returns the input with the ids of the uploaded files that belong to it (a text turn only).
func (t TurnInput) WithAttachments(fileIDs ...string) TurnInput {
	t.Attachments = append([]string(nil), fileIDs...)
	return t
}

// validate checks a turn input before anything is sent: exactly one of text, choice_id, confirmation and
// reject, with a well-formed id and secret; attachments only with a text, at most MaxTurnAttachments file ids.
func (t TurnInput) validate() error {
	kinds := 0
	for _, set := range []bool{t.Text != "", t.ChoiceID != "", t.Confirmation != nil, t.Reject != nil} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return invalidArgument("input", "exactly one of text, choice_id, confirmation and reject is required")
	}
	if len(t.Attachments) > 0 {
		if t.Text == "" {
			return invalidArgument("attachments", "files go with a text message")
		}
		if len(t.Attachments) > MaxTurnAttachments {
			return invalidArgument("attachments", fmt.Sprintf("a turn carries at most %d files", MaxTurnAttachments))
		}
		for _, id := range t.Attachments {
			if err := patternID("attachments", filePattern, id); err != nil {
				return err
			}
		}
	}
	switch {
	case t.Confirmation != nil:
		if t.Language != "" {
			return invalidArgument("input", "language does not apply to a confirmation")
		}
		if err := pathID("confirmation.preparation_id", t.Confirmation.PreparationID); err != nil {
			return err
		}
		return checkConfirmationToken(t.Confirmation.Token)
	case t.Reject != nil:
		if t.Language != "" {
			return invalidArgument("input", "language does not apply to a rejection")
		}
		return pathID("reject.preparation_id", t.Reject.PreparationID)
	}
	return nil
}

// ConfirmationGrant is the single-use secret that lets the user's client confirm one prepared action. It is
// not a TurnEvent: it arrives in the X-AlixAI-Confirmation-Token header of an event stream, or in the
// `confirmation` member of a JSON turn. Show it to the user's client only and present it once.
type ConfirmationGrant struct {
	PreparationID string `json:"preparation_id"`
	Token         string `json:"token"`
}

// TurnResult is a finished turn: its events in order (the last is `done`) and, when it prepared an action
// that needs the user, the grant.
type TurnResult struct {
	TurnID       string                   `json:"turn_id"`
	Events       []conversation.TurnEvent `json:"events"`
	Confirmation *ConfirmationGrant       `json:"confirmation,omitempty"`
}

// ---- actions ----

// ActionOutcome is the body of a confirmation that reached the host and committed (or queued) the action.
// The OpenAPI document lists both members as optional because the same shape also carries a failure; a 200
// always has the receipt, and the SDK refuses a 200 without one.
type ActionOutcome struct {
	Preparation *actions.PreparedAction `json:"preparation,omitempty"`
	Receipt     *actions.ActionReceipt  `json:"receipt"`
}

func (o *ActionOutcome) validate() error {
	if o.Receipt == nil {
		return fmt.Errorf("action outcome: the receipt is required")
	}
	if o.Preparation != nil {
		if err := checkPrepared(o.Preparation); err != nil {
			return err
		}
	}
	return checkReceipt(o.Receipt)
}

// ---- applications ----

var applicationStatuses = []string{"active", "quarantined", "rejected", "superseded", "not_installed"}

// PackageRecord is an installed application package and its state.
type PackageRecord struct {
	ManifestHash   string `json:"manifest_hash"`
	PackageVersion string `json:"package_version"`
	// State is quarantined, active, rejected or superseded.
	State           string    `json:"state"`
	StatusReason    string    `json:"status_reason,omitempty"`
	SignerKeyID     string    `json:"signer_key_id,omitempty"`
	Warnings        int       `json:"warnings"`
	AuditReportID   string    `json:"audit_report_id,omitempty"`
	InstalledAt     time.Time `json:"installed_at"`
	StatusChangedAt time.Time `json:"status_changed_at"`
}

func (p *PackageRecord) validate() error {
	if p.ManifestHash == "" || p.PackageVersion == "" ||
		!slices.Contains([]string{"quarantined", "active", "rejected", "superseded"}, p.State) || p.Warnings < 0 {
		return fmt.Errorf("package: manifest_hash, package_version, state and warnings are required")
	}
	return nil
}

// Finding is a warning or information from the load checks of a registered package.
type Finding struct {
	CheckID string `json:"check_id"`
	// Severity is warning or info.
	Severity    string `json:"severity"`
	Capability  string `json:"capability,omitempty"`
	Path        string `json:"path,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

// Registration is the answer to registering an application package: it lands in quarantine.
type Registration struct {
	ApplicationID string        `json:"application_id"`
	Package       PackageRecord `json:"package"`
	// Created is true when this call installed the package, false when the same content was already installed.
	Created  bool      `json:"created"`
	Findings []Finding `json:"findings"`
}

func (r *Registration) validate() error {
	if r.ApplicationID == "" {
		return fmt.Errorf("registration: application_id is required")
	}
	return r.Package.validate()
}

// ApplicationHealth is the application's own health report; only read for an active application.
type ApplicationHealth struct {
	// Status is ok, degraded or unavailable.
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	ApplicationVersion string `json:"application_version,omitempty"`
}

// Application is the caller's application with its installed packages and their states.
type Application struct {
	ApplicationID string             `json:"application_id"`
	Status        string             `json:"status"`
	Packages      []PackageRecord    `json:"packages"`
	Health        *ApplicationHealth `json:"health,omitempty"`
}

func (a *Application) validate() error {
	if a.ApplicationID == "" || !slices.Contains(applicationStatuses, a.Status) {
		return fmt.Errorf("application: application_id and status are required")
	}
	for i := range a.Packages {
		if err := a.Packages[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

// Capability is a capability the caller may see, with the effective risk level the policy gateway assigns.
type Capability struct {
	ID                   string   `json:"id"`
	Title                string   `json:"title,omitempty"`
	Description          string   `json:"description,omitempty"`
	RiskLevel            int      `json:"risk_level"`
	RequiresConfirmation bool     `json:"requires_confirmation"`
	RequiresStepUp       bool     `json:"requires_step_up"`
	Fields               []string `json:"fields"`
}

// CapabilityList is the answer to listing the capabilities.
type CapabilityList struct {
	Capabilities []Capability `json:"capabilities"`
}

func (l *CapabilityList) validate() error {
	for _, c := range l.Capabilities {
		if c.ID == "" || c.RiskLevel < 0 || c.RiskLevel > 3 {
			return fmt.Errorf("capability: id and a risk_level from 0 to 3 are required")
		}
	}
	return nil
}

// ---- operations ----

var readinessStatuses = []string{"ok", "degraded", "unavailable"}

// ReadyRole is the readiness of one model role.
type ReadyRole struct {
	Role       string `json:"role"`
	Configured bool   `json:"configured"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Effect     string `json:"effect,omitempty"`
}

// ReadyCheck is one readiness check.
type ReadyCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// ReadyReport is the readiness report of /readyz (200 when ready, 503 when not: both are returned, neither
// is an error).
type ReadyReport struct {
	// Status is ok, degraded or unavailable.
	Status string       `json:"status"`
	Ready  bool         `json:"ready"`
	Roles  []ReadyRole  `json:"roles"`
	Checks []ReadyCheck `json:"checks"`
}

func (r *ReadyReport) validate() error {
	if !slices.Contains(readinessStatuses, r.Status) {
		return fmt.Errorf("readiness: status is required")
	}
	return nil
}

// HealthApplication names the caller's application in the authenticated health report.
type HealthApplication struct {
	ApplicationID string `json:"application_id"`
	Status        string `json:"status"`
}

// Health is the authenticated report of /v1/health: the readiness report plus the caller's application.
type Health struct {
	ReadyReport
	Application HealthApplication `json:"application"`
}

func (h *Health) validate() error {
	if err := h.ReadyReport.validate(); err != nil {
		return err
	}
	if h.Application.ApplicationID == "" || !slices.Contains(applicationStatuses, h.Application.Status) {
		return fmt.Errorf("health: application is required")
	}
	return nil
}

// Liveness is the answer of /healthz.
type Liveness struct {
	Status string `json:"status"`
}

func (l *Liveness) validate() error {
	if l.Status != "ok" {
		return fmt.Errorf("liveness: status ok is required")
	}
	return nil
}

// ---- rag ----

// Source is one registered RAG source: the declaration of the active package it was made from, and its status.
type Source struct {
	// Declaration is the name the application's active package declares the source under.
	Declaration string `json:"declaration"`
	// PackageVersion is the version of the package the registration was last bound to.
	PackageVersion string `json:"package_version"`
	// Serving is true while the source is bound to the active package and may be read.
	Serving bool `json:"serving"`
	// Status is the lifecycle, availability, staleness, counts and serving generation.
	Status rag.SourceStatus `json:"status"`
}

func (s *Source) validate() error {
	if s.Declaration == "" || s.PackageVersion == "" {
		return fmt.Errorf("source: declaration and package_version are required")
	}
	return nil
}

type sourceList struct {
	Sources []Source `json:"sources"`
}

func (l *sourceList) validate() error {
	for i := range l.Sources {
		if err := l.Sources[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

type generationList struct {
	Generations []rag.IndexGeneration `json:"generations"`
}

// ReauditReport counts what a re-audit did.
type ReauditReport struct {
	Checked int `json:"checked"`
	Rebound int `json:"rebound"`
	Queued  int `json:"queued"`
	Removed int `json:"removed"`
	Failed  int `json:"failed"`
}

// SyncAccepted is an accepted sync or rebuild: the durable job, whose progress is the status of the source.
type SyncAccepted struct {
	SourceID actions.SourceID `json:"source_id"`
	JobID    string           `json:"job_id"`
}

func (s *SyncAccepted) validate() error {
	if s.SourceID == "" || s.JobID == "" {
		return fmt.Errorf("sync: source_id and job_id are required")
	}
	return nil
}

// CitationResolution is what the `file` open reference of a citation resolves to, read at the moment it is
// resolved: the state of the source now, the passage that was cited and the way to open the source itself.
type CitationResolution struct {
	SourceID     actions.SourceID                  `json:"source_id"`
	DisplayName  string                            `json:"display_name"`
	Version      actions.OpaqueVersion             `json:"version"`
	Locator      *conversation.CitationLocator     `json:"locator,omitempty"`
	Availability conversation.CitationAvailability `json:"availability"`
	Staleness    conversation.CitationStaleness    `json:"staleness"`
	OpenRef      conversation.CitationOpenRef      `json:"open_ref"`
	// Preview is the passage the citation was grounded on; empty when previews are off.
	Preview   string    `json:"preview,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *CitationResolution) validate() error {
	if c.SourceID == "" || c.DisplayName == "" || c.Version == "" || c.OpenRef.Value == nil || c.ExpiresAt.IsZero() {
		return fmt.Errorf("citation resolution: source_id, display_name, version, open_ref and expires_at are required")
	}
	return nil
}

// Answered returns the grounded answer of a result: a citation for every claim.
func Answered(r rag.AnswerResult) (rag.GroundedAnswer, bool) {
	v, ok := r.Value.(rag.GroundedAnswer)
	return v, ok
}

// NoEvidence returns the typed absence of an answer: no authorised evidence supports one.
func NoEvidence(r rag.AnswerResult) (rag.NoSupportedEvidence, bool) {
	v, ok := r.Value.(rag.NoSupportedEvidence)
	return v, ok
}
