package integrationaudit

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Builtins returns the checks this package implements: 1 authenticity, 3 RAG
// inventory (declared fields), 4 documentation completeness, 5 conversation
// contracts and 7 control surface (declared against discovered), plus the
// supplementary package, runtime and informational checks. Sections 2, 6, 8, 9
// and 10 are not here: they need the application to answer probes, and they
// register themselves from internal/integrationaudit/probes (M1-18) and
// internal/integrationaudit/rag (M2-12). Until they do, the report says those
// sections were not run.
func Builtins() []Check {
	return []Check{
		NewCheck(Spec{ID: "package.authenticity", Title: "Package authenticity", Section: SectionAuthenticity,
			Stage: StageSignature, Target: TargetPackage, WithoutPackage: true}, runAuthenticity),
		NewCheck(Spec{ID: "package.documentation", Title: "Documentation completeness", Section: SectionDocumentation,
			Stage: StageDocumentation, Target: TargetPackage, WithoutPackage: true}, runDocumentation),
		NewCheck(Spec{ID: "package.contracts", Title: "Conversation contracts", Section: SectionContracts,
			Stage: StageDocumentation, Target: TargetPackage, WithoutPackage: true}, runContracts),
		NewCheck(Spec{ID: "package.inventory", Title: "Capability and adapter inventory", Section: SectionControlSurface,
			Stage: StageInventory, Target: TargetPackage, WithoutPackage: true}, runInventory),
		NewCheck(Spec{ID: "package.policy", Title: "Policy and package tests", Section: sectionSupplementary,
			Stage: StageInventory, Target: TargetPackage, WithoutPackage: true}, runPolicy),
		NewCheck(Spec{ID: "package.rag_inventory", Title: "RAG source inventory", Section: SectionRAGInventory,
			Stage: StageRAG, Target: TargetPackage, WithoutPackage: true}, runRAGInventory),
		NewCheck(Spec{ID: "package.external_inventory", Title: "What could leave the deployment", Section: sectionSupplementary,
			Stage: StageRAG, Target: TargetPackage}, runExternalInventory),
		NewCheck(Spec{ID: "application.control_surface", Title: "Control surface: declared against discovered", Section: SectionControlSurface,
			Stage: StageInventory, Target: TargetApplication}, runControlSurface),
		NewCheck(Spec{ID: "runtime.inspection", Title: "Runtime and system inspection", Section: sectionSupplementary,
			Stage: StageInventory, Target: TargetRuntime}, runInspection),
	}
}

// loaderFor returns the loader findings that belong to a section.
func loaderFor(in *Input, section int) []contracts.Finding {
	var out []contracts.Finding
	for _, f := range in.LoadFindings {
		if loaderSection(f.CheckID) == section {
			out = append(out, f)
		}
	}
	return out
}

// noPackage records that a check could not look at the typed package.
func noPackage(out *Outcome, section int, what string) {
	out.Unverify(Unverified{
		Subject: what, Section: section, Target: TargetPackage,
		Reason: "the package could not be loaded and verified, so it was not examined",
	})
}

// operations lists the operations a capability declares, in canonical order.
func declaredOperations(c ap.Capability) []string {
	var ops []string
	add := func(name string, b *ap.CapabilityOperationBinding) {
		if b != nil {
			ops = append(ops, name)
		}
	}
	add("read", c.Operations.Read)
	add("validate", c.Operations.Validate)
	add("prepare", c.Operations.Prepare)
	add("commit", c.Operations.Commit)
	add("status", c.Operations.Status)
	return ops
}

// bindingOf returns the binding of an operation of a capability.
func bindingOf(c ap.Capability, op string) *ap.CapabilityOperationBinding {
	switch op {
	case "read":
		return c.Operations.Read
	case "validate":
		return c.Operations.Validate
	case "prepare":
		return c.Operations.Prepare
	case "commit":
		return c.Operations.Commit
	case "status":
		return c.Operations.Status
	}
	return nil
}

// callName returns the adapter call a binding maps to: the MCP tool, OpenAPI
// operationId or native method.
func callName(b *ap.CapabilityOperationBinding) string {
	switch {
	case b == nil:
		return ""
	case b.MCPTool != nil:
		return string(*b.MCPTool)
	case b.OpenAPIOperationID != nil:
		return string(*b.OpenAPIOperationID)
	case b.NativeMethod != nil:
		return string(*b.NativeMethod)
	}
	return ""
}

