package settings

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"sort"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// Deps is what a Service is built from.
type Deps struct {
	// DB is the store with the "settings" and "audit" migrations applied.
	DB *store.DB
	// Clock is the only source of time. Nil means the wall clock.
	Clock clock.Clock
	// Audit records every change in the same transaction as the change. Required:
	// a change that can not be recorded does not happen.
	Audit audit.Appender
	// Policy is the policy gateway. It decides which capabilities a document may
	// name (only ones the caller may itself use) and which capabilities are
	// enabled for the caller; the service never decides that by itself.
	Policy *policy.Gateway
	// Packages finds the active package of the caller's application. Required: an
	// application whose package is not active serves no settings.
	Packages PackageSource
	// Limits is what the operator approved and capped (the settings section).
	Limits Limits
	// Findings and Providers feed the two read-outs; without one, that read-out is
	// unavailable.
	Findings  AuditSource
	Providers ProviderSource
	// Retention is where the history retention policy of the application and the
	// tenant is stored (internal/history), see retention.go. With it, a tenant or
	// deployment document's retention is delegated to it and not stored here; without
	// it (a test, a standalone embedding) the module keeps the retention in the
	// document and enforces nothing, which `alixai serve` never does.
	Retention RetentionStore
}

// Service is the settings API (master plan 14.2): typed, versioned and
// permission-checked settings at user, tenant and deployment level, the approved
// profiles, the capabilities enabled for the caller, the integration-audit
// findings and the provider health. It is safe for concurrent use.
//
// Every method takes the verified Scope and nothing else about the caller: which
// document is read or written, and what the caller may do to it, comes from that
// scope alone.
type Service struct {
	db        *store.DB
	clk       clock.Clock
	audit     audit.Appender
	policy    *policy.Gateway
	packages  PackageSource
	limits    Limits
	findings  AuditSource
	providers ProviderSource
	retention RetentionStore
}

// New returns a Service. It refuses to build over limits that contradict
// themselves, or without the dependencies that make a change safe.
func New(d Deps) (*Service, error) {
	switch {
	case d.DB == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "settings: the service needs a database")
	case d.Audit == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "settings: the service needs an audit log: a change that can not be recorded does not happen")
	case d.Policy == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "settings: the service needs the policy gateway")
	case d.Packages == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "settings: the service needs the application package source")
	}
	if err := d.Limits.validate(); err != nil {
		return nil, err
	}
	if _, err := loadSchemas(); err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "settings: the schemas can not be compiled")
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	l := d.Limits
	l.ApprovedProfiles = append([]string(nil), l.ApprovedProfiles...)
	l.ApprovedVoices = append([]string(nil), l.ApprovedVoices...)
	return &Service{db: d.DB, clk: clk, audit: d.Audit, policy: d.Policy, packages: d.Packages, limits: l, findings: d.Findings, providers: d.Providers, retention: d.Retention}, nil
}

// Limits returns a copy of the limits the service enforces: what the operator
// approved and capped when it was built.
func (s *Service) Limits() Limits {
	l := s.limits
	l.ApprovedProfiles = append([]string(nil), l.ApprovedProfiles...)
	l.ApprovedVoices = append([]string(nil), l.ApprovedVoices...)
	return l
}

// ---- permission ----

// authorize is the permission check of every operation, in the order that fails
// closed: a valid scope (scope_missing / scope_violation), a known level, then the
// capability scope the token must carry for the level and operation. It decides
// from the token alone; no setting, stored or submitted, can grant anything.
func authorize(scope contracts.Scope, level Level, op Operation) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	need := RequiredCapability(level, op)
	if need == "" {
		return contracts.New(contracts.ErrValidationFailed, "settings: unknown settings level")
	}
	if !scope.Permits(need) {
		return contracts.New(contracts.ErrPermissionDenied, "the context token does not permit this settings operation").
			WithDetails("capability", need)
	}
	return nil
}

