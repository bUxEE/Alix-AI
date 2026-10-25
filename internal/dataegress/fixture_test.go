package dataegress

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The synthetic deployment the tests run against.
const (
	tDeployment = "dep_test"
	tApp        = "app_crm"
	tOtherApp   = "app_other"
	tTenantA    = "ten_acme"
	tTenantB    = "ten_globex"
	tWsp1       = "wsp_main"
	tWsp2       = "wsp_second"
	tUser       = "usr_ada"
	tOperator   = "opr_root"

	tProvider = providers.ProviderID("openai")
	tModel    = providers.ModelID("gpt-4.1-mini")
)

var tNow = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// Sentinels planted in content: none may appear in any record, error or wire payload
// that must not carry content. They are synthetic and match no real secret.
func tSecret() string { return "sk-" + "SENTINEL0123456789abcdef" }

const (
	tUserMarker    = "USERMARKER-7f3a"
	tExcerptMarker = "EXCERPTMARKER-9c1d"
	tLocalMarker   = "LOCALFIELD-4b2e"
	tPathMarker    = "pathmarker"
)

// allCaps are the capability scopes of a fully authorised operator principal.
var allCaps = []string{
	CapabilityRead, CapabilityApprove,
	capabilityApplicationWrite, capabilityTenantWrite, capabilityWorkspaceWrite, capabilitySourceWrite,
}

func scopeOf(deployment, app, tenant, wsp, principal string, caps ...string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: deployment, ApplicationID: app, TenantID: tenant, WorkspaceID: wsp, PrincipalID: principal,
		Roles: []string{"member"}, CapabilityScopes: caps, SessionID: "ses_" + principal,
	}
}

// ada is a fully authorised principal of tenant A, workspace 1.
func ada() contracts.Scope { return scopeOf(tDeployment, tApp, tTenantA, tWsp1, tUser, allCaps...) }

// failingAudit wraps the real log and can be made to fail.
type failingAudit struct {
	audit.Appender
	mu   sync.Mutex
	fail error
	n    int
}

func (f *failingAudit) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *failingAudit) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	f.mu.Lock()
	err := f.fail
	f.n++
	f.mu.Unlock()
	if err != nil {
		return audit.Event{}, err
	}
	return f.Appender.AppendAt(ctx, loc, ev)
}

func (f *failingAudit) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	f.mu.Lock()
	err := f.fail
	f.n++
	f.mu.Unlock()
	if err != nil {
		return audit.Event{}, err
	}
	return f.Appender.AppendTx(ctx, tx, loc, ev)
}

type fixture struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.Fake
	log   *audit.Log
	audit *failingAudit
	svc   *Service
	prof  providers.ProviderProfile
	scope contracts.Scope
	op    Operator
}

// newFixture builds a service over a real store and audit log, with a qualified
// provider profile and NO policy: external mode is off, as it is by default.
func newFixture(t *testing.T, mut ...func(*Deps)) *fixture {
	t.Helper()
	f := &fixture{t: t, clk: clock.NewFake(tNow), scope: ada(), op: Operator{DeploymentID: tDeployment, PrincipalID: tOperator}}
	f.db = storetest.Open(t, nil)
	var err error
	if f.log, err = audit.New(f.db, audit.Options{Clock: f.clk}); err != nil {
		t.Fatal(err)
	}
	f.audit = &failingAudit{Appender: f.log}
	d := Deps{DB: f.db, Clock: f.clk, Audit: f.audit, IDs: ids.NewGenerator(f.clk)}
	for _, m := range mut {
		m(&d)
	}
	if f.svc, err = New(d); err != nil {
		t.Fatal(err)
	}
	f.prof = loadProfile(t)
	return f
}

