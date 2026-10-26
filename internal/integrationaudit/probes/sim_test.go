package probes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// This file is the test host of the probes: a small notes application with a
// disposable-tenant provisioner, written to be correct by default and to break in
// exactly one chosen way (a knob). Every probe is proved to catch the failure it
// exists for by turning one knob and watching it fail, and to leave a correct
// host alone by leaving them all off.
//
// It speaks raw JSON replies and runs them through adapters.Normalise, as a real
// transport does, so the probes see what they would see through a real adapter,
// and it is put behind adapters.Wrap in the tests, as production wires it.

const (
	simAppID  = "notes-app"
	capGet    = "notes.get"
	capList   = "notes.list"
	capUpdate = "notes.update"
	capPin    = "notes.pin"
	entNote   = "note"
)

var simT0 = time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)

// simKnobs are the ways the host can be made to misbehave.
type simKnobs struct {
	nonIdempotent     bool // a commit is applied every time it is received
	noExpiry          bool // prepare does not say how long it honours the preparation
	longExpiry        bool // prepare promises longer than the capability declares
	noExpiryEnforce   bool // commit ignores that the preparation expired
	ignoreStale       bool // commit ignores that the data version changed
	leakTenant        bool // notes.get finds notes of other tenants
	leakList          bool // notes.list lists the notes of every workspace and tenant
	disclose          bool // a refusal carries the identifier of the record
	oracle            bool // a record of another scope is refused differently from one that does not exist
	genericSuccess    bool // validate and prepare answer ok to an empty text
	prose             bool // update's prepare answers in prose
	etagVersion       bool // validate reports a version that is new on every response
	blindVersion      bool // validate reports one version whatever changed
	dupVersions       bool // prepare gives one record two data versions
	okNoFreshness     bool // notes.get answers ok without freshness
	genericBlob       bool // validate answers a bare {status: ok, message} object
	faultAnswersOK    bool // a dependency that is down is answered as if it were up
	lostReplyNoRecord bool // a commit whose reply was lost leaves no record
	commitUnbound     bool // a commit result names a record the preparation does not target
	unstableRead      bool // a read lists its values in another order each time
	statusOKUnknown   bool // status of a key never seen answers ok
	statusOddUnknown  bool // status of a key never seen is rejected, but not as the marker
	statusCrossTenant bool // status finds the commit of another tenant
	statusLies        bool // status reports a different outcome than the commit did
	statusUnavailable bool // status of a key it has a record of answers unavailable
	cancelReverses    bool // cancel undoes an applied update
	cancelThenCommit  bool // cancel is accepted but the key can still be committed
	successNoRecord   bool // commit answers ok and records nothing
	leftBehind        bool // cleanup leaves notes behind and reports them
	staysAfterCleanup bool // cleanup reports an empty tenant but the tenant is still there
	commitForeign     bool // commit does not check the tenant of the prepared record
	unauthorised      bool // every request is answered ok whoever asks
	downAfter         int  // after this many calls every call fails as unavailable (0: never)
}

type simNote struct {
	id        string
	workspace string
	text      string
	version   int
}

type simPrincipal struct {
	workspace string
	roles     []string
}

type simTenant struct {
	workspaces map[string]bool
	principals map[string]simPrincipal
	notes      map[string]*simNote
}

// sim is the notes application.
type sim struct {
	mu       sync.Mutex
	clk      *clock.Fake
	k        simKnobs
	tenants  map[string]*simTenant
	commits  map[string]map[string]any // tenant|key -> the host members of the reply
	prior    map[string]*simNote       // tenant|key -> the note as it was before the update
	faults   []Fault
	nextNote int
	nextETag int
	reads    int
	calls    int
	effects  int
	created  int
	cleaned  int
	prepares int
}

func newSim(k ...func(*simKnobs)) *sim {
	s := &sim{
		clk: clock.NewFake(simT0), tenants: map[string]*simTenant{}, commits: map[string]map[string]any{}, prior: map[string]*simNote{},
	}
	for _, f := range k {
		f(&s.k)
	}
	return s
}

// ---- the contract ----

func (s *sim) body(op adapters.Operation, capID, key string, m map[string]any) (act.DeterministicResult, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return act.DeterministicResult{}, err
	}
	return adapters.Normalise(raw, adapters.Expect{Operation: op, CapabilityID: capID, IdempotencyKey: key})
}

func rejected(code, field, msg string) map[string]any {
	issue := map[string]any{"code": code, "message": msg}
	if field != "" {
		issue["field"] = field
	}
	return map[string]any{
		"status": "rejected", "identifiers": []any{}, "values": []any{}, "errors": []any{issue}, "allowed_next_actions": []any{},
	}
}

func unavailableBody() map[string]any {
	return map[string]any{
		"status": "unavailable", "identifiers": []any{}, "values": []any{},
		"errors":               []any{map[string]any{"code": "unavailable", "message": "a dependency is down"}},
		"allowed_next_actions": []any{map[string]any{"action": "retry"}},
	}
}

func textFact(field, v string) map[string]any {
	return map[string]any{"field": field, "value": map[string]any{"kind": "text", "value": v}}
}

func rec(entity, id string) map[string]any {
	return map[string]any{"entity_type": entity, "entity_id": id}
}

