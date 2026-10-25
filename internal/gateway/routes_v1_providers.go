package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The operator-facing surface of External Provider Mode (additive plan 6.3, 11 and 16; milestone X).
// The contract is schemas/api/paths/providers.yaml and oauth.yaml; TestRoutesMatchOpenAPISpec fails on any
// difference between the routes registered here and the documents.
//
//	GET    /v1/providers                                   profiles and their connection status
//	PUT    /v1/providers/{provider}/credential             write-only manual credential submit
//	DELETE /v1/providers/{provider}/connection             disconnect (revokes the credential)
//	POST   /v1/providers/{provider}/billing-notice/ack     billing-notice acknowledgement
//	GET    /v1/providers/{provider}/disclosure             egress disclosure
//	GET    /v1/providers/mode, PUT /v1/providers/mode      mode per scope level
//	GET    /v1/providers/egress-policy, PUT                egress policy per level
//	GET    /v1/providers/budgets, PUT                      budgets and their states
//	GET    /v1/providers/usage/export
//	GET    /v1/providers/kill-switch, PUT                  the global external-disable switch
//	POST   /v1/oauth/{provider}/start                      starts browser OAuth, returns the authorization URL
//	POST   /v1/oauth/callback                              completes it with the redirect the browser followed
//
// # Who may call
//
// Every route needs an OPERATOR, decided by ProvidersService.AuthorizeOperator from the deployment's own
// configuration, never from a role or capability scope the token asserts (a token that says "admin" is as
// powerless as one that says nothing). Two levels exist: a tenant administrator, for what concerns the
// caller's own tenant, and a deployment operator, for what concerns more than one tenant (the kill switch, the
// deployment and application egress policy and modes, provider-wide budgets). The decision is made BEFORE any body
// is read. With nobody configured nobody passes. The M3 settings API has no member and no route for any of it.
//
// # Scope
//
// Application, tenant, workspace and principal come from the verified single-use context token. A path holds the
// provider id and nothing else; a query is validation_failed except on the few reads that name one, and every
// body is a closed object: a client cannot name another tenant.
//
// # Secrets
//
// The manual credential route is the one place a secret enters the system over HTTP. It reads the body into a
// type that prints as a placeholder everywhere (String, GoString, Format, LogValue, MarshalJSON, MarshalText),
// hands it to the service and answers with the connection status only. The access log records method, path and
// status, never a body or a query. A malformed body, a service error and a panic all answer with fixed messages:
// no decoder or service message is forwarded, so the value cannot reach a response. The OAuth start response holds
// the authorization URL (state, challenge and nonce, no verifier) and the consent; the callback carries a one-time
// code that is passed to the service and never logged or echoed.
//
// # Wiring
//
// The area needs the ApplicationService and only WANTS the ProvidersService: the composition root provides it
// when external mode is built and enabled, and until then every route answers unavailable. That keeps the
// noexternal build starting and the rigs that provide only the ApplicationService working.

func init() { RegisterArea("v1_providers", registerV1Providers) }

// ProvidersOperatorLevel is who may do something.
type ProvidersOperatorLevel string

// The two operator levels.
const (
	// ProvidersOperatorTenant is an administrator of the caller's own tenant.
	ProvidersOperatorTenant ProvidersOperatorLevel = "tenant"
	// ProvidersOperatorDeployment is an operator of the deployment.
	ProvidersOperatorDeployment ProvidersOperatorLevel = "deployment"
)

