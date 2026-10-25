package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestSettingsTableIsScoped: the module's table follows the store's scope rule, the
// coarser levels leave the finer columns empty and the table refuses any other
// shape, so a row can not be half scoped.
func TestSettingsTableIsScoped(t *testing.T) {
	f := newFixture(t)
	// Only this module's table is judged here: the schema also holds the tables of
	// every module the test binary links (audit, trust, ...), which are theirs to assert.
	violations, err := store.CheckScopeColumns(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		if v.Table == "settings_documents" {
			t.Errorf("settings_documents breaks the scope rule: %s", v.Problem)
		}
	}

	exec := func(level, app, tenant, workspace, subject string) error {
		_, err := f.db.ExecContext(context.Background(),
			"INSERT INTO settings_documents (application_id, tenant_id, workspace_id, level, subject_id, schema_id, version, document, updated_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'urn:x', 1, '{}', 'u', 0, 0)",
			app, tenant, workspace, level, subject)
		return err
	}
	for name, tc := range map[string]struct{ level, app, tenant, workspace, subject string }{
		"a tenant row with a workspace":      {"tenant", "a", "t", "w", ""},
		"a tenant row with a subject":        {"tenant", "a", "t", "", "u"},
		"a tenant row without a tenant":      {"tenant", "a", "", "", ""},
		"a deployment row with a tenant":     {"deployment", "a", "t", "", ""},
		"a deployment row with a subject":    {"deployment", "a", "", "", "u"},
		"a user row without a workspace":     {"user", "a", "t", "", "u"},
		"a user row without a tenant":        {"user", "a", "", "w", "u"},
		"a user row without a principal":     {"user", "a", "t", "w", ""},
		"a row without an application":       {"deployment", "", "", "", ""},
		"a level that does not exist":        {"planet", "a", "", "", ""},
		"an application-wide user row":       {"user", "a", "", "", ""},
		"a user row with every column empty": {"user", "", "", "", ""},
	} {
		err := exec(tc.level, tc.app, tc.tenant, tc.workspace, tc.subject)
		if err == nil {
			t.Errorf("%s: the table accepted it", name)
		} else if !store.IsConstraint(err) {
			t.Errorf("%s: %v is not a constraint failure", name, err)
		}
	}
	for name, tc := range map[string]struct{ level, app, tenant, workspace, subject string }{
		"deployment": {"deployment", "a", "", "", ""},
		"tenant":     {"tenant", "a", "t", "", ""},
		"user":       {"user", "a", "t", "w", "u"},
	} {
		if err := exec(tc.level, tc.app, tc.tenant, tc.workspace, tc.subject); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		// A second row for the same key is refused by the unique index.
		if err := exec(tc.level, tc.app, tc.tenant, tc.workspace, tc.subject); !store.IsUniqueViolation(err) {
			t.Errorf("%s: a duplicate key gave %v", name, err)
		}
	}
	// A document that is not a JSON object is refused by the table itself.
	_, err = f.db.ExecContext(context.Background(),
		"INSERT INTO settings_documents (application_id, tenant_id, workspace_id, level, subject_id, schema_id, version, document, updated_by, created_at, updated_at) VALUES ('b', '', '', 'deployment', '', 'urn:x', 1, '[1]', 'u', 0, 0)")
	if !store.IsConstraint(err) {
		t.Errorf("an array document: %v", err)
	}
}

