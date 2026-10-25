package decision

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// CandidateResolver is the seam to the deterministic application adapter and policy
// layer. The real one arrives with dialogue integration (D-04); it lives behind this
// interface so this package imports no adapter, and above all no commit path.
//
// Both methods are authoritative and deterministic. The scorer never feeds them
// anything but candidate ids it was given.
type CandidateResolver interface {
	// Exposed reports whether the candidate may be shown to a scorer for this scope
	// now: it belongs to the scope's tenant, the principal may see it and its data
	// version is current. A nil error means yes. contracts.ErrScopeViolation (or
	// scope_missing) marks a cross-tenant candidate, ErrStale and ErrExpired a stale
	// one, and every other error is an unauthorised candidate.
	Exposed(ctx context.Context, scope contracts.Scope, c gen.CandidateSetCandidate) error
	// Resolve re-resolves a candidate id the scorer selected, against the
	// application, before anything may use it. Any error refuses the selection.
	Resolve(ctx context.Context, scope contracts.Scope, candidateID string) (Resolved, error)
}

// Resolved is what the deterministic adapter says a selected candidate id is. It is a
// fact from the adapter, not from the scorer. Kind and Version are opaque to this
// package.
type Resolved struct {
	ID      string
	Kind    string
	Version string
}