func (s *sim) fresh(version string) map[string]any {
	return map[string]any{"as_of": s.clk.Now().UTC().Format("2006-01-02T15:04:05Z"), "version": version}
}

func okBody(fresh map[string]any) map[string]any {
	return map[string]any{
		"status": "ok", "identifiers": []any{}, "values": []any{}, "errors": []any{}, "allowed_next_actions": []any{},
		"freshness": fresh,
	}
}

// pre runs what every call starts with: counting, the outage knob, an armed fault.
func (s *sim) pre(op adapters.Operation, capID string) (fault FaultKind, down error) {
	s.calls++
	if s.k.downAfter > 0 && s.calls > s.k.downAfter {
		return "", contracts.New(contracts.ErrUnavailable, "the application is down")
	}
	for i, f := range s.faults {
		if f.Operation == op && (f.CapabilityID == "" || f.CapabilityID == capID) {
			s.faults = append(s.faults[:i], s.faults[i+1:]...)
			if f.Kind == FaultDependencyDown && s.k.faultAnswersOK {
				return "", nil
			}
			return f.Kind, nil
		}
	}
	return "", nil
}

// authz resolves the caller: the tenant and principal as the host recorded them.
func (s *sim) authz(sc contracts.Scope) (*simTenant, simPrincipal, map[string]any) {
	if s.k.unauthorised {
		t := s.tenants[sc.TenantID]
		if t == nil {
			t = &simTenant{notes: map[string]*simNote{}}
		}
		return t, simPrincipal{workspace: sc.WorkspaceID, roles: []string{"editor"}}, nil
	}
	t := s.tenants[sc.TenantID]
	if t == nil {
		return nil, simPrincipal{}, rejected("scope_violation", "", "outside the caller's scope")
	}
	p, ok := t.principals[sc.PrincipalID]
	if !ok || p.workspace != sc.WorkspaceID {
		return nil, simPrincipal{}, rejected("scope_violation", "", "outside the caller's scope")
	}
	return t, p, nil
}

func hasRole(p simPrincipal, role string) bool {
	for _, r := range p.roles {
		if r == role {
			return true
		}
	}
	return false
}

// note finds a note in the caller's scope; the knobs say how scope is broken.
func (s *sim) note(t *simTenant, sc contracts.Scope, id string) *simNote {
	if n, ok := t.notes[id]; ok && n.workspace == sc.WorkspaceID {
		return n
	}
	if s.k.leakTenant {
		for _, other := range s.tenants {
			if n, ok := other.notes[id]; ok {
				return n
			}
		}
	}
	return nil
}

func (s *sim) scopeRefusal(id string) map[string]any {
	b := rejected("scope_violation", "note", "the note is outside the caller's scope or does not exist")
	if s.k.disclose {
		b["identifiers"] = []any{rec(entNote, id)}
	}
	return b
}

func (s *sim) exists(id string) bool {
	for _, t := range s.tenants {
		if _, ok := t.notes[id]; ok {
			return true
		}
	}
	return false
}

func (s *sim) refusal(id string) map[string]any {
	if s.k.oracle && s.exists(id) {
		return rejected("permission_denied", "note", "the note exists but is not yours")
	}
	return s.scopeRefusal(id)
}

func stringField(f map[string]any, name string) (string, bool) {
	v, ok := f[name]
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	return str, ok
}

func (s *sim) Read(_ context.Context, sc contracts.Scope, r adapters.ReadRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fault, down := s.pre(adapters.OpRead, r.CapabilityID)
	if down != nil {
		return act.DeterministicResult{}, down
	}
	if fault == FaultDependencyDown {
		return s.body(adapters.OpRead, r.CapabilityID, "", unavailableBody())
	}
	t, _, rej := s.authz(sc)
	if rej != nil {
		return s.body(adapters.OpRead, r.CapabilityID, "", rej)
	}
	switch r.CapabilityID {
	case capGet:
		id, present := stringField(r.Fields, "note")
		if !present || id == "" {
			return s.body(adapters.OpRead, r.CapabilityID, "", rejected("missing_fields", "note", "note is required"))
		}
		n := s.note(t, sc, id)
		if n == nil {
			return s.body(adapters.OpRead, r.CapabilityID, "", s.refusal(id))
		}
		b := okBody(s.fresh(fmt.Sprintf("n%d", n.version)))
		b["identifiers"] = []any{rec(entNote, n.id)}
		b["values"] = []any{textFact("text", n.text)}
		if s.k.okNoFreshness {
			delete(b, "freshness")
		}
		return s.body(adapters.OpRead, r.CapabilityID, "", b)
	case capList:
		s.reads++
		var ids []string
		visible := func(tt *simTenant, own bool) {
			for id, n := range tt.notes {
				if !own || n.workspace == sc.WorkspaceID {
					ids = append(ids, id)
				}
			}
		}
		if s.k.leakList {
			for _, tt := range s.tenants {
				visible(tt, false)
			}
		} else {
			visible(t, true)
		}
		sort.Strings(ids)
		if s.k.unstableRead && s.reads%2 == 0 {
			for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
		b := okBody(s.fresh(fmt.Sprintf("l%d", len(ids))))
		refs := make([]any, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, rec(entNote, id))
		}
		b["identifiers"] = refs
		return s.body(adapters.OpRead, r.CapabilityID, "", b)
	}
	return s.body(adapters.OpRead, r.CapabilityID, "", rejected("unknown_capability", "", "no such operation"))
}

