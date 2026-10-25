package ledger

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/pkg/bridge"
)

// Disposable tenants for the integration audit's runtime probes (master plan 4.5
// check 10). The audit creates an organisation with its teams and principals,
// seeds synthetic records, runs its probes through the real operations and then
// removes everything. All of it is synthetic.
//
// The operations are management operations, not capabilities: no capability maps
// them, the package's adapter configuration approves them by name, and the bridge
// declares them only when the application runs in test support mode. A tenant that
// was not created as disposable is never touched by them. Three are the audit's
// disposable-tenant calls (create_tenant, seed, cleanup); the fourth, approve_step_up,
// is the audit's way to complete a level 3 void: it does what the signed-in manager
// does in the application's own interface (ApproveVoid) and nothing more, so the
// approval is recorded exactly as it is in production, for the same checks.

const defaultContract = "# Master services agreement\n\nPayment is due 30 days after the invoice date. Late payments carry a fee of 1.5 percent per month.\n"

var scopeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func mgmtRefuse(code, message string) map[string]any {
	return map[string]any{
		"status": "rejected",
		"errors": []any{map[string]any{"code": code, "message": message}},
	}
}

func asMap(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }

func asList(v any) ([]any, bool) { l, ok := v.([]any); return l, ok }

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// wholeNumber reads an integer from a management argument: a JSON number decodes as
// float64 on this path, and only an exact whole number inside the safe range counts.
func wholeNumber(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return 0, false
		}
		return int64(x), true
	default:
		n, err := minorFrom(v)
		return n, err == nil
	}
}

// createTenant makes a disposable organisation: create_tenant.
func (l *Ledger) createTenant(args map[string]any) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := str(args, "tenant_id")
	if !scopeID.MatchString(id) {
		return mgmtRefuse("validation_failed", "tenant_id must be a valid scope identifier")
	}
	teams, ok := asList(args["workspaces"])
	if !ok || len(teams) == 0 || len(teams) > 16 {
		return mgmtRefuse("validation_failed", "workspaces must list between 1 and 16 teams")
	}
	t := newTenant(id, firstNonEmpty(str(args, "name"), "Synthetic Organisation"), true)
	var teamIDs []any
	for i, raw := range teams {
		m, ok := asMap(raw)
		if !ok || !scopeID.MatchString(str(m, "workspace_id")) || t.teams[str(m, "workspace_id")] != nil {
			return mgmtRefuse("validation_failed", "a team needs a unique valid workspace_id")
		}
		wid := str(m, "workspace_id")
		t.teams[wid] = &team{id: wid, name: firstNonEmpty(str(m, "name"), fmt.Sprintf("Synthetic Team %d", i+1))}
		teamIDs = append(teamIDs, wid)
	}
	var created []any
	if raw, present := args["principals"]; present {
		list, ok := asList(raw)
		if !ok || len(list) > 64 {
			return mgmtRefuse("validation_failed", "principals must list at most 64 principals")
		}
		for i, rawP := range list {
			m, ok := asMap(rawP)
			if !ok || !scopeID.MatchString(str(m, "principal_id")) || t.principals[str(m, "principal_id")] != nil {
				return mgmtRefuse("validation_failed", "a principal needs a unique valid principal_id")
			}
			if t.teams[str(m, "workspace_id")] == nil {
				return mgmtRefuse("validation_failed", "a principal must belong to one of the created teams")
			}
			roles, _ := asList(m["roles"])
			role := Role(str(m, "role"))
			if len(roles) == 1 {
				role = Role(fmt.Sprint(roles[0]))
			}
			if _, known := rolePermissions[role]; !known {
				return mgmtRefuse("validation_failed", "a principal holds exactly one role: accountant, manager or viewer")
			}
			p := &principal{id: str(m, "principal_id"), team: str(m, "workspace_id"), role: role, name: fmt.Sprintf("Synthetic %s %d", role, i+1)}
			t.principals[p.id] = p
			created = append(created, map[string]any{"principal_id": p.id, "workspace_id": p.team, "role": string(role)})
		}
	}
	for _, wid := range sortedKeys(t.teams) {
		// Every team starts with its standard contract, so the contracts source has
		// something to serve in a disposable tenant.
		l.putContract(t, wid, "master-services", "Master services agreement", defaultContract)
	}
	if l.tenants[id] != nil {
		return mgmtRefuse("conflict", "a tenant with this identifier already exists")
	}
	l.tenants[id] = t
	return map[string]any{"status": "ok", "tenant_id": id, "workspaces": teamIDs, "principals": created}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

type seedRecord struct {
	ref, entityType, team string
	m                     map[string]any
}

