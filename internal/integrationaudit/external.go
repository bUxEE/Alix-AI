package integrationaudit

import (
	"context"
	"sort"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The data classes and source egress modes of the external provider mode
// (additive plan section 6.1 and 8). They are read here only to be listed; the
// audit never enables or restricts anything. The package declares them as the
// typed members egress_mode and classification of a RAG source, classification
// of a capability field and external.forbid of the manifest (X-03), read
// through the apppkg accessors so a member a package does not declare reads
// as the safe default.
const (
	egressLocalOnly = "local_only"
	classInternal   = "internal"
)

var classRank = map[string]int{"public": 0, "internal": 1, "confidential": 2, "restricted": 3}

// classRankOf ranks a data class. An unknown class is the most restrictive:
// it is never read as harmless.
func classRankOf(c string) int {
	if r, ok := classRank[c]; ok {
		return r
	}
	return classRank["restricted"]
}

// sourceDecl is one RAG source as the inventory reads it: the effective egress
// mode and classification, and whether the package declared each.
type sourceDecl struct {
	mode, class                 string
	modeDeclared, classDeclared bool
}

// fieldDecl is one capability field as the inventory reads it.
type fieldDecl struct {
	capability, name, class string
	classDeclared           bool
}

// manifestDecl is what the manifest says about external providers.
type manifestDecl struct {
	// declared is true when the manifest has an external block.
	declared bool
	forbid   bool
}

// externalInventory lists what could leave the deployment if external mode
// were enabled: every RAG source whose egress mode is not local_only, every
// field classified above internal, and the package's external.forbid. The
// findings are informational: they let an operator see the exposure before
// enabling the mode.
func externalInventory(manifest manifestDecl, sources map[string]sourceDecl, fields []fieldDecl) (*External, []contracts.Finding) {
	ext := &External{Sources: []ExternalSource{}, Fields: []ExternalField{}}
	var findings []contracts.Finding

	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		src := sources[id]
		if src.modeDeclared || src.classDeclared {
			ext.DeclaredByPackage = true
		}
		if src.mode == egressLocalOnly {
			continue
		}
		ext.Sources = append(ext.Sources, ExternalSource{ID: id, Egress: src.mode, Classification: src.class})
		findings = append(findings, newFinding(fdExtSource, "", "rag-sources/",
			"RAG source %s declares egress mode %s with classification %s", quote(id), quote(src.mode), quote(src.class)))
	}

	sort.Slice(fields, func(i, j int) bool {
		if fields[i].capability != fields[j].capability {
			return fields[i].capability < fields[j].capability
		}
		return fields[i].name < fields[j].name
	})
	for _, f := range fields {
		if f.classDeclared {
			ext.DeclaredByPackage = true
		}
		if classRankOf(f.class) <= classRank[classInternal] {
			continue
		}
		ext.Fields = append(ext.Fields, ExternalField{Capability: f.capability, Field: f.name, Classification: f.class})
		findings = append(findings, newFinding(fdExtField, f.capability, "capabilities/",
			"field %s of capability %s is classified %s", quote(f.name), quote(f.capability), quote(f.class)))
	}

	if manifest.declared {
		ext.DeclaredByPackage = true
	}
	if manifest.forbid {
		ext.ForbidExternal = true
		findings = append(findings, newFinding(fdExtForbid, "", "alixai.yaml", ""))
	}
	if !ext.DeclaredByPackage {
		findings = append(findings, newFinding(fdExtDefaults, "", "", ""))
	}
	return ext, findings
}

// runExternalInventory is the informational inventory of what could leave the
// deployment. It runs on the typed package.
func runExternalInventory(_ context.Context, in *Input) (*Outcome, error) {
	out := &Outcome{}
	pkg := in.Package
	if pkg == nil {
		noPackage(out, sectionSupplementary, "external")
		return out, nil
	}
	sources := map[string]sourceDecl{}
	for id, src := range pkg.Sources {
		sources[id] = sourceDecl{
			mode:          string(apppkg.EffectiveSourceEgress(src)),
			class:         string(apppkg.EffectiveSourceClassification(src)),
			modeDeclared:  src.EgressMode != nil,
			classDeclared: src.Classification != nil,
		}
	}
	var fields []fieldDecl
	for _, id := range pkg.SortedCapabilityIDs() {
		for _, f := range pkg.Capabilities[id].Fields {
			fields = append(fields, fieldDecl{
				capability:    id,
				name:          string(f.Name),
				class:         string(apppkg.EffectiveFieldClassification(f)),
				classDeclared: f.Classification != nil,
			})
		}
	}
	m := manifestDecl{declared: pkg.Manifest.External != nil, forbid: apppkg.ForbidsExternal(pkg.Manifest)}
	ext, findings := externalInventory(m, sources, fields)
	out.External = ext
	out.Add(findings...)
	return out, nil
}
