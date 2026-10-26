package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Defaults.
const (
	DefaultStateTTL       = 10 * time.Minute
	DefaultRefreshSkew    = 60 * time.Second
	DefaultRequestTimeout = 15 * time.Second
	// DefaultAccessTTL is assumed when a token response has no expires_in: the credential service needs an
	// expiry to renew from, and a short assumption only costs one early refresh.
	DefaultAccessTTL    = time.Hour
	DefaultMaxPending   = 1024
	DefaultMaxPerTenant = 16
)

// Credentials is what the engine needs from the credential service (*credentials.Service satisfies it).
// Tokens go in through AddOAuth / AddOAuthIssuedKey and Rotate and come out through Resolve, nowhere else.
// The service does no role check: the caller authorises management calls.
type Credentials interface {
	AddOAuth(ctx context.Context, scope contracts.Scope, g credentials.OAuthGrant) (providers.CredentialRef, error)
	AddOAuthIssuedKey(ctx context.Context, scope contracts.Scope, g credentials.OAuthIssuedKeyGrant) (providers.CredentialRef, error)
	Rotate(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID, rot credentials.Rotation) (providers.CredentialRef, error)
	Resolve(ctx context.Context, scope contracts.Scope, ref string, use credentials.Use) (credentials.Secret, error)
	Get(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID) (providers.CredentialRef, error)
	List(ctx context.Context, scope contracts.Scope, provider providers.ProviderID) ([]providers.CredentialRef, error)
	Flag(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID, f credentials.Flag) (providers.CredentialRef, error)
	Revoke(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID) (providers.CredentialRef, error)
	RevokeTenant(ctx context.Context, scope contracts.Scope) (int, error)
}

// Config wires an Engine.
type Config struct {
	// Credentials holds every token. Required.
	Credentials Credentials
	// Egress holds the destination allowlist of each application: the token and revocation endpoints must be
	// on the allowlist of the application, or the call is refused. Required (nil means nothing is reachable).
	Egress *egress.Registry
	// Providers are the provider configs. A provider that is absent, or has OAuthSupported false, or is
	// Disabled, has the manual API-key fallback.
	Providers []ProviderConfig
	// Clock is the source of time; nil means the wall clock.
	Clock clock.Clock
	// Logger receives references, reasons and outcomes, never a token, a code, a state or a URL. Nil discards.
	Logger *slog.Logger

	StateTTL       time.Duration // 0 means DefaultStateTTL
	RefreshSkew    time.Duration // 0 means DefaultRefreshSkew
	RequestTimeout time.Duration // 0 means DefaultRequestTimeout
	AccessTTL      time.Duration // 0 means DefaultAccessTTL
	MaxPending     int           // 0 means DefaultMaxPending
	MaxPerTenant   int           // 0 means DefaultMaxPerTenant
	// PopupOrigins is the operator allowlist for presentation popup (exact origins, never *). Empty refuses popup start.
	PopupOrigins []string
	// PollInterval is suggested for popup flow polling; 0 means DefaultPollInterval.
	PollInterval time.Duration
}

// Engine is the OAuth 2.x authorization-code + PKCE engine. It opens no browser and no listener: Begin returns
// the authorization URL, the gateway receives the callback and passes it to Complete. It is safe for
// concurrent use. Pending authorizations live in memory only, so a restart abandons them (the user starts
// again): failing closed is the safe direction.
type Engine struct {
	cr      Credentials
	eg      *egress.Registry
	prov    map[providers.ProviderID]ProviderConfig
	clk     clock.Clock
	log     *slog.Logger
	ttl     time.Duration
	skew    time.Duration
	timeout time.Duration
	atl     time.Duration
	maxAll  int
	maxTen  int

	mu        sync.Mutex
	pending   map[string]*pendingFlow // key: hash of the state (or loopback key when NoState)
	consumed  map[string]usedState    // hash of a state already used or expired
	flights   map[string]*flight
	clients   map[string]*http.Client // key: application ID
	refLocks  map[string]*refLock
	flows     map[string]*FlowRecord // key: flow id
	loopbacks map[string]io.Closer   // key: flow id → ephemeral loopback listener
	// PopupOrigins is the operator allowlist for presentation popup (exact origins). Empty refuses popup.
	PopupOrigins []string
	// PollInterval is suggested for GET /v1/oauth/flows/{id}; 0 means DefaultPollInterval.
	PollInterval time.Duration
}

