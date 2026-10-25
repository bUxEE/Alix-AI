//go:build !noexternal

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/external/anthropic"
	"github.com/buxee/alix-ai/adapters/external/gemini"
	"github.com/buxee/alix-ai/adapters/external/meta"
	"github.com/buxee/alix-ai/adapters/external/openai"
	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/dialogue/extmode"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/modes/hybrid"
	"github.com/buxee/alix-ai/internal/oauth"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/verify"
	voiceprofile "github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

type externalProviderTestKeys struct{}

func (externalProviderTestKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return "fixture-credential-value-0001", nil
}

func TestExternalProviderOpenAIBuildsAddsItsDestinationAndNeverWidens(t *testing.T) {
	if !ExternalProvidersBuilt() {
		t.Fatal("the default build must carry the adapters")
	}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	reg := egress.NewProviderRegistry()
	spec := ExternalProviderSpec{
		Provider: ExternalProviderOpenAI,
		OpenAI:   openai.Config{BaseURL: srv.URL, CredentialRef: "cred_01TESTREFERENCE", Model: "gpt-5-mini"}, // TEST-ONLY opaque reference id, not a key
		Egress:   reg, Keys: externalProviderTestKeys{},
	}
	p, err := NewExternalProvider(spec)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatal("building the provider sent a request")
	}
	if p.Name() != "openai" || reg.Allowlist("openai") == nil || reg.Allowlist("openai").Len() != 1 {
		t.Fatalf("name %q, allowlist %v", p.Name(), reg.Allowlist("openai"))
	}
	caps, err := p.Capabilities(context.Background())
	if err != nil || caps.StructuredOutput != providers.StructuredStrict {
		t.Fatalf("%+v %v", caps, err)
	}
	h, err := p.Health(context.Background())
	if err != nil || h.Status == providers.HealthUnavailable {
		t.Fatalf("%+v %v", h, err)
	}

	// An allowlist the operator already set is kept as it is.
	other, _ := egress.NewAllowlist(egress.Destination{Host: "example.com", Ports: []int{443}, Scheme: "https"})
	reg2 := egress.NewProviderRegistry()
	_ = reg2.Set("openai", other)
	spec.Egress = reg2
	if _, err := NewExternalProvider(spec); err != nil {
		t.Fatal(err)
	}
	if got := reg2.Allowlist("openai").Destinations(); len(got) != 1 || got[0].Host != "example.com" {
		t.Fatalf("the operator's allowlist was changed: %+v", got)
	}

	for name, bad := range map[string]ExternalProviderSpec{
		"unknown provider": {Provider: "nope", Egress: reg, Keys: externalProviderTestKeys{}},
		"no registry":      {Provider: "openai", OpenAI: spec.OpenAI, Keys: externalProviderTestKeys{}},
		"no keys":          {Provider: "openai", OpenAI: spec.OpenAI, Egress: reg},
	} {
		if _, err := NewExternalProvider(bad); err == nil {
			t.Errorf("%s: built", name)
		}
	}
}

// ---- the providers and oauth composition (X-13) ----

type externalSpyAudit struct {
	audit.Appender
	mu    sync.Mutex
	types []audit.EventType
	fail  bool
}

func (s *externalSpyAudit) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	s.mu.Lock()
	fail := s.fail
	s.types = append(s.types, ev.Type)
	s.mu.Unlock()
	if fail {
		return audit.Event{}, errors.New("audit down")
	}
	return s.Appender.AppendAt(ctx, loc, ev)
}

func (s *externalSpyAudit) count(t audit.EventType) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.types {
		if x == t {
			n++
		}
	}
	return n
}

const externalTestDeployment = "dep_test"

func externalTestScope(tenant, principal string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: externalTestDeployment, ApplicationID: "app_demo", TenantID: tenant, WorkspaceID: "wsp_main",
		PrincipalID: principal, Roles: []string{"member"}, CapabilityScopes: []string{"items.get"}, SessionID: "ses_01",
	}
}

type externalAcceptKeys struct{}

func (externalAcceptKeys) ValidateKey(context.Context, credentials.ValidationRequest) (credentials.ValidationResult, error) {
	return credentials.ValidationResult{Accepted: true}, nil
}

func externalTestAdmin(t *testing.T, dir string, spy **externalSpyAudit) *externalAdmin {
	t.Helper()
	db := storetest.Open(t, nil)
	log, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sp := &externalSpyAudit{Appender: log}
	if spy != nil {
		*spy = sp
	}
	a, err := newExternalAdmin(externalAdminDeps{
		DB: db, Audit: sp, DeploymentID: externalTestDeployment, Dir: dir, Validator: externalAcceptKeys{},
		Section: externalConfig{Enabled: true, DeploymentOperators: []string{"app_demo/usr_ops"}, TenantAdmins: []string{"app_demo/ten_acme/usr_admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Operators come from the section and from nothing else: a tenant administrator is one tenant's, a deployment
// operator is everybody's, a stranger and another deployment's operator are nobody's.
func TestExternalAdminOperatorsComeFromTheSection(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	ok := func(sc contracts.Scope, l gateway.ProvidersOperatorLevel) bool {
		return a.AuthorizeOperator(ctx, sc, l) == nil
	}
	ops, adm := externalTestScope("ten_acme", "usr_ops"), externalTestScope("ten_acme", "usr_admin")
	if !ok(ops, gateway.ProvidersOperatorDeployment) || !ok(ops, gateway.ProvidersOperatorTenant) {
		t.Error("a deployment operator was refused")
	}
	if !ok(adm, gateway.ProvidersOperatorTenant) || ok(adm, gateway.ProvidersOperatorDeployment) {
		t.Error("a tenant administrator got the wrong level")
	}
	if ok(externalTestScope("ten_other", "usr_admin"), gateway.ProvidersOperatorTenant) {
		t.Error("a tenant administrator of one tenant passed in another")
	}
	if ok(externalTestScope("ten_acme", "usr_nobody"), gateway.ProvidersOperatorTenant) {
		t.Error("a stranger passed")
	}
	other := ops
	other.DeploymentID = "dep_other"
	if ok(other, gateway.ProvidersOperatorTenant) {
		t.Error("an operator of another deployment passed")
	}
	// A token that asserts every role is no operator.
	powerful := externalTestScope("ten_acme", "usr_nobody")
	powerful.Roles = []string{"admin", "operator", "deployment_operator"}
	if ok(powerful, gateway.ProvidersOperatorTenant) {
		t.Error("asserted roles made an operator")
	}
}

// An operator entry names its application: each application's issuer chooses the subject it signs, so another
// application that mints sub=usr_ops is not the operator. It cannot clear the kill switch, change the deployment
// mode or the egress policy, and its tenant administrator of the same name is nobody either.
func TestExternalAdminOperatorIsBoundToItsApplication(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	ops := externalTestScope("ten_acme", "usr_ops")
	if _, err := a.SetKillSwitch(ctx, ops, true, "provider_incident"); err != nil {
		t.Fatal(err)
	}
	forged := func(principal string) contracts.Scope {
		sc := externalTestScope("ten_acme", principal)
		sc.ApplicationID = "app_other"
		return sc
	}
	for _, p := range []string{"usr_ops", "usr_admin"} {
		for _, l := range []gateway.ProvidersOperatorLevel{gateway.ProvidersOperatorDeployment, gateway.ProvidersOperatorTenant} {
			if err := a.AuthorizeOperator(ctx, forged(p), l); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
				t.Errorf("%s of another application at level %v: %v", p, l, err)
			}
		}
	}
	other := forged("usr_ops")
	if _, err := a.SetKillSwitch(ctx, other, false, ""); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("another application released the kill switch: %v", err)
	}
	if ks, _ := a.KillSwitch(ctx, ops); !ks.Engaged {
		t.Error("the kill switch is no longer engaged")
	}
	if _, err := a.SetMode(ctx, other, gateway.ProvidersModeDeployment, pv.DeploymentModeExternal); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("another application changed the deployment mode: %v", err)
	}
	if v, _ := a.Mode(ctx, ops); v.Levels[gateway.ProvidersModeDeployment] != "" {
		t.Errorf("the deployment mode reads %+v", v.Levels)
	}
	in := gateway.ProvidersEgressPolicyInput{Level: pv.EgressLevelDeployment}
	if _, err := a.PutEgressPolicy(ctx, other, in); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
		t.Errorf("another application changed the deployment egress policy: %v", err)
	}
	// The operator of the listed application still passes, at both levels.
	if _, err := a.SetKillSwitch(ctx, ops, false, ""); err != nil {
		t.Errorf("the operator could not release the kill switch: %v", err)
	}
}

// The mode survives a restart, the file is owner only, the change is audited, and a change that cannot be
// audited is not applied.
func TestExternalAdminModePersistedAuditedAndFailClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "external")
	var spy *externalSpyAudit
	a := externalTestAdmin(t, dir, &spy)
	ctx := context.Background()
	sc := externalTestScope("ten_acme", "usr_admin")

	v, err := a.Mode(ctx, sc)
	if err != nil || v.Effective != pv.DeploymentModeLocal {
		t.Fatalf("default mode = %+v, %v", v, err)
	}
	v, err = a.SetMode(ctx, sc, gateway.ProvidersModeTenant, pv.DeploymentModeHybrid)
	if err != nil || v.Effective != pv.DeploymentModeLocal {
		// The deployment and application levels are unset, which counts as local: the most restrictive wins.
		t.Fatalf("set = %+v, %v", v, err)
	}
	if v.Levels[gateway.ProvidersModeTenant] != pv.DeploymentModeHybrid {
		t.Errorf("the tenant level reads %+v", v.Levels)
	}
	if n := spy.count("providers.mode_changed"); n != 1 {
		t.Errorf("%d mode_changed events, want 1", n)
	}
	// Another tenant is unaffected.
	if v, _ := a.Mode(ctx, externalTestScope("ten_other", "usr_x")); len(v.Levels) != 0 {
		t.Errorf("another tenant sees %+v", v)
	}

	// A change whose audit record cannot be appended is not applied.
	spy.mu.Lock()
	spy.fail = true
	spy.mu.Unlock()
	if _, err := a.SetMode(ctx, sc, gateway.ProvidersModeTenant, pv.DeploymentModeExternal); err == nil {
		t.Fatal("a mode change went through with the audit log down")
	}
	if v, _ := a.Mode(ctx, sc); v.Levels[gateway.ProvidersModeTenant] != pv.DeploymentModeHybrid {
		t.Errorf("tenant mode after the failed change = %v, want hybrid", v.Levels[gateway.ProvidersModeTenant])
	}

	// A new process over the same directory reads the same mode.
	f, err := openExternalModeFile(filepath.Join(dir, "modes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m, set, _ := f.Mode(ctx, modes.LevelTenant, sc); !set || m != pv.DeploymentModeHybrid {
		t.Errorf("reloaded mode = %v %v", m, set)
	}
	// SEC-081: owner only (POSIX); on Windows the mode bits carry no meaning.
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, "modes.json"))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("modes.json is %v: group or world can read it", st.Mode().Perm())
		}
		if st, _ := os.Stat(dir); st.Mode().Perm()&0o077 != 0 {
			t.Errorf("the directory is %v", st.Mode().Perm())
		}
	}
	// A corrupt file stops start-up instead of being read as "external".
	if err := os.WriteFile(filepath.Join(dir, "modes.json"), []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openExternalModeFile(filepath.Join(dir, "modes.json")); err == nil {
		t.Error("a corrupt mode file was accepted")
	}
}