// accessOf says what the token permits on each level.
func accessOf(scope contracts.Scope) Access {
	one := func(l Level) LevelAccess {
		return LevelAccess{Read: scope.Permits(RequiredCapability(l, OpRead)), Write: scope.Permits(RequiredCapability(l, OpWrite))}
	}
	return Access{User: one(LevelUser), Tenant: one(LevelTenant), Deployment: one(LevelDeployment)}
}

// activePackage returns the caller's active package, or quarantined: an
// application whose package is not active serves no traffic (SEC-054, SEC-060).
func (s *Service) activePackage(ctx context.Context, scope contracts.Scope) (Package, error) {
	pkg, err := s.packages.Package(ctx, scope.ApplicationID)
	if err != nil {
		var ce *contracts.Error
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "settings: the application package could not be read")
	}
	if pkg == nil || pkg.Status() != policy.StatusActive || pkg.ApplicationID() != scope.ApplicationID {
		return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package: it is quarantined, rejected or not installed")
	}
	return pkg, nil
}

// ---- documents ----

// Get returns the document of one level for the caller's scope: the stored
// document, or version 0 and the empty document when the level has none yet.
func (s *Service) Get(ctx context.Context, scope contracts.Scope, level Level) (Record, error) {
	if err := authorize(scope, level, OpRead); err != nil {
		return Record{}, err
	}
	if _, err := s.activePackage(ctx, scope); err != nil {
		return Record{}, err
	}
	k, err := keyOf(level, scope)
	if err != nil {
		return Record{}, err
	}
	r, found, err := readRow(ctx, s.db, k)
	if err != nil {
		return Record{}, wrapStore(err, "settings: the settings could not be read")
	}
	rec := emptyRecord(level)
	if found {
		rec = r.record(level)
	}
	if s.delegatesRetention(level) {
		cur, err := s.retention.Retention(ctx, scope)
		if err != nil {
			return Record{}, wrapStore(err, "settings: the history retention could not be read")
		}
		if rec.Settings, err = mergeRetention(level, rec.Settings, retentionOf(level, cur)); err != nil {
			return Record{}, err
		}
	}
	return rec, nil
}

