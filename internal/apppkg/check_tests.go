package apppkg

import (
	"strings"

	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// safetyKinds are the seven safe-execution probe kinds every consequential
// capability needs (master plan 4.5 check 8).
var safetyKinds = []ap.PackageTestsSafetyProbeKind{
	ap.PackageTestsSafetyProbeKindConfirmedCommit,
	ap.PackageTestsSafetyProbeKindUnconfirmedCommit,
	ap.PackageTestsSafetyProbeKindReplayedCommit,
	ap.PackageTestsSafetyProbeKindExpiredPreparation,
	ap.PackageTestsSafetyProbeKindStaleRecord,
	ap.PackageTestsSafetyProbeKindCancelledPreparation,
	ap.PackageTestsSafetyProbeKindUncertainCommitRecovery,
}

// checkTests implements section 14 item 16 and master plan 4.5 checks 6 and 8:
// the references of every suite resolve, and the suites together cover every
// capability and every RAG source, and every consequential capability with
// all seven safe-execution probe kinds. The probes are only declared here; the
// integration audit (M1-17) executes them.
func (md *model) checkTests() {
	scoped := map[string]bool{}           // capability -> has a scope probe
	retrieved := map[string]bool{}        // source -> has a retrieve probe
	kinds := map[string]map[string]bool{} // capability -> safety kinds covered
	entityTypes := map[string]bool{}
	for _, e := range md.caps {
		for t := range e.c.EntityTypes {
			entityTypes[t] = true
		}
	}

	for _, se := range md.suites {
		t, path := se.t, se.path
		ref := func(format string, args ...any) { md.c.add(chkTestRef, "", path, format, args...) }
		fixture := func(format string, args ...any) { md.c.add(chkTestsFixtures, "", path, format, args...) }
		dup := func(what, id string) {
			md.c.add(chkDuplicate, "", path, "%s %s is declared more than once in the suite", what, q(id))
		}

		// provisioning
		prov := t.Fixtures.Provisioning
		md.checkCall(path, "", "fixtures.provisioning.create_tenant", prov.CreateTenant)
		md.checkCall(path, "", "fixtures.provisioning.seed", prov.Seed)
		md.checkCall(path, "", "fixtures.provisioning.cleanup", prov.Cleanup)
		if prov.SeedData != nil {
			if p := *prov.SeedData; !strings.HasPrefix(p, "tests/") || !md.snap.Has(p) {
				ref("fixtures.provisioning.seed_data %s is not a file below tests/ in the package", q(p))
			}
		}

		// tenants and workspaces
		workspaces := map[string]map[string]bool{} // tenant ref -> workspace refs
		for _, tn := range t.Fixtures.Tenants {
			r := string(tn.Ref)
			if _, ok := workspaces[r]; ok {
				fixture("tenant reference %s is used twice", q(r))
				continue
			}
			ws := map[string]bool{}
			for _, w := range tn.Workspaces {
				ws[string(w)] = true
			}
			workspaces[r] = ws
		}
		belongs := func(tenant, workspace string) (known bool, ok bool) {
			ws, found := workspaces[tenant]
			if !found {
				return false, false
			}
			return true, ws[workspace]
		}

		// principals
		principals := map[string]bool{}
		for _, p := range t.Fixtures.Principals {
			r := string(p.Ref)
			if principals[r] {
				fixture("principal reference %s is used twice", q(r))
				continue
			}
			principals[r] = true
			switch known, ok := belongs(string(p.Tenant), string(p.Workspace)); {
			case !known:
				ref("principal %s names tenant %s, which the suite does not declare", q(r), q(string(p.Tenant)))
			case !ok:
				fixture("principal %s is in workspace %s, which is not a workspace of tenant %s", q(r), q(string(p.Workspace)), q(string(p.Tenant)))
			}
			for _, role := range p.Roles {
				if _, ok := md.m.Tenancy.Roles[string(role)]; !ok {
					ref("principal %s carries role %s, which tenancy.roles does not declare", q(r), q(string(role)))
				}
			}
		}

		// records
		records := map[string]bool{}
		for _, rec := range t.Fixtures.Records {
			r := string(rec.Ref)
			if records[r] {
				fixture("record reference %s is used twice", q(r))
				continue
			}
			records[r] = true
			switch known, ok := belongs(string(rec.Tenant), string(rec.Workspace)); {
			case !known:
				ref("record %s names tenant %s, which the suite does not declare", q(r), q(string(rec.Tenant)))
			case !ok:
				fixture("record %s is in workspace %s, which is not a workspace of tenant %s", q(r), q(string(rec.Workspace)), q(string(rec.Tenant)))
			}
			if !entityTypes[string(rec.EntityType)] {
				ref("record %s has entity type %s, which no capability declares", q(r), q(string(rec.EntityType)))
			}
		}

		capDeclared := func(id string) bool { _, ok := md.m.Capabilities[id]; return ok }
		principal := func(what, p string) {
			if !principals[p] {
				ref("%s names principal %s, which the suite does not declare", what, q(p))
			}
		}

		// scope probes
		seen := map[string]bool{}
		for _, sp := range t.ScopeProbes {
			id := string(sp.ID)
			what := "scope probe " + q(id)
			if seen[id] {
				dup("scope probe", id)
			}
			seen[id] = true
			principal(what, string(sp.Principal))
			if sp.CapabilityID != nil {
				c := string(*sp.CapabilityID)
				if !capDeclared(c) {
					ref("%s names capability %s, which the manifest does not declare", what, q(c))
				} else {
					scoped[c] = true
				}
			}
			if sp.TargetRecord != nil && !records[string(*sp.TargetRecord)] {
				ref("%s targets record %s, which the suite does not declare", what, q(string(*sp.TargetRecord)))
			}
			if sp.TargetSource != nil {
				s := string(*sp.TargetSource)
				if _, ok := md.m.RAGSources[s]; !ok {
					ref("%s targets source %s, which the manifest does not declare", what, q(s))
				} else if sp.Operation == ap.PackageTestsScopeProbeOperationRetrieve {
					retrieved[s] = true
				}
			}
		}

		// safety probes
		seen = map[string]bool{}
		for _, sp := range t.SafetyProbes {
			id := string(sp.ID)
			what := "safety probe " + q(id)
			if seen[id] {
				dup("safety probe", id)
			}
			seen[id] = true
			principal(what, string(sp.Principal))
			c := string(sp.CapabilityID)
			ce, ok := md.caps[c]
			if !capDeclared(c) {
				ref("%s names capability %s, which the manifest does not declare", what, q(c))
				continue
			}
			if kinds[c] == nil {
				kinds[c] = map[string]bool{}
			}
			kinds[c][string(sp.Kind)] = true
			if ok && int64(ce.c.RiskLevel) == 0 {
				md.c.add(chkProbeExtra, c, path, "%s targets read-only capability %s", what, q(c))
			}
		}

		// conversation cases
		seen = map[string]bool{}
		for _, cc := range t.ConversationCases {
			id := string(cc.ID)
			what := "conversation case " + q(id)
			if seen[id] {
				dup("conversation case", id)
			}
			seen[id] = true
			principal(what, string(cc.Principal))
			if !capDeclared(string(cc.CapabilityID)) {
				ref("%s names capability %s, which the manifest does not declare", what, q(string(cc.CapabilityID)))
			}
			if !md.langs[string(cc.Language)] {
				ref("%s is in language %s, which the manifest does not support", what, q(string(cc.Language)))
			}
		}

		// smoke test
		seen = map[string]bool{}
		for _, sp := range t.Smoke.Paths {
			id := string(sp.ID)
			what := "smoke path " + q(id)
			if seen[id] {
				dup("smoke path", id)
			}
			seen[id] = true
			principal(what, string(sp.Principal))
			if sp.CapabilityID != nil && !capDeclared(string(*sp.CapabilityID)) {
				ref("%s names capability %s, which the manifest does not declare", what, q(string(*sp.CapabilityID)))
			}
		}
	}

	// coverage across suites
	for _, id := range md.capIDs() {
		e := md.caps[id]
		if !scoped[id] {
			md.c.add(chkProbeCapability, id, e.path, "no test suite has a scope probe for capability %s", q(id))
		}
		if int64(e.c.RiskLevel) >= 1 {
			var missing []string
			for _, k := range safetyKinds {
				if !kinds[id][string(k)] {
					missing = append(missing, string(k))
				}
			}
			if len(missing) > 0 {
				md.c.add(chkSafetyProbes, id, e.path, "capability %s has risk level %d but no safety probe of kind %s", q(id), int64(e.c.RiskLevel), qs(missing))
			}
		}
	}
	for _, id := range md.sourceIDs() {
		if !retrieved[id] {
			md.c.add(chkProbeSource, "", md.sources[id].path, "no test suite has a retrieve scope probe that targets source %s", q(id))
		}
	}
}
