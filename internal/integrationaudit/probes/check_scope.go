package probes

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

func scopeCheck(o Options) integrationaudit.Check {
	return integrationaudit.NewCheck(integrationaudit.Spec{
		ID: IDScope, Title: "Exact scope enforcement: positive and negative cross-tenant, workspace, principal and record probes on a disposable tenant",
		Section: integrationaudit.SectionScope, Stage: integrationaudit.StageScope, Target: integrationaudit.TargetApplication,
	}, func(ctx context.Context, in *integrationaudit.Input) (*integrationaudit.Outcome, error) {
		return drive(ctx, o, in, integrationaudit.SectionScope, (*suiteCtx).scope)
	})
}

// scopeCase is one scope probe: declared by the package's suite or derived by the
// audit from what the suite seeded.
type scopeCase struct {
	id        string
	dimension string
	capID     string
	op        string // read, validate, prepare, commit or status
	principal string
	target    string // the reference of the record attempted, or ""
	allow     bool
	code      string // the code a denied attempt must produce, or ""
	derived   bool
}

// scope is master plan 4.5 check 6 for one suite. It executes the suite's scope
// probes against the disposable tenants, positive and negative, and adds the
// cross-scope attempts the suite's records make possible:
//
//   - every request declared allowed must succeed, and its result must not contain
//     the identifier of any record outside the caller's scope;
//   - every request declared denied must be refused with a typed rejection that
//     carries no identifiers and no values, and names no record of another scope.
//     A denied probe passes only if nothing about the unauthorised record reaches
//     the caller, and so the model;
//   - derived: for every capability that takes a record, each principal the suite
//     names for it tries every record of another tenant (and, for a capability
//     scoped to a workspace or a principal, of another workspace); and a principal
//     of another tenant or workspace tries to commit a preparation it does not own
//     and to read the status of a commit it does not own;
//   - the application must answer a record of another scope as it answers one that
//     does not exist (it must not let the caller enumerate identifiers).
//
// Retrieval probes (RAG sources and documents) belong to the RAG probes of M2-12.
func (sc *suiteCtx) scope(ctx context.Context) {
	r := sc.r
	var retrieves []string
	for _, p := range sc.suite.ScopeProbes {
		if p.Operation == ap.PackageTestsScopeProbeOperationRetrieve {
			retrieves = append(retrieves, string(p.ID))
			continue
		}
		k := scopeCase{
			id: string(p.ID), dimension: string(p.Dimension), op: string(p.Operation), principal: string(p.Principal),
			allow: p.Expect == ap.PackageTestsScopeProbeExpectAllow,
		}
		if p.CapabilityID != nil {
			k.capID = string(*p.CapabilityID)
		}
		if p.TargetRecord != nil {
			k.target = string(*p.TargetRecord)
		}
		if p.ErrorCode != nil {
			k.code = string(*p.ErrorCode)
		}
		if sc.down() {
			return
		}
		sc.runCase(ctx, k)
	}
	if sc.down() {
		return
	}
	sc.sweep(ctx)
	if sc.down() {
		return
	}
	sc.foreignCommits(ctx)

	for _, id := range retrieves {
		if !r.in.HasOtherCheck(integrationaudit.SectionScope, IDScope) {
			r.out.Unverify(integrationaudit.Unverified{
				Subject: "scope_probe:" + id, Section: integrationaudit.SectionScope, Target: integrationaudit.TargetApplication,
				Reason: "retrieval scope probes of RAG sources and documents belong to the RAG probes (M2-12), which are not part of this build",
			})
		}
	}
	for _, id := range r.caps {
		if r.ran[id] > 0 {
			r.probed(id)
		} else {
			r.notProbed(id, "no scope probe could be run for the capability")
		}
	}
}

// unverifiedProbe records a probe that could not be run, without failing it.
func (sc *suiteCtx) unverifiedProbe(k scopeCase, reason string) {
	sc.r.out.Unverify(integrationaudit.Unverified{
		Subject: "scope_probe:" + k.id, Capability: k.capID, Section: integrationaudit.SectionScope,
		Target: integrationaudit.TargetApplication, Reason: reason,
	})
}

