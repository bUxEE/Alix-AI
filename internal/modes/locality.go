package modes

import (
	"context"
	"slices"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Locality says where a capability runs (additive plan section 12).
type Locality string

// The labels. A deployment can never look local when it is not: a capability that runs on a provider
// is never labelled local, and one nothing can serve right now is labelled unavailable.
const (
	// LocalityLocal runs on this deployment and works offline.
	LocalityLocal Locality = "local"
	// LocalityRemote runs on an external provider when served.
	LocalityRemote Locality = "remote"
	// LocalityOfflineUnavailable needs the network and is unavailable while offline (OAuth consent,
	// terms revalidation, invoice reconciliation).
	LocalityOfflineUnavailable Locality = "offline_unavailable"
	// LocalityProviderDependent depends on the provider: which provider serves it, or whether one is
	// ready at all, decides whether and where it runs.
	LocalityProviderDependent Locality = "provider_dependent"
	// LocalityUnavailable cannot be served by anything right now.
	LocalityUnavailable Locality = "unavailable"
)

// Offline is what happens to a capability when the provider or the network is gone.
type Offline string

// The offline effects.
const (
	OfflineWorks         Offline = "works"
	OfflineLocalFallback Offline = "local_fallback"
	OfflineUnavailable   Offline = "unavailable"
)

// Entry is one capability of the locality report.
type Entry struct {
	// Feature is a stable id (the role for model features).
	Feature  string
	Role     Role // empty for a control-plane or network feature
	Locality Locality
	// Available is false when nothing can serve the capability now.
	Available bool
	// Reason is a fixed code that explains the label.
	Reason  string
	Offline Offline
	// LocalUsable reports whether the local provider of the role could serve on its own.
	LocalUsable bool
	// Providers are the external profiles the capability depends on, in policy order.
	Providers []pv.ProfileID
}

// Report is the feature-locality report of a scope.
type Report struct {
	Mode       Mode
	DecidedBy  string
	KillSwitch bool
	// RequiredLocalReady is true when every required role (the controller) is usable locally. When it
	// is false and the mode is not Local, external mode is covering a local gap: the deployment does not
	// pass as a local deployment and must not be presented as one.
	RequiredLocalReady bool
	CoveringLocalGap   bool
	Entries            []Entry
}

// Entry returns the entry of a feature.
func (r Report) Entry(feature string) (Entry, bool) {
	for _, e := range r.Entries {
		if e.Feature == feature {
			return e, true
		}
	}
	return Entry{}, false
}

// Control-plane features of additive plan section 12: they always run on this deployment.
var controlFeatures = []string{
	"application_adapters", "authorization_and_scopes", "session_state", "rag_authorization_and_indexes",
	"file_classification_and_redaction", "audit_and_action_receipts", "action_prepare_and_commit",
}

// Features that need the network and exist only when external processing is configured.
var networkFeatures = []string{"provider_oauth_consent", "provider_terms_revalidation", "usage_reconciliation"}

// LocalityReport states, for every capability, whether it runs local, remote, is unavailable offline
// or depends on a provider, under the scope's effective mode.
func (r *Router) LocalityReport(ctx context.Context, scope contracts.Scope) (Report, error) {
	res, err := r.resolver.Resolve(ctx, scope)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Mode: res.Mode, DecidedBy: res.DecidedBy, KillSwitch: res.KillSwitch, RequiredLocalReady: true}
	// The controller is the only required role; realisation shares its local text generator.
	if rr, ok := RoleController.runtimeRole(); ok && rr.Required() && !r.local.Usable(RoleController) {
		rep.RequiredLocalReady = false
	}
	rep.CoveringLocalGap = !rep.RequiredLocalReady && res.Mode != ModeLocal

	for _, f := range controlFeatures {
		rep.Entries = append(rep.Entries, Entry{Feature: f, Locality: LocalityLocal, Available: true, Reason: "control_plane_local", Offline: OfflineWorks})
	}
	for _, role := range AllRoles() {
		rep.Entries = append(rep.Entries, r.modelEntry(ctx, scope, res, role))
	}
	if res.Mode != ModeLocal {
		for _, f := range networkFeatures {
			rep.Entries = append(rep.Entries, Entry{Feature: f, Locality: LocalityOfflineUnavailable, Available: true, Reason: "needs_network", Offline: OfflineUnavailable})
		}
	}
	return rep, nil
}

func (r *Router) modelEntry(ctx context.Context, scope contracts.Scope, res Resolved, role Role) Entry {
	localOK := r.local.Usable(role)
	e := Entry{Feature: string(role), Role: role, LocalUsable: localOK, Offline: OfflineUnavailable}
	localOnly := func(reason string) Entry {
		e.Reason = reason
		if localOK {
			e.Locality, e.Available, e.Offline = LocalityLocal, true, OfflineWorks
		} else {
			e.Locality, e.Reason = LocalityUnavailable, "no_local_provider"
		}
		return e
	}
	ext, extErr := role.External()
	if res.Mode == ModeLocal {
		if res.KillSwitch {
			return localOnly("kill_switch")
		}
		return localOnly("mode_local")
	}
	rp, err := r.policy.RolePolicy(ctx, scope, role)
	if extErr != nil || err != nil || len(rp.Profiles) == 0 {
		return localOnly("no_external_route")
	}
	e.Providers = slices.Clone(rp.Profiles)
	ready, reason := r.providerReady(ctx, scope, ext, rp)
	named := len(rp.LocalFallback) > 0
	switch {
	case !ready:
		e.Locality, e.Reason = LocalityProviderDependent, reason
		if localOK && named {
			e.Offline = OfflineLocalFallback
		}
	case res.Mode == ModeHybrid && localOK:
		e.Locality, e.Available, e.Reason, e.Offline = LocalityProviderDependent, true, "hybrid_split_by_data_class", OfflineLocalFallback
	default:
		e.Locality, e.Available, e.Reason = LocalityRemote, true, "external_route"
		if localOK && named {
			e.Offline = OfflineLocalFallback
		}
	}
	return e
}

// providerReady reports whether at least one candidate is qualified for the role and has a budget on
// the interactive channel. It reads; it quarantines nothing.
func (r *Router) providerReady(ctx context.Context, scope contracts.Scope, ext pv.Role, rp RolePolicy) (bool, string) {
	reason := "provider_not_qualified"
	for _, id := range rp.Profiles {
		p, err := r.profiles.Get(ctx, scope, id)
		if err != nil || p.Lifecycle != pv.ProfileLifecycleQualified || !slices.Contains(p.Roles, ext) {
			continue
		}
		states, err := r.budgets.States(ctx, scope, p.ProviderID, pv.ChannelInteractive)
		if err != nil || len(states) == 0 {
			reason = "no_budget"
			continue
		}
		return true, ""
	}
	return false, reason
}
