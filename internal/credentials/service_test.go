package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestRefBoundToProviderDeploymentApplicationTenant(t *testing.T) {
	g := newRig(t)
	oauth, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{
		ProviderID: "fake", Label: "consented", Scopes: []string{"models.read", "chat.write"},
		Account: providers.SafeAccount{AccountID: ptr("acct_9")}, Access: Secret(tKey), Refresh: Secret(tKey2),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaValid(t, oauth)

	// the reference records exactly the binding of the scope that made it
	b := oauth.Binding
	if string(b.ProviderID) != "fake" || string(b.DeploymentID) != tDeploy || string(b.ApplicationID) != tApp || string(b.TenantID) != tTenant ||
		!reflect.DeepEqual(b.Scopes, []string{"models.read", "chat.write"}) {
		t.Fatalf("binding = %+v", b)
	}

	use := Use{Provider: "fake", Purpose: "inference", Scopes: []string{"chat.write"}}
	if v, err := g.svc.Resolve(bg, g.scope, string(oauth.RefID), use); err != nil || v.Reveal() != tKey {
		t.Fatalf("a use inside the binding: %v", err)
	}

	t.Run("provider", func(t *testing.T) {
		u := use
		u.Provider = "other"
		_, err := g.svc.Resolve(bg, g.scope, string(oauth.RefID), u)
		mustCode(t, err, contracts.ErrScopeViolation, ReasonProviderMismatch)
	})
	t.Run("deployment", func(t *testing.T) {
		// another installation over a copy of the same database and backends
		other, err := New(Config{
			DB: g.db, Audit: g.log, DeploymentID: "dep_other", Clock: g.clk, Backends: []Backend{g.enc}, Default: BackendEncryptedFile,
		})
		if err != nil {
			t.Fatal(err)
		}
		s := g.scope
		s.DeploymentID = "dep_other"
		_, err = other.Resolve(bg, s, string(oauth.RefID), use)
		mustCode(t, err, contracts.ErrScopeViolation, ReasonDeploymentMismatch)
		// and this installation refuses a scope of that one
		_, err = g.svc.Resolve(bg, s, string(oauth.RefID), use)
		mustCode(t, err, contracts.ErrScopeViolation, ReasonDeploymentMismatch)
	})
	t.Run("application", func(t *testing.T) {
		_, err := g.svc.Resolve(bg, scopeOf(tOther, tTenant), string(oauth.RefID), use)
		mustCode(t, err, contracts.ErrScopeViolation, ReasonNotBound)
	})
	t.Run("tenant", func(t *testing.T) {
		_, err := g.svc.Resolve(bg, scopeOf(tApp, tTenant2), string(oauth.RefID), use)
		mustCode(t, err, contracts.ErrScopeViolation, ReasonNotBound)
	})
	t.Run("permitted scopes", func(t *testing.T) {
		u := use
		u.Scopes = []string{"chat.write", "files.read"}
		_, err := g.svc.Resolve(bg, g.scope, string(oauth.RefID), u)
		mustCode(t, err, contracts.ErrScopeViolation, ReasonScopeNotPermitted)
	})
	t.Run("a scope of no level, or a malformed use", func(t *testing.T) {
		if _, err := g.svc.Resolve(bg, contracts.Scope{}, string(oauth.RefID), use); contracts.CodeOf(err) != contracts.ErrScopeMissing {
			t.Fatalf("zero scope: %v", err)
		}
		u := use
		u.Provider = ""
		_, err := g.svc.Resolve(bg, g.scope, string(oauth.RefID), u)
		mustCode(t, err, contracts.ErrValidationFailed, ReasonBadRequest)
	})

	// the record is bound: the encrypted value does not open under another binding
	slot := Slot{Ref: string(oauth.RefID), Binding: binding("fake", tDeploy, tApp, tTenant2)}
	if _, err := g.enc.Get(bg, slot); err == nil {
		t.Fatal("the stored value opened under another tenant's binding")
	}
	// the table is scoped like every other
	storetest.AssertScopedTables(t, g.db, "trust_publisher_keys")
}

func TestCrossTenantRefDenied(t *testing.T) {
	g := newRig(t)
	a := g.addKey(tKey)
	scopeB := scopeOf(tApp, tTenant2)
	scopeX := scopeOf(tOther, tTenant)

	missing := "cred_" + strings.Repeat("0", 25) + "1"
	_, errMissing := g.svc.Resolve(bg, scopeB, missing, g.use())
	_, errOther := g.svc.Resolve(bg, scopeB, string(a.RefID), g.use())
	mustCode(t, errOther, contracts.ErrScopeViolation, ReasonNotBound)
	if errMissing == nil || errMissing.Error() != errOther.Error() {
		t.Fatalf("existence leaks: %v vs %v", errMissing, errOther)
	}

	for _, s := range []contracts.Scope{scopeB, scopeX} {
		if _, err := g.svc.Get(bg, s, a.RefID); ReasonOf(err) != ReasonNotBound {
			t.Errorf("Get: %v", err)
		}
		if _, err := g.svc.CredentialStatus(bg, s, a.RefID); ReasonOf(err) != ReasonNotBound {
			t.Errorf("CredentialStatus: %v", err)
		}
		if _, err := g.svc.Rotate(bg, s, a.RefID, Rotation{Key: Secret(tKey2)}); ReasonOf(err) != ReasonNotBound {
			t.Errorf("Rotate: %v", err)
		}
		if _, err := g.svc.Revoke(bg, s, a.RefID); ReasonOf(err) != ReasonNotBound {
			t.Errorf("Revoke: %v", err)
		}
		if _, err := g.svc.Flag(bg, s, a.RefID, Flag{Status: providers.CredentialRefStatusInvalid}); ReasonOf(err) != ReasonNotBound {
			t.Errorf("Flag: %v", err)
		}
		if l, err := g.svc.List(bg, s, ""); err != nil || len(l) != 0 {
			t.Errorf("List shows %d of another tenant's references (%v)", len(l), err)
		}
		if n, err := g.svc.RevokeTenant(bg, s); err != nil || n != 0 {
			t.Errorf("RevokeTenant touched %d references of another tenant (%v)", n, err)
		}
	}
	// tenant A is untouched and still works
	if v, err := g.resolve(a.RefID); err != nil || v.Reveal() != tKey {
		t.Fatalf("the owner's credential: %v", err)
	}
	// the refusals are in the audit trail of the tenant that asked, not of the owner
	if rows := g.auditRows(EventUse.Name(), tApp, tTenant2); len(rows) < 2 || rows[0]["outcome"] != "denied" || rows[0]["reason"] != string(ReasonNotBound) {
		t.Fatalf("the refusals are not in the asking tenant's chain: %v", rows)
	}
	for _, r := range g.auditRows(EventUse.Name(), tApp, tTenant) {
		if r["outcome"] == "denied" {
			t.Fatalf("a refusal of another tenant landed in the owner's chain: %v", r)
		}
	}
}

func TestRevocationImmediate(t *testing.T) {
	g := newRig(t)
	ref := g.addKey(tKey)
	r, err := g.svc.Resolver(g.scope, ResolverOptions{Provider: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := r.Resolve(bg, string(ref.RefID)); err != nil || v.Reveal() != tKey {
		t.Fatalf("before: %v", err)
	}
	revoked, err := g.svc.Revoke(bg, g.scope, ref.RefID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != providers.CredentialRefStatusRevoked || revoked.Times.RevokedAt == nil {
		t.Fatalf("revoked reference: %+v", revoked)
	}
	assertSchemaValid(t, revoked)
	// the very next call through the SAME resolver (no restart) fails closed
	_, err = r.Resolve(bg, string(ref.RefID))
	mustCode(t, err, contracts.ErrUnauthenticated, ReasonRevoked)
	if st, _ := g.svc.CredentialStatus(bg, g.scope, ref.RefID); st != providers.CredentialRefStatusRevoked {
		t.Fatalf("status = %s", st)
	}
	// the value is gone from the backend as well
	if _, err := g.enc.Get(bg, Slot{Ref: string(ref.RefID), Binding: binding("fake", tDeploy, tApp, tTenant)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the value outlived the revocation: %v", err)
	}
	// idempotent, never revived
	if again, err := g.svc.Revoke(bg, g.scope, ref.RefID); err != nil || again.Status != providers.CredentialRefStatusRevoked {
		t.Fatalf("second revoke: %v", err)
	}
	_, err = g.svc.Rotate(bg, g.scope, ref.RefID, Rotation{Key: Secret(tKey2)})
	mustCode(t, err, contracts.ErrUnauthenticated, ReasonRevoked)
	if rows := g.auditRows(EventLifecycle.Name(), tApp, tTenant); len(rows) != 2 || rows[1]["action"] != "revoked" {
		t.Fatalf("lifecycle trail: %v", rows)
	}

	t.Run("a revoked environment reference stays revoked while the variable is still set", func(t *testing.T) {
		g := newRig(t, func(c *Config) { c.Backends = append(c.Backends, NewEnvBackend("")) })
		t.Setenv("ALIXAI_SECRET_FAKE", tKey)
		ref, err := g.svc.AddExternalRef(bg, g.scope, ExternalRefRequest{ProviderID: "fake", Backend: BackendEnv, Locator: "ALIXAI_SECRET_FAKE"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.resolve(ref.RefID); err != nil {
			t.Fatal(err)
		}
		if _, err := g.svc.Revoke(bg, g.scope, ref.RefID); err != nil {
			t.Fatal(err)
		}
		_, err = g.resolve(ref.RefID)
		mustCode(t, err, contracts.ErrUnauthenticated, ReasonRevoked)
	})

	t.Run("tenant removal revokes every reference of the tenant and only that tenant's", func(t *testing.T) {
		g := newRig(t)
		a1, a2 := g.addKey(tKey), g.addKey(tKey2)
		other := scopeOf(tApp, tTenant2)
		b, err := g.svc.AddAPIKey(bg, other, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey)})
		if err != nil {
			t.Fatal(err)
		}
		if n, err := g.svc.RevokeTenant(bg, g.scope); err != nil || n != 2 {
			t.Fatalf("RevokeTenant = %d, %v", n, err)
		}
		for _, id := range []providers.CredentialRefID{a1.RefID, a2.RefID} {
			if _, err := g.resolve(id); ReasonOf(err) != ReasonRevoked {
				t.Errorf("%s: %v", id, err)
			}
		}
		if _, err := g.svc.Resolve(bg, other, string(b.RefID), g.use()); err != nil {
			t.Fatalf("another tenant's credential was revoked: %v", err)
		}
	})
}

func TestRotationWithoutRestart(t *testing.T) {
	g := newRig(t)
	ref := g.addKey(tKey)
	r, _ := g.svc.Resolver(g.scope, ResolverOptions{Provider: "fake"})
	if v, _ := r.Resolve(bg, string(ref.RefID)); v.Reveal() != tKey {
		t.Fatal("before")
	}
	g.clk.Advance(time.Hour)
	exp := g.clk.Now().Add(30 * 24 * time.Hour)
	rotated, err := g.svc.Rotate(bg, g.scope, ref.RefID, Rotation{Key: Secret(tKey2), ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaValid(t, rotated)
	if rotated.RefID != ref.RefID || rotated.Times.RotatedAt == nil || *rotated.Fingerprint != Fingerprint(tKey2) || *rotated.Fingerprint == *ref.Fingerprint {
		t.Fatalf("rotated reference: %+v", rotated)
	}
	// the same resolver, the same service, no restart: the next call returns the new value
	if v, err := r.Resolve(bg, string(ref.RefID)); err != nil || v.Reveal() != tKey2 {
		t.Fatalf("after rotation: %v", err)
	}
	if g.val.count() != 2 {
		t.Fatalf("the new key was validated %d times in all, want once per key", g.val.count())
	}

	t.Run("a key the provider rejects changes nothing", func(t *testing.T) {
		g.val.res = ValidationResult{Accepted: false}
		_, err := g.svc.Rotate(bg, g.scope, ref.RefID, Rotation{Key: Secret("TESTONLY-rejected-key")})
		mustCode(t, err, contracts.ErrUnauthenticated, ReasonRejected)
		if v, _ := r.Resolve(bg, string(ref.RefID)); v.Reveal() != tKey2 {
			t.Fatal("a rejected rotation replaced the value")
		}
		g.val.res = ValidationResult{Accepted: true}
	})

	t.Run("the oauth engine rotates access and refresh values", func(t *testing.T) {
		o, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Scopes: []string{"chat.write"}, Access: "TESTONLY-access-1", Refresh: "TESTONLY-refresh-1"})
		if err != nil {
			t.Fatal(err)
		}
		use := Use{Provider: "fake", Purpose: "oauth.refresh", Refresh: true}
		if v, err := g.svc.Resolve(bg, g.scope, string(o.RefID), use); err != nil || v.Reveal() != "TESTONLY-refresh-1" {
			t.Fatalf("refresh value: %v", err)
		}
		exp := g.clk.Now().Add(time.Hour)
		if _, err := g.svc.Rotate(bg, g.scope, o.RefID, Rotation{Key: "TESTONLY-access-2", Refresh: "TESTONLY-refresh-2", ExpiresAt: &exp}); err != nil {
			t.Fatal(err)
		}
		if v, _ := g.svc.Resolve(bg, g.scope, string(o.RefID), Use{Provider: "fake", Purpose: "inference"}); v.Reveal() != "TESTONLY-access-2" {
			t.Fatal("access value not rotated")
		}
		if v, _ := g.svc.Resolve(bg, g.scope, string(o.RefID), use); v.Reveal() != "TESTONLY-refresh-2" {
			t.Fatal("refresh value not rotated")
		}
		// a key reference has no refresh value
		_, err = g.svc.Resolve(bg, g.scope, string(ref.RefID), use)
		mustCode(t, err, contracts.ErrValidationFailed, ReasonNoRefresh)
	})

	t.Run("a value provisioned outside is rotated there", func(t *testing.T) {
		g := newRig(t, func(c *Config) { c.Backends = append(c.Backends, NewEnvBackend("")) })
		t.Setenv("ALIXAI_SECRET_FAKE", tKey)
		ref, err := g.svc.AddExternalRef(bg, g.scope, ExternalRefRequest{ProviderID: "fake", Backend: BackendEnv, Locator: "ALIXAI_SECRET_FAKE"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = g.svc.Rotate(bg, g.scope, ref.RefID, Rotation{Key: Secret(tKey2)})
		mustCode(t, err, contracts.ErrValidationFailed, ReasonUnsupported)
		t.Setenv("ALIXAI_SECRET_FAKE", tKey2) // the supervisor injects a new value: effective on the next call
		if v, err := g.resolve(ref.RefID); err != nil || v.Reveal() != tKey2 {
			t.Fatalf("env rotation: %v", err)
		}
	})
}

func TestMaskedFingerprintOnly(t *testing.T) {
	g := newRig(t)
	ref := g.addKey(tKey)
	assertSchemaValid(t, ref)
	if ref.Fingerprint == nil || !strings.HasPrefix(*ref.Fingerprint, "sha256:") || len(*ref.Fingerprint) != len("sha256:")+8 {
		t.Fatalf("fingerprint = %v", ref.Fingerprint)
	}
	hexpart := strings.TrimPrefix(*ref.Fingerprint, "sha256:")
	if strings.Contains(tKey, hexpart) || strings.HasPrefix(tKey, hexpart) || strings.HasSuffix(tKey, hexpart) {
		t.Fatal("the fingerprint is a part of the key")
	}
	// what the host can see: Get, List, the status source, every JSON form
	got, err := g.svc.Get(bg, g.scope, ref.RefID)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := g.svc.List(bg, g.scope, "")
	raw, _ := json.Marshal([]any{ref, got, list})
	notContains(t, "what the host sees", raw, tKey, tKey[:12], tKey[len(tKey)-12:])
	if *got.Fingerprint != *ref.Fingerprint || got.Kind != providers.CredentialKindAPIKey {
		t.Fatalf("Get = %+v", got)
	}
	// an oauth reference has no fingerprint
	o, _ := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Scopes: []string{"s"}, Access: "TESTONLY-a"})
	if o.Fingerprint != nil {
		t.Fatalf("oauth fingerprint = %v", *o.Fingerprint)
	}
	// the typed reference has no member that could hold a value
	for _, typ := range []reflect.Type{reflect.TypeOf(providers.CredentialRef{}), reflect.TypeOf(providers.CredentialRefBinding{})} {
		for i := 0; i < typ.NumField(); i++ {
			n := strings.ToLower(typ.Field(i).Name)
			for _, bad := range []string{"secret", "key", "token", "password", "value"} {
				if strings.Contains(n, bad) {
					t.Errorf("%s.%s can hold a secret", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

func TestManualKeyValidatedWithMinimalRequest(t *testing.T) {
	g := newRig(t)
	// nothing is stored before the provider says yes
	g.val.hook = func(req ValidationRequest) {
		if n, _ := countRows(g); n != 0 {
			t.Error("a reference was stored before the validation")
		}
		if _, err := os.Stat(filepath.Join(g.dir, FileName)); err == nil {
			t.Error("a value was stored before the validation")
		}
	}
	ref := g.addKey(tKey, func(r *AddAPIKeyRequest) { r.Scopes = []string{"models.read"} })
	if g.val.count() != 1 {
		t.Fatalf("validator called %d times, want exactly once", g.val.count())
	}
	req := g.val.calls[0]
	if req.ProviderID != "fake" || req.Key.Reveal() != tKey || !reflect.DeepEqual(req.Scopes, []string{"models.read"}) {
		t.Fatalf("validation request = %+v", req)
	}
	// the request type is the whole of what a validator receives: the provider, the key, the scopes
	if n := reflect.TypeOf(ValidationRequest{}).NumField(); n != 3 {
		t.Fatalf("ValidationRequest has %d fields: tenant, application or prompt data must not be added", n)
	}
	if len(g.val.deadln) != 1 || g.val.deadln[0].IsZero() {
		t.Fatal("the validation request had no deadline")
	}
	if ref.Times.LastValidatedAt == nil || ref.Status != providers.CredentialRefStatusActive {
		t.Fatalf("reference = %+v", ref)
	}
	g.val.hook = nil

	t.Run("the provider rejects the key: nothing is stored", func(t *testing.T) {
		g := newRig(t)
		g.val.res = ValidationResult{Accepted: false}
		_, err := g.svc.AddAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey)})
		mustCode(t, err, contracts.ErrUnauthenticated, ReasonRejected)
		if n, _ := countRows(g); n != 0 {
			t.Fatalf("%d references stored for a rejected key", n)
		}
		if _, err := os.Stat(filepath.Join(g.dir, FileName)); err == nil {
			t.Fatal("a rejected key was written to the backend")
		}
		if rows := g.auditRows(EventLifecycle.Name(), tApp, tTenant); len(rows) != 1 || rows[0]["action"] != "rejected" {
			t.Fatalf("audit: %v", rows)
		}
	})
	t.Run("the validation cannot be made: fails closed, error text is not passed on", func(t *testing.T) {
		g := newRig(t)
		g.val.err = errors.New("dial failed for key " + tCanary)
		_, err := g.svc.AddAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey)})
		mustCode(t, err, contracts.ErrUnavailable, ReasonValidationFailed)
		notContains(t, "the error", []byte(err.Error()), tCanary, tKey)
		if n, _ := countRows(g); n != 0 {
			t.Fatal("a key was stored without a validation")
		}
	})
	t.Run("no validator, no manual key", func(t *testing.T) {
		g := newRig(t, func(c *Config) { c.Validator = nil })
		_, err := g.svc.AddAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey)})
		mustCode(t, err, contracts.ErrUnavailable, ReasonValidatorMissing)
	})
	t.Run("label, expiry and malformed requests", func(t *testing.T) {
		g := newRig(t)
		past := g.clk.Now().Add(-time.Hour)
		future := g.clk.Now().Add(48 * time.Hour)
		for name, mut := range map[string]func(*AddAPIKeyRequest){
			"expired":       func(r *AddAPIKeyRequest) { r.ExpiresAt = &past },
			"control label": func(r *AddAPIKeyRequest) { r.Label = "a\x00b" },
			"long label":    func(r *AddAPIKeyRequest) { r.Label = strings.Repeat("x", 81) },
			"empty key":     func(r *AddAPIKeyRequest) { r.Key = "" },
			"bad provider":  func(r *AddAPIKeyRequest) { r.ProviderID = "Not A Provider" },
			"bad scope":     func(r *AddAPIKeyRequest) { r.Scopes = []string{"has space"} },
		} {
			req := AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey)}
			mut(&req)
			if _, err := g.svc.AddAPIKey(bg, g.scope, req); contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("%s: %v", name, err)
			}
		}
		ref, err := g.svc.AddAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey), Label: "prod key", ExpiresAt: &future})
		if err != nil {
			t.Fatal(err)
		}
		assertSchemaValid(t, ref)
		if ref.Label == nil || *ref.Label != "prod key" || ref.Times.ExpiresAt == nil {
			t.Fatalf("label and expiry not kept: %+v", ref)
		}
		// 48 hours left: expiring soon; then past the expiry: expired and refused
		got, _ := g.svc.Get(bg, g.scope, ref.RefID)
		if !hasWarning(got, providers.ConnectionWarningExpiringSoon) {
			t.Fatalf("no expiring_soon warning: %v", got.Warnings)
		}
		g.clk.Advance(49 * time.Hour)
		_, err = g.resolve(ref.RefID)
		mustCode(t, err, contracts.ErrExpired, ReasonExpired)
		if st, _ := g.svc.CredentialStatus(bg, g.scope, ref.RefID); st != providers.CredentialRefStatusExpired {
			t.Fatalf("status = %s", st)
		}
	})
	t.Run("account identifiers that are not provider ids are dropped", func(t *testing.T) {
		g := newRig(t)
		g.val.res = ValidationResult{Accepted: true, Account: providers.SafeAccount{AccountID: ptr("someone@example.com"), OrganizationID: ptr("org-7"), ProjectID: ptr("Jane Doe")}}
		ref := g.addKey(tKey)
		if ref.Account.AccountID != nil || ref.Account.ProjectID != nil || ref.Account.OrganizationID == nil {
			t.Fatalf("account = %+v", ref.Account)
		}
		assertSchemaValid(t, ref)
	})
}

