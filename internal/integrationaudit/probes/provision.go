package probes

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Provisioner creates, seeds and removes the application's disposable tenants
// (master plan 4.5 check 10, docs/integration/application-package.md section 12,
// fixtures.provisioning). The package names the three operations (create_tenant,
// seed, cleanup) as adapter calls; AppAdapter has no way to call an arbitrary
// operation, and the adapter the audit is given is the adapters.Wrap wrapper,
// which exposes nothing but the contract, so a Provisioner comes from its own
// seam (SetProvisionerSource). A transport that can run the package's
// provisioning operations (an MCP server with test tools, an OpenAPI
// management endpoint, a native management method) supplies one and registers
// it from an init function of its own file, as an adapter registers itself.
//
// Provisioning is deliberately narrow. It only ever acts on tenants whose
// identifiers the audit generated for this run, and an application must refuse
// to touch any tenant it did not create as disposable. Every method must be
// safe to call again: Cleanup of a tenant that does not exist reports
// Existed false and nothing remaining.
type Provisioner interface {
	// CreateTenant creates a disposable tenant with its workspaces and
	// principals. The application fails the call if the tenant exists.
	CreateTenant(ctx context.Context, t Tenant) error
	// Seed loads the synthetic records of a tenant and returns the identifier
	// the application gave each, by the record's logical reference.
	Seed(ctx context.Context, s Seed) (SeedResult, error)
	// Cleanup removes the tenant and everything created for it and reports what
	// is left.
	Cleanup(ctx context.Context, tenantID string) (CleanupReport, error)
}

// Tenant is one disposable tenant to create. Every identifier was generated for
// this run; the package's logical references never reach the application.
type Tenant struct {
	ID         string
	Workspaces []string
	Principals []Principal
}

// Principal is a synthetic principal of a disposable tenant.
type Principal struct {
	// Ref is the principal's logical reference in the suite (fixtures.principals[].ref),
	// the name the seed data file gives a record's principal. A Provisioner that has
	// to find the principal a reference names matches it here and never derives it
	// from ID: how the audit builds an identifier is its own business and changes
	// without notice. A reference is unique within the tenant.
	Ref         string
	ID          string
	WorkspaceID string
	Roles       []string
	// Service is true for a service principal, false for a human one.
	Service bool
}

// Seed asks for the synthetic records of one tenant.
type Seed struct {
	TenantID string
	Records  []Record
	// DataFile is the package-relative path (below tests/) of the synthetic data
	// file the suite names for the seed operation, or "". The audit does not read
	// it; a provisioner that runs the package's seed operation takes it from the
	// installed package.
	DataFile string
}

// Record is one synthetic record the seed operation creates.
type Record struct {
	// Ref is the record's logical reference in the suite.
	Ref        string
	EntityType string
	// WorkspaceID is the generated identifier of the workspace the record belongs to.
	WorkspaceID string
}

// SeedResult is what the application created.
type SeedResult struct {
	// Entities holds the application's identifier of each seeded record, by Ref.
	Entities map[string]string
}

// CleanupReport is the application's account of a cleanup.
type CleanupReport struct {
	// Existed is true when the tenant existed when the cleanup began.
	Existed bool
	// Remaining counts what is left of the tenant, by kind of record. A clean
	// removal leaves nothing: every count is zero.
	Remaining map[string]int
}

// Left returns how many records remain.
func (r CleanupReport) Left() int {
	n := 0
	for _, c := range r.Remaining {
		if c > 0 {
			n += c
		}
	}
	return n
}

// RecordChanger is optionally implemented by a Provisioner whose application can
// change one record of a disposable tenant on request (a test-support operation),
// so that the record's data version moves without an action being committed. The
// audit uses it to prove that Validate notices a change and that a preparation
// made before it is refused as stale. Without it those two probes are recorded
// as unverified.
type RecordChanger interface {
	ChangeRecord(ctx context.Context, tenantID, entityType, entityID string) error
}

// FaultKind is a failure the audit asks a cooperating application to produce.
type FaultKind string

const (
	// FaultDependencyDown makes the next request of the operation find a
	// dependency down: the application must answer with status unavailable and
	// apply nothing.
	FaultDependencyDown FaultKind = "dependency_down"
	// FaultReplyLost makes the next Commit apply its effect and then lose its
	// reply, so the caller sees an uncertain outcome.
	FaultReplyLost FaultKind = "reply_lost"
)

// Fault is one armed, one-shot failure.
type Fault struct {
	Operation    adapters.Operation
	CapabilityID string
	Kind         FaultKind
}

