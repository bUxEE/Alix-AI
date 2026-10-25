package extmode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ---- staged requests ----

// staged is a request the Guard released. The engine hands the generator only a ChatRequest, with
// no metadata, so the Guard keeps what the router needs (the filtered candidate, the requirement
// and the original prompt for a local serve) under a key made of the scope and the released
// prompt. A request that was not released is not found and is refused.
type staged struct {
	original providers.ChatRequest
	filtered providers.ChatRequest
	cand     dataegress.Candidate
	role     modes.Role
	req      providerreg.Requirement
	commit   bool
	// schemaOut: the caller asked for a JSON Schema constrained reply.
	schemaOut bool
	// localOnly: the request is served by the local runtime only (Local mode, or Hybrid with
	// sensitive data); no candidate is ever sent.
	localOnly bool
	tools     toolSet
}

type stageEntry struct {
	st *staged
	at time.Time
}

type stage struct {
	clk clock.Clock
	mu  sync.Mutex
	m   map[string][]stageEntry
	n   int
}

const (
	stageTTL = 10 * time.Minute
	stageMax = 512
)

func newStage(clk clock.Clock) *stage { return &stage{clk: clk, m: map[string][]stageEntry{}} }

func stageKey(scope contracts.Scope, req providers.ChatRequest) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	_ = enc.Encode([]string{scope.DeploymentID, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, scope.PrincipalID, scope.SessionID})
	for _, m := range req.Messages {
		_ = enc.Encode([]any{m.Role, m.Trusted, m.Content})
	}
	_ = enc.Encode(string(req.Output.JSONSchema))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *stage) put(scope contracts.Scope, released providers.ChatRequest, st *staged) {
	st.filtered = released
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, es := range s.m {
		keep := es[:0]
		for _, e := range es {
			if now.Sub(e.at) < stageTTL {
				keep = append(keep, e)
			} else {
				s.n--
			}
		}
		if len(keep) == 0 {
			delete(s.m, k)
		} else {
			s.m[k] = keep
		}
	}
	if s.n >= stageMax {
		return // a full stage releases nothing more: the request is refused at send time
	}
	k := stageKey(scope, released)
	s.m[k] = append(s.m[k], stageEntry{st: st, at: now})
	s.n++
}

func (s *stage) take(scope contracts.Scope, req providers.ChatRequest) *staged {
	k := stageKey(scope, req)
	s.mu.Lock()
	defer s.mu.Unlock()
	es := s.m[k]
	if len(es) == 0 {
		return nil
	}
	st := es[0].st
	if len(es) == 1 {
		delete(s.m, k)
	} else {
		s.m[k] = es[1:]
	}
	s.n--
	return st
}

// ---- the generator the engine calls ----

// routed is the text generator of the external route.
type routed struct{ r *Runtime }

func (g routed) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	return g.r.chat(ctx, scope, req)
}

func (routed) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "extmode: the external route does not stream")
}

func (r *Runtime) chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	if err := scope.Validate(); err != nil {
		return providers.ChatResult{}, err
	}
	st := r.stage.take(scope, req)
	if st == nil {
		return providers.ChatResult{}, deny("not_released_by_guard")
	}
	return r.run(ctx, scope, st)
}

// run routes one released request: kill switch, profile gate, egress guard, budget, send.
func (r *Runtime) run(ctx context.Context, scope contracts.Scope, st *staged) (providers.ChatResult, error) {
	var localRes, extRes providers.ChatResult
	mreq := modes.Request{
		Scope: scope, Role: st.role, Requirement: st.req, Candidate: st.cand, CommitCapable: st.commit,
		Estimate: r.estimator(st),
	}
	if st.localOnly {
		mreq.Candidate = dataegress.Candidate{} // nothing external can be built from it
	}
	o, err := r.router.Execute(ctx, mreq, modes.Executors{
		Local: func(ctx context.Context) error {
			p, err := r.cfg.Local.Select(ctx, scope, providers.RoleController)
			if err != nil {
				return err
			}
			if !p.Local || p.Generator == nil {
				return contracts.New(contracts.ErrUnavailable, "extmode: no local runtime")
			}
			localRes, err = p.Generator.Chat(ctx, scope, st.original)
			return err
		},
		External: func(ctx context.Context, call modes.ExternalCall) (modes.ExternalResult, error) {
			if st.localOnly {
				return modes.ExternalResult{}, &modes.CallError{Kind: pv.ProviderErrorKindBadRequest, Sent: false}
			}
			res, ur, err := r.send(ctx, scope, st, call)
			extRes = res
			return ur, err
		},
	})
	if r.cfg.OnOutcome != nil {
		r.cfg.OnOutcome(scope, o)
	}
	if err != nil {
		return providers.ChatResult{}, err
	}
	if o.Target == modes.TargetLocal {
		return localRes, nil
	}
	return extRes, nil
}

// ---- sending ----

