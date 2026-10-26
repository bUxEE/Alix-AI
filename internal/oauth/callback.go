package oauth

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// CallbackRequest is what the gateway's callback route hands over: the parameters of the redirect the browser
// followed, and the redirect URI they arrived on (scheme, host, port and path, without the query).
type CallbackRequest struct {
	State string
	Code  string
	// Error is the "error" parameter, when the user declined or the provider refused. The error description is
	// not passed on: it is free text.
	Error string
	// Iss is the RFC 9207 issuer parameter, when present.
	Iss         string
	RedirectURI string
}

// CallbackFromQuery builds a CallbackRequest from the query of the callback URL.
func CallbackFromQuery(redirectURI string, q url.Values) CallbackRequest {
	return CallbackRequest{State: q.Get("state"), Code: q.Get("code"), Error: q.Get("error"), Iss: q.Get("iss"), RedirectURI: redirectURI}
}

// Connection is the outcome of a completed authorization: only safe facts, never a token.
type Connection struct {
	Ref providers.CredentialRef `json:"ref"`
	// Scopes are the provider scopes consented to and now permitted for the reference.
	Scopes       []string  `json:"scopes"`
	ConsentedAt  time.Time `json:"consented_at"`
	Reauthorized bool      `json:"reauthorized"`
	Switched     bool      `json:"switched"`
}

func sameOwner(a, b contracts.Scope) bool {
	return a.DeploymentID == b.DeploymentID && a.ApplicationID == b.ApplicationID && a.TenantID == b.TenantID &&
		a.WorkspaceID == b.WorkspaceID && a.PrincipalID == b.PrincipalID
}

// Complete consumes the callback: it checks state (single use, expiring, constant-time), scope, redirect
// binding, issuer and provider error, exchanges the code with the PKCE verifier, validates the ID token and
// nonce of an OIDC provider, and stores the tokens through the credential service. The state is spent by the
// first call, whatever its outcome, so a replayed callback fails. Nothing is stored unless every check passed;
// tokens a provider issued for a flow that then failed are revoked at the provider where it offers revocation.
func (e *Engine) Complete(ctx context.Context, scope contracts.Scope, cb CallbackRequest) (Connection, error) {
	if err := scope.Validate(); err != nil {
		return Connection{}, err
	}
	f, err := e.take(cb.State)
	if err != nil {
		e.log.Warn("oauth callback refused", "tenant", scope.TenantID, "reason", string(ReasonOf(err)))
		return Connection{}, err
	}
	conn, err := e.completeTaken(ctx, scope, f, cb)
	if err != nil {
		e.markFlowFailed(pendingKey(f), ReasonOf(err))
		return Connection{}, err
	}
	e.markFlowCompleted(pendingKey(f), conn)
	return conn, nil
}

