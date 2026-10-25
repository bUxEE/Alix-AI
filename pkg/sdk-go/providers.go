package sdkgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ProvidersClient is the operator-facing surface of External Provider Mode (additive plan 6.3, 11 and 16):
// provider connections, the deployment mode, the data-egress policy, the egress disclosure, budgets and usage,
// the global kill switch and the browser OAuth flow. Get one with Client.Providers; it shares the client's
// options, transport and token provider.
//
// External mode is OFF by default. A deployment without the providers service (the local-only build, or external
// mode not enabled) answers every route `unavailable`: that is the absence of the feature, never an empty result.
// Every route needs an operator, decided by the deployment's own configuration and never by a role or scope a
// token asserts (`permission_denied` for everybody else). The application, tenant, workspace and principal are
// the verified context token and nothing an argument says.
//
// Credentials. The SDK never persists, logs or echoes a provider credential: a manual key travels only inside
// SubmitCredential as a Secret, and a connected provider is represented to the host by a ConnectionStatus (the
// state and a safe account id) and nothing else. Money is integer micro-units; the SDK has no float for it.
type ProvidersClient struct{ c *Client }

// Providers returns the client of the providers area.
func (c *Client) Providers() *ProvidersClient { return &ProvidersClient{c: c} }

// ---- the secret ----

const secretPlaceholder = "[redacted]"

// Secret is a credential value in transit. It prints as a placeholder in every format a logger, an error or an
// encoder could use (String, GoString, Format, slog, JSON and text encoding); the one place its value is read is
// the request body SubmitCredential builds, and nothing else in the SDK can reach it.
type Secret struct{ v string }

// NewSecret wraps a credential value.
func NewSecret(value string) Secret { return Secret{v: value} }

// String implements fmt.Stringer.
func (Secret) String() string { return secretPlaceholder }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return "sdkgo.Secret(" + secretPlaceholder + ")" }

// Format implements fmt.Formatter: no verb prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, secretPlaceholder) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(secretPlaceholder) }

// MarshalJSON never encodes the value: a Secret placed in some other document cannot leak.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + secretPlaceholder + `"`), nil }

// MarshalText never encodes the value.
func (Secret) MarshalText() ([]byte, error) { return []byte(secretPlaceholder), nil }

// Empty reports whether no value was given.
func (s Secret) Empty() bool { return s.v == "" }

// ---- types ----

// ProviderList is the provider profiles of the caller's application with the connection status of the caller's tenant.
type ProviderList struct {
	Providers []pv.ConnectionStatus `json:"providers"`
}

// ModeLevel is where a mode is configured, widest first.
type ModeLevel string

// The levels of a mode.
const (
	ModeLevelDeployment  ModeLevel = "deployment"
	ModeLevelApplication ModeLevel = "application"
	ModeLevelTenant      ModeLevel = "tenant"
)

// Valid reports whether l is one of the three levels.
func (l ModeLevel) Valid() bool {
	return l == ModeLevelDeployment || l == ModeLevelApplication || l == ModeLevelTenant
}

// ProviderMode is the mode configured at each level and the effective one: the most restrictive level, `local`
// under the kill switch. Local is the default everywhere.
type ProviderMode struct {
	Effective  pv.DeploymentMode               `json:"effective"`
	Configured pv.DeploymentMode               `json:"configured"`
	DecidedBy  string                          `json:"decided_by"`
	KillSwitch bool                            `json:"kill_switch"`
	Levels     map[ModeLevel]pv.DeploymentMode `json:"levels"`
}

// EgressPolicyView is the answer of GetEgressPolicy: the policy, or nil when the level has none (external
// processing is disabled there).
type EgressPolicyView struct {
	Policy *pv.EgressPolicy
}