func (k scopeCase) kind() integrationaudit.ProbeKind {
	if k.allow {
		return integrationaudit.ProbePositiveScope
	}
	return integrationaudit.ProbeNegativeScope
}

func (k scopeCase) subject() string {
	s := fmt.Sprintf("%s: %s %s as %s", k.id, k.op, k.capID, k.principal)
	if k.target != "" {
		s += " on " + k.target
	}
	return s
}

func (k scopeCase) expected() string {
	switch {
	case k.allow:
		return "allowed (ok)"
	case k.code != "":
		return "refused with " + k.code + ", nothing disclosed"
	}
	return "refused, nothing disclosed"
}

// record adds the probe result.
func (sc *suiteCtx) record(k scopeCase, observed string, passed bool) {
	sc.r.out.Probes = append(sc.r.out.Probes, probe(k.kind(), k.capID, k.subject(), k.expected(), observed, passed))
	sc.r.ran[k.capID]++
}

// runCase performs one declared or derived probe in a clean world.
func (sc *suiteCtx) runCase(ctx context.Context, k scopeCase) {
	r := sc.r
	c, ok := r.pkg.Capabilities[k.capID]
	if !ok {
		sc.unverifiedProbe(k, "the package declares no such capability")
		return
	}
	w, ok := sc.world(ctx)
	if !ok {
		return
	}
	scope, p, ok := w.scopeOf(k.principal)
	if !ok {
		sc.unverifiedProbe(k, "the suite names no such principal")
		return
	}
	var target *wRecord
	if k.target != "" {
		if target, ok = w.records[k.target]; !ok {
			sc.unverifiedProbe(k, "the suite names no such record")
			return
		}
	}
	switch k.op {
	case "commit", "status":
		sc.commitSurfaceCase(ctx, w, k, c, scope, p, target)
		return
	}
	if !hasOp(c, k.op) {
		sc.unverifiedProbe(k, "the capability declares no "+k.op+" operation")
		return
	}
	fields, err := w.fieldsFor(c, p, target)
	if err != nil {
		sc.unverifiedProbe(k, err.Error())
		return
	}
	call := func(f map[string]any) (act.DeterministicResult, error) {
		switch k.op {
		case "read":
			return r.h.read(ctx, scope, k.capID, f)
		case "validate":
			return r.h.validate(ctx, scope, k.capID, f)
		}
		return r.h.prepare(ctx, scope, k.capID, f)
	}
	res, err := call(fields)
	verdict := sc.judge(k, c, w, p, target, res, err)
	if verdict == verdictRefused && target != nil && !k.allow {
		sc.oracle(ctx, k, c, w, p, target, res, call)
	}
}

type verdict int

const (
	verdictPass verdict = iota
	verdictFail
	verdictRefused // a denial that held (a pass)
	verdictInconclusive
)

