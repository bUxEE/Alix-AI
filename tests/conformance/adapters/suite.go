// Package adaptersuite is the shared conformance suite for application adapters
// (master plan 22.2: "Every provider and application adapter passes a shared
// suite"). Every transport (MCP, OpenAPI, native) and the in-memory fake run the
// same checks through RunAdapterSuite, so the contract the rest of the core
// relies on is tested once and the same way for all of them. It is imported as
//
//	import adaptersuite "github.com/buxee/alix-ai/tests/conformance/adapters"
//
// # Running it
//
// A transport's test provides a Factory. For each check the suite calls the
// factory for a fresh Subject: the adapter under test, exactly as production
// wires it (so adapters.Wrap(transport, ...)), and a Fixture that controls the
// host application behind it.
//
//	func TestConformanceMCPAdapter(t *testing.T) {
//	    adaptersuite.RunAdapterSuite(t, func(t *testing.T) adaptersuite.Subject {
//	        app := fake.NewApp(fake.WithOversizeBytes(256 << 10))
//	        srv := startMCPServerOver(t, app)          // your wire; calls app.Handle
//	        a, _ := adapters.Wrap(mcp.New(srv.Config()), adapters.Options{ApplicationID: "app_demo"})
//	        return adaptersuite.Subject{Adapter: a, Fixture: app, ApplicationID: "app_demo", MaxResponseBytes: 64 << 10}
//	    })
//	}
//
// # The fixture
//
// The host application is internal/adapters/fake.App, a deterministic
// in-memory application that speaks raw JSON, so every transport puts it behind
// its own wire: decode the request, call App.Handle, send Reply.Body, and act on
// Reply.Action (Deliver, Hang, Drop, Refuse). It records the identity context of
// every request it receives, counts the effects it applies and misbehaves on
// command (Inject). Subject.Fixture is that App. The capabilities it serves are
// fake.ReadCapability ("items.get") and fake.ActionCapability ("items.create").
//
// # What is checked
//
// Each check is one behaviour the core depends on; the names are the subtests.
// Master plan 22.2 items are in brackets.
//
//	describe                           Describe returns a valid, stable description
//	version_negotiation                [version negotiation] highest shared version
//	                                   chosen; a host with only a higher major is refused
//	describe_backwards_compatibility   [backwards compatibility] the v1 members stay;
//	                                   a newer host's extra members are ignored
//	typed_results                      every operation answers with a DeterministicResult
//	                                   for the call made; host refusals are results
//	scope_propagation                  [authentication and scope propagation] the host
//	                                   receives exactly the identity of each call, one
//	                                   request per call, never a stale or shared one
//	scope_fail_closed                  an invalid, foreign or unpermitted scope sends nothing
//	tenant_isolation                   a record of one tenant or workspace is invisible to another
//	host_denial                        a host reauthorisation denial is a rejected result,
//	                                   is not retried and is not sticky; a denied Status is
//	                                   never mistaken for "the host has no record"
//	idempotent_commit                  [idempotency] the effect is applied once per key
//	status_after_uncertain_commit      a lost reply or request is uncertain_outcome, never
//	                                   retried by the adapter; Status tells what happened
//	                                   (the no-record marker proves nothing was applied)
//	timeouts_and_cancellation          [timeouts and cancellation] hung hosts are cut off,
//	                                   a commit that may have run is uncertain_outcome
//	malformed_and_partial_responses    [malformed and partial responses] prose, truncated,
//	                                   partial, mismatched replies are schema_invalid
//	resource_bounds                    [resource bounds] an oversize reply is refused at the limit
//	unavailable_dependency             [unavailable dependencies] a host reporting a down
//	                                   dependency is a typed result, never success
//	health_and_recovery                [health and recovery] a down host is reported down
//	                                   and calls fail unavailable; recovery is immediate
//	inspect                            unreported aspects are unsupported, never assumed
//
// Security backlog entries covered: SEC-041 (uncertain commit resolved by Status
// before any retry), SEC-044 (identity at every read, prepare and
// commit; a host denial is not worked around), SEC-050 (forged and malformed
// results), SEC-051 (hung, crashing and oversize hosts).
//
// # The suite tests itself
//
// suite_test.go runs every check against deliberately broken adapters (a host
// whose commit is not idempotent, an adapter that retries, ignores its context,
// drops the scope, reports success after a failure, reads an unbounded reply,
// is not wrapped) and requires the checks to fail, so the suite cannot pass an
// adapter that violates the contract.
package adaptersuite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
)

