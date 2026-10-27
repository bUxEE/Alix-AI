package dialogue

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// Defaults of Config. The token budgets are not here: they are the context profile's
// (Config.Profile, Lite by default; internal/dialogue/context and
// docs/operations/resource-budgets.md section 6.2). Token figures are estimates made with
// providers.EstimateTokens (one token per three bytes), which overestimates on purpose.
const (
	DefaultMaxTerms        = 6
	DefaultMaxSynonyms     = 3
	DefaultMaxChoices      = 5
	DefaultMaxFacts        = 24
	DefaultDecisionTimeout = 250 * time.Millisecond
	DefaultLanguage        = "en"
)

// Config bounds the engine. A zero field takes its default.
type Config struct {
	// DefaultLanguage is the response language when the host gives none and the
	// package does not choose one ("en" or "it").
	DefaultLanguage string
	// Profile is the context budget of the deployment profile: the hard cap on the
	// estimated prompt (the output constraint included), the generated tokens of each
	// model call, the shortlist a prompt may describe, how much of the user's text is
	// read, the caps of the output constraint and of the response prompt, and how many
	// turn rows, capsules and goals set aside the history part of a prompt may hold, with
	// a token cap of its own (internal/dialogue/context). The zero value takes
	// dctx.Lite(); dctx.ProfileByName selects another and is an error for a name it does
	// not know. It is the one place these numbers are set: the engine has no limit of its
	// own beside it. A profile that is not valid is an error from New.
	Profile dctx.Profile
	// MaxTerms and MaxSynonyms bound the terminology lines of the prompt.
	MaxTerms    int
	MaxSynonyms int
	// MaxChoices bounds the choices of a clarification.
	MaxChoices int
	// MaxFacts bounds the facts of a directive.
	MaxFacts int
	// DecisionTimeout bounds the optional decision stage.
	DecisionTimeout time.Duration
	// TemplateOnly lists the dialogue acts whose response is always the
	// deterministic template, with no model call (refusals, failures, step-up,
	// cancellation, no-evidence: nothing for a model to add, and nothing to risk).
	// Nil takes the default; an empty non-nil slice means every act may be
	// verbalised by the model.
	TemplateOnly []conversation.DialogueDirectiveAct
	// RouterOptions are passed to capabilities.NewRouter when Deps.Router is nil:
	// the composition root passes WithEmbedder / WithProviders / WithConfig here.
	RouterOptions []capabilities.RouterOption
	// StreamVerified, when true, realises replies through ChatStream where the provider
	// supports it and emits each verified sentence as soon as it passes the incremental
	// verifier (ADR 0046). Default false: keeps 1.3 whole-reply behaviour. Nothing
	// unverified is ever emitted; a failing segment replaces shown text with the template.
	StreamVerified bool
}

func defaultTemplateOnly() []conversation.DialogueDirectiveAct {
	return []conversation.DialogueDirectiveAct{
		conversation.DialogueDirectiveActRefuse,
		conversation.DialogueDirectiveActReportFailure,
		conversation.DialogueDirectiveActRequestStepUp,
		conversation.DialogueDirectiveActAcknowledgeCancel,
		conversation.DialogueDirectiveActReportNoEvidence,
	}
}

func (c Config) normalized() Config {
	if c.DefaultLanguage == "" {
		c.DefaultLanguage = DefaultLanguage
	}
	setInt := func(p *int, d int) {
		if *p <= 0 {
			*p = d
		}
	}
	if c.Profile == (dctx.Profile{}) {
		c.Profile = dctx.Lite()
	}
	setInt(&c.MaxTerms, DefaultMaxTerms)
	setInt(&c.MaxSynonyms, DefaultMaxSynonyms)
	setInt(&c.MaxChoices, DefaultMaxChoices)
	setInt(&c.MaxFacts, DefaultMaxFacts)
	if c.DecisionTimeout <= 0 {
		c.DecisionTimeout = DefaultDecisionTimeout
	}
	if c.TemplateOnly == nil {
		c.TemplateOnly = defaultTemplateOnly()
	}
	return c
}

