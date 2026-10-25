package adaptersuite

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

const (
	metaApp   = "app_demo"
	metaLimit = 64 << 10
)

// reference builds the reference subject: the fake adapter inside adapters.Wrap.
func reference(t *testing.T, appOpts ...fake.AppOption) (Subject, *fake.App) {
	t.Helper()
	app := fake.NewApp(append([]fake.AppOption{fake.WithOversizeBytes(4 * metaLimit)}, appOpts...)...)
	guarded, err := adapters.Wrap(fake.NewAdapter(app, fake.WithMaxResponseBytes(metaLimit)), adapters.Options{ApplicationID: metaApp})
	if err != nil {
		t.Fatal(err)
	}
	return Subject{Adapter: guarded, Fixture: app, ApplicationID: metaApp, MaxResponseBytes: metaLimit}, app
}

// failures runs every check against a subject and returns the names of the
// checks that reported a violation.
func failures(t *testing.T, shortGrace bool, mk func(t *testing.T) Subject) map[string]string {
	t.Helper()
	if shortGrace {
		old := grace
		grace = 300 * time.Millisecond
		defer func() { grace = old }()
	}
	out := map[string]string{}
	for _, c := range Checks() {
		s := mk(t)
		if err := s.validate(); err != nil {
			t.Fatalf("subject: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := c.Run(ctx, s); err != nil {
			out[c.Name] = err.Error()
		}
		cancel()
	}
	return out
}

func names(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSuiteAcceptsReferenceAdapter: the reference adapter passes every check,
// so a failure in the tests below is caused by the defect and nothing else.
func TestSuiteAcceptsReferenceAdapter(t *testing.T) {
	got := failures(t, false, func(t *testing.T) Subject { s, _ := reference(t); return s })
	if len(got) != 0 {
		t.Fatalf("the reference adapter fails %v:\n%v", names(got), got)
	}
}

// tamper wraps an adapter and lets a test change what the six result
// operations do.
type tamper struct {
	adapters.AppAdapter
	result   func(op adapters.Operation, ctx context.Context, sc contracts.Scope, next func(context.Context, contracts.Scope) (actions.DeterministicResult, error)) (actions.DeterministicResult, error)
	describe func(ctx context.Context, next func(context.Context) (adapters.Description, error)) (adapters.Description, error)
	health   func(ctx context.Context, next func(context.Context) (adapters.HealthReport, error)) (adapters.HealthReport, error)
}

func (t tamper) dispatch(op adapters.Operation, ctx context.Context, sc contracts.Scope, next func(context.Context, contracts.Scope) (actions.DeterministicResult, error)) (actions.DeterministicResult, error) {
	if t.result == nil {
		return next(ctx, sc)
	}
	return t.result(op, ctx, sc, next)
}

func (t tamper) Read(ctx context.Context, sc contracts.Scope, r adapters.ReadRequest) (actions.DeterministicResult, error) {
	return t.dispatch(adapters.OpRead, ctx, sc, func(c context.Context, s contracts.Scope) (actions.DeterministicResult, error) {
		return t.AppAdapter.Read(c, s, r)
	})
}

func (t tamper) Validate(ctx context.Context, sc contracts.Scope, r adapters.ValidateRequest) (actions.DeterministicResult, error) {
	return t.dispatch(adapters.OpValidate, ctx, sc, func(c context.Context, s contracts.Scope) (actions.DeterministicResult, error) {
		return t.AppAdapter.Validate(c, s, r)
	})
}

func (t tamper) Prepare(ctx context.Context, sc contracts.Scope, r adapters.PrepareRequest) (actions.DeterministicResult, error) {
	return t.dispatch(adapters.OpPrepare, ctx, sc, func(c context.Context, s contracts.Scope) (actions.DeterministicResult, error) {
		return t.AppAdapter.Prepare(c, s, r)
	})
}

func (t tamper) Commit(ctx context.Context, sc contracts.Scope, r adapters.CommitRequest) (actions.DeterministicResult, error) {
	return t.dispatch(adapters.OpCommit, ctx, sc, func(c context.Context, s contracts.Scope) (actions.DeterministicResult, error) {
		return t.AppAdapter.Commit(c, s, r)
	})
}

func (t tamper) Status(ctx context.Context, sc contracts.Scope, r adapters.StatusRequest) (actions.DeterministicResult, error) {
	return t.dispatch(adapters.OpStatus, ctx, sc, func(c context.Context, s contracts.Scope) (actions.DeterministicResult, error) {
		return t.AppAdapter.Status(c, s, r)
	})
}

func (t tamper) Cancel(ctx context.Context, sc contracts.Scope, r adapters.CancelRequest) (actions.DeterministicResult, error) {
	return t.dispatch(adapters.OpCancel, ctx, sc, func(c context.Context, s contracts.Scope) (actions.DeterministicResult, error) {
		return t.AppAdapter.Cancel(c, s, r)
	})
}

func (t tamper) Describe(ctx context.Context) (adapters.Description, error) {
	if t.describe == nil {
		return t.AppAdapter.Describe(ctx)
	}
	return t.describe(ctx, t.AppAdapter.Describe)
}

func (t tamper) Health(ctx context.Context) (adapters.HealthReport, error) {
	if t.health == nil {
		return t.AppAdapter.Health(ctx)
	}
	return t.health(ctx, t.AppAdapter.Health)
}

// brokenOutside builds a subject whose adapter is the reference adapter with
// tamper applied on the outside.
func brokenOutside(t *testing.T, mutate func(t *testing.T, tp *tamper)) Subject {
	t.Helper()
	s, _ := reference(t)
	tp := tamper{AppAdapter: s.Adapter}
	mutate(t, &tp)
	s.Adapter = tp
	return s
}

// claimedOK is a result an adapter fabricates when it hides a failure.
func claimedOK(op adapters.Operation) actions.DeterministicResult {
	res, _ := op.Result()
	return actions.DeterministicResult{
		Operation:    res,
		CapabilityID: fake.ActionCapability,
		Status:       actions.DeterministicResultStatusOk,
		Freshness:    &actions.Freshness{AsOf: "2026-10-09T08:00:00Z", Version: "v1"},
	}
}

// TestSuiteRejectsBrokenAdapters: each defect below violates part of the
// contract, and the named checks must notice. A suite that passed any of these
// would let a transport with the same defect through.
func TestSuiteRejectsBrokenAdapters(t *testing.T) {
	type broken struct {
		name  string
		build func(t *testing.T) Subject
		fails []string
	}
	cases := []broken{
		{
			name: "a host whose commit is not idempotent",
			build: func(t *testing.T) Subject {
				s, _ := reference(t, fake.WithNonIdempotentCommit())
				return s
			},
			fails: []string{"idempotent_commit", "status_after_uncertain_commit"},
		},
		{
			name: "an adapter that retries every failed call",
			build: func(t *testing.T) Subject {
				return brokenOutside(t, func(t *testing.T, tp *tamper) {
					tp.result = func(op adapters.Operation, ctx context.Context, sc contracts.Scope, next func(context.Context, contracts.Scope) (actions.DeterministicResult, error)) (actions.DeterministicResult, error) {
						res, err := next(ctx, sc)
						if err != nil {
							return next(ctx, sc)
						}
						return res, nil
					}
				})
			},
			fails: []string{"malformed_and_partial_responses", "status_after_uncertain_commit"},
		},
		{
			name: "an adapter that keeps using the first identity it saw",
			build: func(t *testing.T) Subject {
				return brokenOutside(t, func(t *testing.T, tp *tamper) {
					var first *contracts.Scope
					tp.result = func(op adapters.Operation, ctx context.Context, sc contracts.Scope, next func(context.Context, contracts.Scope) (actions.DeterministicResult, error)) (actions.DeterministicResult, error) {
						if first == nil {
							c := sc.Clone()
							first = &c
						}
						return next(ctx, *first)
					}
				})
			},
			fails: []string{"scope_propagation", "tenant_isolation"},
		},
		{
			name: "an adapter that ignores its context",
			build: func(t *testing.T) Subject {
				return brokenOutside(t, func(t *testing.T, tp *tamper) {
					stop, cancel := context.WithCancel(context.Background())
					t.Cleanup(cancel)
					tp.result = func(op adapters.Operation, ctx context.Context, sc contracts.Scope, next func(context.Context, contracts.Scope) (actions.DeterministicResult, error)) (actions.DeterministicResult, error) {
						return next(stop, sc)
					}
				})
			},
			fails: []string{"timeouts_and_cancellation"},
		},
		{
			name: "an adapter that reports success after a failure",
			build: func(t *testing.T) Subject {
				return brokenOutside(t, func(t *testing.T, tp *tamper) {
					tp.result = func(op adapters.Operation, ctx context.Context, sc contracts.Scope, next func(context.Context, contracts.Scope) (actions.DeterministicResult, error)) (actions.DeterministicResult, error) {
						res, err := next(ctx, sc)
						if err != nil {
							return claimedOK(op), nil
						}
						return res, nil
					}
					tp.health = func(ctx context.Context, next func(context.Context) (adapters.HealthReport, error)) (adapters.HealthReport, error) {
						h, err := next(ctx)
						if err != nil || h.Status != adapters.HealthOK {
							return adapters.HealthReport{Status: adapters.HealthOK}, nil
						}
						return h, nil
					}
				})
			},
			fails: []string{"malformed_and_partial_responses", "timeouts_and_cancellation", "scope_fail_closed", "health_and_recovery", "status_after_uncertain_commit"},
		},
		{
			name: "an adapter that reports an uncertain commit as a plain failure",
			build: func(t *testing.T) Subject {
				return brokenOutside(t, func(t *testing.T, tp *tamper) {
					tp.result = func(op adapters.Operation, ctx context.Context, sc contracts.Scope, next func(context.Context, contracts.Scope) (actions.DeterministicResult, error)) (actions.DeterministicResult, error) {
						res, err := next(ctx, sc)
						if op == adapters.OpCommit && contracts.CodeOf(err) == contracts.ErrUncertainOutcome {
							return actions.DeterministicResult{}, contracts.New(contracts.ErrUnavailable, "the application is unavailable")
						}
						return res, err
					}
				})
			},
			fails: []string{"status_after_uncertain_commit", "malformed_and_partial_responses", "timeouts_and_cancellation", "resource_bounds"},
		},
		{
			name: "a transport that is not wrapped",
			build: func(t *testing.T) Subject {
				app := fake.NewApp(fake.WithOversizeBytes(4 * metaLimit))
				return Subject{Adapter: fake.NewAdapter(app, fake.WithMaxResponseBytes(metaLimit)), Fixture: app, ApplicationID: metaApp, MaxResponseBytes: metaLimit}
			},
			fails: []string{"scope_fail_closed", "timeouts_and_cancellation", "malformed_and_partial_responses", "resource_bounds"},
		},
		{
			name: "an adapter that reads an unbounded reply",
			build: func(t *testing.T) Subject {
				app := fake.NewApp(fake.WithOversizeBytes(4 * metaLimit))
				guarded, err := adapters.Wrap(fake.NewAdapter(app, fake.WithMaxResponseBytes(1<<30)), adapters.Options{ApplicationID: metaApp})
				if err != nil {
					t.Fatal(err)
				}
				return Subject{Adapter: guarded, Fixture: app, ApplicationID: metaApp, MaxResponseBytes: metaLimit}
			},
			fails: []string{"resource_bounds"},
		},
		{
			name: "an adapter that always claims the host speaks version 1",
			build: func(t *testing.T) Subject {
				return brokenOutside(t, func(t *testing.T, tp *tamper) {
					tp.describe = func(ctx context.Context, next func(context.Context) (adapters.Description, error)) (adapters.Description, error) {
						d, err := next(ctx)
						d.ContractVersions = []int{adapters.ContractVersion}
						return d, err
					}
				})
			},
			fails: []string{"version_negotiation"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := failures(t, true, tc.build)
			for _, want := range tc.fails {
				if _, ok := got[want]; !ok {
					t.Errorf("check %s passed although the adapter is broken; failing checks: %v", want, names(got))
				}
			}
			if len(got) == 0 {
				t.Fatal("no check noticed the defect")
			}
		})
	}
}

// TestSubjectIsValidated: a factory that forgets what the suite needs is told so.
func TestSubjectIsValidated(t *testing.T) {
	good, app := reference(t)
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Subject{
		"no adapter":     {Fixture: app, ApplicationID: metaApp, MaxResponseBytes: metaLimit},
		"no fixture":     {Adapter: good.Adapter, ApplicationID: metaApp, MaxResponseBytes: metaLimit},
		"no application": {Adapter: good.Adapter, Fixture: app, MaxResponseBytes: metaLimit},
		"no limit":       {Adapter: good.Adapter, Fixture: app, ApplicationID: metaApp},
		"limit above the fixture's oversize reply": {Adapter: good.Adapter, Fixture: app, ApplicationID: metaApp, MaxResponseBytes: int64(app.OversizeBytes())},
	} {
		if err := s.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
