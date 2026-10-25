package actions

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// invalidationConditions are the events that void a preparation before it
// expires. The coordinator honours all of them: data_version_changed,
// permission_changed, principal_changed and application_quarantined are checked
// at commit; session_ended, goal_changed, field_corrected and application_event
// are raised by the session and event layers through Invalidate.
func invalidationConditions() []act.PreparedActionInvalidationConditionsItem {
	return []act.PreparedActionInvalidationConditionsItem{
		act.PreparedActionInvalidationConditionsItemDataVersionChanged,
		act.PreparedActionInvalidationConditionsItemPermissionChanged,
		act.PreparedActionInvalidationConditionsItemPrincipalChanged,
		act.PreparedActionInvalidationConditionsItemSessionEnded,
		act.PreparedActionInvalidationConditionsItemGoalChanged,
		act.PreparedActionInvalidationConditionsItemFieldCorrected,
		act.PreparedActionInvalidationConditionsItemApplicationEvent,
		act.PreparedActionInvalidationConditionsItemApplicationQuarantined,
	}
}

// checkRequest is the first check of every request-scoped call: a live context
// and a valid scope. There is no default scope.
func (c *Coordinator) checkRequest(ctx context.Context, scope contracts.Scope) error {
	if ctx == nil {
		return contracts.New(contracts.ErrInternal, "actions: no context")
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return contracts.Wrap(contracts.ErrTimeout, err, "actions: the request's deadline passed before the work started")
		}
		return contracts.Wrap(contracts.ErrUnavailable, err, "actions: the request was canceled before the work started")
	}
	return scope.Validate()
}

// authorized is a proposal that policy lets through.
type authorized struct {
	pkg      policy.ActivePackage
	decision policy.Decision
	decl     policy.CapabilityDecl
	fields   map[string]any
	dropped  []string
}

// authorize is the part of Read and Prepare that comes before the host is
// called: the session is the same authenticated context, the policy gateway
// allows the capability (bound to the session's stored application, tenant and
// workspace), and only the fields the model may propose are kept.
func (c *Coordinator) authorize(ctx context.Context, scope, session contracts.Scope, capabilityID string, fields map[string]any, taskFields []string) (authorized, error) {
	var zero authorized
	if err := c.checkRequest(ctx, scope); err != nil {
		return zero, err
	}
	if err := session.Validate(); err != nil {
		return zero, err
	}
	if err := identity.CheckContinuity(session, scope); err != nil {
		return zero, err
	}
	pkg, err := c.packages.ActivePackage(ctx, scope.ApplicationID)
	if err != nil {
		return zero, err
	}
	binding := policy.BindingOf(session)

	// A null is "no value", not a value.
	cur := make(map[string]any, len(fields))
	for k, v := range fields {
		if v != nil {
			cur[k] = v
		}
	}
	var dropped []string
	// What the host sees must be what the policy decided on: dropping a field
	// can change which host rules apply, so decide again over what is left until
	// nothing more is dropped.
	for range 4 {
		d := c.policy.Decide(ctx, policy.Request{
			Scope: scope, Package: pkg, CapabilityID: capabilityID, Binding: binding,
			Phase: policy.PhasePrepare, Fields: cur, TaskFields: taskFields,
		})
		if !d.Allowed {
			return zero, d.Err()
		}
		kept, drop := d.Project(cur)
		dropped = append(dropped, drop...)
		if len(drop) == 0 {
			decl, ok := pkg.Capability(capabilityID)
			if !ok {
				return zero, contracts.New(contracts.ErrUnknownCapability, "actions: the package does not declare the capability")
			}
			sort.Strings(dropped)
			return authorized{pkg: pkg, decision: d, decl: decl, fields: kept, dropped: dedupe(dropped)}, nil
		}
		cur = kept
	}
	return zero, contracts.New(contracts.ErrPermissionDenied, "actions: which fields the model may propose did not settle")
}