func hasWarning(r providers.CredentialRef, w providers.ConnectionWarning) bool {
	for _, x := range r.Warnings {
		if x == w {
			return true
		}
	}
	return false
}

func countRows(g *rig) (int, error) {
	var n int
	err := g.db.QueryRowContext(bg, "SELECT COUNT(*) FROM credentials_refs").Scan(&n)
	return n, err
}

func TestManualKeyWarnsBroaderScope(t *testing.T) {
	g := newRig(t)
	ref := g.addKey(tKey)
	if !hasWarning(ref, providers.ConnectionWarningBroaderThanOauthScopes) {
		t.Fatalf("warnings = %v", ref.Warnings)
	}
	assertSchemaValid(t, ref)
	// the warning is part of what the host reads later, and survives a rotation
	got, _ := g.svc.Get(bg, g.scope, ref.RefID)
	if !hasWarning(got, providers.ConnectionWarningBroaderThanOauthScopes) {
		t.Fatalf("Get warnings = %v", got.Warnings)
	}
	rot, err := g.svc.Rotate(bg, g.scope, ref.RefID, Rotation{Key: Secret(tKey2)})
	if err != nil || !hasWarning(rot, providers.ConnectionWarningBroaderThanOauthScopes) {
		t.Fatalf("after rotation: %v %v", err, rot.Warnings)
	}
	// an oauth credential does not carry it
	o, _ := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Scopes: []string{"s"}, Access: "TESTONLY-a"})
	if hasWarning(o, providers.ConnectionWarningBroaderThanOauthScopes) {
		t.Fatal("an oauth credential warns about being broader than oauth")
	}
	// a key that has been in use for a long time warns that it is overdue for rotation
	g.clk.Advance(100 * 24 * time.Hour)
	old, _ := g.svc.Get(bg, g.scope, ref.RefID)
	if !hasWarning(old, providers.ConnectionWarningRotationOverdue) {
		t.Fatalf("no rotation_overdue warning: %v", old.Warnings)
	}
}