// New returns an Engine, or an error for an invalid provider config.
func New(cfg Config) (*Engine, error) {
	if cfg.Credentials == nil {
		return nil, fail(contracts.ErrInternal, ReasonBadConfig, "oauth: a credential service is required")
	}
	e := &Engine{
		cr: cfg.Credentials, eg: cfg.Egress, prov: map[providers.ProviderID]ProviderConfig{},
		clk: cfg.Clock, log: cfg.Logger, ttl: cfg.StateTTL, skew: cfg.RefreshSkew, timeout: cfg.RequestTimeout,
		atl: cfg.AccessTTL, maxAll: cfg.MaxPending, maxTen: cfg.MaxPerTenant,
		PopupOrigins: slices.Clone(cfg.PopupOrigins), PollInterval: cfg.PollInterval,
		pending: map[string]*pendingFlow{}, consumed: map[string]usedState{}, flights: map[string]*flight{},
		clients: map[string]*http.Client{}, refLocks: map[string]*refLock{},
		flows: map[string]*FlowRecord{}, loopbacks: map[string]io.Closer{},
	}
	if e.PollInterval <= 0 {
		e.PollInterval = DefaultPollInterval
	}
	if e.clk == nil {
		e.clk = clock.Real{}
	}
	if e.log == nil {
		e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if e.ttl <= 0 {
		e.ttl = DefaultStateTTL
	}
	if e.skew <= 0 {
		e.skew = DefaultRefreshSkew
	}
	if e.timeout <= 0 {
		e.timeout = DefaultRequestTimeout
	}
	if e.atl <= 0 {
		e.atl = DefaultAccessTTL
	}
	if e.maxAll <= 0 {
		e.maxAll = DefaultMaxPending
	}
	if e.maxTen <= 0 {
		e.maxTen = DefaultMaxPerTenant
	}
	for _, p := range cfg.Providers {
		n, err := p.normalized()
		if err != nil {
			return nil, err
		}
		if _, dup := e.prov[n.ID]; dup {
			return nil, fail(contracts.ErrValidationFailed, ReasonBadConfig, "oauth: two configs for one provider")
		}
		e.prov[n.ID] = n
	}
	return e, nil
}

// AuthMode is how a provider is connected.
type AuthMode string

// The modes.
const (
	ModeOAuth        AuthMode = "oauth"
	ModeManualAPIKey AuthMode = "manual_api_key"
)

// Mode says whether the provider is connected by browser OAuth or by the manual API-key fallback of
// internal/credentials (AddAPIKey). A provider without a config, without OAuth support, or switched off by the
// operator has the manual fallback.
func (e *Engine) Mode(id providers.ProviderID) AuthMode {
	if p, ok := e.prov[id]; ok && p.OAuthSupported && !p.Disabled {
		return ModeOAuth
	}
	return ModeManualAPIKey
}

// provider returns the config of a provider that connects by OAuth, or the typed answer that the manual
// fallback applies. No network call is made.
func (e *Engine) provider(id providers.ProviderID) (ProviderConfig, error) {
	if e.Mode(id) != ModeOAuth {
		return ProviderConfig{}, fail(contracts.ErrValidationFailed, ReasonManualFallback,
			"oauth: this provider has no browser authorization flow here; the manual API-key fallback applies").
			WithDetails("fallback", "api_key")
	}
	return e.prov[id], nil
}

// ---------------------------------------------------------------------------------------------
// Consent

// ScopeConsent is one scope as the consent screen shows it.
type ScopeConsent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	DataAccess  string `json:"data_access"`
}

