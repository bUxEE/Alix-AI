package dataegress

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestDecisionAuditRecordHasNoContent(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	secret := tSecret()
	sentinels := []string{secret, tPathMarker, tUserMarker, tExcerptMarker, tLocalMarker, "Refund policy title"}

	c := f.candidate(excerptPart(src, "body "+tExcerptMarker, "Refund policy title"))
	c.Messages[0].Parts[0].Text = "open /home/ada/" + tPathMarker + "/x with " + secret + " " + tUserMarker
	lo := statePart("home " + tLocalMarker)
	lo.LocalOnly = true
	c.Messages[0].Parts = append(c.Messages[0].Parts, lo)
	allowed := f.mustAllow(c)

	// A denial, with content planted in every place content can be.
	bad := f.candidate(excerptPart(src, "body "+tExcerptMarker, "Refund policy title"))
	bad.Messages[0].Parts[0].Text = secret + " " + tUserMarker + " /home/ada/" + tPathMarker
	restricted := statePart("rs " + tLocalMarker)
	restricted.Class = string(providers.DataClassificationRestricted)
	bad.Messages[0].Parts = append(bad.Messages[0].Parts, restricted)
	denied, ce := f.mustDeny(bad, providers.EgressDecisionReasonRestrictedData, providers.EgressDecisionCheckNameClassification)

	// Nothing of the content is in the error, its details or the returned decisions.
	assertAbsent(t, "denial error", ce.Error(), sentinels...)
	if raw, err := json.Marshal(ce.Details); err != nil {
		t.Fatal(err)
	} else {
		assertAbsent(t, "denial details", string(raw), sentinels...)
	}
	for name, d := range map[string]providers.EgressDecision{"allow": allowed.Decision, "deny": denied.Decision} {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		assertAbsent(t, name+" decision", string(raw), sentinels...)
	}
	assertAbsent(t, "disclosure", mustJSON(t, allowed.Disclosure), sentinels...)

	// What was stored: one record per decision, in the tenant chain of the scope, valid by schema, with no content.
	events := f.decisionEvents(tApp, tTenantA)
	if len(events) != 2 {
		t.Fatalf("decision events = %d, want one per decision", len(events))
	}
	for i, data := range events {
		assertAbsent(t, "audit record", data, sentinels...)
		assertAbsent(t, "audit record", data, `"text"`, `"label"`, `"instructions"`)
		if msg := schemaViolations(t, urnDecision, []byte(data)); msg != "" {
			t.Errorf("record %d does not validate: %s", i, msg)
		}
	}
	var stored providers.EgressDecision
	if err := json.Unmarshal([]byte(events[0]), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.DecisionID != allowed.Decision.DecisionID || stored.Outcome != providers.EgressDecisionOutcomeAllow ||
		stored.Provider.ProviderID != tProvider || stored.Provider.ModelID != tModel || stored.Provider.ModelVersion != f.prof.ModelVersion || stored.Provider.ProfileID != f.prof.ProfileID {
		t.Errorf("the record names the provider, model, version and decision: %+v", stored)
	}
	if !hasCategory(stored.Categories.Sent, providers.SendCategoryUserMessage) || !hasCategory(stored.Categories.Sent, providers.SendCategoryRAGExcerpt) ||
		!hasCategory(stored.Categories.Stripped, providers.BlockedCategoryCredential) || !hasCategory(stored.Categories.Stripped, providers.BlockedCategoryFilesystemPath) ||
		!hasCategory(stored.Categories.Stripped, providers.BlockedCategoryLocalOnlyField) {
		t.Errorf("the record lists sent and stripped categories: %+v", stored.Categories)
	}
	var storedDeny providers.EgressDecision
	if err := json.Unmarshal([]byte(events[1]), &storedDeny); err != nil {
		t.Fatal(err)
	}
	if storedDeny.Outcome != providers.EgressDecisionOutcomeDeny || storedDeny.Reason != providers.EgressDecisionReasonRestrictedData || storedDeny.Code == nil || *storedDeny.Code != "permission_denied" {
		t.Errorf("the denial is recorded as a denial: %+v", storedDeny)
	}
	if got := f.principalsOf(string(EventDecision.Name())); len(got) != 2 || got[0] != tUser {
		t.Errorf("the record is attributed to the verified principal: %v", got)
	}
	// Other tenants' chains hold nothing of it.
	if n := len(f.decisionEvents(tApp, tTenantB)) + len(f.decisionEvents(tOtherApp, tTenantA)); n != 0 {
		t.Errorf("records leaked into other chains: %d", n)
	}

	// The record is written before the envelope is returned: if it cannot be written, nothing is returned.
	f.audit.set(errors.New("audit store down"))
	res, err := f.eval(f.candidate())
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied || res.Envelope != nil || res.Decision.DecisionID != "" {
		t.Fatalf("an allow that cannot be recorded must not be returned: %v %+v", err, res.Envelope)
	}
	var ce2 *contracts.Error
	if !asContractsError(err, &ce2) || ce2.Details["rule"] != "guard_failure" {
		t.Errorf("details = %v", ce2)
	}
	// A denial stays a denial when it cannot be recorded, and says so.
	res, err = f.eval(bad)
	var ce3 *contracts.Error
	if !asContractsError(err, &ce3) || ce3.Code != contracts.ErrPermissionDenied || ce3.Details["audit_recorded"] != false || res.Envelope != nil {
		t.Errorf("an unrecorded denial: %v", err)
	}
	f.audit.set(nil)
	if n := len(f.decisionEvents(tApp, tTenantA)); n != 2 {
		t.Errorf("failed appends left records behind: %d", n)
	}
}

func (f *fixture) principalsOf(typ string) []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(testCtx, "SELECT principal_id FROM audit_events WHERE event_type = ? ORDER BY occurred_at, chain_seq", typ)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestDisclosureListsProviderModelAndCategories(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	excerpt := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	metadata := f.newSource(providers.EgressModeAllowMetadata, providers.DataClassificationInternal, nil)
	localOnly := f.newSource(providers.EgressModeLocalOnly, providers.DataClassificationInternal, nil)
	in := DisclosureInput{Role: providers.RoleConversationalController, Sources: []providers.SourceID{excerpt}, EstimatedToksIn: 1000, MaxOutputToks: 1024}

	d, err := f.svc.Disclose(testCtx, f.scope, f.prof, in)
	if err != nil {
		t.Fatal(err)
	}
	assertValid(t, urnDisclosure, d)
	if !d.ExternalEnabled || d.Provider.ProviderID != tProvider || d.Provider.ModelID != tModel || d.Provider.ModelVersion != f.prof.ModelVersion || d.Provider.ProfileID != f.prof.ProfileID {
		t.Errorf("provider and model: %+v", d.Provider)
	}
	if got := d.DataClasses; len(got) != 2 || got[0] != providers.EgressEligibleClassPublic || got[1] != providers.EgressEligibleClassInternal {
		t.Errorf("classes = %v", got)
	}
	for _, c := range []providers.SendCategory{providers.SendCategoryUserMessage, providers.SendCategoryStructuredState, providers.SendCategoryRAGExcerpt, providers.SendCategoryRAGMetadata} {
		if !hasCategory(d.DataCategories, c) {
			t.Errorf("categories = %v, want %s", d.DataCategories, c)
		}
	}
	if hasCategory(d.DataCategories, providers.SendCategoryImageContent) || hasCategory(d.DataCategories, providers.SendCategoryFileContent) {
		t.Errorf("a text-only profile discloses no media: %v", d.DataCategories)
	}
	if !d.RAGExcerptsIncluded {
		t.Error("RAG excerpts may be included")
	}
	if len(d.Regions) != 2 || d.Regions[0] != "us" || d.Regions[1] != "eu" {
		t.Errorf("regions = %v", d.Regions)
	}
	if d.Terms.Retention != providers.RetentionModeLimited || d.Terms.RetentionDays == nil || *d.Terms.RetentionDays != 30 || d.Terms.TrainingUse != providers.TrainingUseNever || d.Terms.Residency != providers.ResidencySelectable {
		t.Errorf("terms = %+v", d.Terms)
	}
	// ceil(1000 * 150000 / 1e6) + ceil(1024 * 600000 / 1e6) = 150 + 615, in integer micro-units.
	if d.Cost.EstimateUpperMicroUnits == nil || *d.Cost.EstimateUpperMicroUnits != 765 || d.Cost.Currency != "USD" || d.Cost.BudgetState != providers.BudgetStatusOk {
		t.Errorf("cost = %+v", d.Cost)
	}
	if d.OnUnavailable.Behavior != providers.EgressDisclosureOnUnavailableBehaviorFail || d.OnUnavailable.FallbackProfileID != nil {
		t.Errorf("on_unavailable = %+v", d.OnUnavailable)
	}

	// Metadata-only sources disclose metadata and no excerpts; a local_only source adds nothing.
	d, _ = f.svc.Disclose(testCtx, f.scope, f.prof, DisclosureInput{Role: providers.RoleConversationalController, Sources: []providers.SourceID{metadata, localOnly}})
	if d.RAGExcerptsIncluded || hasCategory(d.DataCategories, providers.SendCategoryRAGExcerpt) || !hasCategory(d.DataCategories, providers.SendCategoryRAGMetadata) {
		t.Errorf("metadata only: %+v", d.DataCategories)
	}
	d, _ = f.svc.Disclose(testCtx, f.scope, f.prof, DisclosureInput{Role: providers.RoleConversationalController, Sources: []providers.SourceID{localOnly}})
	if d.RAGExcerptsIncluded || hasCategory(d.DataCategories, providers.SendCategoryRAGExcerpt) || hasCategory(d.DataCategories, providers.SendCategoryRAGMetadata) {
		t.Errorf("a local_only source contributes nothing: %+v", d.DataCategories)
	}
	assertValid(t, urnDisclosure, d)

	// A named fallback and the budget state are shown; no price means no estimate.
	p := cloneProfile(t, f.prof)
	fb := providers.ProfileID("pprf_01J0000000000000000000000Q")
	p.Fallback = providers.ProviderProfileFallback{ProfileID: &fb, On: []providers.ProviderProfileFallbackOnItem{providers.ProviderProfileFallbackOnItemUnavailable}}
	p.Pricing.Prices.ToksOut = nil
	d, err = f.svc.Disclose(testCtx, f.scope, p, DisclosureInput{Role: providers.RoleConversationalController, EstimatedToksIn: 10, Budget: providers.BudgetStatusWarning})
	if err != nil {
		t.Fatal(err)
	}
	if d.OnUnavailable.Behavior != providers.EgressDisclosureOnUnavailableBehaviorFallback || d.OnUnavailable.FallbackProfileID == nil || *d.OnUnavailable.FallbackProfileID != fb {
		t.Errorf("on_unavailable = %+v", d.OnUnavailable)
	}
	if d.Cost.EstimateUpperMicroUnits != nil || d.Cost.BudgetState != providers.BudgetStatusWarning {
		t.Errorf("cost = %+v", d.Cost)
	}
	assertValid(t, urnDisclosure, d)
	if _, err := f.svc.Disclose(testCtx, f.scope, f.prof, DisclosureInput{Role: providers.RoleConversationalController, Budget: "free"}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("an unknown budget state: %v", err)
	}

	// The disclosure never promises what the guard refuses: it follows the same checks.
	f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) { in.AllowedClasses = nil })
	d, _ = f.svc.Disclose(testCtx, f.scope, f.prof, DisclosureInput{Role: providers.RoleConversationalController})
	if d.ExternalEnabled || len(d.DataClasses) != 0 || len(d.DataCategories) != 0 {
		t.Errorf("no class may leave, so nothing is listed: %+v", d)
	}
	f.put(providers.EgressLevelWorkspace, nil)
	f.put(providers.EgressLevelTenant, func(in *PolicyInput) { in.AllowedProviders = nil })
	d, _ = f.svc.Disclose(testCtx, f.scope, f.prof, DisclosureInput{Role: providers.RoleConversationalController})
	if d.ExternalEnabled || len(d.DataCategories) != 0 {
		t.Errorf("a provider not allowed: %+v", d)
	}
	// A candidate profile is disclosed as local too.
	f.put(providers.EgressLevelTenant, nil)
	q := cloneProfile(t, f.prof)
	q.Lifecycle = providers.ProfileLifecycleCandidate
	if d, _ = f.svc.Disclose(testCtx, f.scope, q, DisclosureInput{Role: providers.RoleConversationalController}); d.ExternalEnabled {
		t.Errorf("a candidate profile: %+v", d)
	}

	// What Evaluate returns discloses what it sent.
	res := f.mustAllow(f.candidate(excerptPart(excerpt, "body", "T")))
	if !res.Disclosure.ExternalEnabled || res.Disclosure.Provider != res.Decision.Provider || !res.Disclosure.RAGExcerptsIncluded ||
		strings.Join(catStrings(res.Disclosure.DataCategories), ",") != strings.Join(catStrings(res.Decision.Categories.Sent), ",") {
		t.Errorf("evaluate's disclosure = %+v", res.Disclosure)
	}
	if res.Disclosure.Cost.EstimateUpperMicroUnits == nil {
		t.Error("an estimate is given for a text request")
	}
	// A denied request is disclosed as staying local.
	f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) { in.Enabled = false })
	den, _ := f.eval(f.candidate())
	if den.Disclosure.ExternalEnabled || len(den.Disclosure.DataCategories) != 0 {
		t.Errorf("a denied request leaves nothing: %+v", den.Disclosure)
	}
	assertValid(t, urnDisclosure, den.Disclosure)
}

func catStrings(c []providers.SendCategory) []string {
	out := make([]string, len(c))
	for i, x := range c {
		out[i] = string(x)
	}
	return out
}
