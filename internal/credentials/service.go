package credentials

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"time"
	"unicode"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// idPrefix is the prefix of credential reference IDs ("cred_" and a ULID, as the schema says).
const idPrefix = "cred"

var (
	refIDRE      = regexp.MustCompile(`^cred_[0-9A-HJKMNP-TV-Z]{26}$`)
	providerIDRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	scopeRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	accountRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)
	purposeRE    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

const (
	maxScopes       = 32
	maxLabelRunes   = 80
	maxSecretBytes  = 8 << 10
	expiringSoon    = 7 * 24 * time.Hour
	rotationOverdue = 90 * 24 * time.Hour
	// DefaultValidateTimeout bounds the validation request of a manual key.
	DefaultValidateTimeout = 15 * time.Second
)

// KeyValidator makes the minimally scoped provider request that proves a manual key works and
// reports what the provider says about it (additive plan 11.1). The adapter of the provider
// implements it, through the egress policy. The service gives it the key and the provider scopes the
// capability needs, nothing else: no tenant, no application data, no prompt. An error means the
// request could not be made; a refusal by the provider is Accepted=false.
type KeyValidator interface {
	ValidateKey(ctx context.Context, req ValidationRequest) (ValidationResult, error)
}

// ValidationRequest is the whole of what a validator receives.
type ValidationRequest struct {
	ProviderID providers.ProviderID
	Key        Secret
	// Scopes are the provider scopes the selected capability needs; the probe asks for nothing broader.
	Scopes []string
}

// ValidationResult is what the provider said about the key.
type ValidationResult struct {
	// Accepted: the provider accepted the key for the probe.
	Accepted bool
	// Account holds provider-assigned identifiers only; a member that is not shaped like one is dropped.
	Account providers.SafeAccount
	// ExcessivePermissions: the provider reports that the key can do far more than the probe needs.
	ExcessivePermissions bool
	// UnsupportedAccountPolicy: the provider reports an account type or policy that AlixAI does not support.
	UnsupportedAccountPolicy bool
}

// Config wires a Service.
type Config struct {
	// DB holds the references (never a secret). Required.
	DB *store.DB
	// Audit records every use and every change. Required: without it nothing is released.
	Audit audit.Appender
	// DeploymentID is this installation; a scope of another deployment is refused. Required.
	DeploymentID string
	// Backends are the places values can live. At least the Default one.
	Backends []Backend
	// Default is where new manual keys and tokens are stored; it must not be read only. There is no
	// fallback to a plaintext place: with no usable default, adding a credential fails.
	Default BackendName
	// Validator validates a manual key. Without one no manual key is accepted.
	Validator KeyValidator
	// ValidateTimeout bounds one validation; 0 means DefaultValidateTimeout.
	ValidateTimeout time.Duration
	// Clock is the source of time (expiry, timestamps). Nil means the wall clock.
	Clock clock.Clock
	// Logger receives references, reasons and outcomes, never a value. Nil discards.
	Logger *slog.Logger
}

// Service is the local credential service. It is safe for concurrent use. It holds no secret between
// calls: every Resolve reads the backend again, so a rotation or a revocation is effective on the
// next call and no host application restarts.
type Service struct {
	db       *store.DB
	aud      audit.Appender
	dep      string
	backends map[BackendName]Backend
	def      Backend
	val      KeyValidator
	valTO    time.Duration
	clk      clock.Clock
	ids      *ids.Generator
	log      *slog.Logger
}