// runAuthenticity is check 1: the package is signed, hash-verified and
// version-compatible. The loader performed the verification from the stored
// bytes just before the audit; this check reports what it found.
func runAuthenticity(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	out.Add(loaderFor(in, SectionAuthenticity)...)
	if in.Package == nil {
		// The loader's critical findings say why; make sure the report is
		// blocked even if it found nothing it could name in this section.
		if !contracts.HasCritical(in.LoadFindings) {
			out.Add(newFinding(fdPackageRefused, "", "", ""))
		}
		return out, nil
	}
	if in.Authenticity != apppkg.AuthVerified || in.Signer == nil {
		out.Add(newFinding(fdPackageRefused, "", "",
			"the signature of the installed package was not verified against a trust anchor (%s)", quote(string(in.Authenticity))))
		return out, nil
	}
	out.Extension = map[string]any{
		"authenticity":         string(in.Authenticity),
		"signer_key_id":        in.Signer.KeyID,
		"signature_signed_at":  in.Signer.SignedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"signature_expires_at": in.Signer.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"),
		"files":                len(in.Package.Files),
	}
	return out, nil
}

// facet identifies one piece of documentation a capability needs.
type facet struct{ key, label string }

func facetKey(capability, kind, subject string) string {
	return capability + "|" + kind + "|" + subject
}

// requiredFacets lists the documentation a capability needs in the default
// language (docs/integration/application-package.md section 8): every field,
// validation rule, permission, failure state, the risk level, the confirmation
// rule and every user-visible outcome.
func requiredFacets(pkg *apppkg.Package, c ap.Capability, lang string) []facet {
	id := string(c.ID)
	var out []facet
	for _, f := range c.Fields {
		out = append(out, facet{facetKey(id, "field", string(f.Name)), "field " + string(f.Name)})
	}
	for _, v := range c.Validation {
		out = append(out, facet{facetKey(id, "validation_rule", string(v.ID)), "validation rule " + string(v.ID)})
	}
	for _, p := range c.Permissions {
		out = append(out, facet{facetKey(id, "permission", string(p)), "permission " + string(p)})
	}
	for _, f := range c.FailureStates {
		out = append(out, facet{facetKey(id, "failure_state", string(f.Code)), "failure state " + string(f.Code)})
	}
	out = append(out,
		facet{facetKey(id, "risk_level", ""), "risk level"},
		facet{facetKey(id, "confirmation_rule", ""), "confirmation rule"})
	if c.Audience == ap.CapabilityAudienceUser {
		if ct, ok := pkg.ContractFor(id, lang); ok {
			for _, s := range ct.Success {
				out = append(out, facet{facetKey(id, "user_visible_outcome", string(s.Outcome)), "outcome " + string(s.Outcome)})
			}
		}
	}
	return out
}

func documented(pkg *apppkg.Package, lang string) map[string]bool {
	have := map[string]bool{}
	for _, e := range pkg.Documentation.Entries {
		if string(e.Language) != lang {
			continue
		}
		id := string(e.CapabilityID)
		subject := ""
		switch e.Facet {
		case ap.DocumentationIndexEntryFacetField:
			if e.Field != nil {
				subject = string(*e.Field)
			}
		case ap.DocumentationIndexEntryFacetValidationRule:
			if e.RuleID != nil {
				subject = string(*e.RuleID)
			}
		case ap.DocumentationIndexEntryFacetPermission:
			if e.Permission != nil {
				subject = string(*e.Permission)
			}
		case ap.DocumentationIndexEntryFacetFailureState:
			if e.ErrorCode != nil {
				subject = string(*e.ErrorCode)
			}
		case ap.DocumentationIndexEntryFacetUserVisibleOutcome:
			if e.Outcome != nil {
				subject = string(*e.Outcome)
			}
		}
		have[facetKey(id, string(e.Facet), subject)] = true
	}
	return have
}

// runDocumentation is check 4: the signed documentation describes each
// supported capability, field, validation rule, permission, failure state, risk
// level, confirmation rule and user-visible outcome. Missing documentation is a
// failed capability, not an instruction for the model to guess.
//
// The loader checks this when it loads the package; the audit recomputes it
// from the typed package so that a weakened loader cannot let a gap through.
func runDocumentation(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	out.Add(loaderFor(in, SectionDocumentation)...)
	if in.Package == nil {
		noPackage(out, SectionDocumentation, "documentation")
		return out, nil
	}
	pkg := in.Package
	lang := string(pkg.Manifest.Languages.Default)
	have := documented(pkg, lang)
	total, covered := 0, 0
	for _, id := range pkg.SortedCapabilityIDs() {
		c := pkg.Capabilities[id]
		out.Covered = append(out.Covered, id)
		var missing []string
		for _, f := range requiredFacets(pkg, c, lang) {
			total++
			if have[f.key] {
				covered++
			} else {
				missing = append(missing, f.label)
			}
		}
		if len(missing) > 0 {
			out.Add(newFinding(fdDocsIncomplete, id, "documentation/index.yaml",
				"capability %s has no %s documentation for: %s", quote(id), quote(lang), strings.Join(missing, ", ")))
		}
	}
	out.Coverage = append(out.Coverage, CoverageItem{Name: "documentation_facets", Covered: covered, Total: total})
	return out, nil
}

