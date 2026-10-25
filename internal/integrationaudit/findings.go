package integrationaudit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// findingDef is one kind of finding the audit itself raises (the loader's own
// findings keep their IDs and wording and pass through unchanged). Severity and
// remediation are fixed here: a finding never carries advice composed from
// content that came from the package or the application.
type findingDef struct {
	id          string
	severity    contracts.Severity
	summary     string
	remediation string
}

var defs = map[string]*findingDef{}

func def(id string, sev contracts.Severity, summary, remediation string) *findingDef {
	if _, dup := defs[id]; dup {
		panic("integrationaudit: finding " + id + " defined twice")
	}
	d := &findingDef{id: id, severity: sev, summary: summary, remediation: remediation}
	defs[id] = d
	return d
}

const (
	crit = contracts.SeverityCritical
	warn = contracts.SeverityWarning
	info = contracts.SeverityInfo
)

// The findings of the framework itself.
var (
	fdCheckFailed = def("audit.check_failed", crit,
		"a check could not run",
		"Find the cause in the service log, fix it and run the audit again. An audit that did not run is not a pass.")
	fdCheckInvalid = def("audit.check_invalid", crit,
		"a check returned something that cannot be reported",
		"Fix the check: its findings must name a check, a severity, a message and a remediation, and its report data must use only strings, integers, booleans, arrays and objects.")
	fdSectionNotRun = def("audit.section_not_run", warn,
		"no check is registered for a required check of master plan 4.5, so nothing is known about it",
		"Register the check that implements the section (the runtime probes of M1-18 and the RAG probes of M2-12 register themselves) or have an authorised operator accept this warning; acceptance never widens the tools or scope the model receives.")
	fdPackageRefused = def("audit.package_refused", crit,
		"the installed package no longer passes the loader's checks",
		"Reinstall the package from its publisher, or sign it again; an installed package whose files, signature or signing key no longer verify cannot be activated.")
)

// Package-target findings.
var (
	fdRAGFieldMissing = def("rag.field_missing", crit,
		"a RAG source does not declare a field master plan 4.5 check 3 requires",
		"Declare the owner, scope, include and exclude rules, credential reference, refresh policy, deletion behaviour and citation resolver of the source in its rag-sources file.")
	fdRAGNoCredential = def("rag.api_source_without_credential", warn,
		"a RAG source that reads from an API declares no credential reference",
		"Declare the credential reference the API source needs (an opaque cred: ID), or confirm in writing that the API is reachable without one and accept this warning.")
	fdDocsIncomplete = def("docs.incomplete", crit,
		"a capability is not fully documented in the default language",
		"Add the missing documentation entries to documentation/index.yaml and list them in the capability's doc_refs. Missing documentation is a failed capability: AlixAI never guesses.")
	fdContractsIncomplete = def("contracts.incomplete", crit,
		"a user capability has no conversation contract in a supported language",
		"Add the conversation contract for the capability and language under conversation-contracts/.")
	fdPermMissing = def("permissions.capability_ungranted", warn,
		"a user capability is granted to no role, so nobody can use it",
		"Grant the capability to the roles that may use it in a policies/ role_grants entry, or remove the capability from the package.")
	fdPermAdmin = def("permissions.admin_capability_granted", warn,
		"a role is granted a capability whose audience is admin, which is never offered in user conversations",
		"Remove the grant, or change the capability's audience to user and give it a conversation contract.")
	fdPermRoleEmpty = def("permissions.role_without_grants", info,
		"a role declared by the tenancy model is granted no capability",
		"Grant the role its capabilities in a policies/ role_grants entry, or drop the role if it is not used.")
)

// Application-target findings: the control surface (check 7).
var (
	fdNotInspected = def("surface.not_inspected", warn,
		"no inspection adapter was available, so the control surface of the application could not be compared with the package",
		"Run the audit with an adapter for the application (MCP, OpenAPI or native) that implements Describe, or have an authorised operator accept this warning. Anything not inspected stays unverified in the report.")
	fdDescribeFailed = def("surface.describe_failed", crit,
		"the application's adapter could not describe the application's operations",
		"Make the application reachable and its Describe answer valid, then run the audit again.")
	fdVersionIncompatible = def("surface.version_incompatible", crit,
		"the adapter and this core share no contract version",
		"Upgrade AlixAI or the adapter so that they speak a common version of the adapter contract.")
	fdUnreachable = def("surface.unreachable_operation", crit,
		"the package maps an operation the application does not offer",
		"Expose the operation on the application, or remove the mapping from the capability file. A capability whose declared operation cannot be reached cannot be used.")
	fdOverBroad = def("surface.over_broad_operation", crit,
		"the application offers an operation for a capability that the package does not declare for it",
		"Narrow the application's adapter mapping to the operations the capability declares, or declare the operation in the capability file and document it. An operation that can prepare or commit under a capability that does not declare it is never accepted.")
	fdOverBroadMinor = def("surface.over_broad_operation_minor", warn,
		"the application offers a read-only operation for a capability that the package does not declare for it",
		"Narrow the adapter mapping or declare the operation in the capability file; or have an authorised operator accept this warning. The operation stays uncallable by AlixAI.")
	fdUndeclaredCapability = def("surface.undeclared_capability", warn,
		"the application describes a capability the package does not declare",
		"Declare and document the capability in the package, or remove it from the adapter mapping. AlixAI never routes to a capability the package does not declare, so it stays uncallable.")
	fdUndocumented = def("surface.undocumented_operation", warn,
		"the application exposes an operation that no capability of the package maps or documents",
		"Map and document the operation in a capability, or take it off the interface AlixAI is given. It stays invisible to the model and uncallable by AlixAI.")
	fdAmbiguous = def("surface.ambiguous_operation", crit,
		"the application exposes an operation whose name differs only by case or by '_', '-' and '.' from an operation the package maps",
		"Rename the operation on the application side so that it cannot be mistaken for the mapped one; AlixAI will not guess which was meant.")
	fdAppVersion = def("surface.application_version_mismatch", warn,
		"the running application reports another version than the one the package was written for",
		"Audit and, if needed, update the package for the running application version; or have an authorised operator accept this warning.")
)