// The kill switch is engaged and released by a deployment operator; the mode reads local under it.
func TestExternalAdminKillSwitchOverridesMode(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	ops := externalTestScope("ten_acme", "usr_ops")
	for _, lv := range []gateway.ProvidersModeLevel{gateway.ProvidersModeDeployment, gateway.ProvidersModeApplication, gateway.ProvidersModeTenant} {
		if _, err := a.SetMode(ctx, ops, lv, pv.DeploymentModeExternal); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := a.Mode(ctx, ops); v.Effective != pv.DeploymentModeExternal {
		t.Fatalf("mode = %+v", v)
	}
	ks, err := a.SetKillSwitch(ctx, ops, true, "provider_incident")
	if err != nil || !ks.Engaged || ks.Reason != "provider_incident" || ks.ChangedBy != "usr_ops" {
		t.Fatalf("kill switch = %+v, %v", ks, err)
	}
	if v, _ := a.Mode(ctx, ops); v.Effective != pv.DeploymentModeLocal || !v.KillSwitch || v.DecidedBy != modes.DecidedByKillSwitch {
		t.Errorf("mode under the kill switch = %+v", v)
	}
	if ks, err = a.SetKillSwitch(ctx, ops, false, ""); err != nil || ks.Engaged {
		t.Fatalf("release = %+v, %v", ks, err)
	}
	if _, err := a.SetKillSwitch(ctx, ops, true, "Free text"); err == nil {
		t.Error("a free-text reason was accepted")
	}
}

// A provider without a registered profile has nothing to connect to: the key is refused before it is stored.
func TestExternalAdminCredentialWithoutProfileIsRefusedAndNotStored(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	sc := externalTestScope("ten_acme", "usr_admin")
	var in gateway.ProvidersCredentialInput
	if err := json.Unmarshal([]byte(`{"key":"sk-test-0123456789"}`), &in); err != nil {
		t.Fatal(err)
	}
	_, err := a.SubmitCredential(ctx, sc, "openai", in)
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("err = %v", err)
	}
	if refs, _ := a.creds.List(ctx, sc, "openai"); len(refs) != 0 {
		t.Errorf("%d references were stored", len(refs))
	}
	if list, err := a.Connections(ctx, sc); err != nil || len(list) != 0 {
		t.Errorf("connections = %v, %v", list, err)
	}
	// OAuth is not configured for any provider: it cannot start.
	_, err = a.OAuthStart(ctx, sc, gateway.ProvidersOAuthStart{Provider: "openai", Capabilities: []string{"chat"}, RedirectURI: "http://127.0.0.1:5555/cb"})
	if err == nil {
		t.Fatal("OAuth started for a provider with no registration")
	}
}

// The manual-key validator asks the provider's model list with the candidate key and nothing else, over the
// allowlisted origin: an accepted key, a rejected key and an unreachable provider are told apart.
func TestExternalKeyValidatorUsesTheModelListOnly(t *testing.T) {
	var gotAuth, gotPath string
	var hits int
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4.1-mini"}]}`)
	}))
	defer srv.Close()
	v := externalKeyValidator{base: srv.URL, deployment: externalTestDeployment}
	req := credentials.ValidationRequest{ProviderID: "openai", Key: "sk-candidate-0001"}

	res, err := v.ValidateKey(context.Background(), req)
	if err != nil || !res.Accepted {
		t.Fatalf("accepted key: %+v, %v", res, err)
	}
	if gotPath != "/v1/models" || gotAuth != "Bearer sk-candidate-0001" || hits != 1 {
		t.Errorf("request: %s auth=%q hits=%d", gotPath, gotAuth, hits)
	}
	status = http.StatusUnauthorized
	if res, err = v.ValidateKey(context.Background(), req); err != nil || res.Accepted {
		t.Errorf("rejected key: %+v, %v", res, err)
	}
	if _, err = v.ValidateKey(context.Background(), credentials.ValidationRequest{ProviderID: "other", Key: "k"}); err == nil {
		t.Error("an unknown provider was validated")
	}
	srv.Close()
	if res, err = v.ValidateKey(context.Background(), req); err == nil || res.Accepted {
		t.Errorf("unreachable provider: %+v, %v", res, err)
	}
}

// Off by default: with the section disabled nothing is built, nothing is provided and no file is created; the
// section refuses an operator entry that could never match.
func TestExternalServeProviderOffByDefaultAndSectionValidates(t *testing.T) {
	dir := t.TempDir()
	svc := gateway.NewServices()
	if externalSection.Enabled {
		t.Fatal("external mode is enabled by default")
	}
	if err := externalServeProvider(context.Background(), serveDeps{Services: svc, DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.Lookup[gateway.ProvidersService](svc); ok {
		t.Error("the service was provided with external mode off")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files were created: %v", entries)
	}
	for name, c := range map[string]externalConfig{
		"bad operator":  {DeploymentOperators: []string{"has space"}},
		"bad admin":     {TenantAdmins: []string{"no-slash"}},
		"bad admin two": {TenantAdmins: []string{"ten/"}},
		// The forms that named no application: a principal id alone is what any application's issuer can sign.
		"operator without application": {DeploymentOperators: []string{"usr_ops"}},
		"admin without application":    {TenantAdmins: []string{"ten_acme/usr_admin"}},
		"operator with a tenant":       {DeploymentOperators: []string{"app_demo/ten_acme/usr_ops"}},
		"admin empty part":             {TenantAdmins: []string{"app_demo//usr_admin"}},
		"bad base":                     {OpenAIBaseURL: "https://example.com/path"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (externalConfig{DeploymentOperators: []string{"app_demo/usr_ops"}, TenantAdmins: []string{"app_demo/ten_acme/usr_admin"}, OpenAIBaseURL: "https://api.openai.com"}).Validate(); err != nil {
		t.Errorf("a valid section: %v", err)
	}
}

// ---- external mode wired end to end (X-26) ----

// Mode settings are the operator's: a stranger cannot write one at any level, a tenant administrator writes the
// tenant level only, a deployment operator writes all three. What was written survives a restart, and the other
// writes of the area (budgets, the usage of the whole application, the kill switch) hold the same line in the
// service itself, not only in the route.
func TestModeSettingsPersistAndRequireAnOperator(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "external")
	var spy *externalSpyAudit
	a := externalTestAdmin(t, dir, &spy)
	ctx := context.Background()
	stranger, tenantAdmin, ops := externalTestScope("ten_acme", "usr_nobody"), externalTestScope("ten_acme", "usr_admin"), externalTestScope("ten_acme", "usr_ops")
	levels := []gateway.ProvidersModeLevel{gateway.ProvidersModeDeployment, gateway.ProvidersModeApplication, gateway.ProvidersModeTenant}
	denied := func(what string, err error) {
		t.Helper()
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: %v, want permission_denied", what, err)
		}
	}

	for _, lv := range levels {
		_, err := a.SetMode(ctx, stranger, lv, pv.DeploymentModeExternal)
		denied("a stranger at "+string(lv), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "modes.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write left a mode file: %v", err)
	}
	if n := spy.count("providers.mode_changed"); n != 0 {
		t.Fatalf("%d mode changes were recorded for refused writes", n)
	}

	if _, err := a.SetMode(ctx, tenantAdmin, gateway.ProvidersModeTenant, pv.DeploymentModeHybrid); err != nil {
		t.Fatalf("a tenant administrator at the tenant level: %v", err)
	}
	for _, lv := range []gateway.ProvidersModeLevel{gateway.ProvidersModeDeployment, gateway.ProvidersModeApplication} {
		_, err := a.SetMode(ctx, tenantAdmin, lv, pv.DeploymentModeExternal)
		denied("a tenant administrator at "+string(lv), err)
	}
	// An operator of another deployment is nobody.
	foreign := ops
	foreign.DeploymentID = "dep_other"
	_, err := a.SetMode(ctx, foreign, gateway.ProvidersModeTenant, pv.DeploymentModeExternal)
	denied("an operator of another deployment", err)
	for _, lv := range levels {
		if _, err := a.SetMode(ctx, ops, lv, pv.DeploymentModeExternal); err != nil {
			t.Fatalf("a deployment operator at %s: %v", lv, err)
		}
	}
	if n := spy.count("providers.mode_changed"); n != 4 {
		t.Errorf("%d mode changes recorded, want 4 (one tenant, three operator)", n)
	}

	// A restart reads what was written, level by level, and nothing else.
	f, err := openExternalModeFile(filepath.Join(dir, "modes.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
		if m, set, err := f.Mode(ctx, lv, ops); err != nil || !set || m != pv.DeploymentModeExternal {
			t.Errorf("after a restart %s = %v %v %v", lv, m, set, err)
		}
	}
	if _, set, _ := f.Mode(ctx, modes.LevelTenant, externalTestScope("ten_other", "usr_x")); set {
		t.Error("another tenant reads a mode it never set")
	}

	// The same line is held by the other writes of the area.
	_, err = a.PutBudget(ctx, stranger, pv.Budget{})
	denied("PutBudget by a stranger", err)
	_, err = a.PutBudget(ctx, tenantAdmin, pv.Budget{Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider}})
	denied("a provider-wide budget by a tenant administrator", err)
	_, err = a.Budgets(ctx, stranger)
	denied("Budgets by a stranger", err)
	_, err = a.Usage(ctx, stranger, gateway.ProvidersUsageQuery{})
	denied("Usage by a stranger", err)
	_, err = a.Usage(ctx, tenantAdmin, gateway.ProvidersUsageQuery{Wide: true})
	denied("the usage of the whole application by a tenant administrator", err)
	_, err = a.SetKillSwitch(ctx, tenantAdmin, true, "provider_incident")
	denied("the kill switch by a tenant administrator", err)
	_, err = a.PutEgressPolicy(ctx, tenantAdmin, gateway.ProvidersEgressPolicyInput{Level: pv.EgressLevelApplication})
	denied("the application egress policy by a tenant administrator", err)
	if ks, err := a.KillSwitch(ctx, stranger); err != nil || ks.Engaged {
		t.Errorf("a refused kill switch engaged it: %+v %v", ks, err)
	}
}

// A tenant administrator reads and writes the budgets of its own tenant only: another tenant's budget is not
// listed, cannot be changed and cannot be created; a deployment operator still reads and writes every tenant's.
func TestSecProvidersExternalBudgetsStayInTheTenant(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	admin, ops := externalTestScope("ten_acme", "usr_admin"), externalTestScope("ten_acme", "usr_ops")
	budget := func(tenant string, daily int64) pv.Budget {
		return pv.Budget{
			Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindTenant, ApplicationID: new(common.Identifier("app_demo")), TenantID: new(common.Identifier(tenant))},
			Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
			Limits: pv.BudgetLimits{DailyMicroUnits: pv.MicroUnits(daily), MonthlyMicroUnits: 10_000_000},
		}
	}
	other, err := a.PutBudget(ctx, ops, budget("ten_other", 1_000_000))
	if err != nil {
		t.Fatalf("a deployment operator writes another tenant's budget: %v", err)
	}
	own, err := a.PutBudget(ctx, admin, budget("ten_acme", 1_000_000))
	if err != nil {
		t.Fatalf("a tenant administrator writes its own tenant's budget: %v", err)
	}
	ids := func(v gateway.ProvidersBudgetsView) map[pv.BudgetID]bool {
		got := map[pv.BudgetID]bool{}
		for _, b := range v.Budgets {
			got[b.BudgetID] = true
		}
		for _, s := range v.States {
			got[s.BudgetID] = true
		}
		return got
	}

	v, err := a.Budgets(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(v); got[other.BudgetID] || !got[own.BudgetID] {
		t.Errorf("a tenant administrator lists %v, want its own budget and not the other tenant's", got)
	}

	refused := func(what string, err error) {
		t.Helper()
		if code := contracts.CodeOf(err); code != contracts.ErrScopeViolation && code != contracts.ErrPermissionDenied {
			t.Errorf("%s: %v, want a refusal", what, err)
		}
	}
	upd := other
	upd.Limits.DailyMicroUnits = 0
	_, err = a.PutBudget(ctx, admin, upd)
	refused("changing another tenant's budget", err)
	_, err = a.PutBudget(ctx, admin, budget("ten_third", 0))
	refused("creating a budget for another tenant", err)

	v, err = a.Budgets(ctx, ops)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(v); !got[other.BudgetID] || !got[own.BudgetID] || len(v.Budgets) != 2 {
		t.Errorf("a deployment operator lists %v, want both budgets and nothing more", got)
	}
	for _, b := range v.Budgets {
		if b.BudgetID == other.BudgetID && (b.Revision != 1 || b.Limits.DailyMicroUnits != 1_000_000) {
			t.Errorf("the other tenant's budget was changed: %+v", b)
		}
	}
	if _, err := a.PutBudget(ctx, ops, upd); err != nil {
		t.Errorf("a deployment operator changes another tenant's budget: %v", err)
	}
}

// providersBudgetsTickingClock moves a minute every time it is read, so two reads never see one instant.
type providersBudgetsTickingClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *providersBudgetsTickingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Minute)
	return c.t
}

