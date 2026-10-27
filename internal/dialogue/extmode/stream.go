package extmode

import (
	"context"
	"io"
	"sync"

	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// StreamSender is an optional Sender that can open a provider stream after gates pass.
type StreamSender interface {
	SendStream(ctx context.Context, scope contracts.Scope, req SendRequest) (providers.RichStream, error)
}

// SendStream implements StreamSender for GeneratorSender.
func (s GeneratorSender) SendStream(ctx context.Context, scope contracts.Scope, req SendRequest) (providers.RichStream, error) {
	g, err := s.For(req.Profile)
	if err != nil {
		return nil, err
	}
	return g.GenerateStream(ctx, scope, req.Rich)
}

// chatStream prefers a local ChatStream; for external it opens a real provider stream when the
// Sender supports StreamSender (egress + budget reserved before the first byte; settle on done /
// cancel). Otherwise falls back to one-shot Chat (1.3 behaviour).
func (r *Runtime) chatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	st := r.stage.take(scope, req)
	if st == nil {
		return nil, deny("not_released_by_guard")
	}
	if st.localOnly && r.cfg.Local != nil {
		p, err := r.cfg.Local.Select(ctx, scope, providers.RoleController)
		if err == nil && p.Local && p.Generator != nil {
			if s, serr := p.Generator.ChatStream(ctx, scope, st.original); serr == nil {
				return s, nil
			}
		}
	}
	if _, ok := r.cfg.Sender.(StreamSender); ok && !st.localOnly {
		if s, err := r.openExternalStream(ctx, scope, st); err == nil {
			return s, nil
		} else if contracts.CodeOf(err) != contracts.ErrUnavailable {
			return nil, err
		}
	}
	res, err := r.run(ctx, scope, st)
	if err != nil {
		return nil, err
	}
	return &oneShotStream{chunks: []providers.ChatChunk{
		{Delta: res.Text, Done: true, FinishReason: res.FinishReason, Usage: res.Usage, Timings: res.Timings, TraceID: res.TraceID, Model: res.Model},
	}}, nil
}

func (r *Runtime) openExternalStream(ctx context.Context, scope contracts.Scope, st *staged) (providers.ChatStream, error) {
	ss, ok := r.cfg.Sender.(StreamSender)
	if !ok {
		return nil, contracts.New(contracts.ErrUnavailable, "extmode: sender does not stream")
	}
	mreq := modes.Request{
		Scope: scope, Role: st.role, Requirement: st.req, Candidate: st.cand, CommitCapable: st.commit,
		Estimate: r.estimator(st),
	}
	lease, o, err := r.router.OpenStream(ctx, mreq)
	if r.cfg.OnOutcome != nil {
		r.cfg.OnOutcome(scope, o)
	}
	if err != nil {
		return nil, err
	}
	if lease == nil || o.Target != modes.TargetExternal {
		return nil, contracts.New(contracts.ErrUnavailable, "extmode: stream not external")
	}
	rich, err := richFromEnvelope(lease.Call.Envelope, st)
	if err != nil {
		lease.SettleFail(ctx, pv.ProviderErrorKindBadRequest, nil)
		return nil, &modes.CallError{Kind: pv.ProviderErrorKindBadRequest, Sent: false, Err: err}
	}
	rs, err := ss.SendStream(ctx, scope, SendRequest{
		Call: lease.Call, Profile: lease.Call.Admission.Profile, Rich: rich,
		Features: r.effectiveFeatures(st, len(rich.Tools) > 0),
	})
	if err != nil {
		ce := callError(err)
		if c, ok := ce.(*modes.CallError); ok {
			lease.SettleFail(ctx, c.Kind, c.Metering)
		} else {
			lease.SettleFail(ctx, pv.ProviderErrorKindConnectionLost, nil)
		}
		return nil, ce
	}
	return &leasedChatStream{ctx: ctx, lease: lease, inner: rs, st: st, r: r}, nil
}

// leasedChatStream maps a RichStream to ChatStream and settles the lease once.
type leasedChatStream struct {
	ctx   context.Context
	lease *modes.StreamLease
	inner providers.RichStream
	st    *staged
	r     *Runtime

	mu      sync.Mutex
	closed  bool
	usage   providers.CostUsage
	model   string
	finish  providers.FinishReason
	timings providers.Timings
	trace   string
}

func (s *leasedChatStream) Recv() (providers.ChatChunk, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return providers.ChatChunk{}, providers.ErrStreamClosed
	}
	s.mu.Unlock()

	for {
		ck, err := s.inner.Recv()
		if err == io.EOF {
			s.finishOK()
			return providers.ChatChunk{}, io.EOF
		}
		if err != nil {
			s.finishErr(err)
			return providers.ChatChunk{}, err
		}
		switch ck.Kind {
		case providers.DeltaText:
			if ck.Text == "" {
				continue
			}
			return providers.ChatChunk{Delta: ck.Text}, nil
		case providers.DeltaUsage:
			if ck.Usage != nil {
				s.usage = *ck.Usage
			}
		case providers.DeltaDone:
			if ck.Usage != nil {
				s.usage = *ck.Usage
			}
			s.finish = ck.FinishReason
			s.timings = ck.Timings
			s.trace = ck.TraceID
			s.model = ck.Model
			s.finishOK()
			u := s.usage.Usage()
			return providers.ChatChunk{
				Done: true, FinishReason: ck.FinishReason, Usage: u,
				Timings: ck.Timings, TraceID: ck.TraceID, Model: ck.Model,
			}, nil
		default:
			// tool/media parts are ignored for text realisation streams
		}
	}
}

func (s *leasedChatStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	_ = s.inner.Close()
	if s.lease != nil {
		s.lease.SettleCancel(s.ctx)
		s.lease = nil
	}
	return nil
}

func (s *leasedChatStream) finishOK() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease == nil {
		return
	}
	metering := meteringOf(s.usage)
	er := modes.ExternalResult{
		Metering: metering, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported, ReplyModel: s.model,
	}
	if s.usage.Estimated {
		er.MeteringBasis = pv.UsageRecordMeteringBasisEstimated
	} else if s.usage.CostMicros > 0 {
		m := pv.MicroUnits(s.usage.CostMicros)
		er.ReportedMicroUnits = &m
	}
	s.lease.SettleSuccess(s.ctx, er)
	s.lease = nil
}

func (s *leasedChatStream) finishErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lease == nil {
		return
	}
	kind := pv.ProviderErrorKindConnectionLost
	if c, ok := err.(*modes.CallError); ok {
		kind = c.Kind
	}
	var m *providerbudget.Units
	if s.usage.ToksIn > 0 || s.usage.ToksOut > 0 {
		u := meteringOf(s.usage)
		m = &u
	}
	s.lease.SettleFail(s.ctx, kind, m)
	s.lease = nil
}