func TestExcessivePermissionsQuarantine(t *testing.T) {
	for name, res := range map[string]struct {
		r    ValidationResult
		want providers.ConnectionWarning
	}{
		"excessive permissions": {ValidationResult{Accepted: true, ExcessivePermissions: true}, providers.ConnectionWarningExcessivePermissionsReported},
		"unsupported policy":    {ValidationResult{Accepted: true, UnsupportedAccountPolicy: true}, providers.ConnectionWarningUnsupportedAccountPolicy},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t)
			g.val.res = res.r
			ref := g.addKey(tKey)
			if ref.Status != providers.CredentialRefStatusQuarantined || !hasWarning(ref, res.want) || !hasWarning(ref, providers.ConnectionWarningBroaderThanOauthScopes) {
				t.Fatalf("reference = %+v", ref)
			}
			assertSchemaValid(t, ref)
			_, err := g.resolve(ref.RefID)
			mustCode(t, err, contracts.ErrQuarantined, ReasonQuarantined)
			if st, _ := g.svc.CredentialStatus(bg, g.scope, ref.RefID); st != providers.CredentialRefStatusQuarantined {
				t.Fatalf("status = %s", st)
			}
			// a rotation to a key within limits lifts it; a key that is still excessive does not
			g.val.res = ValidationResult{Accepted: true}
			ok, err := g.svc.Rotate(bg, g.scope, ref.RefID, Rotation{Key: Secret(tKey2)})
			if err != nil || ok.Status != providers.CredentialRefStatusActive {
				t.Fatalf("rotation to a narrower key: %v %+v", err, ok)
			}
			assertSchemaValid(t, ok)
		})
	}

	t.Run("a verdict after the fact quarantines an active credential", func(t *testing.T) {
		g := newRig(t)
		ref := g.addKey(tKey)
		if _, err := g.svc.Flag(bg, g.scope, ref.RefID, Flag{Status: providers.CredentialRefStatusQuarantined}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("a quarantine without a reason: %v", err)
		}
		q, err := g.svc.Flag(bg, g.scope, ref.RefID, Flag{Status: providers.CredentialRefStatusQuarantined, Warning: providers.ConnectionWarningUnsupportedAccountPolicy})
		if err != nil {
			t.Fatal(err)
		}
		assertSchemaValid(t, q)
		_, err = g.resolve(ref.RefID)
		mustCode(t, err, contracts.ErrQuarantined, ReasonQuarantined)
		if _, err := g.svc.Flag(bg, g.scope, ref.RefID, Flag{Status: providers.CredentialRefStatusActive}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("a flag cannot make a credential active: %v", err)
		}
	})
}

