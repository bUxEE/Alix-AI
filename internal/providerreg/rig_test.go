package providerreg

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The synthetic deployment the tests run against. Nothing here is a real key, account or URL.
const (
	tApp     = "app_crm"
	tOther   = "app_other"
	tTenant  = "ten_42"
	tTenant2 = "ten_43"
	tWsp     = "wsp_main"
	tWsp2    = "wsp_second"
	tAdmin   = "usr_admin"

	// tSecret is a TEST-ONLY canary that stands for a provider key (not a real key). It must never be stored.
	tSecret = "TESTONLY-canary-key-0000000000000000"
)

var bg = context.Background()

var tStart = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// credRef returns a well-formed credential reference; n tells them apart.
func credRef(n byte) providers.CredentialRefID {
	return providers.CredentialRefID("cred_" + strings.Repeat("0", 25) + string('A'+rune(n)))
}

// fakeCreds is a scripted credential service: it knows statuses by reference and counts calls.
type fakeCreds struct {
	mu     sync.Mutex
	status map[providers.CredentialRefID]providers.CredentialRefStatus
	err    error
	calls  int
}

func (f *fakeCreds) CredentialStatus(_ context.Context, _ contracts.Scope, ref providers.CredentialRefID) (providers.CredentialRefStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if s, ok := f.status[ref]; ok {
		return s, nil
	}
	return providers.CredentialRefStatusInvalid, nil
}

func (f *fakeCreds) set(ref providers.CredentialRefID, s providers.CredentialRefStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[ref] = s
}

func (f *fakeCreds) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type rig struct {
	t     *testing.T
	db    *store.DB
	reg   *Registry
	clk   *clock.Fake
	creds *fakeCreds
	scope contracts.Scope // the operator scope: tApp / tTenant / tWsp
	next  byte
}

func newRig(t *testing.T) *rig {
	t.Helper()
	sr := store.NewRegistry()
	if err := sr.Register("providerreg", migrations); err != nil {
		t.Fatalf("registering migrations: %v", err)
	}
	db := storetest.Open(t, sr)
	clk := clock.NewFake(tStart)
	creds := &fakeCreds{status: map[providers.CredentialRefID]providers.CredentialRefStatus{}}
	reg, err := New(db, Config{Clock: clk, Credentials: creds})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := storetest.Scope(tApp, tTenant, tWsp)
	s.PrincipalID = tAdmin
	return &rig{t: t, db: db, reg: reg, clk: clk, creds: creds, scope: s}
}

// scopeOf returns the operator's scope moved to another application, tenant and workspace.
func (g *rig) scopeOf(app, tenant, wsp string) contracts.Scope {
	s := storetest.Scope(app, tenant, wsp)
	s.PrincipalID = tAdmin
	return s
}