// Deps is what an Engine is built from. The composition root wires concrete
// providers and adapters; the engine sees interfaces only.
type Deps struct {
	// Sessions is the durable session store.
	Sessions *sessions.Store
	// Registry is the capability registry of active packages. Router is built from
	// it with the policy gateway unless Router is given.
	Registry *capabilities.Registry
	Router   *capabilities.Router
	// Packages resolves the typed package a turn is served from (see PackageSource).
	Packages PackageSource
	// Policy is the policy gateway. It decides what the model may be shown and
	// whether a proposed capability may be used; it runs again, inside the
	// coordinator, at prepare and commit.
	Policy *policy.Gateway
	// Actions is the prepare / confirm / commit coordinator: the only path to an
	// adapter write, and the path of every governed read.
	Actions *actions.Coordinator
	// Adapters is used by the default entity resolver (AdapterResolver). It must
	// be the same wrapped adapter source the coordinator uses.
	Adapters actions.AdapterSource
	// Providers picks the text generator for a role. Use LocalSelector for Local mode.
	Providers ProviderSelector
	// Audit receives one summary event per turn (identifiers, act, code and prompt
	// sizes; never text). Optional: nil means no turn events. The consequential
	// events (policy decisions, the action lifecycle) are written by the gateway
	// and the coordinator regardless.
	Audit audit.Appender
	// Clock is the only source of time and of IDs. Nil means clock.Real.
	Clock clock.Clock
	// Config bounds the engine; the zero value takes the defaults.
	Config Config
	// Logger receives stage outcomes: identifiers, codes and sizes, never text.
	// Nil discards.
	Logger *slog.Logger

	// The optional seams. A nil value takes the default described on the interface.
	Decision DecisionStage
	Guard    PromptGuard
	Limiter  RateLimiter
	Resolver EntityResolver
	// Detectors decide the bounded control utterances (help, cancel, restart, resume,
	// reject, confirm, correct). A nil member takes the rule detector (RuleDetectors);
	// the optional decision layer may supply its own for any of them. Whatever decides,
	// the transitions are the engine's.
	Detectors Detectors
	// Knowledge answers a text request no capability of the application handles from the
	// sources the principal may read (see Knowledge). Nil: such a request is the refusal
	// unknown_capability, as it always was. A deployment whose engine is built before the
	// RAG area is provided gives it with Engine.SetKnowledge instead.
	Knowledge Knowledge
	// UITools is the host's allow-list of UI-only tools per application (see UIToolRegistry). Nil registers none: the
	// model is never offered a UI tool and a proposal that names one is refused.
	UITools UIToolRegistry
	// OnReply, when set, sees the verified reply of every acknowledged turn (see ReplyHook). Nil: no hook, and the
	// engine behaves exactly as before.
	OnReply ReplyHook
	// OnVerifiedSegment, when set with Config.StreamVerified, receives each verified sentence (or a
	// replace+template step) during realisation (ADR 0046). Nil: segments are still verified but not
	// pushed mid-turn; Outcome.Streamed is still set when streaming ran.
	OnVerifiedSegment VerifiedSegmentHook
}

// Engine is the dialogue engine: the turn pipeline of master plan 11.1. It is safe
// for concurrent use; turns of one session are serialised.
type Engine struct {
	sessions  *sessions.Store
	router    *capabilities.Router
	registry  *capabilities.Registry
	packages  PackageSource
	policy    *policy.Gateway
	actions   *actions.Coordinator
	providers ProviderSelector
	audit     audit.Appender
	clk       clock.Clock
	ids       *ids.Generator
	cfg       Config
	log       *slog.Logger
	// builder builds every proposal prompt within the profile's budget; budget is the
	// profile's request budget (what a guard may filter a prompt down to, never past) and
	// respBudget the smaller one the response prompt is built against.
	builder    *dctx.Builder
	budget     providers.Budget
	respBudget providers.Budget

	decision  DecisionStage
	guard     PromptGuard
	limiter   RateLimiter
	resolver  EntityResolver
	detect    Detectors
	uiTools   UIToolRegistry
	onReply   ReplyHook
	onSegment VerifiedSegmentHook
	// knowledge is the Knowledge of the engine (Deps.Knowledge, Engine.SetKnowledge): read by
	// every turn and set at composition time, so it is an atomic value and a turn takes no
	// lock to read it.
	knowledge atomic.Pointer[knowledgeSlot]
	// memory is long-term memory (ADR 0042); nil when disabled. Set via SetMemory.
	memory atomic.Pointer[MemoryStore]

	locks keyedLocks
}

