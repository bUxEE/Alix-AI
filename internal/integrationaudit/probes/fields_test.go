package probes

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// perPrincipalHost is the notes application with the visibility some real applications have: a
// principal listed in only sees the one record it names (a technician sees the work orders
// assigned to them, not every order of the site). Every other principal sees what the workspace
// shows, as in the sim. A refused request is the sim's own scope_violation.
type perPrincipalHost struct {
	*sim
	only map[string]string // principal ref -> the one record ref it may see

	vmu   sync.Mutex
	refOf map[string]string // note id -> record ref, learned from what Seed returned
}

func newPerPrincipalHost(only map[string]string) *perPrincipalHost {
	return &perPrincipalHost{sim: newSim(), only: only, refOf: map[string]string{}}
}

func (h *perPrincipalHost) Seed(ctx context.Context, sd Seed) (SeedResult, error) {
	res, err := h.sim.Seed(ctx, sd)
	h.vmu.Lock()
	defer h.vmu.Unlock()
	for ref, id := range res.Entities {
		h.refOf[id] = ref
	}
	return res, err
}

// hidden reports whether the caller is restricted and the note the request names is not its own.
func (h *perPrincipalHost) hidden(sc contracts.Scope, fields map[string]any) bool {
	h.vmu.Lock()
	defer h.vmu.Unlock()
	for principal, record := range h.only {
		if !strings.HasPrefix(sc.PrincipalID, "usr_"+slug(principal)+"_") {
			continue
		}
		id, _ := stringField(fields, "note")
		return h.refOf[id] != record
	}
	return false
}

func (h *perPrincipalHost) refuse(op adapters.Operation, capID string) (act.DeterministicResult, error) {
	return h.sim.body(op, capID, "", rejected("scope_violation", "note", "the note is outside the caller's scope or does not exist"))
}

func (h *perPrincipalHost) Read(ctx context.Context, sc contracts.Scope, r adapters.ReadRequest) (act.DeterministicResult, error) {
	if h.hidden(sc, r.Fields) {
		return h.refuse(adapters.OpRead, r.CapabilityID)
	}
	return h.sim.Read(ctx, sc, r)
}

func (h *perPrincipalHost) Validate(ctx context.Context, sc contracts.Scope, r adapters.ValidateRequest) (act.DeterministicResult, error) {
	if h.hidden(sc, r.Fields) {
		return h.refuse(adapters.OpValidate, r.CapabilityID)
	}
	return h.sim.Validate(ctx, sc, r)
}

func (h *perPrincipalHost) Prepare(ctx context.Context, sc contracts.Scope, r adapters.PrepareRequest) (act.DeterministicResult, error) {
	if h.hidden(sc, r.Fields) {
		return h.refuse(adapters.OpPrepare, r.CapabilityID)
	}
	return h.sim.Prepare(ctx, sc, r)
}

// perPrincipalRig is the notes package with a second editor of workspace ws-1, "editor-a1b", who
// sees note-a1b only. The suite lists editor-a1b's allow probe for notes.update, on the record
// named by target, before the other editor's, as the facility package lists the technician's close
// probe before the supervisor's: so the probes of results and safety act as editor-a1b, on note-a1b
// if the audit follows the probe's own target_record and on note-a1 (the first record of the
// workspace) if it does not.
type perPrincipalRig struct {
	*simEnv
	host *perPrincipalHost
}