// New returns a Service. The "credentials" and "audit" migrations must have been applied to the
// database (importing this package registers the first, importing internal/audit the second).
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.Audit == nil || cfg.DeploymentID == "" {
		return nil, fail(contracts.ErrInternal, ReasonBadRequest, "credentials: a store, an audit log and a deployment ID are required")
	}
	s := &Service{
		db: cfg.DB, aud: cfg.Audit, dep: cfg.DeploymentID, backends: map[BackendName]Backend{},
		val: cfg.Validator, valTO: cfg.ValidateTimeout, clk: cfg.Clock, log: cfg.Logger,
	}
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	if s.valTO <= 0 {
		s.valTO = DefaultValidateTimeout
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s.ids = ids.NewGenerator(s.clk)
	for _, b := range cfg.Backends {
		if b == nil {
			continue
		}
		if _, dup := s.backends[b.Name()]; dup {
			return nil, fail(contracts.ErrInternal, ReasonBackendUnknown, "credentials: two backends with one name")
		}
		s.backends[b.Name()] = b
	}
	if cfg.Default != "" {
		d, ok := s.backends[cfg.Default]
		if !ok || d.ReadOnly() {
			return nil, fail(contracts.ErrInternal, ReasonBackendReadOnly, "credentials: the default backend is missing or read only")
		}
		s.def = d
	}
	return s, nil
}

// begin validates the scope and returns where the events of the call go.
func (s *Service) begin(scope contracts.Scope) (audit.Locator, error) {
	if err := scope.Validate(); err != nil {
		return audit.Locator{}, err
	}
	return audit.ForScope(scope)
}

func (s *Service) checkDeployment(scope contracts.Scope) error {
	if scope.DeploymentID != s.dep {
		return deny(ReasonDeploymentMismatch, "credentials: the scope belongs to another deployment")
	}
	return nil
}

func (s *Service) lifecycle(ctx context.Context, loc audit.Locator, e lifecycleAudit) error {
	_, err := s.aud.AppendAt(ctx, loc, EventLifecycle.Event(e))
	return err
}

// ---------------------------------------------------------------------------------------------
// Requests

func validProvider(p providers.ProviderID) error {
	if !providerIDRE.MatchString(string(p)) {
		return fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the provider ID is malformed")
	}
	return nil
}

func validLabel(l string) (*string, error) {
	if l == "" {
		return nil, nil
	}
	n := 0
	for _, r := range l {
		n++
		if unicode.IsControl(r) {
			return nil, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the label has a control character")
		}
	}
	if n > maxLabelRunes {
		return nil, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the label is too long")
	}
	return &l, nil
}

func validScopes(sc []string) ([]string, error) {
	if len(sc) > maxScopes {
		return nil, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: too many scopes")
	}
	out := make([]string, 0, len(sc))
	for _, v := range sc {
		if !scopeRE.MatchString(v) || slices.Contains(out, v) {
			return nil, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: a provider scope is malformed or repeated")
		}
		out = append(out, v)
	}
	return out, nil
}

func validValue(v Secret) error {
	if v.Empty() || len(v) > maxSecretBytes {
		return fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the value is empty or too long")
	}
	for _, r := range v.Reveal() {
		if unicode.IsControl(r) {
			return fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the value has a control character")
		}
	}
	return nil
}

func (s *Service) validExpiry(t *time.Time) (*common.Timestamp, error) {
	if t == nil {
		return nil, nil
	}
	if !t.After(s.clk.Now()) {
		return nil, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the expiry is not in the future")
	}
	return tsPtr(*t), nil
}

// sanitizeAccount keeps only members shaped like provider-assigned identifiers: an e-mail address, a
// name or anything else is dropped, never stored.
func sanitizeAccount(a providers.SafeAccount) providers.SafeAccount {
	keep := func(p *string) *string {
		if p == nil || !accountRE.MatchString(*p) {
			return nil
		}
		v := *p
		return &v
	}
	return providers.SafeAccount{AccountID: keep(a.AccountID), OrganizationID: keep(a.OrganizationID), ProjectID: keep(a.ProjectID)}
}

// validate asks the injected validator, once, for the verdict of the provider on a key. Nothing is
// stored before it says yes. The error of the validator is not passed on: it can echo the key.
func (s *Service) validate(ctx context.Context, provider providers.ProviderID, key Secret, scopes []string) (ValidationResult, error) {
	if s.val == nil {
		return ValidationResult{}, fail(contracts.ErrUnavailable, ReasonValidatorMissing, "credentials: no validator is configured, so a manual key cannot be accepted")
	}
	cctx, cancel := context.WithTimeout(ctx, s.valTO)
	defer cancel()
	res, err := s.val.ValidateKey(cctx, ValidationRequest{ProviderID: provider, Key: key, Scopes: slices.Clone(scopes)})
	if err != nil {
		return ValidationResult{}, fail(contracts.ErrUnavailable, ReasonValidationFailed, "credentials: the key could not be validated")
	}
	if !res.Accepted {
		return ValidationResult{}, fail(contracts.ErrUnauthenticated, ReasonRejected, "credentials: the provider did not accept the key")
	}
	return res, nil
}

