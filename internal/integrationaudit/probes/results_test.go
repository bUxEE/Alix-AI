package probes

import (
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestProbeDeterministicResultShape is check 2: a correct host passes it, and each
// way an application can fail to return a typed, stable result is found, with a
// critical finding that names the capability.
func TestProbeDeterministicResultShape(t *testing.T) {
	t.Run("a correct host passes and every operation is covered", func(t *testing.T) {
		e := newEnv(t)
		out := e.run(integrationaudit.SectionDeterministic)
		if hasCritical(out) || len(findingIDs(out, contracts.SeverityWarning)) > 0 {
			t.Fatalf("a correct host must pass:\n%s", dump(out))
		}
		if len(out.Unverified) != 0 {
			t.Errorf("nothing is unverified for a correct host:\n%s", dump(out))
		}
		want := []string{capGet, capList, capUpdate}
		if len(out.Covered) != len(want) {
			t.Fatalf("covered = %v, want %v", out.Covered, want)
		}
		for i := range want {
			if out.Covered[i] != want[i] {
				t.Errorf("covered = %v, want %v", out.Covered, want)
			}
		}
		var cov *integrationaudit.CoverageItem
		for i := range out.Coverage {
			if out.Coverage[i].Name == "deterministic_result_operations" {
				cov = &out.Coverage[i]
			}
		}
		// get: read; list: read; update: validate, prepare, commit, status
		if cov == nil || cov.Covered != 6 || cov.Total != 6 {
			t.Errorf("coverage = %+v, want 6 of 6 operations", cov)
		}
		for _, p := range out.Probes {
			if !p.Passed {
				t.Errorf("probe failed on a correct host: %+v", p)
			}
		}
		if e.sim.tenantCount() != 0 || e.sim.noteCount() != 0 {
			t.Errorf("the disposable tenants were left behind: %d tenants, %d notes", e.sim.tenantCount(), e.sim.noteCount())
		}
	})

	t.Run("a refusal without freshness is the documented exception, not a failure", func(t *testing.T) {
		// The sim's refusals (rejected, unavailable) carry no freshness member, as
		// the contract allows: freshness is required only for ok, queued and
		// partial. The probe asks for a refusal and must accept it.
		e := newEnv(t)
		out := e.run(integrationaudit.SectionDeterministic)
		var asked bool
		for _, p := range out.Probes {
			if p.Subject == "read without its required fields" || p.Subject == "validate without its required fields" {
				asked = true
				if !p.Passed || p.Observed != "a typed rejected result" {
					t.Errorf("refusal probe = %+v", p)
				}
			}
		}
		if !asked {
			t.Fatalf("no refusal was asked for:\n%s", dump(out))
		}
		if hasFinding(out, "probes.untyped_reply") {
			t.Errorf("a rejected result without freshness was held against the host:\n%s", dump(out))
		}
	})

	cases := []struct {
		name string
		knob func(*simKnobs)
		want string
		cap  string
	}{
		{"an ok result without freshness is not a typed result", func(k *simKnobs) { k.okNoFreshness = true }, "probes.untyped_reply", capGet},
		{"a prepare answered in prose is not a typed result", func(k *simKnobs) { k.prose = true }, "probes.untyped_reply", capUpdate},
		{"the same request listing in another order is not stable", func(k *simKnobs) { k.unstableRead = true }, "results.unstable", capList},
		{"validate with a per-response version breaks the prepare snapshot contract", func(k *simKnobs) { k.etagVersion = true }, "results.validate_version_mismatch", capUpdate},
		{"a record with two data versions cannot become a preparation", func(k *simKnobs) { k.dupVersions = true }, "results.prepare_unusable", capUpdate},
		{"a status that reports another outcome than the commit", func(k *simKnobs) { k.statusLies = true }, "results.status_disagrees", capUpdate},
		{"a commit result naming a record the preparation does not target", func(k *simKnobs) { k.commitUnbound = true }, "results.commit_unbound", capUpdate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.knob)
			out := e.run(integrationaudit.SectionDeterministic)
			var found *contracts.Finding
			for i := range out.Findings {
				if out.Findings[i].CheckID == tc.want {
					found = &out.Findings[i]
				}
			}
			if found == nil {
				t.Fatalf("finding %s not raised:\n%s", tc.want, dump(out))
			}
			if found.Severity != contracts.SeverityCritical || found.Capability != tc.cap {
				t.Errorf("finding = %s, want critical on %s", found, tc.cap)
			}
			if e.sim.tenantCount() != 0 {
				t.Errorf("the disposable tenants were left behind after a failing probe")
			}
		})
	}

}
