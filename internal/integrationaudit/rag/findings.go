package rag

import (
	"fmt"
	"sort"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// findingDef is one kind of finding a RAG probe raises. Severity and remediation are
// fixed here: a finding never carries advice composed from text that came from the
// package or the deployment. Messages name declared source slugs, fixture references,
// probe IDs and typed error codes, never a value the deployment returned and never an
// identifier generated for a run (a source, tenant, principal, chunk or reference ID),
// so two audits of the same package give the same report.
type findingDef struct {
	id          string
	severity    contracts.Severity
	summary     string
	remediation string
}

var defs = map[string]*findingDef{}

func def(id string, sev contracts.Severity, summary, remediation string) *findingDef {
	if _, dup := defs[id]; dup {
		panic("rag audit: finding " + id + " defined twice")
	}
	d := &findingDef{id: id, severity: sev, summary: summary, remediation: remediation}
	defs[id] = d
	return d
}

const (
	crit = contracts.SeverityCritical
	warn = contracts.SeverityWarning
)

// newFinding builds a finding from a definition. path is the package-relative place the
// finding concerns; the definition's summary stands in for an empty message.
func newFinding(d *findingDef, path, format string, args ...any) contracts.Finding {
	msg := d.summary
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return contracts.Finding{
		CheckID:     d.id,
		Severity:    d.severity,
		Path:        path,
		Message:     msg,
		Remediation: d.remediation,
	}
}

// FindingInfo describes one kind of finding the RAG probes raise.
type FindingInfo struct {
	ID          string
	Severity    contracts.Severity
	Summary     string
	Remediation string
}

// Findings lists every kind of finding the RAG probes raise, sorted by ID.
func Findings() []FindingInfo {
	out := make([]FindingInfo, 0, len(defs))
	for _, d := range defs {
		out = append(out, FindingInfo{ID: d.id, Severity: d.severity, Summary: d.summary, Remediation: d.remediation})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// quote renders a name from the package for a message: quoted and cut to a bounded
// length.
func quote(s string) string {
	const max = 80
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max]) + "..."
	}
	return fmt.Sprintf("%q", s)
}

// Paths the findings point at.
const (
	pathSources = "rag-sources/"
	pathTests   = "tests/"
)

// Check 3 (master plan 4.5): the RAG source inventory, live half.
var (
	fdUndeclared = def("ragprobe.source_undeclared", crit,
		"the deployment holds a registered RAG source for this application that the package under audit does not declare",
		"Remove the registration, or declare the source in the package, have the package audited and register it again. A source that the signed package does not declare is never served: registration may only instantiate what an audited package declares.")
	fdBindingMismatch = def("ragprobe.source_binding_mismatch", crit,
		"a registered RAG source is bound to a tenant or workspace that the package's declaration does not allow",
		"Remove the registration and register the source again for the scope its declaration names: a request-bound level takes the registering scope's tenant or workspace, a fixed level takes the declaration's own, and a shared level names none.")
	fdDefinitionRefused = def("ragprobe.source_definition_refused", crit,
		"the core refuses to build a registrable source from the package's declaration",
		"Correct the declaration in rag-sources/ so that the core can register it: a type that fits its root, at least one include rule and one accepted format, positive size limits, complete tenant and workspace bindings and a root the provider accepts (a folder root may not be a whole volume or share).")
	fdUnreachable = def("ragprobe.source_unreachable", warn,
		"a declared RAG source cannot be reached from this deployment, so what it holds cannot be indexed or cited",
		"Make the declared root reachable (create or mount the folder, register the operator's mount under the id the package names) and audit the package again, or have an authorised operator accept this warning; acceptance never widens the sources the model can read.")
	fdRulesEmpty = def("ragprobe.source_rules_match_nothing", warn,
		"a declared RAG source's include and exclude rules admit nothing",
		"Correct the include and exclude rules: an include pattern that no item can match, or an exclude pattern that removes everything an include admits, leaves a source that serves no document.")
	fdCredentialUnknown = def("ragprobe.source_credential_unknown", warn,
		"a declared RAG source names a credential reference that the application does not report among its credential references",
		"Provision the credential under the reference the declaration names (or correct the reference) and expose the references through the application's inspection adapter; the audit never reads a secret, only the references.")
	fdScopeNotEnforced = def("ragprobe.source_scope_not_enforced", crit,
		"the core's check of a source's declared scope did not admit or refuse as the declaration says",
		"This is a fault of the core, not of the package: a source's tenant and workspace binding must admit the scope it serves and refuse every other. Do not activate packages on this build.")
)

