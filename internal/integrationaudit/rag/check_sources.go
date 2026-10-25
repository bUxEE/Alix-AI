package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The IDs of the checks of this package.
const (
	IDSources   = "application.rag_sources"
	IDCitations = "application.rag_citations"
	IDScope     = "application.rag_scope"
	IDSmoke     = "application.rag_smoke"
)

// ProbeRAGSource is the kind of the probe results that say how a declared field of a source
// was verified.
const ProbeRAGSource integrationaudit.ProbeKind = "rag_source"

// ragFields are the fields master plan 4.5 check 3 requires of a source, in the order the
// static check lists them.
var ragFields = []string{"owner", "scope", "include", "exclude", "credential", "refresh", "deletion", "citation"}

// How a field of a source was verified.
const (
	basisLive        = "live"        // something the audit executed or looked at now
	basisDeclaration = "declaration" // a fact the audit can only require to be stated
)

// fieldVerdict is the outcome for one field of one source.
type fieldVerdict struct {
	state  int // fieldPass, fieldFail or fieldUnknown
	basis  string
	reason string // why it failed or could not be looked at
}

const (
	fieldUnknown = iota // not looked at: never a pass
	fieldPass
	fieldFail
)

func sourcesCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDSources, Title: "RAG sources: undeclared and unreachable sources, declared fields verified live",
		Section: integrationaudit.SectionRAGInventory, Stage: integrationaudit.StageRAG, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return runSources(ctx, o, in)
	})
}

// Placeholders for the scope the audit probes a source under. They name no tenant of the
// application; a source bound to a fixed tenant or workspace is probed under its own.
const (
	auditTenant    = "tnt_ragaudit"
	auditWorkspace = "ws_ragaudit"
)

// runSources is the live half of master plan 4.5 check 3. For every source the package
// declares it builds the definition the core would register, asks the deployment's own
// provider whether the root can be reached (Probe only: the audit never walks a real
// root and never reads a document of a real source), looks at each of the eight declared
// fields against what the deployment shows now, and records what it could not look at as
// unverified. Then it compares the declaration with the sources the deployment holds for
// the application, and reports the ones the package does not declare.
//
// Where the deployment does not show something (no provider for a source type, no listing
// of registrations, mounts the audit does not know), the answer is unverified, never a
// pass and never an absence.
func runSources(ctx context.Context, o Options, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
	out := &integrationaudit.Outcome{}
	if in.Package == nil {
		noPackage(out, integrationaudit.SectionRAGInventory, "rag_sources:package")
		return out, nil
	}
	pkg := in.Package
	appID := string(pkg.Manifest.Application.ID)

	dep, depErr := o.deployment(ctx, in)
	declared := sortedKeys(pkg.Sources)
	live := 0
	for _, id := range declared {
		if ctx.Err() != nil {
			out.Unverify(sourceUnverified(id, "the audit was cancelled before the source was looked at"))
			continue
		}
		sv := &sourceVerifier{ctx: ctx, in: in, dep: dep, depErr: depErr, id: id, decl: pkg.Sources[id], appID: appID, out: out}
		if sv.run() {
			live++
		}
	}
	out.Coverage = append(out.Coverage, integrationaudit.CoverageItem{Name: "rag_sources_live", Covered: live, Total: len(declared)})

	registrations := -1 // not listed
	switch {
	case depErr != nil:
		out.Unverify(integrationaudit.Unverified{
			Subject: "rag_sources:registrations", Section: integrationaudit.SectionRAGInventory, Target: integrationaudit.TargetApplication,
			Reason: "the deployment's RAG side could not be reached (" + codeString(depErr) + "), so undeclared sources could not be looked for",
		})
	case dep == nil || dep.Registrations == nil:
		out.Unverify(integrationaudit.Unverified{
			Subject: "rag_sources:registrations", Section: integrationaudit.SectionRAGInventory, Target: integrationaudit.TargetApplication,
			Reason: "the audit has no way to list the sources the deployment has registered for the application, so undeclared sources could not be looked for",
		})
	default:
		regs, err := dep.Registrations(ctx, appID)
		if err != nil {
			out.Unverify(integrationaudit.Unverified{
				Subject: "rag_sources:registrations", Section: integrationaudit.SectionRAGInventory, Target: integrationaudit.TargetApplication,
				Reason: "the registrations of the application could not be listed (" + codeString(err) + "), so undeclared sources could not be looked for",
			})
			break
		}
		registrations = len(regs)
		compareRegistrations(out, pkg.Sources, regs)
	}

	out.Extension = map[string]any{"declared_sources": len(declared), "sources_verified_live": live}
	if registrations >= 0 {
		out.Extension["registrations_listed"] = registrations
	}
	return out, nil
}

