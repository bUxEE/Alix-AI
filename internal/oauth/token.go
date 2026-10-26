package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// maxResponseBytes bounds what the engine reads from a provider.
const maxResponseBytes = 1 << 20

var oauthErrorRE = regexp.MustCompile(`^[a-z][a-z_]{0,39}$`)

// exchangeError is a failed call to a token or revocation endpoint. It holds a protocol error code (already
// shaped like one) and never the text of the response, which can echo a credential.
type exchangeError struct {
	code      string // RFC 6749 5.2 error code, "" when none
	transient bool   // the provider could not answer (network, 5xx, 429): the credential is not at fault
	egress    bool   // the destination is not on the application's allowlist
}

func (e *exchangeError) Error() string { return "oauth: the provider call failed" }

// typed maps the failure to the package's typed error.
func (e *exchangeError) typed() error {
	switch {
	case e.egress:
		return fail(contracts.ErrPermissionDenied, ReasonEgressDenied, "oauth: the provider endpoint is not on the egress allowlist")
	case e.transient:
		return fail(contracts.ErrUnavailable, ReasonProviderDown, "oauth: the provider could not be reached")
	default:
		return fail(contracts.ErrUnauthenticated, ReasonExchangeFailed, "oauth: the provider refused the request").WithDetails("oauth_error", e.code)
	}
}

// tokenSet is a parsed token response. The values are credentials.Secret from the moment they are read.
type tokenSet struct {
	access    credentials.Secret
	refresh   credentials.Secret
	expiresIn time.Duration // 0 when absent
	scopes    []string      // nil when the response had no scope member (the requested scopes were granted)
	idToken   credentials.Secret
	members   map[string]any // top-level members, for the account mapping only
}

// post sends a form to a provider endpoint through the application's egress dialer.
func (e *Engine) post(ctx context.Context, scope contracts.Scope, endpoint string, form url.Values, basicUser string, basicPass credentials.Secret) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, &exchangeError{transient: true}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basicUser != "" {
		req.SetBasicAuth(url.QueryEscape(basicUser), url.QueryEscape(basicPass.Reveal()))
	}
	resp, err := e.client(scope.ApplicationID).Do(req)
	if err != nil {
		err = egress.StripURL(err)
		if egress.IsDenied(err) {
			return nil, 0, &exchangeError{egress: true}
		}
		return nil, 0, &exchangeError{transient: true}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return nil, resp.StatusCode, &exchangeError{transient: true}
	}
	return body, resp.StatusCode, nil
}

// client returns the HTTP client of an application's token and revocation calls, built on first use and kept, so
// the calls share its idle connections instead of each leaving its own open. Every connection and request is still
// held to the application's allowlist as it is at that moment (the egress dialer reads it on each one). A redirect
// is answered, not followed: a 307 or 308 would replay the form (code, verifier, client secret, refresh value) to
// another origin.
func (e *Engine) client(app string) *http.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	if hc, ok := e.clients[app]; ok {
		return hc
	}
	hc := e.eg.Dialer(app).HTTPClient(e.timeout)
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	e.clients[app] = hc
	return hc
}

// clientAuth adds the client's authentication to a form, resolving the client secret from the credential
// service when the provider needs one.
func (e *Engine) clientAuth(ctx context.Context, scope contracts.Scope, p ProviderConfig, form url.Values) (user string, pass credentials.Secret, err error) {
	switch p.ClientAuth {
	case ClientAuthPost, ClientAuthBasic:
		sec, err := e.cr.Resolve(ctx, scope, p.ClientSecretRef, credentials.Use{Provider: p.ID, Purpose: "oauth.client"})
		if err != nil {
			return "", "", err
		}
		if p.ClientAuth == ClientAuthBasic {
			return p.ClientID, sec, nil
		}
		form.Set("client_id", p.ClientID)
		form.Set("client_secret", sec.Reveal())
	default:
		form.Set("client_id", p.ClientID)
	}
	return "", "", nil
}

