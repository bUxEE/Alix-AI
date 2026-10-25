package gemini

import (
	"context"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// AuthKind says how a connection authenticates to the Gemini API.
type AuthKind string

const (
	// AuthAPIKey is a Gemini API key held by the credential service. It is
	// sent in the x-goog-api-key header, never in a query string.
	AuthAPIKey AuthKind = "api_key"
	// AuthOAuth is a Google OAuth 2.0 access token (authorization code flow
	// with PKCE, run by the shared engine in internal/oauth). It is sent as a
	// bearer token, with the Cloud project in x-goog-user-project.
	AuthOAuth AuthKind = "oauth"
)

// AuthSupport declares the authentication kinds the Gemini API accepts, in
// order of preference (additive plan 11.1: browser OAuth first, the manual key
// as the fallback). The account decides per connection which one is used
// (Config.Auth); a Google Cloud project is behind both.
func AuthSupport() []AuthKind { return []AuthKind{AuthOAuth, AuthAPIKey} }

// KeySource resolves the credential reference to the API key for ONE call, for
// the scope the call serves. ServiceKeys adapts *credentials.Service; tests use
// a fake. Nothing is cached here, so a rotation or revocation takes effect on
// the next call.
type KeySource interface {
	Key(ctx context.Context, scope contracts.Scope, ref string) (credentials.Secret, error)
}

// TokenSource resolves the credential reference to the OAuth access token for
// ONE call, refreshing it first when it is near expiry. The shared engine does
// this (internal/oauth Engine.Token); adapters may not import it (module map),
// so the composition root wraps it:
//
//	func (w engineTokens) Bearer(ctx context.Context, sc contracts.Scope, ref string, scopes []string, force bool) (credentials.Secret, error) {
//		return w.e.Token(ctx, sc, oauth.TokenRequest{Ref: providers.CredentialRefID(ref), Scopes: scopes, ForceRefresh: force})
//	}
//
// forceRefresh is true on the one retry after the API answered 401 (the access
// token was revoked or rotated provider-side). The token is held for the length
// of the call and never cached here.
type TokenSource interface {
	Bearer(ctx context.Context, scope contracts.Scope, ref string, scopes []string, forceRefresh bool) (credentials.Secret, error)
}

// ServiceKeys adapts the credential service to KeySource. Every use is audited
// by the service, which also refuses a reference that is not the scope's, not
// for this provider, revoked, expired or quarantined.
type ServiceKeys struct {
	Service *credentials.Service
}

// Key implements KeySource.
func (k ServiceKeys) Key(ctx context.Context, scope contracts.Scope, ref string) (credentials.Secret, error) {
	return external.ExtResolveKey(ctx, ProviderName, k.Service, nil, scope, ref)
}

// Google OAuth 2.0 facts, for the composition root that builds the shared
// engine's ProviderConfig (internal/cli, task X-26). They are plain strings so
// that this package imports no engine. Read 2026-10-08 as summarised by a fetch
// tool from https://ai.google.dev/gemini-api/docs/oauth (scopes, consent
// screen); the three endpoints are Google's published OAuth 2.0 endpoints
// (https://developers.google.com/identity/protocols/oauth2/web-server), not
// re-fetched on that date. Not verified by AlixAI; live consent is X-H02.
const (
	OAuthAuthorizationEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	OAuthTokenEndpoint         = "https://oauth2.googleapis.com/token"
	OAuthRevocationEndpoint    = "https://oauth2.googleapis.com/revoke"

	// ScopeCloudPlatform is the scope the vendor's page names for the gcloud
	// based setup of Gemini API OAuth. It is broad (all of Google Cloud for
	// the account): the page names no narrower scope for generateContent, so
	// the narrowest known to work is still to be confirmed live (X-H02).
	ScopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
	// ScopeRetriever is the scope the vendor's page names for the semantic
	// retrieval service. This adapter does not use it.
	ScopeRetriever = "https://www.googleapis.com/auth/generative-language.retriever"

	// OAuthCapability is the capability name the engine maps to InferenceScopes.
	OAuthCapability = "inference"
)

// InferenceScopes are the scopes the "inference" capability requests: the one
// scope the vendor's page names for calling the API with OAuth, and nothing
// more.
var InferenceScopes = []string{ScopeCloudPlatform}

// OAuthAuthParams are the extra authorization parameters Google needs for a
// refresh token ("access_type=offline"); none is reserved by the engine.
func OAuthAuthParams() map[string]string {
	return map[string]string{"access_type": "offline"}
}

// OAuthConsentNotes records, with the date they were read, what the vendor says
// about the consent screen of a Cloud project that offers this OAuth flow
// (2026-10-08, as summarised, not verified). They are for the setup screen of
// the operator; none of them is something this package can enforce.
func OAuthConsentNotes() []string {
	return []string{
		"The Google Cloud project needs an OAuth consent screen with User Type External (read 2026-10-08).",
		"While the app is in testing, each user who connects must be added as a test user (read 2026-10-08).",
		"An unverified app shows the 'Google hasn't verified this app' warning; that is expected in testing, and a production app must add and verify its scopes with Google (read 2026-10-08).",
		"Gemini API usage through OAuth is charged to a Google Cloud project: quota and billing are that project's, not the account's consumer subscription.",
	}
}
