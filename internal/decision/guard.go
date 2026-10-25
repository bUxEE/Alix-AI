package decision

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// RejectCode says why the candidate guard rejected a request. It travels with the
// candidate_set_rejected fallback reason, in the Advice and the audit event.
type RejectCode string

// The reject codes.
const (
	RejectNone         RejectCode = ""
	RejectEmpty        RejectCode = "empty"
	RejectOversized    RejectCode = "oversized"
	RejectDuplicate    RejectCode = "duplicate"
	RejectStale        RejectCode = "stale"
	RejectCrossTenant  RejectCode = "cross_tenant"
	RejectUnauthorized RejectCode = "unauthorized"
	RejectInvalid      RejectCode = "invalid"
)

const timestampLayout = "2006-01-02T15:04:05.000Z"

func parseTimestamp(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// checkCandidates is the candidate guard. It runs before any provider call and
// returns the ids of the set (for the later "selected is one of ours" gate) or the
// reason to reject. Cheap structural checks come first; the resolver, which may touch
// the application, runs last and only on a structurally sound set.
func (e *Engine) checkCandidates(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (map[string]struct{}, RejectCode) {
	cfg := e.cfg
	cs := req.CandidateSet
	n := len(cs.Candidates)
	switch {
	case n == 0:
		return nil, RejectEmpty
	case n > cfg.MaxCandidates:
		return nil, RejectOversized
	case cs.MaxSelections < 1 || int(cs.MaxSelections) > n:
		return nil, RejectInvalid
	}
	builtAt, ok := parseTimestamp(string(cs.BuiltAt))
	if !ok {
		return nil, RejectStale
	}
	now := e.clk.Now()
	if now.Sub(builtAt) > cfg.MaxCandidateAge || builtAt.Sub(now) > cfg.MaxClockSkew {
		return nil, RejectStale
	}

	var allowed map[string]struct{}
	if req.DecisionType == gen.DecisionTypeActionSelection {
		// Choosing among already-authorised actions: a candidate must be one of the
		// session's permitted next actions (the action kind or its capability id).
		allowed = make(map[string]struct{}, len(req.AllowedNextActions)*2)
		for _, a := range req.AllowedNextActions {
			allowed[string(a.Action)] = struct{}{}
			if a.CapabilityID != nil {
				allowed[string(*a.CapabilityID)] = struct{}{}
			}
		}
	}
	seen := make(map[string]struct{}, n)
	for i := range cs.Candidates {
		c := &cs.Candidates[i]
		id := string(c.CandidateID)
		if id == "" {
			return nil, RejectInvalid
		}
		if len(c.Label) > cfg.MaxLabelLen || (c.Text != nil && len(*c.Text) > cfg.MaxTextLen) {
			return nil, RejectOversized
		}
		if _, dup := seen[id]; dup {
			return nil, RejectDuplicate
		}
		seen[id] = struct{}{}
		if allowed != nil {
			if _, ok := allowed[id]; !ok {
				return nil, RejectUnauthorized
			}
		}
	}
	for i := range cs.Candidates {
		if err := e.resolver.Exposed(ctx, scope, cs.Candidates[i]); err != nil {
			switch contracts.CodeOf(err) {
			case contracts.ErrScopeViolation, contracts.ErrScopeMissing:
				return nil, RejectCrossTenant
			case contracts.ErrStale, contracts.ErrExpired:
				return nil, RejectStale
			}
			return nil, RejectUnauthorized
		}
	}
	return seen, RejectNone
}

// shapeSupported reports whether the request is one the scorer is qualified for: a
// supported decision type, schema version 1 and a bounded state of scalars within the
// caps. Anything else is an unsupported shape, decided before the provider is called.
func (e *Engine) shapeSupported(req gen.DecisionRequest) bool {
	cfg := e.cfg
	if req.SchemaVersion != 1 || !req.DecisionType.Valid() || !cfg.supports(req.DecisionType) {
		return false
	}
	if len(req.BoundedState) > cfg.MaxStateKeys {
		return false
	}
	for _, v := range req.BoundedState {
		switch s := v.(type) {
		case nil, bool, int, int32, int64, float32, float64:
		case string:
			if len(s) > cfg.MaxStateStringLen {
				return false
			}
		default:
			return false
		}
	}
	return true
}
