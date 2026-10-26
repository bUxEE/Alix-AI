package sdkgo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

const testKey = "sk-test-0123456789abcdef-NEVER-ECHO"

func providersHarness(t *testing.T, principal string) *harness {
	h := newHarness(t)
	h.claims = sdktest.Claims{PrincipalID: principal}
	cfg := sdktest.DefaultProvidersConfig()
	cfg.TenantAdmins = []string{"admin-2"}
	h.fake.ConfigureProviders(cfg)
	return h
}

func ackAndConnect(t *testing.T, h *harness, provider string) {
	t.Helper()
	ctx := context.Background()
	p := h.client.Providers()
	must[*pv.BillingNoticeAck](t, "ack")(p.AcknowledgeBillingNotice(ctx, provider, sdkgo.BillingNoticeAckInput{
		NoticeVersion: "2026-10-02.1", ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true,
	}))
	must[*pv.ConnectionStatus](t, "credential")(p.SubmitCredential(ctx, provider, sdkgo.CredentialInput{Key: sdkgo.NewSecret("ok-" + testKey)}))
}

func oauthState(t *testing.T, started *sdkgo.OAuthStarted) string {
	t.Helper()
	u, err := url.Parse(started.AuthorizationURL)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("the authorization URL has no state: %v", err)
	}
	return u.Query().Get("state")
}

func TestProvidersAreUnavailableWithoutTheService(t *testing.T) {
	h := newHarness(t)
	h.claims = sdktest.Claims{PrincipalID: "principal-1"}
	ctx := context.Background()
	p := h.client.Providers()
	_, err := p.List(ctx)
	hasCode(t, "no providers service (never configured)", err, contracts.ErrUnavailable)
	_, err = p.GetMode(ctx)
	hasCode(t, "mode without the service", err, contracts.ErrUnavailable)
	_, err = p.StartOAuth(ctx, "openai", sdkgo.OAuthStartInput{Capabilities: []string{"chat"}, RedirectURI: "http://127.0.0.1:9/cb"})
	hasCode(t, "oauth without the service", err, contracts.ErrUnavailable)

	h.fake.ConfigureProviders(sdktest.DefaultProvidersConfig())
	must[*sdkgo.ProviderList](t, "list")(p.List(ctx))
	h.fake.SetProvidersUnavailable(true)
	_, err = p.GetKillSwitch(ctx)
	hasCode(t, "switched off", err, contracts.ErrUnavailable)
}

func TestProvidersListShowsOnlyStatusAndASafeAccount(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	ackAndConnect(t, h, "acme")
	list := must[*sdkgo.ProviderList](t, "list")(h.client.Providers().List(ctx))
	if len(list.Providers) != 2 {
		t.Fatalf("two profiles are on offer, got %d", len(list.Providers))
	}
	byID := map[pv.ProviderID]pv.ConnectionStatus{}
	for _, s := range list.Providers {
		byID[s.Provider.ProviderID] = s
	}
	acme, openai := byID["acme"], byID["openai"]
	if acme.State != pv.ConnectionStatusStateConnected || acme.Kind == nil || *acme.Kind != pv.CredentialKindAPIKey || !acme.BillingNoticeAcknowledged {
		t.Fatalf("acme is connected with a key and acknowledged, got %+v", acme)
	}
	if acme.Account.OrganizationID == nil || *acme.Account.OrganizationID != "org-fake" {
		t.Fatalf("a safe account id is shown, got %+v", acme.Account)
	}
	if openai.State != pv.ConnectionStatusStateNotConnected || openai.Kind != nil || openai.BillingNoticeAcknowledged {
		t.Fatalf("openai is not connected, got %+v", openai)
	}
	if reqs := h.fake.RequestsOf("listProviders"); len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].RawQuery != "" {
		t.Fatalf("one plain GET, got %+v", reqs)
	}
}