// compareRegistrations reports the registrations the package does not declare, and those
// whose binding the declaration does not allow. A declaration that several scopes have
// registered is reported once.
func compareRegistrations(out *integrationaudit.Outcome, declared map[string]ap.RAGSource, regs []Registration) {
	undeclared := map[string]int{}
	mismatched := map[string]int{}
	for _, r := range regs {
		decl, ok := declared[r.PackageSourceID]
		if !ok {
			undeclared[r.PackageSourceID]++
			continue
		}
		if bindingConflicts(decl.Scope.Tenant, r.BoundTenantID) || bindingConflicts(decl.Scope.Workspace, r.BoundWorkspaceID) {
			mismatched[r.PackageSourceID]++
		}
	}
	for _, id := range sortedKeys(undeclared) {
		out.Add(newFinding(fdUndeclared, pathSources,
			"the deployment holds %d registration(s) of source %s, which the package does not declare", undeclared[id], quote(id)))
		out.Probes = append(out.Probes, integrationaudit.ProbeResult{
			Kind: integrationaudit.ProbeNegativeScope, Subject: "registered source " + quote(id) + " against the package's declarations",
			Expected: "every registered source is declared", Observed: "not declared", Passed: false,
		})
	}
	for _, id := range sortedKeys(mismatched) {
		out.Add(newFinding(fdBindingMismatch, pathSources,
			"%d registration(s) of source %s are bound to a tenant or workspace its declaration does not allow", mismatched[id], quote(id)))
	}
	if len(undeclared) == 0 && len(mismatched) == 0 {
		out.Probes = append(out.Probes, integrationaudit.ProbeResult{
			Kind: integrationaudit.ProbePositiveScope, Subject: "registered sources against the package's declarations",
			Expected: "every registered source is declared, bound as declared", Observed: fmt.Sprintf("%d registration(s), all declared", len(regs)), Passed: true,
		})
	}
}

// bindingConflicts reports whether the tenant or workspace a registration was bound to
// contradicts how the declaration binds that level.
func bindingConflicts(b ap.RAGSourceScopeBinding, bound string) bool {
	switch b.Mode {
	case ap.RAGSourceScopeBindingModeShared:
		return bound != ""
	case ap.RAGSourceScopeBindingModeFixed:
		return b.ID == nil || bound != string(*b.ID)
	case ap.RAGSourceScopeBindingModeRequest:
		return bound == ""
	}
	return true
}

func sourceUnverified(id, reason string) integrationaudit.Unverified {
	return integrationaudit.Unverified{
		Subject: "rag_source:" + id, Section: integrationaudit.SectionRAGInventory, Target: integrationaudit.TargetApplication, Reason: reason,
	}
}

// sourceVerifier verifies one declared source.
type sourceVerifier struct {
	ctx    context.Context
	in     *integrationaudit.Input
	dep    *Deployment
	depErr error
	id     string
	decl   ap.RAGSource
	appID  string
	out    *integrationaudit.Outcome

	fields  map[string]fieldVerdict
	reasons []string // why the source could not be fully looked at
}

func (v *sourceVerifier) set(field string, state int, basis, reason string) {
	v.fields[field] = fieldVerdict{state: state, basis: basis, reason: reason}
	if state != fieldPass && reason != "" {
		v.reasons = append(v.reasons, reason)
	}
}