// Put replaces the document of one level for the caller's scope.
//
// The document is checked in this order, and the first failure refuses it whole
// with nothing stored and nothing recorded: the token's permission, the active
// package, strict JSON (one object, no duplicate member, bounded), the level's
// schema (closed: a member that is not in it does not exist), then the rules a
// schema can not state (approved profile and voice, supported language,
// the operator's ceilings, and every named capability being one the policy
// gateway lets the caller use). The write itself is optimistic: it succeeds only
// when ExpectedVersion is still the stored version (0 to create), the new
// revision is exactly one more, and the audit event is appended in the same
// transaction. Writing a document identical to the stored one changes nothing,
// records nothing and returns the stored record. With a RetentionStore, the
// history retention of a tenant or deployment document goes to the store, in the
// same transaction and before the document is written, and is not stored in it
// (retention.go).
func (s *Service) Put(ctx context.Context, scope contracts.Scope, level Level, req PutRequest) (Record, error) {
	if err := authorize(scope, level, OpWrite); err != nil {
		return Record{}, err
	}
	pkg, err := s.activePackage(ctx, scope)
	if err != nil {
		return Record{}, err
	}
	if req.ExpectedVersion < 0 {
		return Record{}, contracts.New(contracts.ErrValidationFailed, "settings: expected_version must be 0 (create) or the version that was read")
	}
	canonical, err := s.prepare(ctx, scope, pkg, level, req.Settings)
	if err != nil {
		return Record{}, err
	}
	k, err := keyOf(level, scope)
	if err != nil {
		return Record{}, err
	}
	loc, err := locatorFor(level, scope)
	if err != nil {
		return Record{}, err
	}
	// The retention of a tenant or deployment document is the history store's, not a
	// member this module stores (retention.go). It is written in the document's
	// transaction: a stale write is refused before the store is touched, the store's
	// own refusals come before the document is written, and either both commit or
	// neither does.
	delegated, days := s.delegatesRetention(level), 0
	if delegated {
		if canonical, days, err = splitRetention(level, canonical); err != nil {
			return Record{}, err
		}
	}

	var out Record
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		cur, found, err := readRow(ctx, tx, k)
		if err != nil {
			return err
		}
		current := int64(0)
		if found {
			current = cur.version
		}
		if req.ExpectedVersion != current {
			return contracts.New(contracts.ErrConflict, "the settings changed since they were read; read them again and retry").
				WithDetails("current_version", current)
		}
		if delegated {
			if err := s.applyRetention(ctx, tx, scope, level, days); err != nil {
				return err
			}
		}
		if found && bytes.Equal(cur.document, canonical) {
			out = cur.record(level)
			return nil
		}
		now := s.clk.Now().UTC()
		next := current + 1
		oldDoc := []byte("{}")
		if found {
			oldDoc = cur.document
			res, err := tx.ExecContext(ctx,
				"UPDATE settings_documents SET document = ?, schema_id = ?, version = ?, updated_by = ?, updated_at = ? WHERE "+keyWhere+" AND version = ?",
				append([]any{string(canonical), level.SchemaID(), next, scope.PrincipalID, now.UnixMilli()}, append(k.args(), current)...)...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return contracts.New(contracts.ErrConflict, "the settings changed since they were read; read them again and retry").
					WithDetails("current_version", current)
			}
		} else {
			_, err := tx.ExecContext(ctx,
				"INSERT INTO settings_documents (application_id, tenant_id, workspace_id, level, subject_id, schema_id, version, document, updated_by, created_at, updated_at)"+
					" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				k.application, k.tenant, k.workspace, string(k.level), k.subject, level.SchemaID(), next, string(canonical), scope.PrincipalID, now.UnixMilli(), now.UnixMilli())
			if store.IsUniqueViolation(err) {
				return contracts.New(contracts.ErrConflict, "the settings changed since they were read; read them again and retry")
			}
			if err != nil {
				return err
			}
		}
		if _, err := EventDocumentWritten.AppendTx(ctx, s.audit, tx, loc, DocumentWritten{
			Level: string(level), Version: next, PreviousVersion: current, Schema: level.SchemaID(),
			SHA256: digest(canonical), ChangedSections: changedSections(oldDoc, canonical),
		}); err != nil {
			// Fail closed: a change that can not be recorded does not happen.
			return contracts.Wrap(contracts.ErrUnavailable, err, "the settings change could not be recorded")
		}
		out = Record{Level: level, Version: next, Schema: level.SchemaID(), UpdatedAt: &now, UpdatedBy: visibleUpdater(level, scope.PrincipalID), Settings: canonical}
		return nil
	})
	if err != nil {
		return Record{}, wrapStore(err, "settings: the settings could not be written")
	}
	if delegated {
		if out.Settings, err = mergeRetention(level, out.Settings, days); err != nil {
			return Record{}, err
		}
	}
	return out, nil
}

// prepare turns a submitted document into its canonical bytes, or refuses it.
func (s *Service) prepare(ctx context.Context, scope contracts.Scope, pkg Package, level Level, raw []byte) ([]byte, error) {
	if err := checkStrictJSON(raw); err != nil {
		return nil, err
	}
	if err := validateAgainstSchema(level, raw); err != nil {
		return nil, err
	}
	doc, err := decodeDocument(level, raw)
	if err != nil {
		return nil, refuse("the settings document does not match its schema")
	}
	var unavailable error
	check := semantic{
		limits:    s.limits,
		languages: pkg.SupportedLanguages(),
		mayUse: func(id string) bool {
			d := s.policy.Decide(ctx, policy.Request{
				Scope: scope, Package: pkg, CapabilityID: id, Binding: policy.BindingOf(scope), Phase: policy.PhaseExpose,
			})
			if !d.Allowed && d.Code == contracts.ErrUnavailable {
				unavailable = d.Err()
			}
			return d.Allowed
		},
	}
	if err := check.check(doc); err != nil {
		if unavailable != nil {
			return nil, unavailable
		}
		return nil, err
	}
	return doc.canonical()
}