// exchangeIssuedKey posts the OpenRouter-style JSON key exchange and returns the issued API key.
func (e *Engine) exchangeIssuedKey(ctx context.Context, scope contracts.Scope, p ProviderConfig, code string, verifier credentials.Secret) (credentials.Secret, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{
		"code":                  code,
		"code_verifier":         verifier.Reveal(),
		"code_challenge_method": "S256",
	})
	if err != nil {
		return "", &exchangeError{transient: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.KeyExchangeEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", &exchangeError{transient: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := e.client(scope.ApplicationID).Do(req)
	if err != nil {
		err = egress.StripURL(err)
		if egress.IsDenied(err) {
			return "", &exchangeError{egress: true}
		}
		return "", &exchangeError{transient: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return "", &exchangeError{transient: true}
	}
	if resp.StatusCode != http.StatusOK {
		return "", errorFromResponse(resp.StatusCode, raw)
	}
	var m struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Key == "" {
		return "", badResponse()
	}
	return credentials.Secret(m.Key), nil
}

// tokenCall posts a token request and parses the response.
func (e *Engine) tokenCall(ctx context.Context, scope contracts.Scope, p ProviderConfig, form url.Values) (*tokenSet, error) {
	user, pass, err := e.clientAuth(ctx, scope, p, form)
	if err != nil {
		return nil, err
	}
	body, status, err := e.post(ctx, scope, p.TokenEndpoint, form, user, pass)
	if err != nil {
		var xe *exchangeError
		if errors.As(err, &xe) {
			return nil, xe
		}
		return nil, err
	}
	if status != http.StatusOK {
		return nil, errorFromResponse(status, body)
	}
	return parseTokenResponse(body)
}

// errorFromResponse reads the RFC 6749 5.2 error code of a failure, and nothing else of the body.
func errorFromResponse(status int, body []byte) *exchangeError {
	if status >= 500 || status == http.StatusTooManyRequests || status == http.StatusRequestTimeout {
		return &exchangeError{transient: true}
	}
	var m struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &m)
	code := ""
	if oauthErrorRE.MatchString(m.Error) {
		code = m.Error
	}
	if code == "" {
		code = "invalid_response"
	}
	return &exchangeError{code: code}
}

func badResponse() error {
	return fail(contracts.ErrUnavailable, ReasonBadTokenResponse, "oauth: the token response is malformed")
}

func parseTokenResponse(body []byte) (*tokenSet, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	m := map[string]any{}
	if err := dec.Decode(&m); err != nil {
		return nil, badResponse()
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	t := &tokenSet{access: credentials.Secret(str("access_token")), refresh: credentials.Secret(str("refresh_token")),
		idToken: credentials.Secret(str("id_token")), members: m}
	if t.access.Empty() {
		return nil, badResponse()
	}
	if tt := str("token_type"); !strings.EqualFold(tt, "bearer") {
		return nil, badResponse()
	}
	switch v := m["expires_in"].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 && n < 1<<31 {
			t.expiresIn = time.Duration(n) * time.Second
		}
	case string:
		var n int64
		if err := json.Unmarshal([]byte(v), &n); err == nil && n > 0 && n < 1<<31 {
			t.expiresIn = time.Duration(n) * time.Second
		}
	}
	if s, ok := m["scope"].(string); ok {
		t.scopes = strings.Fields(s)
	}
	return t, nil
}

// idClaims are the ID-token claims the engine validates.
type idClaims struct {
	Iss   string          `json:"iss"`
	Sub   string          `json:"sub"`
	Aud   json.RawMessage `json:"aud"`
	Exp   json.Number     `json:"exp"`
	Nonce string          `json:"nonce"`
	Azp   string          `json:"azp"`
}

// validateIDToken checks the ID token an OIDC provider returned from its token endpoint: issuer, audience,
// expiry, nonce (constant-time, and required). The signature is not verified: OpenID Connect Core 3.1.3.7
// lets a client rely on TLS to the token endpoint, which the egress transport verifies (no skip switch), when
// the token is received directly from it in the code flow, as here. It returns all claims for the account
// mapping.
func (e *Engine) validateIDToken(p ProviderConfig, raw credentials.Secret, nonce string) (map[string]any, error) {
	bad := func(r Reason) error {
		return fail(contracts.ErrUnauthenticated, r, "oauth: the ID token was not accepted")
	}
	parts := strings.Split(raw.Reveal(), ".")
	if len(parts) != 3 {
		return nil, bad(ReasonIDToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, bad(ReasonIDToken)
	}
	var c idClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, bad(ReasonIDToken)
	}
	if c.Iss == "" || c.Iss != p.Issuer {
		return nil, bad(ReasonIssuerMismatch)
	}
	var aud []string
	if json.Unmarshal(c.Aud, &aud) != nil {
		var one string
		if json.Unmarshal(c.Aud, &one) != nil {
			return nil, bad(ReasonIDToken)
		}
		aud = []string{one}
	}
	found := false
	for _, a := range aud {
		found = found || a == p.ClientID
	}
	if !found || (len(aud) > 1 && c.Azp != p.ClientID) {
		return nil, bad(ReasonIDToken)
	}
	exp, err := c.Exp.Int64()
	if err != nil || !e.clk.Now().Before(time.Unix(exp, 0)) {
		return nil, bad(ReasonIDToken)
	}
	if c.Nonce == "" || nonce == "" || !equalConst(c.Nonce, nonce) {
		return nil, bad(ReasonNonce)
	}
	if c.Sub == "" {
		return nil, bad(ReasonIDToken)
	}
	claims := map[string]any{}
	_ = json.Unmarshal(payload, &claims)
	return claims, nil
}

// accountFrom reads the safe account identifiers from the token response members and the ID-token claims.
func accountFrom(p ProviderConfig, sources ...map[string]any) providers.SafeAccount {
	get := func(field string) *string {
		if field == "" {
			return nil
		}
		for _, m := range sources {
			if s, ok := m[field].(string); ok && s != "" {
				return &s
			}
		}
		return nil
	}
	return providers.SafeAccount{AccountID: get(p.Account.AccountID), OrganizationID: get(p.Account.OrganizationID), ProjectID: get(p.Account.ProjectID)}
}

// expiryOf is the expiry of an access value received now.
func (e *Engine) expiryOf(t *tokenSet) time.Time {
	d := t.expiresIn
	if d <= 0 {
		d = e.atl
	}
	return e.clk.Now().Add(d)
}