func TestInvalidCredentialFailsClosedNoAlternate(t *testing.T) {
	var spy *spyBackend
	g := newRig(t, func(c *Config) {
		spy = &spyBackend{Backend: c.Backends[0]}
		c.Backends = []Backend{spy}
	})
	bad := g.addKey(tKey)
	good := g.addKey(tKey2) // a second, healthy credential for the same provider and tenant
	if _, err := g.resolve(good.RefID); err != nil {
		t.Fatal(err)
	}
	spy.gets = nil

	flagged, err := g.svc.Flag(bg, g.scope, bad.RefID, Flag{Status: providers.CredentialRefStatusInvalid})
	if err != nil || flagged.Status != providers.CredentialRefStatusInvalid {
		t.Fatalf("Flag: %v", err)
	}
	assertSchemaValid(t, flagged)
	v, err := g.resolve(bad.RefID)
	mustCode(t, err, contracts.ErrUnauthenticated, ReasonInvalid)
	if v != "" {
		t.Fatal("a value was returned for an invalid credential")
	}
	if contracts.CodeOf(err).Retryable() {
		t.Fatal("an invalid credential is retryable")
	}
	// nothing looked at the other credential, or at any value at all, on the way to refusing
	if len(spy.gets) != 0 {
		t.Fatalf("the backend was read for %v while refusing", spy.gets)
	}
	// the registry's status source says not usable, so it will not route anywhere else either
	if st, _ := g.svc.CredentialStatus(bg, g.scope, bad.RefID); st == providers.CredentialRefStatusActive {
		t.Fatal("the status source reports an invalid credential as active")
	}
	// the same through a resolver and through the adapter-shaped provider
	r, _ := g.svc.Resolver(g.scope, ResolverOptions{Provider: "fake"})
	if _, err := r.Resolve(bg, string(bad.RefID)); ReasonOf(err) != ReasonInvalid {
		t.Fatalf("resolver: %v", err)
	}
	// the package has no way to ask for "any credential of this provider"
	svc := reflect.TypeOf(g.svc)
	for i := 0; i < svc.NumMethod(); i++ {
		switch n := svc.Method(i).Name; {
		case strings.Contains(n, "Alternate"), strings.Contains(n, "Fallback"), strings.Contains(n, "Any"), strings.Contains(n, "Pick"):
			t.Errorf("Service.%s selects a credential on its own", n)
		}
	}
	// a flagged credential never comes back by itself: a revoked one stays revoked, an invalid one needs a rotation
	rot, err := g.svc.Rotate(bg, g.scope, bad.RefID, Rotation{Key: Secret("TESTONLY-fixed-key")})
	if err != nil || rot.Status != providers.CredentialRefStatusActive {
		t.Fatalf("rotating an invalid credential: %v", err)
	}
}