func locatorFor(level Level, scope contracts.Scope) (audit.Locator, error) {
	if level == LevelDeployment {
		return audit.ForApplication(scope.ApplicationID, scope.PrincipalID)
	}
	return audit.ForScope(scope)
}

// ---- effective settings ----

// loadLayers reads the three documents of the caller in one snapshot.
func (s *Service) loadLayers(ctx context.Context, scope contracts.Scope) (layers, error) {
	ly := layers{versions: map[Level]int64{}}
	err := s.db.View(ctx, func(tx *store.Tx) error {
		ly = layers{versions: map[Level]int64{}}
		for _, level := range Levels() {
			k, err := keyOf(level, scope)
			if err != nil {
				return err
			}
			r, found, err := readRow(ctx, tx, k)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			d, err := decodeDocument(level, r.document)
			if err != nil {
				return contracts.New(contracts.ErrInternal, "settings: a stored document can not be read")
			}
			ly.versions[level] = r.version
			switch level {
			case LevelDeployment:
				ly.deployment = d.policy
			case LevelTenant:
				ly.tenant = d.policy
			case LevelUser:
				ly.user = d.user
			}
		}
		return nil
	})
	if err != nil {
		return layers{}, wrapStore(err, "settings: the settings could not be read")
	}
	if err := s.layerRetention(ctx, scope, &ly); err != nil {
		return layers{}, err
	}
	return ly, nil
}

// Effective returns what the three levels add up to for the caller. It needs the
// permission to read the caller's own settings; it reveals the combined values,
// not the tenant's or the deployment's documents.
func (s *Service) Effective(ctx context.Context, scope contracts.Scope) (gen.EffectiveSettings, error) {
	if err := authorize(scope, LevelUser, OpRead); err != nil {
		return gen.EffectiveSettings{}, err
	}
	if _, err := s.activePackage(ctx, scope); err != nil {
		return gen.EffectiveSettings{}, err
	}
	ly, err := s.loadLayers(ctx, scope)
	if err != nil {
		return gen.EffectiveSettings{}, err
	}
	return computeEffective(s.limits, ly), nil
}

// Profiles returns the approved model and resource profiles and the one the
// caller's effective settings select.
func (s *Service) Profiles(ctx context.Context, scope contracts.Scope) (ProfileList, error) {
	eff, err := s.Effective(ctx, scope)
	if err != nil {
		return ProfileList{}, err
	}
	out := ProfileList{Profiles: make([]Profile, 0, len(s.limits.ApprovedProfiles))}
	for _, id := range s.limits.ApprovedProfiles {
		out.Profiles = append(out.Profiles, Profile{ID: id, Default: id == s.limits.DefaultProfile, Selected: id == string(eff.ModelProfile)})
	}
	sort.Slice(out.Profiles, func(i, j int) bool { return out.Profiles[i].ID < out.Profiles[j].ID })
	return out, nil
}

// ---- read-outs ----

