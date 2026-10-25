//go:build !noexternal

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// extEgressProfile is the qualified example profile, permitted to the test application, serving the multimodal
// roles and allowed every egress-eligible class: what may leave is then up to the policy alone.
func extEgressProfile(t *testing.T) pv.ProviderProfile {
	t.Helper()
	raw, err := os.ReadFile("../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json")
	if err != nil {
		t.Fatal(err)
	}
	var prof pv.ProviderProfile
	if err := json.Unmarshal(raw, &prof); err != nil {
		t.Fatal(err)
	}
	prof.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: common.Identifier("app_demo")}}
	prof.Roles = append(prof.Roles, pv.RoleVision, pv.RoleTTS, pv.RoleSTT)
	prof.AllowedDataClasses = []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal, pv.EgressEligibleClassConfidential}
	return prof
}

// extEgressPolicies writes, through the operator API, a policy at the four request levels that lets public and
// internal data go to the profile's provider and model, with the files setting given (nil: none).
func extEgressPolicies(t *testing.T, a *externalAdmin, prof pv.ProviderProfile, edit func(*gateway.ProvidersEgressPolicyInput)) {
	t.Helper()
	ctx := context.Background()
	ops := externalTestScope("ten_acme", "usr_ops")
	ops.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		c, _ := dataegress.WriteCapability(lv)
		ops.CapabilityScopes = append(ops.CapabilityScopes, c)
	}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		cur, found, err := a.EgressPolicy(ctx, ops, lv, "")
		if err != nil {
			t.Fatal(err)
		}
		in := gateway.ProvidersEgressPolicyInput{
			Level: lv, Enabled: true, SourceMode: pv.EgressModeLocalOnly,
			AllowedProviders: []pv.ProviderID{prof.ProviderID}, AllowedModels: []pv.ModelRef{{ProviderID: prof.ProviderID, ModelID: prof.ModelID}},
			AllowedClasses: []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal}, AllowedRegions: []pv.RegionCode{"us", "eu"},
		}
		if found {
			in.ExpectedRevision = cur.Revision
		}
		if edit != nil {
			edit(&in)
		}
		if _, err := a.PutEgressPolicy(ctx, ops, in); err != nil {
			t.Fatalf("PutEgressPolicy(%s): %v", lv, err)
		}
	}
}

func extEgressFiles(max pv.EgressEligibleClass) func(*gateway.ProvidersEgressPolicyInput) {
	return func(in *gateway.ProvidersEgressPolicyInput) {
		in.Files = &pv.EgressPolicyContentEgress{Enabled: true, MaxClass: max}
	}
}

// A file excerpt (file_content at class confidential) leaves only under the files setting the operator API wrote:
// a policy that lets the user's words leave keeps it home, a setting that stops below confidential keeps it home,
// and one that reaches confidential lets it go with an egress decision. An excerpt the guard would have to change
// (a credential in it) never leaves.
func TestExternalFileExcerptEgressFollowsTheFilesSetting(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	ctx := context.Background()
	scope := externalTestScope("ten_acme", "usr_member")
	prof := extEgressProfile(t)
	target := extmodal.Target{Profile: prof}
	gate := extWireTextEgress{guard: a.egress}
	const excerpt = "Clause 4: the tenant pays the deposit by 1 March."

	extEgressPolicies(t, a, prof, nil)
	if _, err := gate.AllowText(ctx, scope, target, extmodal.PurposeVisionAsk, "what does clause 4 say"); err != nil {
		t.Fatalf("the user's words: %v", err)
	}
	_, err := gate.AllowText(ctx, scope, target, extmodal.PurposeExcerpt, excerpt)
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied || strings.Contains(err.Error(), "deposit") {
		t.Fatalf("an excerpt with no files setting: %v", err)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Details["check"] != string(pv.EgressDecisionCheckNameFiles) {
		t.Fatalf("the refusal does not name the files check: %v", err)
	}

	extEgressPolicies(t, a, prof, extEgressFiles(pv.EgressEligibleClassInternal))
	if _, err := gate.AllowText(ctx, scope, target, extmodal.PurposeExcerpt, excerpt); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("an excerpt above max_class: %v", err)
	}

	extEgressPolicies(t, a, prof, extEgressFiles(pv.EgressEligibleClassConfidential))
	d, err := gate.AllowText(ctx, scope, target, extmodal.PurposeExcerpt, excerpt)
	if err != nil || d.DecisionID == "" || d.EnvelopeID == "" {
		t.Fatalf("an excerpt under the files setting: %+v, %v", d, err)
	}
	if _, err := gate.AllowText(ctx, scope, target, extmodal.PurposeExcerpt, "the key is sk-"+"TESTSENTINEL0123456789abcdef"); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("an excerpt with a credential in it: %v", err)
	}
}