// keyWarnings are the warnings of a manual key: always that it may be broader than OAuth scopes, and
// what the provider reported.
func keyWarnings(res ValidationResult) []providers.ConnectionWarning {
	w := []providers.ConnectionWarning{providers.ConnectionWarningBroaderThanOauthScopes}
	if res.ExcessivePermissions {
		w = append(w, providers.ConnectionWarningExcessivePermissionsReported)
	}
	if res.UnsupportedAccountPolicy {
		w = append(w, providers.ConnectionWarningUnsupportedAccountPolicy)
	}
	return w
}

func keyStatus(res ValidationResult) providers.CredentialRefStatus {
	if res.ExcessivePermissions || res.UnsupportedAccountPolicy {
		return providers.CredentialRefStatusQuarantined
	}
	return providers.CredentialRefStatusActive
}

// ---------------------------------------------------------------------------------------------
// Creating

// AddAPIKeyRequest is a manual API key or token entered through the host's authenticated settings
// flow (additive plan 11.1, the fallback when OAuth is unavailable).
type AddAPIKeyRequest struct {
	ProviderID providers.ProviderID
	// Label is shown in settings; "" for none. Never a place for key material.
	Label string
	Key   Secret
	// ExpiresAt is the operator's expiry date for the key; nil for none.
	ExpiresAt *time.Time
	// Scopes are the provider scopes the capability needs (the validation probe asks for no more); may be
	// empty for a key that has no scope concept.
	Scopes []string
}

// AddAPIKey validates a manual key with a minimally scoped provider request, stores it in the default
// backend and returns its reference: label, expiry, a masked fingerprint and the warning that a manual
// key may grant broader access than OAuth scopes. The key is not stored if the provider does not accept
// it. When the provider reports excessive permissions or an unsupported account policy the reference
// is stored quarantined and no value is ever released from it.
func (s *Service) AddAPIKey(ctx context.Context, scope contracts.Scope, req AddAPIKeyRequest) (providers.CredentialRef, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	if s.def == nil {
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonBackendUnknown, "credentials: no writable backend is configured")
	}
	label, scopes, expires, err := s.checkCommon(req.ProviderID, req.Label, req.Scopes, req.ExpiresAt)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if err := validValue(req.Key); err != nil {
		return providers.CredentialRef{}, err
	}
	res, err := s.validate(ctx, req.ProviderID, req.Key, scopes)
	if err != nil {
		_ = s.lifecycle(ctx, loc, lifecycleAudit{Provider: string(req.ProviderID), Action: "rejected", Reason: string(ReasonOf(err))})
		return providers.CredentialRef{}, err
	}
	return s.create(ctx, scope, loc, newRef{
		kind: providers.CredentialKindAPIKey, provider: req.ProviderID, label: label, scopes: scopes, expires: expires,
		status: keyStatus(res), warnings: keyWarnings(res), account: sanitizeAccount(res.Account),
		fingerprint: Fingerprint(req.Key), validated: true,
	}, s.def, "", req.Key, "")
}

func (s *Service) checkCommon(p providers.ProviderID, label string, sc []string, exp *time.Time) (*string, []string, *common.Timestamp, error) {
	if err := validProvider(p); err != nil {
		return nil, nil, nil, err
	}
	l, err := validLabel(label)
	if err != nil {
		return nil, nil, nil, err
	}
	scopes, err := validScopes(sc)
	if err != nil {
		return nil, nil, nil, err
	}
	e, err := s.validExpiry(exp)
	if err != nil {
		return nil, nil, nil, err
	}
	return l, scopes, e, nil
}