// runContracts is check 5: every ordinary user capability has a versioned
// conversation contract in every supported language covering its prompts,
// clarifications, corrections, cancellations, confirmations, success and
// failure responses and permitted next actions.
func runContracts(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	out.Add(loaderFor(in, SectionContracts)...)
	if in.Package == nil {
		noPackage(out, SectionContracts, "conversation_contracts")
		return out, nil
	}
	pkg := in.Package
	var langs []string
	for _, l := range pkg.Manifest.Languages.Supported {
		langs = append(langs, string(l))
	}
	sort.Strings(langs)

	total, covered := 0, 0
	for _, id := range pkg.SortedCapabilityIDs() {
		c := pkg.Capabilities[id]
		out.Covered = append(out.Covered, id)
		if c.Audience != ap.CapabilityAudienceUser {
			continue // an admin capability is never offered in conversation
		}
		for _, lang := range langs {
			total++
			ct, ok := pkg.ContractFor(id, lang)
			if !ok {
				out.Add(newFinding(fdContractsIncomplete, id, "conversation-contracts/",
					"capability %s has no conversation contract in %s", quote(id), quote(lang)))
				continue
			}
			var gaps []string
			prompts := map[string]bool{}
			for _, p := range ct.FieldPrompts {
				prompts[string(p.Field)] = true
			}
			for _, f := range c.Fields {
				if f.Required && !prompts[string(f.Name)] {
					gaps = append(gaps, "a prompt for required field "+string(f.Name))
				}
			}
			responses := map[string]bool{}
			for _, r := range ct.Failures {
				responses[string(r.Code)] = true
			}
			for _, f := range c.FailureStates {
				if !responses[string(f.Code)] {
					gaps = append(gaps, "a response for failure "+string(f.Code))
				}
			}
			if c.Confirmation.Mode != ap.CapabilityConfirmationModeNone && ct.Confirmations == nil {
				gaps = append(gaps, "the confirmation wording")
			}
			if len(ct.Success) == 0 {
				gaps = append(gaps, "a success response")
			}
			if len(gaps) > 0 {
				out.Add(newFinding(fdContractsIncomplete, id, "conversation-contracts/",
					"the %s conversation contract of capability %s lacks: %s", quote(lang), quote(id), strings.Join(gaps, ", ")))
				continue
			}
			covered++
		}
	}
	out.Coverage = append(out.Coverage, CoverageItem{Name: "conversation_contracts", Covered: covered, Total: total})
	return out, nil
}

// runInventory is the package half of check 7: the declared capability, schema
// and adapter inventory, as the loader cross-checked it.
func runInventory(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	out.Add(loaderFor(in, SectionControlSurface)...)
	if in.Package == nil {
		noPackage(out, SectionControlSurface, "inventory")
		return out, nil
	}
	out.Covered = append(out.Covered, in.Package.SortedCapabilityIDs()...)
	return out, nil
}

// runPolicy reports the policy and package-test findings of the loader and the
// audit's reading of the permission model: capabilities nobody can use, admin
// capabilities granted to roles, roles that can do nothing.
func runPolicy(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	out.Add(loaderFor(in, sectionSupplementary)...)
	for _, f := range in.LoadFindings {
		switch f.CheckID {
		case "policy.conflict", "policy.required_field_hidden", "policy.rule_hides_required_field", "policy.override_not_higher":
			out.Permissions.Contradictory = append(out.Permissions.Contradictory, f.CheckID+": "+f.Message)
		}
	}
	if in.Package == nil {
		noPackage(out, sectionSupplementary, "permissions")
		return out, nil
	}
	pkg := in.Package
	grants := map[string]map[string]bool{} // role -> capabilities
	granted := map[string]bool{}           // capability granted to some role
	for _, hp := range pkg.Policies {
		for _, g := range hp.RoleGrants {
			if grants[g.Role] == nil {
				grants[g.Role] = map[string]bool{}
			}
			for _, c := range g.Capabilities {
				grants[g.Role][c] = true
				granted[c] = true
			}
		}
	}
	for _, id := range pkg.SortedCapabilityIDs() {
		c := pkg.Capabilities[id]
		if c.Audience == ap.CapabilityAudienceUser && !granted[id] {
			out.Add(newFinding(fdPermMissing, id, "policies/", "capability %s is granted to no role", quote(id)))
			out.Permissions.Missing = append(out.Permissions.Missing, "capability "+id+" is granted to no role")
		}
	}
	roles := make([]string, 0, len(grants))
	for r := range grants {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		caps := make([]string, 0, len(grants[r]))
		for c := range grants[r] {
			caps = append(caps, c)
		}
		sort.Strings(caps)
		for _, id := range caps {
			if c, ok := pkg.Capabilities[id]; ok && c.Audience == ap.CapabilityAudienceAdmin {
				out.Add(newFinding(fdPermAdmin, id, "policies/", "role %s is granted admin capability %s", quote(r), quote(id)))
				out.Permissions.Excessive = append(out.Permissions.Excessive, "role "+r+" is granted admin capability "+id)
			}
		}
	}
	declared := make([]string, 0, len(pkg.Manifest.Tenancy.Roles))
	for r := range pkg.Manifest.Tenancy.Roles {
		declared = append(declared, r)
	}
	sort.Strings(declared)
	for _, r := range declared {
		if len(grants[r]) == 0 {
			out.Add(newFinding(fdPermRoleEmpty, "", "policies/", "role %s is granted no capability", quote(r)))
			out.Permissions.Missing = append(out.Permissions.Missing, "role "+r+" is granted no capability")
		}
	}
	return out, nil
}