// The budgets view is one read: every state in it is of one snapshot at one instant, in the order of the
// budgets, however many budgets it holds.
func TestProvidersBudgetsViewIsOneRead(t *testing.T) {
	db := storetest.Open(t, nil)
	log, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := newExternalAdmin(externalAdminDeps{
		DB: db, Audit: log, Clock: &providersBudgetsTickingClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)},
		DeploymentID: externalTestDeployment, Dir: filepath.Join(t.TempDir(), "external"), Validator: externalAcceptKeys{},
		Section: externalConfig{Enabled: true, DeploymentOperators: []string{"app_demo/usr_ops"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ops := externalTestScope("ten_acme", "usr_ops")
	for _, p := range []string{"anthropic", "openai", "gemini"} {
		if _, err := a.PutBudget(ctx, ops, pv.Budget{
			Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider, ProviderID: new(pv.ProviderID(p))},
			Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
			Limits: pv.BudgetLimits{DailyMicroUnits: 1_000_000, MonthlyMicroUnits: 10_000_000},
		}); err != nil {
			t.Fatal(err)
		}
	}
	v, err := a.Budgets(ctx, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Budgets) != 3 || len(v.States) != 3 {
		t.Fatalf("the view holds %d budgets and %d states, want 3 of each", len(v.Budgets), len(v.States))
	}
	for i, st := range v.States {
		if st.BudgetID != v.Budgets[i].BudgetID || st.AsOf != v.States[0].AsOf {
			t.Errorf("state %d is of %s as of %s; want %s as of %s", i, st.BudgetID, st.AsOf, v.Budgets[i].BudgetID, v.States[0].AsOf)
		}
	}
}

// The tenant usage export (the default scope of /v1/providers/usage/export) is the usage of the caller's tenant:
// every workspace of it, and nothing of another tenant.
func TestProvidersUsageTenantExportCoversTheTenant(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	day := time.Now().UTC().Format("2006-01-02")
	if _, err := a.budget.PutBudget(ctx, externalTestScope("ten_acme", "usr_ops"), pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider, ProviderID: new(pv.ProviderID("anthropic"))},
		Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: 1_000_000, MonthlyMicroUnits: 10_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	call := func(sc contracts.Scope) {
		t.Helper()
		r, err := a.budget.Reserve(ctx, sc, providerbudget.Call{
			Provider: pv.ProviderRef{ProfileID: pv.ProfileID(ids.New("pprf")), ProviderID: "anthropic", ModelID: "claude-haiku-4-5", ModelVersion: "2026-10-01"},
			Role:     pv.RoleConversationalController, Channel: pv.ChannelInteractive, EnvelopeID: pv.EnvelopeID(ids.New("envl")), DecisionID: pv.EgressDecisionID(ids.New("edec")),
			Prices: pv.PriceTable{Currency: "USD", EffectiveOn: day, Prices: pv.PriceTablePrices{ToksOut: &pv.PriceTablePrice{PerUnits: 1000, MicroUnits: 2000}}},
			Units:  providerbudget.Units{ToksOut: 100},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.budget.Settle(ctx, sc, r.ID, providerbudget.Actual{Outcome: pv.UsageRecordOutcomeSucceeded, Metering: providerbudget.Units{ToksOut: 100}}); err != nil {
			t.Fatal(err)
		}
	}
	side := externalTestScope("ten_acme", "usr_y")
	side.WorkspaceID = "wsp_side"
	call(externalTestScope("ten_acme", "usr_x"))
	call(side)
	call(externalTestScope("ten_other", "usr_z"))

	res, err := a.Usage(ctx, externalTestScope("ten_acme", "usr_admin"), gateway.ProvidersUsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var exp providerbudget.UsageExport
	if err := json.Unmarshal(res.Body, &exp); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range exp.Rows {
		got[string(r.Record.Subject.TenantID)+"/"+string(r.Record.Subject.WorkspaceID)]++
	}
	if len(exp.Rows) != 2 || got["ten_acme/wsp_main"] != 1 || got["ten_acme/wsp_side"] != 1 {
		t.Errorf("the tenant export holds %v, want one call of each workspace of ten_acme and nothing else", got)
	}
}

// The Anthropic adapter is built beside OpenAI's: it adds its own origin to the destination registry, never widens
// an entry the operator set, sends nothing while it is built and needs a key source.
func TestExternalProviderAnthropicBuildsAndAddsItsDestination(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	reg := egress.NewProviderRegistry()
	spec := ExternalProviderSpec{
		Provider:  ExternalProviderAnthropic,
		Anthropic: anthropic.Config{BaseURL: srv.URL, CredentialRef: "cred_01TESTREFERENCE", Model: "claude-haiku-4-5"}, // TEST-ONLY opaque reference id, not a key
		Egress:    reg, Keys: externalProviderTestKeys{},
	}
	p, err := NewExternalProvider(spec)
	if err != nil {
		t.Fatal(err)
	}
	if hits != 0 || p.Name() != "anthropic" || reg.Allowlist("anthropic") == nil || reg.Allowlist("anthropic").Len() != 1 {
		t.Fatalf("hits %d, name %q, allowlist %v", hits, p.Name(), reg.Allowlist("anthropic"))
	}
	other, _ := egress.NewAllowlist(egress.Destination{Host: "example.com", Ports: []int{443}, Scheme: "https"})
	reg2 := egress.NewProviderRegistry()
	_ = reg2.Set("anthropic", other)
	spec.Egress = reg2
	if _, err := NewExternalProvider(spec); err != nil {
		t.Fatal(err)
	}
	if got := reg2.Allowlist("anthropic").Destinations(); len(got) != 1 || got[0].Host != "example.com" {
		t.Fatalf("the operator's allowlist was changed: %+v", got)
	}
	spec.Keys = nil
	if _, err := NewExternalProvider(spec); err == nil {
		t.Error("built without a key source")
	}
	spec.Keys, spec.Egress = externalProviderTestKeys{}, nil
	if _, err := NewExternalProvider(spec); err == nil {
		t.Error("built without a destination registry")
	}
}

// The text gate asks the data-egress guard, with the registry profile of the target: with no egress policy the
// guard refuses (permission_denied, a rule code, nothing about the text), an unknown purpose and a missing guard
// refuse too, and the candidate it builds is one the guard accepts as well formed.
func TestExternalTextEgressAsksTheGuard(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	scope := externalTestScope("ten_acme", "usr_admin")
	draft, err := extWireDraft(ExternalProviderAnthropic, "", "app_demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft.LastPolicyReview, draft.Terms.RecordedOn = time.Now().UTC().Format("2006-01-02"), time.Now().UTC().Format("2006-01-02")
	prof, err := a.reg.Register(ctx, externalTestScope("ten_acme", "usr_ops"), draft)
	if err != nil {
		t.Fatal(err)
	}
	target := extmodal.Target{Profile: prof}
	gate := extWireTextEgress{guard: a.egress}
	for _, p := range []extmodal.Purpose{extmodal.PurposeSpeech, extmodal.PurposeVisionAsk, extmodal.PurposeImagePrompt, extmodal.PurposeExcerpt} {
		_, err := gate.AllowText(ctx, scope, target, p, "ciao")
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied || strings.Contains(err.Error(), "ciao") {
			t.Errorf("%s with no policy: %v", p, err)
		}
	}
	if _, err := gate.AllowText(ctx, scope, target, "other", "ciao"); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("an unknown purpose: %v", err)
	}
	if _, err := (extWireTextEgress{}).AllowText(ctx, scope, target, extmodal.PurposeSpeech, "ciao"); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("no guard: %v", err)
	}
}

// externalTextTestGuard allows every candidate with the result it holds.
type externalTextTestGuard struct{ res dataegress.Result }

func (g externalTextTestGuard) Evaluate(context.Context, dataegress.Candidate) (dataegress.Result, error) {
	return g.res, nil
}

// The caller of the text gate sends the text it passed, as it is: a text the guard allowed only after scrubbing,
// cutting or dropping it is refused, so the raw value can never leave.
func TestExternalTextEgressRefusesTextTheGuardWouldChange(t *testing.T) {
	ctx := context.Background()
	scope := externalTestScope("ten_acme", "usr_admin")
	env := &pv.RequestEnvelope{EnvelopeID: pv.EnvelopeID(ids.New("envl"))}
	for name, tc := range map[string]struct {
		res   dataegress.Result
		allow bool
	}{
		"kept as it is": {dataegress.Result{Envelope: env, Decision: pv.EgressDecision{DecisionID: pv.EgressDecisionID(ids.New("edec"))}, Parts: []dataegress.PartOutcome{{Kept: true}}}, true},
		"scrubbed":      {dataegress.Result{Envelope: env, Parts: []dataegress.PartOutcome{{Kept: true, Why: "scrubbed"}}}, false},
		"truncated":     {dataegress.Result{Envelope: env, Parts: []dataegress.PartOutcome{{Kept: true, Why: "truncated"}}}, false},
		"dropped":       {dataegress.Result{Envelope: env, Parts: []dataegress.PartOutcome{{Kept: false, Why: "empty_content"}}}, false},
		"no envelope":   {dataegress.Result{Parts: []dataegress.PartOutcome{{Kept: true}}}, false},
	} {
		d, err := extWireTextEgress{guard: externalTextTestGuard{tc.res}}.AllowText(ctx, scope, extmodal.Target{}, extmodal.PurposeSpeech, "ciao")
		if tc.allow != (err == nil) || (err != nil && contracts.CodeOf(err) != contracts.ErrPermissionDenied) {
			t.Errorf("%s: %v", name, err)
		}
		// An allowed text names the envelope and the decision its call's budget reservation belongs to.
		if tc.allow && (d.EnvelopeID != env.EnvelopeID || d.DecisionID != tc.res.Decision.DecisionID) {
			t.Errorf("%s: decision %+v", name, d)
		}
	}
}

// A hosted-Llama profile whose registry model id is mapped to a wire id (meta_models): a reply that names the wire
// id is the pinned model, so the router keeps it; a reply that names another model is still reported as it came.
func TestExternalMetaReplyOfTheMappedModelMatchesTheProfile(t *testing.T) {
	const wire = "meta-llama/Llama-3.3-70B-Instruct"
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	scope := externalTestScope("ten_acme", "usr_admin")
	echo := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		model := body.Model
		if echo != "" {
			model = echo
		}
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n", model)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n", model)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, model)
	}))
	defer srv.Close()
	ref, err := a.creds.AddAPIKey(ctx, scope, credentials.AddAPIKeyRequest{ProviderID: "meta", Key: "fixture-credential-value-0001"})
	if err != nil {
		t.Fatal(err)
	}
	sender := &extWireSender{
		admin: a, reg: egress.NewProviderRegistry(), cache: map[string]ExternalProvider{},
		sec: externalConfig{MetaDialect: "openai-compatible", MetaBaseURL: srv.URL, MetaModels: map[string]string{"llama-3-3-70b": wire}},
	}
	refID := pv.CredentialRefID(ref.RefID)
	prof := pv.ProviderProfile{ProfileID: "pprf_01", ProviderID: "meta", ModelID: "llama-3-3-70b", ModelVersion: "2026-10-01", CredRef: &refID}
	gen, err := sender.generatorFor(prof)
	if err != nil {
		t.Fatal(err)
	}
	req := providers.RichRequest{Messages: []providers.RichMessage{
		{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart("You answer.")}},
		{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("hi")}},
	}}
	reply := func() []string {
		t.Helper()
		res, err := gen.Generate(ctx, scope, req)
		if err != nil {
			t.Fatal(err)
		}
		st, err := gen.GenerateStream(ctx, scope, req)
		if err != nil {
			t.Fatal(err)
		}
		sres, err := providers.CollectRich(st, req)
		if err != nil {
			t.Fatal(err)
		}
		return []string{res.Model, sres.Model}
	}
	for _, got := range reply() {
		if !providerreg.ReplyModelMatches(prof.ModelID, prof.ModelVersion, got) {
			t.Errorf("a reply of the mapped model reports %q, which does not match the profile", got)
		}
	}
	echo = "meta-llama/Llama-3.1-8B-Instruct"
	for _, got := range reply() {
		if got != echo {
			t.Errorf("a reply of another model reports %q, want %q", got, echo)
		}
	}
}

type extWireTestLocal struct{}