// ExternalRefRequest registers a value that is provisioned outside the service, for a headless
// install: an environment variable (BackendEnv, with the DefaultEnvPrefix) or a file in the secret
// directory (BackendFile). The value is read on every use, so replacing it rotates the credential
// without a restart. The value is validated like a manual key when it is registered.
type ExternalRefRequest struct {
	ProviderID providers.ProviderID
	Label      string
	Backend    BackendName
	// Locator is the variable name or the file name.
	Locator   string
	ExpiresAt *time.Time
	Scopes    []string
}

// AddExternalRef registers an environment or file reference. Only an operator's composition root
// calls it: the variable or file can hold a value that no tenant provisioned, so the caller decides
// who may bind it, and every binding is audited.
func (s *Service) AddExternalRef(ctx context.Context, scope contracts.Scope, req ExternalRefRequest) (providers.CredentialRef, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	b, ok := s.backends[req.Backend]
	if !ok || !b.ReadOnly() {
		return providers.CredentialRef{}, fail(contracts.ErrValidationFailed, ReasonBackendUnknown, "credentials: an environment or file backend is required")
	}
	if v, ok := b.(interface{ ValidLocator(string) bool }); !ok || !v.ValidLocator(req.Locator) {
		return providers.CredentialRef{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: the locator is not allowed")
	}
	label, scopes, expires, err := s.checkCommon(req.ProviderID, req.Label, req.Scopes, req.ExpiresAt)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	val, err := b.Get(ctx, Slot{Locator: req.Locator})
	if err != nil {
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonSecretMissing, "credentials: the referenced value cannot be read")
	}
	if err := validValue(val); err != nil {
		return providers.CredentialRef{}, err
	}
	res, err := s.validate(ctx, req.ProviderID, val, scopes)
	if err != nil {
		_ = s.lifecycle(ctx, loc, lifecycleAudit{Provider: string(req.ProviderID), Action: "rejected", Reason: string(ReasonOf(err))})
		return providers.CredentialRef{}, err
	}
	return s.create(ctx, scope, loc, newRef{
		kind: providers.CredentialKindAPIKey, provider: req.ProviderID, label: label, scopes: scopes, expires: expires,
		status: keyStatus(res), warnings: keyWarnings(res), account: sanitizeAccount(res.Account),
		fingerprint: Fingerprint(val), validated: true,
	}, b, req.Locator, "", "")
}

// OAuthGrant is what the OAuth engine (internal/oauth) hands over after a completed authorization.
type OAuthGrant struct {
	ProviderID providers.ProviderID
	Label      string
	// Scopes are the provider scopes consented to: the permitted scopes of the reference. At least one.
	Scopes  []string
	Account providers.SafeAccount
	Access  Secret
	// Refresh is the refresh value, if the provider issued one.
	Refresh Secret
	// ExpiresAt is the expiry of the access value; nil if it does not expire.
	ExpiresAt *time.Time
}

// AddOAuth stores the values of an authorization and returns the reference, active, with the time of
// consent. Tokens are held only here.
func (s *Service) AddOAuth(ctx context.Context, scope contracts.Scope, g OAuthGrant) (providers.CredentialRef, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	if s.def == nil {
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonBackendUnknown, "credentials: no writable backend is configured")
	}
	label, scopes, expires, err := s.checkCommon(g.ProviderID, g.Label, g.Scopes, g.ExpiresAt)
	if err != nil {
		return providers.CredentialRef{}, err
	}
	if len(scopes) == 0 {
		return providers.CredentialRef{}, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: an OAuth credential has at least one scope")
	}
	if err := validValue(g.Access); err != nil {
		return providers.CredentialRef{}, err
	}
	if !g.Refresh.Empty() {
		if err := validValue(g.Refresh); err != nil {
			return providers.CredentialRef{}, err
		}
	}
	return s.create(ctx, scope, loc, newRef{
		kind: providers.CredentialKindOauth, provider: g.ProviderID, label: label, scopes: scopes, expires: expires,
		status: providers.CredentialRefStatusActive, account: sanitizeAccount(g.Account),
	}, s.def, "", g.Access, g.Refresh)
}