// Check 3: citation resolver probes.
var (
	fdCitationMissing = def("ragprobe.citation_missing", crit,
		"a grounded answer carries a claim with no citation, or no citation at all",
		"This is a fault of the core, not of the package: every claim of a successful RAG answer must be supported by at least one citation (master plan rule 9). Do not activate packages on this build.")
	fdCitationUnresolved = def("ragprobe.citation_unresolved", crit,
		"a citation of a grounded answer does not resolve for the principal it was written for",
		"Every citation's open/preview reference must resolve, permission-checked, for the principal the answer was written for, until it expires. Check the source's citation resolver declaration and the source catalog.")
	fdCitationOriginal = def("ragprobe.citation_wrong_original", crit,
		"a citation resolves to something other than the original it cites: another source, another version, or a passage that is not in the original",
		"A reference must resolve to the source and version it was issued for, with the passage that was cited, and to nothing else.")
	fdCitationOpaque = def("ragprobe.citation_not_opaque", crit,
		"a citation's open reference is not an opaque scoped identifier: it carries a path, a URL or another kind of reference",
		"A citation reference must be an opaque, random file reference that is never a path or a URL and that only resolves for the scope it was issued to.")
	fdCitationForeign = def("ragprobe.citation_resolves_for_other_scope", crit,
		"a citation reference resolved for a principal, tenant or workspace other than the one it was issued to",
		"A reference is a pointer to a decision, never a grant: resolving it must check the application, tenant, workspace and principal it was issued for and ask the source catalog afresh whether the caller may still read the source.")
	fdCitationOracle = def("ragprobe.citation_existence_disclosed", warn,
		"the answer to a reference of another scope differs from the answer to a reference that never existed",
		"Answer both with the same typed refusal, so the answer cannot be used to find out which references exist for other tenants.")
	fdCitationRevoked = def("ragprobe.citation_survives_revocation", crit,
		"a citation reference still resolved after the source it cites was revoked or removed",
		"A reference must be refused as soon as the source catalog no longer lets the principal read the source, however recently the reference was issued.")
	fdCitationExpiry = def("ragprobe.citation_does_not_expire", crit,
		"a citation reference still resolved after its expiry",
		"A reference must stop resolving when its time to live has passed, and must say that it expired, to the scope it belongs to only.")
	fdSourceChange = def("ragprobe.source_change_not_followed", crit,
		"a change to a source's content, or its removal, was not followed by what retrieval and citation serve",
		"After a document is replaced, retrieval must serve the new text and not the old; after it is deleted, neither retrieval nor an existing citation may serve it. Check the source's refresh and deletion declaration and the index.")
)

// Check 6: exact scope enforcement, retrieval half.
var (
	fdChunkLeak = def("ragprobe.chunk_leak", crit,
		"a chunk, source or identifier of another tenant or workspace reached the evidence package a principal's retrieval returned",
		"Apply the tenant and workspace scope in the index before ranking and again at every hand-over: a chunk of another scope must never be a candidate, so it can never reach the model. Do not activate packages on this build.")
	fdPackageScope = def("ragprobe.package_scope_wrong", crit,
		"an evidence package was bound to another scope than the principal that asked",
		"An evidence package belongs to the application, tenant, workspace and principal it was authorised for and is never reused for another. Do not activate packages on this build.")
	fdOwnChunkMissing = def("ragprobe.own_chunk_missing", crit,
		"retrieval did not return a document that the audit indexed in the principal's own scope",
		"A principal must be able to retrieve what its own scope holds: if the probe could not find its own synthetic passage, the isolation probes prove nothing. Check the source's declaration and the index.")
	fdByReference = def("ragprobe.chunk_readable_by_reference", crit,
		"a chunk of another scope could be read, or rechecked as live, by its reference",
		"Reading a chunk by its reference must apply the caller's scope and must answer a reference of another scope exactly as one that does not exist.")
)

// Check 10: runtime smoke test, RAG path.
var (
	fdSmokePath = def("ragprobe.smoke_path_failed", crit,
		"a RAG smoke path did not complete as the package declares",
		"Fix the application's sources or the smoke path in the package's tests/ suite: a question the principal's scope can answer must reach a cited, resolvable answer, and one it cannot answer must end in no_supported_evidence.")
	fdCleanup = def("ragprobe.cleanup_incomplete", crit,
		"synthetic documents the audit indexed were still served after the audit removed them",
		"Removing a source must hide everything it held at once: neither retrieval nor a citation may serve it. Do not activate packages on this build.")
	fdLabUnavailable = def("ragprobe.lab_unavailable", warn,
		"the audit could not start its throwaway RAG index, so the retrieval, citation and smoke probes did not run",
		"Make sure the audit can create a temporary directory and a SQLite store (the ScratchDir option or the system temporary directory), then audit the package again, or have an authorised operator accept this warning; acceptance never widens the sources the model can read.")
	fdScratchLeft = def("ragprobe.scratch_left_behind", warn,
		"the throwaway store of a RAG probe could not be removed after the audit",
		"Remove the leftover directory under the audit's scratch directory; it holds only synthetic documents the audit wrote.")
)
