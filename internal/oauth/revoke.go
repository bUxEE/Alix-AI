package oauth

import (
	"context"
	"net/http"
	"net/url"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// DisconnectResult says what a disconnect did. The local revocation always happens; revocation at the provider
// is attempted first, where the provider has a revocation endpoint and the tokens could be read.
type DisconnectResult struct {
	Ref providers.CredentialRef `json:"ref"`
	// ProviderRevoked: the provider confirmed the revocation of every token the engine held.
	ProviderRevoked bool `json:"provider_revoked"`

	err error // the local revocation failed
}

// readTokens fetches the held values for revocation at the provider. A value that cannot be released (the
// reference is flagged, expired, revoked) is simply not returned.
func (e *Engine) readTokens(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID, prov providers.ProviderID) (access, refresh credentials.Secret) {
	if v, err := e.cr.Resolve(ctx, scope, string(ref), credentials.Use{Provider: prov, Purpose: "oauth.revoke"}); err == nil {
		access = v
	}
	if v, err := e.cr.Resolve(ctx, scope, string(ref), credentials.Use{Provider: prov, Purpose: "oauth.revoke", Refresh: true}); err == nil {
		refresh = v
	}
	return access, refresh
}

// revokeAtProvider revokes tokens at the provider's RFC 7009 endpoint (refresh value first: it ends the whole
// grant at most providers). It reports whether every token it was given was confirmed revoked; with no
// endpoint, or nothing to revoke, it reports false.
func (e *Engine) revokeAtProvider(ctx context.Context, scope contracts.Scope, p ProviderConfig, access, refresh credentials.Secret) bool {
	if p.RevocationEndpoint == "" || (access.Empty() && refresh.Empty()) {
		return false
	}
	ok := true
	for _, t := range []struct {
		v    credentials.Secret
		hint string
	}{{refresh, "refresh_token"}, {access, "access_token"}} {
		if t.v.Empty() {
			continue
		}
		form := url.Values{}
		form.Set("token", t.v.Reveal())
		form.Set("token_type_hint", t.hint)
		user, pass, err := e.clientAuth(ctx, scope, p, form)
		if err != nil {
			ok = false
			continue
		}
		_, status, err := e.post(ctx, scope, p.RevocationEndpoint, form, user, pass)
		if err != nil || status != http.StatusOK {
			ok = false
			e.log.Warn("oauth revocation at the provider failed", "provider", string(p.ID), "tenant", scope.TenantID)
		}
	}
	return ok
}

// Disconnect ends a connection: the tokens are revoked at the provider (where it offers revocation), then the
// reference is revoked in the credential service, which deletes the stored values. The local revocation
// happens whatever the provider answered, so a provider outage never leaves a usable connection behind. The
// caller authorises the disconnect; this does no role check.
func (e *Engine) Disconnect(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID) (DisconnectResult, error) {
	cur, err := e.cr.Get(ctx, scope, ref)
	if err != nil {
		return DisconnectResult{}, err
	}
	if cur.Kind != providers.CredentialKindOauth {
		return DisconnectResult{}, fail(contracts.ErrValidationFailed, ReasonNotOAuth, "oauth: the reference is not an OAuth connection")
	}
	res := e.disconnect(ctx, scope, cur)
	return res, res.err
}

func (e *Engine) disconnect(ctx context.Context, scope contracts.Scope, cur providers.CredentialRef) DisconnectResult {
	res := DisconnectResult{}
	if p, ok := e.prov[cur.Binding.ProviderID]; ok && cur.Status != providers.CredentialRefStatusRevoked {
		access, refresh := e.readTokens(ctx, scope, cur.RefID, p.ID)
		res.ProviderRevoked = e.revokeAtProvider(ctx, scope, p, access, refresh)
	}
	revoked, err := e.cr.Revoke(ctx, scope, cur.RefID)
	res.Ref = revoked
	if err != nil {
		e.log.Error("oauth disconnect could not revoke locally", "ref", string(cur.RefID), "reason", string(credentials.ReasonOf(err)))
		if revoked.RefID == "" {
			res.Ref = cur
		}
		res.err = err
	}
	return res
}

// DisconnectTenant is tenant removal or a security reset: every OAuth connection of the scope's tenant is
// revoked at its provider and locally, then everything else the tenant holds (manual keys) is revoked in the
// credential service. It returns how many references ended up revoked and the first error.
func (e *Engine) DisconnectTenant(ctx context.Context, scope contracts.Scope) (int, error) {
	refs, err := e.cr.List(ctx, scope, "")
	if err != nil {
		return 0, err
	}
	var first error
	n := 0
	for _, r := range refs {
		if r.Kind != providers.CredentialKindOauth || r.Status == providers.CredentialRefStatusRevoked {
			continue
		}
		if res := e.disconnect(ctx, scope, r); res.err != nil {
			if first == nil {
				first = res.err
			}
			continue
		}
		n++
	}
	m, err := e.cr.RevokeTenant(ctx, scope)
	if first == nil {
		first = err
	}
	return n + m, first
}
