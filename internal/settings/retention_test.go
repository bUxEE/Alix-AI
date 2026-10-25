package settings

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// retentionHistoryStore is RetentionStore over the real history service, the way
// the composition root of `alixai serve` builds it (internal/cli/cmd_history_retention.go):
// settings has authorised the write, so history is not asked for its own capability, and
// the application-wide policy is an operator action satisfied by settings.deployment.write.
type retentionHistoryStore struct{ svc *history.Service }

func (a retentionHistoryStore) scope(sc contracts.Scope) contracts.Scope {
	sc = sc.Clone()
	sc.CapabilityScopes = append(sc.CapabilityScopes, history.CapRetention)
	return sc
}

func (a retentionHistoryStore) Retention(ctx context.Context, sc contracts.Scope) (Retention, error) {
	p, err := a.svc.RetentionPolicy(ctx, a.scope(sc))
	if err != nil {
		return Retention{}, err
	}
	out := Retention{EffectiveDays: p.EffectiveDays, Source: p.Source}
	if p.Application != nil {
		out.Application = p.Application.Days
	}
	if p.Tenant != nil {
		out.Tenant = p.Tenant.Days
	}
	return out, nil
}

func (a retentionHistoryStore) SetRetention(ctx context.Context, tx *store.Tx, sc contracts.Scope, level Level, days int) (err error) {
	if level == LevelTenant {
		_, err = a.svc.SetRetentionTx(ctx, tx, a.scope(sc), days)
	} else {
		_, err = a.svc.SetApplicationRetentionTx(ctx, tx, sc, days)
	}
	return err
}

