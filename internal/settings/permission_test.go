package settings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// SEC-026: settings (and the integration-audit and provider read-outs) expose
// another scope's data or exceed the caller's authorisation. Every operation checks
// the verified scope and the capability scope the token carries for the level and
// the operation; a stored document or a submitted one grants nothing.
func TestSettingsPermissionChecked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	full := ada()
	// Something to read at every level, so a denied read has something to leak.
	f.put(full, LevelUser, 0, `{"language":"it"}`)
	f.put(full, LevelTenant, 0, `{"default_language":"it"}`)
	f.put(full, LevelDeployment, 0, `{"default_language":"it"}`)
	f.findings.byApp[tApp] = AuditStatus{ApplicationID: tApp, State: "active", Audited: true, RunID: "run_1", Outcome: "clear"}
	f.provs.health = ProviderHealth{Status: "ok", Roles: []ProviderRole{{Role: "controller", Configured: true, Status: "ok"}}}

	t.Run("each level and operation needs its own capability scope", func(t *testing.T) {
		for _, level := range Levels() {
			read, write := RequiredCapability(level, OpRead), RequiredCapability(level, OpWrite)
			if read == "" || write == "" || read == write {
				t.Fatalf("%s: scopes %q and %q", level, read, write)
			}
			// Everything the token has EXCEPT the scope in question.
			without := func(drop string) contracts.Scope {
				var keep []string
				for _, c := range full.CapabilityScopes {
					if c != drop {
						keep = append(keep, c)
					}
				}
				return withScopes(full, keep...)
			}
			before := f.storedDump()
			if _, err := f.svc.Get(ctx, without(read), level); err == nil {
				t.Errorf("%s: read without %s succeeded", level, read)
			} else {
				wantCode(t, err, contracts.ErrPermissionDenied)
				var ce *contracts.Error
				asErr(err, &ce)
				if ce.Details["capability"] != read {
					t.Errorf("%s: the refusal names %v, want %s", level, ce.Details["capability"], read)
				}
			}
			_, err := f.svc.Put(ctx, without(write), level, PutRequest{ExpectedVersion: 1, Settings: json.RawMessage(`{"default_language":"en"}`)})
			wantCode(t, err, contracts.ErrPermissionDenied)
			if f.storedDump() != before {
				t.Errorf("%s: a refused write changed the store", level)
			}
			// The read scope does not write, and the write scope does not read.
			readOnly := withScopes(full, read)
			_, err = f.svc.Put(ctx, readOnly, level, PutRequest{ExpectedVersion: 1, Settings: json.RawMessage(`{}`)})
			wantCode(t, err, contracts.ErrPermissionDenied)
			writeOnly := withScopes(full, write)
			_, err = f.svc.Get(ctx, writeOnly, level)
			wantCode(t, err, contracts.ErrPermissionDenied)
			// And a scope of another level is no substitute.
			for _, other := range Levels() {
				if other == level {
					continue
				}
				wrongLevel := withScopes(full, RequiredCapability(other, OpRead), RequiredCapability(other, OpWrite))
				_, err := f.svc.Get(ctx, wrongLevel, level)
				wantCode(t, err, contracts.ErrPermissionDenied)
			}
		}
	})

	t.Run("an application capability or a wildcard is no settings permission", func(t *testing.T) {
		for _, caps := range [][]string{{capGet, capCreate, capDelete}, {"settings.read"}, {"settings.user"}, {"SETTINGS.USER.READ"}} {
			_, err := f.svc.Get(ctx, withScopes(full, caps...), LevelUser)
			wantCode(t, err, contracts.ErrPermissionDenied)
		}
		// A wildcard (or padding) is not a scope at all: the token itself is malformed.
		for _, bad := range []string{"settings.*", "*", "settings.user.read "} {
			_, err := f.svc.Get(ctx, withScopes(full, bad), LevelUser)
			wantCode(t, err, contracts.ErrScopeViolation)
		}
	})

	t.Run("an unverified scope is refused before anything is read", func(t *testing.T) {
		calls := f.packages.calls
		for name, sc := range map[string]contracts.Scope{
			"zero":              {},
			"no tenant":         func() contracts.Scope { s := full.Clone(); s.TenantID = ""; return s }(),
			"no workspace":      func() contracts.Scope { s := full.Clone(); s.WorkspaceID = ""; return s }(),
			"no principal":      func() contracts.Scope { s := full.Clone(); s.PrincipalID = ""; return s }(),
			"no application":    func() contracts.Scope { s := full.Clone(); s.ApplicationID = ""; return s }(),
			"no scopes":         func() contracts.Scope { s := full.Clone(); s.CapabilityScopes = nil; return s }(),
			"wildcard tenant":   func() contracts.Scope { s := full.Clone(); s.TenantID = "*"; return s }(),
			"padded principal":  func() contracts.Scope { s := full.Clone(); s.PrincipalID = " usr_ada"; return s }(),
			"no deployment":     func() contracts.Scope { s := full.Clone(); s.DeploymentID = ""; return s }(),
			"no roles":          func() contracts.Scope { s := full.Clone(); s.Roles = nil; return s }(),
			"a blank session":   func() contracts.Scope { s := full.Clone(); s.SessionID = "  "; return s }(),
			"over-long tenant":  func() contracts.Scope { s := full.Clone(); s.TenantID = strings.Repeat("t", 200); return s }(),
			"control character": func() contracts.Scope { s := full.Clone(); s.WorkspaceID = "wsp\n"; return s }(),
		} {
			for _, level := range Levels() {
				if _, err := f.svc.Get(ctx, sc, level); err == nil {
					t.Errorf("%s/%s: Get succeeded", name, level)
				} else if c := code(err); c != contracts.ErrScopeMissing && c != contracts.ErrScopeViolation {
					t.Errorf("%s/%s: code %s", name, level, c)
				}
				if _, err := f.svc.Put(ctx, sc, level, PutRequest{Settings: json.RawMessage(`{}`)}); err == nil {
					t.Errorf("%s/%s: Put succeeded", name, level)
				}
			}
			for fn, call := range map[string]func() error{
				"Effective":        func() error { _, e := f.svc.Effective(ctx, sc); return e },
				"Profiles":         func() error { _, e := f.svc.Profiles(ctx, sc); return e },
				"Capabilities":     func() error { _, e := f.svc.Capabilities(ctx, sc); return e },
				"IntegrationAudit": func() error { _, e := f.svc.IntegrationAudit(ctx, sc); return e },
				"ProviderHealth":   func() error { _, e := f.svc.ProviderHealth(ctx, sc); return e },
			} {
				if err := call(); err == nil {
					t.Errorf("%s/%s succeeded", name, fn)
				}
			}
		}
		if f.packages.calls != calls {
			t.Error("a request with an unverified scope reached the package source")
		}
		if len(f.findings.asked) != 0 {
			t.Errorf("an unverified scope reached the audit source: %v", f.findings.asked)
		}
	})

	t.Run("the read-outs need their own permission", func(t *testing.T) {
		userOnly := withScopes(full, CapabilityUserRead, CapabilityUserWrite, capGet)
		for name, call := range map[string]func(contracts.Scope) error{
			"IntegrationAudit": func(s contracts.Scope) error { _, e := f.svc.IntegrationAudit(ctx, s); return e },
			"ProviderHealth":   func(s contracts.Scope) error { _, e := f.svc.ProviderHealth(ctx, s); return e },
		} {
			wantCode(t, call(userOnly), contracts.ErrPermissionDenied)
			if err := call(withScopes(full, CapabilityDeploymentRead)); err != nil {
				t.Errorf("%s with settings.deployment.read: %v", name, err)
			}
			wantCode(t, call(withScopes(full, CapabilityDeploymentWrite)), contracts.ErrPermissionDenied)
		}
		for name, call := range map[string]func(contracts.Scope) error{
			"Effective":    func(s contracts.Scope) error { _, e := f.svc.Effective(ctx, s); return e },
			"Profiles":     func(s contracts.Scope) error { _, e := f.svc.Profiles(ctx, s); return e },
			"Capabilities": func(s contracts.Scope) error { _, e := f.svc.Capabilities(ctx, s); return e },
		} {
			wantCode(t, call(withScopes(full, CapabilityTenantRead, CapabilityDeploymentRead, capGet)), contracts.ErrPermissionDenied)
			if err := call(withScopes(full, CapabilityUserRead, capGet)); err != nil {
				t.Errorf("%s with settings.user.read: %v", name, err)
			}
		}
	})

	t.Run("the read-outs never answer for another application", func(t *testing.T) {
		f.findings.byApp[tApp] = AuditStatus{ApplicationID: tOther, State: "active"} // a source that answers for the wrong application
		_, err := f.svc.IntegrationAudit(ctx, full)
		wantCode(t, err, contracts.ErrInternal)
		f.findings.byApp[tApp] = AuditStatus{ApplicationID: tApp, State: "active"}
		other := scopeOf(tOther, tTenantA, tWsp1, tUser1, full.CapabilityScopes...)
		if _, err := f.svc.IntegrationAudit(ctx, other); err != nil {
			t.Fatal(err)
		}
		if got := f.findings.asked[len(f.findings.asked)-1]; got != tOther {
			t.Errorf("the source was asked about %q, want the caller's application %q", got, tOther)
		}
	})

	t.Run("a document grants nothing: settings are never an input to authorisation", func(t *testing.T) {
		userOnly := withScopes(full, CapabilityUserRead, CapabilityUserWrite, capGet)
		// Even with every kind of document stored, the weaker token stays weaker.
		for _, level := range []Level{LevelTenant, LevelDeployment} {
			if _, err := f.svc.Get(ctx, userOnly, level); code(err) != contracts.ErrPermissionDenied {
				t.Errorf("%s: %v", level, err)
			}
		}
		access, err := f.svc.Capabilities(ctx, userOnly)
		if err != nil {
			t.Fatal(err)
		}
		if got := access.SettingsAccess; got.User != (LevelAccess{true, true}) || got.Tenant != (LevelAccess{}) || got.Deployment != (LevelAccess{}) {
			t.Errorf("settings access = %+v", got)
		}
	})

	t.Run("a quarantined application serves no settings, but its audit findings stay readable", func(t *testing.T) {
		g := newFixture(t)
		g.put(full, LevelUser, 0, `{"language":"it"}`)
		g.findings.byApp[tApp] = AuditStatus{ApplicationID: tApp, State: "quarantined", Audited: true, Outcome: "blocked"}
		g.packages.quarantine(tApp, true)
		for _, level := range Levels() {
			_, err := g.svc.Get(ctx, full, level)
			wantCode(t, err, contracts.ErrQuarantined)
			wantCode(t, g.tryPut(full, level, 0, `{}`), contracts.ErrQuarantined)
		}
		for name, call := range map[string]func() error{
			"Effective":    func() error { _, e := g.svc.Effective(ctx, full); return e },
			"Profiles":     func() error { _, e := g.svc.Profiles(ctx, full); return e },
			"Capabilities": func() error { _, e := g.svc.Capabilities(ctx, full); return e },
		} {
			wantCode(t, call(), contracts.ErrQuarantined)
			_ = name
		}
		st, err := g.svc.IntegrationAudit(ctx, full)
		if err != nil || st.State != "quarantined" || st.Outcome != "blocked" {
			t.Errorf("findings of a quarantined application: %+v, %v", st, err)
		}
		g.packages.quarantine(tApp, false)
		if got := g.get(full, LevelUser); got.Version != 1 {
			t.Errorf("the document did not survive the quarantine: %+v", got)
		}
	})

	t.Run("a package that is not the caller's application is not served", func(t *testing.T) {
		g := newFixture(t)
		g.packages.pkgs[tApp] = buildPackage(t, tOther) // a source that hands back another application's package
		_, err := g.svc.Get(ctx, full, LevelUser)
		wantCode(t, err, contracts.ErrQuarantined)
		g.packages.pkgs[tApp] = tPackage{Package: buildPackage(t, tApp).WithStatus("quarantined"), langs: []string{"en"}}
		_, err = g.svc.Get(ctx, full, LevelUser)
		wantCode(t, err, contracts.ErrQuarantined)
	})
}