// judge decides a probe from the application's answer and records it. The target
// of a refused request may be named in its errors (the caller supplied it); any
// other identifier of another scope may not.
func (sc *suiteCtx) judge(k scopeCase, c ap.Capability, w *world, p *wPrincipal, target *wRecord, res act.DeterministicResult, err error) verdict {
	r := sc.r
	if err != nil {
		switch {
		case untyped(err):
			r.out.Add(newFinding(fdUntyped, k.capID, "the %s answer of capability %s to scope probe %s is not a typed DeterministicResult", k.op, quote(k.capID), quote(k.id)))
			sc.record(k, "a reply that is not a typed result", false)
			return verdictFail
		case outage(err):
			return verdictInconclusive
		}
		// Any other typed error is the adapter's own: it never reached the
		// application's authorisation, so it proves nothing about scope.
		sc.unverifiedProbe(k, "the call failed with error code "+codeOf(err)+" before the application could answer")
		return verdictInconclusive
	}
	outside := w.outsideScope(p, c.ScopeLevel)
	foreign := allIDs(outside)
	if target != nil {
		foreign = removeString(foreign, target.id)
	}
	if k.allow {
		switch {
		case recorded(res):
			if leaked := mentions(res, foreign); len(leaked) > 0 {
				r.out.Add(newFinding(fdScopeForeignData, k.capID, "the result of allowed probe %s for capability %s contained the identifier of a record of another scope", quote(k.id), quote(k.capID)))
				sc.record(k, "ok, with a record of another scope in the result", false)
				return verdictFail
			}
			sc.record(k, "ok", true)
			return verdictPass
		case refused(res):
			if k.derived {
				sc.unverifiedProbe(k, "the application refused the request the audit built for it (error code "+firstCode(res)+")")
				return verdictInconclusive
			}
			r.out.Add(newFinding(fdScopeAllowRefused, k.capID, "probe %s declares %s allowed to %s %s, and it was refused with %s", quote(k.id), quote(k.principal), k.op, quote(k.capID), firstCode(res)))
			sc.record(k, "refused ("+firstCode(res)+")", false)
			return verdictFail
		}
		sc.unverifiedProbe(k, "the application answered "+string(res.Status)+", neither a success nor a refusal")
		return verdictInconclusive
	}

	// denied
	switch {
	case refused(res):
		observed := "refused (" + firstCode(res) + ")"
		if disclosed(res) || len(mentions(res, foreign)) > 0 {
			r.out.Add(newFinding(fdScopeLeak, k.capID, "the refusal of probe %s for capability %s disclosed data of a record of another scope", quote(k.id), quote(k.capID)))
			sc.record(k, observed+", but it disclosed data", false)
			return verdictFail
		}
		if k.code != "" && firstCode(res) != k.code {
			r.out.Add(newFinding(fdScopeCode, k.capID, "probe %s for capability %s was refused with %s, the probe declares %s", quote(k.id), quote(k.capID), firstCode(res), k.code))
		}
		sc.record(k, observed, true)
		return verdictRefused
	case recorded(res):
		r.out.Add(newFinding(fdScopeAllowed, k.capID, "probe %s: %s was answered %s for capability %s, outside the scope of the record", quote(k.id), k.op, res.Status, quote(k.capID)))
		sc.record(k, "answered "+string(res.Status), false)
		return verdictFail
	}
	sc.unverifiedProbe(k, "the application answered "+string(res.Status)+", so it neither refused nor allowed the request")
	return verdictInconclusive
}

// oracle checks that a record of another scope is refused exactly as a record
// that does not exist is: the code must be the same, or the caller can tell which
// identifiers exist elsewhere.
func (sc *suiteCtx) oracle(ctx context.Context, k scopeCase, c ap.Capability, w *world, p *wPrincipal, target *wRecord, real act.DeterministicResult, call func(map[string]any) (act.DeterministicResult, error)) {
	r := sc.r
	unknown := unknownLike(target.id, w)
	fields, err := w.fieldsFor(c, p, &wRecord{ref: target.ref, entityType: target.entityType, id: unknown})
	if err != nil {
		return
	}
	res, err := call(fields)
	if err != nil || !refused(res) {
		return
	}
	same := firstCode(res) == firstCode(real)
	if !same {
		r.out.Add(newFinding(fdScopeOracle, k.capID, "capability %s answers a record of another scope (probe %s) with %s and a record that does not exist with %s",
			quote(k.capID), quote(k.id), firstCode(real), firstCode(res)))
	}
	r.out.Probes = append(r.out.Probes, probe(integrationaudit.ProbeNegativeScope, k.capID, k.subject()+" (existence)",
		"the same refusal as for a record that does not exist", map[bool]string{true: "the same refusal", false: "a different refusal"}[same], same))
}

// unknownLike returns an identifier shaped like id that no seeded record has:
// the last character is replaced by another of the same class.
func unknownLike(id string, w *world) string {
	known := map[string]bool{}
	for _, rec := range w.records {
		known[rec.id] = true
	}
	r := []rune(id)
	if len(r) == 0 {
		return "unknown"
	}
	for range 20 {
		last := &r[len(r)-1]
		switch {
		case *last >= '0' && *last <= '9':
			*last = '0' + (*last-'0'+1)%10
		case *last >= 'a' && *last <= 'z':
			*last = 'a' + (*last-'a'+1)%26
		case *last >= 'A' && *last <= 'Z':
			*last = 'A' + (*last-'A'+1)%26
		default:
			*last = 'x'
		}
		if cand := string(r); !known[cand] {
			return cand
		}
	}
	return string(r) + "0"
}

