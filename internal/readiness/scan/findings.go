package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/readiness/catalog"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

const (
	scanMaxEvidence      = 32
	scanEvidencePerGroup = 3
)

// scanVerdict is what a decider concluded about one requirement.
type scanVerdict struct {
	status  readiness.Status
	groups  []string
	summary string
	reason  string
}

func vFound(summary string, groups ...string) scanVerdict {
	return scanVerdict{status: readiness.StatusFound, groups: groups, summary: summary}
}

func vPartial(summary string, groups ...string) scanVerdict {
	return scanVerdict{status: readiness.StatusPartial, groups: groups, summary: summary}
}

func vMissing(summary string) scanVerdict {
	return scanVerdict{status: readiness.StatusMissing, summary: summary}
}

func vUnverified(reason string, groups ...string) scanVerdict {
	return scanVerdict{status: readiness.StatusUnverified, groups: groups, reason: reason, summary: reason}
}

func vContradicted(summary string, groups ...string) scanVerdict {
	return scanVerdict{status: readiness.StatusContradicted, groups: groups, summary: summary}
}

// scanLadder is the common three-step verdict: everything required, something, nothing.
func (s *scanState) ladder(what string, full bool, some bool, groups ...string) scanVerdict {
	switch {
	case full:
		return vFound(what+" found.", groups...)
	case some:
		return vPartial(what+" found in part.", groups...)
	}
	return vMissing("Not found: " + what + ".")
}

type scanDecider func(*scanState) scanVerdict