// ADR-0016: deployment-level settings are per application. A write for one
// application is invisible to another, even in the same tenant id, and the tenant
// level is keyed by (application, tenant).
func TestSettingsDeploymentLevelIsPerApplication(t *testing.T) {
	f := newFixture(t)
	a := ada()
	b := scopeOf(tOther, tTenantA, tWsp1, tUser1, ada().CapabilityScopes...) // same tenant id and principal id, another application
	f.put(a, LevelDeployment, 0, `{"history":{"retention_days":10}}`)
	f.put(a, LevelTenant, 0, `{"history":{"retention_days":20}}`)
	f.put(a, LevelUser, 0, `{"history":{"retention_days":30}}`)
	for _, level := range Levels() {
		if got := f.get(b, level); got.Version != 0 || string(got.Settings) != "{}" {
			t.Errorf("application B reads application A's %s document: %+v", level, got)
		}
	}
	effB, err := f.svc.Effective(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if effB.Levels.Deployment+effB.Levels.Tenant+effB.Levels.User != 0 || effB.History.RetentionDays != 90 {
		t.Errorf("application B's effective settings were shaped by application A: %+v", effB)
	}
	// Writing B's deployment document does not disturb A's, and both start at version 1.
	if rec := f.put(b, LevelDeployment, 0, `{"history":{"retention_days":40}}`); rec.Version != 1 {
		t.Errorf("B's first write is version %d", rec.Version)
	}
	if got := f.get(a, LevelDeployment); got.Version != 1 || !strings.Contains(string(got.Settings), `"retention_days":10`) {
		t.Errorf("A's deployment document = %+v", got)
	}
	// Workspaces and principals partition the user level.
	w2 := scopeOf(tApp, tTenantA, tWsp2, tUser1, ada().CapabilityScopes...)
	bob := scopeOf(tApp, tTenantA, tWsp1, tUser2, ada().CapabilityScopes...)
	for name, who := range map[string]contracts.Scope{"another workspace": w2, "another principal": bob} {
		if got := f.get(who, LevelUser); got.Version != 0 {
			t.Errorf("%s reads ada's user document: %+v", name, got)
		}
		if got := f.get(who, LevelTenant); got.Version != 1 {
			t.Errorf("%s does not share the tenant document: %+v", name, got)
		}
	}
	// Every row carries only what its level addresses.
	rows, err := f.db.QueryContext(context.Background(), "SELECT application_id, tenant_id, workspace_id, level, subject_id FROM settings_documents ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var a, tn, w, l, s string
		if err := rows.Scan(&a, &tn, &w, &l, &s); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join([]string{a, tn, w, l, s}, "/"))
	}
	want := []string{"app_demo///deployment/", "app_demo/ten_acme//tenant/", "app_demo/ten_acme/wsp_main/user/usr_ada", "app_other///deployment/"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// TestSettingsChangeIsAudited: a change is a registered, typed audit event
// appended in the same transaction, on the chain its level belongs to, carrying
// the revision, the digest and the changed sections but never a value.
func TestSettingsChangeIsAudited(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sc := ada()

	t.Run("the event type is registered once, typed and distinct from the core one", func(t *testing.T) {
		spec, ok := audit.DefaultRegistry.Lookup("settings.document_written")
		if !ok {
			t.Fatal("settings.document_written is not registered")
		}
		if spec.Levels != audit.LevelTenant|audit.LevelApplication {
			t.Errorf("levels = %v", spec.Levels)
		}
		if EventDocumentWritten.Name() != "settings.document_written" {
			t.Errorf("name = %s", EventDocumentWritten.Name())
		}
		if _, ok := audit.DefaultRegistry.Lookup(audit.EventSettingsChanged); !ok {
			t.Error("the core settings.changed type is gone")
		}
	})

	t.Run("a user change goes to the tenant chain, attributed to the principal and session", func(t *testing.T) {
		rec := f.put(sc, LevelUser, 0, `{"voice":{"enabled":true,"voice_id":"it_female_1"},"language":"it"}`)
		evs := f.settingsEvents()
		if len(evs) != 1 {
			t.Fatalf("%d events", len(evs))
		}
		e := evs[0]
		if e.ApplicationID != tApp || e.TenantID != tTenantA || e.WorkspaceID != tWsp1 || e.PrincipalID != tUser1 || e.SessionID != sc.SessionID || e.ChainSeq != 1 {
			t.Errorf("event = %+v", e)
		}
		var data DocumentWritten
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(rec.Settings)
		want := DocumentWritten{Level: "user", Version: 1, PreviousVersion: 0, Schema: LevelUser.SchemaID(), SHA256: hex.EncodeToString(sum[:]), ChangedSections: []string{"language", "voice"}}
		if !equalEvents(data, want) {
			t.Errorf("data = %+v, want %+v", data, want)
		}
		// Evidence of a change, never a copy of it.
		for _, leak := range []string{"it_female_1", `"it"`, "enabled"} {
			if strings.Contains(string(e.Data), leak) {
				t.Errorf("the event carries %q: %s", leak, e.Data)
			}
		}
	})

	t.Run("each change records the revision it replaced and what moved", func(t *testing.T) {
		f.put(sc, LevelUser, 1, `{"voice":{"enabled":false,"voice_id":"it_female_1"},"language":"it","streaming":{"enabled":false}}`)
		f.put(sc, LevelUser, 2, `{"language":"it"}`)
		evs := f.settingsEvents()
		if len(evs) != 3 {
			t.Fatalf("%d events", len(evs))
		}
		var second, third DocumentWritten
		_ = json.Unmarshal(evs[1].Data, &second)
		_ = json.Unmarshal(evs[2].Data, &third)
		if second.Version != 2 || second.PreviousVersion != 1 || strings.Join(second.ChangedSections, ",") != "streaming,voice" {
			t.Errorf("second = %+v", second)
		}
		if third.Version != 3 || third.PreviousVersion != 2 || strings.Join(third.ChangedSections, ",") != "streaming,voice" {
			t.Errorf("third = %+v", third)
		}
	})

	t.Run("a tenant change is on the tenant chain, a deployment change on the application chain", func(t *testing.T) {
		f.put(sc, LevelTenant, 0, `{"default_language":"it"}`)
		f.put(sc, LevelDeployment, 0, `{"voice":{"allowed":true}}`)
		var tenantEv, depEv *audit.Event
		for _, e := range f.settingsEvents() {
			e := e
			var d DocumentWritten
			_ = json.Unmarshal(e.Data, &d)
			switch d.Level {
			case "tenant":
				tenantEv = &e
			case "deployment":
				depEv = &e
			}
		}
		if tenantEv == nil || tenantEv.TenantID != tTenantA || tenantEv.PrincipalID != tUser1 {
			t.Errorf("tenant event = %+v", tenantEv)
		}
		if depEv == nil || depEv.ApplicationID != tApp || depEv.TenantID != "" || depEv.PrincipalID != tUser1 {
			t.Errorf("deployment event = %+v", depEv)
		}
	})

	t.Run("the chains stay intact", func(t *testing.T) {
		for _, key := range []audit.ChainKey{{ApplicationID: tApp, TenantID: tTenantA}, {ApplicationID: tApp}} {
			rep, err := f.log.VerifyChain(ctx, key, audit.VerifyOptions{})
			if err != nil || !rep.OK() || rep.Events == 0 {
				t.Errorf("%s: %+v, %v", key, rep, err)
			}
		}
	})

	t.Run("a refused or unchanged write records nothing", func(t *testing.T) {
		before := len(f.settingsEvents())
		_ = f.tryPut(sc, LevelUser, 3, `{"nope":1}`)
		_ = f.tryPut(sc, LevelUser, 9, `{"language":"it"}`)
		f.put(sc, LevelUser, 3, `{"language":"it"}`) // identical
		if got := len(f.settingsEvents()); got != before {
			t.Errorf("%d events were recorded", got-before)
		}
	})
}

func equalEvents(a, b DocumentWritten) bool {
	return a.Level == b.Level && a.Version == b.Version && a.PreviousVersion == b.PreviousVersion && a.Schema == b.Schema && a.SHA256 == b.SHA256 &&
		strings.Join(a.ChangedSections, ",") == strings.Join(b.ChangedSections, ",")
}

// A deployment document is read by every tenant of the application: its record
// does not name the principal who wrote it, so one tenant never learns the
// principals of another. The writer stays in the store and on the audit chain.
func TestSettingsDeploymentRecordNamesNoPrincipal(t *testing.T) {
	f := newFixture(t)
	a := ada()
	carol := scopeOf(tApp, tTenantB, tWsp1, "usr_carol", a.CapabilityScopes...)

	put := f.put(a, LevelDeployment, 0, `{"default_language":"it"}`)
	if put.UpdatedBy != "" || put.UpdatedAt == nil {
		t.Errorf("the Put response names %q", put.UpdatedBy)
	}
	read := f.get(carol, LevelDeployment)
	if read.Version != 1 || read.UpdatedBy != "" {
		t.Errorf("another tenant reads %+v", read)
	}
	if wire, _ := json.Marshal(read); strings.Contains(string(wire), tUser1) || strings.Contains(string(wire), "updated_by") {
		t.Errorf("the record leaks the writer: %s", wire)
	}
	// The tenant's own document names its writer to that tenant; the user's to its owner.
	if got := f.put(a, LevelTenant, 0, `{"default_language":"it"}`); got.UpdatedBy != tUser1 {
		t.Errorf("tenant record updated_by = %q", got.UpdatedBy)
	}
	bob := scopeOf(tApp, tTenantA, tWsp1, tUser2, a.CapabilityScopes...)
	if got := f.get(bob, LevelTenant); got.UpdatedBy != tUser1 {
		t.Errorf("a principal of the tenant reads updated_by %q", got.UpdatedBy)
	}
	// The writer is still on record where it belongs.
	var who string
	if err := f.db.QueryRowContext(context.Background(), "SELECT updated_by FROM settings_documents WHERE level = 'deployment'").Scan(&who); err != nil || who != tUser1 {
		t.Errorf("stored updated_by = %q, %v", who, err)
	}
	var attributed string
	for _, e := range f.settingsEvents() {
		var d DocumentWritten
		_ = json.Unmarshal(e.Data, &d)
		if d.Level == "deployment" {
			attributed = e.PrincipalID
		}
	}
	if attributed != tUser1 {
		t.Errorf("the audit event is attributed to %q", attributed)
	}
}