// entityTypesOf lists the entity types of a capability's entity_ref fields.
func entityTypesOf(c ap.Capability) map[string]bool {
	out := map[string]bool{}
	for _, f := range c.Fields {
		if f.Type == ap.CapabilityFieldTypeEntityRef && f.EntityType != nil {
			out[string(*f.EntityType)] = true
		}
	}
	return out
}

// firstOp returns the first of the operations the capability declares.
func firstOp(c ap.Capability, ops ...string) string {
	for _, op := range ops {
		if hasOp(c, op) {
			return op
		}
	}
	return ""
}

// sweep derives cross-scope probes from the suite's records: for every capability
// that takes a record, each principal the suite names for it tries every record
// the principal must not reach; and every read capability the declared probes
// never allowed is read once, so its result is scanned for records of other
// scopes.
func (sc *suiteCtx) sweep(ctx context.Context) {
	r := sc.r
	budget := r.opts.maxSweep()
	cut := map[string]bool{}
	// spend takes one probe from the budget. When it is gone, what is left of a
	// capability's derived probes is unverified.
	spend := func(id string) bool {
		if budget > 0 {
			budget--
			return true
		}
		if !cut[id] {
			cut[id] = true
			sc.unverifiedProbe(scopeCase{id: "derived-" + slugOf(id), capID: id},
				fmt.Sprintf("the derived cross-scope probes were cut at the audit's bound (%d probes); the rest were not run", r.opts.maxSweep()))
		}
		return false
	}
	declaredAllow := map[string]bool{}
	for _, p := range sc.suite.ScopeProbes {
		if p.Expect == ap.PackageTestsScopeProbeExpectAllow && p.CapabilityID != nil {
			declaredAllow[string(*p.CapabilityID)] = true
		}
	}
	for _, id := range r.caps {
		if sc.down() {
			return
		}
		c := r.spec(id)
		op := firstOp(c, "read", "validate", "prepare")
		if op == "" {
			continue
		}
		w, ok := sc.world(ctx)
		if !ok {
			return
		}
		var principals []string
		for _, ref := range principalCandidates(sc.suite, id) {
			if _, ok := w.principals[ref]; ok {
				principals = append(principals, ref)
			}
		}
		if len(principals) == 0 {
			sc.unverifiedProbe(scopeCase{id: "derived-" + slugOf(id), capID: id}, "the suite names no fixture principal for the capability, so no cross-scope probe could be derived for it")
			continue
		}
		types := entityTypesOf(c)
		for _, pref := range principals {
			p := w.principals[pref]
			for _, rec := range w.outsideScope(p, c.ScopeLevel) {
				if !types[rec.entityType] {
					continue
				}
				if !spend(id) {
					break
				}
				sc.runCase(ctx, scopeCase{
					id: fmt.Sprintf("derived-%s-%s-%s", slugOf(id), pref, rec.ref), dimension: "record", capID: id, op: op,
					principal: pref, target: rec.ref, allow: false, derived: true,
				})
				if sc.down() {
					return
				}
			}
		}
		if hasOp(c, "read") && !declaredAllow[id] && spend(id) {
			sc.runCase(ctx, scopeCase{
				id: fmt.Sprintf("derived-%s-%s-own-scope", slugOf(id), principals[0]), dimension: "record", capID: id, op: "read",
				principal: principals[0], allow: true, derived: true,
			})
		}
	}
}

func slugOf(capID string) string { return strings.ReplaceAll(capID, ".", "_") }

// ownerOf returns the principal that owns a record's scope: one the suite names
// for the capability in the record's tenant and workspace, else any principal
// there.
func (sc *suiteCtx) ownerOf(w *world, capID string, target *wRecord, fallback string) string {
	if target == nil {
		return fallback
	}
	for _, ref := range principalCandidates(sc.suite, capID) {
		if p, ok := w.principals[ref]; ok && p.tenantRef == target.tenantRef && p.workspaceRef == target.workspaceRef {
			return ref
		}
	}
	for _, ref := range sortedKeys(w.principals) {
		if p := w.principals[ref]; p.tenantRef == target.tenantRef && p.workspaceRef == target.workspaceRef {
			return ref
		}
	}
	return ""
}