func (extWireTestLocal) Select(context.Context, contracts.Scope, providers.Role) (dialogue.Provider, error) {
	return dialogue.Provider{Name: "local", Local: true}, nil
}

func extWireTestPackages() dialogue.PackageSource {
	return dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) {
		return nil, contracts.New(contracts.ErrUnavailable, "no package in this test")
	})
}

// The revalidation job runs on the one queue and pool of the RAG stack under the service identity, and the pool
// accepts it for that identity, that job type and that service scope only. A review that has expired is
// quarantined by the job without a request arriving first.
func TestRevalidationJobIsScheduledWithTheServiceIdentity(t *testing.T) {
	extWireSaveSection(t)
	externalSection = externalConfig{Enabled: true, DeploymentOperators: []string{"app_demo/usr_ops"}}
	f := newRagFixture(t)
	a, err := newExternalAdmin(externalAdminDeps{
		DB: f.db, Audit: f.aud, Clock: f.clk, DeploymentID: externalTestDeployment, Dir: filepath.Join(t.TempDir(), "external"),
		Validator: externalAcceptKeys{}, Section: externalSection,
	})
	if err != nil {
		t.Fatal(err)
	}
	services := gateway.NewServices()
	for _, perr := range []error{gateway.Provide(services, f.stack), gateway.Provide[gateway.ProvidersService](services, a)} {
		if perr != nil {
			t.Fatal(perr)
		}
	}
	del := &extWireDelegate{local: extWireTestLocal{}, guard: dialogue.LocalOnlyGuard{}, packages: extWireTestPackages(), budget: providers.DefaultBudget()}
	var start func(context.Context) func()
	var built *extWireStack
	oldCompose := extWireOnCompose
	extWireOnCompose = func(s *extWireStack) { built = s }
	t.Cleanup(func() { extWireOnCompose = oldCompose })
	err = extWireProvide(context.Background(), serveDeps{
		Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, Audit: f.aud, Services: services, External: del,
		AddService: func(s func(context.Context) func()) { start = s },
	})
	if err != nil {
		t.Fatal(err)
	}
	if built == nil || start == nil || del.rt.Load() == nil {
		t.Fatalf("the provider gave no stack (%t), no loop (%t) or no runtime (%t)", built != nil, start != nil, del.rt.Load() != nil)
	}
	if got, err := del.Select(context.Background(), externalTestScope("ten_acme", "usr_x"), providers.RoleController); err != nil || !got.Local {
		t.Fatalf("Local mode did not select the local provider: %+v, %v", got, err)
	}

	// An application with a live profile gets a job under the service identity.
	day := f.clk.Now().UTC().Format("2006-01-02")
	draft, err := extWireDraft(ExternalProviderAnthropic, "", "app_demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft.LastPolicyReview, draft.Terms.RecordedOn = day, day
	prof, err := a.reg.Register(context.Background(), externalTestScope("ten_acme", "usr_ops"), draft)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := start(ctx)
	defer func() { cancel(); stop() }()
	count := func(where string, args ...any) int {
		return f.countRows(`SELECT count(*) FROM jobs WHERE type = ? AND class = ? AND service_identity = ? `+where,
			append([]any{providerreg.JobRevalidate, int(jobs.ClassMaintenance), providerreg.ServiceIdentity}, args...)...)
	}
	deadline := time.Now().Add(15 * time.Second)
	for count("") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the schedule queued no revalidation job")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.drain()
	if n := count("AND state = ?", "succeeded"); n != 1 {
		t.Fatalf("%d jobs succeeded, want 1\n%s", n, f.dumpJobs())
	}
	if got, _ := a.reg.Get(context.Background(), externalTestScope("ten_acme", "usr_ops"), prof.ProfileID); got.Lifecycle != pv.ProfileLifecycleCandidate {
		t.Fatalf("a fresh review was touched: %s", got.Lifecycle)
	}

	// The review expires; the next period's job quarantines the profile with no request in between.
	f.clk.Advance(40 * 24 * time.Hour)
	if _, err := a.reg.ScheduleRevalidation(context.Background(), built.queue, externalTestDeployment, built.every); err != nil {
		t.Fatal(err)
	}
	f.drain()
	got, _ := a.reg.Get(context.Background(), externalTestScope("ten_acme", "usr_ops"), prof.ProfileID)
	if got.Lifecycle != pv.ProfileLifecycleQuarantined || got.QuarantineReason == nil || *got.QuarantineReason != pv.ProviderProfileQuarantineReasonStaleReview {
		t.Fatalf("after the review expired: %s %v", got.Lifecycle, got.QuarantineReason)
	}

	// What the pool accepts for the identity.
	auth := extWireRevalidateAuthority{deployment: externalTestDeployment}
	svc := contracts.Scope{
		DeploymentID: externalTestDeployment, ApplicationID: "app_demo", TenantID: extWireJobTenant, WorkspaceID: extWireJobTenant,
		PrincipalID: extWireJobPrincipal, Roles: []string{extWireJobRole}, CapabilityScopes: []string{extWireJobCapability}, SessionID: "ses_providerreg_revalidate",
	}
	job := func(mutate func(*jobs.Job)) jobs.Job {
		j := jobs.Job{Type: providerreg.JobRevalidate, Scope: svc, ServiceIdentity: providerreg.ServiceIdentity}
		if mutate != nil {
			mutate(&j)
		}
		return j
	}
	if err := auth.AuthorizeService(context.Background(), providerreg.ServiceIdentity, job(nil)); err != nil {
		t.Fatalf("the job the schedule queues is denied: %v", err)
	}
	for name, tc := range map[string]struct {
		identity string
		mutate   func(*jobs.Job)
	}{
		"another identity":        {"rag-ingest", nil},
		"another job type":        {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Type = "history.retention" }},
		"a payload":               {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Payload = json.RawMessage(`{"all":true}`) }},
		"another deployment":      {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Scope.DeploymentID = "dep_other" }},
		"a tenant scope":          {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Scope.TenantID = "ten_acme" }},
		"a user principal":        {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Scope.PrincipalID = "usr_admin" }},
		"another role":            {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Scope.Roles = []string{"admin"} }},
		"wider capability scopes": {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Scope.CapabilityScopes = append(j.Scope.CapabilityScopes, "items.delete") }},
		"no scope":                {providerreg.ServiceIdentity, func(j *jobs.Job) { j.Scope = contracts.Scope{} }},
	} {
		if err := auth.AuthorizeService(context.Background(), tc.identity, job(tc.mutate)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: %v, want permission_denied", name, err)
		}
	}
	if err := (extWireRevalidateAuthority{}).AuthorizeService(context.Background(), providerreg.ServiceIdentity, job(nil)); err == nil {
		t.Error("an authority with no deployment accepted a job")
	}
}

// Serve builds the external-mode runtime only when the section enables it and the binary carries the adapters:
// off, the engine keeps the local selector and guard, no hook is given a stack, and no file is created; on, the
// engine is built with the delegate, the runtime sits behind it, and the data directory holds the external
// directory.
func TestServeWiresExternalModeOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			extWireSaveSection(t)
			externalSection = externalConfig{Enabled: enabled, DeploymentOperators: []string{"app_demo/usr_ops"}}
			f := newFixture(t)
			t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
			cfg, dataDir := serveConfig(t, f, identityYAML)

			var handle any
			seamsCalls := 0
			stacks := make(chan *extWireStack, 1)
			oldSeams, oldCompose := extWireEngineSeams, extWireOnCompose
			extWireEngineSeams = func(in extWireSeamsIn) extWireSeamsOut {
				out := oldSeams(in)
				seamsCalls++
				handle = out.Handle
				return out
			}
			extWireOnCompose = func(s *extWireStack) { stacks <- s }
			t.Cleanup(func() { extWireEngineSeams, extWireOnCompose = oldSeams, oldCompose })

			srv := startServe(t, cfg)
			if seamsCalls != 1 {
				t.Fatalf("the engine was built with %d calls to the seam, want 1", seamsCalls)
			}
			if !enabled {
				select {
				case <-stacks:
					t.Fatal("a stack was built with external mode off")
				default:
				}
				if handle != nil {
					t.Errorf("the engine was given a delegate with external mode off: %T", handle)
				}
				if _, err := os.Stat(filepath.Join(dataDir, "external")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("external mode off created %s: %v", filepath.Join(dataDir, "external"), err)
				}
				if err := srv.stop(); err != nil {
					t.Fatalf("serve stopped with %v", err)
				}
				return
			}
			var st *extWireStack
			select {
			case st = <-stacks:
			default:
				t.Fatal("serve built no external-mode stack")
			}
			del, ok := handle.(*extWireDelegate)
			if !ok || del.rt.Load() != st.runtime || st.hybrid == nil || st.queue == nil {
				t.Fatalf("handle %T, runtime behind the delegate %t, hybrid %t, queue %t", handle, ok && del.rt.Load() == st.runtime, st.hybrid != nil, st.queue != nil)
			}
			if h := del.hy.Load(); h == nil || h.h != extGapHybridRealiser(st.hybrid) || h.routed {
				t.Errorf("the delegate was not given the hybrid realisation, or one that routes with the default policy: %+v", h)
			}
			if st.policy == nil || len(st.policy) != 0 {
				t.Errorf("the default routing policy is not empty: %v", st.policy)
			}
			if fi, err := os.Stat(filepath.Join(dataDir, "external")); err != nil || !fi.IsDir() {
				t.Errorf("the external directory: %v", err)
			}
			if err := srv.stop(); err != nil {
				t.Fatalf("serve stopped with %v", err)
			}
		})
	}
}

// The Gemini and Meta adapters are built beside the others, each adding only its own origin, and sending nothing.
func TestExternalProviderGeminiAndMetaBuild(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	const ref = "cred_01TESTREFERENCE" // TEST-ONLY opaque reference id, not a key
	for name, spec := range map[string]ExternalProviderSpec{
		"gemini": {Provider: ExternalProviderGemini, Gemini: gemini.Config{BaseURL: srv.URL, CredentialRef: ref, Model: "gemini-2.5-flash"}},
		"meta":   {Provider: ExternalProviderMeta, Meta: meta.Config{BaseURL: srv.URL, CredentialRef: ref, Model: "llama-4-maverick"}},
		"meta hosted llama": {Provider: ExternalProviderMeta, Meta: meta.Config{
			Dialect: meta.DialectOpenAICompat, BaseURL: srv.URL, CredentialRef: ref, Model: "meta-llama/Llama-4-Maverick"}},
	} {
		reg := egress.NewProviderRegistry()
		spec.Egress, spec.Keys = reg, externalProviderTestKeys{}
		p, err := NewExternalProvider(spec)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if reg.Allowlist(p.Name()) == nil || reg.Allowlist(p.Name()).Len() != 1 {
			t.Errorf("%s: allowlist %v", name, reg.Allowlist(p.Name()))
		}
		spec.Keys = nil
		if _, err := NewExternalProvider(spec); err == nil {
			t.Errorf("%s: built without a key source", name)
		}
	}
	if hits != 0 {
		t.Fatalf("building sent %d requests", hits)
	}
	if _, err := NewExternalProvider(ExternalProviderSpec{Provider: ExternalProviderMeta, Egress: egress.NewProviderRegistry(), Keys: externalProviderTestKeys{},
		Meta: meta.Config{Dialect: meta.DialectOpenAICompat, CredentialRef: ref, Model: "x"}}); err == nil {
		t.Error("a hosted Llama endpoint was built with no origin")
	}
}

// ---- X-27: the response seam, external RAG and the budget sweep ----

const extGapClinicPackage = "../../examples/reference-apps/clinic-booking/package"

type extGapActivePackage struct{ capabilities.Package }

func (extGapActivePackage) Status() policy.PackageStatus { return policy.StatusActive }

// extGapSpyRealiser stands for hybrid.Hybrid: it records what the response step handed it and answers with the
// deterministic template, which is what Hybrid itself answers when nothing may leave.
type extGapSpyRealiser struct {
	mu    sync.Mutex
	turns []hybrid.Turn
}

var _ extGapHybridRealiser = (*extGapSpyRealiser)(nil)

