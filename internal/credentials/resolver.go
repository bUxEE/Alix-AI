package credentials

import (
	"context"
	"slices"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ResolverOptions fix what a Resolver is for.
type ResolverOptions struct {
	// Provider is the provider whose calls the Resolver serves. Required: a credential is bound to one
	// provider.
	Provider providers.ProviderID
	// Scopes are the provider scopes those calls need.
	Scopes []string
	// Purpose is the audit purpose code; "" means "provider_call".
	Purpose string
}

// Resolver resolves an opaque credential reference to a secret that is held in memory only for the
// duration of the call: it caches nothing, so a rotation or a revocation is effective on the next call.
// It is bound, when it is built, to one verified scope (application, tenant, ...) and one provider,
// because the interfaces adapters declare (adapters/openapi's SecretProvider) pass a reference and no
// scope, and the repository has no default scope. A composition root builds one per adapter and
// application; a reference of another tenant, application, deployment or provider is refused.
// It is safe for concurrent use.
type Resolver struct {
	svc   *Service
	scope contracts.Scope
	use   Use
}

// Resolver returns a Resolver for the scope and provider. The scope is validated now.
func (s *Service) Resolver(scope contracts.Scope, opt ResolverOptions) (*Resolver, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := validProvider(opt.Provider); err != nil {
		return nil, err
	}
	scopes, err := validScopes(opt.Scopes)
	if err != nil {
		return nil, err
	}
	purpose := opt.Purpose
	if purpose == "" {
		purpose = "provider_call"
	}
	if !purposeRE.MatchString(purpose) {
		return nil, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the purpose is malformed")
	}
	return &Resolver{svc: s, scope: scope.Clone(), use: Use{Provider: opt.Provider, Scopes: slices.Clone(scopes), Purpose: purpose}}, nil
}

// Resolve returns the secret the reference names, or a typed error and no value. Every call is audited.
func (r *Resolver) Resolve(ctx context.Context, ref string) (Secret, error) {
	return r.svc.Resolve(ctx, r.scope, ref, r.use)
}

// SecretProvider adapts a Resolver to an adapter's own secret type: any interface of the shape
//
//	Secret(ctx context.Context, ref string) (S, error)
//
// where S is a string kind that redacts itself, such as adapters/openapi's SecretProvider with
// openapi.Secret. A composition root writes, once:
//
//	secrets := credentials.AsSecretProvider[openapi.Secret](resolver)
//
// and passes it as the adapter's Secrets. The conversion is the only copy of the value this package
// makes, and it is for the caller to put on the wire.
type SecretProvider[S ~string] struct{ r *Resolver }

// AsSecretProvider wraps r.
func AsSecretProvider[S ~string](r *Resolver) SecretProvider[S] { return SecretProvider[S]{r: r} }

// Secret resolves the reference.
func (p SecretProvider[S]) Secret(ctx context.Context, ref string) (S, error) {
	if p.r == nil {
		return "", fail(contracts.ErrInternal, ReasonBadRequest, "credentials: the resolver is not wired")
	}
	v, err := p.r.Resolve(ctx, ref)
	if err != nil {
		return "", err
	}
	return S(v.Reveal()), nil
}
