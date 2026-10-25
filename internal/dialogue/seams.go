package dialogue

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// This file holds the seams of the pipeline: small interfaces that the engine
// calls and a composition root may replace. Each has a default that keeps the
// pipeline exactly what it is without the feature behind it.

// ---- package source ----

// PackageSource resolves the typed application package a turn is served from:
// the same active package the capability registry and the action coordinator
// decide against. internal/capabilities.Registry does not export the compiled
// package, so the composition root backs this with the same Source it gives the
// registry (and the coordinator's own PackageSource). The engine checks the
// package it gets: it must be active, belong to the verified application and, when
// a registry is configured, be the content the registry routed against.
type PackageSource interface {
	Package(ctx context.Context, applicationID string) (capabilities.Package, error)
}

// PackageSourceFunc adapts a function to PackageSource.
type PackageSourceFunc func(ctx context.Context, applicationID string) (capabilities.Package, error)

// Package calls f.
func (f PackageSourceFunc) Package(ctx context.Context, applicationID string) (capabilities.Package, error) {
	return f(ctx, applicationID)
}

// ---- providers: ProviderSelector and PromptGuard ----

// Provider is the text generator chosen for a role, with what a guard needs to
// know about it. The Local mode provider is the one configured local runtime.
type Provider struct {
	// Name identifies the provider in logs and audit records.
	Name string
	// Local is true for the operator's own local runtime. Anything else is an
	// external provider, which the default guard refuses.
	Local bool
	// Generator serves the role. It should be wrapped with providers.WithBudget
	// (LocalSelector does).
	Generator providers.TextGenerator
}

// ProviderSelector picks the provider for a role (external mode additive plan).
// The default, LocalSelector, returns the one configured local provider.
// A selector that returns an error means no model is available for the role: the
// turn fails closed (or, for the response, falls back to the template) and never
// silently tries another provider.
type ProviderSelector interface {
	Select(ctx context.Context, scope contracts.Scope, role providers.Role) (Provider, error)
}

type localSelector struct{ p Provider }

// LocalSelector returns the default selector: it always returns the one local
// provider, wrapped with the request budget (providers.WithBudget), so an
// oversize request never reaches the runtime and a budget violation is
// validation_failed.
func LocalSelector(gen providers.TextGenerator, budget providers.Budget) (ProviderSelector, error) {
	g, err := providers.WithBudget(gen, budget)
	if err != nil {
		return nil, err
	}
	return localSelector{p: Provider{Name: "local", Local: true, Generator: g}}, nil
}

func (s localSelector) Select(context.Context, contracts.Scope, providers.Role) (Provider, error) {
	return s.p, nil
}

// ProviderInfo is what a PromptGuard learns of the provider a prompt is for.
type ProviderInfo struct {
	Name  string
	Local bool
}

// DataClass is the sensitivity of a piece of context, from the external provider
// mode plan: public < internal < confidential < restricted. An empty or unknown
// class is treated as the most restrictive.
type DataClass string

// Data classes.
const (
	ClassPublic       DataClass = "public"
	ClassInternal     DataClass = "internal"
	ClassConfidential DataClass = "confidential"
	ClassRestricted   DataClass = "restricted"
)

// Rank orders the classes; an unknown class ranks as restricted.
func (c DataClass) Rank() int {
	switch c {
	case ClassPublic:
		return 0
	case ClassInternal:
		return 1
	case ClassConfidential:
		return 2
	}
	return 3
}

// ItemKind says what a piece of context is.
type ItemKind string

// The kinds of context item the prompt builder places.
const (
	ItemFrame       ItemKind = "frame"         // system frame, rules, capability catalogue
	ItemState       ItemKind = "session_state" // goal, slots, last directive
	ItemTurn        ItemKind = "recent_turn"
	ItemUserMessage ItemKind = "user_message"
	ItemDirective   ItemKind = "directive" // the directive a response is written from
	// ItemClientContext is the one message of what the client's screen supplied (readable context, UI tool descriptions).
	ItemClientContext ItemKind = "client_context"
)

// ItemMeta is the internal metadata every context item carries. It is for the
// PromptGuard (and for audit); it is stripped before the request is sent, and the
// Local path ignores it.
type ItemMeta struct {
	Kind ItemKind
	// SourceID names where the content came from ("package", "turn:<id>",
	// "session", "directive:<id>"); never a credential or a record body.
	SourceID string
	// Class is the data classification of the content.
	Class DataClass
	// LocalOnly marks content that must never leave the machine.
	LocalOnly bool
}

// ContextItem is one message of a prompt with its internal metadata.
type ContextItem struct {
	Role    providers.MessageRole
	Trusted bool
	Content string
	Meta    ItemMeta
}