// The credential never comes back, never prints and is never kept.
func TestProvidersCredentialIsWriteOnly(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	secret := sdkgo.NewSecret(testKey)

	// the secret prints as a placeholder in every format
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("connect", "key", secret, "input", sdkgo.CredentialInput{Key: secret})
	encoded, _ := json.Marshal(sdkgo.CredentialInput{Key: secret, Label: "main"})
	shown := fmt.Sprintf("%v|%+v|%#v|%s|%q|%d", secret, secret, secret, secret, secret, secret) + logs.String() + string(encoded) +
		fmt.Sprintf("%v|%+v|%#v", sdkgo.CredentialInput{Key: secret}, &sdkgo.CredentialInput{Key: secret}, sdkgo.CredentialInput{Key: secret})
	if strings.Contains(shown, testKey) || !strings.Contains(shown, "[redacted]") {
		t.Fatalf("the secret leaked or is not marked redacted: %s", shown)
	}

	// the status holds no secret, and the fake kept none
	st := must[*pv.ConnectionStatus](t, "submit")(p.SubmitCredential(ctx, "acme", sdkgo.CredentialInput{
		Key: secret, Label: "main", ExpiresAt: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Scopes: []string{"model.request"},
	}))
	raw, _ := json.Marshal(st)
	if st.State != pv.ConnectionStatusStateConnected || strings.Contains(string(raw), testKey) || strings.Contains(fmt.Sprintf("%+v", st), testKey) {
		t.Fatalf("the status is connected and holds no key, got %s", raw)
	}
	if st.ExpiresAt == nil || *st.ExpiresAt != "2027-01-01T00:00:00Z" {
		t.Fatalf("the expiry travels as given, got %v", st.ExpiresAt)
	}
	reqs := h.fake.RequestsOf("putProviderCredential")
	if len(reqs) != 1 || reqs[0].Method != http.MethodPut || reqs[0].Path != "/v1/providers/acme/credential" || reqs[0].RawQuery != "" {
		t.Fatalf("one PUT to the credential route, never a query, got %+v", reqs)
	}
	if strings.Contains(fmt.Sprintf("%+v", h.fake.Requests()), testKey) {
		t.Fatal("the fake's request log holds the key")
	}

	// a refused key: a fixed message, never the key
	_, err := p.SubmitCredential(ctx, "acme", sdkgo.CredentialInput{Key: sdkgo.NewSecret("bad-" + testKey)})
	api := hasCode(t, "a refused key", err, contracts.ErrValidationFailed)
	if strings.Contains(err.Error(), testKey) || strings.Contains(api.Err.Message, testKey) {
		t.Fatalf("the error quotes the key: %v", err)
	}
	// a key broader than needed is quarantined with a warning
	broad := must[*pv.ConnectionStatus](t, "broad")(p.SubmitCredential(ctx, "acme", sdkgo.CredentialInput{Key: sdkgo.NewSecret("broad-" + testKey)}))
	if broad.State != pv.ConnectionStatusStateQuarantined || len(broad.Warnings) == 0 {
		t.Fatalf("a broad key is quarantined with a warning, got %+v", broad)
	}
	// disconnect shows not_connected again
	gone := must[*pv.ConnectionStatus](t, "disconnect")(p.Disconnect(ctx, "acme"))
	if gone.State != pv.ConnectionStatusStateNotConnected {
		t.Fatalf("disconnected, got %+v", gone)
	}
}