// updateFields validates the fields of notes.update and notes.pin and returns the note.
func (s *sim) updateFields(t *simTenant, p simPrincipal, sc contracts.Scope, capID string, f map[string]any) (*simNote, string, map[string]any) {
	if !hasRole(p, "editor") && !s.k.unauthorised {
		return nil, "", rejected("permission_denied", "", "the role may not change notes")
	}
	id, present := stringField(f, "note")
	if !present || id == "" {
		return nil, "", rejected("missing_fields", "note", "note is required")
	}
	text := ""
	if capID == capUpdate {
		var ok bool
		if text, ok = stringField(f, "text"); !ok {
			return nil, "", rejected("missing_fields", "text", "text is required")
		}
		if text == "" || len(text) > 200 {
			if s.k.genericSuccess {
				return nil, "", nil // answered ok below, as if nothing was wrong
			}
			return nil, "", rejected("validation_failed", "text", "text must be 1 to 200 characters")
		}
	}
	n := s.note(t, sc, id)
	if n == nil {
		return nil, "", s.refusal(id)
	}
	return n, text, nil
}

func (s *sim) Validate(_ context.Context, sc contracts.Scope, r adapters.ValidateRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fault, down := s.pre(adapters.OpValidate, r.CapabilityID)
	if down != nil {
		return act.DeterministicResult{}, down
	}
	if fault == FaultDependencyDown {
		return s.body(adapters.OpValidate, r.CapabilityID, "", unavailableBody())
	}
	if s.k.genericBlob && r.CapabilityID == capUpdate {
		return adapters.Normalise([]byte(`{"status":"ok","message":"Done! Everything went fine."}`), adapters.Expect{Operation: adapters.OpValidate, CapabilityID: r.CapabilityID})
	}
	t, p, rej := s.authz(sc)
	if rej != nil {
		return s.body(adapters.OpValidate, r.CapabilityID, "", rej)
	}
	n, _, rej := s.updateFields(t, p, sc, r.CapabilityID, r.Fields)
	if rej != nil {
		return s.body(adapters.OpValidate, r.CapabilityID, "", rej)
	}
	if n == nil { // genericSuccess
		return s.body(adapters.OpValidate, r.CapabilityID, "", okBody(s.fresh("n1")))
	}
	ver := fmt.Sprintf("n%d", n.version)
	switch {
	case s.k.etagVersion:
		s.nextETag++
		ver = fmt.Sprintf("etag-%d", s.nextETag)
	case s.k.blindVersion:
		ver = "n1"
	}
	return s.body(adapters.OpValidate, r.CapabilityID, "", okBody(s.fresh(ver)))
}

func (s *sim) Prepare(_ context.Context, sc contracts.Scope, r adapters.PrepareRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fault, down := s.pre(adapters.OpPrepare, r.CapabilityID)
	if down != nil {
		return act.DeterministicResult{}, down
	}
	if fault == FaultDependencyDown {
		return s.body(adapters.OpPrepare, r.CapabilityID, "", unavailableBody())
	}
	s.prepares++
	if s.k.prose && r.CapabilityID == capUpdate {
		return adapters.Normalise([]byte("Done! Everything went fine and your note was prepared."), adapters.Expect{Operation: adapters.OpPrepare, CapabilityID: r.CapabilityID})
	}
	t, p, rej := s.authz(sc)
	if rej != nil {
		return s.body(adapters.OpPrepare, r.CapabilityID, "", rej)
	}
	n, text, rej := s.updateFields(t, p, sc, r.CapabilityID, r.Fields)
	if rej != nil {
		return s.body(adapters.OpPrepare, r.CapabilityID, "", rej)
	}
	if n == nil { // genericSuccess: nothing is prepared, yet it says ok
		return s.body(adapters.OpPrepare, r.CapabilityID, "", okBody(s.fresh("n1")))
	}
	ver := fmt.Sprintf("n%d", n.version)
	b := okBody(s.fresh(ver))
	b["values"] = []any{textFact("text", text)}
	b["allowed_next_actions"] = []any{map[string]any{"action": "confirm"}, map[string]any{"action": "cancel"}}
	prep := map[string]any{
		"summary": "Change the text of a note",
		"effects": []any{map[string]any{
			"operation": "update", "entity_type": entNote, "entity_id": n.id,
			"description": "Replace the text of the note.", "changes": []any{textFact("text", text)}, "external": false,
		}},
		"data_versions":       []any{map[string]any{"entity_type": entNote, "entity_id": n.id, "version": ver}},
		"declared_risk_level": 2,
	}
	if s.k.dupVersions {
		prep["data_versions"] = []any{
			map[string]any{"entity_type": entNote, "entity_id": n.id, "version": ver},
			map[string]any{"entity_type": entNote, "entity_id": n.id, "version": ver + "x"},
		}
	}
	switch {
	case s.k.noExpiry:
	case s.k.longExpiry:
		prep["valid_for_seconds"] = 3600
	default:
		prep["valid_for_seconds"] = 300
	}
	b["preparation"] = prep
	return s.body(adapters.OpPrepare, r.CapabilityID, "", b)
}