func (e *Engine) completeTaken(ctx context.Context, scope contracts.Scope, f *pendingFlow, cb CallbackRequest) (Connection, error) {
	refuse := func(err error) (Connection, error) {
		e.log.Warn("oauth callback refused", "provider", string(f.provider), "tenant", f.scope.TenantID, "reason", string(ReasonOf(err)))
		return Connection{}, err
	}
	if !sameOwner(f.scope, scope) {
		return refuse(fail(contracts.ErrScopeViolation, ReasonScopeMismatch, "oauth: the authorization belongs to another application, tenant, workspace or principal"))
	}
	p := e.prov[f.provider]
	if cb.RedirectURI != f.redirectURI {
		return refuse(fail(contracts.ErrValidationFailed, ReasonRedirectRejected, "oauth: the callback did not arrive on the redirect URI the flow was bound to"))
	}
	if cb.Error != "" {
		if cb.Error == "access_denied" {
			return refuse(fail(contracts.ErrPermissionDenied, ReasonAccessDenied, "oauth: the user declined the authorization"))
		}
		code := "invalid_response"
		if oauthErrorRE.MatchString(cb.Error) {
			code = cb.Error
		}
		return refuse(fail(contracts.ErrUnauthenticated, ReasonProviderError, "oauth: the provider refused the authorization").WithDetails("oauth_error", code))
	}
	if (cb.Iss != "" && p.Issuer != "" && cb.Iss != p.Issuer) || (p.RequireIssParam && cb.Iss == "") {
		return refuse(fail(contracts.ErrUnauthenticated, ReasonIssuerMismatch, "oauth: the callback names another issuer"))
	}
	if !validCode(cb.Code) {
		return refuse(fail(contracts.ErrValidationFailed, ReasonBadRequest, "oauth: the callback has no valid code"))
	}
	if !e.clk.Now().Before(f.expires) {
		return refuse(fail(contracts.ErrExpired, ReasonStateExpired, "oauth: the authorization expired; start again"))
	}

	if p.GrantKind == GrantIssuedKey {
		return e.completeIssuedKey(ctx, scope, p, f, cb.Code, refuse)
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", cb.Code)
	form.Set("redirect_uri", f.redirectURI)
	form.Set("code_verifier", f.verifier.Reveal())
	if f.resource != "" {
		form.Set("resource", f.resource)
	}
	// Use the flow's effective client id (SIWC issued id) for the token request.
	pTok := p
	if f.clientID != "" {
		pTok.ClientID = f.clientID
	}
	ts, err := e.tokenCall(ctx, scope, pTok, form)
	if err != nil {
		return refuse(asTyped(err))
	}
	abort := func(err error) (Connection, error) {
		e.revokeAtProvider(ctx, scope, p, ts.access, ts.refresh)
		return refuse(err)
	}
	var claims map[string]any
	if p.OIDC {
		if ts.idToken.Empty() {
			return abort(fail(contracts.ErrUnauthenticated, ReasonIDToken, "oauth: the provider returned no ID token"))
		}
		audClient := f.clientID
		if audClient == "" {
			audClient = p.ClientID
		}
		pAud := p
		pAud.ClientID = audClient
		if claims, err = e.validateIDToken(pAud, ts.idToken, f.nonce); err != nil {
			return abort(err)
		}
	}
	permitted, excess := splitGranted(f.scopes, ts.scopes)
	if len(permitted) == 0 {
		return abort(fail(contracts.ErrPermissionDenied, ReasonAccessDenied, "oauth: no requested scope was granted"))
	}
	acct := accountFrom(p, ts.members, claims)
	switched := false
	if f.reauth != "" {
		switched = !sameAccount(f.prior, acct)
		if switched && !f.switchAcct {
			return abort(fail(contracts.ErrScopeViolation, ReasonAccountMismatch,
				"oauth: another account answered; switching accounts must be started as an account switch"))
		}
	}
	expires := e.expiryOf(ts)
	label := f.label
	if label == "" {
		label = string(p.ID)
	}
	var ref providers.CredentialRef
	if f.reauth == "" {
		grant := credentials.OAuthGrant{ProviderID: p.ID, Label: label, Scopes: permitted,
			Account: acct, Access: ts.access, Refresh: ts.refresh, ExpiresAt: &expires}
		// SIWC ChatGPT plan usage, and every principal-bucket OAuth start (ADR 0041), bind to this principal.
		if credentials.IsPrincipalCredentialTenant(scope.TenantID) || slices.Contains(permitted, "chatgpt.tokens.use.direct") {
			grant.OwnerPrincipal = scope.PrincipalID
		}
		ref, err = e.cr.AddOAuth(ctx, scope, grant)
	} else {
		unlock := e.lockRef(scope, f.reauth)
		var oldAccess, oldRefresh credentials.Secret
		if switched {
			oldAccess, oldRefresh = e.readTokens(ctx, scope, f.reauth, p.ID)
		}
		rot := credentials.Rotation{Key: ts.access, Refresh: ts.refresh, ExpiresAt: &expires, Scopes: permitted}
		if acct.AccountID != nil || acct.OrganizationID != nil || acct.ProjectID != nil {
			rot.Account = &acct
		}
		ref, err = e.cr.Rotate(ctx, scope, f.reauth, rot)
		unlock()
		if err == nil && switched {
			e.revokeAtProvider(ctx, scope, p, oldAccess, oldRefresh)
		}
	}
	if err != nil {
		e.log.Error("oauth tokens could not be stored", "provider", string(p.ID), "tenant", scope.TenantID, "reason", string(credentials.ReasonOf(err)))
		e.revokeAtProvider(ctx, scope, p, ts.access, ts.refresh)
		return Connection{}, err
	}
	if issued := issuedClientID(ts.members); issued != "" {
		// Persist the SIWC issued client id as the account project id (safe identifier); composition
		// roots that need it on re-auth read it from the credential account fields.
		if ref.Account.ProjectID == nil {
			ref.Account.ProjectID = &issued
			_, _ = e.cr.Rotate(ctx, scope, ref.RefID, credentials.Rotation{Key: ts.access, Refresh: ts.refresh, ExpiresAt: &expires, Account: &ref.Account})
		}
	}
	if len(excess) > 0 {
		q, ferr := e.cr.Flag(ctx, scope, ref.RefID, credentials.Flag{Status: providers.CredentialRefStatusQuarantined,
			Warning: providers.ConnectionWarningExcessivePermissionsReported})
		if ferr != nil {
			e.log.Error("oauth over-granted connection could not be quarantined", "provider", string(p.ID), "tenant", scope.TenantID, "reason", string(credentials.ReasonOf(ferr)))
			_, _ = e.cr.Revoke(ctx, scope, ref.RefID)
			e.revokeAtProvider(ctx, scope, p, ts.access, ts.refresh)
			return Connection{}, ferr
		}
		ref = q
	}
	e.log.Info("oauth authorization completed", "provider", string(p.ID), "tenant", scope.TenantID, "ref", string(ref.RefID), "switched", switched)
	return Connection{Ref: ref, Scopes: permitted, ConsentedAt: e.clk.Now(), Reauthorized: f.reauth != "", Switched: switched}, nil
}

func issuedClientID(members map[string]any) string {
	if members == nil {
		return ""
	}
	for _, k := range []string{"client_id", "issued_client_id"} {
		if s, ok := members[k].(string); ok && strings.HasPrefix(s, "oaiapp_") {
			return s
		}
	}
	return ""
}

func (e *Engine) completeIssuedKey(ctx context.Context, scope contracts.Scope, p ProviderConfig, f *pendingFlow, code string, refuse func(error) (Connection, error)) (Connection, error) {
	key, err := e.exchangeIssuedKey(ctx, scope, p, code, f.verifier)
	if err != nil {
		return refuse(asTyped(err))
	}
	label := f.label
	if label == "" {
		label = string(p.ID)
	}
	scopes := f.scopes
	if len(scopes) == 0 {
		scopes = []string{"api"}
	}
	issued := credentials.OAuthIssuedKeyGrant{ProviderID: p.ID, Label: label, Scopes: scopes, Key: key}
	if credentials.IsPrincipalCredentialTenant(scope.TenantID) {
		issued.OwnerPrincipal = scope.PrincipalID
	}
	ref, err := e.cr.AddOAuthIssuedKey(ctx, scope, issued)
	if err != nil {
		e.log.Error("oauth issued key could not be stored", "provider", string(p.ID), "tenant", scope.TenantID, "reason", string(credentials.ReasonOf(err)))
		return Connection{}, err
	}
	e.log.Info("oauth issued-key authorization completed", "provider", string(p.ID), "tenant", scope.TenantID, "ref", string(ref.RefID))
	return Connection{Ref: ref, Scopes: scopes, ConsentedAt: e.clk.Now()}, nil
}

// asTyped turns an exchange failure into the typed error; other errors pass through.
func asTyped(err error) error {
	if xe, ok := err.(*exchangeError); ok {
		return xe.typed()
	}
	return err
}

func validCode(c string) bool {
	if c == "" || len(c) > 2048 {
		return false
	}
	return !strings.ContainsFunc(c, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}

// splitGranted returns the requested scopes that were granted (all of them when the response named none) and
// the granted scopes that were not requested.
func splitGranted(requested, granted []string) (permitted, excess []string) {
	if granted == nil {
		return slices.Clone(requested), nil
	}
	for _, s := range requested {
		if slices.Contains(granted, s) {
			permitted = append(permitted, s)
		}
	}
	for _, s := range granted {
		if !slices.Contains(requested, s) {
			excess = append(excess, s)
		}
	}
	return permitted, excess
}

// sameAccount: the provider account identifier is the same. An account known on one side only is a change
// (fail closed). When neither side knows one the provider exposes no account to compare, and there is
// nothing to protect.
func sameAccount(a, b providers.SafeAccount) bool {
	if a.AccountID == nil || b.AccountID == nil {
		return a.AccountID == nil && b.AccountID == nil
	}
	return *a.AccountID == *b.AccountID
}
