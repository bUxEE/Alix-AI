package apppkg

import (
	"sort"
	"strings"

	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const docIndexPath = "documentation/index.yaml"

// documented holds what the documentation index covers for one capability,
// in the default language.
type documented struct {
	fields, rules, permissions, errorCodes, outcomes map[string]bool
	risk, confirmation                               bool
}

// checkDocs implements section 14 item 10 and master plan 4.5 check 4: every
// facet of every capability is documented, every reference resolves, every
// document exists. Missing documentation is a failed capability.
func (md *model) checkDocs() {
	idx := md.docs
	if idx == nil {
		// no index: every capability fails for lack of documentation
		for _, id := range md.capIDs() {
			md.c.add(chkDocFacet, id, docIndexPath, "the package has no documentation index, so none of the seven facets of the capability is documented")
		}
		return
	}

	byID := map[string]ap.DocumentationIndexEntry{}
	byCap := map[string][]ap.DocumentationIndexEntry{}
	for _, e := range idx.Entries {
		id := string(e.ID)
		if _, dup := byID[id]; dup {
			md.c.add(chkDuplicate, string(e.CapabilityID), docIndexPath, "documentation entry %s is defined more than once", q(id))
			continue
		}
		byID[id] = e
		byCap[string(e.CapabilityID)] = append(byCap[string(e.CapabilityID)], e)

		if _, listed := md.m.Capabilities[string(e.CapabilityID)]; !listed {
			md.c.add(chkDocCapability, "", docIndexPath, "entry %s documents capability %s, which the manifest does not declare", q(id), q(string(e.CapabilityID)))
		}
		if p := e.Location.Path; !strings.HasPrefix(p, "documentation/") || !md.snap.Has(p) {
			md.c.add(chkDocLocation, string(e.CapabilityID), docIndexPath, "entry %s points at %s, which is not a file below documentation/ in the package", q(id), q(p))
		}
	}

	def := string(md.m.Languages.Default)
	for _, id := range md.capIDs() {
		c := md.caps[id].c
		refs := map[string]bool{}
		for _, r := range c.DocRefs {
			refs[string(r)] = true
			e, ok := byID[string(r)]
			switch {
			case !ok:
				md.c.add(chkDocRef, id, md.caps[id].path, "doc_refs names %s, which is not in the documentation index", q(string(r)))
			case string(e.CapabilityID) != id:
				md.c.add(chkDocRefCap, id, md.caps[id].path, "doc_refs names %s, which documents capability %s", q(string(r)), q(string(e.CapabilityID)))
			}
		}
		for _, e := range byCap[id] {
			if !refs[string(e.ID)] {
				md.c.add(chkDocUnref, id, docIndexPath, "documentation entry %s is not in the capability's doc_refs", q(string(e.ID)))
			}
		}

		got := md.coverage(byCap[id], def)
		want := md.requirements(id, c)
		md.reportMissing(id, "field", want.fields, got.fields)
		md.reportMissing(id, "validation_rule", want.rules, got.rules)
		md.reportMissing(id, "permission", want.permissions, got.permissions)
		md.reportMissing(id, "failure_state", want.errorCodes, got.errorCodes)
		md.reportMissing(id, "user_visible_outcome", want.outcomes, got.outcomes)
		if !got.risk {
			md.c.add(chkDocFacet, id, docIndexPath, "facet risk_level has no entry in language %s", q(def))
		}
		if !got.confirmation {
			md.c.add(chkDocFacet, id, docIndexPath, "facet confirmation_rule has no entry in language %s", q(def))
		}

		// entries whose subject does not exist
		for _, e := range byCap[id] {
			if why := subjectUnknown(e, want); why != "" {
				md.c.add(chkDocSubject, id, docIndexPath, "entry %s documents %s, which the capability does not have", q(string(e.ID)), why)
			}
		}
	}
}

// requirements lists what the capability needs documented.
func (md *model) requirements(id string, c ap.Capability) documented {
	w := documented{
		fields: map[string]bool{}, rules: map[string]bool{}, permissions: map[string]bool{},
		errorCodes: map[string]bool{}, outcomes: map[string]bool{},
	}
	for _, f := range c.Fields {
		w.fields[string(f.Name)] = true
	}
	for _, r := range c.Validation {
		w.rules[string(r.ID)] = true
	}
	for _, p := range c.Permissions {
		w.permissions[string(p)] = true
	}
	for _, f := range c.FailureStates {
		w.errorCodes[string(f.Code)] = true
	}
	// the user-visible outcomes are the success outcomes of the contracts
	for k, ce := range md.contracts {
		if k.cap != id {
			continue
		}
		for _, s := range ce.c.Success {
			w.outcomes[string(s.Outcome)] = true
		}
	}
	return w
}

// coverage collects what the entries document in language lang.
func (md *model) coverage(entries []ap.DocumentationIndexEntry, lang string) documented {
	g := documented{
		fields: map[string]bool{}, rules: map[string]bool{}, permissions: map[string]bool{},
		errorCodes: map[string]bool{}, outcomes: map[string]bool{},
	}
	for _, e := range entries {
		if string(e.Language) != lang {
			continue
		}
		switch string(e.Facet) {
		case "field":
			if e.Field != nil {
				g.fields[string(*e.Field)] = true
			}
		case "validation_rule":
			if e.RuleID != nil {
				g.rules[string(*e.RuleID)] = true
			}
		case "permission":
			if e.Permission != nil {
				g.permissions[string(*e.Permission)] = true
			}
		case "failure_state":
			if e.ErrorCode != nil {
				g.errorCodes[string(*e.ErrorCode)] = true
			}
		case "user_visible_outcome":
			if e.Outcome != nil {
				g.outcomes[string(*e.Outcome)] = true
			}
		case "risk_level":
			g.risk = true
		case "confirmation_rule":
			g.confirmation = true
		}
	}
	return g
}

func (md *model) reportMissing(id, facet string, want, got map[string]bool) {
	var missing []string
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	md.c.add(chkDocFacet, id, docIndexPath, "facet %s has no entry in language %s for %s", facet, q(string(md.m.Languages.Default)), qs(missing))
}

// subjectUnknown returns a description of the entry's subject when the
// capability does not have it, or "".
func subjectUnknown(e ap.DocumentationIndexEntry, want documented) string {
	switch string(e.Facet) {
	case "field":
		if e.Field != nil && !want.fields[string(*e.Field)] {
			return "field " + q(string(*e.Field))
		}
	case "validation_rule":
		if e.RuleID != nil && !want.rules[string(*e.RuleID)] {
			return "validation rule " + q(string(*e.RuleID))
		}
	case "permission":
		if e.Permission != nil && !want.permissions[string(*e.Permission)] {
			return "permission " + q(string(*e.Permission))
		}
	case "failure_state":
		if e.ErrorCode != nil && !want.errorCodes[string(*e.ErrorCode)] {
			return "failure state " + q(string(*e.ErrorCode))
		}
	case "user_visible_outcome":
		if e.Outcome != nil && len(want.outcomes) > 0 && !want.outcomes[string(*e.Outcome)] {
			return "outcome " + q(string(*e.Outcome))
		}
	}
	return ""
}