// ProvidersService is what the providers and oauth areas need. The composition root provides the
// implementation over the X packages (internal/cli/providers_external.go); tests stand in their own. Every
// method takes the verified Scope and answers for it alone. The service does not check roles except
// AuthorizeOperator, which the handlers call first; the other methods are called only for an authorised operator.
type ProvidersService interface {
	// AuthorizeOperator returns nil only for an operator of the deployment's own configuration at the
	// level (never a role or capability the token asserts) and permission_denied for everybody else. A
	// deployment operator is also a tenant administrator.
	AuthorizeOperator(ctx context.Context, scope contracts.Scope, level ProvidersOperatorLevel) error

	// Connections lists the provider profiles of the scope's application with their connection status.
	Connections(ctx context.Context, scope contracts.Scope) ([]pv.ConnectionStatus, error)
	// OAuthStart prepares the consent and starts the authorization; the result holds the URL.
	OAuthStart(ctx context.Context, scope contracts.Scope, req ProvidersOAuthStart) (ProvidersOAuthStarted, error)
	// OAuthCallback completes the authorization with the redirect the browser followed. A state that does not
	// match is refused and the state is spent either way.
	OAuthCallback(ctx context.Context, scope contracts.Scope, cb ProvidersOAuthCallback) (pv.ConnectionStatus, error)
	// Disconnect revokes the credential of the provider for the scope's tenant.
	Disconnect(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (pv.ConnectionStatus, error)
	// SubmitCredential validates and stores a manual credential and returns the status.
	SubmitCredential(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in ProvidersCredentialInput) (pv.ConnectionStatus, error)
	// AcknowledgeBillingNotice records the acknowledgement of the provider's billing notice.
	AcknowledgeBillingNotice(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in ProvidersBillingAckInput) (pv.BillingNoticeAck, error)
	// Disclosure computes what a host may show before a request leaves the deployment.
	Disclosure(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in ProvidersDisclosureInput) (pv.EgressDisclosure, error)

	// Mode reports the configured modes of the scope's levels and the effective one.
	Mode(ctx context.Context, scope contracts.Scope) (ProvidersModeView, error)
	// SetMode sets the mode of one level and audits the change.
	SetMode(ctx context.Context, scope contracts.Scope, level ProvidersModeLevel, mode pv.DeploymentMode) (ProvidersModeView, error)

	// EgressPolicy returns the policy of one level (found false: the level has none and is disabled).
	EgressPolicy(ctx context.Context, scope contracts.Scope, level pv.EgressLevel, source pv.SourceID) (pv.EgressPolicy, bool, error)
	// PutEgressPolicy creates or replaces the policy of one level.
	PutEgressPolicy(ctx context.Context, scope contracts.Scope, in ProvidersEgressPolicyInput) (pv.EgressPolicy, error)

	// Budgets lists the budgets of the scope with their states.
	Budgets(ctx context.Context, scope contracts.Scope) (ProvidersBudgetsView, error)
	// PutBudget creates or changes a budget.
	PutBudget(ctx context.Context, scope contracts.Scope, b pv.Budget) (pv.Budget, error)
	// Usage exports the usage records as JSON or CSV with totals.
	Usage(ctx context.Context, scope contracts.Scope, q ProvidersUsageQuery) (ProvidersUsageResult, error)

	// KillSwitch reports the global external-disable switch of the deployment.
	KillSwitch(ctx context.Context, scope contracts.Scope) (ProvidersKillSwitch, error)
	// SetKillSwitch engages or releases it, recording the operator and the fixed reason code.
	SetKillSwitch(ctx context.Context, scope contracts.Scope, engage bool, reason string) (ProvidersKillSwitch, error)
}

// ProvidersOAuthStart is the body of POST /v1/oauth/{provider}/start. The provider comes from the path.
type ProvidersOAuthStart struct {
	Provider      pv.ProviderID
	Capabilities  []string
	RedirectURI   string
	Reauthorize   pv.CredentialRefID
	SwitchAccount bool
	Label         string
}

// ProvidersOAuthScope is one scope as the consent shows it.
type ProvidersOAuthScope struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	DataAccess  string `json:"data_access"`
}

// ProvidersOAuthConsent is what the host shows before the user is sent to the provider. No secret.
type ProvidersOAuthConsent struct {
	ProviderName    string                `json:"provider_name"`
	Account         pv.SafeAccount        `json:"account"`
	Scopes          []ProvidersOAuthScope `json:"scopes"`
	DataAccess      []string              `json:"data_access"`
	Reauthorization bool                  `json:"reauthorization"`
	SwitchAccount   bool                  `json:"switch_account"`
}