// newRef is the typed part of a reference being created.
type newRef struct {
	kind        providers.CredentialKind
	provider    providers.ProviderID
	label       *string
	scopes      []string
	expires     *common.Timestamp
	status      providers.CredentialRefStatus
	warnings    []providers.ConnectionWarning
	account     providers.SafeAccount
	fingerprint string
	validated   bool
}

func (s *Service) create(ctx context.Context, scope contracts.Scope, loc audit.Locator, n newRef, b Backend, locator string, key, refresh Secret) (providers.CredentialRef, error) {
	now := s.clk.Now()
	ref := providers.CredentialRef{
		RefID: providers.CredentialRefID(s.ids.New(idPrefix)),
		Kind:  n.kind,
		Label: n.label,
		Binding: providers.CredentialRefBinding{
			ProviderID:    n.provider,
			DeploymentID:  common.Identifier(scope.DeploymentID),
			ApplicationID: common.Identifier(scope.ApplicationID),
			TenantID:      common.Identifier(scope.TenantID),
			Scopes:        n.scopes,
		},
		Status:    n.status,
		Account:   n.account,
		Warnings:  n.warnings,
		CreatedAt: ts(now),
		UpdatedAt: ts(now),
		Times: providers.CredentialRefTimes{
			AuthorizedAt: tsPtr(now),
			ExpiresAt:    n.expires,
		},
	}
	if n.fingerprint != "" {
		fp := n.fingerprint
		ref.Fingerprint = &fp
	}
	if n.validated {
		ref.Times.LastValidatedAt = tsPtr(now)
	}
	r := row{ref: ref, backend: b.Name(), locator: locator, hasRefresh: !refresh.Empty()}
	slot := r.slot()
	if !b.ReadOnly() {
		if err := b.Put(ctx, slot, key); err != nil {
			return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonBackendFailed, "credentials: the value could not be stored")
		}
		if !refresh.Empty() {
			if err := b.Put(ctx, slot.refresh(), refresh); err != nil {
				_ = b.Delete(ctx, slot)
				return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonBackendFailed, "credentials: the value could not be stored")
			}
		}
	}
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		if err := insertRow(ctx, tx, r, now); err != nil {
			return err
		}
		_, err := s.aud.AppendTx(ctx, tx, loc, EventLifecycle.Event(lifecycleAudit{
			Ref: string(ref.RefID), Provider: string(n.provider), Action: "created", To: string(ref.Status)}))
		return err
	})
	if err != nil {
		if !b.ReadOnly() {
			_ = b.Delete(ctx, slot)
			_ = b.Delete(ctx, slot.refresh())
		}
		return providers.CredentialRef{}, fail(contracts.ErrUnavailable, ReasonAuditFailed, "credentials: the reference could not be recorded")
	}
	return s.view(r), nil
}

// ---------------------------------------------------------------------------------------------
// Reading metadata

// view is the reference as the host may see it: the status takes the clock into account (an active
// credential past its expiry is expired), and the warnings that depend on time are added. It never
// carries more than the typed reference does, which has no member that can hold a secret.
func (s *Service) view(r row) providers.CredentialRef {
	ref := r.ref
	ref.Warnings = slices.Clone(ref.Warnings)
	ref.Binding.Scopes = slices.Clone(ref.Binding.Scopes)
	now := s.clk.Now()
	if ref.Status == providers.CredentialRefStatusActive {
		if exp, ok := parseTS(ref.Times.ExpiresAt); ok {
			switch {
			case !now.Before(exp):
				ref.Status = providers.CredentialRefStatusExpired
			case exp.Sub(now) <= expiringSoon && !slices.Contains(ref.Warnings, providers.ConnectionWarningExpiringSoon):
				ref.Warnings = append(ref.Warnings, providers.ConnectionWarningExpiringSoon)
			}
		}
		if ref.Kind == providers.CredentialKindAPIKey {
			since, ok := parseTS(ref.Times.RotatedAt)
			if !ok {
				since, ok = parseTS(ref.Times.AuthorizedAt)
			}
			if ok && now.Sub(since) >= rotationOverdue && !slices.Contains(ref.Warnings, providers.ConnectionWarningRotationOverdue) {
				ref.Warnings = append(ref.Warnings, providers.ConnectionWarningRotationOverdue)
			}
		}
	}
	if ref.Warnings == nil {
		ref.Warnings = []providers.ConnectionWarning{}
	}
	return ref
}