// run verifies the source and reports whether it was verified live: reached, and every field
// held. It adds its findings, probes and unverified item to the outcome, and the source's
// result when it is live.
func (v *sourceVerifier) run() bool {
	v.fields = map[string]fieldVerdict{}
	decl := v.decl

	// owner: an organisational fact. The audit can require it to be named, nothing more.
	if strings.TrimSpace(string(decl.Owner.Name)) != "" {
		v.set("owner", fieldPass, basisDeclaration, "")
	} else {
		v.set("owner", fieldFail, basisDeclaration, "the source names no owner")
	}

	def, built, scope := v.definition()
	if built {
		v.scopeField(def, scope)
	} else {
		v.set("scope", fieldUnknown, basisLive, "")
	}
	v.rulesFields()
	v.credentialField()
	v.refreshAndDeletionFields()
	v.citationField()

	reached := false
	if built {
		reached = v.reach(def, scope)
	}

	// The probe results: what was verified or found wanting, by field. A field that could
	// not be looked at is not a probe; it is unverified.
	for _, f := range ragFields {
		fv := v.fields[f]
		switch fv.state {
		case fieldPass:
			v.out.Probes = append(v.out.Probes, integrationaudit.ProbeResult{
				Kind: ProbeRAGSource, Subject: v.id + ": " + f, Expected: "the declared " + f + " holds",
				Observed: "holds (" + fv.basis + ")", Passed: true,
			})
		case fieldFail:
			v.out.Probes = append(v.out.Probes, integrationaudit.ProbeResult{
				Kind: ProbeRAGSource, Subject: v.id + ": " + f, Expected: "the declared " + f + " holds",
				Observed: "does not hold", Passed: false,
			})
		}
	}

	ok := reached
	for _, f := range ragFields {
		if v.fields[f].state != fieldPass {
			ok = false
		}
	}
	if !ok {
		if len(v.reasons) == 0 {
			v.reasons = append(v.reasons, "a declared field did not hold, or the source was not reached")
		}
		v.out.Unverify(sourceUnverified(v.id, strings.Join(uniqueStrings(v.reasons), "; ")))
		return false
	}
	res := integrationaudit.SourceResult{
		ID: v.id, Type: string(decl.Type), Owner: string(decl.Owner.Name),
		Scope: fmt.Sprintf("application=%s tenant=%s workspace=%s",
			string(decl.Scope.ApplicationID), bindingString(decl.Scope.Tenant), bindingString(decl.Scope.Workspace)),
		Declared: append([]string{}, ragFields...), Missing: []string{}, Verified: "live",
	}
	v.out.Sources = append(v.out.Sources, res)
	return true
}

func bindingString(b ap.RAGSourceScopeBinding) string {
	if b.Mode == ap.RAGSourceScopeBindingModeFixed && b.ID != nil {
		return "fixed:" + string(*b.ID)
	}
	return string(b.Mode)
}

// definition builds the definition the core would register for the declaration, bound the way
// the declaration says, and the scope the probes act under.
func (v *sourceVerifier) definition() (source.Definition, bool, contracts.Scope) {
	decl := v.decl
	b := source.Binding{SourceID: ids.New("src")}
	if decl.Scope.Tenant.Mode == ap.RAGSourceScopeBindingModeRequest {
		b.TenantID = auditTenant
	}
	if decl.Scope.Workspace.Mode == ap.RAGSourceScopeBindingModeRequest {
		b.WorkspaceID = auditWorkspace
	}
	b.Mounts = v.mounts()
	if m, ok := decl.Root.Value.(ap.RAGSourceRootMount); ok {
		if v.dep != nil && v.dep.Mounts != nil {
			if _, registered := v.dep.Mounts(string(m.MountID)); !registered {
				v.out.Add(newFinding(fdUnreachable, pathSources,
					"source %s is declared on mount %s, which this deployment has not registered", quote(v.id), quote(string(m.MountID))))
				v.reasons = append(v.reasons, "the mount the source is declared on is not registered with this deployment")
				return source.Definition{}, false, contracts.Scope{}
			}
		}
	}
	def, err := source.FromDeclaration(decl, b)
	if err != nil {
		v.out.Add(newFinding(fdDefinitionRefused, pathSources,
			"the core refuses to build source %s from its declaration (%s)", quote(v.id), codeString(err)))
		v.reasons = append(v.reasons, "the core refuses the declaration ("+codeString(err)+")")
		return source.Definition{}, false, contracts.Scope{}
	}
	tenant, workspace := auditTenant, auditWorkspace
	if def.Tenant.ID != "" {
		tenant = def.Tenant.ID
	}
	if def.Workspace.ID != "" {
		workspace = def.Workspace.ID
	}
	sc := contracts.Scope{
		DeploymentID: integrationaudit.DefaultDeploymentID, ApplicationID: v.appID, TenantID: tenant, WorkspaceID: workspace,
		PrincipalID: "usr_ragaudit", Roles: []string{"auditor"}, CapabilityScopes: []string{"rag.probe"}, SessionID: "ses_ragaudit",
	}
	return def, true, sc
}

// mounts returns the resolver the definition is built with. A deployment that does not tell
// the audit its mounts gets a placeholder, only so that the rest of the declaration can still
// be looked at: reaching the root is then unverified.
func (v *sourceVerifier) mounts() source.MountResolver {
	if v.dep != nil && v.dep.Mounts != nil {
		return v.dep.Mounts
	}
	return func(string) (string, bool) { return "/ragaudit/unknown-mount", true }
}