func TestUseNotRecordedReleasesNothing(t *testing.T) {
	g := newRig(t)
	ref := g.addKey(tKey)
	broken := &failAudit{Appender: g.log, failAll: true}
	svc, err := New(Config{DB: g.db, Audit: broken, DeploymentID: tDeploy, Clock: g.clk, Backends: []Backend{g.enc}, Default: BackendEncryptedFile})
	if err != nil {
		t.Fatal(err)
	}
	v, err := svc.Resolve(bg, g.scope, string(ref.RefID), g.use())
	mustCode(t, err, contracts.ErrUnavailable, ReasonAuditFailed)
	if v != "" {
		t.Fatal("a value was released although the use could not be recorded")
	}
	notContains(t, "the error", []byte(err.Error()), tCanary, tKey)
	// the audit trail of the healthy service has one allowed use per Resolve, and none of the failed one
	if _, err := g.resolve(ref.RefID); err != nil {
		t.Fatal(err)
	}
	if rows := g.auditRows(EventUse.Name(), tApp, tTenant); len(rows) != 1 || rows[0]["outcome"] != "allowed" || rows[0]["ref"] != string(ref.RefID) || rows[0]["purpose"] != "inference" {
		t.Fatalf("use trail = %v", rows)
	}
}

func TestExternalRefsAreOperatorBoundAndConfined(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-secrets")
	if err := ensureSecureDir(dir); err != nil {
		t.Fatal(err)
	}
	fb, err := NewFileBackend(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := newRig(t, func(c *Config) { c.Backends = append(c.Backends, NewEnvBackend(""), fb) })
	if err := writeFileOwnerOnly(filepath.Join(dir, "fake.key"), []byte(tKey+"\n"), false); err != nil {
		t.Fatal(err)
	}
	ref, err := g.svc.AddExternalRef(bg, g.scope, ExternalRefRequest{ProviderID: "fake", Label: "mounted", Backend: BackendFile, Locator: "fake.key"})
	if err != nil {
		t.Fatal(err)
	}
	assertSchemaValid(t, ref)
	if *ref.Fingerprint != Fingerprint(tKey) {
		t.Fatal("fingerprint")
	}
	if v, err := g.resolve(ref.RefID); err != nil || v.Reveal() != tKey {
		t.Fatalf("Resolve: %v", err)
	}
	// replacing the file rotates the credential with no restart
	if err := writeFileOwnerOnly(filepath.Join(dir, "fake.key"), []byte(tKey2+"\n"), false); err != nil {
		t.Fatal(err)
	}
	if v, _ := g.resolve(ref.RefID); v.Reveal() != tKey2 {
		t.Fatal("file rotation not seen")
	}
	// the file disappears: fails closed
	_ = os.Remove(filepath.Join(dir, "fake.key"))
	_, err = g.resolve(ref.RefID)
	mustCode(t, err, contracts.ErrUnavailable, ReasonSecretMissing)

	for name, req := range map[string]ExternalRefRequest{
		"env outside the prefix": {ProviderID: "fake", Backend: BackendEnv, Locator: "PATH"},
		"a path":                 {ProviderID: "fake", Backend: BackendFile, Locator: "../fake.key"},
		"a managed backend":      {ProviderID: "fake", Backend: BackendEncryptedFile, Locator: "x"},
		"an unconfigured one":    {ProviderID: "fake", Backend: BackendOSStore, Locator: "x"},
		"a missing value":        {ProviderID: "fake", Backend: BackendEnv, Locator: "ALIXAI_SECRET_NOT_SET"},
	} {
		if _, err := g.svc.AddExternalRef(bg, g.scope, req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	notContains(t, "the database", allBytes(t, filepath.Dir(g.db.Path())), tKey, tKey2)
}

func TestExpiryUsesTheInjectedClock(t *testing.T) {
	g := newRig(t)
	exp := g.clk.Now().Add(time.Hour)
	o, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Scopes: []string{"s"}, Access: "TESTONLY-a", ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	use := Use{Provider: "fake", Purpose: "inference", Scopes: []string{"s"}}
	if _, err := g.svc.Resolve(bg, g.scope, string(o.RefID), use); err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(time.Hour)
	_, err = g.svc.Resolve(bg, g.scope, string(o.RefID), use)
	mustCode(t, err, contracts.ErrExpired, ReasonExpired)
	// the OAuth engine refreshes it: usable again, same reference
	exp2 := g.clk.Now().Add(time.Hour)
	if _, err := g.svc.Rotate(bg, g.scope, o.RefID, Rotation{Key: "TESTONLY-b", ExpiresAt: &exp2}); err != nil {
		t.Fatal(err)
	}
	if v, err := g.svc.Resolve(bg, g.scope, string(o.RefID), use); err != nil || v.Reveal() != "TESTONLY-b" {
		t.Fatalf("after refresh: %v", err)
	}
	// an oauth reference without a scope is not a thing
	if _, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Access: "TESTONLY-a"}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("scope-less oauth: %v", err)
	}
}

// A tampered or foreign row is refused, never repaired.
func TestEditedRowIsRefused(t *testing.T) {
	g := newRig(t)
	ref := g.addKey(tKey)
	if _, err := g.db.ExecContext(bg, "UPDATE credentials_refs SET status = 'revoked' WHERE ref_id = ?", string(ref.RefID)); err != nil {
		t.Fatal(err)
	}
	_, err := g.resolve(ref.RefID)
	mustCode(t, err, contracts.ErrInternal, ReasonCorrupt)
	if _, err := g.svc.Get(bg, g.scope, ref.RefID); ReasonOf(err) != ReasonCorrupt {
		t.Fatalf("Get: %v", err)
	}
	// the table refuses a tenant-less or workspace-level row
	_, err = g.db.ExecContext(bg, `INSERT INTO credentials_refs (application_id, tenant_id, workspace_id, ref_id, deployment_id, provider_id, kind, status, backend, locator, has_refresh, document, created_at, updated_at)
	  VALUES ('a', '', '', 'cred_X', 'd', 'p', 'api_key', 'active', 'os_store', '', 0, '{}', 0, 0)`)
	if err == nil {
		t.Fatal("a reference without a tenant was accepted")
	}
	_, err = g.db.ExecContext(bg, `INSERT INTO credentials_refs (application_id, tenant_id, workspace_id, ref_id, deployment_id, provider_id, kind, status, backend, locator, has_refresh, document, created_at, updated_at)
	  VALUES ('a', 't', 'w', 'cred_X', 'd', 'p', 'api_key', 'active', 'os_store', '', 0, '{}', 0, 0)`)
	if err == nil {
		t.Fatal("a workspace-level reference was accepted")
	}
}

var _ = fmt.Sprint
var _ audit.EventType

func TestAnExpiredOAuthReferenceStillReleasesItsRefreshValue(t *testing.T) {
	g := newRig(t)
	exp := g.clk.Now().Add(time.Hour)
	o, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Scopes: []string{"s"}, Access: "TESTONLY-a", Refresh: "TESTONLY-r", ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(2 * time.Hour)
	use := Use{Provider: "fake", Purpose: "inference", Scopes: []string{"s"}}
	_, err = g.svc.Resolve(bg, g.scope, string(o.RefID), use)
	mustCode(t, err, contracts.ErrExpired, ReasonExpired)
	use.Refresh = true
	if v, err := g.svc.Resolve(bg, g.scope, string(o.RefID), use); err != nil || v.Reveal() != "TESTONLY-r" {
		t.Fatalf("refresh of an expired reference: %v", err)
	}
	// a revoked reference never releases it
	if _, err := g.svc.Revoke(bg, g.scope, o.RefID); err != nil {
		t.Fatal(err)
	}
	_, err = g.svc.Resolve(bg, g.scope, string(o.RefID), use)
	mustCode(t, err, contracts.ErrUnauthenticated, ReasonRevoked)
}

// rotateHookBackend runs hook once, at the first Put after it is set: between a rotation's read of the reference
// and its transaction.
type rotateHookBackend struct {
	Backend
	hook func()
}

func (b *rotateHookBackend) Put(ctx context.Context, slot Slot, value Secret) error {
	if h := b.hook; h != nil {
		b.hook = nil
		h()
	}
	return b.Backend.Put(ctx, slot, value)
}

// A refresh rotation that was in flight while the connection was re-authorized and quarantined undoes neither:
// the quarantine, its warning and the newly consented scopes stay.
func TestRefreshRotationKeepsAConcurrentReauthorization(t *testing.T) {
	hb := &rotateHookBackend{}
	g := newRig(t, func(c *Config) { hb.Backend = c.Backends[0]; c.Backends = []Backend{hb} })
	o, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{ProviderID: "fake", Scopes: []string{"chat.write"}, Access: "TESTONLY-access-1", Refresh: "TESTONLY-refresh-1"})
	if err != nil {
		t.Fatal(err)
	}
	exp := g.clk.Now().Add(time.Hour)
	hb.hook = func() {
		if _, err := g.svc.Rotate(bg, g.scope, o.RefID, Rotation{Key: "TESTONLY-access-3", ExpiresAt: &exp, Scopes: []string{"chat.write", "files.read"}}); err != nil {
			t.Error(err)
		}
		if _, err := g.svc.Flag(bg, g.scope, o.RefID, Flag{Status: providers.CredentialRefStatusQuarantined, Warning: providers.ConnectionWarningExcessivePermissionsReported}); err != nil {
			t.Error(err)
		}
	}
	if _, err := g.svc.Rotate(bg, g.scope, o.RefID, Rotation{Key: "TESTONLY-access-2", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	got, err := g.svc.Get(bg, g.scope, o.RefID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != providers.CredentialRefStatusQuarantined || !hasWarning(got, providers.ConnectionWarningExcessivePermissionsReported) ||
		strings.Join(got.Binding.Scopes, " ") != "chat.write files.read" {
		t.Fatalf("after the refresh: %s %v %v", got.Status, got.Warnings, got.Binding.Scopes)
	}
}