// UnmarshalJSON requires the `policy` member (null or a policy): an answer without it is not a view.
func (v *EgressPolicyView) UnmarshalJSON(data []byte) error {
	var raw struct {
		Policy json.RawMessage `json:"policy"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if len(raw.Policy) == 0 {
		return errors.New("egress policy view: policy is required")
	}
	v.Policy = nil
	if string(bytes.TrimSpace(raw.Policy)) == "null" {
		return nil
	}
	var p pv.EgressPolicy
	if err := json.Unmarshal(raw.Policy, &p); err != nil {
		return err
	}
	v.Policy = &p
	return nil
}

// EgressPolicyUpdate is the operator-supplied part of an egress policy. The target, id, approval and timestamps
// are minted by the core. ExpectedRevision is the revision the writer read, 0 to create; a stale one is `conflict`.
type EgressPolicyUpdate struct {
	Level            pv.EgressLevel           `json:"level"`
	SourceID         pv.SourceID              `json:"source_id,omitempty"`
	ExpectedRevision int64                    `json:"expected_revision"`
	Enabled          bool                     `json:"enabled"`
	SourceMode       pv.EgressMode            `json:"source_mode"`
	SourceClass      *pv.DataClassification   `json:"source_class,omitempty"`
	AllowedProviders []pv.ProviderID          `json:"allowed_providers,omitempty"`
	AllowedModels    []pv.ModelRef            `json:"allowed_models,omitempty"`
	AllowedClasses   []pv.EgressEligibleClass `json:"allowed_classes,omitempty"`
	AllowedRegions   []pv.RegionCode          `json:"allowed_regions,omitempty"`
	// Files says whether an excerpt of an uploaded file may leave under this level, and up to which class; nil
	// (left out) keeps every excerpt local. Not at source level.
	Files *pv.EgressPolicyContentEgress `json:"files,omitempty"`
	// Audio says whether the raw audio of an utterance may leave under this level (to an external speech
	// recogniser), and up to which class; nil (left out) keeps all audio local. Not at source level.
	Audio *pv.EgressPolicyContentEgress `json:"audio,omitempty"`
}

// ProviderBudgets is the budgets of the caller's scope with the current state of each.
type ProviderBudgets struct {
	Budgets []pv.Budget      `json:"budgets"`
	States  []pv.BudgetState `json:"states"`
}

// KillSwitch is the state of the global external-disable switch.
type KillSwitch struct {
	Engaged   bool   `json:"engaged"`
	Reason    string `json:"reason,omitempty"`
	ChangedBy string `json:"changed_by,omitempty"`
	ChangedAt string `json:"changed_at,omitempty"`
}

// BillingNoticeAckInput is what an operator confirms when they acknowledge a provider's billing notice. Both
// statements must be true; the SDK refuses to send anything else.
type BillingNoticeAckInput struct {
	// NoticeVersion is the profile's current notice version.
	NoticeVersion string
	// ConsumerSubscriptionIsNotAPIAccess confirms that a consumer chat subscription is not API access.
	ConsumerSubscriptionIsNotAPIAccess bool
	// SeparateAPIBillingUnderstood confirms that API usage is billed separately.
	SeparateAPIBillingUnderstood bool
}

// CredentialInput is a manual credential. Key is write-only: it is sent once and never returned.
type CredentialInput struct {
	// Key is the API key or token.
	Key Secret
	// Label is an optional name for the connection (at most 128 characters).
	Label string
	// ExpiresAt is an optional expiry; zero for none.
	ExpiresAt time.Time
	// Scopes are the provider scopes the capability needs; the validation probe asks for no more.
	Scopes []string
}

// DisclosureQuery describes the request an egress disclosure is for. Zero members take the server's defaults.
type DisclosureQuery struct {
	// Role is the model role (default conversational_controller).
	Role pv.Role
	// SourceIDs are the RAG sources the request would draw on.
	SourceIDs []string
	// EstimatedToksIn and MaxOutputToks bound the cost estimate; nil for none.
	EstimatedToksIn *int64
	MaxOutputToks   *int64
}

// UsageScope is whose usage an export covers.
type UsageScope string

// The usage scopes. The application scope needs a deployment operator.
const (
	UsageScopeTenant      UsageScope = "tenant"
	UsageScopeApplication UsageScope = "application"
)

// UsageQuery selects the usage to export. Zero members take the server's defaults.
type UsageQuery struct {
	From, To time.Time
	// Provider narrows the export to one provider id.
	Provider string
	Channel  pv.Channel
	Scope    UsageScope
}

// UsageExport is the usage records (no prompt and no response) with totals per provider, channel and currency,
// for reconciliation with the provider's invoice. The SDK asks for JSON; the CSV rendering is not spoken.
type UsageExport map[string]any

// OAuthStartInput begins a browser OAuth authorization.
type OAuthStartInput struct {
	// Capabilities are the capabilities to connect for; the engine requests the smallest set of provider scopes they map to.
	Capabilities []string
	// RedirectURI is where the provider sends the browser back: a loopback URI on a port the host bound, or a registered HTTPS redirect.
	RedirectURI string
	// Reauthorize names an existing connection to renew (re-consent or account switch).
	Reauthorize string
	// SwitchAccount asks the provider for another account.
	SwitchAccount bool
	// Label is an optional name for the connection.
	Label string
}

// OAuthScope is one scope as the consent shows it.
type OAuthScope struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	DataAccess  string `json:"data_access"`
}

// OAuthConsent is what the host shows before the user is sent to the provider: the provider, the account, the
// scopes and what they reach. It holds no secret.
type OAuthConsent struct {
	ProviderName    string         `json:"provider_name"`
	Account         pv.SafeAccount `json:"account"`
	Scopes          []OAuthScope   `json:"scopes"`
	DataAccess      []string       `json:"data_access"`
	Reauthorization bool           `json:"reauthorization"`
	SwitchAccount   bool           `json:"switch_account"`
}

// OAuthStarted is the answer of StartOAuth.
type OAuthStarted struct {
	// AuthorizationURL is opened in the user's browser. It holds the state, the PKCE challenge and the nonce,
	// never a token or a verifier.
	AuthorizationURL string       `json:"authorization_url"`
	RedirectURI      string       `json:"redirect_uri"`
	ExpiresAt        string       `json:"expires_at"`
	Consent          OAuthConsent `json:"consent"`
}

// OAuthCallbackInput is the redirect the browser followed, relayed by the host.
type OAuthCallbackInput struct {
	// RedirectURI is the redirect URI the browser arrived on, without the query.
	RedirectURI string
	// Query is the query string of the redirect (`code`, `state`, `error`, `iss`). It holds the authorization
	// code: the SDK sends it and never echoes or logs it.
	Query string
}

// ---- validation ----

var (
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	killReasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

const (
	maxProviderList  = 64
	maxCredentialKey = 4096
)

func providerParam(provider string) (map[string]string, error) {
	if !providerIDPattern.MatchString(provider) {
		return nil, invalidArgument("provider", "it must be a provider id: a lowercase letter, then lowercase letters, digits, _ or -, at most 32 characters")
	}
	return map[string]string{"provider": provider}, nil
}

func providersRequest(op Operation, params map[string]string, query url.Values, body []byte) request {
	req := request{op: op, params: params, query: query, accept: "application/json"}
	if body != nil {
		req.contentType = "application/json"
		req.body = body
	}
	return req
}

func checkConnectionStatus(s *pv.ConnectionStatus) error {
	if s.AsOf == "" || s.Provider.ProfileID == "" || s.Provider.ProviderID == "" {
		return errors.New("connection status: as_of and provider are required")
	}
	if !s.State.Valid() || !s.Health.Valid() {
		return errors.New("connection status: state or health is not known")
	}
	if s.Kind != nil && !s.Kind.Valid() {
		return errors.New("connection status: kind is not known")
	}
	if s.State == pv.ConnectionStatusStateNotConnected && (s.Kind != nil || s.ConnectedAt != nil) {
		return errors.New("connection status: a provider that is not connected has no kind and no connection time")
	}
	if s.State == pv.ConnectionStatusStateConnected && (s.Kind == nil || s.ConnectedAt == nil) {
		return errors.New("connection status: a connected provider has a kind and a connection time")
	}
	return nil
}

func checkBudgetState(s *pv.BudgetState) error {
	if s.BudgetID == "" || !s.State.Valid() || len(s.Windows) != 2 {
		return errors.New("budget state: a budget id, a known state and two windows are required")
	}
	return nil
}

// ---- operations ----

// List returns the provider profiles of the caller's application with the connection status of the caller's
// tenant: the state, how it is connected (`oauth` or `api_key`), a safe account identifier, times, warnings,
// health, budget state and whether the billing notice was acknowledged. Never a credential reference, a
// fingerprint or a secret.
func (p *ProvidersClient) List(ctx context.Context, options ...CallOption) (*ProviderList, error) {
	return doJSON(ctx, p.c.t, providersRequest(opListProviders, nil, nil, nil), applyOptions(options).deadline(p.c.timeout), func(l *ProviderList) error {
		if l.Providers == nil {
			return errors.New("provider list: providers is required")
		}
		for i := range l.Providers {
			if err := checkConnectionStatus(&l.Providers[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// SubmitCredential sends a manual credential for a provider without a suitable OAuth flow. It is the one place
// a secret enters the system over HTTP and it is write-only: the key goes straight to the credential service, is
// validated with a minimally scoped provider request and stored in the secret backend; it is never returned,
// logged or put in an error (the SDK refuses a response that echoes it, and strips it from any error it finds
// in one). The returned status shows the state and a safe account identifier, and a warning when the provider
// reports a key broader than needed (such a key is stored quarantined). Prefer OAuth where the provider has it.
func (p *ProvidersClient) SubmitCredential(ctx context.Context, provider string, input CredentialInput, options ...CallOption) (*pv.ConnectionStatus, error) {
	params, err := providerParam(provider)
	if err != nil {
		return nil, err
	}
	key := input.Key.v
	if key == "" || len(key) > maxCredentialKey || strings.IndexFunc(key, unicode.IsControl) >= 0 || strings.TrimSpace(key) != key {
		// The key is never named in the message.
		return nil, invalidArgument("Key", "it must be 1 to 4096 characters with no control character and no padding")
	}
	if len(input.Label) > 128 {
		return nil, invalidArgument("Label", "it must be at most 128 characters")
	}
	if len(input.Scopes) > maxProviderList {
		return nil, invalidArgument("Scopes", "it must list at most 64 scopes")
	}
	wire := struct {
		Key       string   `json:"key"`
		Label     string   `json:"label,omitempty"`
		ExpiresAt string   `json:"expires_at,omitempty"`
		Scopes    []string `json:"scopes,omitempty"`
	}{Key: key, Label: input.Label, Scopes: input.Scopes}
	if !input.ExpiresAt.IsZero() {
		wire.ExpiresAt = input.ExpiresAt.UTC().Format(time.RFC3339)
	}
	body, _ := json.Marshal(wire) // the only place the key is read
	st, err := doJSON(ctx, p.c.t, providersRequest(opPutProviderCredential, params, nil, body), applyOptions(options).deadline(p.c.timeout),
		func(s *pv.ConnectionStatus) error {
			if err := checkConnectionStatus(s); err != nil {
				return err
			}
			// A status never holds the key. One that does is a server that echoes secrets: refuse it, and say nothing about it.
			if echoed, _ := json.Marshal(s); len(key) >= 8 && bytes.Contains(echoed, []byte(key)) {
				return errors.New("the answer repeats the credential")
			}
			return nil
		})
	if err != nil {
		return nil, scrubSecret(err, key)
	}
	return st, nil
}

// scrubSecret returns err unchanged unless it quotes the key, in which case it becomes a fixed error of the same code.
func scrubSecret(err error, key string) error {
	if len(key) < 4 {
		return err
	}
	if strings.Contains(err.Error(), key) {
		code := contracts.CodeOf(err)
		if code == "" {
			code = contracts.ErrInternal
		}
		return contracts.New(code, "the credential was not accepted")
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		if details, _ := json.Marshal(ce.Details); bytes.Contains(details, []byte(key)) {
			return contracts.New(ce.Code, "the credential was not accepted")
		}
	}
	return err
}

// Disconnect revokes the tenant's credential for a provider at once (at the provider too, where it offers
// revocation). Calls that need it fail closed and never fall back by themselves. It returns the status after.
func (p *ProvidersClient) Disconnect(ctx context.Context, provider string, options ...CallOption) (*pv.ConnectionStatus, error) {
	params, err := providerParam(provider)
	if err != nil {
		return nil, err
	}
	return doJSON(ctx, p.c.t, providersRequest(opDeleteProviderConn, params, nil, nil), applyOptions(options).deadline(p.c.timeout), checkConnectionStatus)
}

// AcknowledgeBillingNotice records that the operator has seen the provider's billing notice and confirms both
// statements (a consumer chat subscription is not API access; API usage is billed separately). The operator and
// the time are the token's and the clock's. A provider is not usable until its notice is acknowledged. The
// notice version must be the profile's current one (`conflict` otherwise).
func (p *ProvidersClient) AcknowledgeBillingNotice(ctx context.Context, provider string, input BillingNoticeAckInput, options ...CallOption) (*pv.BillingNoticeAck, error) {
	params, err := providerParam(provider)
	if err != nil {
		return nil, err
	}
	if input.NoticeVersion == "" || len(input.NoticeVersion) > 64 {
		return nil, invalidArgument("NoticeVersion", "it must be the notice version, 1 to 64 characters")
	}
	if !input.ConsumerSubscriptionIsNotAPIAccess || !input.SeparateAPIBillingUnderstood {
		return nil, invalidArgument("input", "both billing statements must be confirmed")
	}
	body := jsonBody(map[string]any{
		"notice_version": input.NoticeVersion,
		"consumer_subscription_is_not_api_access": true,
		"separate_api_billing_understood":         true,
	})
	return doJSON(ctx, p.c.t, providersRequest(opAckProviderBillingNote, params, nil, body), applyOptions(options).deadline(p.c.timeout),
		func(a *pv.BillingNoticeAck) error {
			if a.AckID == "" || a.NoticeVersion == "" || !a.Confirmations.ConsumerSubscriptionIsNotAPIAccess || !a.Confirmations.SeparateAPIBillingUnderstood {
				return errors.New("billing notice acknowledgement: id, version and both confirmations are required")
			}
			return nil
		})
}

// GetDisclosure returns what would leave the deployment for a request to a provider: which provider and model
// would process it, which data classes and categories may leave, whether RAG excerpts are included, the
// provider's stated retention and training terms, an upper-bound cost and what happens when the provider is
// unavailable. When any request-independent check fails (kill switch, policy, profile, allowlist, region)
// nothing is listed as leaving. It records nothing.
func (p *ProvidersClient) GetDisclosure(ctx context.Context, provider string, query DisclosureQuery, options ...CallOption) (*pv.EgressDisclosure, error) {
	params, err := providerParam(provider)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if query.Role != "" {
		if !query.Role.Valid() {
			return nil, invalidArgument("Role", "it is not a known role")
		}
		q.Set("role", string(query.Role))
	}
	if len(query.SourceIDs) > maxProviderList {
		return nil, invalidArgument("SourceIDs", "it must list at most 64 sources")
	}
	for _, s := range query.SourceIDs {
		if s == "" || len(s) > 64 || strings.ContainsAny(s, " \t\r\n,") {
			return nil, invalidArgument("SourceIDs", "a source id is malformed")
		}
		q.Add("source", s)
	}
	for name, n := range map[string]*int64{"estimated_toks_in": query.EstimatedToksIn, "max_output_toks": query.MaxOutputToks} {
		if n == nil {
			continue
		}
		if *n < 0 || *n > 1<<40 {
			return nil, invalidArgument(name, "it must be a non-negative count")
		}
		q.Set(name, strconv.FormatInt(*n, 10))
	}
	return doJSON(ctx, p.c.t, providersRequest(opGetProviderDisclosure, params, q, nil), applyOptions(options).deadline(p.c.timeout),
		func(d *pv.EgressDisclosure) error {
			if d.GeneratedAt == "" || d.Provider.ProviderID == "" || !d.Role.Valid() {
				return errors.New("egress disclosure: generated_at, provider and role are required")
			}
			if !d.ExternalEnabled && (len(d.DataClasses) > 0 || len(d.DataCategories) > 0 || d.RAGExcerptsIncluded) {
				return errors.New("egress disclosure: a request that stays local lists nothing as leaving")
			}
			return nil
		})
}

func checkMode(m *ProviderMode) error {
	if !m.Effective.Valid() || !m.Configured.Valid() || m.DecidedBy == "" || m.Levels == nil {
		return errors.New("provider mode: effective, configured, decided_by, kill_switch and levels are required")
	}
	for level, mode := range m.Levels {
		if !level.Valid() || !mode.Valid() {
			return errors.New("provider mode: a level or a mode is not known")
		}
	}
	if m.KillSwitch && m.Effective != pv.DeploymentModeLocal {
		return errors.New("provider mode: under the kill switch the effective mode is local")
	}
	return nil
}

// GetMode returns the mode configured at each level (deployment, application, tenant), the effective mode (the
// most restrictive level; `local` under the kill switch) and what decided it. Local is the default everywhere.
func (p *ProvidersClient) GetMode(ctx context.Context, options ...CallOption) (*ProviderMode, error) {
	return doJSON(ctx, p.c.t, providersRequest(opGetProviderMode, nil, nil, nil), applyOptions(options).deadline(p.c.timeout), checkMode)
}

// SetMode sets `local`, `hybrid` or `external` at one level of the caller's own scope. The tenant level needs a
// tenant administrator; the application and deployment levels need a deployment operator. The change is audited.
// The M3 settings API cannot do this.
func (p *ProvidersClient) SetMode(ctx context.Context, level ModeLevel, mode pv.DeploymentMode, options ...CallOption) (*ProviderMode, error) {
	if !level.Valid() {
		return nil, invalidArgument("level", "it must be deployment, application or tenant")
	}
	if !mode.Valid() {
		return nil, invalidArgument("mode", "it must be local, hybrid or external")
	}
	body := jsonBody(map[string]string{"level": string(level), "mode": string(mode)})
	return doJSON(ctx, p.c.t, providersRequest(opPutProviderMode, nil, nil, body), applyOptions(options).deadline(p.c.timeout), checkMode)
}

func checkEgressLevel(level pv.EgressLevel, sourceID string) error {
	if !level.Valid() {
		return invalidArgument("level", "it must be deployment, application, tenant, workspace or source")
	}
	if (level == pv.EgressLevelSource) != (sourceID != "") || len(sourceID) > 64 {
		return invalidArgument("sourceID", "it is for the source level and only for it")
	}
	return nil
}

// GetEgressPolicy returns the data-egress policy (what may leave) at one level of the caller's scope; the
// policy is nil for a level that has none, and external processing is disabled there. sourceID names the RAG
// source of a source-level policy, and only then.
func (p *ProvidersClient) GetEgressPolicy(ctx context.Context, level pv.EgressLevel, sourceID string, options ...CallOption) (*EgressPolicyView, error) {
	if err := checkEgressLevel(level, sourceID); err != nil {
		return nil, err
	}
	q := url.Values{"level": {string(level)}}
	if sourceID != "" {
		q.Set("source_id", sourceID)
	}
	return doJSON[EgressPolicyView](ctx, p.c.t, providersRequest(opGetProviderEgressPolicy, nil, q, nil), applyOptions(options).deadline(p.c.timeout), nil)
}

// SetEgressPolicy creates or replaces the policy of one level and returns it as stored. The tenant, workspace and
// source levels need a tenant administrator; the application and deployment levels need a deployment operator,
// and the token must carry the capability of the level (`dataegress.<level>.write`, and `dataegress.approve` for
// a policy that lets internal data leave). A stale ExpectedRevision is `conflict`. Credentials and raw
// application database content never leave, whatever a policy says.
func (p *ProvidersClient) SetEgressPolicy(ctx context.Context, update EgressPolicyUpdate, options ...CallOption) (*pv.EgressPolicy, error) {
	if err := checkEgressLevel(update.Level, string(update.SourceID)); err != nil {
		return nil, err
	}
	if !update.SourceMode.Valid() {
		return nil, invalidArgument("SourceMode", "it is not a known mode")
	}
	if update.ExpectedRevision < 0 {
		return nil, invalidArgument("ExpectedRevision", "it must not be negative")
	}
	if update.Files != nil && !update.Files.MaxClass.Valid() {
		return nil, invalidArgument("Files", "its MaxClass must be public, internal or confidential")
	}
	if update.Audio != nil && !update.Audio.MaxClass.Valid() {
		return nil, invalidArgument("Audio", "its MaxClass must be public, internal or confidential")
	}
	return doJSON(ctx, p.c.t, providersRequest(opPutProviderEgressPolicy, nil, nil, jsonBody(update)), applyOptions(options).deadline(p.c.timeout),
		func(e *pv.EgressPolicy) error {
			if e.PolicyID == "" || e.Revision < 1 || !e.Level.Valid() {
				return errors.New("egress policy: id, revision and level are required")
			}
			return nil
		})
}

// GetBudgets returns the per-provider, tenant, workspace and principal budgets (integer micro-units) of the
// caller's scope with the current state of each: spent, remaining, warnings and hard stops.
func (p *ProvidersClient) GetBudgets(ctx context.Context, options ...CallOption) (*ProviderBudgets, error) {
	return doJSON(ctx, p.c.t, providersRequest(opGetProviderBudgets, nil, nil, nil), applyOptions(options).deadline(p.c.timeout), func(b *ProviderBudgets) error {
		if b.Budgets == nil || b.States == nil {
			return errors.New("provider budgets: budgets and states are required")
		}
		for i := range b.States {
			if err := checkBudgetState(&b.States[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetBudget creates or changes a budget and returns it as stored. An empty BudgetID creates one; a given one
// must carry its current Revision (a stale one is `conflict`) and the subject, channel and currency of a budget
// never change. The core sets the id, revision, updated_at and updated_by, so whatever is put there is ignored.
// A provider-wide budget needs a deployment operator; a tenant, workspace or principal budget needs a tenant
// administrator. Money is integer micro-units.
func (p *ProvidersClient) SetBudget(ctx context.Context, budget pv.Budget, options ...CallOption) (*pv.Budget, error) {
	if !budget.Subject.Kind.Valid() || !budget.Channel.Valid() || budget.Currency == "" {
		return nil, invalidArgument("budget", "subject.kind, channel and currency are required")
	}
	if budget.WarnAtPercent < 1 || budget.WarnAtPercent > 99 {
		return nil, invalidArgument("budget", "warn_at_percent must be 1 to 99")
	}
	if budget.Limits.DailyMicroUnits < 0 || budget.Limits.MonthlyMicroUnits < 0 {
		return nil, invalidArgument("budget", "a limit must not be negative")
	}
	if budget.BudgetID != "" && budget.Revision < 1 {
		return nil, invalidArgument("budget", "a budget that exists is changed with the revision that was read")
	}
	return doJSON(ctx, p.c.t, providersRequest(opPutProviderBudget, nil, nil, jsonBody(budget)), applyOptions(options).deadline(p.c.timeout),
		func(b *pv.Budget) error {
			if b.BudgetID == "" || b.Revision < 1 {
				return errors.New("budget: id and revision are required")
			}
			return nil
		})
}

// ExportUsage exports the usage records (no prompt and no response) with totals per provider, channel and
// currency, for reconciliation with the provider's invoice. The tenant's usage is the default; the whole
// application needs a deployment operator.
func (p *ProvidersClient) ExportUsage(ctx context.Context, query UsageQuery, options ...CallOption) (UsageExport, error) {
	q := url.Values{}
	if !query.From.IsZero() {
		q.Set("from", query.From.UTC().Format(time.RFC3339))
	}
	if !query.To.IsZero() {
		q.Set("to", query.To.UTC().Format(time.RFC3339))
	}
	if query.Provider != "" {
		if !providerIDPattern.MatchString(query.Provider) {
			return nil, invalidArgument("Provider", "it must be a provider id")
		}
		q.Set("provider", query.Provider)
	}
	if query.Channel != "" {
		if !query.Channel.Valid() {
			return nil, invalidArgument("Channel", "it must be interactive or background")
		}
		q.Set("channel", string(query.Channel))
	}
	switch query.Scope {
	case "":
	case UsageScopeTenant, UsageScopeApplication:
		q.Set("scope", string(query.Scope))
	default:
		return nil, invalidArgument("Scope", "it must be tenant or application")
	}
	q.Set("format", "json")
	out, err := doJSON[UsageExport](ctx, p.c.t, providersRequest(opExportProviderUsage, nil, q, nil), applyOptions(options).deadline(p.c.timeout), nil)
	if err != nil {
		return nil, err
	}
	return *out, nil
}

func checkKillSwitch(k *KillSwitch) error {
	if k.Reason != "" && !killReasonPattern.MatchString(k.Reason) {
		return errors.New("kill switch: the reason is not a fixed code")
	}
	return nil
}

// GetKillSwitch reports whether the global external-disable switch is engaged. A deployment operator only.
func (p *ProvidersClient) GetKillSwitch(ctx context.Context, options ...CallOption) (*KillSwitch, error) {
	return doJSON(ctx, p.c.t, providersRequest(opGetProviderKillSwitch, nil, nil, nil), applyOptions(options).deadline(p.c.timeout), checkKillSwitch)
}

// SetKillSwitch engages or releases the global kill switch. Engaging stops every external provider request of
// the deployment at once, whatever any policy says, and survives a restart; releasing it enables nothing by
// itself. reason is a fixed code (lowercase letters, digits and _), never free text; empty for none. A
// deployment operator only.
func (p *ProvidersClient) SetKillSwitch(ctx context.Context, engaged bool, reason string, options ...CallOption) (*KillSwitch, error) {
	if reason != "" && !killReasonPattern.MatchString(reason) {
		return nil, invalidArgument("reason", "it must be a fixed code of lowercase letters, digits and _")
	}
	wire := map[string]any{"engaged": engaged}
	if reason != "" {
		wire["reason"] = reason
	}
	return doJSON(ctx, p.c.t, providersRequest(opPutProviderKillSwitch, nil, nil, jsonBody(wire)), applyOptions(options).deadline(p.c.timeout), checkKillSwitch)
}

// IsManualFallback reports whether err is the answer of StartOAuth for a provider without OAuth for API
// access: `validation_failed` with details.reason `manual_fallback`. Use SubmitCredential for that provider.
func IsManualFallback(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrValidationFailed {
		return false
	}
	reason, _ := ce.Details["reason"].(string)
	return reason == "manual_fallback"
}

// StartOAuth prepares the consent for the requested capabilities and begins the browser authorization. Open
// the returned AuthorizationURL in the user's browser and show the Consent first. A provider without OAuth for
// API access answers `validation_failed` with details.reason `manual_fallback` (test with IsManualFallback):
// use SubmitCredential for it. The PKCE verifier and the tokens never reach the host.
func (p *ProvidersClient) StartOAuth(ctx context.Context, provider string, input OAuthStartInput, options ...CallOption) (*OAuthStarted, error) {
	params, err := providerParam(provider)
	if err != nil {
		return nil, err
	}
	if len(input.Capabilities) == 0 || len(input.Capabilities) > maxProviderList {
		return nil, invalidArgument("Capabilities", "it must list 1 to 64 capabilities")
	}
	for _, c := range input.Capabilities {
		if c == "" {
			return nil, invalidArgument("Capabilities", "a capability must not be empty")
		}
	}
	if input.RedirectURI == "" || len(input.RedirectURI) > 512 {
		return nil, invalidArgument("RedirectURI", "it is required, at most 512 characters")
	}
	if len(input.Label) > 128 {
		return nil, invalidArgument("Label", "it must be at most 128 characters")
	}
	wire := map[string]any{"capabilities": input.Capabilities, "redirect_uri": input.RedirectURI}
	if input.Reauthorize != "" {
		wire["reauthorize"] = input.Reauthorize
	}
	if input.SwitchAccount {
		wire["switch_account"] = true
	}
	if input.Label != "" {
		wire["label"] = input.Label
	}
	return doJSON(ctx, p.c.t, providersRequest(opStartProviderOAuth, params, nil, jsonBody(wire)), applyOptions(options).deadline(p.c.timeout),
		func(s *OAuthStarted) error {
			if s.AuthorizationURL == "" || s.RedirectURI == "" || s.ExpiresAt == "" || s.Consent.ProviderName == "" || s.Consent.Scopes == nil {
				return errors.New("oauth start: authorization_url, redirect_uri, expires_at and the consent are required")
			}
			return nil
		})
}

// CompleteOAuth relays the redirect the browser followed to the OAuth engine with a FRESH context token of the
// principal that started the flow (tokens are single use). A state that is unknown, replayed, expired or issued
// to another scope is refused and spent; nothing is stored unless every check passed. It returns the status of
// the new connection. The code and the state are sent and never echoed or logged.
func (p *ProvidersClient) CompleteOAuth(ctx context.Context, input OAuthCallbackInput, options ...CallOption) (*pv.ConnectionStatus, error) {
	query := strings.TrimPrefix(input.Query, "?")
	if input.RedirectURI == "" || len(input.RedirectURI) > 512 {
		return nil, invalidArgument("RedirectURI", "it is required, at most 512 characters")
	}
	if query == "" || len(query) > 4096 {
		return nil, invalidArgument("Query", "it is required, at most 4096 characters")
	}
	body := jsonBody(map[string]string{"redirect_uri": input.RedirectURI, "query": query})
	st, err := doJSON(ctx, p.c.t, providersRequest(opCompleteProviderOAuth, nil, nil, body), applyOptions(options).deadline(p.c.timeout), checkConnectionStatus)
	if err != nil {
		return nil, scrubSecret(err, query)
	}
	return st, nil
}