// scopeField runs the core's check of the declared scope against the scopes the declaration
// must admit and refuse.
func (v *sourceVerifier) scopeField(def source.Definition, own contracts.Scope) {
	decl := v.decl
	type attempt struct {
		what  string
		scope contracts.Scope
		admit bool
	}
	other := own
	other.ApplicationID = own.ApplicationID + "-other"
	attempts := []attempt{
		{"the scope the declaration binds the source to", own, true},
		{"another application", other, false},
	}
	if decl.Scope.Tenant.Mode != ap.RAGSourceScopeBindingModeShared {
		t := own
		t.TenantID = own.TenantID + "-other"
		attempts = append(attempts, attempt{"another tenant", t, false})
	}
	if decl.Scope.Workspace.Mode != ap.RAGSourceScopeBindingModeShared {
		w := own
		w.WorkspaceID = own.WorkspaceID + "-other"
		attempts = append(attempts, attempt{"another workspace", w, false})
	}
	good := true
	for _, a := range attempts {
		err := def.CheckScope(a.scope)
		passed := (err == nil) == a.admit
		kind := integrationaudit.ProbeNegativeScope
		expected, observed := "refused", "refused"
		if a.admit {
			kind, expected = integrationaudit.ProbePositiveScope, "admitted"
		}
		if err == nil {
			observed = "admitted"
		}
		v.out.Probes = append(v.out.Probes, integrationaudit.ProbeResult{
			Kind: kind, Subject: v.id + ": " + a.what, Expected: expected, Observed: observed, Passed: passed,
		})
		if !passed {
			good = false
		}
	}
	if !good {
		v.out.Add(newFinding(fdScopeNotEnforced, pathSources,
			"the core's check of the declared scope of source %s did not admit or refuse as its declaration says", quote(v.id)))
		v.set("scope", fieldFail, basisLive, "")
		return
	}
	v.set("scope", fieldPass, basisLive, "")
}

// rulesFields verifies the include and exclude rules: they compile in the core's own matcher,
// and together they admit something. For a source shipped in the package, that is a file the
// signed package holds below the declared path; for any other root the audit never walks the
// root, and asks the matcher about names the rules could match.
func (v *sourceVerifier) rulesFields() {
	decl := v.decl
	include, exclude := globStrings(decl.Include), globStrings(decl.Exclude)
	m, err := source.NewMatcher(include, exclude, false)
	if err != nil {
		v.out.Add(newFinding(fdDefinitionRefused, pathSources,
			"the core cannot compile the include and exclude rules of source %s (%s)", quote(v.id), codeString(err)))
		v.set("include", fieldFail, basisLive, "")
		v.set("exclude", fieldFail, basisLive, "")
		return
	}
	v.set("exclude", fieldPass, basisLive, "")

	if p, ok := decl.Root.Value.(ap.RAGSourceRootPackage); ok {
		if v.in.Package == nil {
			v.set("include", fieldUnknown, basisLive, "the package's files were not available")
			return
		}
		prefix := strings.TrimSuffix(p.Path, "/") + "/"
		n := 0
		for _, f := range v.in.Package.Files {
			if rel, ok := strings.CutPrefix(f, prefix); ok && m.Eligible(rel) {
				n++
			}
		}
		if n == 0 {
			v.out.Add(newFinding(fdRulesEmpty, pathSources,
				"the include and exclude rules of source %s admit none of the files the package holds below its declared path", quote(v.id)))
			v.set("include", fieldFail, basisLive, "")
			return
		}
		v.set("include", fieldPass, basisLive, "")
		return
	}

	tried, admitted := 0, 0
	for _, pat := range include {
		for _, w := range witnesses(pat) {
			if !m.Included(w) {
				continue // not a name the pattern admits after all: no evidence either way
			}
			tried++
			if m.Eligible(w) {
				admitted++
			}
		}
	}
	switch {
	case tried == 0:
		v.set("include", fieldUnknown, basisLive, "the audit could not build a name the include rules admit, to test them against the exclude rules")
	case admitted == 0:
		v.out.Add(newFinding(fdRulesEmpty, pathSources,
			"every name the include rules of source %s admit is removed by its exclude rules", quote(v.id)))
		v.set("include", fieldFail, basisLive, "")
	default:
		v.set("include", fieldPass, basisLive, "")
	}
}

func globStrings(in []ap.GlobPattern) []string {
	out := make([]string, len(in))
	for i, g := range in {
		out[i] = string(g)
	}
	return out
}

