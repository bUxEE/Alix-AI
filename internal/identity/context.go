package identity

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
)

type ctxKey struct{}

// WithVerified returns ctx carrying v. It refuses a Verified whose Scope does
// not validate, so an invalid or default scope can never ride a context.
func WithVerified(ctx context.Context, v *Verified) (context.Context, error) {
	if v == nil {
		return ctx, contracts.New(contracts.ErrScopeMissing, "no verified context")
	}
	if err := v.Scope.Validate(); err != nil {
		return ctx, err
	}
	cp := *v
	cp.Scope = v.Scope.Clone()
	return context.WithValue(ctx, ctxKey{}, &cp), nil
}

// WithScope returns ctx carrying scope, for in-process paths that already hold
// a verified Scope (a background job resuming the scope it was started with, a
// test). It validates first and returns the scope's error for an invalid one.
func WithScope(ctx context.Context, scope contracts.Scope) (context.Context, error) {
	return WithVerified(ctx, &Verified{Scope: scope})
}

// VerifiedFromContext returns the Verified context the middleware stored, as a
// copy the caller may modify.
func VerifiedFromContext(ctx context.Context) (*Verified, bool) {
	v, ok := ctx.Value(ctxKey{}).(*Verified)
	if !ok || v == nil {
		return nil, false
	}
	cp := *v
	cp.Scope = v.Scope.Clone()
	return &cp, true
}

// ScopeFromContext returns the verified Scope in ctx, as a copy.
func ScopeFromContext(ctx context.Context) (contracts.Scope, bool) {
	v, ok := VerifiedFromContext(ctx)
	if !ok {
		return contracts.Scope{}, false
	}
	return v.Scope, true
}

// RequireScope returns the verified Scope in ctx or a scope_missing error. It
// is what request-scoped code calls first: there is no default scope.
func RequireScope(ctx context.Context) (contracts.Scope, error) {
	s, ok := ScopeFromContext(ctx)
	if !ok {
		return contracts.Scope{}, contracts.New(contracts.ErrScopeMissing, "request has no verified scope")
	}
	return s, nil
}
