// Package policyaudit joins the policy gateway to the audit log (master plan
// 4.1; SEC-087).
//
// internal/policy cannot import internal/audit: the audit package's own tests
// build policy records, and a package that imports a package whose tests import
// it fails to compile ("import cycle not allowed in test"). Everything that
// needs both therefore lives in a package of its own, as the composition root
// does for a hand-written sink (audit.RecordSink). This is that package: it
// registers the audit event type of the gateway's exposure record and provides
// the sink that stores the gateway's two kinds of record under their own event
// types.
//
// # Event types
//
//	policy.decision   a decision record (policy.RecordFormat): Decide at any
//	                  phase, and the package-level denial of Expose. Registered
//	                  by internal/audit as a core type.
//	policy.exposure   an exposure record (policy.ExposureFormat): the outcome of
//	                  every capability one Expose call considered, in one event.
//	                  Registered here, from init().
//
// # Wiring
//
// Gateway.Expose hands the sink a policy.Record whose Format is
// policy.ExposureFormat, through the same Emit(ctx, Record) every sink has. A
// sink that stores every record under one event type, such as the
//
//	audit.NewRecordSink[policy.Record](log, audit.EventPolicyDecision, locate)
//
// the composition roots write today, therefore keeps working: the exposure
// record is stored as a policy.decision event whose data says, by its format,
// what it is. NewSink is the same join with the two kinds under their own
// event types, which is what a reader of the log wants:
//
//	gw := policy.NewGateway(clk, policy.WithSink(policyaudit.NewSink(log, nil)))
//
// Both fail closed in the same way: Emit returns nil only after the event is
// committed, and the gateway turns any other result into a denial.
package policyaudit

import (
	"context"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// EventExposure is the registered audit event type of an exposure record, the
// typed handle of policy.Record data. It may be appended to a tenant chain and,
// as policy.decision may, to the platform chain (a record whose subject is
// incomplete is never dropped; see audit.RecordSink).
var EventExposure = audit.MustRegister[policy.Record](audit.TypeSpec{
	Name: "policy.exposure",
	Description: "the outcome of every capability one policy Expose call considered (allowed, or denied with its code and reason, and the effective risk level), " +
		"in one event instead of one policy.decision per capability",
	Levels:   audit.LevelTenant | audit.LevelApplication,
	Required: []string{"decision_id", "phase", "outcome", "considered", "part", "parts", "outcomes"},
	Validate: validateExposure,
})

// validateExposure is the audit log's own check of an exposure event, applied
// before the event is hashed: the log does not trust the writer to have used
// the gateway. It checks the shape that bounds the event (the format, the
// phase, the number of entries and that each entry names a capability and an
// outcome); the schema (schemas/policy/decision-record.schema.json) is the full
// contract.
func validateExposure(data map[string]any) error {
	bad := func(what string) error {
		return contracts.New(contracts.ErrSchemaInvalid, "policy.exposure: "+what)
	}
	if f, _ := data["format"].(string); f != policy.ExposureFormat {
		return bad("format is not " + policy.ExposureFormat)
	}
	if p, _ := data["phase"].(string); p != string(policy.PhaseExpose) {
		return bad("phase is not expose")
	}
	outcomes, ok := data["outcomes"].([]any)
	if !ok {
		return bad("outcomes is not a list")
	}
	if len(outcomes) > policy.MaxExposureOutcomes {
		return bad("too many outcomes")
	}
	for _, o := range outcomes {
		m, ok := o.(map[string]any)
		if !ok {
			return bad("an outcome is not an object")
		}
		if _, ok := m["capability_id"].(string); !ok {
			return bad("an outcome names no capability")
		}
		if v, _ := m["outcome"].(string); v != string(policy.OutcomeAllow) && v != string(policy.OutcomeDeny) {
			return bad("an outcome is neither allow nor deny")
		}
	}
	return nil
}

// SubjectLocator places a record in the tenant chain its own subject names. It
// fails, as audit.ForIDs does, when the subject is incomplete, and
// audit.RecordSink then writes the record to the platform chain instead of
// dropping it.
func SubjectLocator(r policy.Record) (audit.Locator, error) {
	s := r.Subject
	return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
}

// Sink is a policy.Sink that appends decision records as policy.decision events
// and exposure records as policy.exposure events.
type Sink struct {
	decisions *audit.RecordSink[policy.Record]
	exposures *audit.RecordSink[policy.Record]
}

var _ policy.Sink = (*Sink)(nil)

// NewSink returns a sink that appends to a. locate says where a record belongs;
// nil means SubjectLocator.
func NewSink(a audit.Appender, locate func(policy.Record) (audit.Locator, error)) *Sink {
	if locate == nil {
		locate = SubjectLocator
	}
	return &Sink{
		decisions: audit.NewRecordSink[policy.Record](a, audit.EventPolicyDecision, locate),
		exposures: audit.NewRecordSink[policy.Record](a, EventExposure.Name(), locate),
	}
}

// Emit appends r. It returns nil only after the event is committed.
func (s *Sink) Emit(ctx context.Context, r policy.Record) error {
	if s == nil || s.decisions == nil || s.exposures == nil {
		return contracts.New(contracts.ErrInternal, "policyaudit: the sink is not wired")
	}
	if r.IsExposure() {
		return s.exposures.Emit(ctx, r)
	}
	return s.decisions.Emit(ctx, r)
}