// scanDeciders is the rule for every catalog requirement. A requirement without one is reported
// unverified, never missing: the scan cannot say the project lacks something it has no rule for.
var scanDeciders = map[readiness.RequirementID]scanDecider{
	"req.package.manifest": func(s *scanState) scanVerdict {
		return s.ladder("an application package manifest with application, package, compatibility and endpoints", s.has("manifest.file") && s.has("manifest.fields"), s.has("manifest.file"), "manifest.file", "manifest.fields")
	},
	"req.package.signatures": func(s *scanState) scanVerdict {
		return s.ladder("a signatures directory with a signature manifest", s.has("sig.manifest"), s.has("sig.dir"), "sig.manifest", "sig.dir")
	},
	"req.package.tests": func(s *scanState) scanVerdict {
		return s.ladder("package test cases", s.has("tests.case"), false, "tests.case")
	},
	"req.documentation.capabilities": func(s *scanState) scanVerdict {
		n := len(s.caps)
		if n == 0 {
			return vMissing("No capabilities were identified, so no documentation can be matched to one.")
		}
		what := "capabilities"
		if s.capsAreCandidates {
			what = "API operations treated as capability candidates"
		}
		switch {
		case s.capDocCovered >= n:
			return vFound(fmt.Sprintf("All %d %s are described in documentation.", n, what), "doc.cap")
		case s.capDocCovered > 0:
			return vPartial(fmt.Sprintf("%d of %d %s are described in documentation.", s.capDocCovered, n, what), "doc.cap")
		}
		return vMissing(fmt.Sprintf("None of the %d %s is described in documentation.", n, what))
	},
	"req.documentation.fields_and_validation": func(s *scanState) scanVerdict {
		both := s.docFileWith(scanDocFields | scanDocErrors)
		return s.ladder("documentation of fields and validation or error codes", both, s.has("doc.fields", "doc.errors", "def.fields", "def.errors"), "doc.fields", "doc.errors", "def.fields", "def.errors")
	},
	"req.documentation.risk_and_outcomes": func(s *scanState) scanVerdict {
		both := s.docFileWith(scanDocRisk | scanDocConfirm)
		return s.ladder("documentation of risk and confirmation", both, s.has("doc.risk", "doc.confirm", "def.risk", "def.confirm"), "doc.risk", "doc.confirm", "def.risk", "def.confirm")
	},
	"req.contracts.conversation": func(s *scanState) scanVerdict {
		n := len(s.caps)
		switch {
		case n > 0 && s.capCCCovered >= n:
			return vFound(fmt.Sprintf("Every one of the %d capabilities has conversation-contract material.", n), "cc.cap")
		case s.capCCCovered > 0 || s.has("cc.file"):
			return vPartial("Conversation-contract files exist but do not cover every capability.", "cc.cap", "cc.file")
		}
		return vMissing("No conversation-contract material found.")
	},
	"req.contracts.terminology": func(s *scanState) scanVerdict {
		return s.ladder("terminology material", s.has("terms.file"), s.has("terms.heading"), "terms.file", "terms.heading")
	},
	"req.mcp.server": func(s *scanState) scanVerdict {
		return s.ladder("an MCP server configuration or entry point", s.has("mcp.config", "mcp.entry"), s.has("mcp.dep"), "mcp.config", "mcp.entry", "mcp.dep")
	},
	"req.mcp.tool_list": func(s *scanState) scanVerdict {
		return s.ladder("MCP tool registrations with input schemas", s.has("mcp.tool.schema"), s.has("mcp.tool", "mcp.tool.doc") || len(s.tools) > 0, "mcp.tool.schema", "mcp.tool", "mcp.tool.doc", "mcp.tool.allow")
	},
	"req.openapi.description": func(s *scanState) scanVerdict {
		if s.has("openapi.op") {
			return vFound(fmt.Sprintf("%d OpenAPI or Swagger document(s) declaring operations.", len(s.apis)), "openapi.doc", "openapi.op")
		}
		if s.has("openapi.doc") {
			return vPartial("An OpenAPI or Swagger document was found that declares no operations.", "openapi.doc")
		}
		if s.has("openapi.unparsed") {
			return vUnverified("An OpenAPI or Swagger document was found but could not be parsed within the scan's bounds.", "openapi.unparsed")
		}
		return vMissing("No OpenAPI or Swagger document found.")
	},
	"req.native.adapter": func(s *scanState) scanVerdict {
		return s.ladder("a native adapter using an AlixAI SDK and declaring operations", s.has("native.full"), s.has("native.import"), "native.full", "native.import", "native.ops")
	},
	"req.surface.control_surface": func(s *scanState) scanVerdict {
		return s.ladder("capability declarations with an adapter and a transport", s.has("cap.decl") && s.has("manifest.transport", "cap.transport"),
			s.has("cap.decl", "openapi.op", "mcp.tool") || len(s.tools) > 0, "cap.decl", "manifest.transport", "cap.transport", "openapi.op", "mcp.tool")
	},
	"req.data.read": func(s *scanState) scanVerdict {
		return s.ladder("read operations with typed responses", s.has("op.read.typed"), s.has("op.read"), "op.read.typed", "op.read")
	},
	"req.data.validate_prepare": func(s *scanState) scanVerdict {
		return s.ladder("validate or prepare operations paired with a commit", s.has("op.validate", "op.prepare") && s.has("op.commit"),
			s.has("op.validate", "op.prepare", "op.commit", "op.write"), "op.validate", "op.prepare", "op.commit", "op.write")
	},
	"req.data.commit": func(s *scanState) scanVerdict {
		return s.ladder("commit operations with a typed receipt or identifier result", s.has("op.commit.typed") && s.has("op.commit.receipt"),
			s.has("op.commit", "op.write"), "op.commit.receipt", "op.commit.typed", "op.commit", "op.write")
	},
	"req.data.safe_execution": func(s *scanState) scanVerdict {
		return s.ladder("idempotency, expiry and version handling", s.has("safe.idem") && s.has("safe.expiry") && s.has("safe.version"),
			s.has("safe.idem", "safe.expiry", "safe.version", "safe.doc"), "safe.idem", "safe.expiry", "safe.version", "safe.doc")
	},
	"req.data.typed_failures": func(s *scanState) scanVerdict {
		return s.ladder("an error-code enumeration with typed error responses", s.has("err.enum") && s.has("err.typed"), s.has("err.enum", "err.typed"), "err.enum", "err.typed")
	},
	"req.scope.tenant_workspace": func(s *scanState) scanVerdict {
		return s.ladder("tenant and workspace scope fields", (s.has("scope.tenant") && s.has("scope.workspace")) || s.has("manifest.tenancy"),
			s.has("scope.tenant", "scope.workspace"), "manifest.tenancy", "scope.tenant", "scope.workspace")
	},
	"req.scope.principal_roles": func(s *scanState) scanVerdict {
		return s.ladder("role definitions and an authentication or context-exchange mechanism", s.has("roles.def") && s.has("auth.mech"), s.has("roles.def", "auth.mech"), "roles.def", "auth.mech")
	},
	"req.permissions.model": func(s *scanState) scanVerdict {
		return s.ladder("permission checks or role-to-operation tables", s.has("perm.check", "perm.table"), s.has("perm.doc"), "perm.check", "perm.table", "perm.doc")
	},
	"req.permissions.policy_files": func(s *scanState) scanVerdict {
		return s.ladder("policy documents in a policies directory", s.has("policy.file"), false, "policy.file")
	},
	"req.rag.source_candidates": func(s *scanState) scanVerdict {
		return s.ladder("RAG source candidates (document folders, volume mounts or provider configuration)", s.has("rag.folder", "rag.volume", "rag.provider", "rag.decl"), false, "rag.decl", "rag.folder", "rag.volume", "rag.provider")
	},
	"req.rag.source_declarations": func(s *scanState) scanVerdict {
		n, c := len(s.ragDecls), 0
		for _, d := range s.ragDecls {
			if d.complete {
				c++
			}
		}
		switch {
		case n > 0 && c == n:
			return vFound(fmt.Sprintf("%d RAG source declaration(s), all with the required fields.", n), "rag.decl.complete")
		case n > 0:
			return vPartial(fmt.Sprintf("%d RAG source declaration(s), %d with every required field.", n, c), "rag.decl.complete", "rag.decl")
		}
		return vMissing("No RAG source declarations found.")
	},
	"req.rag.live_sources": func(s *scanState) scanVerdict {
		if len(s.ragDecls) == 0 {
			return vMissing("No RAG source declarations to compare with the project's source candidates.")
		}
		return vUnverified("Whether the declared sources are reachable is verified by the integration audit, not by readiness discovery.", "rag.decl")
	},
	"req.rag.citation_resolver": func(s *scanState) scanVerdict {
		n, c := len(s.ragDecls), 0
		for _, d := range s.ragDecls {
			if d.citation {
				c++
			}
		}
		switch {
		case n > 0 && c == n:
			return vFound("Every RAG source declares a citation resolver.", "rag.citation")
		case c > 0 || s.has("rag.citation.doc"):
			return vPartial("A citation resolver is declared or described for some sources only.", "rag.citation", "rag.citation.doc")
		}
		return vMissing("No citation resolver found.")
	},
	"req.rag.scope_per_source": func(s *scanState) scanVerdict {
		n, c := len(s.ragDecls), 0
		for _, d := range s.ragDecls {
			if d.scope {
				c++
			}
		}
		switch {
		case n > 0 && c == n:
			return vFound("Every RAG source declares its scope.", "rag.scope")
		case c > 0 || s.has("rag.scope.meta"):
			return vPartial("Scope is declared for some sources or in document metadata only.", "rag.scope", "rag.scope.meta")
		}
		return vMissing("No per-source scope found.")
	},
	"req.rag.synthetic_documents": func(s *scanState) scanVerdict {
		return s.ladder("synthetic fixture documents in tests", s.has("rag.synth"), s.has("rag.synth.partial"), "rag.synth", "rag.synth.partial")
	},
	"req.model.runtime_profile": func(s *scanState) scanVerdict {
		if s.has("model.manifest") {
			return vFound("The manifest declares model and profile requirements.", "model.manifest", "model.runtime")
		}
		if s.has("model.runtime") {
			return vPartial("A model runtime is configured but the manifest declares no model requirements.", "model.runtime")
		}
		if len(s.models) > 0 {
			return vUnverified("Model files are present, but binary model files are never read, so no model requirement could be established from them.")
		}
		return vMissing("No model or runtime requirements found.")
	},
	"req.model.external_egress": func(s *scanState) scanVerdict {
		return s.ladder("network permissions or provider references", s.has("egress.manifest"), s.has("egress.provider"), "egress.manifest", "egress.provider")
	},
	"req.deployment.inspection": func(s *scanState) scanVerdict {
		return s.ladder("a health or inspection endpoint", s.has("inspect.endpoint"), false, "inspect.endpoint")
	},
	"req.deployment.compose_and_ports": func(s *scanState) scanVerdict {
		if s.has("deploy.conflict") {
			return vContradicted("A Dockerfile and the compose file next to it disagree about the container port.", "deploy.conflict")
		}
		return s.ladder("compose files with published ports and migrations", s.has("compose.file") && s.has("compose.port") && s.has("db.migration"),
			s.has("compose.file", "compose.port", "db.migration"), "compose.file", "compose.port", "db.migration")
	},
	"req.deployment.smoke_environment": func(s *scanState) scanVerdict {
		return s.ladder("a seed or provisioning script for a test tenant", s.has("smoke.script"), s.has("smoke.file"), "smoke.script", "smoke.file")
	},
}