// loadProfile reads the qualified example profile of schemas/providers and widens
// its scopes to the test application.
func loadProfile(t testing.TB) providers.ProviderProfile {
	t.Helper()
	raw, err := os.ReadFile("../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var p providers.ProviderProfile
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	p.PermittedScopes = []providers.ProviderProfileScopeGrant{{ApplicationID: tApp}}
	return p
}

// clone returns a deep copy of the profile, so a test can change it freely.
func cloneProfile(t testing.TB, p providers.ProviderProfile) providers.ProviderProfile {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out providers.ProviderProfile
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// basePolicy is the policy every level of the baseline carries: enabled, the one
// provider and model, public and internal data, two regions, excerpts allowed.
func basePolicy(level providers.EgressLevel) PolicyInput {
	return PolicyInput{
		Level: level, Enabled: true, SourceMode: providers.EgressModeAllowExcerpt,
		AllowedProviders: []providers.ProviderID{tProvider},
		AllowedModels:    []providers.ModelRef{{ProviderID: tProvider, ModelID: tModel}},
		AllowedClasses:   []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal},
		AllowedRegions:   []providers.RegionCode{"us", "eu"},
	}
}

// put writes a policy of the fixture scope (or, for the deployment level, as the
// operator) over whatever is there, after applying mut to the baseline.
func (f *fixture) put(level providers.EgressLevel, mut func(*PolicyInput)) providers.EgressPolicy {
	f.t.Helper()
	return f.putAs(f.scope, level, "", mut)
}

func (f *fixture) putAs(scope contracts.Scope, level providers.EgressLevel, source providers.SourceID, mut func(*PolicyInput)) providers.EgressPolicy {
	f.t.Helper()
	in := basePolicy(level)
	in.SourceID = source
	if mut != nil {
		mut(&in)
	}
	var cur providers.EgressPolicy
	var found bool
	var err error
	if level == providers.EgressLevelDeployment {
		cur, found, err = f.svc.GetPolicy(context.Background(), scope, level, "")
	} else {
		cur, found, err = f.svc.GetPolicy(context.Background(), scope, level, source)
	}
	if err != nil {
		f.t.Fatalf("GetPolicy(%s): %v", level, err)
	}
	if found {
		in.ExpectedRevision = cur.Revision
	}
	var p providers.EgressPolicy
	if level == providers.EgressLevelDeployment {
		p, err = f.svc.PutDeploymentPolicy(context.Background(), f.op, in)
	} else {
		p, err = f.svc.PutPolicy(context.Background(), scope, in)
	}
	if err != nil {
		f.t.Fatalf("PutPolicy(%s): %v", level, err)
	}
	return p
}

// enableAll writes the baseline policy at the four request levels of the scope.
func (f *fixture) enableAll() {
	f.t.Helper()
	f.enableAllFor(f.scope)
}

func (f *fixture) enableAllFor(scope contracts.Scope) {
	f.t.Helper()
	f.putAs(scope, providers.EgressLevelDeployment, "", nil)
	for _, lv := range []providers.EgressLevel{providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace} {
		f.putAs(scope, lv, "", nil)
	}
}

// newSource registers a source policy of the fixture workspace and returns its id.
func (f *fixture) newSource(mode providers.EgressMode, class providers.DataClassification, mut func(*PolicyInput)) providers.SourceID {
	f.t.Helper()
	return f.newSourceAs(f.scope, mode, class, mut)
}

func (f *fixture) newSourceAs(scope contracts.Scope, mode providers.EgressMode, class providers.DataClassification, mut func(*PolicyInput)) providers.SourceID {
	f.t.Helper()
	id := providers.SourceID(ids.New("src"))
	cls := class
	f.putAs(scope, providers.EgressLevelSource, id, func(in *PolicyInput) {
		in.SourceMode, in.SourceClass = mode, &cls
		if el := providers.EgressEligibleClass(class); el.Valid() {
			in.AllowedClasses = []providers.EgressEligibleClass{el}
		} else {
			in.AllowedClasses = nil
		}
		if mut != nil {
			mut(in)
		}
	})
	return id
}

// userPart is an internal-class user message.
func userPart(text string) Part {
	return Part{Kind: PartText, Text: text, Category: string(providers.SendCategoryUserMessage), Class: string(providers.DataClassificationInternal)}
}

func statePart(text string) Part {
	return Part{Kind: PartText, Text: text, Category: string(providers.SendCategoryStructuredState), Class: string(providers.DataClassificationInternal)}
}

// excerptPart is a RAG excerpt of an internal source.
func excerptPart(src providers.SourceID, text, label string) Part {
	return Part{Kind: PartText, Text: text, Label: label, Category: string(providers.SendCategoryRAGExcerpt), Class: string(providers.DataClassificationInternal), SourceID: string(src)}
}

// candidate is the baseline candidate: a user message and a piece of structured
// state, both internal, plus whatever parts the test adds in a second message.
func (f *fixture) candidate(extra ...Part) Candidate {
	c := Candidate{
		Scope: f.scope, Profile: f.prof, Role: providers.RoleConversationalController, Channel: providers.ChannelInteractive,
		TraceID:      "trc_test",
		Instructions: "You are the application controller. Follow the directive and answer briefly.",
		Messages: []Message{{
			Role:  providers.RequestEnvelopeMessageRoleUser,
			Parts: []Part{userPart("Show my open orders " + tUserMarker), statePart(`{"status":"open"}`)},
		}},
		ResponseFormat: providers.RequestEnvelopeResponseFormat{Kind: providers.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  1024, TimeoutMS: 30000,
	}
	if len(extra) > 0 {
		c.Messages = append(c.Messages, Message{Role: providers.RequestEnvelopeMessageRoleToolResult, Parts: extra})
	}
	return c
}

func (f *fixture) eval(c Candidate) (Result, error) {
	f.t.Helper()
	return f.svc.Evaluate(context.Background(), c)
}

// mustAllow evaluates the candidate and fails unless it is allowed; it validates
// what came back against the schemas.
func (f *fixture) mustAllow(c Candidate) Result {
	f.t.Helper()
	res, err := f.eval(c)
	if err != nil {
		f.t.Fatalf("Evaluate: want allow, got %v (decision %+v)", err, res.Decision.Checks)
	}
	if res.Envelope == nil || res.Decision.Outcome != providers.EgressDecisionOutcomeAllow || res.Decision.Reason != providers.EgressDecisionReasonAllowed {
		f.t.Fatalf("allowed result without an envelope or with a deny decision: %+v", res.Decision)
	}
	assertValid(f.t, urnDecision, res.Decision)
	assertValid(f.t, urnEnvelope, res.Envelope)
	assertValid(f.t, urnDisclosure, res.Disclosure)
	return res
}

// mustDeny evaluates the candidate and fails unless it is denied with the reason
// at the check. It validates the decision and checks the typed denial.
func (f *fixture) mustDeny(c Candidate, reason providers.EgressDecisionReason, check providers.EgressDecisionCheckName) (Result, *contracts.Error) {
	f.t.Helper()
	res, err := f.eval(c)
	if err == nil {
		f.t.Fatalf("Evaluate: want deny %s at %s, got an allow", reason, check)
	}
	var ce *contracts.Error
	if !asContractsError(err, &ce) || ce.Code != contracts.ErrPermissionDenied {
		f.t.Fatalf("denial is not permission_denied: %v", err)
	}
	if res.Envelope != nil {
		f.t.Fatalf("a denial returned an envelope")
	}
	if res.Decision.Outcome != providers.EgressDecisionOutcomeDeny || res.Decision.Reason != reason {
		f.t.Fatalf("decision = %s/%s, want deny/%s (checks %+v)", res.Decision.Outcome, res.Decision.Reason, reason, res.Decision.Checks)
	}
	last := res.Decision.Checks[len(res.Decision.Checks)-1]
	if last.Name != check || last.Passed {
		f.t.Fatalf("last check = %+v, want a failed %s", last, check)
	}
	if len(res.Decision.Categories.Sent) != 0 || res.Decision.RAGExcerptsIncluded {
		f.t.Fatalf("a denial sent categories: %+v", res.Decision.Categories)
	}
	if ce.Details["rule"] != string(reason) || ce.Details["decision_id"] != string(res.Decision.DecisionID) {
		f.t.Fatalf("denial details do not name the rule: %v", ce.Details)
	}
	assertValid(f.t, urnDecision, res.Decision)
	return res, ce
}

func asContractsError(err error, target **contracts.Error) bool {
	for err != nil {
		if e, ok := err.(*contracts.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// wire is what an adapter may send: the payload of the envelope.
func wire(t testing.TB, env *providers.RequestEnvelope) string {
	t.Helper()
	raw, err := json.Marshal(env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// whole is the entire envelope, internal metadata included.
func whole(t testing.TB, env *providers.RequestEnvelope) string {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertAbsent(t testing.TB, where, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			t.Errorf("%s contains %q", where, n)
		}
	}
}

func assertPresent(t testing.TB, where, haystack string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			t.Errorf("%s does not contain %q", where, n)
		}
	}
}

// decisionEvents returns the data of the dataegress.decision events of one tenant chain.
func (f *fixture) decisionEvents(app, tenant string) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		"SELECT data FROM audit_events WHERE event_type = ? AND application_id = ? AND tenant_id = ? ORDER BY chain_seq", string(EventDecision.Name()), app, tenant)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func identifier(s string) common.Identifier { return common.Identifier(s) }

func hasCategory[T ~string](list []T, c T) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

var testCtx = context.Background()