// commitSurfaceCase runs a declared commit or status probe: the principal who
// owns the target prepares (and for status, commits) an action, and the acting
// principal then tries to commit it or to read its status.
func (sc *suiteCtx) commitSurfaceCase(ctx context.Context, w *world, k scopeCase, c ap.Capability, actor contracts.Scope, actorP *wPrincipal, target *wRecord) {
	r := sc.r
	if !hasOp(c, "prepare") || !hasOp(c, "commit") || (k.op == "status" && !hasOp(c, "status")) {
		sc.unverifiedProbe(k, "the capability declares no prepare, commit and status operations to probe")
		return
	}
	ownerRef := sc.ownerOf(w, k.capID, target, k.principal)
	if ownerRef == "" {
		sc.unverifiedProbe(k, "no principal of the target's tenant and workspace exists to own the preparation")
		return
	}
	ownerScope, owner, _ := w.scopeOf(ownerRef)
	fields, err := w.fieldsFor(c, owner, target)
	if err != nil {
		sc.unverifiedProbe(k, err.Error())
		return
	}
	g, err := r.rigOf(ctx)
	if err != nil {
		sc.unverifiedProbe(k, err.Error())
		return
	}
	if r.h.down != nil {
		return
	}
	pr, err := g.prepare(ctx, ownerScope, c, fields)
	if err != nil {
		sc.prepareFailed(k, err)
		return
	}
	key := pr.action.IdempotencyKey
	if k.op == "status" {
		if res, err := r.h.commit(ctx, ownerScope, committing(pr.action)); err != nil || !recorded(res) {
			sc.unverifiedProbe(k, "the owner's own commit could not be completed, so there was no outcome to ask about")
			return
		}
		sc.pool.markDirty()
		res, err := r.h.status(ctx, actor, k.capID, key)
		sc.judge(k, c, w, actorP, target, res, err)
		return
	}
	res, err := r.h.commit(ctx, actor, committing(pr.action))
	v := sc.judge(k, c, w, actorP, target, res, err)
	if recorded(res) && err == nil {
		sc.pool.markDirty()
	}
	if !k.allow && v == verdictRefused {
		// nothing may have been applied by the refused attempt, and only the no-record
		// marker shows that nothing was
		st, err := r.h.status(ctx, ownerScope, k.capID, key)
		switch {
		case err == nil && recorded(st):
			r.out.Add(newFinding(fdScopeAllowed, k.capID, "after probe %s was refused, the owner's status for capability %s shows the action applied", quote(k.id), quote(k.capID)))
		case err != nil || !noRecord(st):
			sc.unverifiedProbe(k, "after the attempt was refused, the owner's Status for its key was not the no-record marker, so it could not be shown that nothing was applied")
		}
	}
}

// prepareFailed records why a preparation for a probe could not be made.
func (sc *suiteCtx) prepareFailed(k scopeCase, err error) {
	switch {
	case untyped(err):
		sc.r.out.Add(newFinding(fdUntyped, k.capID, "the prepare answer of capability %s is not a typed DeterministicResult", quote(k.capID)))
	case outage(err):
		sc.r.h.note(err)
	default:
		sc.unverifiedProbe(k, "the owner's preparation failed with error code "+codeOf(err))
	}
}