func (s *sim) Commit(_ context.Context, sc contracts.Scope, r adapters.CommitRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(r.IdempotencyKey)
	fault, down := s.pre(adapters.OpCommit, r.CapabilityID)
	if down != nil {
		return act.DeterministicResult{}, down
	}
	if fault == FaultDependencyDown {
		return s.body(adapters.OpCommit, r.CapabilityID, key, unavailableBody())
	}
	t, p, rej := s.authz(sc)
	if rej != nil {
		return s.body(adapters.OpCommit, r.CapabilityID, key, rej)
	}
	if !hasRole(p, "editor") && !s.k.unauthorised {
		return s.body(adapters.OpCommit, r.CapabilityID, key, rejected("permission_denied", "", "the role may not change notes"))
	}
	ck := sc.TenantID + "|" + sc.WorkspaceID + "|" + key
	if prev, seen := s.commits[ck]; seen && !s.k.nonIdempotent {
		return s.body(adapters.OpCommit, r.CapabilityID, key, prev)
	}
	var noteID, text, dvVersion string
	for _, e := range r.Prepared.Effects {
		if e.EntityID != nil {
			noteID = string(*e.EntityID)
		}
		for _, ch := range e.Changes {
			if tv, ok := ch.Value.Value.(act.TextValue); ok && ch.Field == "text" {
				text = tv.Value
			}
		}
	}
	for _, dv := range r.Prepared.DataVersions {
		dvVersion = string(dv.Version)
	}
	var n *simNote
	if s.k.commitForeign {
		for _, tt := range s.tenants {
			if x, ok := tt.notes[noteID]; ok {
				n = x
			}
		}
	} else {
		n = s.note(t, sc, noteID)
	}
	if n == nil {
		return s.body(adapters.OpCommit, r.CapabilityID, key, s.refusal(noteID))
	}
	if !s.k.noExpiryEnforce {
		exp, err := time.Parse(time.RFC3339, string(r.Prepared.ExpiresAt))
		if err == nil && !s.clk.Now().Before(exp) {
			return s.body(adapters.OpCommit, r.CapabilityID, key, rejected("expired", "prepared", "the preparation expired"))
		}
	}
	if !s.k.ignoreStale && !s.k.nonIdempotent && dvVersion != fmt.Sprintf("n%d", n.version) {
		return s.body(adapters.OpCommit, r.CapabilityID, key, rejected("stale", "", "the note changed since it was prepared"))
	}
	if s.k.successNoRecord {
		b := okBody(s.fresh(fmt.Sprintf("n%d", n.version)))
		b["identifiers"] = []any{rec(entNote, n.id)}
		b["values"] = []any{textFact("text", text)}
		return s.body(adapters.OpCommit, r.CapabilityID, key, b)
	}
	s.prior[ck] = &simNote{id: n.id, workspace: n.workspace, text: n.text, version: n.version}
	n.text = text
	n.version++
	s.effects++
	b := okBody(s.fresh(fmt.Sprintf("n%d", n.version)))
	b["identifiers"] = []any{rec(entNote, n.id)}
	if s.k.commitUnbound {
		b["identifiers"] = []any{rec(entNote, "note_not_in_the_preparation")}
	}
	b["values"] = []any{textFact("text", text)}
	s.commits[ck] = b
	if fault == FaultReplyLost {
		if s.k.lostReplyNoRecord {
			delete(s.commits, ck)
		}
		return act.DeterministicResult{}, adapters.UncertainOutcome(errors.New("the reply was lost"))
	}
	return s.body(adapters.OpCommit, r.CapabilityID, key, b)
}

func (s *sim) Status(_ context.Context, sc contracts.Scope, r adapters.StatusRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(r.IdempotencyKey)
	fault, down := s.pre(adapters.OpStatus, r.CapabilityID)
	if down != nil {
		return act.DeterministicResult{}, down
	}
	if fault == FaultDependencyDown {
		return s.body(adapters.OpStatus, r.CapabilityID, key, unavailableBody())
	}
	if _, _, rej := s.authz(sc); rej != nil {
		return s.body(adapters.OpStatus, r.CapabilityID, key, rej)
	}
	if b, ok := s.commits[sc.TenantID+"|"+sc.WorkspaceID+"|"+key]; ok {
		if s.k.statusUnavailable {
			return s.body(adapters.OpStatus, r.CapabilityID, key, unavailableBody())
		}
		if s.k.statusLies {
			lie := okBody(s.fresh("n999"))
			lie["identifiers"] = []any{rec(entNote, "note_elsewhere")}
			return s.body(adapters.OpStatus, r.CapabilityID, key, lie)
		}
		return s.body(adapters.OpStatus, r.CapabilityID, key, b)
	}
	if s.k.statusCrossTenant {
		for ck, b := range s.commits {
			if strings.HasSuffix(ck, "|"+key) {
				return s.body(adapters.OpStatus, r.CapabilityID, key, b)
			}
		}
	}
	switch {
	case s.k.statusOKUnknown:
		return s.body(adapters.OpStatus, r.CapabilityID, key, okBody(s.fresh("n1")))
	case s.k.statusOddUnknown:
		return s.body(adapters.OpStatus, r.CapabilityID, key, rejected("validation_failed", "other_field", "unknown"))
	}
	return s.body(adapters.OpStatus, r.CapabilityID, key, rejected("validation_failed", "idempotency_key", "the application has no record of this key"))
}