// seedTenant loads synthetic records into a disposable tenant: seed. It checks every
// record before it writes any, so a record it cannot create fails the whole call and
// leaves the tenant as it was.
func (l *Ledger) seedTenant(args map[string]any) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.tenants[str(args, "tenant_id")]
	if t == nil {
		return mgmtRefuse("scope_violation", "that tenant does not exist")
	}
	if !t.disposable {
		return mgmtRefuse("permission_denied", "only a disposable tenant can be seeded")
	}
	raw, ok := asList(args["records"])
	if !ok || len(raw) == 0 || len(raw) > 1024 {
		return mgmtRefuse("validation_failed", "records must list between 1 and 1024 records")
	}
	var recs []seedRecord
	earlier := map[string]string{} // ref -> entity type, for links inside the call
	for _, r := range raw {
		m, ok := asMap(r)
		if !ok {
			return mgmtRefuse("validation_failed", "a record must be an object")
		}
		rec := seedRecord{ref: str(m, "ref"), entityType: str(m, "entity_type"), team: str(m, "workspace_id"), m: m}
		if rec.ref == "" || earlier[rec.ref] != "" {
			return mgmtRefuse("validation_failed", "a record needs a unique ref")
		}
		if t.teams[rec.team] == nil {
			return mgmtRefuse("validation_failed", "a record must be in one of the tenant's teams")
		}
		switch rec.entityType {
		case "customer", "quote", "invoice", "contract":
		default:
			return mgmtRefuse("validation_failed", "entity_type must be customer, quote, invoice or contract")
		}
		if link := str(m, "customer"); link != "" && earlier[link] != "customer" {
			return mgmtRefuse("validation_failed", "a customer link must name an earlier customer record of the call")
		}
		if cur := str(m, "currency"); cur != "" {
			if _, known := Exponent(cur); !known {
				return mgmtRefuse("validation_failed", "currency is not supported")
			}
		}
		total := int64(125000)
		if v, present := m["amount_minor"]; present {
			n, ok := wholeNumber(v)
			if !ok || n < 1 || n > MaxMinor {
				return mgmtRefuse("validation_failed", "amount_minor must be a whole number of minor units, at least 1")
			}
			total = n
		}
		if v, present := m["paid_minor"]; present {
			n, ok := wholeNumber(v)
			if !ok || n < 0 || n > total {
				return mgmtRefuse("validation_failed", "paid_minor must be from 0 to the invoice total")
			}
		}
		earlier[rec.ref] = rec.entityType
		recs = append(recs, rec)
	}

	byRef := map[string]string{}
	out := make([]any, 0, len(recs))
	for _, rec := range recs {
		id := l.seedOne(t, rec, byRef)
		byRef[rec.ref] = id
		out = append(out, map[string]any{"ref": rec.ref, "entity_type": rec.entityType, "entity_id": id, "workspace_id": rec.team})
	}
	return map[string]any{"status": "ok", "tenant_id": t.id, "records": out}
}

func (l *Ledger) seedOne(t *tenant, rec seedRecord, byRef map[string]string) string {
	m := rec.m
	cur := firstNonEmpty(str(m, "currency"), "EUR")
	custOf := func() *customer {
		if id := byRef[str(m, "customer")]; id != "" {
			return t.customers[id]
		}
		cu := &customer{id: l.nextID("cus"), team: rec.team, name: "Synthetic Customer", currency: cur, version: 1}
		t.customers[cu.id] = cu
		return cu
	}
	amount := func() Money {
		n, ok := wholeNumber(m["amount_minor"])
		if !ok || m["amount_minor"] == nil {
			n = 125000
		}
		return Money{Minor: n, Currency: cur}
	}
	switch rec.entityType {
	case "customer":
		cu := &customer{id: l.nextID("cus"), team: rec.team, name: firstNonEmpty(str(m, "name"), "Synthetic Customer"), currency: cur, version: 1}
		t.customers[cu.id] = cu
		return cu.id
	case "quote":
		cu := custOf()
		amt := amount()
		amt.Currency = cu.currency
		t.quoteSeq++
		q := &quote{
			id: l.nextID("quo"), number: firstNonEmpty(str(m, "number"), "QUO-"+pad(int64(t.quoteSeq), 4)), team: rec.team,
			customerID: cu.id, description: firstNonEmpty(str(m, "description"), "Synthetic services"), amount: amt,
			validUntil: addDays(l.today(), 30), status: quoteDraft, version: 1,
		}
		t.quotes[q.id] = q
		return q.id
	case "invoice":
		cu := custOf()
		amt := amount()
		amt.Currency = cu.currency
		paid, _ := wholeNumber(m["paid_minor"])
		status := invoiceIssued
		switch {
		case str(m, "status") == invoiceVoid && paid == 0:
			status = invoiceVoid
		case paid == amt.Minor:
			status = invoicePaid
		case paid > 0:
			status = invoicePartial
		}
		t.invoiceSeq++
		inv := &invoice{
			id: l.nextID("inv"), number: firstNonEmpty(str(m, "number"), "INV-"+pad(int64(t.invoiceSeq), 4)), team: rec.team,
			customerID: cu.id, total: amt, paid: paid, issuedOn: l.today(), dueOn: addDays(l.today(), 30), status: status, version: 1,
		}
		t.invoices[inv.id] = inv
		return inv.id
	default: // contract
		ct := l.putContract(t, rec.team, firstNonEmpty(str(m, "key"), "contract"), firstNonEmpty(str(m, "title"), "Synthetic contract"),
			firstNonEmpty(str(m, "body"), "This is a synthetic contract. Payment is due within 30 days of the invoice date."))
		return ct.id
	}
}

