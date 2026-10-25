package hybrid

import (
	"context"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue/extmode"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ReasonVerificationFailed is the Transition.Reason of external text that failed fact verification.
const ReasonVerificationFailed = "verification_failed"

// Realiser says who wrote the text a turn shows.
type Realiser string

// The realisers.
const (
	RealiserExternal   Realiser = "external"
	RealiserLocalModel Realiser = "local_model"
	RealiserTemplate   Realiser = "template"
)

// defaultMaxOutputToks bounds the reply asked of a provider when Config.MaxOutputToks is zero.
const defaultMaxOutputToks = 512

// Config wires a Hybrid. The collaborators of the router are required: one missing the egress guard,
// the profile registry, the budget or the kill switch could not apply the gates, so it is not built.
type Config struct {
	Modes          modes.ModeSource
	KillSwitch     modes.KillSwitch
	Policy         modes.PolicySource
	LocalProviders modes.LocalProviders
	Profiles       modes.Profiles
	Egress         dataegress.Guard
	Budgets        modes.Budgets
	// Sender sends an admitted, filtered request to the provider. It has no adapter in reach.
	Sender extmode.Sender
	// Local is the local text generator that realises a turn locally. Nil means a local turn shows the
	// deterministic template.
	Local providers.TextGenerator
	// Budget bounds and prices every call, local and external.
	Budget providers.Budget
	// MaxOutputToks caps the reply asked of a provider. Zero takes 512.
	MaxOutputToks int
	// FactClass is the data class the operator assigns to the facts of a directive, internal at most
	// for it to leave. Empty (the default) keeps every turn local: unknown is the most restrictive.
	FactClass pv.DataClassification
	// Recorder records transitions. Audit builds an AuditRecorder when Recorder is nil. One of them is
	// required: a fallback that cannot be recorded is not built.
	Recorder Recorder
	Audit    audit.Appender
	// Usage is the budget service's usage export, for Report. Optional.
	Usage UsageSource
	Clock clock.Clock
}

// Hybrid is the orchestration of Hybrid production mode. It is safe for concurrent use.
type Hybrid struct {
	cfg    Config
	router *modes.Router
	ledger *ledger
	clk    clock.Clock
}

// New returns a Hybrid. It builds its own router over a realisation-only policy, so a caller cannot route
// the controller or any other role outward through it.
func New(cfg Config) (*Hybrid, error) {
	if cfg.Sender == nil {
		return nil, contracts.New(contracts.ErrInternal, "hybrid: a sender is required")
	}
	if cfg.Recorder == nil {
		if cfg.Audit == nil {
			return nil, contracts.New(contracts.ErrInternal, "hybrid: a recorder or an audit log is required")
		}
		cfg.Recorder = AuditRecorder{Log: cfg.Audit}
	}
	if err := cfg.Budget.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxOutputToks == 0 {
		cfg.MaxOutputToks = defaultMaxOutputToks
	}
	if cfg.MaxOutputToks < 0 || cfg.MaxOutputToks > cfg.Budget.MaxOutputTokens {
		return nil, contracts.New(contracts.ErrInternal, "hybrid: MaxOutputToks is outside the request budget")
	}
	if cfg.FactClass != "" && !cfg.FactClass.Valid() {
		return nil, contracts.New(contracts.ErrInternal, "hybrid: FactClass is not a data class")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	res, err := modes.NewResolver(cfg.Modes, cfg.KillSwitch)
	if err != nil {
		return nil, err
	}
	if cfg.Policy == nil {
		return nil, contracts.New(contracts.ErrInternal, "hybrid: a policy is required")
	}
	rt, err := modes.New(modes.Config{
		Modes: cfg.Modes, KillSwitch: cfg.KillSwitch, Policy: realisationOnly(cfg.Policy, res), Local: cfg.LocalProviders,
		Profiles: cfg.Profiles, Guard: cfg.Egress, Budgets: cfg.Budgets, Clock: cfg.Clock,
	})
	if err != nil {
		return nil, err
	}
	return &Hybrid{cfg: cfg, router: rt, ledger: newLedger(), clk: cfg.Clock}, nil
}

// Router returns the router of the orchestration (its locality report and resolver).
func (h *Hybrid) Router() *modes.Router { return h.router }

// Turn is one response to realise.
type Turn struct {
	Scope contracts.Scope
	// Directive and Receipt are the objects the core built and the response must be faithful to. The
	// receipt is nil unless the directive names one.
	Directive *conversation.DialogueDirective
	Receipt   *actions.ActionReceipt
	// Options carry the response language and the application's field labels.
	Options verify.Options
	// Class is the data class of this turn when it is more restrictive than Config.FactClass (a turn
	// that touches confidential data). It can only tighten.
	Class pv.DataClassification
}

// Realised is a response that is safe to show or to speak: verified external or local text, or the
// deterministic template. It holds what the host needs to audit and chart the turn, and no prompt.
type Realised struct {
	Text     string
	Source   verify.Source
	Reason   verify.Reason
	Realiser Realiser
	// Violations are why external or local text was rejected (kinds and canonical forms only).
	Violations []verify.Violation
	// Outcome is how the router served the realisation role: the target, every fallback hop and the
	// typed failure when it did not serve.
	Outcome modes.Outcome
	// Transitions lists every fallback of the turn, in order. Each was handed to the recorder.
	Transitions []Transition
	// RecordFailed is true when a transition could not be recorded. The text is still correct.
	RecordFailed bool
	Usage        TurnUsage
}

// rank orders the data classes; an unknown class is the most restrictive.
func rank(c pv.DataClassification) int {
	switch c {
	case pv.DataClassificationPublic:
		return 0
	case pv.DataClassificationInternal:
		return 1
	case pv.DataClassificationConfidential:
		return 2
	}
	return 3
}

// classOf is the class the turn is evaluated at: Config.FactClass, tightened by the turn. An empty
// FactClass stays empty (unclassified, so local); an unknown turn class is restricted.
func (h *Hybrid) classOf(turn pv.DataClassification) pv.DataClassification {
	base := h.cfg.FactClass
	if base == "" {
		return ""
	}
	if turn == "" {
		return base
	}
	if rank(turn) > rank(base) {
		if turn.Valid() {
			return turn
		}
		return pv.DataClassificationRestricted
	}
	return base
}

// Realise writes the response of a turn. An error means the directive or receipt cannot be used (see
// verify.Degraded) or the request itself is invalid: nothing is sent and nothing is shown. Every
// other failure still returns the correct facts in the deterministic template, with the typed outcome
// on the result; a failure is never a success-looking result without its Outcome and Transitions.
func (h *Hybrid) Realise(ctx context.Context, t Turn) (Realised, error) {
	if err := t.Scope.Validate(); err != nil {
		return Realised{}, err
	}
	if t.Directive == nil {
		return Realised{}, contracts.New(contracts.ErrValidationFailed, "hybrid: a directive is required")
	}
	// Only a directive the verifier accepts is realised, and the template is the answer to fall back
	// on: nothing below can leave the user without correct facts.
	tmpl, err := verify.Degraded(t.Directive, t.Receipt, t.Options)
	if err != nil {
		return Realised{}, err
	}
	class := h.classOf(t.Class)
	cand, err := h.candidate(t, class)
	if err != nil {
		return Realised{}, err
	}
	var (
		q     quote
		ext   extReply
		local *verify.Outcome
	)
	ctxToks := int64(h.cfg.Budget.EstimateRich(h.richOf(cand)))
	out, execErr := h.router.Execute(ctx, modes.Request{
		Scope: t.Scope, Role: modes.RoleRealisation,
		Requirement: providerreg.Requirement{DataClass: class, ContextToks: ctxToks, OutputToks: int64(h.cfg.MaxOutputToks)},
		Candidate:   cand, Estimate: h.estimator(cand, &q),
	}, modes.Executors{
		Local: func(ctx context.Context) error {
			o := h.realiseLocal(ctx, t, tmpl)
			local = &o
			return nil
		},
		External: func(ctx context.Context, call modes.ExternalCall) (modes.ExternalResult, error) {
			return h.send(ctx, t.Scope, call, &ext)
		},
	})
	if out.Kind == "" { // the request itself was invalid
		return Realised{}, execErr
	}

	r := Realised{Text: tmpl.Text, Source: tmpl.Source, Reason: verify.ReasonDegraded, Realiser: RealiserTemplate, Outcome: out}
	r.Usage = h.usageOf(out, ext, q)
	attempted := false // an attempt to write the text failed, so the template is a fallback
	from := "local"
	switch {
	case out.Served() && out.Target == modes.TargetExternal:
		from = string(out.Profile)
		v, verr := verify.Realise(ext.text, t.Directive, t.Receipt, t.Options)
		if verr != nil {
			return Realised{}, verr
		}
		r.Text, r.Source, r.Reason, r.Violations = v.Text, v.Source, v.Reason, v.Violations
		if v.Source == verify.SourceModel {
			r.Realiser = RealiserExternal
		} else {
			// No second external attempt and no other provider: the template for the same directive.
			attempted = true
			r.Transitions = append(r.Transitions, h.transition(t, out, TransitionTemplateFallback, ReasonVerificationFailed, from, "template", "", v.Violations))
		}
	case out.Served() && local != nil:
		r.Text, r.Source, r.Reason, r.Violations = local.Text, local.Source, local.Reason, local.Violations
		if local.Source == verify.SourceModel {
			r.Realiser = RealiserLocalModel
		} else if h.cfg.Local != nil {
			attempted = true
			reason := "local_degraded"
			if local.Reason == verify.ReasonVerificationFailed {
				reason = "local_" + ReasonVerificationFailed
			}
			r.Transitions = append(r.Transitions, h.transition(t, out, TransitionTemplateFallback, reason, "local", "template", "", local.Violations))
		}
	case !out.Served():
		attempted = true
		if n := len(out.Hops); n > 0 {
			from = out.Hops[n-1].To
		} else if out.Profile != "" {
			from = string(out.Profile)
		} else {
			from = "none"
		}
	}
	// Hops come first in the record: they happened first.
	var hops []Transition
	for _, hp := range out.Hops {
		kind := TransitionLocalFallback
		if hp.To != modes.LocalHop {
			kind = TransitionProfileFallback
		}
		hops = append(hops, h.transition(t, out, kind, "fallback_"+string(hp.Condition), hp.From, hp.To, string(hp.Condition), nil))
	}
	r.Transitions = append(hops, r.Transitions...)
	if !out.Served() && attempted {
		r.Transitions = append(r.Transitions, h.transition(t, out, TransitionTemplateFallback, string(out.Kind), from, "template", "", nil))
	}
	for _, tr := range r.Transitions {
		if err := h.cfg.Recorder.Record(context.WithoutCancel(ctx), t.Scope, tr); err != nil {
			r.RecordFailed = true
		}
	}
	h.ledger.add(t.Scope, &r)
	return r, nil
}

func (h *Hybrid) transition(t Turn, o modes.Outcome, kind TransitionKind, reason, from, to, cond string, vs []verify.Violation) Transition {
	tr := Transition{
		Kind: kind, Reason: reason, From: from, To: to, Condition: cond, Outcome: string(o.Kind),
		Mode: string(o.Mode), Act: string(t.Directive.Act), DataSent: o.DataSent,
	}
	for _, v := range vs {
		tr.Violations = append(tr.Violations, string(v.Kind))
	}
	return tr
}

// extReply is what the external executor captured: the untrusted text and what the call consumed.
type extReply struct {
	text    string
	metered bool
	toksIn  int64
	toksOut int64
	micros  int64
	hasCost bool
	latency time.Duration
}

// send makes the one external call of a turn, from the filtered envelope only. Anything but plain
// text is a malformed reply: a tool call in a realisation response is dropped as malformed.
func (h *Hybrid) send(ctx context.Context, scope contracts.Scope, call modes.ExternalCall, rep *extReply) (modes.ExternalResult, error) {
	rich, err := richFromEnvelope(call.Envelope)
	if err != nil {
		return modes.ExternalResult{}, &modes.CallError{Kind: pv.ProviderErrorKindBadRequest, Sent: false, Err: err}
	}
	started := h.clk.Now()
	res, err := h.cfg.Sender.Send(ctx, scope, extmode.SendRequest{Call: call, Profile: call.Admission.Profile, Rich: rich, Features: extmode.Features{}})
	rep.latency = h.clk.Now().Sub(started)
	if err != nil {
		return modes.ExternalResult{}, callError(err)
	}
	metering := meteringOf(res.Usage)
	rep.metered, rep.toksIn, rep.toksOut = true, int64(res.Usage.ToksIn), int64(res.Usage.ToksOut)
	bad := func() error {
		return &modes.CallError{Kind: pv.ProviderErrorKindMalformedResponse, Sent: true, Metering: &metering}
	}
	checked, err := h.cfg.Budget.CheckRichResult(rich, res)
	if res.Model == "" || err != nil || len(checked.ToolCalls) > 0 || checked.FinishReason == providers.FinishFiltered {
		return modes.ExternalResult{}, bad()
	}
	text := strings.TrimSpace(checked.Text())
	if text == "" {
		return modes.ExternalResult{}, bad()
	}
	rep.text = text
	ur := modes.ExternalResult{Metering: metering, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported, ReplyModel: res.Model}
	if res.Usage.Estimated {
		ur.MeteringBasis = pv.UsageRecordMeteringBasisEstimated
	} else if res.Usage.CostMicros > 0 {
		m := pv.MicroUnits(res.Usage.CostMicros)
		ur.ReportedMicroUnits = &m
		rep.micros, rep.hasCost = res.Usage.CostMicros, true
	}
	return ur, nil
}

// usageOf folds what the call consumed into the TurnUsage of the turn.
func (h *Hybrid) usageOf(o modes.Outcome, e extReply, q quote) TurnUsage {
	u := TurnUsage{CostBasis: CostBasisNone, ReservationID: string(o.ReservationID), LatencyMS: e.latency.Milliseconds()}
	if !o.DataSent {
		return u
	}
	u.ExternalCalls, u.ToksIn, u.ToksOut = 1, e.toksIn, e.toksOut
	u.Currency, u.ReservedMicros = q.currency, q.micros
	switch {
	case e.hasCost:
		u.CostMicros, u.CostBasis = e.micros, CostBasisReported
	case q.micros > 0:
		u.CostMicros, u.CostBasis = q.micros, CostBasisEstimate
	}
	return u
}

// realiseLocal writes the response with the local model and verifies it; a model that is missing,
// fails or writes text that does not verify leaves the template.
func (h *Hybrid) realiseLocal(ctx context.Context, t Turn, tmpl verify.Outcome) verify.Outcome {
	if h.cfg.Local == nil {
		return tmpl
	}
	req, err := h.cfg.Budget.CheckChat(h.localRequest(t))
	if err != nil {
		return tmpl
	}
	res, err := h.cfg.Local.Chat(ctx, t.Scope, req)
	if err != nil {
		return tmpl
	}
	v, err := verify.Realise(res.Text, t.Directive, t.Receipt, t.Options)
	if err != nil {
		return tmpl
	}
	return v
}

// localRequest is the prompt of the local model. It never leaves the deployment, so it carries the
// sources' claims too.
func (h *Hybrid) localRequest(t Turn) providers.ChatRequest {
	dj, _ := directiveJSON(t.Directive, t.Options.Language)
	var b strings.Builder
	b.WriteString("dialogue_directive: " + dj)
	if t.Receipt != nil {
		rj, _ := receiptJSON(t.Receipt)
		b.WriteString("\naction_receipt: " + rj)
	}
	for i, c := range t.Directive.Sources {
		b.WriteString("\nsource " + sourcePart(i, c, "").Text)
	}
	return providers.ChatRequest{
		Role:      providers.RoleController,
		MaxTokens: h.cfg.MaxOutputToks,
		Messages: []providers.Message{
			{Role: providers.MsgSystem, Content: instructions(t.Options.Language), Trusted: true},
			{Role: providers.MsgUser, Content: b.String()},
		},
	}
}