func (s *sim) Cancel(_ context.Context, sc contracts.Scope, r adapters.CancelRequest) (act.DeterministicResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(r.IdempotencyKey)
	if _, down := s.pre(adapters.OpCancel, r.CapabilityID); down != nil {
		return act.DeterministicResult{}, down
	}
	t, _, rej := s.authz(sc)
	if rej != nil {
		return s.body(adapters.OpCancel, r.CapabilityID, key, rej)
	}
	ck := sc.TenantID + "|" + sc.WorkspaceID + "|" + key
	if _, applied := s.commits[ck]; applied {
		if s.k.cancelReverses {
			if prev := s.prior[ck]; prev != nil {
				if n := t.notes[prev.id]; n != nil {
					n.text, n.version = prev.text, prev.version
				}
			}
			delete(s.commits, ck)
			return s.body(adapters.OpCancel, r.CapabilityID, key, okBody(s.fresh("n0")))
		}
		return s.body(adapters.OpCancel, r.CapabilityID, key, rejected("conflict", "", "the action was already applied"))
	}
	if s.k.cancelThenCommit {
		return s.body(adapters.OpCancel, r.CapabilityID, key, okBody(s.fresh("n0")))
	}
	return s.body(adapters.OpCancel, r.CapabilityID, key, rejected("validation_failed", "idempotency_key", "nothing is pending for this key"))
}

func (s *sim) Describe(context.Context) (adapters.Description, error) {
	return adapters.Description{
		Transport: "fake", ContractVersions: []int{adapters.ContractVersion}, ApplicationVersion: "1.0.0",
		Capabilities: []adapters.CapabilityOps{
			{CapabilityID: capGet, Operations: []adapters.Operation{adapters.OpRead}},
			{CapabilityID: capList, Operations: []adapters.Operation{adapters.OpRead}},
			{CapabilityID: capUpdate, Operations: []adapters.Operation{adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus, adapters.OpCancel}},
		},
	}, nil
}

func (s *sim) Health(context.Context) (adapters.HealthReport, error) {
	return adapters.HealthReport{Status: adapters.HealthOK}, nil
}

func (s *sim) Inspect(_ context.Context, _ contracts.Scope, req adapters.InspectRequest) (adapters.Inspection, error) {
	out := adapters.Inspection{}
	aspects := req.Aspects
	if len(aspects) == 0 {
		aspects = adapters.AllAspects()
	}
	for _, a := range aspects {
		out.Reports = append(out.Reports, adapters.AspectReport{Aspect: a, State: adapters.AspectUnsupported, Facts: []act.Fact{}})
	}
	return out, nil
}

// ---- provisioning ----

func (s *sim) CreateTenant(_ context.Context, t Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[t.ID]; ok {
		return contracts.New(contracts.ErrConflict, "tenant exists")
	}
	tt := &simTenant{workspaces: map[string]bool{}, principals: map[string]simPrincipal{}, notes: map[string]*simNote{}}
	for _, w := range t.Workspaces {
		tt.workspaces[w] = true
	}
	for _, p := range t.Principals {
		tt.principals[p.ID] = simPrincipal{workspace: p.WorkspaceID, roles: append([]string(nil), p.Roles...)}
	}
	s.tenants[t.ID] = tt
	s.created++
	return nil
}

func (s *sim) Seed(_ context.Context, sd Seed) (SeedResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tenants[sd.TenantID]
	if t == nil {
		return SeedResult{}, contracts.New(contracts.ErrScopeViolation, "no such tenant")
	}
	res := SeedResult{Entities: map[string]string{}}
	for _, r := range sd.Records {
		s.nextNote++
		id := fmt.Sprintf("note_%d", s.nextNote)
		t.notes[id] = &simNote{id: id, workspace: r.WorkspaceID, text: "synthetic " + r.Ref, version: 1}
		res.Entities[r.Ref] = id
	}
	return res, nil
}

func (s *sim) Cleanup(_ context.Context, tenantID string) (CleanupReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tenants[tenantID]
	if t == nil {
		return CleanupReport{Existed: false, Remaining: map[string]int{}}, nil
	}
	s.cleaned++
	if s.k.leftBehind {
		return CleanupReport{Existed: true, Remaining: map[string]int{"notes": len(t.notes)}}, nil
	}
	if s.k.staysAfterCleanup {
		return CleanupReport{Existed: true, Remaining: map[string]int{}}, nil
	}
	delete(s.tenants, tenantID)
	for ck := range s.commits {
		if strings.HasPrefix(ck, tenantID+"|") {
			delete(s.commits, ck)
		}
	}
	return CleanupReport{Existed: true, Remaining: map[string]int{}}, nil
}

// ChangeRecord implements RecordChanger: the note changes, so its version moves.
func (s *sim) ChangeRecord(_ context.Context, tenantID, entityType, entityID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tenants[tenantID]
	if t == nil || entityType != entNote || t.notes[entityID] == nil {
		return contracts.New(contracts.ErrScopeViolation, "no such record")
	}
	t.notes[entityID].version++
	return nil
}

// InjectFault implements FaultInjector.
func (s *sim) InjectFault(_ context.Context, f Fault) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, f)
	return nil
}

// ---- observation ----

func (s *sim) effectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effects
}

func (s *sim) tenantCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tenants)
}

func (s *sim) noteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.tenants {
		n += len(t.notes)
	}
	return n
}

// ---- the package ----

