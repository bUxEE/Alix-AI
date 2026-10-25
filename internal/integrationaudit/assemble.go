package integrationaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// executed is one check and what it produced.
type executed struct {
	check   Check
	outcome *Outcome
	status  Status
}

// execute runs the checks in order and returns what each produced. A check that
// panics, returns an error, returns something invalid or does not run because
// there is no package never passes: it becomes a finding or an unverified item.
func execute(ctx context.Context, in *Input, checks []Check) []executed {
	out := make([]executed, 0, len(checks))
	for _, c := range checks {
		out = append(out, runOne(ctx, in, c))
	}
	return out
}

func runOne(ctx context.Context, in *Input, c Check) (res executed) {
	res.check = c
	if in.Package == nil {
		if po, ok := c.(PackageOptional); !ok || !po.RunsWithoutPackage() {
			res.outcome = &Outcome{}
			res.outcome.Unverify(Unverified{
				Subject: c.ID(), Section: c.Section(), Target: c.Target(),
				Reason: "the package could not be loaded and verified, so this check did not run",
			})
			res.status = StatusUnverified
			return res
		}
	}
	defer func() {
		if p := recover(); p != nil {
			res.outcome = failedCheck(c, fmt.Sprintf("it panicked: %v", p))
			res.status = StatusFail
		}
	}()
	o, err := c.Run(ctx, in)
	switch {
	case err != nil:
		res.outcome = failedCheck(c, "it returned an error ("+quote(string(contracts.CodeOf(err)))+")")
		res.status = StatusFail
		return res
	case o == nil:
		res.outcome = failedCheck(c, "it returned no outcome")
		res.status = StatusFail
		return res
	}
	if msg := validateOutcome(c, o); msg != "" {
		res.outcome = &Outcome{}
		res.outcome.Add(newFinding(fdCheckInvalid, "", "", "check %s returned an invalid outcome: %s", quote(c.ID()), msg))
		res.status = StatusFail
		return res
	}
	res.outcome = o
	res.status = statusOf(o)
	return res
}

func failedCheck(c Check, why string) *Outcome {
	o := &Outcome{}
	o.Add(newFinding(fdCheckFailed, "", "", "check %s could not run: %s", quote(c.ID()), why))
	return o
}

// statusOf derives a check's status from what it reported.
func statusOf(o *Outcome) Status {
	crit, _, _ := contracts.CountSeverity(o.Findings)
	switch {
	case crit > 0:
		return StatusFail
	case len(o.Unverified) > 0:
		return StatusUnverified
	}
	for _, v := range o.Verdicts {
		if v.Status == CapabilityFailed {
			return StatusFail
		}
		if v.Status == CapabilityUnverified {
			return StatusUnverified
		}
	}
	return StatusPass
}

// validateOutcome returns why an outcome cannot be reported, or "".
func validateOutcome(c Check, o *Outcome) string {
	for _, f := range o.Findings {
		if err := f.Validate(); err != nil {
			return err.Error()
		}
	}
	for _, v := range o.Verdicts {
		if !v.Status.Valid() || v.CapabilityID == "" {
			return "a capability verdict has no capability or an unknown status"
		}
	}
	for _, u := range o.Unverified {
		if !u.Target.Valid() || u.Subject == "" || u.Reason == "" {
			return "an unverified item has no subject, reason or valid target"
		}
	}
	for _, p := range o.Probes {
		if p.Kind == "" {
			return "a probe has no kind"
		}
	}
	for _, m := range o.Measurements {
		if m.Name == "" || m.Unit == "" {
			return "a measurement has no name or unit"
		}
	}
	for _, ci := range o.Coverage {
		if ci.Name == "" || ci.Covered < 0 || ci.Total < ci.Covered {
			return "a coverage item has no name or inconsistent counts"
		}
	}
	if o.Extension != nil {
		if _, err := canonical(o.Extension); err != nil {
			return "its extension data is not canonical JSON (strings, integers, booleans, arrays and objects only)"
		}
	}
	// Everything else goes into the signed document too: refuse text that cannot
	// be canonicalised here, where it can be pinned on the check, and not when
	// the whole report is signed.
	parts := struct {
		Findings     []contracts.Finding
		Verdicts     []CapabilityVerdict
		Unverified   []Unverified
		Sources      []SourceResult
		Coverage     []CoverageItem
		Probes       []ProbeResult
		Measurements []Measurement
		Permissions  Permissions
		External     *External
		Covered      []string
	}{o.Findings, o.Verdicts, o.Unverified, o.Sources, o.Coverage, o.Probes, o.Measurements, o.Permissions, o.External, o.Covered}
	if _, err := canonical(parts); err != nil || invalidText(reflect.ValueOf(parts)) || invalidText(reflect.ValueOf(o.Extension)) {
		return "its results cannot be represented in the signed report (invalid text or numbers that are not integers)"
	}
	return ""
}