// Consent is everything the host shows the user before authorisation (additive plan 11.1): the provider, the
// account (a known one when re-authorizing; otherwise the user picks it at the provider), the organisation or
// project, the scopes with what they reach, and the data access. It carries no secret.
type Consent struct {
	ProviderID   providers.ProviderID  `json:"provider_id"`
	ProviderName string                `json:"provider_name"`
	Account      providers.SafeAccount `json:"account"`
	Scopes       []ScopeConsent        `json:"scopes"`
	DataAccess   []string              `json:"data_access"`
	// Reauthorization: an existing connection is being renewed; CurrentScopes are the scopes it holds.
	Reauthorization bool     `json:"reauthorization"`
	CurrentScopes   []string `json:"current_scopes"`
	// SwitchAccount: the connection will be bound to another account if the user picks one.
	SwitchAccount bool `json:"switch_account"`
	// Digest identifies this consent. Begin demands it back, so an authorization URL is issued only for a
	// consent the host has prepared (and shown).
	Digest string `json:"digest"`
}

// PrepareRequest selects what to connect.
type PrepareRequest struct {
	ProviderID providers.ProviderID
	// Capabilities select the scopes: the engine requests the smallest set the provider config maps them to.
	Capabilities []string
	// Reauthorize is the existing reference to renew (re-consent, or an account switch).
	Reauthorize providers.CredentialRefID
	// SwitchAccount lets the user pick another account for Reauthorize. Without it, a different account at
	// the callback is refused.
	SwitchAccount bool
}

// Prepare returns the consent data. It has no side effect and makes no network call. For a provider without
// OAuth it returns the manual-fallback error (see IsManualFallback).
func (e *Engine) Prepare(ctx context.Context, scope contracts.Scope, req PrepareRequest) (Consent, error) {
	if err := scope.Validate(); err != nil {
		return Consent{}, err
	}
	p, err := e.provider(req.ProviderID)
	if err != nil {
		return Consent{}, err
	}
	scopes, err := p.scopesFor(req.Capabilities)
	if err != nil {
		return Consent{}, err
	}
	c := Consent{ProviderID: p.ID, ProviderName: p.DisplayName, DataAccess: slices.Clone(p.DataAccess), SwitchAccount: req.SwitchAccount}
	if c.ProviderName == "" {
		c.ProviderName = string(p.ID)
	}
	if req.SwitchAccount && req.Reauthorize == "" {
		return Consent{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "oauth: an account switch renews an existing connection")
	}
	if req.Reauthorize != "" {
		ref, err := e.cr.Get(ctx, scope, req.Reauthorize)
		if err != nil {
			return Consent{}, err
		}
		if ref.Kind != providers.CredentialKindOauth || ref.Binding.ProviderID != p.ID {
			return Consent{}, fail(contracts.ErrValidationFailed, ReasonNotOAuth, "oauth: the reference is not an OAuth connection to this provider")
		}
		if ref.Status == providers.CredentialRefStatusRevoked {
			return Consent{}, fail(contracts.ErrUnauthenticated, ReasonReauthRequired, "oauth: a revoked connection is not renewed; connect again")
		}
		c.Reauthorization, c.Account, c.CurrentScopes = true, ref.Account, slices.Clone(ref.Binding.Scopes)
	}
	for _, s := range scopes {
		info, _ := p.scopeInfo(s)
		c.Scopes = append(c.Scopes, ScopeConsent{Name: s, Description: info.Description, DataAccess: info.DataAccess})
	}
	c.Digest = consentDigest(c, scopes, req.Reauthorize)
	return c, nil
}

