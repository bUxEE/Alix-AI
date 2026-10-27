package extmode

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ExternalName is the Provider.Name of the external route the selector hands the engine. It names
// a route, not a profile: the profile is chosen by the registry, per request, inside the Guard.
const ExternalName = "external"

// Config wires a Runtime. Every collaborator is required: a runtime missing the egress guard, the
// profile registry, the budget or the sender could not apply the gates, so it is not built.
type Config struct {
	// Local is the selector of Local mode (dialogue.LocalSelector). It serves every request that is
	// not routed externally and every local fallback.
	Local dialogue.ProviderSelector
	// Modes, KillSwitch, Policy, LocalProviders, Profiles, Egress and Budgets are the collaborators
	// of modes.Router.
	Modes          modes.ModeSource
	KillSwitch     modes.KillSwitch
	Policy         modes.PolicySource
	LocalProviders modes.LocalProviders
	Profiles       modes.Profiles
	Egress         dataegress.Guard
	Budgets        modes.Budgets
	// Packages resolves the typed package a turn is served from; the tool schemas are built from it.
	Packages dialogue.PackageSource
	// Sender sends an admitted, filtered request to the provider. It has no adapter in reach.
	Sender Sender
	// Budget is the request budget used to price the worst case of an external call.
	Budget providers.Budget
	// Clock times staged requests. Nil means the wall clock.
	Clock clock.Clock

	// Features are the provider-native features the operator asked for. They are off for every
	// consequential call whatever is asked (see Features).
	Features Features
	// Convenience lists the native features the operator classified as non-authoritative
	// conveniences. A feature not listed is never enabled.
	Convenience []Feature
	// DirectiveClass is the data class the directive of a response prompt leaves at. Empty (the
	// default) keeps the directive local: an external provider then never realises a response and
	// the deterministic template is used.
	DirectiveClass string
	// OnOutcome, when set, receives the routed outcome of every external-route call: the fallback hops
	// live on it and only the caller audits them. It holds no content.
	OnOutcome func(scope contracts.Scope, o modes.Outcome)
}

// Runtime is the dialogue PromptGuard and ProviderSelector of External Provider Mode. It is safe for
// concurrent use.
type Runtime struct {
	cfg    Config
	router *modes.Router
	stage  *stage
}

var (
	_ dialogue.ProviderSelector = (*Runtime)(nil)
	_ dialogue.PromptGuard      = (*Runtime)(nil)
)

// New returns a Runtime.
func New(cfg Config) (*Runtime, error) {
	switch {
	case cfg.Local == nil, cfg.Packages == nil, cfg.Sender == nil:
		return nil, contracts.New(contracts.ErrInternal, "extmode: a local selector, a package source and a sender are required")
	case cfg.Budget.Validate() != nil:
		return nil, contracts.New(contracts.ErrInternal, "extmode: a valid request budget is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	rt, err := modes.New(modes.Config{
		Modes: cfg.Modes, KillSwitch: cfg.KillSwitch, Policy: cfg.Policy, Local: cfg.LocalProviders,
		Profiles: cfg.Profiles, Guard: cfg.Egress, Budgets: cfg.Budgets, Clock: cfg.Clock,
	})
	if err != nil {
		return nil, err
	}
	return &Runtime{cfg: cfg, router: rt, stage: newStage(cfg.Clock)}, nil
}

// Router returns the provider router the runtime sends through.
func (r *Runtime) Router() *modes.Router { return r.router }

// routedRole maps a runtime role to the role the router sees, for the roles that may leave.
func routedRole(role providers.Role) (modes.Role, bool) {
	if role == providers.RoleController {
		return modes.RoleController, true
	}
	return "", false
}

