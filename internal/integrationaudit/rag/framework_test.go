package rag

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestChecksRegisterWithTheFramework: importing this package is all it takes to register the
// four RAG checks with the audit's default registry, in the sections they belong to, and the
// framework accepts each as it accepts a built-in one.
func TestChecksRegisterWithTheFramework(t *testing.T) {
	want := map[string]int{
		IDSources:   integrationaudit.SectionRAGInventory,
		IDCitations: integrationaudit.SectionRAGInventory,
		IDScope:     integrationaudit.SectionScope,
		IDSmoke:     integrationaudit.SectionRuntimeSmoke,
	}
	have := map[string]int{}
	for _, c := range integrationaudit.Default.Checks() {
		if _, ok := want[c.ID()]; ok {
			have[c.ID()] = c.Section()
			if c.Target() != integrationaudit.TargetApplication {
				t.Errorf("%s audits the %s target, want the application", c.ID(), c.Target())
			}
		}
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("the default registry holds %v, want %v", have, want)
	}
	reg := integrationaudit.NewRegistry()
	for _, c := range Checks(Options{}) {
		if err := reg.Register(c); err != nil {
			t.Errorf("the framework refuses %s: %v", c.ID(), err)
		}
	}
}

// TestNoStaticOrM118RecordVanishes: registering a check of section 3, 6 or 10 silences what the
// static check and the runtime probes record for want of one (the live half of the source
// inventory, the retrieve scope probes, the RAG smoke paths). Whatever this package cannot
// look at, with no deployment seam, no adapter and sources nothing can host, it must therefore
// record itself: every declared source, every retrieve probe and every RAG path of both
// reference packages is accounted for, as unverified, and nothing raises a finding.
func TestNoStaticOrM118RecordVanishes(t *testing.T) {
	for _, dir := range []string{clinicDir, validMinimalDir} {
		pkg := loadRef(t, dir)
		var all, findings []string
		var failed []integrationaudit.ProbeResult
		var coverageTotal int
		for _, c := range Checks(Options{}) {
			out := runCheck(t, c, auditInput(pkg))
			all = append(all, unverifiedSubjects(out)...)
			findings = append(findings, findingIDs(out, contracts.SeverityCritical)...)
			findings = append(findings, findingIDs(out, contracts.SeverityWarning)...)
			failed = append(failed, failedProbes(out)...)
			if c.ID() == IDSmoke {
				for _, ci := range out.Coverage {
					if ci.Name == "smoke_paths" {
						coverageTotal += ci.Total
					}
				}
			}
		}
		if len(findings) > 0 {
			t.Errorf("%s: the no-seam path raised findings %v; it must be unverified only", dir, findings)
		}
		if len(failed) > 0 {
			t.Errorf("%s: probes failed where nothing could be looked at: %+v", dir, failed)
		}
		have := map[string]bool{}
		for _, s := range all {
			have[s] = true
		}
		need := []string{"rag_sources:registrations"}
		for id := range pkg.Sources {
			need = append(need, "rag_source:"+id)
		}
		paths := 0
		for _, suite := range pkg.Tests {
			for _, p := range retrieveProbes(suite) {
				need = append(need, "scope_probe:"+string(p.ID))
			}
			for _, p := range ragSmokePaths(suite) {
				need = append(need, "smoke_path:"+string(p.ID))
				paths++
			}
		}
		sort.Strings(need)
		for _, n := range need {
			if !have[n] {
				t.Errorf("%s: %s is not accounted for; unverified = %v", dir, n, all)
			}
		}
		if coverageTotal != paths || paths == 0 {
			t.Errorf("%s: smoke_paths total = %d, want the %d RAG paths", dir, coverageTotal, paths)
		}
	}
}

// TestChecksAreDeterministic: two audits of the same package give the same results, byte for
// byte: nothing the audit generates for a run (identifiers, markers, times, directories) is in
// them.
func TestChecksAreDeterministic(t *testing.T) {
	pkg := labPackage(t)
	for _, c := range Checks(Options{}) {
		a := runCheck(t, c, auditInput(pkg))
		b := runCheck(t, c, auditInput(pkg))
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s gave two different results for the same package:\n%s\n---\n%s", c.ID(), dump(a), dump(b))
		}
	}
}