// Fixture controls the host application behind the adapter under test.
// *fake.App implements it.
type Fixture interface {
	// Calls returns every request the host received, oldest first, with the
	// identity context it saw. A request the host never received is absent.
	Calls() []fake.Call
	// Effects counts how many times the host applied its create effect.
	Effects() int
	// Inject arms a one-shot misbehaviour for the next request of an operation.
	Inject(op adapters.Operation, f fake.Fault)
	// ClearFaults drops injected faults that have not fired.
	ClearFaults()
	// SetAvailable takes the host down (requests are refused before the host
	// receives them) or brings it back.
	SetAvailable(up bool)
	// SetContractVersions sets the contract versions the host reports.
	SetContractVersions(v ...int)
	// OversizeBytes is the size of the reply FaultOversize produces. It must
	// exceed Subject.MaxResponseBytes.
	OversizeBytes() int
}

var _ Fixture = (*fake.App)(nil)

// Subject is one adapter under test and the fixture behind it.
type Subject struct {
	// Adapter is the adapter as production wires it: wrapped with adapters.Wrap
	// unless the constructor already does.
	Adapter adapters.AppAdapter
	// Fixture controls the host application behind Adapter.
	Fixture Fixture
	// ApplicationID is the application the adapter is bound to
	// (adapters.Options.ApplicationID); scopes in the suite carry it.
	ApplicationID string
	// MaxResponseBytes is the reply size limit the adapter enforces; the suite
	// requires it to be set and checks that a larger reply is refused.
	MaxResponseBytes int64
}

func (s Subject) validate() error {
	switch {
	case s.Adapter == nil:
		return fmt.Errorf("Subject.Adapter is nil")
	case s.Fixture == nil:
		return fmt.Errorf("Subject.Fixture is nil")
	case s.ApplicationID == "":
		return fmt.Errorf("Subject.ApplicationID is empty")
	case s.MaxResponseBytes <= 0:
		return fmt.Errorf("Subject.MaxResponseBytes must state the reply size limit the adapter enforces")
	case int64(s.Fixture.OversizeBytes()) <= s.MaxResponseBytes:
		return fmt.Errorf("the fixture's oversize reply (%d bytes) must exceed Subject.MaxResponseBytes (%d)", s.Fixture.OversizeBytes(), s.MaxResponseBytes)
	}
	return nil
}

// Factory builds a fresh Subject for one check. Register cleanup (closing
// servers, sockets, processes) with t.Cleanup.
type Factory func(t *testing.T) Subject

// Check is one behaviour of the contract.
type Check struct {
	// Name is the subtest name.
	Name string
	// Run returns nil when the subject conforms, otherwise an error that says
	// what it did wrong. It must return within its context's deadline plus a
	// fixed grace period even if the adapter hangs.
	Run func(ctx context.Context, s Subject) error
}

// suiteTimeout bounds one check.
const suiteTimeout = 90 * time.Second

// Checks returns every check in the order RunAdapterSuite runs them.
func Checks() []Check {
	return []Check{
		{"describe", checkDescribe},
		{"version_negotiation", checkVersionNegotiation},
		{"describe_backwards_compatibility", checkDescribeBackwardsCompatibility},
		{"typed_results", checkTypedResults},
		{"scope_propagation", checkScopePropagation},
		{"scope_fail_closed", checkScopeFailClosed},
		{"tenant_isolation", checkTenantIsolation},
		{"host_denial", checkHostDenial},
		{"idempotent_commit", checkIdempotentCommit},
		{"status_after_uncertain_commit", checkStatusAfterUncertainCommit},
		{"timeouts_and_cancellation", checkTimeoutsAndCancellation},
		{"malformed_and_partial_responses", checkMalformedAndPartialResponses},
		{"resource_bounds", checkResourceBounds},
		{"unavailable_dependency", checkUnavailableDependency},
		{"health_and_recovery", checkHealthAndRecovery},
		{"inspect", checkInspect},
	}
}

// RunAdapterSuite runs every check as a subtest, each on a fresh Subject from
// the factory.
//
// SEC-044, SEC-050, SEC-051
func RunAdapterSuite(t *testing.T, factory Factory) {
	t.Helper()
	for _, c := range Checks() {
		t.Run(c.Name, func(t *testing.T) {
			s := factory(t)
			if err := s.validate(); err != nil {
				t.Fatalf("factory: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), suiteTimeout)
			defer cancel()
			if err := c.Run(ctx, s); err != nil {
				t.Error(err)
			}
		})
	}
}