// Select implements dialogue.ProviderSelector. Local mode, the kill switch, a role that never leaves and
// a role with no external route all return exactly what the local selector returns.
func (r *Runtime) Select(ctx context.Context, scope contracts.Scope, role providers.Role) (dialogue.Provider, error) {
	mr, ok := routedRole(role)
	if !ok {
		return r.cfg.Local.Select(ctx, scope, role)
	}
	res, err := r.router.Resolver().Resolve(ctx, scope)
	if err != nil {
		return dialogue.Provider{}, err
	}
	if res.Mode == modes.ModeLocal {
		return r.cfg.Local.Select(ctx, scope, role)
	}
	rp, err := r.cfg.Policy.RolePolicy(ctx, scope, mr)
	if err != nil || len(rp.Profiles) == 0 {
		return r.cfg.Local.Select(ctx, scope, role)
	}
	return dialogue.Provider{Name: ExternalName, Local: false, Generator: routed{r}}, nil
}

// Guard implements dialogue.PromptGuard. A local provider's prompt is returned as it is. For the
// external route the prompt is evaluated by the egress guard against the registry profile and the
// returned request holds only what may be sent.
func (r *Runtime) Guard(ctx context.Context, in dialogue.GuardInput) (dialogue.PromptRequest, error) {
	if in.Provider.Local {
		return in.Request, nil
	}
	if in.Provider.Name != ExternalName {
		return dialogue.PromptRequest{}, deny("provider_not_routed")
	}
	mr, ok := routedRole(in.Request.Role)
	if !ok {
		return dialogue.PromptRequest{}, deny("role_local_only")
	}
	if err := in.Scope.Validate(); err != nil {
		return dialogue.PromptRequest{}, err
	}
	original := in.Request.ChatRequest()
	st := &staged{original: original, role: mr, commit: in.Request.Output.Kind() == providers.ConstraintJSONSchema, schemaOut: in.Request.Output.Kind() == providers.ConstraintJSONSchema}

	// The mode is read again: a switch to Local, or the kill switch, since Select sends the prompt
	// to the local runtime, which may see everything.
	res, err := r.router.Resolver().Resolve(ctx, in.Scope)
	if err != nil {
		return dialogue.PromptRequest{}, err
	}
	cand, m, err := r.candidate(ctx, in, st)
	if err != nil {
		return dialogue.PromptRequest{}, err
	}
	st.req = providerreg.Requirement{
		Role: pv.RoleConversationalController, DataClass: m.maxClass, TypedActionProposals: st.commit && len(cand.Tools) > 0,
		ToolCount: int64(len(cand.Tools)), StructuredOutput: structuredNeed(st, cand),
		ContextToks: int64(r.cfg.Budget.EstimateChat(original)), OutputToks: int64(max(original.MaxTokens, 1)),
	}
	if res.Mode == modes.ModeLocal || (res.Mode == modes.ModeHybrid && sensitive(m.maxClass)) {
		st.localOnly = true
		r.stage.put(in.Scope, original, st)
		return in.Request, nil
	}

	prof, err := r.profileFor(ctx, in.Scope, mr, st.req)
	if err != nil {
		return dialogue.PromptRequest{}, err
	}
	cand.Profile, cand.Role = prof, pv.RoleConversationalController
	if st.commit && len(cand.Tools) > 0 && !prof.ToolCalling.Supported {
		cand.Tools = nil // the provider cannot take tools: it gets the schema alone
	}
	cand.ResponseFormat = responseFormat(st, cand, in.Request.Output)
	cand.MaxOutputToks, cand.TimeoutMS = int64(max(original.MaxTokens, 1)), int64(r.cfg.Budget.Timeout.Milliseconds())
	if cand.TimeoutMS < 1 {
		cand.TimeoutMS = 30000
	}
	cand.TraceID = "trc_extmode"
	eg, err := r.cfg.Egress.Evaluate(ctx, cand)
	if err != nil {
		return dialogue.PromptRequest{}, asDenied(err)
	}
	if eg.Envelope == nil {
		return dialogue.PromptRequest{}, deny("guard_failure")
	}
	out, filtered, err := m.apply(in.Request, cand, eg)
	if err != nil {
		return dialogue.PromptRequest{}, err
	}
	st.cand = filtered
	r.stage.put(in.Scope, out.ChatRequest(), st)
	return out, nil
}

