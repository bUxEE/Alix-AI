package m1

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
)

// ---- what the application was asked ---------------------------------------------------------

// appCalls is how many times the application was asked to do anything for a user: every tool
// call except its provisioning tools (test_*) and the management tools the core's own health and
// description checks use.
func appCalls(s clinicStats) int {
	n := 0
	for tool, k := range s.Calls {
		if strings.HasPrefix(tool, "test_") || tool == "describe" || tool == "health" || tool == "inspect" {
			continue
		}
		n += k
	}
	return n
}

// callsSince lists, as "tool x n", what the application was asked since an earlier reading.
func (s clinicStats) callsSince(before clinicStats) string {
	var out []string
	for tool, k := range s.Calls {
		if d := k - before.Calls[tool]; d != 0 && !strings.HasPrefix(tool, "test_") {
			out = append(out, fmt.Sprintf("%s x%d", tool, d))
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// totalsSince is how the application's row count of a kind moved since an earlier reading.
func (s clinicStats) totalsSince(before clinicStats, kind string) int {
	return s.Totals[kind] - before.Totals[kind]
}

// ---- calling the application directly -------------------------------------------------------

// callAs calls one tool of the application as a principal, directly, with the identity block the
// core sends. It is how a test reads what the application holds, independently of AlixAI.
func (w *world) callAs(t tb, p *person, tool string, fields map[string]any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := w.clinic.callTool(ctx, tool, map[string]any{
		"identity": map[string]any{
			"application_id": clinicApplication, "tenant_id": p.Tenant, "workspace_id": p.Workspace,
			"principal_id": p.ID, "roles": []string{p.Role}, "session_id": "ses_gate_direct",
		},
		"fields": fields,
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	return raw
}

// appResult is the host members of a result, as the application states them.
type appResult struct {
	Status      string `json:"status"`
	Identifiers []struct {
		EntityID   string `json:"entity_id"`
		EntityType string `json:"entity_type"`
	} `json:"identifiers"`
	Values []struct {
		Field string `json:"field"`
		Value struct {
			Kind  string `json:"kind"`
			Value any    `json:"value"`
			Zone  string `json:"zone"`
		} `json:"value"`
		Display string `json:"display"`
	} `json:"values"`
}

// value is the application's rendering of a named value: its display form, else the canonical one.
func (r appResult) value(field string) (string, bool) {
	for _, v := range r.Values {
		if v.Field == field {
			if v.Display != "" {
				return v.Display, true
			}
			return fmt.Sprint(v.Value.Value), true
		}
	}
	return "", false
}

// canonical is the canonical (not the display) form of a named value.
func (r appResult) canonical(field string) (string, bool) {
	for _, v := range r.Values {
		if v.Field == field {
			return fmt.Sprint(v.Value.Value), true
		}
	}
	return "", false
}

func (r appResult) ids() []string {
	var out []string
	for _, i := range r.Identifiers {
		out = append(out, i.EntityID)
	}
	return out
}

func parseResult(t tb, raw json.RawMessage) appResult {
	t.Helper()
	var r appResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("an application result does not decode: %v: %s", err, raw)
	}
	return r
}

// slotInfo is a free slot as the application itself lists it.
type slotInfo struct {
	ID      string
	Display string // "2026-10-04 09:00", the site's local time
	Date    string // "2026-10-04"
	UTC     string // the canonical instant
}

// slots lists the free slots of the principal's site, earliest first, as the application states
// them (not as AlixAI does).
func (w *world) slots(t tb, p *person) []slotInfo {
	t.Helper()
	res := parseResult(t, w.callAs(t, p, "availability_list", map[string]any{"days": 14, "max_results": 10}))
	var out []slotInfo
	for i, id := range res.ids() {
		field := fmt.Sprintf("slot_%d_starts_at", i+1)
		disp, ok := res.value(field)
		if !ok {
			t.Fatalf("the application lists slot %s without a start", id)
		}
		utc, _ := res.canonical(field)
		out = append(out, slotInfo{ID: id, Display: disp, Date: strings.Fields(disp)[0], UTC: utc})
	}
	return out
}

// appointmentByReference asks the application, as the principal, which appointments carry a
// reference: the application's own answer to "does this appointment exist".
func (w *world) appointmentByReference(t tb, p *person, reference string) []string {
	t.Helper()
	res := parseResult(t, w.callAs(t, p, "resolve_appointment", map[string]any{"entity_type": "appointment", "mention": reference}))
	return res.ids()
}

// ---- the audit log ----------------------------------------------------------------------------

// auditEvent is one event of a chain.
type auditEvent struct {
	Type string
	Data map[string]any
}

// chainEvents lists the events of the tenant's audit chain of the application, in order.
func (r *rig) chainEvents(tenant string) []auditEvent {
	r.t.Helper()
	return r.queryEvents(`SELECT event_type, data FROM audit_events WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq`, clinicApplication, tenant)
}

// applicationEvents lists the events of the application's own chain (installs, audits,
// activations, quarantines).
func (r *rig) applicationEvents() []auditEvent {
	r.t.Helper()
	return r.queryEvents(`SELECT event_type, data FROM audit_events WHERE application_id = ? AND tenant_id = '' ORDER BY chain_seq`, clinicApplication)
}

func (r *rig) queryEvents(query string, args ...any) []auditEvent {
	r.t.Helper()
	rows, err := r.db.QueryContext(context.Background(), query, args...)
	if err != nil {
		r.t.Fatalf("read the audit log: %v", err)
	}
	defer rows.Close()
	var out []auditEvent
	for rows.Next() {
		var typ, data string
		if err := rows.Scan(&typ, &data); err != nil {
			r.t.Fatal(err)
		}
		ev := auditEvent{Type: typ}
		if err := json.Unmarshal([]byte(data), &ev.Data); err != nil {
			r.t.Fatalf("an audit event's data does not decode: %v: %s", err, data)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

func eventTypes(evs []auditEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// turnRecord is the dialogue.turn event of one turn: what was decided and where the response
// came from. It holds identifiers and codes, never the user's words.
type turnRecord struct {
	Act        string
	Code       string
	Language   string
	Response   string // "model" or "template"
	ModelCalls int
	Receipt    string
}

// turnRecordOf finds the audit record of a turn.
func (r *rig) turnRecordOf(tenant, turnID string) (turnRecord, bool) {
	r.t.Helper()
	for _, e := range r.chainEvents(tenant) {
		if e.Type != "dialogue.turn" || e.Data["turn_id"] != turnID {
			continue
		}
		str := func(k string) string { s, _ := e.Data[k].(string); return s }
		calls, _ := e.Data["model_calls"].(float64)
		return turnRecord{
			Act: str("act"), Code: str("code"), Language: str("language"), Response: str("response"),
			ModelCalls: int(calls), Receipt: str("receipt_id"),
		}, true
	}
	return turnRecord{}, false
}

// verifyChain checks the tenant's audit chain is intact (every hash, every link) and returns how
// many events it holds.
func (r *rig) verifyChain(tenant string) int {
	r.t.Helper()
	rep, err := r.audit.VerifyChain(context.Background(), audit.ChainKey{ApplicationID: clinicApplication, TenantID: tenant}, audit.VerifyOptions{})
	if err != nil || !rep.OK() {
		r.t.Fatalf("the audit chain of %s does not verify: %v %+v", tenant, err, rep.Problems)
	}
	return int(rep.Events)
}

// preparationState is the state the coordinator's store holds for a preparation: the
// authoritative one (the state member of the announced PreparedAction is a snapshot).
func (r *rig) preparationState(preparationID string) string {
	r.t.Helper()
	var state string
	if err := r.db.QueryRowContext(context.Background(), `SELECT state FROM action_preparations WHERE id = ?`, preparationID).Scan(&state); err != nil {
		r.t.Fatalf("the state of %s: %v", preparationID, err)
	}
	return state
}