func dedupe(in []string) []string {
	out := in[:0:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// missingRequired lists the required fields of the capability that are absent.
func missingRequired(decl policy.CapabilityDecl, fields map[string]any) []string {
	var missing []string
	for _, f := range decl.Fields {
		if f.Required {
			if _, ok := fields[f.Name]; !ok {
				missing = append(missing, f.Name)
			}
		}
	}
	return missing
}

// checkResult re-validates a host result against the call it answers, even
// though the adapter wrapper already did: the coordinator never decides on a
// result it has not seen validated for this operation, capability and key.
func checkResult(res contracts.DeterministicResult, op adapters.Operation, capabilityID, key string) (contracts.DeterministicResult, error) {
	return adapters.CheckResult(res, adapters.Expect{Operation: op, CapabilityID: capabilityID, IdempotencyKey: key})
}

// Read is the level 0 path (master plan 11.4: read-only, executes after
// authorisation). There is no preparation and no confirmation: the policy
// gateway must allow the capability at risk level 0 with nothing required of
// the user, and then the host reads. A capability that needs a confirmation or
// is above level 0 is refused here and goes through Prepare.
func (c *Coordinator) Read(ctx context.Context, scope contracts.Scope, in ReadInput) (ReadOutcome, error) {
	var none ReadOutcome
	a, err := c.authorize(ctx, scope, in.Session, in.CapabilityID, in.Fields, in.TaskFields)
	if err != nil {
		return none, err
	}
	d := a.decision
	switch {
	case d.Requires.StepUp:
		return none, contracts.New(contracts.ErrStepUpRequired, "this capability cannot be completed through a read")
	case d.Requires.Confirmation:
		return none, contracts.New(contracts.ErrConfirmationRequired, "this capability needs a confirmation, which a read cannot obtain")
	case d.EffectiveRisk != contracts.RiskRead:
		return none, contracts.New(contracts.ErrValidationFailed, "the capability is not read-only; it is prepared, confirmed and committed")
	}
	if missing := missingRequired(a.decl, a.fields); len(missing) > 0 {
		return none, contracts.New(contracts.ErrMissingFields, "required fields are missing").WithDetails("fields", missing)
	}
	adapter, err := c.adapters.Adapter(ctx, scope.ApplicationID)
	if err != nil {
		return none, err
	}
	res, err := adapter.Read(ctx, scope, adapters.ReadRequest{CapabilityID: in.CapabilityID, Fields: a.fields})
	if err != nil {
		return none, err
	}
	if res, err = checkResult(res, adapters.OpRead, in.CapabilityID, ""); err != nil {
		return none, err
	}
	out := ReadOutcome{Result: res, Risk: d.EffectiveRisk, VisibleFields: d.VisibleFields, DroppedFields: a.dropped}
	if res.Status != act.DeterministicResultStatusOk {
		return out, hostError(res, "read")
	}
	return out, nil
}

// Prepare validates a proposal with the policy gateway and the host and
// stores a PreparedAction (master plan 3 rule 8: consequential actions are
// validated and prepared before confirmation and execution).
//
// Every authoritative member of the result is decided here and not by the
// model: the preparation ID, idempotency key, state, expiry and confirmation
// requirement are minted by the core; the summary, effects, data versions and
// the host's declared risk level are copied from the host's Prepare result; the
// effective risk level is the highest of the policy's, the host's and the
// platform's minimum for an external effect. Proposed fields the model may not
// see are dropped.
//
// When the host refuses, Prepared.Result holds its typed issues and the error
// carries the first issue's code; nothing is stored.
func (c *Coordinator) Prepare(ctx context.Context, scope contracts.Scope, in PrepareInput) (Prepared, error) {
	var none Prepared
	a, err := c.authorize(ctx, scope, in.Session, in.CapabilityID, in.Fields, in.TaskFields)
	if err != nil {
		return none, err
	}
	if missing := missingRequired(a.decl, a.fields); len(missing) > 0 {
		return Prepared{DroppedFields: a.dropped}, contracts.New(contracts.ErrMissingFields, "required fields are missing").WithDetails("fields", missing)
	}
	adapter, err := c.adapters.Adapter(ctx, scope.ApplicationID)
	if err != nil {
		return none, err
	}
	res, err := adapter.Prepare(ctx, scope, adapters.PrepareRequest{CapabilityID: in.CapabilityID, Fields: a.fields})
	if err != nil {
		return none, err
	}
	if res, err = checkResult(res, adapters.OpPrepare, in.CapabilityID, ""); err != nil {
		return none, err
	}
	if res.Status != act.DeterministicResultStatusOk {
		return Prepared{Result: res, DroppedFields: a.dropped}, hostError(res, "preparation")
	}
	if err := bindPrepare(res); err != nil {
		return none, err
	}
	prep := res.Preparation

	// Risk: the policy's level already holds the platform floor of the effect
	// class, the declared level and every host override and rule. The host's
	// level for this particular preparation and an external effect can only
	// raise it further.
	risk := a.decision.EffectiveRisk
	hostRisk, err := contracts.ParseRiskLevel(int(prep.DeclaredRiskLevel))
	if err != nil {
		return none, mismatch("the declared risk level is out of range")
	}
	risk = risk.Escalate(hostRisk)
	if hasExternalEffect(prep.Effects) {
		risk = risk.Escalate(contracts.RiskConsequential)
	}
	if risk == contracts.RiskRead {
		return none, contracts.New(contracts.ErrValidationFailed, "a read-only capability is read, not prepared")
	}
	mode := act.ConfirmationRequirementModeNone
	switch {
	case risk.RequiresStepUp() || a.decision.Requires.StepUp:
		mode = act.ConfirmationRequirementModeStepUp
	case risk.RequiresConfirmation() || a.decision.Requires.Confirmation:
		mode = act.ConfirmationRequirementModeExplicit
	}

	now := trunc(c.clk.Now())
	ttl := c.cfg.MaxPreparationTTL
	if prep.ValidForSeconds != nil {
		// Compared in seconds: a huge promise must not overflow into the past.
		if secs := int64(*prep.ValidForSeconds); secs > 0 && secs < int64(ttl/time.Second) {
			ttl = time.Duration(secs) * time.Second
		}
	}
	id := c.ids.New(preparationPrefix)
	key := c.ids.New(idempotencyPrefix)
	bindings := []act.ConfirmationRequirementBindingsItem{}
	if mode != act.ConfirmationRequirementModeNone {
		bindings = []act.ConfirmationRequirementBindingsItem{
			act.ConfirmationRequirementBindingsItemPreparationID,
			act.ConfirmationRequirementBindingsItemPrincipal,
			act.ConfirmationRequirementBindingsItemSession,
			act.ConfirmationRequirementBindingsItemDataVersions,
		}
	}
	pa := contracts.PreparedAction{
		PreparationID:          act.PreparationID(id),
		CapabilityID:           common.CapabilityID(in.CapabilityID),
		State:                  act.PreparedActionStatePrepared,
		Summary:                prep.Summary,
		Effects:                cloneSlice(prep.Effects),
		RiskLevel:              act.RiskLevel(risk),
		DataVersions:           cloneSlice(prep.DataVersions),
		PreparedAt:             stamp(now),
		ExpiresAt:              stamp(now.Add(ttl)),
		InvalidationConditions: invalidationConditions(),
		Confirmation:           act.ConfirmationRequirement{Mode: mode, Bindings: bindings},
		IdempotencyKey:         act.IdempotencyKey(key),
	}
	digest := dataDigest(pa.DataVersions)

	var token ConfirmationToken
	var mac string
	if mode == act.ConfirmationRequirementModeExplicit {
		if token, err = newConfirmationToken(); err != nil {
			return none, err
		}
		mac = confirmMAC(token, id, scope.PrincipalID, scope.SessionID, digest)
	}

	r := row{
		id: id, scope: scope, binding: policy.BindingOf(in.Session), capabilityID: in.CapabilityID,
		state: act.PreparedActionStatePrepared, risk: risk, mode: mode, mac: mac, key: key,
		snapshot: string(res.Freshness.Version), digest: digest,
		preparedAt: now, expiresAt: now.Add(ttl),
	}
	fieldsJSON, err := json.Marshal(a.fields)
	if err != nil {
		return none, contracts.Wrap(contracts.ErrInternal, err, "actions: the fields cannot be encoded")
	}
	preparedJSON, err := json.Marshal(pa)
	if err != nil {
		return none, contracts.Wrap(contracts.ErrInternal, err, "actions: the prepared action cannot be encoded")
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return none, contracts.Wrap(contracts.ErrInternal, err, "actions: the scope cannot be encoded")
	}
	err = c.db.Tx(ctx, func(tx *store.Tx) error {
		loc, err := audit.ForScope(scope)
		if err != nil {
			return err
		}
		if err := c.insert(ctx, tx, scope, r, string(fieldsJSON), string(preparedJSON), string(scopeJSON)); err != nil {
			return err
		}
		_, err = c.audit.AppendTx(ctx, tx, loc, c.event(audit.EventActionPrepared, r, map[string]any{
			"to": string(act.PreparedActionStatePrepared), "expires_at": string(pa.ExpiresAt),
		}))
		return err
	})
	if err != nil {
		// Nothing was stored: a preparation that cannot be audited does not exist.
		return none, contracts.Wrap(contracts.ErrUnavailable, err, "actions: the preparation could not be recorded")
	}
	c.log.Info("action prepared", "preparation_id", id, "capability_id", in.CapabilityID, "risk", int(risk), "mode", string(mode))
	return Prepared{Action: pa, Confirmation: token, Result: res, DroppedFields: a.dropped}, nil
}
