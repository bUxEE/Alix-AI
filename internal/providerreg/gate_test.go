package providerreg

import (
	"errors"
	"slices"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestImpossibleRequestRejectedBeforeSend(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*providers.ProviderProfile)
		req    func(*Requirement)
		want   Reason
	}{
		{"typed action proposals need tool calls", func(p *providers.ProviderProfile) { p.ToolCalling = providers.ProviderProfileToolCalling{} }, nil, ReasonToolCallingUnsupported},
		{"tool calls need strict arguments", func(p *providers.ProviderProfile) { p.ToolCalling.StrictArguments = false }, nil, ReasonToolArgumentsNotStrict},
		{"more tools than the provider accepts", nil, func(r *Requirement) { r.ToolCount = 17 }, ReasonTooManyTools},
		{"weak structured output", func(p *providers.ProviderProfile) {
			p.StructuredOutput = providers.ProviderProfileStructuredOutputJSONMode
		}, nil, ReasonStructuredOutputWeak},
		{"no streaming", func(p *providers.ProviderProfile) { p.Streaming = false }, nil, ReasonStreamingUnsupported},
		{"image input on a text-only model", nil, func(r *Requirement) { r.Input = append(r.Input, providers.ModalityImage) }, ReasonInputModalityMissing},
		{"audio output on a text-only model", nil, func(r *Requirement) { r.Output = append(r.Output, providers.ModalityAudio) }, ReasonOutputModalityMissing},
		{"image generation", nil, func(r *Requirement) { r.ImageGeneration = true }, ReasonImageGenerationMissing},
		{"speech to text", nil, func(r *Requirement) { r.SpeechToText = true }, ReasonSpeechToTextMissing},
		{"text to speech", nil, func(r *Requirement) { r.TextToSpeech = true }, ReasonTextToSpeechMissing},
		{"realtime voice", nil, func(r *Requirement) { r.RealtimeVoice = true }, ReasonRealtimeVoiceMissing},
		{"role the profile lacks", nil, func(r *Requirement) { r.Role = providers.RoleEmbedding }, ReasonRoleUnsupported},
		{"context over the limit", nil, func(r *Requirement) { r.ContextToks = 128001 }, ReasonContextTooLarge},
		{"output over the limit", nil, func(r *Requirement) { r.OutputToks = 8193 }, ReasonOutputTooLarge},
		{"body over the limit", nil, func(r *Requirement) { r.RequestBytes = 1<<20 + 1 }, ReasonRequestTooLarge},
		{"negative size", nil, func(r *Requirement) { r.ContextToks = -1 }, ReasonRequirementInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			var muts []func(*providers.ProviderProfile)
			if tc.mutate != nil {
				muts = append(muts, tc.mutate)
			}
			p := g.qualified(credRef(1), muts...)
			r := req()
			if tc.req != nil {
				tc.req(&r)
			}
			before := g.creds.count()
			calls, err := g.route(g.scope, p.ProfileID, r)
			wantReason(t, err, contracts.ErrValidationFailed, ReasonCapability)
			var ce *contracts.Error
			if !errors.As(err, &ce) {
				t.Fatalf("not a typed error: %v", err)
			}
			if reasons, _ := ce.Details["reasons"].([]string); !slices.Contains(reasons, string(tc.want)) {
				t.Fatalf("reasons = %v, want to include %s", ce.Details["reasons"], tc.want)
			}
			if calls != 0 {
				t.Fatalf("send ran %d times for an impossible request", calls)
			}
			if got := g.creds.count(); got != before {
				t.Fatalf("the credential service was consulted (%d calls) for a request that cannot be served", got-before)
			}
		})
	}

	t.Run("a possible request is admitted exactly once", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1))
		calls, err := g.route(g.scope, p.ProfileID, req())
		if err != nil || calls != 1 {
			t.Fatalf("calls = %d, err = %v; want one send and no error", calls, err)
		}
		adm, err := g.reg.Check(bg, g.scope, p.ProfileID, req())
		if err != nil || !adm.Valid() || adm.Profile.ProfileID != p.ProfileID {
			t.Fatalf("Check = %+v, %v", adm, err)
		}
		if (Admission{}).Valid() {
			t.Fatal("the zero Admission must not be valid")
		}
	})

	t.Run("restricted, empty and unknown data classes never leave", func(t *testing.T) {
		g := newRig(t)
		p := g.qualified(credRef(1), func(d *providers.ProviderProfile) {
			d.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential}
		})
		for _, c := range []providers.DataClassification{providers.DataClassificationRestricted, "", "secret-ish"} {
			r := req()
			r.DataClass = c
			calls, err := g.route(g.scope, p.ProfileID, r)
			wantReason(t, err, contracts.ErrPermissionDenied, ReasonDataClassDenied)
			if calls != 0 {
				t.Fatalf("class %q: send ran", c)
			}
		}
		// A class the profile does not list is denied even when it is a valid class.
		q := g.qualified(credRef(2))
		r := req()
		r.DataClass = providers.DataClassificationConfidential
		if _, err := g.route(g.scope, q.ProfileID, r); ReasonOf(err) != ReasonDataClassDenied {
			t.Fatalf("confidential to a public+internal profile: %v", err)
		}
	})
}