// credentialField: a source with no credential reference needs none; one that names a
// reference must find it among the references the application exposes for inspection. The
// audit reads references, never values.
func (v *sourceVerifier) credentialField() {
	ref := v.decl.CredentialRef
	if ref == nil {
		v.set("credential", fieldPass, basisDeclaration, "")
		return
	}
	in := v.in
	if in.Inspection == nil {
		v.set("credential", fieldUnknown, basisLive, "the application's inspection was not available, so the credential reference could not be looked for")
		return
	}
	rep, ok := in.Inspection.Report(adapters.AspectCredentials)
	if !ok || rep.State != adapters.AspectReported {
		v.set("credential", fieldUnknown, basisLive, "the application does not expose its credential references for inspection")
		return
	}
	for _, f := range rep.Facts {
		if string(f.Field) == string(*ref) || textFact(f) == string(*ref) {
			v.set("credential", fieldPass, basisLive, "")
			return
		}
	}
	v.out.Add(newFinding(fdCredentialUnknown, pathSources,
		"source %s names a credential reference that the application does not report among its credential references", quote(v.id)))
	v.set("credential", fieldFail, basisLive, "")
}

func textFact(f actions.Fact) string {
	if t, ok := f.Value.Value.(actions.TextValue); ok {
		return t.Value
	}
	return ""
}

// refreshAndDeletionFields: the refresh policy and the deletion behaviour are declarations of
// what the core does, and the core is what exercises them: the citation check replaces and
// deletes a document of a hosted source and requires retrieval and citations to follow. Here
// the audit requires them to be stated in a form the core accepts.
func (v *sourceVerifier) refreshAndDeletionFields() {
	if v.decl.Sync.Method.Valid() {
		v.set("refresh", fieldPass, basisDeclaration, "")
	} else {
		v.set("refresh", fieldFail, basisDeclaration, "the refresh method is not one the core accepts")
	}
	if v.decl.Deletion.OnSourceRemoved.Valid() && v.decl.Deletion.PurgeAfterHours >= 0 {
		v.set("deletion", fieldPass, basisDeclaration, "")
	} else {
		v.set("deletion", fieldFail, basisDeclaration, "the deletion behaviour is not one the core accepts")
	}
}

// citationField: only the core's own preview resolver is something the audit can exercise.
// The audit never fetches a URL, and cannot call an arbitrary operation of the application.
func (v *sourceVerifier) citationField() {
	switch v.decl.Citation.Value.(type) {
	case ap.RAGSourceCitationAlixAIPreview:
		v.set("citation", fieldPass, basisLive, "")
	case ap.RAGSourceCitationAdapterOperation:
		v.set("citation", fieldUnknown, basisLive, "the citation is resolved by an operation of the application, which the audit cannot call")
	case ap.RAGSourceCitationURLTemplate:
		v.set("citation", fieldUnknown, basisLive, "the citation is a URL, and the audit never fetches one")
	default:
		v.set("citation", fieldFail, basisDeclaration, "the source declares no citation resolver")
	}
}

// reach asks the deployment's provider whether the source's root can be reached. It reports
// whether it was. Everything it cannot establish is recorded as a reason.
func (v *sourceVerifier) reach(def source.Definition, scope contracts.Scope) bool {
	if v.depErr != nil {
		v.reasons = append(v.reasons, "the deployment's RAG side could not be reached ("+codeString(v.depErr)+")")
		return false
	}
	if v.dep == nil {
		v.reasons = append(v.reasons, "no deployment was available to reach the source from")
		return false
	}
	prov := v.dep.providerFor(v.decl.Type)
	if prov == nil {
		v.reasons = append(v.reasons, "no source provider in this deployment serves sources of type "+string(v.decl.Type))
		return false
	}
	if _, isMount := v.decl.Root.Value.(ap.RAGSourceRootMount); isMount && v.dep.Mounts == nil {
		v.reasons = append(v.reasons, "the deployment's operator mounts are not known to the audit, so the mount root could not be reached")
		return false
	}
	h, err := prov.Probe(v.ctx, scope, def)
	switch {
	case err != nil && (v.ctx.Err() != nil || contracts.HasCode(err, contracts.ErrTimeout) || contracts.HasCode(err, contracts.ErrUnavailable)):
		v.reasons = append(v.reasons, "the provider could not be asked ("+codeString(err)+")")
		return false
	case err != nil:
		v.out.Add(newFinding(fdDefinitionRefused, pathSources,
			"the source provider refuses the root of source %s (%s)", quote(v.id), codeString(err)))
		v.reasons = append(v.reasons, "the provider refuses the root ("+codeString(err)+")")
		return false
	case !h.Reachable():
		v.out.Add(newFinding(fdUnreachable, pathSources,
			"the root of source %s cannot be reached from this deployment", quote(v.id)))
		v.reasons = append(v.reasons, "the root cannot be reached")
		return false
	}
	return true
}