func ptr[T any](v T) *T { return &v }

func failure(codes ...string) []ap.CapabilityFailureState {
	out := make([]ap.CapabilityFailureState, 0, len(codes))
	for _, c := range codes {
		out = append(out, ap.CapabilityFailureState{Code: ap.ErrorCode(c), Description: "documented"})
	}
	return out
}

func binding(tool string) *ap.CapabilityOperationBinding {
	return &ap.CapabilityOperationBinding{Endpoint: "sim", MCPTool: ptr(ap.OperationName(tool))}
}

func noteField() ap.CapabilityField {
	return ap.CapabilityField{Name: "note", Type: ap.CapabilityFieldTypeEntityRef, Required: true, EntityType: ptr(ap.Slug(entNote)), Description: "the note"}
}

func simCapabilities() map[string]ap.Capability {
	read := func(id string, fields ...ap.CapabilityField) ap.Capability {
		return ap.Capability{
			ID: ap.CapabilityID(id), Audience: ap.CapabilityAudienceUser, ScopeLevel: ap.CapabilityScopeLevelWorkspace,
			Fields: fields, EffectClass: ap.CapabilityEffectClassNone, RiskLevel: 0,
			Operations:    ap.CapabilityOperations{Read: binding(strings.ReplaceAll(id, ".", "_"))},
			Confirmation:  ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeNone},
			FailureStates: failure("scope_violation", "permission_denied", "missing_fields", "validation_failed", "unavailable"),
		}
	}
	update := ap.Capability{
		ID: capUpdate, Audience: ap.CapabilityAudienceUser, ScopeLevel: ap.CapabilityScopeLevelWorkspace,
		Fields: []ap.CapabilityField{
			noteField(),
			{Name: "text", Type: ap.CapabilityFieldTypeString, Required: true, Description: "the new text", MaxLength: ptr(int64(200))},
		},
		EffectClass: ap.CapabilityEffectClassReversible, RiskLevel: 2,
		Operations: ap.CapabilityOperations{
			Validate: binding("notes_update_validate"), Prepare: binding("notes_update_prepare"),
			Commit: binding("notes_update_commit"), Status: binding("notes_update_status"),
		},
		Confirmation:   ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeExplicit},
		PreparedAction: &ap.CapabilityPreparedAction{TTLSeconds: 300, InvalidatedBy: []ap.CapabilityPreparedActionInvalidatedByItem{ap.CapabilityPreparedActionInvalidatedByItemRecordRevision}},
		Idempotency: &ap.CapabilityIdempotency{
			KeySource: ap.CapabilityIdempotencyKeySourcePreparedActionID, WindowSeconds: 86400, OnReplay: ap.CapabilityIdempotencyOnReplayReturnOriginalReceipt,
		},
		Examples: []ap.CapabilityExample{{
			ID: "en-update", Language: "en", TestOnly: true, Utterance: "Change the note",
			ExpectedFields: ap.ExampleFields{"text": "an updated synthetic text"},
		}},
		FailureStates: failure("scope_violation", "permission_denied", "missing_fields", "validation_failed", "stale", "expired",
			"conflict", "unavailable", "uncertain_outcome"),
	}
	return map[string]ap.Capability{
		capGet:    read(capGet, noteField()),
		capList:   read(capList),
		capUpdate: update,
	}
}

func simPolicy(t testing.TB) *policy.HostPolicy {
	t.Helper()
	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "editor", "capabilities": ["notes.get", "notes.list", "notes.update"] },
    { "role": "viewer", "capabilities": ["notes.get", "notes.list"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		t.Fatal(err)
	}
	return hp
}

func provCall(tool string) ap.AdapterCall {
	return ap.AdapterCall{Endpoint: "sim", MCPTool: ptr(ap.OperationName(tool))}
}