// docFileWith reports whether one documentation file carries every flag.
func (s *scanState) docFileWith(flags uint8) bool {
	for _, v := range s.docFlags {
		if v&flags == flags {
			return true
		}
	}
	return false
}

// evidenceFor picks evidence from the groups: a few per group, no repeats, sorted by path and
// line, at most scanMaxEvidence entries. When a decider named groups that hold both documentation
// flags in one file, the evidence still comes from every group so a reader sees all of it.
func (s *scanState) evidenceFor(groups []string) ([]readiness.FindingEvidence, map[string]string) {
	seen := map[string]bool{}
	var picked []scanSig
	for _, g := range groups {
		n := 0
		for _, sg := range s.sigs[g] {
			k := fmt.Sprintf("%s:%d:%d", sg.Path, sg.Start, sg.End)
			if seen[k] {
				continue
			}
			seen[k] = true
			picked = append(picked, sg)
			if n++; n >= scanEvidencePerGroup {
				break
			}
		}
	}
	sort.SliceStable(picked, func(i, j int) bool {
		if picked[i].Path != picked[j].Path {
			return picked[i].Path < picked[j].Path
		}
		if picked[i].Start != picked[j].Start {
			return picked[i].Start < picked[j].Start
		}
		return picked[i].End < picked[j].End
	})
	if len(picked) > scanMaxEvidence {
		picked = picked[:scanMaxEvidence]
	}
	ev := make([]readiness.FindingEvidence, 0, len(picked))
	detail := map[string]string{}
	for _, sg := range picked {
		f := s.byPath[sg.Path]
		e := readiness.FindingEvidence{
			Path: readiness.RelativePath(sg.Path), LineStart: int64(sg.Start), LineEnd: int64(sg.End), FileSHA256: readiness.SHA256(f.SHA256),
		}
		ev = append(ev, e)
		detail[scanEvidenceKey([]readiness.FindingEvidence{e})] = sg.Detail
	}
	return ev, detail
}