func newPerPrincipalRig(t *testing.T, target string) *perPrincipalRig {
	t.Helper()
	host := newPerPrincipalHost(map[string]string{"editor-a1b": "note-a1b"})
	e := newEnv(t)
	suite := &e.pkg.Tests[0]
	suite.Fixtures.Principals = append(suite.Fixtures.Principals, principal("editor-a1b", "org-a", "ws-1", "editor"))
	suite.ScopeProbes = append([]ap.PackageTestsScopeProbe{{
		ID: "editor-b-prepares-own-note", Dimension: ap.PackageTestsScopeProbeDimensionPrincipal, Operation: ap.PackageTestsScopeProbeOperationPrepare,
		CapabilityID: ptr(ap.CapabilityID(capUpdate)), Principal: "editor-a1b", TargetRecord: ptr(ap.Slug(target)), Expect: ap.PackageTestsScopeProbeExpectAllow,
	}}, suite.ScopeProbes...)

	guard, err := adapters.Wrap(host, adapters.Options{ApplicationID: simAppID, DefaultTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	host.sim.clk = e.clk
	e.sim = host.sim
	e.guard = guard
	e.in.Adapter = guard
	return &perPrincipalRig{simEnv: e, host: host}
}

// run executes the check of a section against the host, which is also the provisioner (it learns
// which note each record reference became).
func (r *perPrincipalRig) run(section int) *integrationaudit.Outcome {
	r.t.Helper()
	opts := Options{
		Source:     func(context.Context, *integrationaudit.Input) (Provisioner, error) { return r.host, nil },
		ScratchDir: r.t.TempDir(),
	}
	for _, c := range Checks(opts) {
		if c.Section() != section {
			continue
		}
		out, err := c.Run(context.Background(), r.in)
		if err != nil {
			r.t.Fatalf("check %d returned an error: %v", section, err)
		}
		return out
	}
	r.t.Fatalf("no check for section %d", section)
	return nil
}

func fieldsCovered(out *integrationaudit.Outcome, capID string) bool {
	for _, c := range out.Covered {
		if c == capID {
			return true
		}
	}
	return false
}

// TestAllowProbeUsesItsOwnTargetRecord: a probe of a capability acts as the principal of the
// capability's first allow probe, and on that probe's own target_record, not on the first record
// of the workspace: a principal that sees only some of them is otherwise refused on a record it
// cannot see, and the capability is reported as not probed (the facility-maintenance case:
// workorders.close as the technician on the site's first work order).
func TestAllowProbeUsesItsOwnTargetRecord(t *testing.T) {
	t.Run("the request is built on the target of the principal's own allow probe", func(t *testing.T) {
		pkg := simPackage(t)
		suite := pkg.Tests[0]
		suite.Fixtures.Principals = append(suite.Fixtures.Principals, principal("editor-a1b", "org-a", "ws-1", "editor"))
		suite.ScopeProbes = append([]ap.PackageTestsScopeProbe{{
			ID: "editor-b-prepares-own-note", Dimension: ap.PackageTestsScopeProbeDimensionPrincipal, Operation: ap.PackageTestsScopeProbeOperationPrepare,
			CapabilityID: ptr(ap.CapabilityID(capUpdate)), Principal: "editor-a1b", TargetRecord: ptr(ap.Slug("note-a1b")), Expect: ap.PackageTestsScopeProbeExpectAllow,
		}}, suite.ScopeProbes...)
		base := contracts.Scope{DeploymentID: "alixai-audit", ApplicationID: simAppID}
		w, _, err := provision(context.Background(), idProvisioner{}, suite, base, []string{capGet, capList, capUpdate}, clock.NewFake(simT0))
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, principal, capID, want string
		}{
			{"a principal with an allow probe acts on its target", "editor-a1b", capUpdate, "note-a1b"},
			{"another principal with one acts on its own", "editor-a1", capUpdate, "note-a1"},
			{"a principal with none acts on the first record of its workspace", "viewer-a1", capGet, "note-a1"},
			{"a capability the principal has no allow probe for is built as before", "editor-a1b", capGet, "note-a1"},
		} {
			f, err := w.fieldsFor(pkg.Capabilities[tc.capID], w.principals[tc.principal], nil)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got, want := f["note"], w.records[tc.want].id; got != want {
				t.Errorf("%s: note = %v, want %s (%s)", tc.name, got, want, tc.want)
			}
		}
		// A target the capability takes no required field for is not offered to it: the request is
		// built as it would be without a target.
		list := pkg.Capabilities[capList]
		if f, err := w.fieldsFor(list, w.principals["viewer-a1"], nil); err != nil || len(f) != 0 {
			t.Errorf("a capability with no record field was given fields %v (%v)", f, err)
		}
		// An explicit target still wins over the principal's own.
		f, err := w.fieldsFor(pkg.Capabilities[capUpdate], w.principals["editor-a1b"], w.records["note-a1"])
		if err != nil || f["note"] != w.records["note-a1"].id {
			t.Errorf("an explicit target was replaced: %v, %v", f, err)
		}
	})

	t.Run("the audit probes the capability as that principal on that record", func(t *testing.T) {
		r := newPerPrincipalRig(t, "note-a1b")
		out := r.run(integrationaudit.SectionDeterministic)
		if hasCritical(out) || len(out.Unverified) != 0 {
			t.Fatalf("a host that refuses a principal a record it cannot see must pass when the probe targets its own record:\n%s", dump(out))
		}
		for _, id := range []string{capGet, capList, capUpdate} {
			if !fieldsCovered(out, id) {
				t.Errorf("%s was not probed: covered = %v\n%s", id, out.Covered, dump(out))
			}
		}
		scope := r.run(integrationaudit.SectionScope)
		if hasCritical(scope) {
			t.Errorf("the scope probes of the same package must pass:\n%s", dump(scope))
		}
	})

	// SEC-029 negative control: following the probe's own target must not hide a probe that targets
	// a record its principal cannot see. The allow probe is refused and reported as critical, and the
	// probe of the result built on that target is refused too and the capability is not covered,
	// instead of the audit quietly moving to a record that works.
	t.Run("a probe that targets a record its principal cannot see is still reported", func(t *testing.T) {
		r := newPerPrincipalRig(t, "note-a1")
		scope := r.run(integrationaudit.SectionScope)
		f := finding(scope, "scope.allow_refused")
		if f == nil || f.Severity != contracts.SeverityCritical || f.Capability != capUpdate {
			t.Fatalf("want a critical scope.allow_refused on %s:\n%s", capUpdate, dump(scope))
		}
		out := r.run(integrationaudit.SectionDeterministic)
		if fieldsCovered(out, capUpdate) {
			t.Errorf("%s was reported as probed although its principal's allow probe targets a record it cannot see:\n%s", capUpdate, dump(out))
		}
		if len(out.Findings) == 0 && len(out.Unverified) == 0 {
			t.Errorf("nothing was reported for %s:\n%s", capUpdate, dump(out))
		}
	})
}