// Feature is a provider-native feature AlixAI does not delegate control to (additive plan section 9).
type Feature string

// The native features.
const (
	FeatureAgents         Feature = "agents"
	FeatureMemory         Feature = "memory"
	FeatureFileSearch     Feature = "file_search"
	FeatureServerToolLoop Feature = "server_tool_loop"
)

// Features says which native features a call may use. The zero value is all off.
type Features struct {
	Agents, Memory, FileSearch, ServerToolLoop bool
}

// Any reports whether a feature is on.
func (f Features) Any() bool { return f.Agents || f.Memory || f.FileSearch || f.ServerToolLoop }

// effectiveFeatures is what a call may use. A consequential call (one that may propose an action, or
// offers tools) uses none, whatever the operator asked for: the complete local audit, scope and
// receipt boundary cannot be proved for a provider-side loop. Any other call uses only the features
// the operator classified as conveniences, and never a server-side tool loop.
func (r *Runtime) effectiveFeatures(st *staged, tools bool) Features {
	if st.commit || tools {
		return Features{}
	}
	want := r.cfg.Features
	ok := func(f Feature) bool { return contains(r.cfg.Convenience, f) }
	return Features{
		Agents: want.Agents && ok(FeatureAgents), Memory: want.Memory && ok(FeatureMemory),
		FileSearch: want.FileSearch && ok(FeatureFileSearch),
	}
}

// SendRequest is everything a Sender receives: the profile, the validated request built from the
// filtered envelope, and the native features that are allowed. Nothing in it can reach an adapter.
type SendRequest struct {
	Call     modes.ExternalCall
	Profile  pv.ProviderProfile
	Rich     providers.RichRequest
	Features Features
}

// Sender sends an admitted, filtered request to a provider and returns its untrusted reply. It is
// implemented by the provider adapters (composition root); the tests use the scripted fake.
type Sender interface {
	Send(ctx context.Context, scope contracts.Scope, req SendRequest) (providers.RichResult, error)
}

// GeneratorSender is a Sender over a providers.MultimodalGenerator per profile. The generator
// interface has no native-feature switch, so a generator never gets one.
type GeneratorSender struct {
	For func(pv.ProviderProfile) (providers.MultimodalGenerator, error)
}

// Send implements Sender.
func (s GeneratorSender) Send(ctx context.Context, scope contracts.Scope, req SendRequest) (providers.RichResult, error) {
	g, err := s.For(req.Profile)
	if err != nil {
		return providers.RichResult{}, err
	}
	return g.Generate(ctx, scope, req.Rich)
}

func (r *Runtime) send(ctx context.Context, scope contracts.Scope, st *staged, call modes.ExternalCall) (providers.ChatResult, modes.ExternalResult, error) {
	rich, err := richFromEnvelope(call.Envelope, st)
	if err != nil {
		return providers.ChatResult{}, modes.ExternalResult{}, &modes.CallError{Kind: pv.ProviderErrorKindBadRequest, Sent: false, Err: err}
	}
	prof := call.Admission.Profile
	res, err := r.cfg.Sender.Send(ctx, scope, SendRequest{
		Call: call, Profile: prof, Rich: rich, Features: r.effectiveFeatures(st, len(rich.Tools) > 0),
	})
	if err != nil {
		return providers.ChatResult{}, modes.ExternalResult{}, callError(err)
	}
	metering := meteringOf(res.Usage)
	failBad := func() error {
		return &modes.CallError{Kind: pv.ProviderErrorKindMalformedResponse, Sent: true, Metering: &metering}
	}
	// A reply that does not say which model wrote it cannot be compared with the pinned model (SEC-128): it is
	// refused like a malformed one. A reply that names another model is refused by the router, which also
	// quarantines the profile.
	if res.Model == "" {
		return providers.ChatResult{}, modes.ExternalResult{}, failBad()
	}
	checked, err := r.cfg.Budget.CheckRichResult(rich, res)
	if err != nil {
		return providers.ChatResult{}, modes.ExternalResult{}, failBad()
	}
	cr, ok := r.chatResult(st, checked)
	if !ok {
		return providers.ChatResult{}, modes.ExternalResult{}, failBad()
	}
	ur := modes.ExternalResult{Metering: metering, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported, ReplyModel: res.Model}
	if res.Usage.Estimated {
		ur.MeteringBasis = pv.UsageRecordMeteringBasisEstimated
	} else if res.Usage.CostMicros > 0 {
		m := pv.MicroUnits(res.Usage.CostMicros)
		ur.ReportedMicroUnits = &m
	}
	return cr, ur, nil
}

func meteringOf(u providers.CostUsage) providerbudget.Units {
	return providerbudget.Units{
		ToksIn: pv.Counter(u.ToksIn), ToksOut: pv.Counter(u.ToksOut),
		ImagesIn: pv.Counter(u.ImagesIn), ImagesOut: pv.Counter(u.ImagesOut), Requests: 1,
	}
}