// ProvidersOAuthStarted is the answer of the start route: the URL to open in the user's browser.
type ProvidersOAuthStarted struct {
	AuthorizationURL string                `json:"authorization_url"`
	RedirectURI      string                `json:"redirect_uri"`
	ExpiresAt        time.Time             `json:"expires_at"`
	Consent          ProvidersOAuthConsent `json:"consent"`
}

// ProvidersOAuthCallback is what the redirect carried: where it arrived and its query.
type ProvidersOAuthCallback struct {
	RedirectURI string
	Query       url.Values
}

// ProvidersSecret is a credential value in transit. It prints as a placeholder in every format a logger, an
// error or an encoder could use; the one way to read it is Reveal, which only the service calls.
type ProvidersSecret struct{ v string }

const providersSecretPlaceholder = "[redacted]"

// Reveal returns the value.
func (s ProvidersSecret) Reveal() string { return s.v }

// Empty reports whether no value was given.
func (s ProvidersSecret) Empty() bool { return s.v == "" }

// String implements fmt.Stringer.
func (ProvidersSecret) String() string { return providersSecretPlaceholder }

// GoString implements fmt.GoStringer.
func (ProvidersSecret) GoString() string {
	return "gateway.ProvidersSecret(" + providersSecretPlaceholder + ")"
}

// Format implements fmt.Formatter: no verb prints the value.
func (ProvidersSecret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, providersSecretPlaceholder)
}

// LogValue implements slog.LogValuer.
func (ProvidersSecret) LogValue() slog.Value { return slog.StringValue(providersSecretPlaceholder) }

// MarshalJSON never encodes the value.
func (ProvidersSecret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + providersSecretPlaceholder + `"`), nil
}

// MarshalText never encodes the value.
func (ProvidersSecret) MarshalText() ([]byte, error) { return []byte(providersSecretPlaceholder), nil }

// UnmarshalJSON reads a JSON string into the secret; it is the only way one is built from a request.
func (s *ProvidersSecret) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return errors.New("not a string")
	}
	s.v = v
	return nil
}