func TestPermittedApplicationTenantWorkspaceOnly(t *testing.T) {
	g := newRig(t)
	ten, ten2 := common.Identifier(tTenant), common.Identifier(tTenant2)
	wsp := common.Identifier(tWsp)
	grant := func(app string, tenant, workspace *common.Identifier) providers.ProviderProfileScopeGrant {
		return providers.ProviderProfileScopeGrant{ApplicationID: common.Identifier(app), TenantID: tenant, WorkspaceID: workspace}
	}

	cases := []struct {
		name   string
		grants []providers.ProviderProfileScopeGrant
		allow  map[[3]string]bool // scope -> allowed; unlisted scopes are denied
	}{
		{"nothing permits nobody", nil, map[[3]string]bool{}},
		{"exact workspace", []providers.ProviderProfileScopeGrant{grant(tApp, &ten, &wsp)},
			map[[3]string]bool{{tApp, tTenant, tWsp}: true}},
		{"a tenant covers its workspaces only", []providers.ProviderProfileScopeGrant{grant(tApp, &ten, nil)},
			map[[3]string]bool{{tApp, tTenant, tWsp}: true, {tApp, tTenant, tWsp2}: true}},
		{"an application covers its tenants", []providers.ProviderProfileScopeGrant{grant(tApp, nil, nil)},
			map[[3]string]bool{{tApp, tTenant, tWsp}: true, {tApp, tTenant, tWsp2}: true, {tApp, tTenant2, tWsp}: true, {tApp, tTenant2, tWsp2}: true}},
		{"two grants", []providers.ProviderProfileScopeGrant{grant(tApp, &ten, &wsp), grant(tApp, &ten2, nil)},
			map[[3]string]bool{{tApp, tTenant, tWsp}: true, {tApp, tTenant2, tWsp}: true, {tApp, tTenant2, tWsp2}: true}},
	}
	every := [][3]string{
		{tApp, tTenant, tWsp}, {tApp, tTenant, tWsp2}, {tApp, tTenant2, tWsp}, {tApp, tTenant2, tWsp2},
		{tOther, tTenant, tWsp}, {tOther, tTenant2, tWsp2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := g.qualified(credRef(3), func(d *providers.ProviderProfile) { d.PermittedScopes = tc.grants })
			for _, s := range every {
				scope := g.scopeOf(s[0], s[1], s[2])
				_, err := g.reg.Check(bg, scope, p.ProfileID, req())
				switch {
				case s[0] != tApp:
					// Another application cannot even see the profile.
					wantReason(t, err, contracts.ErrValidationFailed, ReasonProfileNotFound)
				case tc.allow[s]:
					if err != nil {
						t.Errorf("%v should be permitted: %v", s, err)
					}
				default:
					wantReason(t, err, contracts.ErrPermissionDenied, ReasonScopeNotPermitted)
				}
			}
			g.reg.Retire(bg, g.scope, p.ProfileID)
		})
	}

	t.Run("grants are validated against the owning application", func(t *testing.T) {
		wspOnly := providers.ProviderProfileScopeGrant{ApplicationID: tApp, WorkspaceID: &wsp}
		wildcard := common.Identifier("*")
		for name, grants := range map[string][]providers.ProviderProfileScopeGrant{
			"another application":          {grant(tOther, nil, nil)},
			"a workspace without a tenant": {wspOnly},
			"a wildcard tenant":            {grant(tApp, &wildcard, nil)},
			"a wildcard application":       {grant("*", nil, nil)},
		} {
			d := g.draft()
			d.PermittedScopes = grants
			if _, err := g.reg.Register(bg, g.scope, d); err == nil {
				t.Errorf("%s: Register accepted the grant", name)
			}
			p := g.register()
			if _, err := g.reg.SetPermittedScopes(bg, g.scope, p.ProfileID, grants); err == nil {
				t.Errorf("%s: SetPermittedScopes accepted the grant", name)
			}
		}
	})

	t.Run("a malformed stored grant permits nothing", func(t *testing.T) {
		wspOnly := []providers.ProviderProfileScopeGrant{{ApplicationID: tApp, WorkspaceID: &wsp}}
		if permits(wspOnly, g.scopeOf(tApp, tTenant, tWsp)) {
			t.Fatal("a workspace grant without a tenant permitted a scope")
		}
	})
}