func (s *Service) find(ctx context.Context, scope contracts.Scope, ref string) (row, error) {
	if !refIDRE.MatchString(ref) {
		return row{}, deny(ReasonNotBound, "credentials: no such reference in this application and tenant")
	}
	r, found, err := loadRow(ctx, s.db, scope.ApplicationID, scope.TenantID, ref)
	if err != nil {
		return row{}, err
	}
	if !found {
		// One answer for "does not exist" and "belongs to another tenant": existence does not leak.
		return row{}, deny(ReasonNotBound, "credentials: no such reference in this application and tenant")
	}
	return r, nil
}

// Get returns the reference of the scope's tenant: a status, a masked fingerprint and a safe account
// identifier, never a value.
func (s *Service) Get(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID) (providers.CredentialRef, error) {
	if _, err := s.begin(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return providers.CredentialRef{}, err
	}
	r, err := s.find(ctx, scope, string(ref))
	if err != nil {
		return providers.CredentialRef{}, err
	}
	return s.view(r), nil
}

// List returns the references of the scope's tenant, oldest first; provider "" means every provider.
func (s *Service) List(ctx context.Context, scope contracts.Scope, provider providers.ProviderID) ([]providers.CredentialRef, error) {
	if _, err := s.begin(scope); err != nil {
		return nil, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return nil, err
	}
	rows, err := listRows(ctx, s.db, scope.ApplicationID, scope.TenantID, provider)
	if err != nil {
		return nil, err
	}
	out := make([]providers.CredentialRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.view(r))
	}
	return out, nil
}

// CredentialStatus implements the status source of internal/providerreg: the status of a reference of
// the scope's tenant. An error, or any status but active, means the credential is not usable, and the
// registry then never selects another provider in its place.
func (s *Service) CredentialStatus(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID) (providers.CredentialRefStatus, error) {
	ref2, err := s.Get(ctx, scope, ref)
	if err != nil {
		return "", err
	}
	return ref2.Status, nil
}

// ---------------------------------------------------------------------------------------------
// Using

// Use says what a credential is wanted for. Every member is checked against the binding of the
// reference.
type Use struct {
	// Provider is the provider the caller is about to call. Required: a credential is bound to one
	// provider and never serves another.
	Provider providers.ProviderID
	// Scopes are the provider scopes the call needs; each must be among the permitted scopes.
	Scopes []string
	// Purpose is a short code for the audit trail ("inference", "rag.embed", "validation").
	Purpose string
	// Refresh asks for the refresh value instead of the access value: for the OAuth engine only.
	Refresh bool
}

func (u Use) part() string {
	if u.Refresh {
		return "refresh"
	}
	return "access"
}