// New builds an Engine. Every dependency a turn cannot be served without is
// required: an engine without a policy gateway, a coordinator or a provider could
// only fail open, so it cannot be built.
func New(d Deps) (*Engine, error) {
	need := func(ok bool, what string) error {
		if ok {
			return nil
		}
		return contracts.New(contracts.ErrValidationFailed, "dialogue: Deps."+what+" is required")
	}
	for _, c := range []struct {
		ok   bool
		what string
	}{
		{d.Sessions != nil, "Sessions"},
		{d.Packages != nil, "Packages"},
		{d.Policy != nil, "Policy"},
		{d.Actions != nil, "Actions"},
		{d.Providers != nil, "Providers"},
		{d.Registry != nil || d.Router != nil, "Registry or Deps.Router"},
	} {
		if err := need(c.ok, c.what); err != nil {
			return nil, err
		}
	}
	cfg := d.Config.normalized()
	builder, err := dctx.New(cfg.Profile)
	if err != nil {
		return nil, err
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	lg := d.Logger
	if lg == nil {
		lg = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	router := d.Router
	if router == nil {
		// The router the engine builds reads the same bounds the prompt is built within: a
		// shortlist the prompt could not describe, or text the engine would clip, is not
		// worth ranking. The composition root's own options come after and win.
		opts := append([]capabilities.RouterOption{capabilities.WithConfig(capabilities.Config{
			MaxShortlist: cfg.Profile.MaxShortlist, MaxUtteranceRunes: cfg.Profile.MaxUtteranceRunes,
		})}, cfg.RouterOptions...)
		if router, err = capabilities.NewRouter(d.Registry, d.Policy, opts...); err != nil {
			return nil, err
		}
	}
	e := &Engine{
		sessions: d.Sessions, router: router, registry: d.Registry, packages: d.Packages, policy: d.Policy,
		actions: d.Actions, providers: d.Providers, audit: d.Audit, clk: clk, ids: ids.NewGenerator(clk),
		cfg: cfg, log: lg,
		builder: builder, budget: builder.Budget(), respBudget: cfg.Profile.ResponseBudget(),
		decision: d.Decision, guard: d.Guard, limiter: d.Limiter, resolver: d.Resolver, detect: d.Detectors.withDefaults(), uiTools: d.UITools, onReply: d.OnReply, onSegment: d.OnVerifiedSegment,
		locks: keyedLocks{m: map[string]*lockEntry{}},
	}
	e.SetKnowledge(d.Knowledge)
	if e.decision == nil {
		e.decision = NoopDecisionStage{}
	}
	if e.guard == nil {
		e.guard = LocalOnlyGuard{}
	}
	if e.limiter == nil {
		e.limiter = NewWindowLimiter(clk, RateConfig{})
	}
	if e.resolver == nil {
		if d.Adapters == nil {
			return nil, need(false, "Adapters (or Deps.Resolver)")
		}
		e.resolver = AdapterResolver(d.Adapters)
	}
	return e, nil
}

// keyedLocks serialises the turns of one session. A session's state is read,
// changed and written by a turn, and a turn may span a model call: two concurrent
// turns of the same session would race on the goal. (The state write also compares
// versions, so a second process cannot overwrite silently either.)
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

// lock takes the lock of key, waiting for earlier turns of the session, or gives up
// when ctx ends. The returned function releases it.
func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	e := k.m[key]
	if e == nil {
		e = &lockEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	acquired := make(chan struct{})
	go func() {
		e.mu.Lock()
		close(acquired)
	}()
	release := func() {
		e.mu.Unlock()
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
	select {
	case <-acquired:
		return release, nil
	case <-ctx.Done():
		// The goroutine will take the lock when it frees; release it straight away.
		go func() {
			<-acquired
			release()
		}()
		return nil, providers.ContextError(ctx.Err())
	}
}

// TurnInput is one request to the engine. Exactly one of Text, ChoiceID, Confirm
// and Reject is set.
type TurnInput struct {
	// Text is the user's message (or a finalised transcript).
	Text string
	// Language is the user's BCP 47 tag ("it", "en-GB"). Empty lets the engine
	// choose; a tag the verifier and templates do not support is an error.
	Language string
	// ChoiceID selects one of the choices of the pending clarification. Selecting a
	// choice sends its ID, never free text, so no model is involved.
	ChoiceID string
	// Confirm presents the confirmation of the pending prepared action. It comes from
	// the client UI, never from the model: the token is the single-use secret
	// actions.Prepare returned with the preparation.
	Confirm *Confirmation
	// Reject declines the pending prepared action from the client UI. Nothing is
	// sent to the application.
	Reject string
	// Attachments are the files the user attached to this text message, as the files
	// service let a model see them (never a path, a file id or image bytes). They are
	// untrusted data for the proposal prompt of this turn and nothing else: they are not
	// the user's message, so the router does not rank on them, the transcript does not keep
	// them and they are not replayed; they are counted in the prompt the turn is budgeted
	// and audited on (ModelCall.PromptToks). More than AttachmentMaxCount, or any beside a
	// choice, a confirmation or a rejection, is validation_failed. Their text is cut as
	// BoundAttachments says.
	Attachments []Attachment
}

// Confirmation is a presented confirmation.
type Confirmation struct {
	PreparationID string
	Token         actions.ConfirmationToken
}

// Stage names recorded in Trace.Stages, in pipeline order.
const (
	StageScope      = "scope"
	StageRate       = "rate"
	StageGoal       = "goal"
	StageRules      = "rules"
	StageShortlist  = "shortlist"
	StageDecision   = "decision"
	StageContext    = "context"
	StageProposal   = "proposal"
	StageValidate   = "validate"
	StageResolve    = "resolve"
	StageHost       = "host"
	StageDirective  = "directive"
	StageConfirm    = "confirm"
	StageCommit     = "commit"
	StageReceipt    = "receipt"
	StageVerify     = "verify"
	StagePersist    = "persist"
	StageNoModelUse = "no_model"
)

// ModelCall records one model call: what it was for and how big it was. It holds
// sizes, never text.
type ModelCall struct {
	// Purpose is "proposal" or "response".
	Purpose string
	// PromptToks is the estimated prompt size (providers.EstimateTokens, which
	// overestimates), including the output constraint.
	PromptToks int
	// PromptBytes is the size of the contents and the constraint in bytes.
	PromptBytes int
	// TokIn and TokOut are the runtime's own counts when it reports them.
	TokIn, TokOut int
	// Capabilities and TurnsReplayed describe the proposal prompt: the capabilities the
	// frame lists and the turn rows replayed. HistoryToks is the estimated size of
	// everything in it that derives from history (the replayed rows, the capsule lines and
	// the goals set aside), capped by the profile on its own.
	Capabilities  int
	TurnsReplayed int
	HistoryToks   int
	// Trimmed names what the budget cut, in the order it was cut (turns, digest, capsules,
	// suspended, recent_turns, choices, capabilities; see dctx.Report.Trimmed).
	Trimmed []string
	// Provider is the provider that served the call.
	Provider string
	// Err is the typed code of a failed call.
	Err contracts.ErrCode
}

// Trace says how a turn ran. It holds identifiers, names, codes and sizes, never
// the user's text, a prompt or a value.
type Trace struct {
	Stages []string
	// Route is the router's outcome ("shortlist", "out_of_scope") and Shortlist the
	// capability IDs it returned, best first.
	Route     capabilities.Outcome
	Shortlist []string
	// Decision is the decision stage's fallback reason, or "selected" when it
	// selected a candidate.
	Decision string
	// ModelCalls lists the model calls made, in order.
	ModelCalls []ModelCall
	// Response says whether the reply is the model's verified text or the template.
	Response verify.Source
	// ResponseReason says why: verified, verification_failed or degraded.
	ResponseReason verify.Reason
	// Transitions names the deterministic state transitions the turn took (the
	// Transition* constants): a goal expired, was cancelled, restarted, corrected, set
	// aside, resumed or returned to its parent. Names only, never a value.
	Transitions []string
	// UIAction says what became of the UI tools of the turn (the UIAction* statuses and "refused:" with a UIReason*):
	// empty when none were declared. A status, never a name, an argument or a result.
	UIAction string
}

// Result is the outcome of one turn.
type Result struct {
	// TurnID identifies the turn (trn_...).
	TurnID string
	// Language is the language of the response.
	Language verify.Language
	// Directive is what the core decided to say. Its facts are the host's and its
	// act and codes the core's; the response is realised from it.
	Directive conversation.DialogueDirective
	// Response is the verified reply: the model's text when it passed the verifier,
	// otherwise the deterministic template for the same directive. It is never
	// unverified model text.
	Response verify.Outcome
	// Prepared is the action awaiting a confirmation, when the turn prepared one.
	Prepared *contracts.PreparedAction
	// Confirmation is the single-use secret of Prepared. It is for the client UI
	// only: it is never put in a prompt, a log, the session state or an audit event,
	// and it prints as "[redacted]".
	Confirmation actions.ConfirmationToken
	// Receipt is the outcome of a commit, when the turn committed one.
	Receipt *contracts.ActionReceipt
	// UIAction is the validated call of a UI tool that the client's screen is to run, when the model proposed one that
	// the client declared and the host registered and whose arguments satisfy the declared schema. It is an instruction
	// to the client's UI, not to the application: it is data for the client, never persisted by the engine, never part
	// of the directive, and the result the screen sends back is untrusted client data (WithClientContext). It is never
	// set on a turn that asks for a confirmation, refuses or fails.
	UIAction *UIAction
	// MemoryProposals are assistant-proposed memory items stored this turn as status
	// proposed (ADR 0042). Confirm only via POST /v1/memory/{id}/confirm.
	MemoryProposals []mem.MemoryItem
	// Code is the typed reason when the turn ended in a refusal or a failure; empty
	// when the turn did what was asked (an answer, a clarification, a confirmation
	// request, a committed receipt).
	Code contracts.ErrCode
	// Trace says how the turn ran.
	Trace Trace
}

// Failed reports whether the turn ended in a refusal or a failure.
func (r Result) Failed() bool { return r.Code != "" }