func (s *extGapSpyRealiser) Realise(_ context.Context, t hybrid.Turn) (hybrid.Realised, error) {
	s.mu.Lock()
	s.turns = append(s.turns, t)
	s.mu.Unlock()
	o, err := verify.Degraded(t.Directive, t.Receipt, t.Options)
	if err != nil {
		return hybrid.Realised{}, err
	}
	return hybrid.Realised{Text: o.Text, Source: o.Source, Reason: o.Reason, Realiser: hybrid.RealiserTemplate}, nil
}

func (s *extGapSpyRealiser) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.turns)
}

// The response step hands the directive to the realiser of the engine's provider selector: in Hybrid mode, when the
// deployment routes the realisation role, the local model is not asked to write the reply and Hybrid is; in Local mode,
// in External mode and in a Hybrid deployment that routes nothing the delegate declines and the engine writes the reply
// itself, exactly as before.
func TestHybridRealiseIsCalledFromTheResponseStep(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	db := storetest.Open(t, nil)
	alog, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	gw := policy.NewGateway(clk)
	host := conv.NewClinicHost(clk)
	scope := conv.RoleScope(conv.RoleReceptionist)
	guard, err := adapters.Wrap(host.Adapter(), adapters.Options{ApplicationID: scope.ApplicationID})
	if err != nil {
		t.Fatal(err)
	}
	res, err := apppkg.Load(ctx, apppkg.FromDir(extGapClinicPackage), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil {
		t.Fatalf("load package: %v %v", err, res.Findings)
	}
	pkg := extGapActivePackage{capabilities.FromApppkg(res.Package)}
	reg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
	if err := reg.Activate(pkg); err != nil {
		t.Fatal(err)
	}
	adapterSrc := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })
	co, err := actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: alog, Adapters: adapterSrc, Clock: clk,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pkg, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(db, clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eg, err := dataegress.New(dataegress.Deps{DB: db, Clock: clk, Audit: alog})
	if err != nil {
		t.Fatal(err)
	}
	memModes := modes.NewMemoryModes()
	resolver, err := modes.NewResolver(memModes, eg)
	if err != nil {
		t.Fatal(err)
	}
	setMode := func(m modes.Mode) {
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			if err := memModes.Set(lv, scope, m); err != nil {
				t.Fatal(err)
			}
		}
	}

	local := pfake.New(pfake.WithClock(clk), pfake.WithName("local"))
	budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
	sel, err := dialogue.LocalSelector(local, budget)
	if err != nil {
		t.Fatal(err)
	}
	pkgs := dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return pkg, nil })
	del := &extWireDelegate{local: sel, guard: dialogue.LocalOnlyGuard{}, packages: pkgs, budget: budget}
	spy := &extGapSpyRealiser{}
	eng, err := dialogue.New(dialogue.Deps{
		Sessions: sess, Registry: reg, Packages: pkgs, Policy: gw, Actions: co, Adapters: adapterSrc, Providers: del, Guard: del, Audit: alog, Clock: clk,
		Config: dialogue.Config{TemplateOnly: []conversation.DialogueDirectiveAct{conversation.DialogueDirectiveActRefuse}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// say runs one turn whose reply is written by a model: the local one proposes the listing, then (if asked) replies.
	say := func() dialogue.Result {
		t.Helper()
		local.Reset()
		local.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(
			`{"capability_id":"availability.list","fields":{"date":"2026-10-14"},"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9}}`)
		local.On(pfake.Constrained(providers.ConstraintNone)).Reply("Here is what is free.")
		r, err := eng.Turn(ctx, scope, dialogue.TurnInput{Text: "Which appointments are free on 2026-10-14?", Language: "en"})
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}
		if string(r.Directive.Act) != "present_result" {
			t.Fatalf("act = %s (code %q, response %q), want present_result", r.Directive.Act, r.Code, r.Response.Text)
		}
		return r
	}
	responsePrompts := func() int {
		n := 0
		for _, p := range local.Prompts() {
			if p.Chat.Output.Kind() == providers.ConstraintNone {
				n++
			}
		}
		return n
	}

	// With no Hybrid behind the delegate (external mode off), nothing is declined to: the engine's path.
	setMode(modes.ModeHybrid)
	say()
	if responsePrompts() != 1 || spy.count() != 0 {
		t.Fatalf("no hybrid wired: %d local response prompts, %d realiser calls", responsePrompts(), spy.count())
	}

	for name, tc := range map[string]struct {
		mode   modes.Mode
		routed bool
	}{
		"local mode":                      {modes.ModeLocal, true},
		"external mode":                   {modes.ModeExternal, true},
		"hybrid mode that routes nothing": {modes.ModeHybrid, false},
	} {
		setMode(tc.mode)
		del.hy.Store(&extGapHybrid{h: spy, modes: resolver, routed: tc.routed})
		say()
		if responsePrompts() != 1 || spy.count() != 0 {
			t.Fatalf("%s: %d local response prompts, %d realiser calls; the engine's own path must run", name, responsePrompts(), spy.count())
		}
	}

	// Hybrid mode with the realisation role routed: the directive goes to Hybrid, the local model writes no reply.
	setMode(modes.ModeHybrid)
	del.hy.Store(&extGapHybrid{h: spy, modes: resolver, routed: true})
	r := say()
	if spy.count() != 1 || responsePrompts() != 0 {
		t.Fatalf("hybrid mode: %d realiser calls, %d local response prompts", spy.count(), responsePrompts())
	}
	got := spy.turns[0]
	if got.Directive == nil || got.Directive.Act != r.Directive.Act || got.Scope.TenantID != scope.TenantID || got.Scope.PrincipalID != scope.PrincipalID {
		t.Fatalf("hybrid was given %+v", got)
	}
	if r.Response.Source != verify.SourceTemplate || r.Response.Text == "" {
		t.Fatalf("the response was %+v, want the template Hybrid answered", r.Response)
	}
	if last := r.Trace.ModelCalls[len(r.Trace.ModelCalls)-1]; last.Provider != "realiser" || last.Purpose != "response" {
		t.Fatalf("trace %+v", last)
	}

	// The kill switch is the resolver's: with it engaged the mode is local and the delegate declines.
	if err := eg.EngageKillSwitch(ctx, dataegress.Operator{DeploymentID: scope.DeploymentID, PrincipalID: "usr_ops"}, "test"); err != nil {
		t.Fatal(err)
	}
	say()
	if spy.count() != 1 || responsePrompts() != 1 {
		t.Fatalf("kill switch engaged: %d realiser calls, %d local response prompts", spy.count(), responsePrompts())
	}
}

// ---- external RAG ----

type extGapRetriever struct {
	mu  sync.Mutex
	pkg rag.EvidencePackage
}

func (f *extGapRetriever) Query(context.Context, contracts.Scope, query.Request) (query.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return query.Result{Package: f.pkg}, nil
}

func (f *extGapRetriever) Config() query.Config { return query.DefaultConfig() }

type extGapCatalog struct {
	mu     sync.Mutex
	states map[string]query.SourceState
}

func (c *extGapCatalog) Sources(_ context.Context, sc contracts.Scope, ids []string) (map[string]query.SourceState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]query.SourceState{}
	for _, id := range ids {
		if st, ok := c.states[sc.TenantID+"/"+id]; ok {
			out[id] = st
		}
	}
	return out, nil
}

type extGapProfiles struct{ prof pv.ProviderProfile }

func (f extGapProfiles) Get(context.Context, contracts.Scope, pv.ProfileID) (pv.ProviderProfile, error) {
	return f.prof, nil
}

func (f extGapProfiles) Check(context.Context, contracts.Scope, pv.ProfileID, providerreg.Requirement) (providerreg.Admission, error) {
	return providerreg.Admission{Profile: f.prof}, nil
}

func (f extGapProfiles) Fallback(context.Context, contracts.Scope, pv.ProfileID, providerreg.FallbackCondition, providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrPermissionDenied, "no fallback named")
}

type extGapBudgets struct{}

func (extGapBudgets) Reserve(context.Context, contracts.Scope, providerbudget.Call) (providerbudget.Reservation, error) {
	return providerbudget.Reservation{ID: "rsv_01J0000000000000000000000R"}, nil
}

func (extGapBudgets) Settle(context.Context, contracts.Scope, pv.ReservationID, providerbudget.Actual) (providerbudget.Settlement, error) {
	return providerbudget.Settlement{}, nil
}

func (extGapBudgets) States(context.Context, contracts.Scope, pv.ProviderID, pv.Channel) ([]pv.BudgetState, error) {
	return nil, nil
}

type extGapLocalUsable struct{}

func (extGapLocalUsable) Usable(modes.Role) bool { return true }

// extGapSender hands the filtered request to the scripted external provider and records it.
type extGapSender struct {
	gen  providers.MultimodalGenerator
	mu   sync.Mutex
	reqs []extmode.SendRequest
}

func (s *extGapSender) Send(ctx context.Context, scope contracts.Scope, req extmode.SendRequest) (providers.RichResult, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.gen.Generate(ctx, scope, req.Rich)
}