// Resolve turns an opaque reference into the secret, for the duration of the call: the caller holds it
// only as long as it needs to put it on the wire. It fails closed. A reference that is not the scope's
// (another tenant's, another application's, or none) and one for another deployment or provider, a
// scope that is not permitted, a revoked, expired, invalid, quarantined or not-yet-authorized
// credential, a backend that cannot produce the value and an audit log that cannot record the use all
// return a typed error and no value. Nothing here ever looks at another reference to take its place:
// there is no hidden alternate credential and no alternate provider.
func (s *Service) Resolve(ctx context.Context, scope contracts.Scope, ref string, use Use) (Secret, error) {
	loc, err := s.begin(scope)
	if err != nil {
		return "", err
	}
	// A caller that passes a key where a reference belongs must not get it into the audit trail or the log:
	// only a well-formed opaque reference is ever recorded.
	logged := ""
	if refIDRE.MatchString(ref) {
		logged = ref
	}
	rec := useAudit{Ref: logged, Provider: safeCode(string(use.Provider), providerIDRE), Purpose: safeCode(use.Purpose, purposeRE), Part: use.part()}
	refuse := func(err error) (Secret, error) {
		rec.Outcome, rec.Reason = "denied", string(ReasonOf(err))
		if _, aerr := s.aud.AppendAt(ctx, loc, EventUse.Event(rec)); aerr != nil {
			s.log.Warn("credential use denial was not recorded", "ref", logged, "reason", rec.Reason)
		}
		return "", err
	}
	if err := s.checkDeployment(scope); err != nil {
		return refuse(err)
	}
	if err := validProvider(use.Provider); err != nil || !purposeRE.MatchString(use.Purpose) {
		return refuse(fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: a provider and a purpose are required"))
	}
	r, err := s.find(ctx, scope, ref)
	if err != nil {
		return refuse(err)
	}
	rec.Backend = string(r.backend)
	if string(r.ref.Binding.DeploymentID) != s.dep {
		return refuse(deny(ReasonDeploymentMismatch, "credentials: the reference belongs to another deployment"))
	}
	if r.ref.Binding.ProviderID != use.Provider {
		return refuse(deny(ReasonProviderMismatch, "credentials: the reference is for another provider"))
	}
	for _, sc := range use.Scopes {
		if !slices.Contains(r.ref.Binding.Scopes, sc) {
			return refuse(deny(ReasonScopeNotPermitted, "credentials: a requested scope is not permitted for this reference"))
		}
	}
	// An expired access value is exactly when the OAuth engine needs the refresh value: only that
	// use passes an expired, unrevoked reference. Every other status still refuses.
	if st := s.view(r).Status; !(use.Refresh && r.hasRefresh && st == providers.CredentialRefStatusExpired) {
		if err := statusError(st); err != nil {
			return refuse(err)
		}
	}
	if use.Refresh && !r.hasRefresh {
		return refuse(fail(contracts.ErrValidationFailed, ReasonNoRefresh, "credentials: the reference has no refresh value"))
	}
	b, ok := s.backends[r.backend]
	if !ok {
		return refuse(fail(contracts.ErrUnavailable, ReasonBackendUnknown, "credentials: the backend of the reference is not configured"))
	}
	slot := r.slot()
	if use.Refresh {
		slot = slot.refresh()
	}
	v, err := b.Get(ctx, slot)
	switch {
	case err == nil:
	case isNotFound(err):
		return refuse(fail(contracts.ErrUnavailable, ReasonSecretMissing, "credentials: the backend holds no value for the reference"))
	default:
		return refuse(fail(contracts.ErrUnavailable, ReasonBackendFailed, "credentials: the value could not be read"))
	}
	rec.Outcome = "allowed"
	if _, err := s.aud.AppendAt(ctx, loc, EventUse.Event(rec)); err != nil {
		// A use that cannot be recorded does not happen (SEC-087).
		return "", fail(contracts.ErrUnavailable, ReasonAuditFailed, "credentials: the use could not be recorded")
	}
	return v, nil
}

// safeCode returns v if it has the shape of a code, else "": free text a caller passed is never recorded.
func safeCode(v string, re *regexp.Regexp) string {
	if re.MatchString(v) {
		return v
	}
	return ""
}

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// statusError maps a status that is not active to the typed refusal.
func statusError(st providers.CredentialRefStatus) error {
	switch st {
	case providers.CredentialRefStatusActive:
		return nil
	case providers.CredentialRefStatusExpired:
		return fail(contracts.ErrExpired, ReasonExpired, "credentials: the credential has expired")
	case providers.CredentialRefStatusQuarantined:
		return fail(contracts.ErrQuarantined, ReasonQuarantined, "credentials: the credential is quarantined")
	case providers.CredentialRefStatusRevoked:
		return fail(contracts.ErrUnauthenticated, ReasonRevoked, "credentials: the credential has been revoked")
	case providers.CredentialRefStatusInvalid:
		return fail(contracts.ErrUnauthenticated, ReasonInvalid, "credentials: the provider rejected the credential")
	case providers.CredentialRefStatusReauthorizationRequired:
		return fail(contracts.ErrUnauthenticated, ReasonReauthRequired, "credentials: the credential needs a new authorization")
	default:
		return fail(contracts.ErrUnauthenticated, ReasonPending, "credentials: the credential is not usable yet")
	}
}