// draft is a text-only profile of a scripted provider on a loopback mock, permitted for one
// tenant of tApp, allowed public and internal data, reviewed on the rig's start date.
func (g *rig) draft() providers.ProviderProfile {
	tenant := common.Identifier(tTenant)
	url := "https://billing.example.com/api"
	return providers.ProviderProfile{
		ProviderID:       "fake",
		Endpoint:         "http://127.0.0.1:8099/v1",
		ModelID:          "scripted",
		ModelVersion:     "v1",
		Roles:            []providers.Role{providers.RoleConversationalController, providers.RoleLanguageRealization},
		Modalities:       providers.ProviderProfileModalities{Input: []providers.Modality{providers.ModalityText}, Output: []providers.Modality{providers.ModalityText}},
		Streaming:        true,
		StructuredOutput: providers.ProviderProfileStructuredOutputStrictSchema,
		ToolCalling:      providers.ProviderProfileToolCalling{Supported: true, StrictArguments: true, ParallelCalls: true, MaxTools: ptr(int64(16))},
		Limits:           providers.ProviderProfileLimits{MaxContextToks: 128000, MaxOutputToks: 8192, MaxRequestBytes: 1 << 20},
		ImageGeneration:  providers.ProviderProfileImageGeneration{OutputRetention: providers.RetentionModeUnknown},
		Voice:            providers.ProviderProfileVoice{AudioRetention: providers.RetentionModeUnknown},
		Terms: providers.ProviderProfileTerms{
			Retention: providers.RetentionModeZero, TrainingUse: providers.TrainingUseNever,
			TermsHash: providers.SHA256("sha256:" + strings.Repeat("a", 64)), RecordedOn: "2026-10-02",
		},
		Region: providers.ProviderProfileRegion{Residency: providers.ResidencyUnknown},
		RateLimits: providers.ProviderProfileRateLimits{
			Retry: providers.ProviderProfileRateLimitsRetry{MaxAttempts: 2, InitialBackoffMS: 100, MaxBackoffMS: 1000, HonorsRetryAfter: true},
		},
		Pricing: providers.PriceTable{
			Currency: "USD", EffectiveOn: "2026-10-01",
			Prices: providers.PriceTablePrices{ToksIn: &providers.PriceTablePrice{PerUnits: 1000000, MicroUnits: 150000}},
		},
		UsageMetering:      providers.ProviderProfileUsageMeteringPerResponse,
		PermittedScopes:    []providers.ProviderProfileScopeGrant{{ApplicationID: tApp, TenantID: &tenant}},
		AllowedDataClasses: []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal},
		LastPolicyReview:   "2026-10-02",
		ReviewIntervalDays: 30,
		BillingNotice: providers.ProviderProfileBillingNotice{
			Version: "2026-10-02.1", URL: &url,
			Text: "A consumer chat subscription does not include API access. API usage is billed separately by the provider.",
		},
	}
}

func ptr[T any](v T) *T { return &v }

// prof fails the test on an error and otherwise returns the profile; it is written to take a
// call that returns (profile, error) directly: g.prof(g.reg.Get(...)).
func (g *rig) prof(p providers.ProviderProfile, err error) providers.ProviderProfile {
	g.t.Helper()
	if err != nil {
		g.t.Fatalf("unexpected error: %v", err)
	}
	return p
}

// register stores a draft (after mutate) and returns the candidate.
func (g *rig) register(mutate ...func(*providers.ProviderProfile)) providers.ProviderProfile {
	g.t.Helper()
	d := g.draft()
	for _, m := range mutate {
		m(&d)
	}
	return g.prof(g.reg.Register(bg, g.scope, d))
}

// reportFor builds a fully passing conformance report for the profile's exact model, completed
// at done. failCheck, when set, is a "group.check" whose result is flipped to failed.
func (g *rig) reportFor(p providers.ProviderProfile, done time.Time, failCheck string) providers.ConformanceReport {
	g.t.Helper()
	image, file, audio := declaredMedia(p)
	checks := map[string][]string{
		"inference":  {"text_generation_and_streaming", "bounded_context", "structured_output", "cancellation_and_timeout", "usage_reporting", "malformed_and_error_mapping"},
		"control":    {"typed_task_proposal", "unknown_capability_refusal", "argument_schema_compliance", "no_direct_adapter_invocation", "directive_and_receipt_handling", "confirmation_and_cancellation"},
		"rag":        {"authorized_excerpt_handling", "source_reference_preservation", "no_unsupported_citations", "prompt_injection_resistance", "local_only_source_enforcement", "no_path_or_cred_leakage"},
		"multimodal": {"image_input_output", "file_input_output", "audio_input_output", "cancellation_and_partial_stream", "sanitized_rich_blocks"},
		"operations": {"creds_never_in_responses_or_logs", "quota_and_budget_enforcement", "provider_disablement", "fallback_and_outage_behavior", "tenant_isolation", "audit_completeness", "terms_and_capability_profile_validation"},
	}
	declared := map[string]bool{"image_input_output": image, "file_input_output": file, "audio_input_output": audio}
	groups := map[string]map[string]any{}
	outcome := "passed"
	for grp, names := range checks {
		groups[grp] = map[string]any{}
		for _, n := range names {
			res, reason := "passed", any(nil)
			if d, conditional := declared[n]; conditional && !d {
				res = "not_applicable"
			}
			if failCheck == grp+"."+n {
				res, reason, outcome = "failed", "check_failed", "failed"
			}
			groups[grp][n] = map[string]any{"result": res, "reason": reason}
		}
	}
	doc := map[string]any{
		"report_id":  "conf_" + strings.Repeat("0", 25) + "R",
		"provider":   map[string]any{"profile_id": p.ProfileID, "provider_id": p.ProviderID, "model_id": p.ModelID, "model_version": p.ModelVersion},
		"suite":      map[string]any{"version": "1.0.0", "mode": "fixtures"},
		"declared":   map[string]any{"image": image, "file": file, "audio": audio},
		"started_at": done.Add(-4 * time.Minute).UTC().Format(time.RFC3339), "completed_at": done.UTC().Format(time.RFC3339),
		"outcome": outcome, "groups": groups,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		g.t.Fatal(err)
	}
	var rep providers.ConformanceReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		g.t.Fatalf("building the report: %v", err)
	}
	return rep
}