// foreignCommits is the derived probe of the idempotency and preparation surface,
// which is as much a cross-scope attack surface as a record: for every
// consequential capability, a principal of another tenant (and one of another
// workspace of the same tenant, for a capability that is not scoped to the tenant)
// tries to commit a preparation it does not own, and to read the status of a
// commit it does not own. Neither may change or disclose anything.
func (sc *suiteCtx) foreignCommits(ctx context.Context) {
	r := sc.r
	for _, id := range r.caps {
		if sc.down() {
			return
		}
		c := r.spec(id)
		if !consequential(c) || !hasOp(c, "prepare") || !hasOp(c, "commit") {
			continue
		}
		w, ok := sc.freshWorld(ctx)
		if !ok {
			return
		}
		derived := scopeCase{id: "derived-foreign-commit-" + slugOf(id), capID: id}
		ownerRef := sc.principalFor(w, id)
		if ownerRef == "" {
			sc.unverifiedProbe(derived, "the suite names no fixture principal for the capability to own a preparation")
			continue
		}
		ownerScope, owner, _ := w.scopeOf(ownerRef)
		var actors []string
		for _, ref := range sortedKeys(w.principals) {
			p := w.principals[ref]
			switch {
			case p.tenantRef != owner.tenantRef:
				actors = append(actors, ref)
			case c.ScopeLevel != ap.CapabilityScopeLevelTenant && p.workspaceRef != owner.workspaceRef:
				actors = append(actors, ref)
			}
		}
		if len(actors) == 0 {
			continue
		}
		sort.Strings(actors)
		// one actor of another tenant and one of another workspace are enough
		actors = pickActors(w, owner, actors)
		fields, err := w.fieldsFor(c, owner, nil)
		if err != nil {
			sc.unverifiedProbe(derived, err.Error())
			continue
		}
		g, err := r.rigOf(ctx)
		if err != nil {
			sc.unverifiedProbe(derived, err.Error())
			continue
		}
		if r.h.down != nil {
			continue
		}
		unapplied, err := g.prepare(ctx, ownerScope, c, fields)
		if err != nil {
			sc.prepareFailed(derived, err)
			continue
		}
		for _, aref := range actors {
			actor, _, _ := w.scopeOf(aref)
			k := scopeCase{
				id: fmt.Sprintf("derived-foreign-commit-%s-%s", slugOf(id), aref), dimension: "principal", capID: id, op: "commit",
				principal: aref, allow: false, derived: true,
			}
			res, err := r.h.commit(ctx, actor, committing(unapplied.action))
			if err == nil && recorded(res) {
				sc.pool.markDirty()
			}
			sc.judge(k, c, w, w.principals[aref], nil, res, err)
		}
		// nothing may have been applied by the refused attempts, and only the no-record
		// marker shows that nothing was
		st, err := r.h.status(ctx, ownerScope, id, unapplied.action.IdempotencyKey)
		switch {
		case err == nil && recorded(st):
			r.out.Add(newFinding(fdScopeAllowed, id, "after principals of another scope tried to commit the owner's preparation of capability %s, its status shows the action applied", quote(id)))
		case err != nil || !noRecord(st):
			sc.unverifiedProbe(derived, "after principals of another scope tried to commit the owner's preparation, the owner's Status for its key was not the no-record marker, so it could not be shown that nothing was applied")
		}
		if !hasOp(c, "status") {
			continue
		}
		if r.h.down != nil {
			return
		}
		applied, err := g.prepare(ctx, ownerScope, c, fields)
		if err != nil {
			continue
		}
		if res, err := r.h.commit(ctx, ownerScope, committing(applied.action)); err != nil || !recorded(res) {
			continue
		}
		sc.pool.markDirty()
		for _, aref := range actors {
			actor, _, _ := w.scopeOf(aref)
			k := scopeCase{
				id: fmt.Sprintf("derived-foreign-status-%s-%s", slugOf(id), aref), dimension: "principal", capID: id, op: "status",
				principal: aref, allow: false, derived: true,
			}
			res, err := r.h.status(ctx, actor, id, applied.action.IdempotencyKey)
			sc.judge(k, c, w, w.principals[aref], nil, res, err)
		}
	}
}

// pickActors keeps the first actor of another tenant and the first of another
// workspace of the owner's tenant.
func pickActors(w *world, owner *wPrincipal, actors []string) []string {
	var out []string
	otherTenant, otherWorkspace := false, false
	for _, ref := range actors {
		p := w.principals[ref]
		switch {
		case p.tenantRef != owner.tenantRef && !otherTenant:
			otherTenant = true
			out = append(out, ref)
		case p.tenantRef == owner.tenantRef && !otherWorkspace:
			otherWorkspace = true
			out = append(out, ref)
		}
	}
	return out
}