// A server that repeats the key anywhere in its answer is refused and the key is not repeated by the SDK.
func TestProvidersCredentialRefusesAnEchoingServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"as_of":"2026-10-02T09:00:00Z","provider":{"profile_id":"pprf_01J0000000000000000000000P","provider_id":"acme","model_id":"m","model_version":"1"},`+
			`"lifecycle":"qualified","state":"connected","kind":"api_key","account":{"account_id":null,"organization_id":%q,"project_id":null},`+
			`"connected_at":"2026-10-02T08:00:05Z","expires_at":null,"warnings":[],"health":"healthy","last_error_kind":null,"budget_state":null,"billing_notice_acknowledged":true}`, in.Key)
	}))
	defer srv.Close()
	client, err := sdkgo.NewClient(sdkgo.ClientOptions{
		BaseURL: srv.URL, HTTPClient: srv.Client(),
		GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) { return "tok", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := client.Providers().SubmitCredential(context.Background(), "acme", sdkgo.CredentialInput{Key: sdkgo.NewSecret(testKey)})
	hasCode(t, "an echoing server", err, contracts.ErrInternal)
	if st != nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("nothing is returned and the key is not repeated, got %v / %v", st, err)
	}
}

func TestProvidersArgumentsAreValidatedBeforeAToken(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	big := int64(-1)
	for name, call := range map[string]func() error{
		"a bad provider id": func() error { _, err := p.Disconnect(ctx, "../etc"); return err },
		"an empty key":      func() error { _, err := p.SubmitCredential(ctx, "acme", sdkgo.CredentialInput{}); return err },
		"a key with a newline": func() error {
			_, err := p.SubmitCredential(ctx, "acme", sdkgo.CredentialInput{Key: sdkgo.NewSecret("a\nb")})
			return err
		},
		"an unknown mode":       func() error { _, err := p.SetMode(ctx, sdkgo.ModeLevelTenant, "turbo"); return err },
		"an unknown level":      func() error { _, err := p.SetMode(ctx, "global", pv.DeploymentModeLocal); return err },
		"a source level, no id": func() error { _, err := p.GetEgressPolicy(ctx, pv.EgressLevelSource, ""); return err },
		"an id for a tenant":    func() error { _, err := p.GetEgressPolicy(ctx, pv.EgressLevelTenant, "src_1"); return err },
		"an unconfirmed notice": func() error {
			_, err := p.AcknowledgeBillingNotice(ctx, "acme", sdkgo.BillingNoticeAckInput{NoticeVersion: "1"})
			return err
		},
		"a negative estimate": func() error {
			_, err := p.GetDisclosure(ctx, "acme", sdkgo.DisclosureQuery{EstimatedToksIn: &big})
			return err
		},
		"a free-text kill reason": func() error { _, err := p.SetKillSwitch(ctx, true, "because I said so"); return err },
		"no capabilities": func() error {
			_, err := p.StartOAuth(ctx, "openai", sdkgo.OAuthStartInput{RedirectURI: "http://127.0.0.1/cb"})
			return err
		},
		"an empty callback": func() error {
			_, err := p.CompleteOAuth(ctx, sdkgo.OAuthCallbackInput{RedirectURI: "http://127.0.0.1/cb"})
			return err
		},
		"a budget with no kind": func() error { _, err := p.SetBudget(ctx, pv.Budget{}); return err },
		"a usage scope":         func() error { _, err := p.ExportUsage(ctx, sdkgo.UsageQuery{Scope: "world"}); return err },
		"a restricted files class": func() error {
			_, err := p.SetEgressPolicy(ctx, sdkgo.EgressPolicyUpdate{Level: pv.EgressLevelTenant, SourceMode: pv.EgressModeLocalOnly,
				Files: &pv.EgressPolicyContentEgress{Enabled: true, MaxClass: "restricted"}})
			return err
		},
		"an audio setting with no class": func() error {
			_, err := p.SetEgressPolicy(ctx, sdkgo.EgressPolicyUpdate{Level: pv.EgressLevelTenant, SourceMode: pv.EgressModeLocalOnly,
				Audio: &pv.EgressPolicyContentEgress{Enabled: true}})
			return err
		},
	} {
		hasCode(t, name, call(), contracts.ErrValidationFailed)
	}
	if n := len(h.tokenRequests()); n != 0 {
		t.Fatalf("a malformed argument must not cost a context token, %d were requested", n)
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Fatalf("nothing reaches the wire, %d requests were sent", n)
	}
}

// The operator decision is made from the deployment's own configuration, before the body is read.
func TestProvidersNeedAnOperatorAndTheLevelDecidesWhichOne(t *testing.T) {
	ctx := context.Background()
	stranger := providersHarness(t, "nobody")
	_, err := stranger.client.Providers().List(ctx)
	hasCode(t, "nobody is an operator", err, contracts.ErrPermissionDenied)
	_, err = stranger.client.Providers().SubmitCredential(ctx, "acme", sdkgo.CredentialInput{Key: sdkgo.NewSecret(testKey)})
	hasCode(t, "a credential from a non-operator", err, contracts.ErrPermissionDenied)

	admin := providersHarness(t, "admin-2")
	p := admin.client.Providers()
	must[*sdkgo.ProviderList](t, "a tenant admin lists")(p.List(ctx))
	must[*sdkgo.ProviderMode](t, "a tenant admin sets the tenant mode")(p.SetMode(ctx, sdkgo.ModeLevelTenant, pv.DeploymentModeHybrid))
	for name, call := range map[string]func() error{
		"deployment mode": func() error {
			_, err := p.SetMode(ctx, sdkgo.ModeLevelDeployment, pv.DeploymentModeExternal)
			return err
		},
		"application mode": func() error {
			_, err := p.SetMode(ctx, sdkgo.ModeLevelApplication, pv.DeploymentModeExternal)
			return err
		},
		"kill switch read":  func() error { _, err := p.GetKillSwitch(ctx); return err },
		"kill switch write": func() error { _, err := p.SetKillSwitch(ctx, true, "incident"); return err },
		"the application's usage": func() error {
			_, err := p.ExportUsage(ctx, sdkgo.UsageQuery{Scope: sdkgo.UsageScopeApplication})
			return err
		},
		"the application policy": func() error {
			_, err := p.SetEgressPolicy(ctx, sdkgo.EgressPolicyUpdate{Level: pv.EgressLevelApplication, SourceMode: pv.EgressModeLocalOnly})
			return err
		},
		"a provider-wide budget": func() error { _, err := p.SetBudget(ctx, testBudget(pv.BudgetSubjectKindProvider)); return err },
	} {
		hasCode(t, name+" needs a deployment operator", call(), contracts.ErrPermissionDenied)
	}
	must[*pv.EgressPolicy](t, "a tenant admin writes the tenant policy")(p.SetEgressPolicy(ctx, sdkgo.EgressPolicyUpdate{
		Level: pv.EgressLevelTenant, SourceMode: pv.EgressModeLocalOnly, Enabled: true,
	}))
	must[*pv.Budget](t, "a tenant admin creates a tenant budget")(p.SetBudget(ctx, testBudget(pv.BudgetSubjectKindTenant)))
}

func ptr[T any](v T) *T { return &v }

func testBudget(kind pv.BudgetSubjectKind) pv.Budget {
	subject := pv.BudgetSubject{Kind: kind}
	switch kind {
	case pv.BudgetSubjectKindProvider:
		subject.ProviderID = ptr(pv.ProviderID("openai"))
	case pv.BudgetSubjectKindTenant:
		subject.ApplicationID, subject.TenantID = ptr(identifier("fake-app")), ptr(identifier("tenant-1"))
	}
	return pv.Budget{
		Subject: subject, Channel: pv.ChannelInteractive, Currency: "EUR",
		Limits: pv.BudgetLimits{DailyMicroUnits: 5_000_000, MonthlyMicroUnits: 100_000_000}, WarnAtPercent: 80,
	}
}

func TestProvidersModeAndKillSwitch(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	mode := must[*sdkgo.ProviderMode](t, "mode")(p.GetMode(ctx))
	if mode.Effective != pv.DeploymentModeLocal || mode.DecidedBy != "default" || mode.KillSwitch || len(mode.Levels) != 0 {
		t.Fatalf("local is the default everywhere, got %+v", mode)
	}
	must[*sdkgo.ProviderMode](t, "deployment")(p.SetMode(ctx, sdkgo.ModeLevelDeployment, pv.DeploymentModeExternal))
	mode = must[*sdkgo.ProviderMode](t, "tenant")(p.SetMode(ctx, sdkgo.ModeLevelTenant, pv.DeploymentModeHybrid))
	if mode.Effective != pv.DeploymentModeHybrid || mode.DecidedBy != "tenant" || len(mode.Levels) != 2 {
		t.Fatalf("the most restrictive level decides, got %+v", mode)
	}
	ks := must[*sdkgo.KillSwitch](t, "engage")(p.SetKillSwitch(ctx, true, "incident_42"))
	if !ks.Engaged || ks.Reason != "incident_42" || ks.ChangedBy != "principal-1" {
		t.Fatalf("engaged with its reason, got %+v", ks)
	}
	mode = must[*sdkgo.ProviderMode](t, "mode under the switch")(p.GetMode(ctx))
	if mode.Effective != pv.DeploymentModeLocal || mode.DecidedBy != "kill_switch" || !mode.KillSwitch || mode.Configured != pv.DeploymentModeHybrid {
		t.Fatalf("local under the kill switch, what was configured is still shown, got %+v", mode)
	}
	if ks = must[*sdkgo.KillSwitch](t, "release")(p.SetKillSwitch(ctx, false, "")); ks.Engaged {
		t.Fatal("released")
	}
	if got := must[*sdkgo.KillSwitch](t, "read")(p.GetKillSwitch(ctx)); got.Engaged {
		t.Fatal("releasing enables nothing, and the switch reads released")
	}
}

func TestProvidersDisclosureNamesWhatMayLeave(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	local := must[*pv.EgressDisclosure](t, "local")(p.GetDisclosure(ctx, "openai", sdkgo.DisclosureQuery{}))
	if local.ExternalEnabled || len(local.DataClasses) != 0 || len(local.DataCategories) != 0 || local.OnUnavailable.Behavior != pv.EgressDisclosureOnUnavailableBehaviorFail {
		t.Fatalf("a request in local mode stays local and fails closed, got %+v", local)
	}
	ackAndConnect(t, h, "openai")
	must[*sdkgo.ProviderMode](t, "mode")(p.SetMode(ctx, sdkgo.ModeLevelDeployment, pv.DeploymentModeExternal))
	toks := int64(1200)
	d := must[*pv.EgressDisclosure](t, "external")(p.GetDisclosure(ctx, "openai", sdkgo.DisclosureQuery{
		Role: pv.RoleConversationalController, SourceIDs: []string{"src_a", "src_b"}, EstimatedToksIn: &toks,
	}))
	if !d.ExternalEnabled || !d.RAGExcerptsIncluded || d.Provider.ModelID != "gpt-4.1-mini" || d.Terms.TrainingUse != pv.TrainingUseNever {
		t.Fatalf("an external request names the model, the excerpts and the terms, got %+v", d)
	}
	if d.Cost.EstimateUpperMicroUnits == nil || d.Cost.BudgetState != pv.BudgetStatusOk {
		t.Fatalf("an upper-bound cost in integer micro-units, got %+v", d.Cost)
	}
	reqs := h.fake.RequestsOf("getProviderDisclosure")
	last := reqs[len(reqs)-1]
	q, _ := url.ParseQuery(last.RawQuery)
	if q.Get("role") != "conversational_controller" || len(q["source"]) != 2 || q.Get("estimated_toks_in") != "1200" {
		t.Fatalf("the query carries the request description, got %q", last.RawQuery)
	}
	// the kill switch wins: nothing is listed as leaving
	must[*sdkgo.KillSwitch](t, "engage")(p.SetKillSwitch(ctx, true, "stop"))
	if d = must[*pv.EgressDisclosure](t, "killed")(p.GetDisclosure(ctx, "openai", sdkgo.DisclosureQuery{})); d.ExternalEnabled {
		t.Fatal("under the kill switch nothing leaves")
	}
}

func TestProvidersEgressPolicyRevisions(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	view := must[*sdkgo.EgressPolicyView](t, "none yet")(p.GetEgressPolicy(ctx, pv.EgressLevelDeployment, ""))
	if view.Policy != nil {
		t.Fatalf("a level with no policy is disabled, got %+v", view.Policy)
	}
	update := sdkgo.EgressPolicyUpdate{
		Level: pv.EgressLevelDeployment, Enabled: true, SourceMode: pv.EgressModeLocalOnly,
		AllowedProviders: []pv.ProviderID{"openai"}, AllowedClasses: []pv.EgressEligibleClass{pv.EgressEligibleClassPublic},
		AllowedRegions: []pv.RegionCode{"us"},
	}
	first := must[*pv.EgressPolicy](t, "create")(p.SetEgressPolicy(ctx, update))
	if first.Revision != 1 || !first.Enabled || first.Approval != nil {
		t.Fatalf("created at revision 1, public data needs no approval, got %+v", first)
	}
	_, err := p.SetEgressPolicy(ctx, update)
	hasCode(t, "a stale revision", err, contracts.ErrConflict)
	update.ExpectedRevision = first.Revision
	update.AllowedClasses = []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal}
	second := must[*pv.EgressPolicy](t, "replace")(p.SetEgressPolicy(ctx, update))
	if second.Revision != 2 || second.Approval == nil {
		t.Fatalf("revision 2, letting internal data leave records an approval, got %+v", second)
	}
	view = must[*sdkgo.EgressPolicyView](t, "read")(p.GetEgressPolicy(ctx, pv.EgressLevelDeployment, ""))
	if view.Policy == nil || view.Policy.Revision != 2 {
		t.Fatalf("the stored policy reads back, got %+v", view.Policy)
	}
	class := pv.DataClassificationConfidential
	src := must[*pv.EgressPolicy](t, "source")(p.SetEgressPolicy(ctx, sdkgo.EgressPolicyUpdate{
		Level: pv.EgressLevelSource, SourceID: "src_01J0000000000000000000000S", SourceMode: pv.EgressModeAllowExcerpt, SourceClass: &class,
	}))
	if src.Target.SourceID == nil || *src.Target.SourceID != "src_01J0000000000000000000000S" {
		t.Fatalf("a source policy targets its source, got %+v", src.Target)
	}
}

func TestProvidersEgressPolicyViewNeedsThePolicyMember(t *testing.T) {
	for body, ok := range map[string]bool{`{"policy":null}`: true, `{}`: false, `{"policy":null,"x":1}`: false} {
		var v sdkgo.EgressPolicyView
		if err := json.Unmarshal([]byte(body), &v); (err == nil) != ok {
			t.Errorf("%s: accepted=%v, want %v (%v)", body, err == nil, ok, err)
		}
	}
}

// bodyRecorder keeps the body of every PUT the client sends.
type bodyRecorder struct {
	inner  http.RoundTripper
	mu     sync.Mutex
	bodies [][]byte
}

func (r *bodyRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPut && req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
	}
	return r.inner.RoundTrip(req)
}

// The files and audio settings are sent as the document's update body expects them, are left out when unset, and come
// back on the stored policy; a source policy has neither.
func TestProvidersEgressPolicyFilesAndAudioSettings(t *testing.T) {
	rec := &bodyRecorder{}
	h := newHarness(t, func(o *sdkgo.ClientOptions) {
		rec.inner = o.HTTPClient.Transport
		o.HTTPClient = &http.Client{Transport: rec}
	})
	h.claims = sdktest.Claims{PrincipalID: "principal-1"}
	h.fake.ConfigureProviders(sdktest.DefaultProvidersConfig())
	ctx := context.Background()
	p := h.client.Providers()

	base := sdkgo.EgressPolicyUpdate{
		Level: pv.EgressLevelWorkspace, Enabled: true, SourceMode: pv.EgressModeLocalOnly, AllowedClasses: []pv.EgressEligibleClass{pv.EgressEligibleClassPublic},
	}
	plain := must[*pv.EgressPolicy](t, "no settings")(p.SetEgressPolicy(ctx, base))
	if plain.Files != nil || plain.Audio != nil {
		t.Fatalf("a policy written without the settings has none, got %+v", plain)
	}
	files := pv.EgressPolicyContentEgress{Enabled: true, MaxClass: pv.EgressEligibleClassConfidential}
	audio := pv.EgressPolicyContentEgress{Enabled: true, MaxClass: pv.EgressEligibleClassPublic}
	with := base
	with.ExpectedRevision, with.Files, with.Audio = plain.Revision, &files, &audio
	stored := must[*pv.EgressPolicy](t, "with settings")(p.SetEgressPolicy(ctx, with))
	if stored.Files == nil || *stored.Files != files || stored.Audio == nil || *stored.Audio != audio || stored.Approval == nil {
		t.Fatalf("the settings are stored, and confidential file excerpts carry the approval, got %+v", stored)
	}
	view := must[*sdkgo.EgressPolicyView](t, "read")(p.GetEgressPolicy(ctx, pv.EgressLevelWorkspace, ""))
	if view.Policy == nil || view.Policy.Files == nil || *view.Policy.Files != files || view.Policy.Audio == nil || *view.Policy.Audio != audio {
		t.Fatalf("the settings read back, got %+v", view.Policy)
	}

	sch := loadSpec(t).requestSchema("putProviderEgressPolicy", t)
	if sch == nil || len(rec.bodies) != 2 {
		t.Fatalf("two bodies against the document's update schema, got %d", len(rec.bodies))
	}
	members := make([]map[string]json.RawMessage, len(rec.bodies))
	for i, body := range rec.bodies {
		if err := validateJSON(sch, body); err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		if err := json.Unmarshal(body, &members[i]); err != nil {
			t.Fatal(err)
		}
	}
	if _, has := members[0]["files"]; has {
		t.Fatalf("an unset files setting is left out, got %s", rec.bodies[0])
	}
	if _, has := members[0]["audio"]; has {
		t.Fatalf("an unset audio setting is left out, got %s", rec.bodies[0])
	}
	if string(members[1]["files"]) != `{"enabled":true,"max_class":"confidential"}` || string(members[1]["audio"]) != `{"enabled":true,"max_class":"public"}` {
		t.Fatalf("the settings are sent as {enabled, max_class}, got %s", rec.bodies[1])
	}

	class := pv.DataClassificationPublic
	source := sdkgo.EgressPolicyUpdate{Level: pv.EgressLevelSource, SourceID: "src_01J0000000000000000000000S", SourceMode: pv.EgressModeAllowExcerpt, SourceClass: &class}
	source.Audio = &audio
	_, err := p.SetEgressPolicy(ctx, source)
	hasCode(t, "audio at source level", err, contracts.ErrValidationFailed)
	source.Audio, source.Files = nil, &files
	_, err = p.SetEgressPolicy(ctx, source)
	hasCode(t, "files at source level", err, contracts.ErrValidationFailed)
}

// A stored policy with the files and audio settings reads back with both, as the generated type has them.
func TestProvidersEgressPolicyViewReadsTheFilesAndAudioSettings(t *testing.T) {
	body := `{"policy":{"policy_id":"epol_01J0000000000000000000000E","revision":1,"level":"workspace",` +
		`"target":{"deployment_id":"dep_fake","application_id":"fake-app","tenant_id":"tenant-1","workspace_id":"wsp-1"},` +
		`"enabled":true,"source_mode":"local_only","allowed_providers":[],"allowed_models":[],"allowed_classes":[],"allowed_regions":[],` +
		`"files":{"enabled":true,"max_class":"confidential"},"audio":{"enabled":false,"max_class":"public"},` +
		`"approval":{"approved_by":"user-1","approved_at":"2026-10-10T09:00:00Z"},"updated_at":"2026-10-10T09:00:00Z","updated_by":"user-1"}}`
	var v sdkgo.EgressPolicyView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("a policy with the files and audio settings: %v", err)
	}
	if v.Policy == nil || v.Policy.Files == nil || !v.Policy.Files.Enabled || v.Policy.Files.MaxClass != pv.EgressEligibleClassConfidential ||
		v.Policy.Audio == nil || v.Policy.Audio.Enabled || v.Policy.Audio.MaxClass != pv.EgressEligibleClassPublic {
		t.Fatalf("the settings did not read back: %+v", v.Policy)
	}
}

func TestProvidersBudgetsCreateChangeAndStates(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	created := must[*pv.Budget](t, "create")(p.SetBudget(ctx, testBudget(pv.BudgetSubjectKindProvider)))
	if created.BudgetID == "" || created.Revision != 1 || created.Limits.DailyMicroUnits != 5_000_000 {
		t.Fatalf("created with an id at revision 1, got %+v", created)
	}
	view := must[*sdkgo.ProviderBudgets](t, "list")(p.GetBudgets(ctx))
	if len(view.Budgets) != 1 || len(view.States) != 1 || view.States[0].State != pv.BudgetStatusOk || len(view.States[0].Windows) != 2 {
		t.Fatalf("one budget with its state, got %+v", view)
	}
	// changing it needs the revision that was read
	change := *created
	change.Limits.DailyMicroUnits = 1_000
	stale := change
	stale.Revision = 7
	_, err := p.SetBudget(ctx, stale)
	hasCode(t, "a stale revision", err, contracts.ErrConflict)
	changed := must[*pv.Budget](t, "change")(p.SetBudget(ctx, change))
	if changed.Revision != 2 || changed.Limits.DailyMicroUnits != 1_000 {
		t.Fatalf("changed at revision 2, got %+v", changed)
	}
	// spend moves the state to warning and then to exhausted, and the provider's status carries it
	h.fake.SetProviderBudgetSpend(string(changed.BudgetID), 850, 0)
	view = must[*sdkgo.ProviderBudgets](t, "warning")(p.GetBudgets(ctx))
	if view.States[0].State != pv.BudgetStatusWarning {
		t.Fatalf("85%% of the daily limit is a warning, got %+v", view.States[0])
	}
	h.fake.SetProviderBudgetSpend(string(changed.BudgetID), 1_000, 0)
	view = must[*sdkgo.ProviderBudgets](t, "exhausted")(p.GetBudgets(ctx))
	if view.States[0].State != pv.BudgetStatusExhausted || view.States[0].Windows[0].RemainingMicroUnits != 0 {
		t.Fatalf("nothing remains: exhausted, got %+v", view.States[0])
	}
	list := must[*sdkgo.ProviderList](t, "list")(p.List(ctx))
	for _, s := range list.Providers {
		if s.Provider.ProviderID == "openai" && (s.BudgetState == nil || *s.BudgetState != pv.BudgetStatusExhausted) {
			t.Fatalf("the connection status shows the budget state, got %+v", s.BudgetState)
		}
	}
	// the subject never changes
	moved := *changed
	moved.Subject.ProviderID = ptr(pv.ProviderID("acme"))
	_, err = p.SetBudget(ctx, moved)
	hasCode(t, "another subject is another budget", err, contracts.ErrValidationFailed)
}

func TestProvidersUsageExport(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	h.fake.SetProviderUsage(map[string]any{"records": []any{map[string]any{"provider_id": "openai", "toks_in": 12}}, "totals": []any{}})
	got := must[sdkgo.UsageExport](t, "export")(p.ExportUsage(ctx, sdkgo.UsageQuery{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Provider: "openai", Channel: pv.ChannelBackground, Scope: sdkgo.UsageScopeApplication,
	}))
	if records, _ := got["records"].([]any); len(records) != 1 {
		t.Fatalf("the usage records, got %+v", got)
	}
	reqs := h.fake.RequestsOf("exportProviderUsage")
	q, _ := url.ParseQuery(reqs[0].RawQuery)
	if q.Get("from") != "2026-10-01T00:00:00Z" || q.Get("provider") != "openai" || q.Get("channel") != "background" || q.Get("scope") != "application" || q.Get("format") != "json" {
		t.Fatalf("the query carries the selection, got %q", reqs[0].RawQuery)
	}
}

func TestProvidersOAuthFlowAndManualFallback(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	in := sdkgo.OAuthStartInput{Capabilities: []string{"chat"}, RedirectURI: "http://127.0.0.1:8123/callback", Label: "main"}

	_, err := p.StartOAuth(ctx, "acme", in)
	hasCode(t, "no OAuth for this provider", err, contracts.ErrValidationFailed)
	if !sdkgo.IsManualFallback(err) {
		t.Fatalf("the answer says to use the manual credential route, got %v", err)
	}
	if sdkgo.IsManualFallback(fmt.Errorf("something else")) {
		t.Fatal("only the manual_fallback answer is the fallback")
	}

	started := must[*sdkgo.OAuthStarted](t, "start")(p.StartOAuth(ctx, "openai", in))
	if started.RedirectURI != in.RedirectURI || started.Consent.ProviderName == "" || len(started.Consent.Scopes) == 0 {
		t.Fatalf("the consent names the provider and the scopes, got %+v", started)
	}
	if strings.Contains(started.AuthorizationURL, "verifier") {
		t.Fatal("the authorization URL holds no verifier")
	}
	state := oauthState(t, started)
	query := "code=abc123&state=" + url.QueryEscape(state)

	st := must[*pv.ConnectionStatus](t, "complete")(p.CompleteOAuth(ctx, sdkgo.OAuthCallbackInput{RedirectURI: in.RedirectURI, Query: "?" + query}))
	if st.State != pv.ConnectionStatusStateConnected || st.Kind == nil || *st.Kind != pv.CredentialKindOauth || st.Account.AccountID == nil {
		t.Fatalf("connected through OAuth as a safe account, got %+v", st)
	}
	_, err = p.CompleteOAuth(ctx, sdkgo.OAuthCallbackInput{RedirectURI: in.RedirectURI, Query: query})
	api := hasCode(t, "a replayed state", err, contracts.ErrConflict)
	if strings.Contains(err.Error(), state) || strings.Contains(err.Error(), "abc123") || strings.Contains(api.Err.Message, "abc123") {
		t.Fatalf("the error quotes the state or the code: %v", err)
	}

	// a wrong redirect and a provider error also fail closed and spend the state
	again := must[*sdkgo.OAuthStarted](t, "start again")(p.StartOAuth(ctx, "openai", in))
	_, err = p.CompleteOAuth(ctx, sdkgo.OAuthCallbackInput{RedirectURI: "http://127.0.0.1:1/other", Query: "code=x&state=" + url.QueryEscape(oauthState(t, again))})
	hasCode(t, "another redirect", err, contracts.ErrConflict)
}

func TestProvidersBillingNoticeIsAcknowledgedOnTheCurrentVersion(t *testing.T) {
	h := providersHarness(t, "principal-1")
	ctx := context.Background()
	p := h.client.Providers()
	confirm := sdkgo.BillingNoticeAckInput{NoticeVersion: "old", ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true}
	_, err := p.AcknowledgeBillingNotice(ctx, "openai", confirm)
	hasCode(t, "a stale notice version", err, contracts.ErrConflict)
	confirm.NoticeVersion = "2026-10-02.1"
	ack := must[*pv.BillingNoticeAck](t, "ack")(p.AcknowledgeBillingNotice(ctx, "openai", confirm))
	if ack.NoticeVersion != confirm.NoticeVersion || !ack.Confirmations.SeparateAPIBillingUnderstood || ack.AcknowledgedBy != "principal-1" {
		t.Fatalf("the acknowledgement records the version and the token's principal, got %+v", ack)
	}
	list := must[*sdkgo.ProviderList](t, "list")(p.List(ctx))
	for _, s := range list.Providers {
		if s.Provider.ProviderID == "openai" && !s.BillingNoticeAcknowledged {
			t.Fatal("the status shows the acknowledgement")
		}
	}
}

// exerciseProvidersConformance drives every providers and OAuth operation through the clients the contract test builds, so
// the recording transport checks every exchange against the OpenAPI document. plain is a client without the recorder: the
// budget create sends an empty budget_id (the server's documented create), which the document's request schema, the whole
// Budget, cannot describe.
func exerciseProvidersConformance(t *testing.T, fake *sdktest.Fake, recorded, plain func(sdktest.Claims) *sdkgo.Client) {
	t.Helper()
	ctx := context.Background()
	operator := sdktest.Claims{PrincipalID: "principal-1"}
	p := recorded(operator).Providers()
	_, err := p.List(ctx)
	hasCode(t, "providers before external mode is configured", err, contracts.ErrUnavailable)
	cfg := sdktest.DefaultProvidersConfig()
	cfg.TenantAdmins = []string{"admin-2"}
	fake.ConfigureProviders(cfg)

	must[*sdkgo.ProviderList](t, "list")(p.List(ctx))
	_, err = p.AcknowledgeBillingNotice(ctx, "openai", sdkgo.BillingNoticeAckInput{NoticeVersion: "stale", ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true})
	hasCode(t, "stale notice", err, contracts.ErrConflict)
	must[*pv.BillingNoticeAck](t, "ack")(p.AcknowledgeBillingNotice(ctx, "openai", sdkgo.BillingNoticeAckInput{NoticeVersion: "2026-10-02.1", ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true}))
	must[*pv.ConnectionStatus](t, "credential")(p.SubmitCredential(ctx, "acme", sdkgo.CredentialInput{Key: sdkgo.NewSecret(testKey), Label: "main", Scopes: []string{"model.request"}}))
	_, err = p.SubmitCredential(ctx, "acme", sdkgo.CredentialInput{Key: sdkgo.NewSecret("bad-key-value")})
	hasCode(t, "a refused credential", err, contracts.ErrValidationFailed)
	must[*pv.ConnectionStatus](t, "disconnect")(p.Disconnect(ctx, "acme"))

	in := sdkgo.OAuthStartInput{Capabilities: []string{"chat"}, RedirectURI: "http://127.0.0.1:8123/callback"}
	_, err = p.StartOAuth(ctx, "acme", in)
	if !sdkgo.IsManualFallback(err) {
		t.Fatalf("manual fallback expected, got %v", err)
	}
	started := must[*sdkgo.OAuthStarted](t, "start")(p.StartOAuth(ctx, "openai", in))
	query := "code=c0de&state=" + url.QueryEscape(oauthState(t, started))
	must[*pv.ConnectionStatus](t, "callback")(p.CompleteOAuth(ctx, sdkgo.OAuthCallbackInput{RedirectURI: in.RedirectURI, Query: query}))
	_, err = p.CompleteOAuth(ctx, sdkgo.OAuthCallbackInput{RedirectURI: in.RedirectURI, Query: query})
	hasCode(t, "replayed state", err, contracts.ErrConflict)

	popup := must[*sdkgo.OAuthStarted](t, "popup start")(p.StartOAuth(ctx, "openai", sdkgo.OAuthStartInput{
		Capabilities: []string{"chat"}, RedirectURI: "http://127.0.0.1/oauth/loopback",
		Presentation: "popup", Callback: "loopback", OpenerOrigin: "http://127.0.0.1:3000",
	}))
	if popup.FlowID == "" {
		t.Fatal("popup start returned no flow_id")
	}
	must[*pv.OauthFlowStatus](t, "flow status")(p.GetOAuthFlow(ctx, popup.FlowID))
	if err := p.CancelOAuthFlow(ctx, popup.FlowID); err != nil {
		t.Fatalf("cancel flow: %v", err)
	}
	if err := p.CancelOAuthFlow(ctx, popup.FlowID); err != nil {
		t.Fatalf("cancel again: %v", err)
	}

	must[*sdkgo.ProviderMode](t, "get mode")(p.GetMode(ctx))
	must[*sdkgo.ProviderMode](t, "set mode")(p.SetMode(ctx, sdkgo.ModeLevelDeployment, pv.DeploymentModeExternal))
	must[*pv.EgressDisclosure](t, "disclosure")(p.GetDisclosure(ctx, "openai", sdkgo.DisclosureQuery{SourceIDs: []string{"src_a"}}))

	must[*sdkgo.EgressPolicyView](t, "get policy")(p.GetEgressPolicy(ctx, pv.EgressLevelDeployment, ""))
	must[*pv.EgressPolicy](t, "put policy")(p.SetEgressPolicy(ctx, sdkgo.EgressPolicyUpdate{
		Level: pv.EgressLevelDeployment, Enabled: true, SourceMode: pv.EgressModeLocalOnly, AllowedProviders: []pv.ProviderID{"openai"},
		AllowedModels:  []pv.ModelRef{{ProviderID: "openai", ModelID: "gpt-4.1-mini"}},
		AllowedClasses: []pv.EgressEligibleClass{pv.EgressEligibleClassPublic}, AllowedRegions: []pv.RegionCode{"us"},
	}))
	must[*pv.EgressPolicy](t, "put policy with files and audio")(p.SetEgressPolicy(ctx, sdkgo.EgressPolicyUpdate{
		Level: pv.EgressLevelDeployment, ExpectedRevision: 1, Enabled: true, SourceMode: pv.EgressModeLocalOnly,
		AllowedClasses: []pv.EgressEligibleClass{pv.EgressEligibleClassPublic},
		Files:          &pv.EgressPolicyContentEgress{Enabled: true, MaxClass: pv.EgressEligibleClassInternal},
		Audio:          &pv.EgressPolicyContentEgress{Enabled: false, MaxClass: pv.EgressEligibleClassPublic},
	}))
	must[*sdkgo.EgressPolicyView](t, "get policy again")(p.GetEgressPolicy(ctx, pv.EgressLevelDeployment, ""))

	created := must[*pv.Budget](t, "create a budget")(plain(operator).Providers().SetBudget(ctx, testBudget(pv.BudgetSubjectKindTenant)))
	change := *created
	change.Limits.DailyMicroUnits = 9_000_000
	must[*pv.Budget](t, "change a budget")(p.SetBudget(ctx, change))
	_, err = p.SetBudget(ctx, change)
	hasCode(t, "a stale budget revision", err, contracts.ErrConflict)
	must[*sdkgo.ProviderBudgets](t, "budgets")(p.GetBudgets(ctx))
	must[sdkgo.UsageExport](t, "usage")(p.ExportUsage(ctx, sdkgo.UsageQuery{Provider: "openai"}))

	must[*sdkgo.KillSwitch](t, "kill")(p.GetKillSwitch(ctx))
	must[*sdkgo.KillSwitch](t, "engage")(p.SetKillSwitch(ctx, true, "incident"))
	must[*sdkgo.KillSwitch](t, "release")(p.SetKillSwitch(ctx, false, ""))

	// a caller who is not an operator, and a tenant administrator asking for a deployment-only route
	_, err = recorded(sdktest.Claims{PrincipalID: "nobody"}).Providers().List(ctx)
	hasCode(t, "nobody", err, contracts.ErrPermissionDenied)
	_, err = recorded(sdktest.Claims{PrincipalID: "admin-2"}).Providers().GetKillSwitch(ctx)
	hasCode(t, "tenant admin on a deployment route", err, contracts.ErrPermissionDenied)
	fake.SetProvidersUnavailable(true)
	_, err = p.List(ctx)
	hasCode(t, "switched off", err, contracts.ErrUnavailable)
	fake.SetProvidersUnavailable(false)
}

func identifier(s string) common.Identifier { return common.Identifier(s) }