func consentDigest(c Consent, scopes []string, reauth providers.CredentialRefID) string {
	acct := ""
	if c.Account.AccountID != nil {
		acct = *c.Account.AccountID
	}
	h := sha256.New()
	for _, part := range []string{string(c.ProviderID), strings.Join(scopes, " "), string(reauth), acct, boolText(c.SwitchAccount)} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func boolText(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---------------------------------------------------------------------------------------------
// Begin

// BeginRequest starts an authorization.
type BeginRequest struct {
	PrepareRequest
	// ConsentDigest is Consent.Digest of the matching Prepare.
	ConsentDigest string
	// RedirectURI is where the provider sends the browser back: a registered redirect, or a loopback URI
	// (LoopbackRedirectURI) on the port the caller bound. It is bound to the flow and checked again at the
	// callback and in the token request. With CallbackMode loopback the caller may send LoopbackPlaceholder;
	// StartLoopback (or BindLoopback) replaces it with the ephemeral URI.
	RedirectURI string
	// Label names a new connection in settings (never secret material).
	Label string
	// Presentation is redirect (default) or popup. Popup mints a flow_id for polling.
	Presentation string
	// CallbackMode is host (default) or loopback.
	CallbackMode string
	// OpenerOrigin is required for presentation popup (exact origin on PopupOrigins). Empty for CLI.
	OpenerOrigin string
	// SkipOpenerCheck lets the CLI browser-login path start a popup-shaped flow without an opener
	// (the loopback page then shows a close message without postMessage).
	SkipOpenerCheck bool
	// IssuedClientID is a previously issued dynamic client id (SIWC oaiapp_...) to reuse on re-auth.
	IssuedClientID string
	// ExtAgentHostID is the stable opaque host id for SIWC dynamic registration (urn:uuid:...).
	ExtAgentHostID string
}

// Authorization is what the host needs to send the user to the provider.
type Authorization struct {
	// URL is the authorization URL to open in the user's browser. It holds the state, the PKCE challenge and
	// the nonce, never a token.
	URL            string    `json:"url"`
	RedirectURI    string    `json:"redirect_uri"`
	ExpiresAt      time.Time `json:"expires_at"`
	Consent        Consent   `json:"consent"`
	FlowID         string    `json:"flow_id,omitempty"`
	PollIntervalMS int       `json:"poll_interval_ms,omitempty"`
}

// usedState remembers a state that can no longer start anything, so a replay is told from an unknown state.
type usedState struct {
	until   time.Time
	expired bool
}

type pendingFlow struct {
	state       string
	scope       contracts.Scope
	provider    providers.ProviderID
	redirectURI string
	verifier    credentials.Secret
	nonce       string
	scopes      []string
	reauth      providers.CredentialRefID
	prior       providers.SafeAccount
	switchAcct  bool
	label       string
	created     time.Time
	expires     time.Time
	flowID      string
	opener      string
	clientID    string // effective client id (SIWC issued id when reusing)
	resource    string
	noState     bool // OpenRouter: bind by redirect URI / flow id instead of state
}

// Begin creates a pending authorization (single use, expiring) and returns the URL.
func (e *Engine) Begin(ctx context.Context, scope contracts.Scope, req BeginRequest) (Authorization, error) {
	consent, err := e.Prepare(ctx, scope, req.PrepareRequest)
	if err != nil {
		return Authorization{}, err
	}
	if !equalConst(consent.Digest, req.ConsentDigest) {
		return Authorization{}, fail(contracts.ErrValidationFailed, ReasonConsentMismatch, "oauth: the consent shown is not the one being authorised; prepare it again")
	}
	p := e.prov[req.ProviderID]
	presentation := req.Presentation
	if presentation == "" {
		presentation = PresentationRedirect
	}
	if presentation != PresentationRedirect && presentation != PresentationPopup {
		return Authorization{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "oauth: presentation is redirect or popup")
	}
	callback := req.CallbackMode
	if callback == "" {
		callback = CallbackHost
	}
	if callback != CallbackHost && callback != CallbackLoopback {
		return Authorization{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "oauth: callback is host or loopback")
	}
	if p.LoopbackOnly && callback != CallbackLoopback {
		// SIWC (and any other loopback-only provider): remote host callbacks are not offered.
		// Answer manual_fallback so hosts use the API-key path instead of a rejected redirect.
		return Authorization{}, fail(contracts.ErrValidationFailed, ReasonManualFallback,
			"oauth: this provider only allows loopback redirects (same-machine deployments); use a manual API key for remote hosts").
			WithDetails("fallback", "api_key")
	}
	redirectURI := req.RedirectURI
	if callback == CallbackLoopback {
		if redirectURI != LoopbackPlaceholder && !isBoundLoopbackURI(redirectURI, p.LoopbackPath) {
			return Authorization{}, fail(contracts.ErrValidationFailed, ReasonRedirectRejected,
				"oauth: loopback callback needs redirect_uri "+LoopbackPlaceholder+" or a bound loopback URI")
		}
		if redirectURI == LoopbackPlaceholder {
			return Authorization{}, fail(contracts.ErrValidationFailed, ReasonBadRequest,
				"oauth: bind a loopback listener before Begin (placeholder alone is not a redirect)")
		}
	} else if err := p.checkRedirect(redirectURI); err != nil {
		return Authorization{}, err
	}
	opener := ""
	if presentation == PresentationPopup {
		if req.SkipOpenerCheck {
			// CLI path: no postMessage target
		} else {
			if !OriginAllowed(req.OpenerOrigin, e.PopupOrigins) {
				return Authorization{}, fail(contracts.ErrValidationFailed, ReasonBadRequest,
					"oauth: opener_origin is required for popup and must be listed in external_providers.oauth.popup_origins")
			}
			opener = req.OpenerOrigin
		}
	} else if req.OpenerOrigin != "" {
		return Authorization{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "oauth: opener_origin is only for presentation popup")
	}
	scopes := make([]string, 0, len(consent.Scopes))
	for _, s := range consent.Scopes {
		scopes = append(scopes, s.Name)
	}
	verifier, err := newVerifier()
	if err != nil {
		return Authorization{}, err
	}
	state := ""
	if !p.NoState {
		if state, err = randomURLSafe(32); err != nil {
			return Authorization{}, err
		}
	}
	nonce := ""
	if p.OIDC {
		if nonce, err = randomURLSafe(32); err != nil {
			return Authorization{}, err
		}
	}
	clientID := p.ClientID
	if p.DynamicClient {
		if req.IssuedClientID != "" {
			clientID = req.IssuedClientID
		} else if p.InitialClientID != "" {
			clientID = p.InitialClientID
		}
		if req.ExtAgentHostID == "" && clientID == p.InitialClientID {
			return Authorization{}, fail(contracts.ErrValidationFailed, ReasonBadRequest,
				"oauth: a stable ext_agent_host_id is required for dynamic client registration")
		}
	}
	now := e.clk.Now()
	flowID := ""
	if presentation == PresentationPopup || callback == CallbackLoopback || p.NoState {
		flowID = e.newFlowID()
	}
	f := &pendingFlow{
		state: state, scope: scope, provider: p.ID, redirectURI: redirectURI, verifier: verifier, nonce: nonce,
		scopes: scopes, reauth: req.Reauthorize, prior: consent.Account, switchAcct: req.SwitchAccount,
		label: req.Label, created: now, expires: now.Add(e.ttl), flowID: flowID, opener: opener,
		clientID: clientID, resource: p.Resource, noState: p.NoState,
	}
	if err := e.addPending(f); err != nil {
		return Authorization{}, err
	}
	if flowID != "" {
		e.registerFlow(&FlowRecord{
			FlowID: flowID, Provider: p.ID, Status: FlowPending, UpdatedAt: now,
			scope: scope, stateKey: pendingKey(f), expires: f.expires, opener: opener,
		})
	}
	authURL, err := e.buildAuthURL(p, f, req)
	if err != nil {
		return Authorization{}, err
	}
	e.log.Info("oauth authorization started", "provider", string(p.ID), "tenant", scope.TenantID, "reauth", req.Reauthorize != "")
	out := Authorization{URL: authURL, RedirectURI: redirectURI, ExpiresAt: f.expires, Consent: consent}
	if presentation == PresentationPopup {
		out.FlowID = flowID
		out.PollIntervalMS = int(e.PollInterval / time.Millisecond)
	}
	return out, nil
}

func pendingKey(f *pendingFlow) string {
	if f.noState {
		return "lb:" + f.flowID
	}
	return stateKey(f.state)
}

func isBoundLoopbackURI(raw, path string) bool {
	cfg := ProviderConfig{Loopback: true, LoopbackPath: path}
	if cfg.LoopbackPath == "" {
		cfg.LoopbackPath = defaultLoopbackPath
	}
	return cfg.checkRedirect(raw) == nil
}

func (e *Engine) buildAuthURL(p ProviderConfig, f *pendingFlow, req BeginRequest) (string, error) {
	challenge := ChallengeS256(f.verifier)
	if p.AuthStyle == AuthStyleOpenRouter {
		q := url.Values{}
		q.Set("callback_url", f.redirectURI)
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
		au, err := url.Parse(p.AuthorizationEndpoint)
		if err != nil {
			return "", fail(contracts.ErrValidationFailed, ReasonBadConfig, "oauth: bad authorization endpoint")
		}
		au.RawQuery = q.Encode()
		return au.String(), nil
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", f.clientID)
	q.Set("redirect_uri", f.redirectURI)
	q.Set("scope", strings.Join(f.scopes, " "))
	if f.state != "" {
		q.Set("state", f.state)
	}
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if f.nonce != "" {
		q.Set("nonce", f.nonce)
	}
	if f.resource != "" {
		q.Set("resource", f.resource)
	}
	if p.DynamicClient && f.clientID == p.InitialClientID {
		q.Set("ext_agent_host_id", req.ExtAgentHostID)
		if p.AgentNameHint != "" {
			q.Set("agent_name_hint", p.AgentNameHint)
		}
	}
	switch {
	case req.SwitchAccount:
		q.Set("prompt", p.SwitchPrompt)
	case req.Reauthorize != "":
		q.Set("prompt", p.ConsentPrompt)
	}
	for k, v := range p.ExtraAuthParams {
		q.Set(k, v)
	}
	au, err := url.Parse(p.AuthorizationEndpoint)
	if err != nil {
		return "", fail(contracts.ErrValidationFailed, ReasonBadConfig, "oauth: bad authorization endpoint")
	}
	au.RawQuery = q.Encode()
	return au.String(), nil
}

func stateKey(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// addPending stores a flow, after dropping expired ones. It refuses when the table is full: an attacker
// cannot grow it without bound, and cannot evict someone else's flow.
func (e *Engine) addPending(f *pendingFlow) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.purgeLocked(f.created)
	perTenant := 0
	for _, o := range e.pending {
		if o.scope.ApplicationID == f.scope.ApplicationID && o.scope.TenantID == f.scope.TenantID {
			perTenant++
		}
	}
	if len(e.pending) >= e.maxAll || perTenant >= e.maxTen {
		return fail(contracts.ErrResourceExhausted, ReasonTooManyPending, "oauth: too many authorizations are waiting; finish or wait for them to expire")
	}
	e.pending[pendingKey(f)] = f
	return nil
}

func (e *Engine) purgeLocked(now time.Time) {
	for k, f := range e.pending {
		if !now.Before(f.expires) {
			delete(e.pending, k)
			e.consumed[k] = usedState{until: f.expires.Add(e.ttl), expired: true}
		}
	}
	for k, u := range e.consumed {
		if !now.Before(u.until) {
			delete(e.consumed, k)
		}
	}
	if len(e.consumed) > 4*e.maxAll { // bounded memory: forget the oldest half
		for k := range e.consumed {
			delete(e.consumed, k)
			if len(e.consumed) <= 2*e.maxAll {
				break
			}
		}
	}
}

// take removes the flow of a state in one step: a state works once, whatever happens next.
func (e *Engine) take(state string) (*pendingFlow, error) {
	if state == "" || len(state) > 256 {
		return nil, fail(contracts.ErrValidationFailed, ReasonStateUnknown, "oauth: the callback has no valid state")
	}
	now := e.clk.Now()
	key := stateKey(state)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.purgeLocked(now)
	f, ok := e.pending[key]
	if !ok {
		if u, used := e.consumed[key]; used {
			if u.expired {
				return nil, fail(contracts.ErrExpired, ReasonStateExpired, "oauth: the authorization expired; start again")
			}
			return nil, fail(contracts.ErrReplayed, ReasonReplayed, "oauth: this authorization was already used")
		}
		return nil, fail(contracts.ErrValidationFailed, ReasonStateUnknown, "oauth: the state is unknown")
	}
	delete(e.pending, key)
	e.consumed[key] = usedState{until: now.Add(e.ttl)}
	if !equalConst(f.state, state) { // defence in depth: the lookup was by hash
		return nil, fail(contracts.ErrValidationFailed, ReasonStateUnknown, "oauth: the state is unknown")
	}
	return f, nil
}