// TestChecksSurviveACancelledAudit: an audit that is cancelled before a check runs leaves the
// check with nothing it can say; it must not panic, must raise no finding for it, and must not
// leave its store behind.
func TestChecksSurviveACancelledAudit(t *testing.T) {
	scratch := t.TempDir()
	pkg := labPackage(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range Checks(Options{ScratchDir: scratch}) {
		out, err := c.Run(ctx, auditInput(pkg))
		if err != nil {
			t.Fatalf("%s: %v", c.ID(), err)
		}
		validOutcome(t, c.ID(), out)
		if len(out.Findings) > 0 {
			t.Errorf("%s: a cancelled audit drew findings:\n%s", c.ID(), dump(out))
		}
		if len(out.Unverified) == 0 {
			t.Errorf("%s: a cancelled audit left nothing unverified", c.ID())
		}
	}
	entries, err := readDirNames(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the scratch directory still holds %v", entries)
	}
}

// TestDeploymentSourceIsConsulted: a source registered with SetDeploymentSource is what the
// checks look at, and the first source that returns a deployment wins.
func TestDeploymentSourceIsConsulted(t *testing.T) {
	sourceMu.Lock()
	saved := deploymentSrcs
	deploymentSrcs = nil
	sourceMu.Unlock()
	t.Cleanup(func() {
		sourceMu.Lock()
		deploymentSrcs = saved
		sourceMu.Unlock()
	})

	called := 0
	SetDeploymentSource(nil) // ignored
	SetDeploymentSource(func(context.Context, *integrationaudit.Input) (*Deployment, error) { called++; return nil, nil })
	chosen := folderDeployment(nil)
	chosen.Registrations = func(context.Context, string) ([]Registration, error) {
		return []Registration{{PackageSourceID: "shadow-docs"}}, nil
	}
	removeChosen := SetDeploymentSource(func(context.Context, *integrationaudit.Input) (*Deployment, error) { return chosen, nil })
	after := 0
	SetDeploymentSource(func(context.Context, *integrationaudit.Input) (*Deployment, error) { after++; return nil, nil })

	out := runCheck(t, checkOf(t, Options{}, IDSources), auditInput(labPackage(t)))
	if called != 1 || !hasFinding(out, fdUndeclared.id) {
		t.Fatalf("source called %d times, findings:\n%s", called, dump(out))
	}

	// a source that is removed is consulted no more: the audit falls back to the default
	// deployment, which cannot list registrations
	removeChosen()
	out = runCheck(t, checkOf(t, Options{}, IDSources), auditInput(labPackage(t)))
	if hasFinding(out, fdUndeclared.id) || !hasUnverified(out, "rag_sources:registrations") {
		t.Fatalf("a removed source was still consulted:\n%s", dump(out))
	}

	// a source that fails leaves the deployment unknown: unverified, never clean
	sourceMu.Lock()
	deploymentSrcs = nil
	sourceMu.Unlock()
	SetDeploymentSource(func(context.Context, *integrationaudit.Input) (*Deployment, error) {
		return nil, contracts.New(contracts.ErrUnavailable, "the registry is locked")
	})
	out = runCheck(t, checkOf(t, Options{}, IDSources), auditInput(labPackage(t)))
	if !hasUnverified(out, "rag_sources:registrations") || !hasUnverified(out, "rag_source:loan-policies") || hasFinding(out, fdUndeclared.id) {
		t.Fatalf("%s", dump(out))
	}
}

// TestChecksWithoutAPackageAreUnverified: a check handed no package (the framework never does
// that to a check that is not PackageOptional, but a caller may) says it looked at nothing and
// raises nothing.
func TestChecksWithoutAPackageAreUnverified(t *testing.T) {
	for _, c := range Checks(Options{}) {
		in := auditInput(labPackage(t))
		in.Package = nil
		out := runCheck(t, c, in)
		if len(out.Findings) > 0 || len(out.Unverified) != 1 || len(out.Probes) > 0 {
			t.Errorf("%s:\n%s", c.ID(), dump(out))
		}
	}
}

// TestFindingDefinitions: every finding of this package has its own identifier, in the
// package's family, a fixed summary and a remediation, and is critical or a warning.
func TestFindingDefinitions(t *testing.T) {
	list := Findings()
	if len(list) < 20 {
		t.Fatalf("only %d finding definitions", len(list))
	}
	seen := map[string]bool{}
	for _, f := range list {
		switch {
		case seen[f.ID]:
			t.Errorf("%s is defined twice", f.ID)
		case !strings.HasPrefix(f.ID, "ragprobe."):
			t.Errorf("%s is outside the ragprobe. family", f.ID)
		case f.Summary == "" || f.Remediation == "":
			t.Errorf("%s has no summary or no remediation", f.ID)
		case f.Severity != contracts.SeverityCritical && f.Severity != contracts.SeverityWarning:
			t.Errorf("%s has severity %q", f.ID, f.Severity)
		}
		seen[f.ID] = true
	}
}