// FaultInjector is optionally implemented by a Provisioner whose application can
// produce failures on request (test-support fault injection). Without it the
// audit cannot make the application fail and records the typed handling of
// unavailable and lost-reply failures as unverified.
type FaultInjector interface {
	InjectFault(ctx context.Context, f Fault) error
}

// StepUp is one step-up the audit asks a cooperating application to complete.
type StepUp struct {
	// CapabilityID is the capability whose preparation is to be completed.
	CapabilityID string
	// Scope is the scope of the principal the action was prepared for: the principal
	// the application's own interface knows, and signs in, to complete it.
	Scope contracts.Scope
	// Prepared is the preparation the platform made of the action.
	Prepared contracts.PreparedAction
	// Result is the application's own Prepare reply. Where the application tells the
	// user how to finish (a reference to quote in its own interface) the fact is in
	// its values; the audit does not know what the application calls it.
	Result act.DeterministicResult
}

// StepUpCompleter is optionally implemented by a Provisioner whose application can
// complete, on request, the step-up of a preparation in a disposable tenant (a
// test-support operation that does what the supervisor does in the application's own
// authenticated interface; the audit holds no credential and never learns one). The
// audit uses it to prove that the effect of a level 3 action happens only after the
// application's own step-up. Without it that part is recorded as unverified; the refusal
// of every commit made without the step-up is still proved.
type StepUpCompleter interface {
	CompleteStepUp(ctx context.Context, s StepUp) error
}

// ProvisionerSource returns the Provisioner for the application an audit runs
// against, or nil when none is available (then no disposable tenant can be
// reached and the probes that need one are unverified). It is given the audit's
// whole input, so it can build a client from the installed package's endpoints.
type ProvisionerSource func(ctx context.Context, in *integrationaudit.Input) (Provisioner, error)

var (
	sourceMu       sync.RWMutex
	defaultSources []ProvisionerSource
)

// SetProvisionerSource adds a source to the ones the checks registered with the
// audit consult. The first source that returns a Provisioner wins. It is for the
// composition root and for transports; nothing in this package knows a
// transport. A nil source is ignored.
func SetProvisionerSource(s ProvisionerSource) {
	if s == nil {
		return
	}
	sourceMu.Lock()
	defer sourceMu.Unlock()
	defaultSources = append(defaultSources, s)
}

// defaultSource consults the registered sources, then the adapter itself: an
// adapter that also implements Provisioner (a test double, or a transport the
// composition root did not wrap) is used as it is.
func defaultSource(ctx context.Context, in *integrationaudit.Input) (Provisioner, error) {
	sourceMu.RLock()
	srcs := append([]ProvisionerSource(nil), defaultSources...)
	sourceMu.RUnlock()
	for _, s := range srcs {
		p, err := s(ctx, in)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	if p, ok := in.Adapter.(Provisioner); ok && in.Adapter != nil {
		return p, nil
	}
	return nil, nil
}

// sortedKeys returns the keys of a map in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// provisionTimeout bounds every call to a Provisioner. A provisioner that hangs must
// not hang the audit: the call fails, and what it was for is recorded as not probed.
// Cleanup is not cut short by the audit's own cancellation, but it is bounded the
// same way.
var provisionTimeout = 30 * time.Second

// timed gives every call to the provisioner its own deadline.
type timed struct {
	p Provisioner
	d time.Duration
}

func (t timed) with(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, t.d)
}

func (t timed) CreateTenant(ctx context.Context, x Tenant) error {
	ctx, cancel := t.with(ctx)
	defer cancel()
	return t.p.CreateTenant(ctx, x)
}

func (t timed) Seed(ctx context.Context, x Seed) (SeedResult, error) {
	ctx, cancel := t.with(ctx)
	defer cancel()
	return t.p.Seed(ctx, x)
}

func (t timed) Cleanup(ctx context.Context, id string) (CleanupReport, error) {
	ctx, cancel := t.with(ctx)
	defer cancel()
	return t.p.Cleanup(ctx, id)
}

// timedChanger and timedInjector bound the optional abilities the same way.
type timedChanger struct {
	c RecordChanger
	d time.Duration
}

func (t timedChanger) ChangeRecord(ctx context.Context, tenantID, entityType, entityID string) error {
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	return t.c.ChangeRecord(ctx, tenantID, entityType, entityID)
}

type timedInjector struct {
	f FaultInjector
	d time.Duration
}

func (t timedInjector) InjectFault(ctx context.Context, f Fault) error {
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	return t.f.InjectFault(ctx, f)
}

// timedCompleter bounds the step-up the same way.
type timedCompleter struct {
	c StepUpCompleter
	d time.Duration
}

func (t timedCompleter) CompleteStepUp(ctx context.Context, s StepUp) error {
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	return t.c.CompleteStepUp(ctx, s)
}