// callError maps a sender failure to the router's (modes.CallErrorOf): the provider's own text is never copied,
// and a credential refusal that nothing was sent for stays sent=false.
func callError(err error) error { return modes.CallErrorOf(err) }

// richFromEnvelope builds the request for the provider from the filtered envelope, and only from it.
func richFromEnvelope(env *pv.RequestEnvelope, st *staged) (providers.RichRequest, error) {
	if env == nil {
		return providers.RichRequest{}, deny("no_envelope")
	}
	out := providers.RichRequest{
		Role:    providers.RoleController,
		MaxToks: int(env.Payload.Limits.MaxOutputToks),
		Messages: []providers.RichMessage{{
			Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart(env.Payload.Instructions)},
		}},
	}
	for mi := range env.Payload.Messages {
		m := env.Payload.Messages[mi]
		role := providers.MsgUser
		if m.Role == pv.RequestEnvelopeMessageRoleAssistant {
			role = providers.MsgAssistant
		}
		rm := providers.RichMessage{Role: role}
		for pi := range m.Parts {
			text, ok := envelopeText(env, mi, pi)
			if !ok {
				return out, deny("media_not_supported")
			}
			rm.Parts = append(rm.Parts, providers.TextPart(text))
		}
		out.Messages = append(out.Messages, rm)
	}
	for _, t := range env.Payload.Tools {
		raw, err := json.Marshal(t.Parameters)
		if err != nil {
			return out, deny("bad_tool")
		}
		out.Tools = append(out.Tools, providers.ToolSpec{Name: t.Name, Description: t.Description, Parameters: raw})
	}
	if env.Payload.ResponseFormat.Kind == pv.RequestEnvelopeResponseFormatKindJSONSchema {
		raw, err := json.Marshal(env.Payload.ResponseFormat.Schema)
		if err != nil {
			return out, deny("bad_schema")
		}
		out.Output = providers.SchemaConstraint(raw)
	}
	return out, nil
}

// chatResult turns the provider's reply into the result the engine validates. A proposal comes
// back as the text of a TaskProposal; a response as plain text. Tool calls of a reply that was not
// asked for tools are ignored: they are data and nothing runs them.
func (r *Runtime) chatResult(st *staged, res providers.RichResult) (providers.ChatResult, bool) {
	cr := providers.ChatResult{
		FinishReason: res.FinishReason, Usage: res.Usage.Usage(), Timings: res.Timings, TraceID: res.TraceID, Model: res.Model,
	}
	if res.FinishReason == providers.FinishFiltered {
		return cr, false
	}
	if st.commit && len(res.ToolCalls) > 0 && len(st.tools.specs) > 0 {
		cr.JSON = st.proposalFromCall(res.ToolCalls[0])
		cr.Text = string(cr.JSON)
		return cr, true
	}
	if st.schemaOut {
		cr.Text = res.Text()
		if len(res.JSON) > 0 {
			cr.JSON = res.JSON
		} else if j, err := providers.ParseJSONOutput(cr.Text); err == nil {
			cr.JSON = j
		}
		return cr, strings.TrimSpace(cr.Text) != "" || len(cr.JSON) > 0
	}
	cr.Text = res.Text()
	return cr, strings.TrimSpace(cr.Text) != ""
}

// ---- pricing ----

func priceOf(p *pv.PriceTablePrice) *providers.Price {
	if p == nil {
		return nil
	}
	return &providers.Price{PerUnits: p.PerUnits, MicroUnits: int64(p.MicroUnits)}
}

// priceTable converts the priced table of a registry profile to the one the estimator reads.
func priceTable(t pv.PriceTable) providers.PriceTable {
	p := t.Prices
	return providers.PriceTable{
		Currency: string(t.Currency), ToksIn: priceOf(p.ToksIn), ToksOut: priceOf(p.ToksOut),
		ImagesIn: priceOf(p.ImagesIn), ImagesOut: priceOf(p.ImagesOut), Requests: priceOf(p.Requests),
	}
}

// estimator prices the worst case of the released request for the profile an attempt would use.
func (r *Runtime) estimator(st *staged) modes.Estimator {
	return func(p pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
		rich := providers.FromChat(st.filtered)
		if rich.MaxToks <= 0 {
			rich.MaxToks = r.cfg.Budget.MaxOutputTokens
		}
		for _, t := range st.tools.specs {
			raw, err := json.Marshal(t.Parameters)
			if err != nil {
				return providerbudget.Units{}, 0, err
			}
			rich.Tools = append(rich.Tools, providers.ToolSpec{Name: t.Name, Description: t.Description, Parameters: raw})
		}
		cu, err := r.cfg.Budget.EstimateRichCost(rich, priceTable(p.Pricing))
		if err != nil {
			return providerbudget.Units{}, 0, err
		}
		u := meteringOf(cu)
		return u, pv.MicroUnits(cu.CostMicros), nil
	}
}
