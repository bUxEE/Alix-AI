package apppkg

import (
	"sort"

	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Egress declarations (external provider mode plan sections 6.1 and 8, master
// plan 12.2). A package says what MAY leave the deployment: a RAG source's
// egress_mode and classification, a capability field's classification and
// local_only, and the manifest's external.forbid. Everything is optional and
// defaults to the safest value, so a package that declares nothing keeps
// behaving exactly as in Local mode. The mode itself (Local, External, Hybrid)
// is deployment and tenant policy, never a package field, and a package can
// only restrict: nothing declared here lets anything leave by itself.
//
// These checks only look for declarations that contradict each other or that
// would be unsafe if external mode were enabled; they never enable anything.
var (
	chkEgressRestricted = reg("egress.restricted_source_exposed", crit, "master plan 12.2, additive plan 6.1 and 8",
		"a RAG source classified restricted declares an egress mode that lets content or metadata reach an external provider (allow_excerpt or allow_metadata)",
		"Restricted sources stay local: remove egress_mode or set it to local_only (or deny), or lower the classification only if the data really is not restricted.")
	chkEgressClassMissing = reg("egress.excerpt_classification_missing", crit, "master plan 12.2, additive plan 8",
		"a RAG source declares egress_mode allow_excerpt but no explicit classification, so the default (internal) would be assumed for content that may leave",
		"State the classification of the source (public, internal or confidential) next to egress_mode: allow_excerpt.")
	chkEgressForbidden = reg("egress.forbidden_by_package", crit, "additive plan 6.1",
		"the manifest forbids external providers (external.forbid) but a RAG source declares egress_mode allow_excerpt or allow_metadata",
		"Remove the egress_mode from the source (or set it to local_only or deny), or drop external.forbid: a package cannot both forbid and prepare to send.")
	chkEgressLocalField = reg("egress.local_only_field_in_template", crit, "master plan 9.2, additive plan 6.1",
		"a conversation contract template places the value of a field marked local_only, which can never leave the deployment, into a template",
		"Remove the {input.<field>} placeholder from the template, or clear local_only on the field if its value may appear in model-visible text.")
)

// EffectiveSourceEgress is the egress mode a RAG source declares; a source
// that declares none is local_only.
func EffectiveSourceEgress(s ap.RAGSource) rag.SourceEgress {
	if s.EgressMode == nil || *s.EgressMode == "" {
		return rag.SourceEgressLocalOnly
	}
	return *s.EgressMode
}

// EffectiveSourceClassification is the classification a RAG source declares; a
// source that declares none is internal (the registration default of
// schemas/rag shared sourceClassification).
func EffectiveSourceClassification(s ap.RAGSource) rag.SourceClassification {
	if s.Classification == nil || *s.Classification == "" {
		return rag.SourceClassificationInternal
	}
	return *s.Classification
}

// EffectiveFieldClassification is the classification a capability field
// declares; a field that declares none is internal.
func EffectiveFieldClassification(f ap.CapabilityField) rag.SourceClassification {
	if f.Classification == nil || *f.Classification == "" {
		return rag.SourceClassificationInternal
	}
	return *f.Classification
}

// FieldLocalOnly reports whether a capability field is marked local_only.
func FieldLocalOnly(f ap.CapabilityField) bool { return f.LocalOnly != nil && *f.LocalOnly }

// ForbidsExternal reports whether the manifest forbids external providers.
func ForbidsExternal(m ap.Manifest) bool {
	return m.External != nil && m.External.Forbid != nil && *m.External.Forbid
}

// checkEgress runs the checks of the egress declarations. It is called from
// crossCheck.
func (md *model) checkEgress() {
	forbid := ForbidsExternal(md.m)

	for _, id := range md.sourceIDs() {
		e := md.sources[id]
		mode := EffectiveSourceEgress(e.s)
		leaves := mode == rag.SourceEgressAllowExcerpt || mode == rag.SourceEgressAllowMetadata
		if !leaves {
			continue
		}
		if EffectiveSourceClassification(e.s) == rag.SourceClassificationRestricted {
			md.c.add(chkEgressRestricted, "", e.path,
				"source %s is classified restricted but declares egress_mode %s", q(id), q(string(mode)))
		}
		if mode == rag.SourceEgressAllowExcerpt && e.s.Classification == nil {
			md.c.add(chkEgressClassMissing, "", e.path,
				"source %s declares egress_mode allow_excerpt without a classification", q(id))
		}
		if forbid {
			md.c.add(chkEgressForbidden, "", e.path,
				"the manifest sets external.forbid but source %s declares egress_mode %s", q(id), q(string(mode)))
		}
	}

	keys := make([]ckey, 0, len(md.contracts))
	for k := range md.contracts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cap != keys[j].cap {
			return keys[i].cap < keys[j].cap
		}
		return keys[i].lang < keys[j].lang
	})
	for _, k := range keys {
		cp, ok := md.caps[k.cap]
		if !ok {
			continue
		}
		local := map[string]bool{}
		for _, f := range cp.c.Fields {
			if FieldLocalOnly(f) {
				local[string(f.Name)] = true
			}
		}
		if len(local) == 0 {
			continue
		}
		ce := md.contracts[k]
		for _, s := range contractSlots(ce.c) {
			ph, err := placeholders(s.text)
			if err != nil {
				continue // reported by the contract checks
			}
			reported := map[string]bool{}
			for _, p := range ph {
				name, isInput := cutInput(p)
				if isInput && local[name] && !reported[name] {
					reported[name] = true
					md.c.add(chkEgressLocalField, k.cap, ce.path,
						"%s: placeholder {%s} puts the local_only field %s into a template", s.where, p, q(name))
				}
			}
		}
	}
}

// cutInput returns the field name of an {input.<field>} placeholder.
func cutInput(p string) (string, bool) {
	const prefix = "input."
	if len(p) > len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):], true
	}
	return "", false
}