// ragFields are the fields master plan 4.5 check 3 requires of a source.
var ragFields = []string{"owner", "scope", "include", "exclude", "credential", "refresh", "deletion", "citation"}

// runRAGInventory is the declared half of check 3: every source has an
// explicit owner, scope, include and exclude rules, credential reference,
// refresh policy, deletion behaviour and citation resolver. Detecting
// undeclared or unreachable sources needs a live probe (M2-12); until one is
// registered the audit says so instead of claiming the sources were verified.
func runRAGInventory(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	out.Add(loaderFor(in, SectionRAGInventory)...)
	if in.Package == nil {
		noPackage(out, SectionRAGInventory, "rag_sources")
		return out, nil
	}
	pkg := in.Package
	ids := make([]string, 0, len(pkg.Sources))
	for id := range pkg.Sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	total, covered := 0, 0
	for _, id := range ids {
		s := pkg.Sources[id]
		res := SourceResult{ID: id, Type: string(s.Type), Owner: string(s.Owner.Name), Verified: "declaration"}
		res.Scope = fmt.Sprintf("application=%s tenant=%s workspace=%s",
			string(s.Scope.ApplicationID), bindingString(s.Scope.Tenant), bindingString(s.Scope.Workspace))
		have := map[string]bool{
			"owner":      strings.TrimSpace(string(s.Owner.Name)) != "",
			"scope":      s.Scope.ApplicationID != "" && s.Scope.Tenant.Mode.Valid() && s.Scope.Workspace.Mode.Valid(),
			"include":    len(s.Include) > 0,
			"exclude":    true, // required by the schema; an empty list is a valid rule
			"credential": true, // required by the schema; null means none is needed
			"refresh":    s.Sync.Method.Valid(),
			"deletion":   s.Deletion.OnSourceRemoved.Valid(),
			"citation":   s.Citation.Value != nil,
		}
		var missing []string
		for _, f := range ragFields {
			total++
			if have[f] {
				covered++
				res.Declared = append(res.Declared, f)
			} else {
				missing = append(missing, f)
				res.Missing = append(res.Missing, f)
			}
		}
		if len(missing) > 0 {
			out.Add(newFinding(fdRAGFieldMissing, "", "rag-sources/",
				"RAG source %s does not declare: %s", quote(id), strings.Join(missing, ", ")))
		}
		if _, isAPI := s.Root.Value.(ap.RAGSourceRootAPI); isAPI && s.CredentialRef == nil {
			out.Add(newFinding(fdRAGNoCredential, "", "rag-sources/",
				"RAG source %s reads from an API and declares no credential reference", quote(id)))
		}
		out.Sources = append(out.Sources, res)
	}
	out.Coverage = append(out.Coverage, CoverageItem{Name: "rag_source_fields", Covered: covered, Total: total})
	if !in.HasOtherCheck(SectionRAGInventory, "package.rag_inventory") {
		// Nothing registered can reach a source or look for undeclared ones.
		out.Unverify(Unverified{
			Subject: "rag_sources:live", Section: SectionRAGInventory, Target: TargetApplication,
			Reason: "undeclared and unreachable sources, and every declared field, are verified by live probes, and none is registered for this audit",
		})
	}
	return out, nil
}

func bindingString(b ap.RAGSourceScopeBinding) string {
	if b.Mode == ap.RAGSourceScopeBindingModeFixed && b.ID != nil {
		return "fixed:" + string(*b.ID)
	}
	return string(b.Mode)
}