func simSuite() ap.PackageTests {
	allowRead := func(id, capID, principal, target string) ap.PackageTestsScopeProbe {
		p := ap.PackageTestsScopeProbe{
			ID: ap.Slug(id), Dimension: ap.PackageTestsScopeProbeDimensionRecord, Operation: ap.PackageTestsScopeProbeOperationRead,
			CapabilityID: ptr(ap.CapabilityID(capID)), Principal: ap.Slug(principal), Expect: ap.PackageTestsScopeProbeExpectAllow,
		}
		if target != "" {
			p.TargetRecord = ptr(ap.Slug(target))
		}
		return p
	}
	deny := func(id string, dim ap.PackageTestsScopeProbeDimension, op ap.PackageTestsScopeProbeOperation, capID, principal, target, code string) ap.PackageTestsScopeProbe {
		return ap.PackageTestsScopeProbe{
			ID: ap.Slug(id), Dimension: dim, Operation: op, CapabilityID: ptr(ap.CapabilityID(capID)), Principal: ap.Slug(principal),
			TargetRecord: ptr(ap.Slug(target)), Expect: ap.PackageTestsScopeProbeExpectDeny, ErrorCode: ptr(ap.ErrorCode(code)),
		}
	}
	safety := func(id string, kind ap.PackageTestsSafetyProbeKind, out ap.PackageTestsSafetyExpectOutcome, code string) ap.PackageTestsSafetyProbe {
		p := ap.PackageTestsSafetyProbe{
			ID: ap.Slug(id), CapabilityID: capUpdate, Principal: "editor-a1", Kind: kind,
			Expect: ap.PackageTestsSafetyExpect{Outcome: out},
		}
		if code != "" {
			p.Expect.ErrorCode = ptr(ap.ErrorCode(code))
		}
		return p
	}
	return ap.PackageTests{
		Suite: "sim-safety", Synthetic: true,
		Fixtures: ap.PackageTestsFixtures{
			Provisioning: ap.PackageTestsProvisioning{
				CreateTenant: provCall("test_create_tenant"), Seed: provCall("test_seed"), Cleanup: provCall("test_cleanup"),
				SeedData: ptr("tests/seed/notes.json"),
			},
			Tenants: []ap.PackageTestsFixturesTenantsItem{
				{Ref: "org-a", Workspaces: []ap.Slug{"ws-1", "ws-2"}},
				{Ref: "org-b", Workspaces: []ap.Slug{"ws-1"}},
			},
			Principals: []ap.PackageTestsPrincipal{
				principal("editor-a1", "org-a", "ws-1", "editor"),
				principal("viewer-a1", "org-a", "ws-1", "viewer"),
				principal("editor-a2", "org-a", "ws-2", "editor"),
				principal("editor-b1", "org-b", "ws-1", "editor"),
			},
			Records: []ap.PackageTestsRecord{
				{Ref: "note-a1", EntityType: entNote, Tenant: "org-a", Workspace: "ws-1"},
				{Ref: "note-a1b", EntityType: entNote, Tenant: "org-a", Workspace: "ws-1"},
				{Ref: "note-a2", EntityType: entNote, Tenant: "org-a", Workspace: "ws-2"},
				{Ref: "note-b1", EntityType: entNote, Tenant: "org-b", Workspace: "ws-1"},
			},
		},
		ScopeProbes: []ap.PackageTestsScopeProbe{
			allowRead("editor-reads-own-note", capGet, "editor-a1", "note-a1"),
			allowRead("viewer-lists-notes", capList, "viewer-a1", ""),
			deny("editor-cannot-read-other-workspace", ap.PackageTestsScopeProbeDimensionWorkspace, ap.PackageTestsScopeProbeOperationRead, capGet, "editor-a1", "note-a2", "scope_violation"),
			deny("editor-cannot-read-other-tenant", ap.PackageTestsScopeProbeDimensionTenant, ap.PackageTestsScopeProbeOperationRead, capGet, "editor-a1", "note-b1", "scope_violation"),
			deny("editor-cannot-prepare-other-tenant", ap.PackageTestsScopeProbeDimensionTenant, ap.PackageTestsScopeProbeOperationPrepare, capUpdate, "editor-a1", "note-b1", "scope_violation"),
			deny("viewer-cannot-prepare-update", ap.PackageTestsScopeProbeDimensionRole, ap.PackageTestsScopeProbeOperationPrepare, capUpdate, "viewer-a1", "note-a1", "permission_denied"),
			{
				ID: "editor-prepares-own-note", Dimension: ap.PackageTestsScopeProbeDimensionRole, Operation: ap.PackageTestsScopeProbeOperationPrepare,
				CapabilityID: ptr(ap.CapabilityID(capUpdate)), Principal: "editor-a1", TargetRecord: ptr(ap.Slug("note-a1")), Expect: ap.PackageTestsScopeProbeExpectAllow,
			},
			{
				ID: "editor-retrieves-policies", Dimension: ap.PackageTestsScopeProbeDimensionSource, Operation: ap.PackageTestsScopeProbeOperationRetrieve,
				Principal: "editor-a1", TargetSource: ptr(ap.Slug("note-policies")), Expect: ap.PackageTestsScopeProbeExpectAllow,
			},
		},
		SafetyProbes: []ap.PackageTestsSafetyProbe{
			safety("update-confirmed", ap.PackageTestsSafetyProbeKindConfirmedCommit, ap.PackageTestsSafetyExpectOutcomeCommittedOnce, ""),
			safety("update-unconfirmed", ap.PackageTestsSafetyProbeKindUnconfirmedCommit, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "confirmation_required"),
			safety("update-replayed", ap.PackageTestsSafetyProbeKindReplayedCommit, ap.PackageTestsSafetyExpectOutcomeOriginalReceiptReturned, ""),
			safety("update-expired", ap.PackageTestsSafetyProbeKindExpiredPreparation, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "expired"),
			safety("update-stale", ap.PackageTestsSafetyProbeKindStaleRecord, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "stale"),
			safety("update-cancelled", ap.PackageTestsSafetyProbeKindCancelledPreparation, ap.PackageTestsSafetyExpectOutcomeRejectedNoEffect, "conflict"),
			safety("update-uncertain", ap.PackageTestsSafetyProbeKindUncertainCommitRecovery, ap.PackageTestsSafetyExpectOutcomeRecoveredViaStatus, ""),
		},
		Smoke: ap.PackageTestsSmoke{VerifyCleanup: true, Paths: []ap.PackageTestsSmokePath{
			{ID: "smoke-text", Path: ap.PackageTestsSmokePathPathText, Principal: "viewer-a1", CapabilityID: ptr(ap.CapabilityID(capList)),
				Utterance: "What notes do I have?", Expect: ap.PackageTestsSmokePathExpect{AuditEvents: []ap.DottedID{"policy.decision", "dialogue.turn"}}},
			{ID: "smoke-rag", Path: ap.PackageTestsSmokePathPathRAG, Principal: "viewer-a1", Utterance: "What is the retention policy?",
				Expect: ap.PackageTestsSmokePathExpect{AuditEvents: []ap.DottedID{"rag.retrieved"}, Citations: ptr(true)}},
			{ID: "smoke-action", Path: ap.PackageTestsSmokePathPathAction, Principal: "editor-a1", CapabilityID: ptr(ap.CapabilityID(capUpdate)),
				Utterance: "Change the note", Expect: ap.PackageTestsSmokePathExpect{AuditEvents: []ap.DottedID{"action.prepared", "action.confirmed", "action.committed"}}},
		}},
	}
}