// PromptRequest is a built prompt before it is sent: the context items with
// their metadata, the output constraint and the generation limits.
type PromptRequest struct {
	Role        providers.Role
	Items       []ContextItem
	Output      providers.OutputConstraint
	MaxTokens   int
	Temperature float64
}

// ChatRequest strips the internal metadata and returns the request that is sent.
func (p PromptRequest) ChatRequest() providers.ChatRequest {
	msgs := make([]providers.Message, len(p.Items))
	for i, it := range p.Items {
		msgs[i] = providers.Message{Role: it.Role, Content: it.Content, Trusted: it.Trusted}
	}
	return providers.ChatRequest{
		Role: p.Role, Messages: msgs, Output: p.Output, MaxTokens: p.MaxTokens, Temperature: p.Temperature,
	}
}

// GuardInput is what a PromptGuard sees: the verified scope, the provider the
// prompt is for and the built prompt with its item metadata.
type GuardInput struct {
	Scope    contracts.Scope
	Provider ProviderInfo
	Request  PromptRequest
}

// PromptGuard is called on every provider request after the prompt is built and
// before it is sent (external mode additive plan). It returns the request that
// may be sent, which may be filtered, or an error, which is a typed denial and
// ends the call: nothing is sent. The default allows everything for the local
// provider and denies any other provider, because only a real guard may release
// context to one.
type PromptGuard interface {
	Guard(ctx context.Context, in GuardInput) (PromptRequest, error)
}

// LocalOnlyGuard is the default PromptGuard: a no-op for the local provider.
// For any other provider it denies with permission_denied.
type LocalOnlyGuard struct{}

// Guard implements PromptGuard.
func (LocalOnlyGuard) Guard(_ context.Context, in GuardInput) (PromptRequest, error) {
	if !in.Provider.Local {
		return PromptRequest{}, contracts.New(contracts.ErrPermissionDenied, "no prompt guard is configured for a provider that is not local")
	}
	return in.Request, nil
}

// ---- decision stage ----

// FallbackReason is why the decision stage gave no selection. The values are the
// stable strings of the decision layer (docs/architecture/decision-layer.md).
type FallbackReason string

// Fallback reasons. FallbackNone means the stage selected a candidate.
const (
	FallbackNone                FallbackReason = ""
	FallbackDisabled            FallbackReason = "disabled"
	FallbackUnavailable         FallbackReason = "unavailable"
	FallbackTimeout             FallbackReason = "timeout"
	FallbackMalformedOutput     FallbackReason = "malformed_output"
	FallbackLowConfidence       FallbackReason = "low_confidence"
	FallbackCalibrationFailed   FallbackReason = "calibration_failed"
	FallbackOODUnsupportedShape FallbackReason = "ood_unsupported_shape"
	FallbackModelMismatch       FallbackReason = "model_mismatch"
	FallbackTenantMismatch      FallbackReason = "tenant_mismatch"
	FallbackPolicyConflict      FallbackReason = "policy_conflict"
	FallbackCandidateSetReject  FallbackReason = "candidate_set_rejected"
	FallbackResourceGate        FallbackReason = "resource_gate"
	FallbackUnsupportedRuntime  FallbackReason = "unsupported_runtime"
)

// Valid reports whether r is one of the stable reasons (FallbackNone included).
func (r FallbackReason) Valid() bool {
	switch r {
	case FallbackNone, FallbackDisabled, FallbackUnavailable, FallbackTimeout, FallbackMalformedOutput,
		FallbackLowConfidence, FallbackCalibrationFailed, FallbackOODUnsupportedShape, FallbackModelMismatch,
		FallbackTenantMismatch, FallbackPolicyConflict, FallbackCandidateSetReject, FallbackResourceGate,
		FallbackUnsupportedRuntime:
		return true
	}
	return false
}

// DecisionCandidate is one authorised candidate the decision stage may score. It
// carries only what the policy gateway exposed and what the router found: never
// field values, never RAG text.
type DecisionCandidate struct {
	CapabilityID string
	// Risk is the effective risk level the policy gateway decided.
	Risk contracts.RiskLevel
	// Score is the router's ranking score (a ranking, not a permission).
	Score float64
	// Signals are the kinds of evidence behind the router's score.
	Signals []capabilities.SignalKind
}

// DecisionRequest is the bounded input of the decision stage: the verified scope,
// the user's utterance, the language, the active goal as the router reads it and
// the authorised candidate set, at most Config.MaxShortlist entries.
type DecisionRequest struct {
	Scope      contracts.Scope
	Utterance  string
	Language   string
	Goal       *capabilities.ActiveGoal
	Candidates []DecisionCandidate
}