// assemble builds the deterministic report from the input and what the checks
// produced. It is a pure function: the same input and outcomes give the same
// report, byte for byte.
func assemble(in *Input, results []executed) (*Report, error) {
	r := &Report{Format: ReportFormat}
	r.Subject = subjectOf(in)
	r.Profile = profileOf(in)
	r.Surface = surfaceOf(in)

	var all []contracts.Finding
	var unverified []Unverified
	verdicts := map[string][]CapabilityVerdict{}
	covered := map[string]bool{}
	coverage := map[string]*CoverageItem{}
	sources := map[string]SourceResult{}
	ext := External{Sources: []ExternalSource{}, Fields: []ExternalField{}}
	var perms Permissions
	sectionChecks := map[int][]string{}
	sectionStatus := map[int]Status{}
	targetChecks := map[Target][]string{}
	targetStatus := map[Target]Status{}

	for _, ex := range results {
		c, o := ex.check, ex.outcome
		crit, warn, inf := contracts.CountSeverity(o.Findings)
		r.Checks = append(r.Checks, CheckRecord{
			ID: c.ID(), Title: c.Title(), Section: c.Section(), Stage: c.Stage().String(), Target: c.Target(),
			Status: ex.status, Critical: crit, Warnings: warn, Info: inf, Unverified: len(o.Unverified),
		})
		sectionChecks[c.Section()] = append(sectionChecks[c.Section()], c.ID())
		sectionStatus[c.Section()] = worse(sectionStatus[c.Section()], ex.status)
		targetChecks[c.Target()] = append(targetChecks[c.Target()], c.ID())
		targetStatus[c.Target()] = worse(targetStatus[c.Target()], ex.status)

		all = append(all, o.Findings...)
		unverified = append(unverified, o.Unverified...)
		for _, id := range o.Covered {
			covered[id] = true
		}
		for _, v := range o.Verdicts {
			verdicts[v.CapabilityID] = append(verdicts[v.CapabilityID], v)
		}
		for _, ci := range o.Coverage {
			if cur, ok := coverage[ci.Name]; ok {
				cur.Covered += ci.Covered
				cur.Total += ci.Total
			} else {
				cp := ci
				coverage[ci.Name] = &cp
			}
		}
		for _, s := range o.Sources {
			if cur, ok := sources[s.ID]; !ok || (s.Verified == "live" && cur.Verified != "live") {
				sources[s.ID] = s
			}
		}
		r.Probes = append(r.Probes, o.Probes...)
		r.Measurements = append(r.Measurements, o.Measurements...)
		perms.Missing = append(perms.Missing, o.Permissions.Missing...)
		perms.Excessive = append(perms.Excessive, o.Permissions.Excessive...)
		perms.Contradictory = append(perms.Contradictory, o.Permissions.Contradictory...)
		if e := o.External; e != nil {
			ext.DeclaredByPackage = ext.DeclaredByPackage || e.DeclaredByPackage
			ext.ForbidExternal = ext.ForbidExternal || e.ForbidExternal
			ext.Sources = append(ext.Sources, e.Sources...)
			ext.Fields = append(ext.Fields, e.Fields...)
		}
		if o.Extension != nil {
			r.Extensions = append(r.Extensions, Extension{Check: c.ID(), Data: o.Extension})
		}
	}

	// A loader finding that no check reported would be lost by a registry that
	// lacks the check for its section, so it is added here: the audit can only
	// say more than the loader, never less.
	reported := map[string]bool{}
	for _, f := range all {
		reported[FindingKey(f)] = true
	}
	var stragglers []contracts.Finding
	for _, f := range in.LoadFindings {
		if !reported[FindingKey(f)] {
			stragglers = append(stragglers, f)
			reported[FindingKey(f)] = true
		}
	}
	if len(stragglers) > 0 {
		all = append(all, stragglers...)
		crit, warn, inf := contracts.CountSeverity(stragglers)
		st := StatusPass
		if crit > 0 {
			st = StatusFail
		}
		r.Checks = append(r.Checks, CheckRecord{
			ID: "package.loader", Title: "Package loader findings", Section: sectionSupplementary, Stage: StageSignature.String(),
			Target: TargetPackage, Status: st, Critical: crit, Warnings: warn, Info: inf,
		})
		targetChecks[TargetPackage] = append(targetChecks[TargetPackage], "package.loader")
		targetStatus[TargetPackage] = worse(targetStatus[TargetPackage], st)
	}

	// The ten sections of master plan 4.5. A section nothing implements is not
	// run: that is a warning, never a pass.
	for n := 1; n <= sectionCount; n++ {
		st := StatusNotRun
		if len(sectionChecks[n]) > 0 {
			st = sectionStatus[n]
		} else {
			all = append(all, newFinding(fdSectionNotRun, "", "",
				"check %d of master plan 4.5 (%s) has no registered check, so nothing is known about it", n, SectionTitle(n)))
		}
		r.Sections = append(r.Sections, SectionResult{Number: n, Title: SectionTitle(n), Status: st, Checks: uniqueSorted(sectionChecks[n])})
	}
	for _, t := range Targets() {
		st := StatusNotRun
		if len(targetChecks[t]) > 0 {
			st = targetStatus[t]
		}
		tr := TargetResult{Target: t, Status: st, Checks: uniqueSorted(targetChecks[t]), Unverified: []string{}}
		for _, u := range unverified {
			if u.Target == t {
				tr.Unverified = append(tr.Unverified, u.Subject)
			}
		}
		tr.Unverified = uniqueSorted(tr.Unverified)
		r.Targets = append(r.Targets, tr)
	}

	// Findings: distinct, ordered, keyed.
	contracts.SortFindings(all)
	seen := map[string]contracts.Finding{}
	r.Findings = []KeyedFinding{}
	for _, f := range all {
		k := FindingKey(f)
		if prev, dup := seen[k]; dup {
			if prev != f {
				return nil, contracts.New(contracts.ErrInternal, "integrationaudit: two different findings have the same key")
			}
			continue
		}
		seen[k] = f
		r.Findings = append(r.Findings, KeyedFinding{Key: k, Finding: f})
	}

	// Capabilities.
	if in.Package != nil {
		for _, id := range in.Package.SortedCapabilityIDs() {
			c := in.Package.Capabilities[id]
			res := CapabilityResult{
				ID: id, Audience: string(c.Audience), RiskLevel: int(c.RiskLevel),
				Operations: declaredOperations(c), Findings: []string{}, Notes: []string{},
			}
			status := CapabilitySkipped
			if covered[id] {
				status = CapabilityPassed
			}
			raise := func(s CapabilityStatus) {
				if s.rank() > status.rank() {
					status = s
				}
			}
			for _, kf := range r.Findings {
				if kf.Capability != id {
					continue
				}
				res.Findings = append(res.Findings, kf.Key)
				if kf.Severity.Rank() == 0 {
					raise(CapabilityFailed)
				}
			}
			for _, u := range unverified {
				if u.Capability == id {
					raise(CapabilityUnverified)
					res.Notes = append(res.Notes, "unverified: "+u.Reason)
				}
			}
			for _, v := range verdicts[id] {
				raise(v.Status)
				if v.Reason != "" {
					res.Notes = append(res.Notes, string(v.Status)+": "+v.Reason)
				}
			}
			res.Status = status
			res.Notes = uniqueSorted(res.Notes)
			sort.Strings(res.Findings)
			r.Capabilities = append(r.Capabilities, res)
		}
	}
	if r.Capabilities == nil {
		r.Capabilities = []CapabilityResult{}
	}

	r.RAGSources = []SourceResult{}
	for _, s := range sources {
		s.Declared = nz(s.Declared)
		s.Missing = nz(s.Missing)
		r.RAGSources = append(r.RAGSources, s)
	}
	sort.Slice(r.RAGSources, func(i, j int) bool { return r.RAGSources[i].ID < r.RAGSources[j].ID })

	r.Coverage = []CoverageItem{}
	for _, ci := range coverage {
		r.Coverage = append(r.Coverage, *ci)
	}
	sort.Slice(r.Coverage, func(i, j int) bool { return r.Coverage[i].Name < r.Coverage[j].Name })

	r.Probes = append([]ProbeResult{}, r.Probes...)
	sort.SliceStable(r.Probes, func(i, j int) bool {
		a, b := r.Probes[i], r.Probes[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Capability != b.Capability {
			return a.Capability < b.Capability
		}
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Expected < b.Expected
	})
	r.Measurements = append([]Measurement{}, r.Measurements...)
	sort.SliceStable(r.Measurements, func(i, j int) bool { return r.Measurements[i].Name < r.Measurements[j].Name })

	r.Permissions = Permissions{
		Missing:       uniqueSorted(perms.Missing),
		Excessive:     uniqueSorted(perms.Excessive),
		Contradictory: uniqueSorted(perms.Contradictory),
	}
	sort.Slice(ext.Sources, func(i, j int) bool { return ext.Sources[i].ID < ext.Sources[j].ID })
	sort.Slice(ext.Fields, func(i, j int) bool {
		if ext.Fields[i].Capability != ext.Fields[j].Capability {
			return ext.Fields[i].Capability < ext.Fields[j].Capability
		}
		return ext.Fields[i].Field < ext.Fields[j].Field
	})
	r.External = ext

	r.Unverified = append([]Unverified{}, unverified...)
	sort.SliceStable(r.Unverified, func(i, j int) bool {
		a, b := r.Unverified[i], r.Unverified[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Section != b.Section {
			return a.Section < b.Section
		}
		if a.Subject != b.Subject {
			return a.Subject < b.Subject
		}
		return a.Capability < b.Capability
	})
	sort.SliceStable(r.Extensions, func(i, j int) bool { return r.Extensions[i].Check < r.Extensions[j].Check })
	sort.SliceStable(r.Checks, func(i, j int) bool { return r.Checks[i].ID < r.Checks[j].ID })
	if r.Extensions == nil {
		r.Extensions = []Extension{}
	}

	crit, warn, inf := 0, 0, 0
	r.Remediation = []Remediation{}
	for _, kf := range r.Findings {
		switch kf.Severity.Rank() {
		case 0:
			crit++
		case 1:
			warn++
		default:
			inf++
		}
		if kf.Severity.Rank() <= 1 {
			r.Remediation = append(r.Remediation, Remediation{
				Key: kf.Key, CheckID: kf.CheckID, Severity: kf.Severity, Capability: kf.Capability, Remediation: kf.Remediation,
			})
		}
	}
	r.Summary = Summary{Critical: crit, Warnings: warn, Info: inf, Unverified: len(unverified), Outcome: DispositionClear}
	switch {
	case crit > 0 || len(failedCapabilities(*r)) > 0:
		// A capability can be failed by an explicit verdict, with no finding.
		r.Summary.Outcome = DispositionBlocked
	case warn > 0:
		r.Summary.Outcome = DispositionNeedsAcceptance
	}

	r.ReauditTriggers = triggersOf(in)
	return r, nil
}

// failedCapabilities lists the capabilities the report fails.
func failedCapabilities(r Report) []string {
	var out []string
	for _, c := range r.Capabilities {
		if c.Status == CapabilityFailed {
			out = append(out, c.ID)
		}
	}
	return out
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func subjectOf(in *Input) Subject {
	s := Subject{
		ApplicationID:  in.Record.ApplicationID,
		PackageVersion: in.Record.PackageVersion,
		ManifestHash:   in.Record.ManifestHash,
		CoreVersion:    in.CoreVersion,
		SignerKeyID:    in.Record.SignerKeyID,
		SecurityDomain: in.Record.SecurityDomain,
		Files:          in.Record.Files,
		Bytes:          in.Record.Bytes,
	}
	if in.Package != nil {
		s.ApplicationVersion = in.Package.Manifest.Application.Version
		s.PackageVersion = string(in.Package.Manifest.Package.Version)
	}
	return s
}

func profileOf(in *Input) Profile {
	p := Profile{Name: in.Profile}
	if in.Package != nil {
		p.Minimum = string(in.Package.Manifest.ModelRequirements.MinimumProfile)
	}
	return p
}

// surfaceOf records the declared control surface and what the adapter
// discovered.
func surfaceOf(in *Input) Surface {
	s := Surface{
		ContractVersions: []int{}, Endpoints: []EndpointRecord{}, Declared: []DeclaredOperation{},
		Discovered: []DiscoveredCapability{}, Unmapped: []string{},
	}
	if pkg := in.Package; pkg != nil {
		for id, ep := range pkg.Manifest.Endpoints {
			s.Endpoints = append(s.Endpoints, EndpointRecord{ID: id, Transport: transportOf(ep)})
		}
		sort.Slice(s.Endpoints, func(i, j int) bool { return s.Endpoints[i].ID < s.Endpoints[j].ID })
		for _, id := range pkg.SortedCapabilityIDs() {
			c := pkg.Capabilities[id]
			for _, op := range declaredOperations(c) {
				b := bindingOf(c, op)
				s.Declared = append(s.Declared, DeclaredOperation{
					Capability: id, Operation: op, Endpoint: string(b.Endpoint), Call: callName(b),
				})
			}
		}
	}
	if d := in.Description; d != nil {
		s.Inspected = true
		s.Transport = d.Transport
		s.ContractVersions = append(s.ContractVersions, d.ContractVersions...)
		sort.Ints(s.ContractVersions)
		s.ApplicationVersion = d.ApplicationVersion
		for _, c := range d.Capabilities {
			dc := DiscoveredCapability{CapabilityID: c.CapabilityID, Operations: []string{}}
			for _, op := range c.Operations {
				dc.Operations = append(dc.Operations, string(op))
			}
			sort.Strings(dc.Operations)
			s.Discovered = append(s.Discovered, dc)
		}
		sort.Slice(s.Discovered, func(i, j int) bool { return s.Discovered[i].CapabilityID < s.Discovered[j].CapabilityID })
		s.Unmapped = uniqueSorted(d.Unmapped)
	}
	return s
}

func transportOf(ep interface{ MarshalJSON() ([]byte, error) }) string {
	b, err := ep.MarshalJSON()
	if err != nil {
		return "unknown"
	}
	var m struct {
		Transport string `json:"transport"`
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Transport == "" {
		return "unknown"
	}
	return m.Transport
}

// triggersOf records the hash of everything whose change calls for a new audit
// (master plan 4.5, first paragraph): the package, its documentation,
// conversation contracts, capability contracts, source definitions, permission
// model and the adapter's control surface and version.
func triggersOf(in *Input) []Trigger {
	t := []Trigger{
		{Kind: TriggerPackage, Subject: in.Record.ApplicationID, Hash: in.Record.ManifestHash},
		{Kind: TriggerCoreVersion, Subject: "core", Hash: jsonDigest(in.CoreVersion)},
	}
	if pkg := in.Package; pkg != nil {
		t = append(t, Trigger{Kind: TriggerDocumentation, Subject: "documentation/index.yaml", Hash: jsonDigest(pkg.Documentation)})
		for _, id := range pkg.SortedCapabilityIDs() {
			t = append(t, Trigger{Kind: TriggerCapability, Subject: id, Hash: jsonDigest(pkg.Capabilities[id])})
		}
		keys := make([]string, 0, len(pkg.Contracts))
		byKey := map[string]any{}
		for k, c := range pkg.Contracts {
			name := k.Capability + "/" + k.Language
			keys = append(keys, name)
			byKey[name] = c
		}
		sort.Strings(keys)
		for _, k := range keys {
			t = append(t, Trigger{Kind: TriggerContract, Subject: k, Hash: jsonDigest(byKey[k])})
		}
		ids := make([]string, 0, len(pkg.Sources))
		for id := range pkg.Sources {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			t = append(t, Trigger{Kind: TriggerSource, Subject: id, Hash: jsonDigest(pkg.Sources[id])})
		}
		perms := map[string][]string{}
		for _, id := range pkg.SortedCapabilityIDs() {
			var ps []string
			for _, p := range pkg.Capabilities[id].Permissions {
				ps = append(ps, string(p))
			}
			perms[id] = ps
		}
		t = append(t, Trigger{Kind: TriggerPermissionModel, Subject: "permissions", Hash: jsonDigest(struct {
			Roles        any
			Policies     any
			Capabilities map[string][]string
		}{pkg.Manifest.Tenancy.Roles, pkg.Policies, perms})})
		t = append(t, Trigger{Kind: TriggerApplicationVersion, Subject: "application", Hash: jsonDigest(pkg.Manifest.Application.Version)})
	}
	if d := in.Description; d != nil {
		t = append(t, Trigger{Kind: TriggerAdapterSurface, Subject: d.Transport, Hash: jsonDigest(normalisedSurface(*d))})
	}
	sort.SliceStable(t, func(i, j int) bool {
		if t[i].Kind != t[j].Kind {
			return t[i].Kind < t[j].Kind
		}
		return t[i].Subject < t[j].Subject
	})
	return t
}

// normalisedSurface is the adapter's description in a fixed order, so the hash
// does not move when the adapter lists the same operations differently.
func normalisedSurface(d adapters.Description) any {
	type capOps struct {
		ID  string
		Ops []string
	}
	var caps []capOps
	for _, c := range d.Capabilities {
		var ops []string
		for _, o := range c.Operations {
			ops = append(ops, string(o))
		}
		sort.Strings(ops)
		caps = append(caps, capOps{c.CapabilityID, ops})
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].ID < caps[j].ID })
	return struct {
		Transport string
		Versions  []int
		Caps      []capOps
		Unmapped  []string
	}{d.Transport, append([]int(nil), d.ContractVersions...), caps, uniqueSorted(d.Unmapped)}
}

// canonical is the canonical JSON of v (RFC 8785 restricted to strings,
// integers, booleans and null).
func canonical(v any) ([]byte, error) { return trust.CanonicalJSON(v) }

// invalidText reports whether any string reachable from v is not valid UTF-8.
// encoding/json would replace the bad bytes silently, so the signed document
// would say something the check did not; a check that produces such text is
// refused instead.
func invalidText(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return !utf8.ValidString(v.String())
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && invalidText(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if invalidText(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if invalidText(k) || invalidText(v.MapIndex(k)) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() && invalidText(v.Field(i)) {
				return true
			}
		}
	}
	return false
}