// principal builds a fixture principal; the generated type is decoded from its
// JSON form so that this file needs no package beyond the package types.
func principal(ref, tenant, workspace, role string) ap.PackageTestsPrincipal {
	var p ap.PackageTestsPrincipal
	doc := fmt.Sprintf(`{"ref":%q,"tenant":%q,"workspace":%q,"kind":"human","roles":[%q]}`, ref, tenant, workspace, role)
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		panic(err)
	}
	return p
}

// simPackage is the typed package of the notes application: what the loader
// would hand the audit, built by hand so the host and the package are one
// description. Only the exported members the probes read are filled.
func simPackage(t testing.TB) *apppkg.Package {
	t.Helper()
	m := ap.Manifest{}
	m.Application.ID = simAppID
	m.Application.Version = "1.0.0"
	m.Package.Version = "1.0.0"
	m.Tenancy.Roles = map[string]ap.ManifestTenancyRolesValue{"editor": {}, "viewer": {}}
	return &apppkg.Package{
		Manifest:     m,
		Capabilities: simCapabilities(),
		Policies:     []*policy.HostPolicy{simPolicy(t)},
		Tests:        []ap.PackageTests{simSuite()},
	}
}

// ---- the input of a check ----

type simEnv struct {
	t     *testing.T
	sim   *sim
	pkg   *apppkg.Package
	in    *integrationaudit.Input
	clk   *clock.Fake
	guard adapters.AppAdapter
}

func newEnv(t *testing.T, knobs ...func(*simKnobs)) *simEnv {
	t.Helper()
	return newEnvWith(t, simPackage(t), newSim(knobs...))
}

func newEnvWith(t *testing.T, pkg *apppkg.Package, s *sim) *simEnv {
	t.Helper()
	guard, err := adapters.Wrap(s, adapters.Options{ApplicationID: simAppID, DefaultTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(simT0)
	s.clk = clk
	e := &simEnv{t: t, sim: s, pkg: pkg, clk: clk, guard: guard}
	desc, err := s.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.in = &integrationaudit.Input{
		Description: &desc,
		Package:     pkg, Adapter: guard, Clock: clk, CoreVersion: apppkg.DevCoreVersion,
		Scope: contracts.Scope{
			DeploymentID: "alixai-audit", ApplicationID: simAppID, TenantID: "alixai-audit", WorkspaceID: "alixai-audit",
			PrincipalID: "alixai-audit", Roles: []string{"auditor"}, CapabilityScopes: []string{"audit.inspect"}, SessionID: "audit-run",
		},
	}
	return e
}

// opts are the options a test gives the checks: the sim is the provisioner.
func (e *simEnv) opts() Options {
	return Options{
		Source:     func(context.Context, *integrationaudit.Input) (Provisioner, error) { return e.sim, nil },
		ScratchDir: e.t.TempDir(),
	}
}

func (e *simEnv) check(section int) integrationaudit.Check {
	for _, c := range Checks(e.opts()) {
		if c.Section() == section {
			return c
		}
	}
	e.t.Fatalf("no check for section %d", section)
	return nil
}

// run executes the check of a section and returns its outcome.
func (e *simEnv) run(section int) *integrationaudit.Outcome {
	e.t.Helper()
	out, err := e.check(section).Run(context.Background(), e.in)
	if err != nil {
		e.t.Fatalf("check %d returned an error: %v", section, err)
	}
	return out
}

// findings lists the check IDs of the findings of an outcome of a severity.
func findingIDs(o *integrationaudit.Outcome, sev contracts.Severity) []string {
	var out []string
	for _, f := range o.Findings {
		if f.Severity == sev {
			out = append(out, f.CheckID)
		}
	}
	sort.Strings(out)
	return out
}

func hasFinding(o *integrationaudit.Outcome, id string) bool {
	for _, f := range o.Findings {
		if f.CheckID == id {
			return true
		}
	}
	return false
}

func hasCritical(o *integrationaudit.Outcome) bool { return contracts.HasCritical(o.Findings) }

func dump(o *integrationaudit.Outcome) string {
	var b strings.Builder
	for _, f := range o.Findings {
		fmt.Fprintf(&b, "  %s\n", f.String())
	}
	for _, u := range o.Unverified {
		fmt.Fprintf(&b, "  unverified %s [%s]: %s\n", u.Subject, u.Capability, u.Reason)
	}
	for _, p := range o.Probes {
		if !p.Passed {
			fmt.Fprintf(&b, "  probe %s %s %s: want %s, got %s\n", p.Kind, p.Capability, p.Subject, p.Expected, p.Observed)
		}
	}
	return b.String()
}