// DecisionResult is the stage's ADVISORY answer: either a selected candidate or a
// typed fallback reason. A selection is a ranking decision about which
// interpretation path to try first; it authorises nothing. The pipeline still
// asks the policy gateway, the deterministic adapter, the prepare / confirm /
// commit path and the response verifier, exactly as without a stage.
type DecisionResult struct {
	// Selected is the capability the stage prefers; nil means no selection.
	Selected *DecisionSelection
	// Fallback says why there is no selection. It is FallbackNone exactly when
	// Selected is set; a result that sets both or neither is treated as
	// malformed_output.
	Fallback FallbackReason
}

// DecisionSelection is a selected candidate with its calibrated confidence. The
// confidence is advisory and never read as authority.
type DecisionSelection struct {
	CapabilityID string
	Confidence   float64
}

// DecisionStage is the optional decision-layer seam (master plan 8.5). It is
// called after the deterministic rules and the policy-exposed capability shortlist
// and before context assembly and the controller. It receives the bounded
// authorised candidate set and returns a selected candidate or a typed fallback
// reason. It never sees RAG text, and it can never skip prepare, confirm, commit,
// a policy check or the response verifier: the engine does not give it a way to.
type DecisionStage interface {
	Decide(ctx context.Context, req DecisionRequest) DecisionResult
}

// NoopDecisionStage is the default: it always falls back, so the pipeline is
// identical to one without a stage.
type NoopDecisionStage struct{}

// Decide implements DecisionStage.
func (NoopDecisionStage) Decide(context.Context, DecisionRequest) DecisionResult {
	return DecisionResult{Fallback: FallbackDisabled}
}

// ---- rate limiting ----

// RateLimiter is the scope and rate check at the start of a turn. A refusal is
// rate_limited (retryable) and ends the turn before any other work.
type RateLimiter interface {
	Allow(ctx context.Context, scope contracts.Scope) error
}

// RateConfig bounds a WindowLimiter.
type RateConfig struct {
	// MaxTurns is how many turns one principal may start per Window.
	MaxTurns int
	// Window is the length of the sliding window.
	Window time.Duration
	// MaxKeys bounds how many principals the limiter tracks at once.
	MaxKeys int
}

// Defaults of RateConfig.
const (
	DefaultRateMaxTurns = 60
	DefaultRateWindow   = time.Minute
	DefaultRateMaxKeys  = 4096
)

// WindowLimiter is the default RateLimiter: a sliding window per principal of an
// application, tenant and workspace, in memory, on the injected clock. Quotas that
// outlive the process belong to internal/quotas; this keeps one runaway client from
// starving the model for the others.
type WindowLimiter struct {
	clk clock.Clock
	cfg RateConfig

	mu   sync.Mutex
	hits map[string][]time.Time
}

// NewWindowLimiter returns a limiter. A zero field takes its default.
func NewWindowLimiter(clk clock.Clock, cfg RateConfig) *WindowLimiter {
	if clk == nil {
		clk = clock.Real{}
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = DefaultRateMaxTurns
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultRateWindow
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = DefaultRateMaxKeys
	}
	return &WindowLimiter{clk: clk, cfg: cfg, hits: map[string][]time.Time{}}
}

// Allow implements RateLimiter.
func (l *WindowLimiter) Allow(_ context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s", scope.ApplicationID, scope.TenantID, scope.WorkspaceID, scope.PrincipalID)
	now := l.clk.Now()
	cutoff := now.Add(-l.cfg.Window)

	l.mu.Lock()
	defer l.mu.Unlock()
	hits := l.hits[key][:0:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			hits = append(hits, t)
		}
	}
	if len(hits) >= l.cfg.MaxTurns {
		l.hits[key] = hits
		retry := hits[0].Add(l.cfg.Window).Sub(now)
		return contracts.New(contracts.ErrRateLimited, "too many turns; slow down").
			WithDetails("retry_after_ms", retry.Milliseconds())
	}
	if _, known := l.hits[key]; !known && len(l.hits) >= l.cfg.MaxKeys {
		l.evict(cutoff)
		if len(l.hits) >= l.cfg.MaxKeys {
			return contracts.New(contracts.ErrResourceExhausted, "the rate limiter is full; try again shortly")
		}
	}
	l.hits[key] = append(hits, now)
	return nil
}

// evict drops every key with no hit inside the window. The caller holds the lock.
func (l *WindowLimiter) evict(cutoff time.Time) {
	for k, hs := range l.hits {
		live := false
		for _, t := range hs {
			if t.After(cutoff) {
				live = true
				break
			}
		}
		if !live {
			delete(l.hits, k)
		}
	}
}