// Runtime-target findings.
var (
	fdInspectFailed = def("system.inspect_failed", warn,
		"the application's inspection call failed, so none of its runtime and system aspects could be verified",
		"Make the application's inspection interface reachable and run the audit again; until then every aspect stays unverified in the report.")
	fdHealthDown = def("system.health_unavailable", crit,
		"the application reports itself unavailable",
		"Bring the application up and make it report ok before the audit.")
	fdHealthDegraded = def("system.health_degraded", warn,
		"the application reports itself degraded",
		"Restore the application to full health, or have an authorised operator accept the degraded state for this audit.")
	fdHealthUnknown = def("system.health_unverified", warn,
		"the application's health could not be read",
		"Make the application's health interface reachable and run the audit again.")
	fdSysVersion = def("system.version_mismatch", warn,
		"the application's inspection reports another version than the one the package was written for",
		"Audit and, if needed, update the package for the running application version; or have an authorised operator accept this warning.")
	fdProfileBelow = def("profile.below_minimum", crit,
		"the configured model profile is below the minimum the package requires",
		"Run the integration on a profile that meets the package's model_requirements.minimum_profile, or use a package that supports this profile.")
)

// Informational findings about what could leave the deployment (additive
// plan, external provider mode).
var (
	fdExtDefaults = def("external.defaults_assumed", info,
		"the package declares no egress mode or data classification, so every RAG source is read as local_only and every field as internal",
		"Nothing is needed to stay local. To allow a source or field to reach an external provider, declare its egress mode and classification in the package; the package can only restrict external mode, never enable it.")
	fdExtSource = def("external.source_not_local_only", info,
		"a RAG source declares an egress mode other than local_only: its content could reach an external provider if external mode is enabled",
		"Confirm the egress mode and the classification are intended before external mode is enabled for the tenant.")
	fdExtField = def("external.field_classified", info,
		"a capability field is classified above internal: its values could be restricted from or approved for an external provider",
		"Confirm the classification is intended and that no prompt template includes the field.")
	fdExtForbid = def("external.forbid", info,
		"the package forbids external provider mode",
		"Nothing is needed. The package's external.forbid can only be lifted by publishing a new package version.")
)

// Check-owned loader-pass-through: the sections that the loader's findings
// belong to.
var loaderSections = []struct {
	prefix  string
	section int
}{
	{"signature.", SectionAuthenticity},
	{"compat.", SectionAuthenticity},
	{"manifest.", SectionAuthenticity},
	{"package.", SectionAuthenticity},
	{"archive.", SectionAuthenticity},
	{"yaml.", SectionAuthenticity},
	{"schema.rag_source", SectionRAGInventory},
	{"schema.", SectionAuthenticity},
	{"rag.", SectionRAGInventory},
	{"docs.", SectionDocumentation},
	{"contract.", SectionContracts},
	{"terminology.", SectionContracts},
	{"endpoint.", SectionControlSurface},
	{"adapter.", SectionControlSurface},
	{"capability.", SectionControlSurface},
	{"policy.", sectionSupplementary},
	{"tests.", sectionSupplementary},
	{"unique.", SectionAuthenticity},
	{"load.", SectionAuthenticity},
}

// loaderSection returns the section the loader finding id belongs to; manifest
// and capability findings that are about file references fall under
// authenticity, adapter mapping under the control surface.
func loaderSection(id string) int {
	for _, ls := range loaderSections {
		if strings.HasPrefix(id, ls.prefix) {
			return ls.section
		}
	}
	return SectionAuthenticity
}

// newFinding builds a finding from a definition. detail is the specific,
// bounded message; the definition's summary stands in for an empty one.
func newFinding(d *findingDef, capability, path, format string, args ...any) contracts.Finding {
	msg := d.summary
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return contracts.Finding{
		CheckID:     d.id,
		Severity:    d.severity,
		Capability:  capability,
		Path:        path,
		Message:     msg,
		Remediation: d.remediation,
	}
}

// FindingInfo describes one kind of finding the audit raises itself.
type FindingInfo struct {
	ID          string
	Severity    contracts.Severity
	Summary     string
	Remediation string
}

// Findings lists every kind of finding the audit raises itself, sorted by ID.
// The findings of the package loader (apppkg.Checks) are reported alongside.
func Findings() []FindingInfo {
	out := make([]FindingInfo, 0, len(defs))
	for _, d := range defs {
		out = append(out, FindingInfo{ID: d.id, Severity: d.severity, Summary: d.summary, Remediation: d.remediation})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// quote renders a name from the package or the application for a message:
// quoted and cut to a bounded length.
func quote(s string) string {
	const max = 80
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max]) + "..."
	}
	return fmt.Sprintf("%q", s)
}