// ControllerProfile returns the registry profile the controller route would use for this scope
// (principal bindings / private pins / deployment default), with the principal credential overlay
// when the binding applies. Same selection path as a normal controller Guard.
func (r *Runtime) ControllerProfile(ctx context.Context, scope contracts.Scope) (pv.ProviderProfile, error) {
	if err := scope.Validate(); err != nil {
		return pv.ProviderProfile{}, err
	}
	return r.profileFor(ctx, scope, modes.RoleController, providerreg.Requirement{
		Role: pv.RoleConversationalController, DataClass: pv.DataClassificationInternal,
		ContextToks: 256, OutputToks: 512,
	})
}

// profileFor picks the registry profile the request is evaluated against: the first of the role's
// policy profiles the registry admits. A profile that cannot serve the request is skipped, any
// other refusal ends it. The profile always comes from the registry, never from the request.
func (r *Runtime) profileFor(ctx context.Context, scope contracts.Scope, role modes.Role, req providerreg.Requirement) (pv.ProviderProfile, error) {
	rp, err := r.cfg.Policy.RolePolicy(ctx, scope, role)
	if err != nil {
		return pv.ProviderProfile{}, deny("policy_unreadable")
	}
	for _, id := range rp.Profiles {
		a, err := r.cfg.Profiles.Check(ctx, scope, id, req)
		if err == nil {
			if a.Profile.ProfileID != id {
				return pv.ProviderProfile{}, deny("profile_mismatch")
			}
			return a.Profile, nil
		}
		if providerreg.ReasonOf(err) == providerreg.ReasonCapability {
			continue
		}
		return pv.ProviderProfile{}, asDenied(err)
	}
	return pv.ProviderProfile{}, deny("no_candidate_supports_request")
}

func sensitive(c pv.DataClassification) bool {
	return c != pv.DataClassificationPublic && c != pv.DataClassificationInternal
}

// deny is the typed refusal of this package: permission_denied with a fixed rule code.
func deny(rule string) error {
	return contracts.New(contracts.ErrPermissionDenied, "extmode: the request was refused: "+rule).WithDetails("rule", rule)
}

// asDenied keeps a guard or registry refusal typed. A failure that is not already a denial is
// still a denial, never unavailable, so a caller cannot read it as the trigger of a named fallback.
func asDenied(err error) error {
	if contracts.CodeOf(err) == contracts.ErrPermissionDenied {
		return err
	}
	return contracts.Wrap(contracts.ErrPermissionDenied, err, "extmode: the request was refused").WithDetails("rule", "guard_failure")
}

// structuredNeed is the structured-output guarantee the request needs of a provider that is asked
// for a schema (a provider that proposes through tools needs none).
func structuredNeed(st *staged, c dataegress.Candidate) pv.ProviderProfileStructuredOutput {
	if st.schemaOut && len(c.Tools) == 0 {
		return pv.ProviderProfileStructuredOutputSchemaBestEffort
	}
	return ""
}

// responseFormat is the format asked of the provider: text with tools (the proposal is a tool call),
// the schema when there are no tools, text for a response.
func responseFormat(st *staged, c dataegress.Candidate, out providers.OutputConstraint) pv.RequestEnvelopeResponseFormat {
	if st.schemaOut && len(c.Tools) == 0 && out.Kind() == providers.ConstraintJSONSchema {
		var schema map[string]any
		if json.Unmarshal(out.JSONSchema, &schema) == nil && len(schema) > 0 {
			return pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindJSONSchema, Schema: schema, Strict: true}
		}
	}
	return pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText}
}

func contains[T comparable](s []T, v T) bool { return slices.Contains(s, v) }