// Capabilities returns the capabilities enabled for the caller: exactly the ones
// the policy gateway exposes to this token (the same exposure the model gets),
// each with what settings do to it. A capability the gateway does not expose is
// not listed, whatever any setting says.
func (s *Service) Capabilities(ctx context.Context, scope contracts.Scope) (CapabilityView, error) {
	if err := authorize(scope, LevelUser, OpRead); err != nil {
		return CapabilityView{}, err
	}
	pkg, err := s.activePackage(ctx, scope)
	if err != nil {
		return CapabilityView{}, err
	}
	ly, err := s.loadLayers(ctx, scope)
	if err != nil {
		return CapabilityView{}, err
	}
	eff := computeEffective(s.limits, ly)
	off := disabledBy(ly)
	exp := s.policy.Expose(ctx, policy.ExposeRequest{Scope: scope, Package: pkg, Binding: policy.BindingOf(scope)})
	out := CapabilityView{Capabilities: make([]CapabilityState, 0, len(exp.Visible)), SettingsAccess: accessOf(scope)}
	for _, dec := range exp.Visible {
		id := dec.Record.CapabilityID
		by := off[id]
		if by == nil {
			by = []Level{}
		}
		extra := !dec.Requires.Confirmation && !dec.Requires.StepUp &&
			eff.Confirmation.ExtraFromRiskLevel != nil && int64(dec.EffectiveRisk) >= int64(*eff.Confirmation.ExtraFromRiskLevel)
		out.Capabilities = append(out.Capabilities, CapabilityState{
			ID: id, RiskLevel: int(dec.EffectiveRisk), Enabled: len(by) == 0, DisabledBy: by,
			RequiresConfirmation: dec.Requires.Confirmation || extra, RequiresStepUp: dec.Requires.StepUp, ExtraConfirmation: extra,
			Fields: append([]string{}, dec.VisibleFields...),
		})
	}
	return out, nil
}

// IntegrationAudit returns the latest integration audit of the caller's
// application with the remediation status of each finding. It needs the
// permission to read the deployment settings, and it is available while the
// application is quarantined, which is when its findings matter most.
func (s *Service) IntegrationAudit(ctx context.Context, scope contracts.Scope) (AuditStatus, error) {
	if err := authorize(scope, LevelDeployment, OpRead); err != nil {
		return AuditStatus{}, err
	}
	if s.findings == nil {
		return AuditStatus{}, errNotConfigured("the integration-audit read-out")
	}
	st, err := s.findings.IntegrationAudit(ctx, scope.ApplicationID)
	if err != nil {
		return AuditStatus{}, wrapReadout(err, "the integration audit could not be read")
	}
	if st.ApplicationID != scope.ApplicationID {
		// Never answer with another application's findings, whatever the source says.
		return AuditStatus{}, contracts.New(contracts.ErrInternal, "settings: the integration-audit source answered for another application")
	}
	if st.Findings == nil {
		st.Findings = []AuditFinding{}
	}
	return st, nil
}

var (
	roleRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,15}$`)
	effectRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
)

// ProviderHealth returns the health of the model roles: role, status and effect,
// nothing about what runs behind them. It needs the permission to read the
// deployment settings.
func (s *Service) ProviderHealth(ctx context.Context, scope contracts.Scope) (ProviderHealth, error) {
	if err := authorize(scope, LevelDeployment, OpRead); err != nil {
		return ProviderHealth{}, err
	}
	if s.providers == nil {
		return ProviderHealth{}, errNotConfigured("the provider health read-out")
	}
	h, err := s.providers.ProviderHealth(ctx)
	if err != nil {
		return ProviderHealth{}, wrapReadout(err, "the provider health could not be read")
	}
	// Whatever the source returns, only closed vocabularies leave: a role name, a
	// status and a stable effect code. A free-text reason, a runtime name or an
	// address has no member to ride in, and the three that remain are checked.
	out := ProviderHealth{Status: cleanStatus(h.Status), Roles: make([]ProviderRole, 0, len(h.Roles))}
	for _, r := range h.Roles {
		role := ProviderRole{Role: "unknown", Configured: r.Configured, Status: cleanStatus(r.Status)}
		if roleRE.MatchString(r.Role) {
			role.Role = r.Role
		}
		if effectRE.MatchString(r.Effect) {
			role.Effect = r.Effect
		}
		out.Roles = append(out.Roles, role)
	}
	sort.Slice(out.Roles, func(i, j int) bool { return out.Roles[i].Role < out.Roles[j].Role })
	return out, nil
}

// cleanStatus maps anything but the three statuses to unavailable: an unknown
// status is never reported as healthy.
func cleanStatus(s string) string {
	switch s {
	case "ok", "degraded", "unavailable":
		return s
	}
	return "unavailable"
}

func wrapReadout(err error, msg string) error {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "settings: "+msg)
}