func (s *extGapSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

const (
	extGapQuestion = "What are the opening hours of the clinic building?"
	extGapExcerpt  = "The clinic building opens at 08:00 and closes at 18:00."
)

// In External mode a RAG question is answered by the external answerer, from the excerpts the egress guard let through,
// with citations that resolve through the local resolver; in Local and Hybrid mode the same engine answers it from the
// local model and the provider hears nothing. A source the provider may not receive is reported as egress_restricted
// and nothing is sent.
func TestExternalRagAnswersOnlyInExternalMode(t *testing.T) {
	ctx := context.Background()
	scope := conv.RoleScope(conv.RoleReceptionist)
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	clk := clock.Real{}

	raw, err := os.ReadFile("../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json")
	if err != nil {
		t.Fatal(err)
	}
	var prof pv.ProviderProfile
	if err := json.Unmarshal(raw, &prof); err != nil {
		t.Fatal(err)
	}
	prof.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: common.Identifier(scope.ApplicationID)}}

	local := pfake.New(pfake.WithName("local"))
	ext := pfake.New(pfake.WithName("external"), pfake.WithModel("gpt-4.1-mini"))
	sender := &extGapSender{gen: ext}
	budget := providers.Budget{MaxPromptTokens: 16384, MaxOutputTokens: 2048, MaxBatchItems: 4, Timeout: time.Minute}
	localSel, err := dialogue.LocalSelector(local, budget)
	if err != nil {
		t.Fatal(err)
	}
	policyTable := modes.StaticPolicy{modes.RoleController: {Profiles: []pv.ProfileID{prof.ProfileID}}}
	rt, err := extmode.New(extmode.Config{
		Local: localSel, Modes: a.modes, KillSwitch: a.egress, Policy: hybrid.ControllerLocalPolicy(policyTable, a.resolver),
		LocalProviders: extGapLocalUsable{}, Profiles: extGapProfiles{prof: prof}, Egress: a.egress, Budgets: extGapBudgets{},
		Packages: extWireTestPackages(), Sender: sender, Budget: budget, Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The data-egress policy: external processing allowed for the provider's model and the internal class, at every level.
	admin := scope.Clone()
	admin.PrincipalID, admin.SessionID = "usr_egress_admin", "ses_egress_admin"
	admin.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace, pv.EgressLevelSource} {
		c, _ := dataegress.WriteCapability(lv)
		admin.CapabilityScopes = append(admin.CapabilityScopes, c)
	}
	base := func(level pv.EgressLevel, source pv.SourceID, classes ...pv.EgressEligibleClass) dataegress.PolicyInput {
		return dataegress.PolicyInput{
			Level: level, SourceID: source, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
			AllowedProviders: []pv.ProviderID{prof.ProviderID}, AllowedModels: []pv.ModelRef{{ProviderID: prof.ProviderID, ModelID: prof.ModelID}},
			AllowedClasses: classes, AllowedRegions: []pv.RegionCode{"us", "eu"},
		}
	}
	internal := []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal}
	if _, err := a.egress.PutDeploymentPolicy(ctx, dataegress.Operator{DeploymentID: scope.DeploymentID, PrincipalID: "opr_root"}, base(pv.EgressLevelDeployment, "", internal...)); err != nil {
		t.Fatal(err)
	}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		if _, err := a.egress.PutPolicy(ctx, admin, base(lv, "", internal...)); err != nil {
			t.Fatal(err)
		}
	}
	newSource := func(mode pv.EgressMode, class pv.DataClassification) pv.SourceID {
		id := pv.SourceID(ids.New("src"))
		in := base(pv.EgressLevelSource, id, pv.EgressEligibleClass(class))
		in.SourceMode, in.SourceClass = mode, &class
		if _, err := a.egress.PutPolicy(ctx, admin, in); err != nil {
			t.Fatal(err)
		}
		return id
	}
	allowed := newSource(pv.EgressModeAllowExcerpt, pv.DataClassificationInternal)
	localOnly := newSource(pv.EgressModeLocalOnly, pv.DataClassificationInternal)

	evidence := func(id pv.SourceID, eg rag.SourceEgress) rag.EvidencePackage {
		cls := rag.SourceClassificationInternal
		loc := conversation.CitationLocator{Section: new("Section A")}
		return rag.EvidencePackage{
			PackageID: rag.EvidencePackageID(ids.New("evp")),
			Scope: rag.EvidencePackageScope{
				ApplicationID: common.Identifier(scope.ApplicationID), TenantID: common.Identifier(scope.TenantID),
				WorkspaceID: common.Identifier(scope.WorkspaceID), PrincipalID: common.Identifier(scope.PrincipalID),
			},
			Query: rag.SearchRequestQuery(extGapQuestion), CreatedAt: common.Timestamp("2026-10-12T09:00:00.000Z"),
			Items: []rag.EvidenceItem{{
				EvidenceID: "e1", Rank: 1, ChunkID: "chk_1", GenerationID: rag.GenerationID(ids.New("gen")), SourceID: actgen.SourceID(id),
				DisplayName: "Opening hours", Version: actgen.OpaqueVersion("v1"), Locator: &loc,
				Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
				OpenRef: conversation.CitationOpenRef{Value: conversation.CitationOpenRefSource{Kind: "source"}},
				Text:    extGapExcerpt, EstToks: rag.EstToks((len(extGapExcerpt) + 2) / 3), SourceEgress: &eg, SourceClassification: &cls,
			}},
			Budget:    rag.EvidencePackageBudget{MaxItems: 8, MaxToks: 3000, EstToks: 100},
			Retrieval: rag.EvidencePackageRetrieval{Methods: []rag.RetrievalMethod{rag.RetrievalMethodFts}, Notices: rag.RetrievalNotices{}},
		}
	}
	retr := &extGapRetriever{}
	catalog := &extGapCatalog{states: map[string]query.SourceState{}}
	grant := func(p rag.EvidencePackage) {
		retr.mu.Lock()
		retr.pkg = p
		retr.mu.Unlock()
		catalog.mu.Lock()
		defer catalog.mu.Unlock()
		for _, it := range p.Items {
			catalog.states[scope.TenantID+"/"+string(it.SourceID)] = query.SourceState{
				DisplayName: it.DisplayName, Authorised: true, Availability: it.Availability, Staleness: it.Staleness,
			}
		}
	}

	acfg := answer.DefaultConfig()
	localAns, err := answer.New(answer.Deps{Generator: local, Catalog: catalog, Clock: clk}, acfg)
	if err != nil {
		t.Fatal(err)
	}
	localQueries, err := gateway.NewRAGQueries(retr, localAns)
	if err != nil {
		t.Fatal(err)
	}
	la := &ragServeLocalAnswer{Answerer: localAns, Config: acfg, Budget: providers.DefaultBudget()}
	know, err := extGapNewKnowledge(rt, a.resolver, localQueries, la, retr, catalog, clk)
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{"claims": []map[string]any{{"text": "The building opens at 08:00.", "evidence_ids": []string{"e1"}}}}
	local.On(pfake.Any()).ReplyJSON(claims)
	ext.On(pfake.Any()).ReplyJSON(claims)
	setMode := func(m modes.Mode) {
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			if err := a.modes.set(lv, scope, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	ask := func() rag.AnswerResult {
		t.Helper()
		out, err := know.Answer(ctx, scope, rag.AnswerRequest{Question: rag.SearchRequestQuery(extGapQuestion)})
		if err != nil {
			t.Fatalf("Answer: %v", err)
		}
		return out
	}
	grounded := func(r rag.AnswerResult) rag.GroundedAnswer {
		t.Helper()
		g, ok := r.Value.(rag.GroundedAnswer)
		if !ok {
			t.Fatalf("outcome is %T %+v, want a grounded answer", r.Value, r.Value)
		}
		return g
	}

	grant(evidence(allowed, rag.SourceEgressAllowExcerpt))
	for _, m := range []modes.Mode{modes.ModeLocal, modes.ModeHybrid} {
		setMode(m)
		before := local.Calls()
		grounded(ask())
		if local.Calls() != before+1 || sender.count() != 0 {
			t.Fatalf("%s: local calls %d (was %d), provider requests %d; RAG must stay local", m, local.Calls(), before, sender.count())
		}
	}

	setMode(modes.ModeExternal)
	before := local.Calls()
	g := grounded(ask())
	if sender.count() != 1 || local.Calls() != before {
		t.Fatalf("external: provider requests %d, local calls %d (was %d)", sender.count(), local.Calls(), before)
	}
	if len(g.Citations) != 1 || localAns.Resolver().Len() == 0 {
		t.Fatalf("citations %d, references held by the local resolver %d", len(g.Citations), localAns.Resolver().Len())
	}
	sent := sender.reqs[0]
	if !strings.Contains(fmt.Sprintf("%+v", sent.Rich), extGapExcerpt) {
		t.Error("the allowed excerpt was not what the provider received")
	}

	// A source that stays local is never sent: the typed result says so, and no model answers instead.
	grant(evidence(localOnly, rag.SourceEgressLocalOnly))
	sentBefore, localBefore := sender.count(), local.Calls()
	r := ask()
	if ns, ok := r.Value.(rag.NoSupportedEvidence); !ok || ns.Reason != rag.NoSupportedEvidenceReasonEgressRestricted {
		t.Fatalf("local-only source: %T %+v, want no_supported_evidence egress_restricted", r.Value, r.Value)
	}
	if sender.count() != sentBefore || local.Calls() != localBefore {
		t.Fatalf("local-only source: the provider got %d more requests, the local model %d more calls", sender.count()-sentBefore, local.Calls()-localBefore)
	}
}

// ---- the budget sweep ----

// The sweep is a class 6 job under its own service identity, queued for each application with a live profile by the
// service of the process and run by the pool of the RAG stack: a reservation that timed out is marked expired without a
// request arriving first, and the pool accepts the job for that identity, that job type and that service scope only.
func TestBudgetSweepJobIsScheduled(t *testing.T) {
	extWireSaveSection(t)
	externalSection = externalConfig{Enabled: true, DeploymentOperators: []string{"app_demo/usr_ops"}}
	f := newRagFixture(t)
	a, err := newExternalAdmin(externalAdminDeps{
		DB: f.db, Audit: f.aud, Clock: f.clk, DeploymentID: externalTestDeployment, Dir: filepath.Join(t.TempDir(), "external"),
		Validator: externalAcceptKeys{}, Section: externalSection,
	})
	if err != nil {
		t.Fatal(err)
	}
	services := gateway.NewServices()
	for _, perr := range []error{gateway.Provide(services, f.stack), gateway.Provide[gateway.ProvidersService](services, a)} {
		if perr != nil {
			t.Fatal(perr)
		}
	}
	del := &extWireDelegate{local: extWireTestLocal{}, guard: dialogue.LocalOnlyGuard{}, packages: extWireTestPackages(), budget: providers.DefaultBudget()}
	var start func(context.Context) func()
	var built *extWireStack
	oldCompose := extWireOnCompose
	extWireOnCompose = func(s *extWireStack) { built = s }
	t.Cleanup(func() { extWireOnCompose = oldCompose })
	err = extWireProvide(context.Background(), serveDeps{
		Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, Audit: f.aud, Services: services, External: del,
		AddService: func(s func(context.Context) func()) { start = s },
	})
	if err != nil {
		t.Fatal(err)
	}
	if built == nil || start == nil || built.sweepEvery != extGapSweepDefaultEvery {
		t.Fatalf("the provider gave no stack (%t), no loop (%t), or the sweep period %v", built != nil, start != nil, built != nil && built.sweepEvery != extGapSweepDefaultEvery)
	}
	if h := del.hy.Load(); h == nil || h.h != extGapHybridRealiser(built.hybrid) || h.routed {
		t.Fatalf("the delegate was given no hybrid realisation, or one routed with the default policy: %+v", h)
	}

	// An application with a live profile, a budget, and a reservation that is about to time out.
	ctx := context.Background()
	day := f.clk.Now().UTC().Format("2006-01-02")
	draft, err := extWireDraft(ExternalProviderAnthropic, "", "app_demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft.LastPolicyReview, draft.Terms.RecordedOn = day, day
	if _, err := a.reg.Register(ctx, externalTestScope("ten_acme", "usr_ops"), draft); err != nil {
		t.Fatal(err)
	}
	user := externalTestScope("ten_acme", "usr_x")
	if _, err := a.budget.PutBudget(ctx, externalTestScope("ten_acme", "usr_ops"), pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider, ProviderID: new(pv.ProviderID("anthropic"))},
		Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: 1_000_000, MonthlyMicroUnits: 10_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.budget.Reserve(ctx, user, providerbudget.Call{
		Provider: pv.ProviderRef{ProfileID: pv.ProfileID(ids.New("pprf")), ProviderID: "anthropic", ModelID: "claude-haiku-4-5", ModelVersion: "2026-10-01"},
		Role:     pv.RoleConversationalController, Channel: pv.ChannelInteractive, EnvelopeID: pv.EnvelopeID(ids.New("envl")), DecisionID: pv.EgressDecisionID(ids.New("edec")),
		Prices: pv.PriceTable{Currency: "USD", EffectiveOn: day, Prices: pv.PriceTablePrices{ToksOut: &pv.PriceTablePrice{PerUnits: 1000, MicroUnits: 2000}}},
		Units:  providerbudget.Units{ToksOut: 500}, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	reservations := func(state string) int {
		return f.countRows(`SELECT count(*) FROM providerbudget_reservations WHERE state = ?`, state)
	}
	if reservations("reserved") != 1 {
		t.Fatal("the reservation was not made")
	}
	f.clk.Advance(5 * time.Minute) // past the timeout of the reservation

	cctx, cancel := context.WithCancel(ctx)
	stop := start(cctx)
	defer func() { cancel(); stop() }()
	count := func(where string, args ...any) int {
		return f.countRows(`SELECT count(*) FROM jobs WHERE type = ? AND class = ? AND service_identity = ? `+where,
			append([]any{extGapSweepJob, int(jobs.ClassMaintenance), extGapSweepIdentity}, args...)...)
	}
	deadline := time.Now().Add(15 * time.Second)
	for count("") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the schedule queued no sweep job")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.drain()
	if n := count("AND state = ?", "succeeded"); n != 1 {
		t.Fatalf("%d sweep jobs succeeded, want 1\n%s", n, f.dumpJobs())
	}
	if reservations("expired") != 1 || reservations("reserved") != 0 {
		t.Fatalf("expired %d, still reserved %d", reservations("expired"), reservations("reserved"))
	}
	// The same period queues nothing twice.
	if n, err := built.extGapScheduleSweep(ctx); err != nil || n != 0 {
		t.Fatalf("a second pass in the period queued %d jobs: %v", n, err)
	}

	// What the pool accepts for the identity.
	auth := extGapSweepAuthority{deployment: externalTestDeployment}
	svc := extGapSweepScope(externalTestDeployment, "app_demo")
	job := func(mutate func(*jobs.Job)) jobs.Job {
		j := jobs.Job{Type: extGapSweepJob, Scope: svc, ServiceIdentity: extGapSweepIdentity}
		if mutate != nil {
			mutate(&j)
		}
		return j
	}
	if err := auth.AuthorizeService(ctx, extGapSweepIdentity, job(nil)); err != nil {
		t.Fatalf("the job the schedule queues is denied: %v", err)
	}
	for name, tc := range map[string]struct {
		identity string
		mutate   func(*jobs.Job)
	}{
		"another identity":        {providerreg.ServiceIdentity, nil},
		"another job type":        {extGapSweepIdentity, func(j *jobs.Job) { j.Type = providerreg.JobRevalidate }},
		"a payload":               {extGapSweepIdentity, func(j *jobs.Job) { j.Payload = json.RawMessage(`{"all":true}`) }},
		"another deployment":      {extGapSweepIdentity, func(j *jobs.Job) { j.Scope.DeploymentID = "dep_other" }},
		"a tenant scope":          {extGapSweepIdentity, func(j *jobs.Job) { j.Scope.TenantID = "ten_acme" }},
		"a user principal":        {extGapSweepIdentity, func(j *jobs.Job) { j.Scope.PrincipalID = "usr_admin" }},
		"another role":            {extGapSweepIdentity, func(j *jobs.Job) { j.Scope.Roles = []string{"admin"} }},
		"wider capability scopes": {extGapSweepIdentity, func(j *jobs.Job) { j.Scope.CapabilityScopes = append(j.Scope.CapabilityScopes, "items.delete") }},
		"no scope":                {extGapSweepIdentity, func(j *jobs.Job) { j.Scope = contracts.Scope{} }},
	} {
		if err := auth.AuthorizeService(ctx, tc.identity, job(tc.mutate)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: %v, want permission_denied", name, err)
		}
	}
	if err := (extGapSweepAuthority{}).AuthorizeService(ctx, extGapSweepIdentity, job(nil)); err == nil {
		t.Error("an authority with no deployment accepted a job")
	}
}

// ---- X-28: the Gemini OAuth token source ----

type extFinalFakeEngine struct {
	got []oauth.TokenRequest
	tok credentials.Secret
	err error
}

func (f *extFinalFakeEngine) Token(_ context.Context, _ contracts.Scope, req oauth.TokenRequest) (credentials.Secret, error) {
	f.got = append(f.got, req)
	return f.tok, f.err
}

type extFinalFakeKinds struct {
	kind pv.CredentialKind
	err  error
	asks []pv.CredentialRefID
}

func (f *extFinalFakeKinds) Get(_ context.Context, _ contracts.Scope, ref pv.CredentialRefID) (pv.CredentialRef, error) {
	f.asks = append(f.asks, ref)
	return pv.CredentialRef{Kind: f.kind}, f.err
}

func TestExtFinalEngineTokensAskTheEngineForThisReferenceAndScopes(t *testing.T) {
	eng := &extFinalFakeEngine{tok: "oauth-access-token-0001"} // TEST-ONLY fixture token, not a credential
	w := extFinalEngineTokens{e: eng}
	got, err := w.Bearer(context.Background(), contracts.Scope{}, "cred_01TESTREFERENCE", []string{"scope-a"}, true) // TEST-ONLY opaque reference id, not a key
	if err != nil || got != "oauth-access-token-0001" {
		t.Fatalf("Bearer = %q, %v", got, err)
	}
	if len(eng.got) != 1 || eng.got[0].Ref != "cred_01TESTREFERENCE" || !eng.got[0].ForceRefresh || len(eng.got[0].Scopes) != 1 || eng.got[0].Scopes[0] != "scope-a" {
		t.Fatalf("the engine was asked %+v", eng.got)
	}
	eng.err = contracts.New(contracts.ErrPermissionDenied, "revoked")
	if _, err := w.Bearer(context.Background(), contracts.Scope{}, "cred_01TESTREFERENCE", nil, false); contracts.CodeOf(err) != contracts.ErrPermissionDenied { // TEST-ONLY opaque reference id, not a key
		t.Fatalf("the engine's refusal was lost: %v", err)
	}
	if _, err := (extFinalEngineTokens{}).Bearer(context.Background(), contracts.Scope{}, "cred_01TESTREFERENCE", nil, false); err == nil { // TEST-ONLY opaque reference id, not a key
		t.Fatal("a token source without an engine returned a token")
	}
}

// A Gemini profile is served with the authentication of the credential bound to it, decided per call by the credential
// service in the scope of the call: an OAuth connection sends a bearer token and no key, a key connection sends the key
// and no bearer, and anything else is refused before a byte is sent.
func TestExtFinalGeminiUsesTheAuthenticationOfTheCredentialKind(t *testing.T) {
	const ref = "cred_01TESTREFERENCE" // TEST-ONLY opaque reference id, not a key
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3},"modelVersion":"gemini-2.5-flash","responseId":"abc"}`)
	}))
	defer srv.Close()
	reg := egress.NewProviderRegistry()
	spec := ExternalProviderSpec{
		Provider: ExternalProviderGemini, Egress: reg, Keys: externalProviderTestKeys{},
		Gemini: gemini.Config{BaseURL: srv.URL, CredentialRef: ref, Model: "gemini-2.5-flash"},
	}
	keyClient, err := NewExternalProvider(spec)
	if err != nil {
		t.Fatal(err)
	}
	sc := externalTestScope("ten_acme", "usr_member")
	eng := &extFinalFakeEngine{tok: "oauth-access-token-0001"} // TEST-ONLY fixture token, not a credential
	kinds := &extFinalFakeKinds{}
	g, err := extFinalGeminiBoth(spec, keyClient, eng, kinds)
	if err != nil {
		t.Fatal(err)
	}
	req := providers.RichRequest{Role: providers.RoleController, MaxToks: 16, Messages: []providers.RichMessage{{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("hi")}}}}
	last := func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return nil
		}
		return seen[len(seen)-1]
	}
	requests := func() int { mu.Lock(); defer mu.Unlock(); return len(seen) }

	kinds.kind = pv.CredentialKindOauth
	if _, err := g.Generate(context.Background(), sc, req); err != nil {
		t.Fatalf("oauth connection: %v", err)
	}
	if h := last(); h.Get("Authorization") != "Bearer oauth-access-token-0001" || h.Get("X-Goog-Api-Key") != "" || len(eng.got) != 1 || eng.got[0].Ref != ref {
		t.Fatalf("oauth connection: headers %v, token requests %+v", h, eng.got)
	}

	kinds.kind = pv.CredentialKindAPIKey
	if _, err := g.Generate(context.Background(), sc, req); err != nil {
		t.Fatalf("api key connection: %v", err)
	}
	if h := last(); h.Get("X-Goog-Api-Key") == "" || h.Get("Authorization") != "" || len(eng.got) != 1 {
		t.Fatalf("api key connection: headers %v, token requests %d", h, len(eng.got))
	}

	// Refusals send nothing, and say so: sent=false, authentication_failed.
	before := requests()
	for name, set := range map[string]func(){
		"a reference of another scope": func() { kinds.err = contracts.New(contracts.ErrScopeViolation, "not this scope's") },
		"an unknown kind":              func() { kinds.err, kinds.kind = nil, "other" },
	} {
		kinds.err, kinds.kind = nil, pv.CredentialKindOauth
		set()
		_, err := g.Generate(context.Background(), sc, req)
		if err == nil {
			t.Fatalf("%s: the call went through", name)
		}
		ce := modes.CallErrorOf(err)
		if ce.Sent || ce.Kind != pv.ProviderErrorKindAuthenticationFailed {
			t.Fatalf("%s: mapped to %+v, want authentication_failed with sent=false", name, ce)
		}
	}
	if requests() != before {
		t.Fatalf("a refused call sent %d request(s)", requests()-before)
	}

	// Without an engine there is nothing to tell the kinds apart with: the key client alone.
	if alone, err := extFinalGeminiBoth(spec, keyClient, nil, kinds); err != nil || alone != keyClient {
		t.Fatalf("without an engine: %v %v", alone, err)
	}
}

// The destination registries of external mode carry the platform deny-list: a platform port is refused even when an
// allowlist names it.
func TestExtFinalRegistriesCarryThePlatformDenyList(t *testing.T) {
	const port = 42991
	platformDenyAddListener("127.0.0.1:42991")
	t.Cleanup(func() {
		platformDenyExtra.mu.Lock()
		defer platformDenyExtra.mu.Unlock()
		platformDenyExtra.addrs = nil
	})
	dest := egress.Destination{Host: "127.0.0.1", Ports: []int{port}, Scheme: egress.SchemeHTTP}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	oauthReg := extFinalOAuthRegistry()
	if err := oauthReg.Set("app_demo", al); err != nil {
		t.Fatal(err)
	}
	if got := platformDenyDial(t, oauthReg.Dialer("app_demo"), port); got != egress.ReasonPlatformPort {
		t.Errorf("OAuth registry: dial reason %q, want platform_port", got)
	}
	provReg := extFinalProviderRegistry()
	if err := provReg.Set("openai", al); err != nil {
		t.Fatal(err)
	}
	if got := platformDenyDial(t, provReg.Dialer("openai"), port); got != egress.ReasonPlatformPort {
		t.Errorf("provider registry: dial reason %q, want platform_port", got)
	}
}

// ---- X-28: the external voice factory with Attest ----

type extFinalKS struct{ engaged bool }

func (k *extFinalKS) KillSwitch(context.Context, string) (dataegress.KillSwitchState, error) {
	return dataegress.KillSwitchState{Engaged: k.engaged}, nil
}

// extFinalProfiles admits the one profile, or refuses with err.
type extFinalProfiles struct {
	prof pv.ProviderProfile
	err  error
}

func (p extFinalProfiles) Get(context.Context, contracts.Scope, pv.ProfileID) (pv.ProviderProfile, error) {
	return p.prof, nil
}

func (p extFinalProfiles) Check(context.Context, contracts.Scope, pv.ProfileID, providerreg.Requirement) (providerreg.Admission, error) {
	return providerreg.Admission{Profile: p.prof}, p.err
}

func (p extFinalProfiles) Fallback(context.Context, contracts.Scope, pv.ProfileID, providerreg.FallbackCondition, providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrPermissionDenied, "no fallback")
}

type extFinalKit struct{ kit extFinalVoiceKit }

func (k extFinalKit) Kit(context.Context, contracts.Scope, pv.ProviderProfile) (extFinalVoiceKit, bool) {
	return k.kit, true
}

type extFinalAllowText struct{ deny bool }

func (a extFinalAllowText) AllowText(context.Context, contracts.Scope, extmodal.Target, extmodal.Purpose, string) (extmodal.TextDecision, error) {
	if a.deny {
		return extmodal.TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "guard says no")
	}
	return extmodal.TextDecision{EnvelopeID: pv.EnvelopeID(ids.New("envl")), DecisionID: pv.EgressDecisionID(ids.New("edec"))}, nil
}

// extFinalAdmitBudget admits every reservation and records nothing.
type extFinalAdmitBudget struct{}

func (extFinalAdmitBudget) Reserve(context.Context, contracts.Scope, providerbudget.Call) (providerbudget.Reservation, error) {
	return providerbudget.Reservation{ID: pv.ReservationID(ids.New("rsv"))}, nil
}

func (extFinalAdmitBudget) Settle(context.Context, contracts.Scope, pv.ReservationID, providerbudget.Actual) (providerbudget.Settlement, error) {
	return providerbudget.Settlement{}, nil
}

func (extFinalAdmitBudget) Release(context.Context, contracts.Scope, pv.ReservationID) error {
	return nil
}

type extFinalVoiceRig struct {
	t              *testing.T
	v              *extFinalVoice
	scope          contracts.Scope
	extTTS, locTTS *vfake.Provider
	extSTT, locSTT *vfake.Provider
	ks             *extFinalKS
	modes          *modes.MemoryModes
	policy         modes.StaticPolicy
	modalities     *extmodal.MemoryPolicy
	prof           pv.ProviderProfile
}

func extFinalNewVoiceRig(t *testing.T, mode modes.Mode, fallback ...modes.Condition) *extFinalVoiceRig {
	t.Helper()
	r := &extFinalVoiceRig{
		t: t, scope: externalTestScope("ten_acme", "usr_member"), ks: &extFinalKS{},
		extTTS: vfake.New(), locTTS: vfake.New(), extSTT: vfake.New(), locSTT: vfake.New(), modes: modes.NewMemoryModes(),
	}
	prof := pv.ProviderProfile{
		ProfileID: pv.ProfileID("pprf_" + strings.Repeat("0", 25) + "V"), ProviderID: "acme", ModelID: "acme-omni",
		Roles:      []pv.Role{pv.RoleSTT, pv.RoleTTS},
		Modalities: pv.ProviderProfileModalities{Input: []pv.Modality{pv.ModalityText, pv.ModalityAudio}, Output: []pv.Modality{pv.ModalityText, pv.ModalityAudio}},
		Media: &pv.ProviderProfileMedia{
			Audio: &pv.ProviderProfileMediaHandling{MaxBytes: 1 << 20, MimeTypes: []string{"audio/wav"}, Retention: pv.RetentionModeTransient},
		},
		Voice: pv.ProviderProfileVoice{STT: true, TTS: true, AudioRetention: pv.RetentionModeTransient},
	}
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
		if err := r.modes.Set(lv, r.scope, mode); err != nil {
			t.Fatal(err)
		}
	}
	res, err := modes.NewResolver(r.modes, r.ks)
	if err != nil {
		t.Fatal(err)
	}
	mp := extmodal.NewMemoryPolicy()
	mp.SetProvider("acme", extmodal.Enable(true, extmodal.ModTTS, extmodal.ModSTT))
	mp.SetTenant(r.scope, extmodal.Enable(true, extmodal.ModTTS, extmodal.ModSTT))
	svc, err := extmodal.New(extmodal.Deps{Policy: mp, Egress: extFinalAllowText{}, Budgets: extFinalAdmitBudget{}})
	if err != nil {
		t.Fatal(err)
	}
	r.modalities, r.prof = mp, prof
	r.policy = modes.StaticPolicy{
		modes.RoleTTS: {Profiles: []pv.ProfileID{prof.ProfileID}, LocalFallback: fallback},
		modes.RoleSTT: {Profiles: []pv.ProfileID{prof.ProfileID}, LocalFallback: fallback},
	}
	r.v = &extFinalVoice{
		svc: svc, modes: res, policy: r.policy, profiles: extFinalProfiles{prof: prof},
		source: extFinalKit{extFinalVoiceKit{STT: r.extSTT, TTS: r.extTTS, Caps: pfake.DefaultCapabilities("acme", "acme-omni")}},
		log:    slog.New(slog.DiscardHandler),
	}
	extFinalVoiceHolder.Store(r.v)
	t.Cleanup(func() { extFinalVoiceHolder.Store(nil) })
	return r
}

func (r *extFinalVoiceRig) speak(text string) error {
	r.t.Helper()
	st, err := extFinalTTS{local: r.locTTS}.SynthesizeStream(context.Background(), r.scope, voice.SynthesizeRequest{Text: text})
	if st != nil {
		_ = st.Close()
	}
	return err
}

func extFinalTestDirective() *conversation.DialogueDirective {
	capID := common.CapabilityID("appointments.book")
	return &conversation.DialogueDirective{
		TurnID: "trn_01J0000000000000000000000A", CapabilityID: &capID, Act: conversation.DialogueDirectiveActPresentResult,
		Facts:        []actgen.Fact{{Field: "doctor", Value: actgen.CanonicalValue{Value: actgen.TextValue{Value: "Dr. Bianchi"}}}},
		MustState:    []common.FieldName{"doctor"},
		Confirmation: actgen.ConfirmationRequirement{Mode: actgen.ConfirmationRequirementModeNone},
	}
}

func TestExtFinalVoiceFactoryWrapsOnlyTheRoutedRolesWhenExternalModeIsOn(t *testing.T) {
	extWireSaveSection(t)
	local := voiceprofile.Providers{STT: vfake.New(), TTS: vfake.New(), VAD: vfake.New()}

	externalSection = externalConfig{}
	if got := extFinalWrapVoice(local); got.STT != local.STT || got.TTS != local.TTS {
		t.Fatal("with external mode off the voice providers must be returned untouched")
	}
	externalSection = externalConfig{Enabled: true, Routing: map[string]externalRoute{"tts": {Profiles: []string{"pprf_x"}}}}
	got := extFinalWrapVoice(local)
	if _, ok := got.TTS.(extFinalTTS); !ok || got.STT != local.STT || got.VAD != local.VAD {
		t.Fatalf("only the routed role is wrapped: %T %T", got.TTS, got.STT)
	}
	externalSection.Routing["stt"] = externalRoute{Profiles: []string{"pprf_x"}}
	if got := extFinalWrapVoice(local); func() bool { _, ok := got.STT.(extFinalSTT); return !ok || got.VAD != local.VAD }() {
		t.Fatalf("the STT role is routed and not wrapped: %T", got.STT)
	}
	// The transport calls the hook; the hook is set by this file.
	if voiceLocalAIExternal == nil {
		t.Fatal("the LocalAI voice transport has no external hook")
	}
}

func TestExtFinalExternalSpeechSpeaksOnlyAttestedText(t *testing.T) {
	const spoken = "Your visit is booked for Tuesday."

	t.Run("an attested response is spoken by the external provider, and an edit of it is not", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeHybrid)
		if err := r.v.svc.Attest(r.scope, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
			t.Fatal(err)
		}
		if err := r.speak(spoken); err != nil {
			t.Fatalf("speak: %v", err)
		}
		if c := r.extTTS.Calls(); len(c) != 1 || c[0].Text != spoken || r.locTTS.CallCount() != 0 {
			t.Fatalf("external calls %+v, local %d", c, r.locTTS.CallCount())
		}
		if err := r.speak("Your visit is booked for Wednesday."); extmodal.ReasonOf(err) != extmodal.ReasonTextNotVerified {
			t.Fatalf("an unattested text: %v", err)
		}
		if r.extTTS.CallCount() != 1 {
			t.Fatal("unverified text reached the external provider")
		}
	})

	t.Run("without the fallback in the policy, unattested text is refused and nothing else speaks", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeExternal)
		if err := r.speak(spoken); extmodal.ReasonOf(err) != extmodal.ReasonTextNotVerified {
			t.Fatalf("err = %v", err)
		}
		if r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 0 {
			t.Fatalf("external %d local %d: a refusal must not choose another provider", r.extTTS.CallCount(), r.locTTS.CallCount())
		}
	})

	t.Run("the policy names egress_denied: the refusal is served locally and nothing is sent", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeExternal, modes.CondEgressDenied)
		if err := r.speak(spoken); err != nil {
			t.Fatalf("err = %v", err)
		}
		if r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 1 {
			t.Fatalf("external %d local %d", r.extTTS.CallCount(), r.locTTS.CallCount())
		}
	})

	t.Run("Local mode and the kill switch keep the speech local", func(t *testing.T) {
		for name, setup := range map[string]func() *extFinalVoiceRig{
			"local mode": func() *extFinalVoiceRig { return extFinalNewVoiceRig(t, modes.ModeLocal) },
			"kill switch": func() *extFinalVoiceRig {
				r := extFinalNewVoiceRig(t, modes.ModeExternal)
				r.ks.engaged = true
				return r
			},
		} {
			r := setup()
			_ = r.v.svc.Attest(r.scope, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified})
			if err := r.speak(spoken); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 1 {
				t.Fatalf("%s: external %d local %d", name, r.extTTS.CallCount(), r.locTTS.CallCount())
			}
		}
	})

	t.Run("a profile the registry refuses is an error, not another provider", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeExternal, modes.CondEgressDenied)
		r.v.profiles = extFinalProfiles{err: contracts.New(contracts.ErrQuarantined, "quarantined").WithDetails("reason", "quarantined")}
		if err := r.speak(spoken); err == nil || r.locTTS.CallCount() != 0 || r.extTTS.CallCount() != 0 {
			t.Fatalf("err = %v, local %d external %d", err, r.locTTS.CallCount(), r.extTTS.CallCount())
		}
	})

	t.Run("an engine that is not composed leaves the speech local", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeExternal)
		extFinalVoiceHolder.Store(nil)
		if err := r.speak(spoken); err != nil || r.locTTS.CallCount() != 1 {
			t.Fatalf("err = %v, local %d", err, r.locTTS.CallCount())
		}
	})
}

// External speech is reserved on the provider budget of the process, as text is: with no budget for the call
// nothing is sent and only a policy that names budget_exhausted serves it locally, and a call the budget admits
// leaves its usage record when the stream ends.
func TestExternalSpeechIsReservedOnTheProviderBudget(t *testing.T) {
	const spoken = "Your visit is booked for Tuesday."
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	rig := func(fallback ...modes.Condition) *extFinalVoiceRig {
		r := extFinalNewVoiceRig(t, modes.ModeExternal, fallback...)
		r.prof.ModelVersion = "2026-10-01"
		r.prof.Pricing = pv.PriceTable{Currency: "USD", Prices: pv.PriceTablePrices{
			ToksIn: &pv.PriceTablePrice{PerUnits: 1000, MicroUnits: 1500}, Requests: &pv.PriceTablePrice{PerUnits: 1, MicroUnits: 10},
		}}
		r.v.profiles = extFinalProfiles{prof: r.prof}
		svc, err := extmodal.New(extmodal.Deps{Policy: r.modalities, Egress: extFinalAllowText{}, Budgets: a.budget})
		if err != nil {
			t.Fatal(err)
		}
		r.v.svc = svc
		if err := svc.Attest(r.scope, verify.Outcome{Text: spoken, Source: verify.SourceModel, Reason: verify.ReasonVerified}); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// No budget applies: nothing is sent, and the egress_denied fallback does not cover it; budget_exhausted does.
	r := rig(modes.CondEgressDenied)
	if err := r.speak(spoken); contracts.CodeOf(err) != contracts.ErrResourceExhausted || r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 0 {
		t.Fatalf("no budget: %v, external %d, local %d", err, r.extTTS.CallCount(), r.locTTS.CallCount())
	}
	r = rig(modes.CondBudgetExhausted)
	if err := r.speak(spoken); err != nil || r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 1 {
		t.Fatalf("no budget, budget fallback: %v, external %d, local %d", err, r.extTTS.CallCount(), r.locTTS.CallCount())
	}

	// A budget admits the call: it is made, and its usage is recorded.
	if _, err := a.PutBudget(ctx, externalTestScope("ten_acme", "usr_admin"), pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindTenant, ApplicationID: new(common.Identifier("app_demo")), TenantID: new(common.Identifier("ten_acme"))},
		Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: 1_000_000, MonthlyMicroUnits: 10_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	r = rig()
	if err := r.speak(spoken); err != nil || r.extTTS.CallCount() != 1 {
		t.Fatalf("with a budget: %v, external %d", err, r.extTTS.CallCount())
	}
	exp, err := a.budget.ExportUsage(ctx, r.scope, providerbudget.UsageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Rows) != 1 || exp.Rows[0].Record.Role != pv.RoleTTS || exp.Rows[0].Record.Outcome != pv.UsageRecordOutcomeSucceeded || exp.Rows[0].ChargedMicroUnits <= 0 {
		t.Fatalf("usage %+v", exp.Rows)
	}
}

func TestExtFinalAttestTurnAttestsTheOutcomeTheEngineWillProduce(t *testing.T) {
	r := extFinalNewVoiceRig(t, modes.ModeHybrid)
	d := extFinalTestDirective()
	in := dialogue.ResponseInput{Scope: r.scope, Directive: d, Options: verify.Options{Language: verify.LangEN}}
	template, err := verify.Degraded(d, nil, in.Options)
	if err != nil {
		t.Fatal(err)
	}

	// A realiser error, a template outcome and a model text that does not verify all leave the template speakable,
	// and the model's text is not.
	const invented = "Your appointment is with Dr. Rossi and the fee is 99.00 EUR."
	extFinalAttestTurn(in, verify.Outcome{Text: invented, Source: verify.SourceModel, Reason: verify.ReasonVerified}, false)
	if r.v.svc.Verified(r.scope, invented) {
		t.Fatal("unverified model text became speakable")
	}
	if !r.v.svc.Verified(r.scope, template.Text) {
		t.Fatal("the template that replaced the failed text is not speakable")
	}

	// A model text that verifies is speakable, exactly as the verifier returns it.
	ok := template.Text
	extFinalAttestTurn(in, verify.Outcome{Text: ok, Source: verify.SourceModel, Reason: verify.ReasonVerified}, false)
	if !r.v.svc.Verified(r.scope, ok) {
		t.Fatal("a verified text is not speakable")
	}

	// Another session's turn is not attested here.
	other := r.scope
	other.SessionID = "ses_other"
	if r.v.svc.Verified(other, ok) {
		t.Fatal("an attestation leaked to another session")
	}
	extFinalAttestTurn(dialogue.ResponseInput{Scope: r.scope}, verify.Outcome{}, true) // no directive: nothing, no panic
}