// ProvidersCredentialInput is the body of the manual credential submit.
type ProvidersCredentialInput struct {
	Key       ProvidersSecret `json:"key"`
	Label     string          `json:"label,omitempty"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
	Scopes    []string        `json:"scopes,omitempty"`
}

// ProvidersBillingAckInput is the body of the billing-notice acknowledgement: the version shown and the two
// statements. The operator and the time are the token's and the clock's.
type ProvidersBillingAckInput struct {
	NoticeVersion                      string `json:"notice_version"`
	ConsumerSubscriptionIsNotAPIAccess bool   `json:"consumer_subscription_is_not_api_access"`
	SeparateAPIBillingUnderstood       bool   `json:"separate_api_billing_understood"`
}

// ProvidersDisclosureInput describes the request a disclosure is for.
type ProvidersDisclosureInput struct {
	Role            pv.Role
	Sources         []pv.SourceID
	EstimatedToksIn int64
	MaxOutputToks   int64
}

// ProvidersModeLevel is where a mode is set.
type ProvidersModeLevel string

// The levels, widest first.
const (
	ProvidersModeDeployment  ProvidersModeLevel = "deployment"
	ProvidersModeApplication ProvidersModeLevel = "application"
	ProvidersModeTenant      ProvidersModeLevel = "tenant"
)

// Valid reports whether l is one of the three levels.
func (l ProvidersModeLevel) Valid() bool {
	switch l {
	case ProvidersModeDeployment, ProvidersModeApplication, ProvidersModeTenant:
		return true
	}
	return false
}

// ProvidersModeView is the mode of a scope. Effective is the most restrictive level, local under the kill switch.
type ProvidersModeView struct {
	Effective  pv.DeploymentMode                        `json:"effective"`
	Configured pv.DeploymentMode                        `json:"configured"`
	DecidedBy  string                                   `json:"decided_by"`
	KillSwitch bool                                     `json:"kill_switch"`
	Levels     map[ProvidersModeLevel]pv.DeploymentMode `json:"levels"`
}

// ProvidersEgressPolicyInput is the body of PUT /v1/providers/egress-policy: the operator-supplied members of an
// egress policy. The target, id, approval and timestamps are minted by the core.
type ProvidersEgressPolicyInput struct {
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
	// Files is the files setting (whether excerpts of uploaded files may leave, and up to which class); absent keeps
	// them local.
	Files *pv.EgressPolicyContentEgress `json:"files,omitempty"`
	// Audio is the audio setting (whether raw audio may leave for an external speech recogniser, and up to which
	// class); absent keeps it local.
	Audio *pv.EgressPolicyContentEgress `json:"audio,omitempty"`
}

// ProvidersBudgetsView is the budgets of a scope with their states.
type ProvidersBudgetsView struct {
	Budgets []pv.Budget      `json:"budgets"`
	States  []pv.BudgetState `json:"states"`
}

// ProvidersUsageQuery selects the usage to export.
type ProvidersUsageQuery struct {
	From, To   time.Time
	ProviderID pv.ProviderID
	Channel    pv.Channel
	// Wide asks for the whole application instead of the caller's tenant (a deployment operator only).
	Wide bool
	// CSV selects the CSV rendering.
	CSV bool
}

// ProvidersUsageResult is a rendered export.
type ProvidersUsageResult struct {
	ContentType string
	Body        []byte
}

// ProvidersKillSwitch is the state of the global switch.
type ProvidersKillSwitch struct {
	Engaged   bool       `json:"engaged"`
	Reason    string     `json:"reason,omitempty"`
	ChangedBy string     `json:"changed_by,omitempty"`
	ChangedAt *time.Time `json:"changed_at,omitempty"`
}

var (
	providersProviderPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	providersReasonPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	providersModelPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)
)

const (
	providersCredentialBodyMax = 8 << 10
	providersCallbackQueryMax  = 4 << 10
	providersMaxListed         = 64
)

type providersHandler struct {
	apps ApplicationService
	svc  ProvidersService // nil while none is provided
}

func registerV1Providers(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[ProvidersService](n)
	if !n.Ready() {
		return
	}
	h := &providersHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("GET /providers", h.list)
	v1.HandleFunc("PUT /providers/{provider}/credential", h.submitCredential)
	v1.HandleFunc("DELETE /providers/{provider}/connection", h.disconnect)
	v1.HandleFunc("POST /providers/{provider}/billing-notice/ack", h.ackBillingNotice)
	v1.HandleFunc("GET /providers/{provider}/disclosure", h.disclosure)
	v1.HandleFunc("GET /providers/mode", h.getMode)
	v1.HandleFunc("PUT /providers/mode", h.putMode)
	v1.HandleFunc("GET /providers/egress-policy", h.getEgressPolicy)
	v1.HandleFunc("PUT /providers/egress-policy", h.putEgressPolicy)
	v1.HandleFunc("GET /providers/budgets", h.getBudgets)
	v1.HandleFunc("PUT /providers/budgets", h.putBudget)
	v1.HandleFunc("GET /providers/usage/export", h.usageExport)
	v1.HandleFunc("GET /providers/kill-switch", h.getKillSwitch)
	v1.HandleFunc("PUT /providers/kill-switch", h.putKillSwitch)
	v1.HandleFunc("POST /oauth/{provider}/start", h.oauthStart)
	v1.HandleFunc("POST /oauth/callback", h.oauthCallback)
}

// begin is the start of every handler: the verified scope, a query only where the route allows names, an active
// application, a provided service and the operator gate, or the refusal already written. The gate runs before
// anything of the body is read.
func (h *providersHandler) begin(w http.ResponseWriter, r *http.Request, level ProvidersOperatorLevel, queryNames ...string) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "external providers are not available on this server"))
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := providersAuthorize(r.Context(), h.svc, scope, level); err != nil {
		WriteError(w, r, err)
		return contracts.Scope{}, false
	}
	if bad := providersQueryError(r, queryNames); bad != nil {
		WriteError(w, r, bad)
		return contracts.Scope{}, false
	}
	return scope, true
}

// providersAuthorize asks the service; an error that is not typed is not a decision: deny.
func providersAuthorize(ctx context.Context, svc ProvidersService, scope contracts.Scope, level ProvidersOperatorLevel) error {
	if err := svc.AuthorizeOperator(ctx, scope, level); err != nil {
		var typed *contracts.Error
		if !errors.As(err, &typed) {
			err = contracts.New(contracts.ErrPermissionDenied, "the provider routes are for operators")
		}
		return err
	}
	return nil
}

// providersQueryError is nil when the query holds only the named parameters.
func providersQueryError(r *http.Request, names []string) error {
	if r.URL.RawQuery == "" {
		return nil
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return contracts.New(contracts.ErrValidationFailed, "the query is malformed")
	}
	for k := range q {
		allowed := false
		for _, n := range names {
			if k == n {
				allowed = true
			}
		}
		if !allowed {
			return contracts.New(contracts.ErrValidationFailed, "the query holds a parameter this route does not take: the scope is the verified token")
		}
	}
	return nil
}

func providersProvider(w http.ResponseWriter, r *http.Request) (pv.ProviderID, bool) {
	p := r.PathValue("provider")
	if !providersProviderPattern.MatchString(p) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the provider id is malformed"))
		return "", false
	}
	return pv.ProviderID(p), true
}

// providersStatus refuses a status that cannot be shown: a bad enum is an internal error, never a success.
func providersStatusOK(w http.ResponseWriter, r *http.Request, st pv.ConnectionStatus, status int) {
	b, err := json.Marshal(st)
	if err != nil {
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the connection status is invalid"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ---- profiles and connection ----

func (h *providersHandler) list(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	got, err := h.svc.Connections(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got == nil {
		got = []pv.ConnectionStatus{}
	}
	writeJSON(w, http.StatusOK, struct {
		Providers []pv.ConnectionStatus `json:"providers"`
	}{Providers: got})
}

func (h *providersHandler) disconnect(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	prov, ok := providersProvider(w, r)
	if !ok {
		return
	}
	st, err := h.svc.Disconnect(r.Context(), scope, prov)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	providersStatusOK(w, r, st, http.StatusOK)
}

// credentialBody is the closed body of the manual credential submit.
type providersCredentialBody struct {
	Key       ProvidersSecret `json:"key"`
	Label     string          `json:"label"`
	ExpiresAt *time.Time      `json:"expires_at"`
	Scopes    []string        `json:"scopes"`
}

// submitCredential is the one place a secret enters the system over HTTP. It reads the body itself, with a
// small cap, and answers every failure with a fixed message.
func (h *providersHandler) submitCredential(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	prov, ok := providersProvider(w, r)
	if !ok {
		return
	}
	refuse := func(why string) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, why))
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		WriteErrorStatus(w, r, http.StatusUnsupportedMediaType, contracts.New(contracts.ErrValidationFailed, "Content-Type must be application/json"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, providersCredentialBodyMax))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			WriteError(w, r, err)
			return
		}
		refuse("the credential body could not be read")
		return
	}
	var body providersCredentialBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		// Never forward the decoder's message: it can quote the caller's bytes.
		refuse("the credential body is not valid for this endpoint")
		return
	}
	if body.Key.Empty() || len(body.Label) > 128 || len(body.Scopes) > providersMaxListed {
		refuse("key is required; label and scopes are bounded")
		return
	}
	in := ProvidersCredentialInput{Key: body.Key, Label: body.Label, ExpiresAt: body.ExpiresAt, Scopes: body.Scopes}
	st, err := h.svc.SubmitCredential(r.Context(), scope, prov, in)
	if err != nil {
		providersCredentialError(w, r, err)
		return
	}
	providersStatusOK(w, r, st, http.StatusOK)
}

// providersCredentialError answers with the typed code and a fixed message: whatever the service or the
// provider said is dropped, because it might quote the key.
func providersCredentialError(w http.ResponseWriter, r *http.Request, err error) {
	code := contracts.CodeOf(err)
	if code == "" {
		code = contracts.ErrInternal
	}
	WriteError(w, r, contracts.New(code, "the credential was not accepted"))
}

func (h *providersHandler) ackBillingNotice(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	prov, ok := providersProvider(w, r)
	if !ok {
		return
	}
	var in ProvidersBillingAckInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.NoticeVersion == "" || len(in.NoticeVersion) > 64 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "notice_version is required"))
		return
	}
	if !in.ConsumerSubscriptionIsNotAPIAccess || !in.SeparateAPIBillingUnderstood {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "both billing confirmations must be true"))
		return
	}
	ack, err := h.svc.AcknowledgeBillingNotice(r.Context(), scope, prov, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

func (h *providersHandler) disclosure(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant, "role", "source", "estimated_toks_in", "max_output_toks")
	if !ok {
		return
	}
	prov, ok := providersProvider(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	in := ProvidersDisclosureInput{Role: pv.RoleConversationalController}
	if v := q.Get("role"); v != "" {
		in.Role = pv.Role(v)
		if !in.Role.Valid() {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "role is not a known role"))
			return
		}
	}
	for _, s := range q["source"] {
		if len(s) == 0 || len(s) > 64 || strings.ContainsAny(s, " \t\r\n,") {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "source is malformed"))
			return
		}
		in.Sources = append(in.Sources, pv.SourceID(s))
	}
	if len(in.Sources) > providersMaxListed {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "too many sources"))
		return
	}
	for name, dst := range map[string]*int64{"estimated_toks_in": &in.EstimatedToksIn, "max_output_toks": &in.MaxOutputToks} {
		if v := q.Get(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 || n > 1<<40 {
				WriteError(w, r, contracts.New(contracts.ErrValidationFailed, name+" is not a valid count"))
				return
			}
			*dst = n
		}
	}
	d, err := h.svc.Disclosure(r.Context(), scope, prov, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ---- mode ----

func (h *providersHandler) getMode(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	v, err := h.svc.Mode(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, providersModeView(v))
}

func providersModeView(v ProvidersModeView) ProvidersModeView {
	if v.Levels == nil {
		v.Levels = map[ProvidersModeLevel]pv.DeploymentMode{}
	}
	return v
}

type providersModeBody struct {
	Level ProvidersModeLevel `json:"level"`
	Mode  pv.DeploymentMode  `json:"mode"`
}

func (h *providersHandler) putMode(w http.ResponseWriter, r *http.Request) {
	// Whoever calls is at least a tenant administrator; the level decides whether more is needed, and that is
	// decided before the body of another level is acted on.
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	var in providersModeBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if !in.Level.Valid() || !in.Mode.Valid() {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "level (deployment, application or tenant) and mode (local, hybrid or external) are required"))
		return
	}
	if in.Level != ProvidersModeTenant {
		if err := providersAuthorize(r.Context(), h.svc, scope, ProvidersOperatorDeployment); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	v, err := h.svc.SetMode(r.Context(), scope, in.Level, in.Mode)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, providersModeView(v))
}

// ---- egress policy ----

// providersPolicyView is the body of GET /v1/providers/egress-policy: the policy, or null for a level with none.
type providersPolicyView struct {
	Policy *pv.EgressPolicy `json:"policy"`
}

func providersPolicyOperator(level pv.EgressLevel) ProvidersOperatorLevel {
	if level == pv.EgressLevelDeployment || level == pv.EgressLevelApplication {
		return ProvidersOperatorDeployment
	}
	return ProvidersOperatorTenant
}

func (h *providersHandler) getEgressPolicy(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant, "level", "source_id")
	if !ok {
		return
	}
	level := pv.EgressLevel(r.URL.Query().Get("level"))
	if !level.Valid() {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "level (deployment, application, tenant, workspace or source) is required"))
		return
	}
	source := pv.SourceID(r.URL.Query().Get("source_id"))
	if (level == pv.EgressLevelSource) != (source != "") || len(source) > 64 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "source_id is for the source level and only for it"))
		return
	}
	p, found, err := h.svc.EgressPolicy(r.Context(), scope, level, source)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if !found {
		// A level with no policy is disabled: external processing is off there.
		writeJSON(w, http.StatusOK, providersPolicyView{Policy: nil})
		return
	}
	writeJSON(w, http.StatusOK, providersPolicyView{Policy: &p})
}

func (h *providersHandler) putEgressPolicy(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	var in ProvidersEgressPolicyInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if !in.Level.Valid() || (in.Level == pv.EgressLevelSource) != (in.SourceID != "") || len(in.SourceID) > 64 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "level is required; source_id is for the source level and only for it"))
		return
	}
	if err := providersCheckAllowedModels(in.AllowedModels); err != nil {
		WriteError(w, r, err)
		return
	}
	if !in.SourceMode.Valid() || in.ExpectedRevision < 0 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "source_mode is required and expected_revision is not negative"))
		return
	}
	if (in.Files != nil && !in.Files.MaxClass.Valid()) || (in.Audio != nil && !in.Audio.MaxClass.Valid()) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "files and audio need enabled and a max_class of public, internal or confidential"))
		return
	}
	if need := providersPolicyOperator(in.Level); need == ProvidersOperatorDeployment {
		if err := providersAuthorize(r.Context(), h.svc, scope, need); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	p, err := h.svc.PutEgressPolicy(r.Context(), scope, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ---- budgets and usage ----

func (h *providersHandler) getBudgets(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	v, err := h.svc.Budgets(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if v.Budgets == nil {
		v.Budgets = []pv.Budget{}
	}
	if v.States == nil {
		v.States = []pv.BudgetState{}
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *providersHandler) putBudget(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	var b pv.Budget
	if !decodeJSON(w, r, &b) {
		return
	}
	// A provider-wide budget limits every tenant: a deployment operator. The others limit one tenant,
	// workspace or principal.
	if b.Subject.Kind == pv.BudgetSubjectKindProvider {
		if err := providersAuthorize(r.Context(), h.svc, scope, ProvidersOperatorDeployment); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	got, err := h.svc.PutBudget(r.Context(), scope, b)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *providersHandler) usageExport(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant, "from", "to", "provider", "channel", "scope", "format")
	if !ok {
		return
	}
	q := r.URL.Query()
	var in ProvidersUsageQuery
	for name, dst := range map[string]*time.Time{"from": &in.From, "to": &in.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				WriteError(w, r, contracts.New(contracts.ErrValidationFailed, name+" is not an RFC 3339 time"))
				return
			}
			*dst = t
		}
	}
	if v := q.Get("provider"); v != "" {
		if !providersProviderPattern.MatchString(v) {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "provider is malformed"))
			return
		}
		in.ProviderID = pv.ProviderID(v)
	}
	if v := q.Get("channel"); v != "" {
		in.Channel = pv.Channel(v)
		if !in.Channel.Valid() {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "channel is not a known channel"))
			return
		}
	}
	switch q.Get("format") {
	case "", "json":
	case "csv":
		in.CSV = true
	default:
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "format is json or csv"))
		return
	}
	switch q.Get("scope") {
	case "", "tenant":
	case "application":
		if err := providersAuthorize(r.Context(), h.svc, scope, ProvidersOperatorDeployment); err != nil {
			WriteError(w, r, err)
			return
		}
		in.Wide = true
	default:
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "scope is tenant or application"))
		return
	}
	res, err := h.svc.Usage(r.Context(), scope, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ct := res.ContentType
	if ct != "text/csv; charset=utf-8" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Body)
}

// ---- kill switch ----

func (h *providersHandler) getKillSwitch(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorDeployment)
	if !ok {
		return
	}
	ks, err := h.svc.KillSwitch(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ks)
}

type providersKillBody struct {
	Engaged *bool  `json:"engaged"`
	Reason  string `json:"reason"`
}

func (h *providersHandler) putKillSwitch(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorDeployment)
	if !ok {
		return
	}
	var in providersKillBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Engaged == nil || (in.Reason != "" && !providersReasonPattern.MatchString(in.Reason)) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "engaged is required; reason is a fixed code of lowercase letters, digits and _"))
		return
	}
	ks, err := h.svc.SetKillSwitch(r.Context(), scope, *in.Engaged, in.Reason)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ks)
}

// ---- OAuth ----

type providersOAuthStartBody struct {
	Capabilities  []string           `json:"capabilities"`
	RedirectURI   string             `json:"redirect_uri"`
	Reauthorize   pv.CredentialRefID `json:"reauthorize"`
	SwitchAccount bool               `json:"switch_account"`
	Label         string             `json:"label"`
}

func (h *providersHandler) oauthStart(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	prov, ok := providersProvider(w, r)
	if !ok {
		return
	}
	var in providersOAuthStartBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if len(in.Capabilities) == 0 || len(in.Capabilities) > providersMaxListed || in.RedirectURI == "" || len(in.RedirectURI) > 512 || len(in.Label) > 128 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "capabilities and redirect_uri are required and bounded"))
		return
	}
	got, err := h.svc.OAuthStart(r.Context(), scope, ProvidersOAuthStart{
		Provider: prov, Capabilities: in.Capabilities, RedirectURI: in.RedirectURI,
		Reauthorize: in.Reauthorize, SwitchAccount: in.SwitchAccount, Label: in.Label,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Consent.Scopes == nil {
		got.Consent.Scopes = []ProvidersOAuthScope{}
	}
	if got.Consent.DataAccess == nil {
		got.Consent.DataAccess = []string{}
	}
	writeJSON(w, http.StatusOK, got)
}

type providersOAuthCallbackBody struct {
	RedirectURI string `json:"redirect_uri"`
	Query       string `json:"query"`
}

// oauthCallback completes an authorization. The host application received the browser's redirect on the
// address it registered, and relays it here with a fresh token of the same principal that started the flow;
// the engine refuses any other scope.
func (h *providersHandler) oauthCallback(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r, ProvidersOperatorTenant)
	if !ok {
		return
	}
	var in providersOAuthCallbackBody
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.RedirectURI == "" || len(in.RedirectURI) > 512 || in.Query == "" || len(in.Query) > providersCallbackQueryMax {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "redirect_uri and query are required and bounded"))
		return
	}
	q, err := url.ParseQuery(strings.TrimPrefix(in.Query, "?"))
	if err != nil || q.Get("state") == "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the callback query has no usable state"))
		return
	}
	st, err := h.svc.OAuthCallback(r.Context(), scope, ProvidersOAuthCallback{RedirectURI: in.RedirectURI, Query: q})
	if err != nil {
		// The engine's message is already free of the state, the code and the URL; the typed code is kept.
		code := contracts.CodeOf(err)
		if code == "" {
			code = contracts.ErrInternal
		}
		WriteError(w, r, contracts.New(code, "the authorization could not be completed"))
		return
	}
	providersStatusOK(w, r, st, http.StatusOK)
}

// providersMaxAllowedModels is the most provider and model pairs one egress policy lists (the specification's maxItems).
const providersMaxAllowedModels = 128

// providersCheckAllowedModels holds the allowed models of an egress policy to the specification: each is a provider
// and a model id of the documented grammar (never a URL or a path), and the list is bounded. The decoder has already
// refused members the specification does not declare.
func providersCheckAllowedModels(models []pv.ModelRef) error {
	if len(models) > providersMaxAllowedModels {
		return contracts.New(contracts.ErrValidationFailed, "allowed_models lists too many models")
	}
	for _, m := range models {
		if !providersProviderPattern.MatchString(string(m.ProviderID)) || !providersModelPattern.MatchString(string(m.ModelID)) {
			return contracts.New(contracts.ErrValidationFailed, "each allowed model needs a provider_id and a model_id of the documented grammar")
		}
	}
	return nil
}
