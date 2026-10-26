package oauth

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// TokenRequest asks for the access value of a connection, for the length of one provider call.
type TokenRequest struct {
	Ref providers.CredentialRefID
	// Scopes the call needs; each must be among the permitted scopes of the reference.
	Scopes []string
	// Purpose is the short audit code ("inference"); empty means "inference".
	Purpose string
	// ForceRefresh refreshes first even if the access value is not near expiry: for a caller that got a 401
	// from the provider API (the access value was revoked or rotated provider-side).
	ForceRefresh bool
}

// Token returns the access value of a connection, refreshing it first when it is within the refresh skew of
// its expiry. The caller puts it on the wire and drops it; the engine keeps nothing. Concurrent callers of one
// reference share a single refresh request. A connection the provider no longer honours is flagged
// (reauthorization_required) and every later call fails closed with a typed error: no other credential and no
// other provider is tried.
func (e *Engine) Token(ctx context.Context, scope contracts.Scope, req TokenRequest) (credentials.Secret, error) {
	ref, err := e.cr.Get(ctx, scope, req.Ref)
	if err != nil {
		return "", err
	}
	purpose := req.Purpose
	if purpose == "" {
		purpose = "inference"
	}
	use := credentials.Use{Provider: ref.Binding.ProviderID, Scopes: req.Scopes, Purpose: purpose}
	if ref.Kind != providers.CredentialKindOauth {
		return "", fail(contracts.ErrValidationFailed, ReasonNotOAuth, "oauth: the reference is not an OAuth connection")
	}
	expiry, hasExpiry := parseExpiry(ref)
	now := e.clk.Now()
	near := hasExpiry && expiry.Sub(now) <= e.skew
	need := req.ForceRefresh || near || ref.Status == providers.CredentialRefStatusExpired
	if ref.Status != providers.CredentialRefStatusActive && ref.Status != providers.CredentialRefStatusExpired {
		return e.cr.Resolve(ctx, scope, string(req.Ref), use) // a typed refusal: revoked, invalid, quarantined, ...
	}
	if !need {
		return e.cr.Resolve(ctx, scope, string(req.Ref), use)
	}
	if err := e.refresh(ctx, scope, ref.Binding.ProviderID, req.Ref); err != nil {
		stillValid := hasExpiry && now.Before(expiry) && !req.ForceRefresh
		if stillValid && ReasonOf(err) == ReasonProviderDown {
			// The provider could not be reached but the access value is still valid: use it.
			return e.cr.Resolve(ctx, scope, string(req.Ref), use)
		}
		return "", err
	}
	return e.cr.Resolve(ctx, scope, string(req.Ref), use)
}

func parseExpiry(ref providers.CredentialRef) (time.Time, bool) {
	if ref.Times.ExpiresAt == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(*ref.Times.ExpiresAt))
	return t, err == nil
}

// flight is one refresh in progress.
type flight struct {
	done chan struct{}
	err  error
}

// refresh exchanges the refresh value for a new access value (and a new refresh value where the provider
// rotates it) and stores both through the credential service. It is single-flight per reference.
func (e *Engine) refresh(ctx context.Context, scope contracts.Scope, prov providers.ProviderID, ref providers.CredentialRefID) error {
	key := scope.ApplicationID + "|" + scope.TenantID + "|" + string(ref)
	e.mu.Lock()
	if f, ok := e.flights[key]; ok {
		e.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return fail(contracts.ErrTimeout, ReasonProviderDown, "oauth: the refresh did not finish in time")
		}
	}
	f := &flight{done: make(chan struct{})}
	e.flights[key] = f
	e.mu.Unlock()

	// The refresh outlives the caller that started it: the others wait on it.
	f.err = e.doRefresh(context.WithoutCancel(ctx), scope, prov, ref)
	e.mu.Lock()
	delete(e.flights, key)
	e.mu.Unlock()
	close(f.done)
	return f.err
}

// refLock is the lock of one reference's tokens, with the number of callers holding or waiting for it.
type refLock struct {
	mu sync.Mutex
	n  int
}

// lockRef takes the lock of a reference's tokens and returns its release. A refresh holds it from reading the
// refresh value to storing what the provider returned, and a re-authorization while it stores its tokens, so a
// refresh that was in flight can never overwrite the tokens of a newer grant: whichever comes second works from
// what the first stored.
func (e *Engine) lockRef(scope contracts.Scope, ref providers.CredentialRefID) (unlock func()) {
	key := scope.ApplicationID + "|" + scope.TenantID + "|" + string(ref)
	e.mu.Lock()
	l := e.refLocks[key]
	if l == nil {
		l = &refLock{}
		e.refLocks[key] = l
	}
	l.n++
	e.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		e.mu.Lock()
		if l.n--; l.n == 0 {
			delete(e.refLocks, key)
		}
		e.mu.Unlock()
	}
}

func (e *Engine) doRefresh(ctx context.Context, scope contracts.Scope, prov providers.ProviderID, ref providers.CredentialRefID) error {
	unlock := e.lockRef(scope, ref)
	defer unlock()
	p, err := e.provider(prov)
	if err != nil {
		return err
	}
	rv, err := e.cr.Resolve(ctx, scope, string(ref), credentials.Use{Provider: prov, Purpose: "oauth.refresh", Refresh: true})
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rv.Reveal())
	if p.Resource != "" {
		form.Set("resource", p.Resource)
	}
	ts, err := e.tokenCall(ctx, scope, p, form)
	if err != nil {
		xe, ok := err.(*exchangeError)
		if !ok {
			return err
		}
		switch xe.code {
		case "invalid_grant": // the refresh value was revoked, expired or already used: detected, fail closed
			e.flag(ctx, scope, ref, providers.CredentialRefStatusReauthorizationRequired)
			return fail(contracts.ErrUnauthenticated, ReasonReauthRequired, "oauth: the provider no longer honours this connection; authorize again")
		case "invalid_client", "unauthorized_client":
			e.flag(ctx, scope, ref, providers.CredentialRefStatusInvalid)
		}
		return xe.typed()
	}
	expires := e.expiryOf(ts)
	rot := credentials.Rotation{Key: ts.access, Refresh: ts.refresh, ExpiresAt: &expires} // an empty refresh keeps the one held
	if _, err := e.cr.Rotate(ctx, scope, ref, rot); err != nil {
		// A provider that rotates refresh values may have retired the old one already: the connection cannot be
		// trusted to renew again, so it is flagged rather than left looking healthy.
		e.flag(ctx, scope, ref, providers.CredentialRefStatusReauthorizationRequired)
		return err
	}
	e.log.Info("oauth connection refreshed", "provider", string(prov), "tenant", scope.TenantID, "ref", string(ref), "rotated", !ts.refresh.Empty())
	return nil
}

func (e *Engine) flag(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID, st providers.CredentialRefStatus) {
	if _, err := e.cr.Flag(ctx, scope, ref, credentials.Flag{Status: st}); err != nil {
		e.log.Warn("oauth connection could not be flagged", "ref", string(ref), "reason", string(credentials.ReasonOf(err)))
		return
	}
	e.log.Warn("oauth connection flagged", "ref", string(ref), "status", string(st))
}
