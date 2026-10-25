package decision

import (
	"context"
	"strings"
	"sync"

	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// Kinds of candidate this layer produces; they are the Resolved.Kind of the resolver.
const (
	kindCapability = "capability"
	kindAct        = "dialogue_act"
)

// actPrefix marks a dialogue-act candidate id ("act:cancel"). Ids of capabilities never
// take this shape in the resolver: a capability id that starts with it is treated as an
// act id and refused when the act is unknown.
const actPrefix = "act:"

// Resolver is the CandidateResolver over the real deterministic side: the policy gateway
// decides which capabilities the verified scope may see, against the same active package
// the dialogue engine serves the turn from. It is built from nothing the scorer or the
// request supplies, so a candidate the request lists but the gateway does not expose is
// refused before any scorer sees it, and a selected id is re-resolved against the gateway
// once more before the pipeline may use it.
//
// Cost: one Gateway.Expose (one policy record) shared by the guard's Exposed checks of a
// request through the exposure memo of the context, and one more for Resolve, which never
// reuses it: a selected id is re-resolved against the state as it is after the scorer ran.
type Resolver struct {
	gw   *policy.Gateway
	pkgs dialogue.PackageSource
}

// NewResolver returns the resolver. Both arguments are required.
func NewResolver(gw *policy.Gateway, pkgs dialogue.PackageSource) (*Resolver, error) {
	if gw == nil || pkgs == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "decision: a resolver needs a policy gateway and a package source")
	}
	return &Resolver{gw: gw, pkgs: pkgs}, nil
}

var _ core.CandidateResolver = (*Resolver)(nil)

// exposure is what the gateway let a scope see at one moment, and the package version it
// was decided against.
type exposure struct {
	key     string
	hash    string
	visible map[string]policy.Decision
}

type exposureMemo struct {
	mu sync.Mutex
	x  *exposure
}

type exposureKey struct{}

// withExposureMemo lets every resolver call of one decision share one Gateway.Expose.
func withExposureMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, exposureKey{}, &exposureMemo{})
}

func scopeKey(s contracts.Scope) string {
	return strings.Join([]string{
		s.DeploymentID, s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID,
		strings.Join(s.Roles, ","), strings.Join(s.CapabilityScopes, ","),
	}, "\x00")
}

// exposure returns what the gateway shows the scope. With memo it may reuse the decision
// already made for this context (the guard's checks of one request); without it, it always
// asks the gateway again (re-resolution of a selected id must see the state as it is now).
func (r *Resolver) exposure(ctx context.Context, scope contracts.Scope, memo bool) (*exposure, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	key := scopeKey(scope)
	m, _ := ctx.Value(exposureKey{}).(*exposureMemo)
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if memo && m.x != nil && m.x.key == key {
			return m.x, nil
		}
	}
	pkg, err := r.pkgs.Package(ctx, scope.ApplicationID)
	if err != nil {
		return nil, err
	}
	if pkg == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "decision: no active package")
	}
	ex := r.gw.Expose(ctx, policy.ExposeRequest{Scope: scope, Package: pkg, Binding: policy.BindingOf(scope)})
	if ex.Code != "" {
		return nil, contracts.New(ex.Code, "decision: the policy gateway exposes nothing to this scope")
	}
	x := &exposure{key: key, hash: pkg.ContentHash(), visible: make(map[string]policy.Decision, len(ex.Visible))}
	for _, d := range ex.Visible {
		if d.Allowed {
			x.visible[d.Record.CapabilityID] = d
		}
	}
	if m != nil {
		m.x = x
	}
	return x, nil
}

// Exposed implements core.CandidateResolver: the candidate must be a capability the policy
// gateway shows this scope now, at the data version the request was built from, or one of
// the dialogue acts this layer defines.
func (r *Resolver) Exposed(ctx context.Context, scope contracts.Scope, c gen.CandidateSetCandidate) error {
	id := string(c.CandidateID)
	if strings.HasPrefix(id, actPrefix) {
		if _, ok := actByID(id); !ok {
			return contracts.New(contracts.ErrPermissionDenied, "decision: unknown dialogue act")
		}
		return nil
	}
	x, err := r.exposure(ctx, scope, true)
	if err != nil {
		return err
	}
	if _, ok := x.visible[id]; !ok {
		return contracts.New(contracts.ErrPermissionDenied, "decision: the capability is not exposed to this scope")
	}
	if c.DataVersion != nil && string(*c.DataVersion) != x.hash {
		return contracts.New(contracts.ErrStale, "decision: the package changed since the candidate set was built")
	}
	return nil
}

// Resolve implements core.CandidateResolver: it re-resolves a selected id against the
// gateway and the active package and refuses anything the gateway does not expose.
func (r *Resolver) Resolve(ctx context.Context, scope contracts.Scope, id string) (core.Resolved, error) {
	if strings.HasPrefix(id, actPrefix) {
		if _, ok := actByID(id); !ok {
			return core.Resolved{}, contracts.New(contracts.ErrPermissionDenied, "decision: unknown dialogue act")
		}
		return core.Resolved{ID: id, Kind: kindAct, Version: "1"}, nil
	}
	x, err := r.exposure(ctx, scope, false)
	if err != nil {
		return core.Resolved{}, err
	}
	if _, ok := x.visible[id]; !ok {
		return core.Resolved{}, contracts.New(contracts.ErrPermissionDenied, "decision: the capability is not exposed to this scope")
	}
	return core.Resolved{ID: id, Kind: kindCapability, Version: x.hash}, nil
}
