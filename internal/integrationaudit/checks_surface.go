package integrationaudit

import (
	"context"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// lookalike folds a name the way the loader does (apppkg adapter.lookalike_operation):
// lower case, with '_', '-' and '.' removed. Two different names that fold to
// the same string could be mistaken for one another.
func lookalike(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("_", "", "-", "", ".", "").Replace(s)
}

// runControlSurface is the application half of check 7: the declared control
// surface is compared with what the application's adapter says it exposes, and
// every deviation is reported.
//
// Describe returns, per capability, the operations the adapter can perform and
// the host operations no capability maps. Against the package that gives four
// classes of problem:
//
//   - unreachable: the package maps an operation the application does not offer
//     for that capability. The capability cannot work as declared.
//   - over-broad: the application offers an operation, or a whole capability,
//     the package does not declare. An operation that can prepare or commit is
//     critical; a read-only one is a warning. cancel is part of the adapter
//     contract and has no package mapping, so it is never over-broad.
//   - undocumented: a host operation that no capability maps (Describe's
//     Unmapped). It is invisible to the model and uncallable.
//   - ambiguous: a host operation whose name differs only by case or by '_',
//     '-' and '.' from a mapped one.
//
// Nothing here widens anything: every finding is a report. An accepted warning
// leaves the operation exactly as uncallable as it was.
func runControlSurface(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	pkg := in.Package
	if pkg == nil {
		out.Unverify(Unverified{Subject: "control_surface", Section: SectionControlSurface, Target: TargetApplication,
			Reason: "the package could not be loaded and verified, so its declared surface is unknown"})
		return out, nil
	}
	ids := pkg.SortedCapabilityIDs()
	unverifyAll := func(reason string) {
		for _, id := range ids {
			out.Verdicts = append(out.Verdicts, CapabilityVerdict{CapabilityID: id, Status: CapabilityUnverified, Reason: reason})
		}
		out.Unverify(Unverified{Subject: "control_surface", Section: SectionControlSurface, Target: TargetApplication, Reason: reason})
	}
	if in.Adapter == nil {
		out.Add(newFinding(fdNotInspected, "", "", ""))
		unverifyAll("no inspection adapter was available")
		return out, nil
	}
	if in.Description == nil {
		out.Add(newFinding(fdDescribeFailed, "", "", "the adapter could not describe the application (%s)", quote(string(in.DescribeErr))))
		unverifyAll("the adapter could not describe the application")
		return out, nil
	}
	desc := *in.Description
	if _, err := adapters.SelectVersion(adapters.SupportedContractVersions, desc.ContractVersions); err != nil {
		out.Add(newFinding(fdVersionIncompatible, "", "", "the adapter speaks contract versions %v, this core speaks %v",
			desc.ContractVersions, adapters.SupportedContractVersions))
	}

	// What the package maps: operations per capability, and every call name.
	mapped := map[string]bool{}
	for _, id := range ids {
		c := pkg.Capabilities[id]
		for _, op := range declaredOperations(c) {
			mapped[callName(bindingOf(c, op))] = true
		}
		for _, et := range c.EntityTypes {
			if et.Resolve != nil {
				mapped[callName(et.Resolve)] = true
			}
		}
	}
	delete(mapped, "")

	discovered := map[string]map[string]bool{}
	for _, c := range desc.Capabilities {
		set := map[string]bool{}
		for _, op := range c.Operations {
			set[string(op)] = true
		}
		discovered[c.CapabilityID] = set
	}

	reachable, total := 0, 0
	for _, id := range ids {
		c := pkg.Capabilities[id]
		out.Covered = append(out.Covered, id)
		declared := map[string]bool{}
		for _, op := range declaredOperations(c) {
			declared[op] = true
			total++
			if desc.Supports(id, adapters.Operation(op)) {
				reachable++
				continue
			}
			out.Add(newFinding(fdUnreachable, id, "capabilities/",
				"capability %s maps its %s operation to %s, but the application does not offer it",
				quote(id), op, quote(callName(bindingOf(c, op)))))
		}
		extra := make([]string, 0)
		for op := range discovered[id] {
			if !declared[op] && op != string(adapters.OpCancel) {
				extra = append(extra, op)
			}
		}
		sort.Strings(extra)
		for _, op := range extra {
			d := fdOverBroadMinor
			if op == string(adapters.OpPrepare) || op == string(adapters.OpCommit) {
				d = fdOverBroad
			}
			out.Add(newFinding(d, id, "capabilities/",
				"the application offers %s for capability %s, which the package does not declare for it", op, quote(id)))
		}
	}

	var undeclared []string
	for id := range discovered {
		if _, ok := pkg.Capabilities[id]; !ok {
			undeclared = append(undeclared, id)
		}
	}
	sort.Strings(undeclared)
	for _, id := range undeclared {
		out.Add(newFinding(fdUndeclaredCapability, "", "",
			"the application describes capability %s (%s), which the package does not declare",
			quote(id), operationList(discovered[id])))
	}

	folded := map[string]string{} // folded mapped name -> mapped name
	for name := range mapped {
		folded[lookalike(name)] = name
	}
	unmapped := append([]string(nil), desc.Unmapped...)
	sort.Strings(unmapped)
	for _, name := range uniqueSorted(unmapped) {
		if mapped[name] {
			continue
		}
		if m, ok := folded[lookalike(name)]; ok {
			out.Add(newFinding(fdAmbiguous, "", "",
				"the application exposes operation %s, which differs only by case or by '_', '-' and '.' from the mapped operation %s",
				quote(name), quote(m)))
			continue
		}
		out.Add(newFinding(fdUndocumented, "", "",
			"the application exposes operation %s, which no capability of the package maps", quote(name)))
	}

	if v := desc.ApplicationVersion; v != "" && pkg.Manifest.Application.Version != "" && v != pkg.Manifest.Application.Version {
		out.Add(newFinding(fdAppVersion, "", "alixai.yaml",
			"the application reports version %s, the package was written for %s", quote(v), quote(pkg.Manifest.Application.Version)))
	}
	out.Coverage = append(out.Coverage, CoverageItem{Name: "control_surface_operations", Covered: reachable, Total: total})
	return out, nil
}

func operationList(set map[string]bool) string {
	ops := make([]string, 0, len(set))
	for op := range set {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	return strings.Join(ops, ", ")
}

// profileRank orders the model profiles of master plan 7.
var profileRank = map[string]int{"experimental-4g": 0, "lite": 1, "balanced": 2, "performance": 3}

// runInspection is the runtime and system target of master plan 4.5: reachable
// service version, health, and whichever of the nine aspects the application
// chooses to expose. AlixAI never scans for the rest; what is not exposed is
// recorded as unverified, and an unverified aspect is never shown as passed.
func runInspection(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	pkg := in.Package

	// The model and runtime profile against the package's requirement.
	switch {
	case in.Profile == "":
		out.Unverify(Unverified{Subject: "profile", Section: sectionSupplementary, Target: TargetRuntime,
			Reason: "no model or runtime profile was configured for this audit"})
	case pkg == nil:
	default:
		min := string(pkg.Manifest.ModelRequirements.MinimumProfile)
		have, known := profileRank[in.Profile]
		need, needKnown := profileRank[min]
		switch {
		case !known:
			out.Unverify(Unverified{Subject: "profile", Section: sectionSupplementary, Target: TargetRuntime,
				Reason: "the configured profile is not one of experimental-4g, lite, balanced and performance"})
		case needKnown && have < need:
			out.Add(newFinding(fdProfileBelow, "", "alixai.yaml",
				"the package requires at least profile %s, the audit ran on %s", quote(min), quote(in.Profile)))
		}
	}

	all := adapters.AllAspects()
	unverifyAspects := func(reason string) {
		for _, a := range all {
			out.Unverify(Unverified{Subject: "aspect:" + string(a), Section: sectionSupplementary, Target: TargetRuntime, Reason: reason})
		}
	}
	if in.Adapter == nil {
		unverifyAspects("no inspection adapter was available")
		out.Unverify(Unverified{Subject: "health", Section: sectionSupplementary, Target: TargetRuntime,
			Reason: "no inspection adapter was available"})
		return out, nil
	}

	switch {
	case in.Health == nil:
		out.Add(newFinding(fdHealthUnknown, "", "", "the application's health could not be read (%s)", quote(string(in.HealthErr))))
		out.Unverify(Unverified{Subject: "health", Section: sectionSupplementary, Target: TargetRuntime,
			Reason: "the health probe failed"})
	case in.Health.Status == adapters.HealthUnavailable:
		out.Add(newFinding(fdHealthDown, "", "", "the application reports itself unavailable"))
	case in.Health.Status == adapters.HealthDegraded:
		out.Add(newFinding(fdHealthDegraded, "", "", "the application reports itself degraded"))
	}

	if in.Inspection == nil {
		out.Add(newFinding(fdInspectFailed, "", "", "the application's inspection call failed (%s)", quote(string(in.InspectErr))))
		unverifyAspects("the inspection call failed")
		return out, nil
	}
	states := map[string]any{}
	facts := map[string]any{}
	for _, r := range in.Inspection.Reports {
		states[string(r.Aspect)] = string(r.State)
		if r.State != adapters.AspectReported {
			out.Unverify(Unverified{Subject: "aspect:" + string(r.Aspect), Section: sectionSupplementary, Target: TargetRuntime,
				Reason: "the application does not expose this aspect for inspection"})
			continue
		}
		for _, f := range r.Facts {
			facts[string(r.Aspect)+"."+string(f.Field)] = factString(f)
		}
		if r.Aspect == adapters.AspectVersions && pkg != nil {
			for _, f := range r.Facts {
				if string(f.Field) != "application_version" {
					continue
				}
				v := textOf(f)
				want := pkg.Manifest.Application.Version
				if v != "" && want != "" && v != want {
					out.Add(newFinding(fdSysVersion, "", "alixai.yaml",
						"the application's inspection reports version %s, the package was written for %s", quote(v), quote(want)))
				}
			}
		}
	}
	out.Extension = map[string]any{"aspects": states, "facts": facts}
	return out, nil
}

// textOf returns the value of a text fact, or "" for any other kind.
func textOf(f actions.Fact) string {
	if t, ok := f.Value.Value.(actions.TextValue); ok {
		return t.Value
	}
	return ""
}

// factString renders a fact's canonical value for the report as its canonical
// JSON, so any kind is representable without floating point.
func factString(f actions.Fact) string {
	b, err := f.Value.MarshalJSON()
	if err != nil {
		return "unrepresentable"
	}
	if canon, err := trust.Canonicalize(b); err == nil {
		return string(canon)
	}
	return string(b)
}