func (s *scanState) buildFindings(cat *catalog.Catalog) ([]readiness.Finding, []ScanExcerpt, error) {
	findings := make([]readiness.Finding, 0, len(cat.Requirements))
	var excerpts []ScanExcerpt
	for _, req := range cat.Requirements {
		dec, ok := scanDeciders[req.ID]
		var v scanVerdict
		if ok {
			v = dec(s)
		} else {
			v = vUnverified("The scanner has no rule for this requirement, so it could not be determined.")
		}
		var ev []readiness.FindingEvidence
		var details map[string]string
		if v.status != readiness.StatusMissing {
			ev, details = s.evidenceFor(v.groups)
		}
		// found, partial and contradicted need evidence; without it the verdict cannot stand.
		if len(ev) == 0 && (v.status == readiness.StatusFound || v.status == readiness.StatusPartial || v.status == readiness.StatusContradicted) {
			v = vUnverified("A rule matched but produced no citable evidence, so the status could not be confirmed.")
		}
		sum := ScanRedact(v.summary)
		if len(sum) > 1000 {
			sum = sum[:1000]
		}
		f := readiness.Finding{
			SchemaVersion: 1,
			RequirementID: req.ID,
			Category:      req.Category,
			Status:        v.status,
			Source:        readiness.SourceDeterministic,
			CheckID:       req.CheckID,
			Evidence:      ev,
			Remediation:   readiness.RemediationText{En: req.Remediation.En, It: &req.Remediation.It},
		}
		if sum != "" {
			t := readiness.Text(sum)
			f.Summary = &t
		}
		if v.status == readiness.StatusUnverified {
			r := v.reason
			if r == "" {
				r = "The status could not be determined."
			}
			t := readiness.Text(ScanRedact(r))
			f.UnverifiedReason = &t
		}
		sumID := sha256.Sum256([]byte(string(req.ID) + "\n" + scanEvidenceKey(ev)))
		f.FindingID = readiness.FindingID("fnd_" + hex.EncodeToString(sumID[:])[:16])
		findings = append(findings, f)
		for _, e := range ev {
			text := details[scanEvidenceKey([]readiness.FindingEvidence{e})]
			excerpts = append(excerpts, ScanExcerpt{
				FindingID: string(f.FindingID), Path: string(e.Path), LineStart: int(e.LineStart), LineEnd: int(e.LineEnd), Text: strings.TrimSpace(ScanRedact(text)),
			})
		}
	}
	if excerpts == nil {
		excerpts = []ScanExcerpt{}
	}
	return findings, excerpts, nil
}