// cleanupTenant removes a disposable tenant and everything the run created: cleanup.
// It can be repeated: a tenant that is already gone answers existed=false with nothing
// remaining, which is also how the audit proves nothing was left.
func (l *Ledger) cleanupTenant(args map[string]any) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := str(args, "tenant_id")
	if !scopeID.MatchString(id) {
		return mgmtRefuse("validation_failed", "tenant_id must be a valid scope identifier")
	}
	t := l.tenants[id]
	if t == nil {
		return map[string]any{"status": "ok", "tenant_id": id, "existed": false, "remaining": l.remaining(id)}
	}
	if !t.disposable {
		return mgmtRefuse("permission_denied", "only a disposable tenant can be removed")
	}
	delete(l.tenants, id)
	kept := l.outbox[:0]
	for _, e := range l.outbox {
		if e.TenantID != id {
			kept = append(kept, e)
		}
	}
	l.outbox = kept
	return map[string]any{"status": "ok", "tenant_id": id, "existed": true, "remaining": l.remaining(id)}
}

// remaining counts what is still held for a tenant: zero for everything once it is
// gone.
func (l *Ledger) remaining(id string) map[string]any {
	n := map[string]int{}
	if t := l.tenants[id]; t != nil {
		n["customers"], n["quotes"], n["invoices"] = len(t.customers), len(t.quotes), len(t.invoices)
		n["payments"], n["contracts"], n["principals"] = len(t.payments), len(t.contracts), len(t.principals)
	}
	for _, e := range l.outbox {
		if e.TenantID == id {
			n["events"]++
		}
	}
	out := map[string]any{}
	for _, k := range []string{"customers", "quotes", "invoices", "payments", "contracts", "principals", "events"} {
		out[k] = n[k]
	}
	return out
}

// approveStepUp completes a prepared level 3 void in a disposable tenant, as the manager it
// was prepared for: approve_step_up. The arguments name the tenant, the team and the
// principal the void was prepared for, the capability, and the facts Prepare stated, from
// which the ledger takes its own void_reference. The principal's role comes from the
// ledger's own directory, as in any request, and ApproveVoid applies every rule the
// application's interface applies: the role, the team, a voidable invoice at the prepared
// version. A tenant that was not created as disposable is never touched.
func (l *Ledger) approveStepUp(args map[string]any) map[string]any {
	id, refused := l.stepUpIdentity(args)
	if refused != nil {
		return refused
	}
	vals, _ := asMap(args["values"])
	reference := str(vals, "void_reference")
	if reference == "" {
		return mgmtRefuse("validation_failed", "values must state the void_reference")
	}
	if err := l.ApproveVoid(id, reference); err != nil {
		var r *refusal
		if errors.As(err, &r) {
			return mgmtRefuse(r.code, r.message)
		}
		return mgmtRefuse("internal", "the approval could not be recorded")
	}
	return map[string]any{"status": "ok", "tenant_id": id.TenantID, "approved": true}
}

// stepUpIdentity checks the arguments of approve_step_up against the disposable tenant and
// returns the identity ApproveVoid reauthorises, or the refusal to answer with. It holds the
// ledger's lock only while it reads the directory; ApproveVoid takes it again.
func (l *Ledger) stepUpIdentity(args map[string]any) (bridge.Identity, map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	refuseWith := func(code, message string) (bridge.Identity, map[string]any) {
		return bridge.Identity{}, mgmtRefuse(code, message)
	}
	tenantID := str(args, "tenant_id")
	if !scopeID.MatchString(tenantID) {
		return refuseWith("validation_failed", "tenant_id must be a valid scope identifier")
	}
	t := l.tenants[tenantID]
	if t == nil {
		return refuseWith("scope_violation", "that tenant does not exist")
	}
	if !t.disposable {
		return refuseWith("permission_denied", "only a disposable tenant can be approved for")
	}
	if str(args, "capability_id") != "invoices.void" {
		return refuseWith("validation_failed", "only invoices.void has a step-up here")
	}
	p := t.principals[str(args, "principal_id")]
	if p == nil || p.team != str(args, "workspace_id") {
		return refuseWith("scope_violation", "the principal is not in that team of the tenant")
	}
	return bridge.Identity{
		ApplicationID: ApplicationID, TenantID: tenantID, WorkspaceID: p.team, PrincipalID: p.id,
		Roles: []string{string(p.role)}, SessionID: "test-support-step-up",
	}, nil
}

// registerManagement declares the operations; the bridge serves them only to a peer that
// holds the application key, and only when test support is on.
func (l *Ledger) registerManagement(srv *bridge.Server) error {
	for name, fn := range map[string]func(map[string]any) map[string]any{
		"create_tenant": l.createTenant, "seed": l.seedTenant, "cleanup": l.cleanupTenant, "approve_step_up": l.approveStepUp,
	} {
		if err := srv.HandleManagement(name, func(_ context.Context, args map[string]any) (any, error) { return fn(args), nil }); err != nil {
			return err
		}
	}
	return nil
}
