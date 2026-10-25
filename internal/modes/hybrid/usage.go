package hybrid

import (
	"context"
	"slices"
	"sync"

	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Cost bases of a TurnUsage.
const (
	CostBasisNone     = "none"
	CostBasisReported = "provider_reported"
	CostBasisEstimate = "estimated_upper_bound"
)

// TurnUsage is what the external call of one turn consumed, as integers. It holds no content. The
// host feeds it to its own dashboards; the budget service holds the authoritative ledger.
type TurnUsage struct {
	// ExternalCalls is 1 when a request was sent to a provider (even one that failed), else 0.
	ExternalCalls int64  `json:"external_calls"`
	ToksIn        int64  `json:"toks_in"`
	ToksOut       int64  `json:"toks_out"`
	Currency      string `json:"currency,omitempty"`
	// CostMicros is in micro-units of Currency: what the provider reported, else the estimate that
	// was reserved (an upper bound). CostBasis says which.
	CostMicros int64  `json:"cost_micros"`
	CostBasis  string `json:"cost_basis"`
	// ReservedMicros is the upper bound reserved before the call.
	ReservedMicros int64  `json:"reserved_micros"`
	ReservationID  string `json:"reservation_id,omitempty"`
	LatencyMS      int64  `json:"latency_ms"`
}

// Spend is the cost of one currency in a Summary.
type Spend struct {
	Currency string `json:"currency"`
	// ReportedMicros is what providers reported; EstimatedMicros the upper bounds of the calls that
	// reported nothing.
	ReportedMicros  int64 `json:"reported_micros"`
	EstimatedMicros int64 `json:"estimated_micros"`
}

// Summary is the running total of a tenant since the process started. It is a convenience for a
// dashboard; the durable record is the budget service's usage export and the audit log.
type Summary struct {
	Turns int64 `json:"turns"`
	// ExternalTurns, LocalModelTurns and TemplateTurns count the turns by who wrote the shown text.
	ExternalTurns   int64 `json:"external_turns"`
	LocalModelTurns int64 `json:"local_model_turns"`
	TemplateTurns   int64 `json:"template_turns"`
	// LocalFallbacks counts local hops, VerificationFailures external texts that failed fact
	// verification, and TemplateFallbacks every turn whose text fell back to the template after an
	// attempt.
	LocalFallbacks       int64   `json:"local_fallbacks"`
	TemplateFallbacks    int64   `json:"template_fallbacks"`
	VerificationFailures int64   `json:"verification_failures"`
	ExternalCalls        int64   `json:"external_calls"`
	ToksIn               int64   `json:"toks_in"`
	ToksOut              int64   `json:"toks_out"`
	Spend                []Spend `json:"spend"`
	// ByOutcome counts the router outcomes of the turns by kind.
	ByOutcome map[string]int64 `json:"by_outcome"`
}

// UsageSource is the provider usage export of the budget service. *providerbudget.Service implements it.
type UsageSource interface {
	ExportUsage(ctx context.Context, scope contracts.Scope, f providerbudget.UsageFilter) (providerbudget.UsageExport, error)
}

var _ UsageSource = (*providerbudget.Service)(nil)

// Report is the cost and usage data a host reads for its own dashboards.
type Report struct {
	// Summary is the running total of the tenant of the scope.
	Summary Summary `json:"summary"`
	// Provider is the budget service's usage export (rows and totals per provider, channel and
	// currency), or nil when no usage source is configured.
	Provider *providerbudget.UsageExport `json:"provider,omitempty"`
}

type ledgerKey struct{ deployment, application, tenant string }

type ledger struct {
	mu sync.Mutex
	m  map[ledgerKey]*Summary
}

func newLedger() *ledger { return &ledger{m: map[ledgerKey]*Summary{}} }

func keyOf(s contracts.Scope) ledgerKey {
	return ledgerKey{deployment: s.DeploymentID, application: s.ApplicationID, tenant: s.TenantID}
}

// add folds one turn into the totals of its tenant.
func (l *ledger) add(scope contracts.Scope, r *Realised) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.m[keyOf(scope)]
	if s == nil {
		s = &Summary{ByOutcome: map[string]int64{}}
		l.m[keyOf(scope)] = s
	}
	s.Turns++
	switch r.Realiser {
	case RealiserExternal:
		s.ExternalTurns++
	case RealiserLocalModel:
		s.LocalModelTurns++
	default:
		s.TemplateTurns++
	}
	for _, t := range r.Transitions {
		switch t.Kind {
		case TransitionLocalFallback:
			s.LocalFallbacks++
		case TransitionTemplateFallback:
			s.TemplateFallbacks++
			if t.Reason == ReasonVerificationFailed {
				s.VerificationFailures++
			}
		}
	}
	s.ByOutcome[string(r.Outcome.Kind)]++
	u := r.Usage
	s.ExternalCalls += u.ExternalCalls
	s.ToksIn += u.ToksIn
	s.ToksOut += u.ToksOut
	if u.ExternalCalls > 0 && u.CostBasis != CostBasisNone {
		i := slices.IndexFunc(s.Spend, func(x Spend) bool { return x.Currency == u.Currency })
		if i < 0 {
			s.Spend = append(s.Spend, Spend{Currency: u.Currency})
			i = len(s.Spend) - 1
		}
		if u.CostBasis == CostBasisReported {
			s.Spend[i].ReportedMicros += u.CostMicros
		} else {
			s.Spend[i].EstimatedMicros += u.CostMicros
		}
	}
}

func (l *ledger) summary(scope contracts.Scope) Summary {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := Summary{ByOutcome: map[string]int64{}, Spend: []Spend{}}
	if s := l.m[keyOf(scope)]; s != nil {
		out = *s
		out.Spend = slices.Clone(s.Spend)
		if out.Spend == nil {
			out.Spend = []Spend{}
		}
		out.ByOutcome = make(map[string]int64, len(s.ByOutcome))
		for k, v := range s.ByOutcome {
			out.ByOutcome[k] = v
		}
	}
	return out
}

// Usage returns the running totals of the tenant of the scope.
func (h *Hybrid) Usage(scope contracts.Scope) (Summary, error) {
	if err := scope.Validate(); err != nil {
		return Summary{}, err
	}
	return h.ledger.summary(scope), nil
}

// Report returns the running totals of the tenant and, when a usage source is configured, the
// provider usage export for the filter. The export is confined to the scope's tenant and workspace
// unless the filter asks for the whole application, which the host must gate by role.
func (h *Hybrid) Report(ctx context.Context, scope contracts.Scope, f providerbudget.UsageFilter) (Report, error) {
	sum, err := h.Usage(scope)
	if err != nil {
		return Report{}, err
	}
	out := Report{Summary: sum}
	if h.cfg.Usage != nil {
		exp, err := h.cfg.Usage.ExportUsage(ctx, scope, f)
		if err != nil {
			return Report{}, err
		}
		out.Provider = &exp
	}
	return out, nil
}
