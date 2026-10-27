package settings

import (
	"time"

	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// AbilityName is one analyst ability key.
type AbilityName string

const (
	AbilityLoop         AbilityName = "loop"
	AbilityCompute      AbilityName = "compute"
	AbilityForecasts    AbilityName = "forecasts"
	AbilityPresentation AbilityName = "presentation"
	AbilityDrafts       AbilityName = "drafts"
	AbilityKnowledge    AbilityName = "knowledge"
	AbilityWebSearch    AbilityName = "web_search"
	AbilityWebFetch     AbilityName = "web_fetch"
)

var allAbilities = []AbilityName{
	AbilityLoop, AbilityCompute, AbilityForecasts, AbilityPresentation,
	AbilityDrafts, AbilityKnowledge, AbilityWebSearch, AbilityWebFetch,
}

// ResolveInput is everything AbilityResolver needs (no I/O).
type ResolveInput struct {
	Deployment     an.Config
	Application    *applicationpackage.ManifestAssistant
	Tenant         an.TenantSettings
	AnalystCapable bool
	WebConfigured  bool
	DomainDeclared bool
	Now            time.Time
}

// AbilityResolver intersects levels and applies analyst_capable / domain / provider gates.
type AbilityResolver struct{}

// Resolve returns the effective ability set and typed disabled_reasons.
func (AbilityResolver) Resolve(in ResolveInput) an.Abilities {
	depMaster := boolOr(in.Deployment.Enabled, false)
	tenMaster := true
	if in.Tenant.Enabled != nil {
		tenMaster = *in.Tenant.Enabled
	}
	master := depMaster && tenMaster

	depFlags := flagsFromConfig(in.Deployment.Abilities)
	appFlags := flagsFromApp(in.Application)
	tenFlags := flagsFromTenant(in.Tenant.Abilities)

	eff := an.AbilitiesAbilities{}
	reasons := an.AbilitiesDisabledReasons{}

	resolveOne := func(name AbilityName) (bool, an.AbilitiesReasonList) {
		var rs an.AbilitiesReasonList
		if !depMaster {
			rs = append(rs, an.AbilitiesReasonListItemMasterDisabled)
		} else if !depFlags[name] {
			rs = append(rs, an.AbilitiesReasonListItemDeploymentOff)
		}
		if !appFlags[name] {
			rs = append(rs, an.AbilitiesReasonListItemApplicationOff)
		}
		if !tenMaster {
			rs = append(rs, an.AbilitiesReasonListItemTenantOff)
		} else if in.Tenant.Abilities != nil && !tenFlags[name] {
			rs = append(rs, an.AbilitiesReasonListItemTenantOff)
		}
		if !in.AnalystCapable {
			rs = append(rs, an.AbilitiesReasonListItemModelNotAnalystCapable)
		}
		needsWeb := name == AbilityWebSearch || name == AbilityWebFetch
		if needsWeb && !in.WebConfigured {
			rs = append(rs, an.AbilitiesReasonListItemProviderUnconfigured)
		}
		needsDomain := name == AbilityKnowledge || name == AbilityWebSearch || name == AbilityWebFetch
		if needsDomain && !in.DomainDeclared {
			rs = append(rs, an.AbilitiesReasonListItemDomainUndeclared)
		}
		on := master && in.AnalystCapable && depFlags[name] && appFlags[name] && tenFlags[name]
		if needsWeb {
			on = on && in.WebConfigured
		}
		if needsDomain {
			on = on && in.DomainDeclared
		}
		if on {
			return true, nil
		}
		if len(rs) == 0 {
			rs = append(rs, an.AbilitiesReasonListItemDeploymentOff)
		}
		return false, uniqueReasons(rs)
	}

	eff.Loop, reasons.Loop = resolveOne(AbilityLoop)
	eff.Compute, reasons.Compute = resolveOne(AbilityCompute)
	eff.Forecasts, reasons.Forecasts = resolveOne(AbilityForecasts)
	eff.Presentation, reasons.Presentation = resolveOne(AbilityPresentation)
	eff.Drafts, reasons.Drafts = resolveOne(AbilityDrafts)
	eff.Knowledge, reasons.Knowledge = resolveOne(AbilityKnowledge)
	eff.WebSearch, reasons.WebSearch = resolveOne(AbilityWebSearch)
	eff.WebFetch, reasons.WebFetch = resolveOne(AbilityWebFetch)

	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return an.Abilities{
		Enabled:         master && in.AnalystCapable && eff.Loop,
		AnalystCapable:  in.AnalystCapable,
		Abilities:       eff,
		DisabledReasons: reasons,
		ResolvedAt:      common.Timestamp(now.Format(time.RFC3339Nano)),
	}
}

func uniqueReasons(in an.AbilitiesReasonList) an.AbilitiesReasonList {
	seen := map[an.AbilitiesReasonListItem]bool{}
	var out an.AbilitiesReasonList
	for _, r := range in {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
		if len(out) >= 8 {
			break
		}
	}
	return out
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func flagsFromConfig(f *an.ConfigAbilityFlags) map[AbilityName]bool {
	out := map[AbilityName]bool{}
	for _, a := range allAbilities {
		out[a] = false
	}
	if f == nil {
		return out
	}
	out[AbilityLoop] = boolOr(f.Loop, false)
	out[AbilityCompute] = boolOr(f.Compute, false)
	out[AbilityForecasts] = boolOr(f.Forecasts, false)
	out[AbilityPresentation] = boolOr(f.Presentation, false)
	out[AbilityDrafts] = boolOr(f.Drafts, false)
	out[AbilityKnowledge] = boolOr(f.Knowledge, false)
	out[AbilityWebSearch] = boolOr(f.WebSearch, false)
	out[AbilityWebFetch] = boolOr(f.WebFetch, false)
	return out
}

func flagsFromApp(a *applicationpackage.ManifestAssistant) map[AbilityName]bool {
	out := map[AbilityName]bool{}
	for _, n := range allAbilities {
		out[n] = false
	}
	if a == nil || a.Abilities == nil {
		return out
	}
	f := a.Abilities
	out[AbilityLoop] = boolOr(f.Loop, false)
	out[AbilityCompute] = boolOr(f.Compute, false)
	out[AbilityForecasts] = boolOr(f.Forecasts, false)
	out[AbilityPresentation] = boolOr(f.Presentation, false)
	out[AbilityDrafts] = boolOr(f.Drafts, false)
	out[AbilityKnowledge] = boolOr(f.Knowledge, false)
	out[AbilityWebSearch] = boolOr(f.WebSearch, false)
	out[AbilityWebFetch] = boolOr(f.WebFetch, false)
	return out
}

func flagsFromTenant(f *an.TenantSettingsAbilities) map[AbilityName]bool {
	out := map[AbilityName]bool{}
	for _, n := range allAbilities {
		out[n] = true
	}
	if f == nil {
		return out
	}
	out[AbilityLoop] = boolOr(f.Loop, false)
	out[AbilityCompute] = boolOr(f.Compute, false)
	out[AbilityForecasts] = boolOr(f.Forecasts, false)
	out[AbilityPresentation] = boolOr(f.Presentation, false)
	out[AbilityDrafts] = boolOr(f.Drafts, false)
	out[AbilityKnowledge] = boolOr(f.Knowledge, false)
	out[AbilityWebSearch] = boolOr(f.WebSearch, false)
	out[AbilityWebFetch] = boolOr(f.WebFetch, false)
	return out
}

// TenantWidens reports abilities the tenant tries to enable above ceilings.
func TenantWidens(tenant an.TenantSettings, deployment an.Config, app *applicationpackage.ManifestAssistant) []AbilityName {
	dep := flagsFromConfig(deployment.Abilities)
	ap := flagsFromApp(app)
	ten := flagsFromTenant(tenant.Abilities)
	depMaster := boolOr(deployment.Enabled, false)
	var bad []AbilityName
	if tenant.Enabled != nil && *tenant.Enabled && !depMaster {
		bad = append(bad, "enabled")
	}
	if tenant.Abilities == nil {
		return bad
	}
	for _, n := range allAbilities {
		if ten[n] && !(depMaster && dep[n] && ap[n]) {
			bad = append(bad, n)
		}
	}
	return bad
}