// retentionFixture is a settings service whose retention is the real history service over
// the same store and audit log.
func retentionFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, func(d *Deps) {
		sess, err := sessions.New(d.DB, d.Clock, sessions.Config{})
		if err != nil {
			t.Fatal(err)
		}
		svc, err := history.New(history.Deps{
			DB: d.DB, Transcripts: sess, Audit: d.Audit, Clock: d.Clock,
			Operator: func(_ context.Context, sc contracts.Scope, action history.Action) error {
				if action != history.ActionApplicationRetention || !sc.Permits(CapabilityDeploymentWrite) {
					return contracts.New(contracts.ErrPermissionDenied, "not an operator")
				}
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		d.Retention = retentionHistoryStore{svc: svc}
	})
}

func (f *fixture) policyRows(application, tenant string) (n, days int) {
	f.t.Helper()
	err := f.db.QueryRowContext(context.Background(),
		"SELECT count(*), coalesce(max(retain_days), 0) FROM history_retention WHERE application_id = ? AND tenant_id = ?", application, tenant).Scan(&n, &days)
	if err != nil {
		f.t.Fatal(err)
	}
	return n, days
}

func (f *fixture) storedDocument(level Level) string {
	f.t.Helper()
	var doc string
	if err := f.db.QueryRowContext(context.Background(), "SELECT document FROM settings_documents WHERE level = ?", string(level)).Scan(&doc); err != nil {
		f.t.Fatal(err)
	}
	return doc
}

func (f *fixture) retentionOf(sc contracts.Scope) (int, gen.DecidedBy) {
	f.t.Helper()
	eff, err := f.svc.Effective(context.Background(), sc)
	if err != nil {
		f.t.Fatal(err)
	}
	return int(eff.History.RetentionDays), eff.DecidedBy["history.retention_days"]
}

func retentionDaysOf(t *testing.T, rec Record) int {
	t.Helper()
	var d gen.TenantSettings
	if err := json.Unmarshal(rec.Settings, &d); err != nil {
		t.Fatal(err)
	}
	if d.History == nil || d.History.RetentionDays == nil {
		return 0
	}
	return int(*d.History.RetentionDays)
}

// The retention a tenant or deployment document states is history's: one stored policy, written by
// history.Service in its own transaction with the settings.changed event, not a second copy in the
// settings document. The document reads back with it, and the effective retention takes history's
// shorter-of figure instead of applying a rule of its own.
func TestRetentionPreferenceDelegatesToHistoryAndIsStoredOnce(t *testing.T) {
	f := retentionFixture(t)
	sc := ada()
	changed := func() int { return len(f.eventsOfType(string(audit.EventSettingsChanged))) }

	// Tenant level: the policy is history's, the document keeps the rest.
	rec := f.put(sc, LevelTenant, 0, `{"history":{"retention_days":30,"export_allowed":true}}`)
	if rec.Version != 1 || retentionDaysOf(t, rec) != 30 {
		t.Fatalf("Put returned version %d, retention %d; want 1 and 30", rec.Version, retentionDaysOf(t, rec))
	}
	if doc := f.storedDocument(LevelTenant); containsAll(doc, "retention_days") || !containsAll(doc, `"export_allowed":true`) {
		t.Errorf("the settings document keeps its own copy of the retention: %s", doc)
	}
	if n, days := f.policyRows(tApp, tTenantA); n != 1 || days != 30 {
		t.Fatalf("history holds %d policies of %d days, want exactly one of 30", n, days)
	}
	if got := retentionDaysOf(t, f.get(sc, LevelTenant)); got != 30 {
		t.Errorf("Get shows a retention of %d, want history's 30", got)
	}
	if days, by := f.retentionOf(sc); days != 30 || by != gen.DecidedByTenant {
		t.Errorf("effective retention = %d by %s, want 30 by tenant", days, by)
	}
	if changed() != 1 || len(f.settingsEvents()) != 1 {
		t.Errorf("events: %d settings.changed and %d settings.document_written, want one each", changed(), len(f.settingsEvents()))
	}
	var ev struct{ Setting, Level string }
	if err := json.Unmarshal(f.eventsOfType(string(audit.EventSettingsChanged))[0].Data, &ev); err != nil || ev.Setting != "history.retention" || ev.Level != "tenant" {
		t.Errorf("the retention audit event is %s (%v)", f.eventsOfType(string(audit.EventSettingsChanged))[0].Data, err)
	}

	// Writing back what was read changes nothing.
	again := f.put(sc, LevelTenant, 1, string(f.get(sc, LevelTenant).Settings))
	if again.Version != 1 || changed() != 1 || len(f.settingsEvents()) != 1 {
		t.Errorf("an unchanged document wrote: version %d, %d settings.changed events", again.Version, changed())
	}

	// A change of the retention alone is history's: one policy, one more event, the document stays at its version.
	rec = f.put(sc, LevelTenant, 1, `{"history":{"retention_days":45,"export_allowed":true}}`)
	if n, days := f.policyRows(tApp, tTenantA); n != 1 || days != 45 || rec.Version != 1 || retentionDaysOf(t, rec) != 45 {
		t.Errorf("after 45: %d policies of %d days, version %d, document retention %d", n, days, rec.Version, retentionDaysOf(t, rec))
	}
	if changed() != 2 {
		t.Errorf("%d settings.changed events, want 2", changed())
	}

	// The deployment level is the application-wide policy; the shorter of it and the tenant's applies, once.
	f.put(sc, LevelDeployment, 0, `{"history":{"retention_days":60}}`)
	if n, days := f.policyRows(tApp, "*"); n != 1 || days != 60 {
		t.Fatalf("the application-wide policy is %d rows of %d days, want one of 60", n, days)
	}
	if days, by := f.retentionOf(sc); days != 45 || by != gen.DecidedByTenant {
		t.Errorf("tenant 45, application 60: effective %d by %s, want 45 by tenant", days, by)
	}
	f.put(sc, LevelDeployment, 1, `{"history":{"retention_days":20}}`)
	if days, by := f.retentionOf(sc); days != 20 || by != gen.DecidedByDeployment {
		t.Errorf("tenant 45, application 20: effective %d by %s, want 20 by deployment", days, by)
	}
	if got := retentionDaysOf(t, f.get(sc, LevelDeployment)); got != 20 {
		t.Errorf("the deployment document reads back %d days, want 20", got)
	}

	// The user level has no history equivalent: it stays a stored preference that narrows the figure.
	f.put(sc, LevelUser, 0, `{"history":{"retention_days":10}}`)
	if days, by := f.retentionOf(sc); days != 10 || by != gen.DecidedByUser {
		t.Errorf("a user preference of 10: effective %d by %s, want 10 by user", days, by)
	}
	if n, days := f.policyRows(tApp, tTenantA); n != 1 || days != 45 {
		t.Errorf("a user preference reached history: %d policies of %d days", n, days)
	}
	if doc := f.storedDocument(LevelUser); !containsAll(doc, `"retention_days":10`) {
		t.Errorf("the user's preference is not stored: %s", doc)
	}

	// Leaving the retention out of a replaced document removes the policy, like any other member.
	f.put(sc, LevelTenant, 1, `{"history":{"export_allowed":true}}`)
	if n, _ := f.policyRows(tApp, tTenantA); n != 0 {
		t.Errorf("history still holds %d tenant policies", n)
	}
	if got := retentionDaysOf(t, f.get(sc, LevelTenant)); got != 0 {
		t.Errorf("Get shows a retention of %d after it was removed", got)
	}
}

// A tenant may keep history for no longer than its application allows: the refusal is history's,
// it carries the limit, and nothing is stored - no policy, no document, no event.
func TestTenantCannotLengthenApplicationRetention(t *testing.T) {
	f := retentionFixture(t)
	sc := ada()
	f.put(sc, LevelDeployment, 0, `{"history":{"retention_days":30}}`)
	before := len(f.eventsOfType(string(audit.EventSettingsChanged)))

	err := f.tryPut(sc, LevelTenant, 0, `{"default_language":"it","history":{"retention_days":60}}`)
	wantCode(t, err, contracts.ErrValidationFailed)
	var ce *contracts.Error
	if !asErr(err, &ce) || ce.Details["max_days"] != 30 {
		t.Errorf("the refusal does not carry the application's limit: %v", err)
	}
	if n, _ := f.policyRows(tApp, tTenantA); n != 0 {
		t.Errorf("a refused retention was stored: %d tenant policies", n)
	}
	if rec := f.get(sc, LevelTenant); rec.Version != 0 || retentionDaysOf(t, rec) != 0 {
		t.Errorf("a refused document was stored: %+v", rec)
	}
	if got := len(f.eventsOfType(string(audit.EventSettingsChanged))); got != before || len(f.eventsOfType(string(EventDocumentWritten.Name()))) != 1 {
		t.Errorf("a refused write left events behind (%d settings.changed, was %d)", got, before)
	}

	// The limit itself and anything shorter are fine, and the application can shorten under a tenant.
	f.put(sc, LevelTenant, 0, `{"history":{"retention_days":30}}`)
	f.put(sc, LevelTenant, 1, `{"history":{"retention_days":14}}`)
	f.put(sc, LevelDeployment, 1, `{"history":{"retention_days":7}}`)
	if days, by := f.retentionOf(sc); days != 7 || by != gen.DecidedByDeployment {
		t.Errorf("tenant 14, application 7: effective %d by %s, want 7 by deployment", days, by)
	}
}

// A stale write or a token without the level's write scope never reaches history.
func TestRetentionIsUntouchedByRefusedWrites(t *testing.T) {
	f := retentionFixture(t)
	sc := ada()
	f.put(sc, LevelTenant, 0, `{"history":{"retention_days":30}}`)

	wantCode(t, f.tryPut(sc, LevelTenant, 0, `{"history":{"retention_days":5}}`), contracts.ErrConflict)
	readOnly := withScopes(sc, CapabilityTenantRead, CapabilityDeploymentRead, CapabilityDeploymentWrite)
	wantCode(t, f.tryPut(readOnly, LevelTenant, 1, `{"history":{"retention_days":5}}`), contracts.ErrPermissionDenied)
	tenantOnly := withScopes(sc, CapabilityTenantRead, CapabilityTenantWrite)
	wantCode(t, f.tryPut(tenantOnly, LevelDeployment, 0, `{"history":{"retention_days":5}}`), contracts.ErrPermissionDenied)
	if n, days := f.policyRows(tApp, tTenantA); n != 1 || days != 30 {
		t.Errorf("a refused write changed the tenant's policy: %d rows of %d days", n, days)
	}
	if n, _ := f.policyRows(tApp, "*"); n != 0 {
		t.Errorf("a refused write set the application-wide policy")
	}
}

// The retention a document states and the document itself are one transaction: a Put whose
// document cannot be recorded leaves no policy and no settings.changed event behind, and the same
// Put goes through, both together, once it can be.
func TestRetentionCommitsWithTheDocumentOrNotAtAll(t *testing.T) {
	f := retentionFixture(t)
	sc := ada()
	f.rec.fail, f.rec.failType = errors.New("audit log unavailable"), string(EventDocumentWritten.Name())
	wantCode(t, f.tryPut(sc, LevelTenant, 0, `{"default_language":"it","history":{"retention_days":30}}`), contracts.ErrUnavailable)
	if n, days := f.policyRows(tApp, tTenantA); n != 0 {
		t.Errorf("a Put that was not recorded left a policy of %d days", days)
	}
	if got := len(f.eventsOfType(string(audit.EventSettingsChanged))); got != 0 {
		t.Errorf("a Put that was not recorded left %d settings.changed events", got)
	}
	if rec := f.get(sc, LevelTenant); rec.Version != 0 || retentionDaysOf(t, rec) != 0 {
		t.Errorf("a Put that was not recorded left %+v", rec)
	}

	f.rec.fail, f.rec.failType = nil, ""
	f.put(sc, LevelTenant, 0, `{"default_language":"it","history":{"retention_days":30}}`)
	if n, days := f.policyRows(tApp, tTenantA); n != 1 || days != 30 {
		t.Errorf("after the retry: %d policies of %d days, want one of 30", n, days)
	}
	// The event the refused Put appended went with it: the chain is intact.
	rep, err := f.log.VerifyChain(context.Background(), audit.ChainKey{ApplicationID: tApp, TenantID: tTenantA}, audit.VerifyOptions{})
	if err != nil || !rep.OK() || rep.Events != 2 {
		t.Errorf("the tenant chain after a refused Put and its retry: %+v, %v; want intact with 2 events", rep, err)
	}
}

// Without a RetentionStore the module behaves as it did: the retention stays in the document.
func TestRetentionStaysInTheDocumentWithoutAStore(t *testing.T) {
	f := newFixture(t)
	sc := ada()
	f.put(sc, LevelTenant, 0, `{"history":{"retention_days":30}}`)
	if doc := f.storedDocument(LevelTenant); !containsAll(doc, `"retention_days":30`) {
		t.Errorf("document = %s", doc)
	}
	if days, by := f.retentionOf(sc); days != 30 || by != gen.DecidedByTenant {
		t.Errorf("effective %d by %s", days, by)
	}
}