// ack acknowledges the profile's current billing notice as the operator.
func (g *rig) ack(p providers.ProviderProfile) providers.ProviderProfile {
	g.t.Helper()
	return g.prof(g.reg.AcknowledgeBillingNotice(bg, g.scope, p.ProfileID, Acknowledgement{
		NoticeVersion: p.BillingNotice.Version, ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true,
	}))
}

// qualified registers a profile and takes it through every step to production qualification.
// ref is the credential reference to bind, set active in the credential service.
func (g *rig) qualified(ref providers.CredentialRefID, mutate ...func(*providers.ProviderProfile)) providers.ProviderProfile {
	g.t.Helper()
	p := g.register(mutate...)
	g.creds.set(ref, providers.CredentialRefStatusActive)
	p = g.prof(g.reg.BindCredential(bg, g.scope, p.ProfileID, ref))
	g.ack(p)
	g.prof(g.reg.RecordConformance(bg, g.scope, p.ProfileID, g.reportFor(p, g.clk.Now().Add(-time.Hour), "")))
	return g.prof(g.reg.Qualify(bg, g.scope, p.ProfileID))
}

// req is a request every default profile can serve.
func req() Requirement {
	return Requirement{
		Role: providers.RoleConversationalController, DataClass: providers.DataClassificationInternal,
		TypedActionProposals: true, ToolCount: 3, StructuredOutput: providers.ProviderProfileStructuredOutputStrictSchema,
		Streaming: true, Input: []providers.Modality{providers.ModalityText}, Output: []providers.Modality{providers.ModalityText},
		ContextToks: 4000, OutputToks: 512, RequestBytes: 20000,
	}
}

// send counts how often the sender ran.
type sender struct{ calls int }

func (s *sender) send(context.Context, Admission) error { s.calls++; return nil }

// route runs Route with a counting sender and returns the call count and the error.
func (g *rig) route(scope contracts.Scope, id providers.ProfileID, r Requirement) (int, error) {
	var s sender
	err := g.reg.Route(bg, scope, id, r, s.send)
	return s.calls, err
}

// wantReason asserts err is a typed error with the code and reason.
func wantReason(t *testing.T, err error, code contracts.ErrCode, reason Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s/%s, got nil", code, reason)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("code = %s, want %s (%v)", got, code, err)
	}
	if got := ReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q (%v)", got, reason, err)
	}
	if contracts.CodeOf(err).Retryable() {
		t.Fatalf("code %s is retryable: a caller could mistake it for a reason to try elsewhere", contracts.CodeOf(err))
	}
}
