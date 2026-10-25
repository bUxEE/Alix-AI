package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// ClinicHost is the in-process stand-in for the clinic-booking reference application
// (examples/reference-apps/clinic-booking), so the conversation tests are hermetic: the
// real application is TypeScript on Node over MCP and is exercised by its own tests and
// by the live checks, never here.
//
// It is exported so that the harness that scores live models on conversation-v1.jsonl
// (tools/bench, master plan 17.3) runs the same world the corpus was written against:
// NewClinicHost seeds the synthetic world (world "clinic-synthetic-v1"), Adapter is the
// adapters.AppAdapter to put inside adapters.Wrap, RoleScope is the verified scope of a
// role and Apply performs a corpus host event. The seed is part of the corpus: its record
// identifiers appear in expected outcomes, so changing it is a new corpus.
//
// It is a transcription of the application's behaviour, not a new design: the same
// roles, the same record-level visibility, the same four resolvers with the same
// reserved arguments, the same business rules (a booking is 60 minutes to 90 days
// ahead, a slot is booked once, a patient has no overlapping appointments, a started
// appointment is not cancelled), the same fact names, the same per-record snapshot
// versions and the same idempotent commit. Every reply is raw JSON made of the host
// members of a DeterministicResult, read back through adapters.Normalise like a real
// transport's, and TestClinicHostRepliesMatchThePackagesOwnSchemas holds each one to
// the result schemas the signed package declares for it, so the stand-in cannot drift
// into saying something the real application would not.
//
// One site is modelled (a clinic group, a clinic site, in UTC) with a receptionist, a
// doctor and a patient who each reach their own slice of it. A refusal is a result with
// status rejected, never an error, as the contract says.
type ClinicHost struct {
	mu  sync.Mutex
	clk clock.Clock

	principals map[string]clinicPrincipal
	patients   map[string]*clinicPatient
	doctors    map[string]*clinicDoctor
	slots      map[string]*clinicSlot
	appts      map[string]*clinicAppt

	keys    map[string]clinicKey
	calls   []clinicCall
	replies []clinicReply
	nextID  int
	// effects counts the writes the host applied (a booking, a cancellation or a contact
	// change), so a test can say that nothing happened.
	effects int
	// down makes the host unreachable (connection refused).
	down bool
}

type clinicPrincipal struct {
	id, role, patientID, doctorID string
}

type clinicPatient struct {
	id, given, family, dob, phone, email string
	version                              int
}

type clinicDoctor struct{ id, name, specialty string }

type clinicSlot struct {
	id, doctorID string
	startsAt     time.Time
	minutes      int
	status       string // free, booked
	apptID       string
	version      int
}

type clinicAppt struct {
	id, ref, slotID, patientID, doctorID string
	startsAt                             time.Time
	status                               string // booked, cancelled
	reason, cancelReason                 string
	version                              int
}

type clinicKey struct {
	capability, principal, fingerprint string
	reply                              map[string]any
}

// clinicCall is one request the host received.
type clinicCall struct {
	Operation    adapters.Operation
	CapabilityID string
	Principal    string
	Fields       map[string]any
}

// clinicReply is one reply the host sent, as raw JSON, for the schema check.
type clinicReply struct {
	Operation    adapters.Operation
	CapabilityID string
	Fields       map[string]any
	Body         []byte
}

// The identifiers of the synthetic world. They are distinctive so a leak into a prompt
// is found by a substring search.
const (
	clinicApp      = "clinic-booking"
	clinicTenant   = "grp-medica-4410"
	clinicSite     = "site-nord-2290"
	clinicDeploy   = "dep-test-3301"
	clinicSiteName = "Clinic North"

	pidMaria   = "pat_maria0001"
	pidMaria2  = "pat_maria0002"
	pidLuca    = "pat_luca00001"
	didVerdi   = "doc_verdi0001"
	didNeri    = "doc_neri00001"
	apptMaria  = "apt_maria0001"
	refMaria   = "APT-7K3Q9X"
	apptLuca   = "apt_luca00001"
	refLuca    = "APT-4M2D8Z"
	userRecep  = "usr-reception-1"
	userDoctor = "usr-doctor-1"
	userMaria  = "usr-patient-1"
)

const (
	minLeadMinutes = 60
	maxAheadDays   = 90
	bookingTTL     = 600
	cancelTTL      = 600
	contactTTL     = 120
)

// NewClinicHost seeds the world relative to the clock: today is a Monday and the
// listed slots start two to four days later.
func NewClinicHost(clk clock.Clock) *ClinicHost {
	h := &ClinicHost{
		clk:        clk,
		principals: map[string]clinicPrincipal{},
		patients:   map[string]*clinicPatient{},
		doctors:    map[string]*clinicDoctor{},
		slots:      map[string]*clinicSlot{},
		appts:      map[string]*clinicAppt{},
		keys:       map[string]clinicKey{},
	}
	h.principals[userRecep] = clinicPrincipal{id: userRecep, role: "receptionist"}
	h.principals[userDoctor] = clinicPrincipal{id: userDoctor, role: "doctor", doctorID: didVerdi}
	h.principals[userMaria] = clinicPrincipal{id: userMaria, role: "patient", patientID: pidMaria}

	// Two patients called Maria Rossi (different birth dates) make a name ambiguous.
	h.patients[pidMaria] = &clinicPatient{id: pidMaria, given: "Maria", family: "Rossi", dob: "1984-03-12", version: 1}
	h.patients[pidMaria2] = &clinicPatient{id: pidMaria2, given: "Maria", family: "Rossi", dob: "1991-07-30", version: 1}
	h.patients[pidLuca] = &clinicPatient{id: pidLuca, given: "Luca", family: "Ferri", dob: "1975-11-02", version: 1}
	h.doctors[didVerdi] = &clinicDoctor{id: didVerdi, name: "Dr. Anna Verdi", specialty: "general practice"}
	h.doctors[didNeri] = &clinicDoctor{id: didNeri, name: "Dr. Marco Neri", specialty: "cardiology"}

	day := func(d, hh, mm int) time.Time {
		n := clk.Now().UTC()
		return time.Date(n.Year(), n.Month(), n.Day()+d, hh, mm, 0, 0, time.UTC)
	}
	h.addSlot("slot_free00001", didVerdi, day(2, 9, 0))
	h.addSlot("slot_free00002", didVerdi, day(2, 10, 30))
	h.addSlot("slot_free00003", didNeri, day(2, 11, 30))
	h.addSlot("slot_free00004", didNeri, day(3, 9, 0))
	h.addSlot("slot_free00005", didVerdi, day(3, 14, 0))
	h.addSlot("slot_soon000001", didVerdi, clk.Now().UTC().Add(30*time.Minute)) // too soon to book

	// Two booked appointments, so a name or a reference can be cancelled.
	h.bookDirect("slot_booked0001", apptMaria, refMaria, pidMaria, didVerdi, day(4, 10, 0))
	h.bookDirect("slot_booked0002", apptLuca, refLuca, pidLuca, didNeri, day(4, 15, 0))
	return h
}

func (h *ClinicHost) addSlot(id, doctorID string, at time.Time) {
	h.slots[id] = &clinicSlot{id: id, doctorID: doctorID, startsAt: at, minutes: 30, status: "free", version: 1}
}

func (h *ClinicHost) bookDirect(slotID, apptID, ref, patientID, doctorID string, at time.Time) {
	h.slots[slotID] = &clinicSlot{id: slotID, doctorID: doctorID, startsAt: at, minutes: 30, status: "booked", apptID: apptID, version: 2}
	h.appts[apptID] = &clinicAppt{id: apptID, ref: ref, slotID: slotID, patientID: patientID, doctorID: doctorID, startsAt: at, status: "booked", version: 1}
}

// ---- the exported surface: the world a corpus is written against ----

// Adapter returns the adapter over the host. Put it inside adapters.Wrap with the
// application ID "clinic-booking", as a real transport is.
func (h *ClinicHost) Adapter() adapters.AppAdapter { return clinicAdapter{h} }

// Apply performs a corpus host event: another user takes a slot, time passes (the clock
// the host was built with must be a *clock.Fake) or a patient record exists. An event it
// does not know, or cannot perform, is an error: a replay never skips one.
func (h *ClinicHost) Apply(ev HostEvent) error {
	switch ev.Kind {
	case "slot_taken":
		return h.takeSlot(ev.Slot, ev.ByPatient)
	case "time_passes":
		fake, ok := h.clk.(*clock.Fake)
		if !ok || ev.Minutes <= 0 {
			return fmt.Errorf("time_passes needs a fake clock and a positive number of minutes")
		}
		fake.Advance(time.Duration(ev.Minutes) * time.Minute)
	case "add_patient":
		if ev.PatientID == "" || ev.Given == "" || ev.Family == "" || ev.Born == "" {
			return fmt.Errorf("add_patient needs patient_id, given, family and born")
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.patients[ev.PatientID] = &clinicPatient{id: ev.PatientID, given: ev.Given, family: ev.Family, dob: ev.Born, version: 1}
	default:
		return fmt.Errorf("unknown host event %q", ev.Kind)
	}
	return nil
}

// Roles of the clinic and the verified scope of each: the principal the application knows
// them as and a context token that permits every capability of the application (the host
// and the policy gateway still decide what the role may do).
const (
	RoleReceptionist = "receptionist"
	RoleDoctor       = "doctor"
	RolePatient      = "patient"
)

// RoleScope returns the verified scope of the clinic's principal for a role, or a zero
// scope for a role it does not have.
func RoleScope(role string) contracts.Scope {
	switch role {
	case RoleReceptionist:
		return ScopeFor(userRecep, role, "ses-reception-5521")
	case RoleDoctor:
		return ScopeFor(userDoctor, role, "ses-doctor-5522")
	case RolePatient:
		return ScopeFor(userMaria, role, "ses-patient-5523")
	}
	return contracts.Scope{}
}

// ScopeFor is a verified scope for one principal of the clinic.
func ScopeFor(principal, role, session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: clinicDeploy, ApplicationID: clinicApp, TenantID: clinicTenant, WorkspaceID: clinicSite,
		PrincipalID: principal, Roles: []string{role}, SessionID: session,
		CapabilityScopes: []string{"availability.list", "patients.find", "appointments.book", "appointments.cancel", "patients.update_contact"},
	}
}

// ---- test controls ----

func (h *ClinicHost) setDown(down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down = down
}

// takeSlot books a free slot for a patient behind the conversation's back, as another
// receptionist would: the slot's revision moves, and the application applies no effect of
// its own (it is not one of the conversation's writes).
func (h *ClinicHost) takeSlot(slotID, patientID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.slots[slotID]
	if _, known := h.patients[patientID]; !ok || !known || s.status != "free" {
		return fmt.Errorf("slot_taken: slot %q is not a free slot or patient %q is unknown", slotID, patientID)
	}
	h.nextID++
	id := fmt.Sprintf("apt_other%04d", h.nextID)
	s.status, s.apptID, s.version = "booked", id, s.version+1
	h.appts[id] = &clinicAppt{id: id, ref: fmt.Sprintf("APT-X%05d", h.nextID), slotID: slotID, patientID: patientID, doctorID: s.doctorID,
		startsAt: s.startsAt, status: "booked", version: 1}
	return nil
}

// touchPatient changes a patient record (a new revision), as another user of the
// application would.
func (h *ClinicHost) touchPatient(patientID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.patients[patientID].version++
}

func (h *ClinicHost) count(op adapters.Operation) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c.Operation == op {
			n++
		}
	}
	return n
}

// countFor counts the calls of one operation on one capability.
func (h *ClinicHost) countFor(op adapters.Operation, capabilityID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c.Operation == op && c.CapabilityID == capabilityID {
			n++
		}
	}
	return n
}

func (h *ClinicHost) totalCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *ClinicHost) writes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.effects
}

func (h *ClinicHost) appointment(id string) clinicAppt {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.appts[id]
}

func (h *ClinicHost) slot(id string) clinicSlot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.slots[id]
}

func (h *ClinicHost) patient(id string) clinicPatient {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.patients[id]
}

// appointmentsOf lists the booked appointments of a patient.
func (h *ClinicHost) appointmentsOf(patientID string) []clinicAppt {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []clinicAppt
	for _, a := range h.appts {
		if a.patientID == patientID && a.status == "booked" {
			out = append(out, *a)
		}
	}
	slices.SortFunc(out, func(a, b clinicAppt) int { return a.startsAt.Compare(b.startsAt) })
	return out
}

func (h *ClinicHost) repliesSnapshot() []clinicReply {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]clinicReply(nil), h.replies...)
}

func (h *ClinicHost) callsSnapshot() []clinicCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]clinicCall(nil), h.calls...)
}

// ---- results, as the application writes them ----

type fact = map[string]any

func factText(field, v string) fact {
	return fact{"field": field, "value": map[string]any{"kind": "text", "value": v}}
}

func factInt(field string, v int) fact {
	return fact{"field": field, "value": map[string]any{"kind": "integer", "value": v}}
}

func factBool(field string, v bool) fact {
	return fact{"field": field, "value": map[string]any{"kind": "boolean", "value": v}}
}

func factDate(field, v string) fact {
	return fact{"field": field, "value": map[string]any{"kind": "date", "value": v}}
}

func factIdent(field, v string) fact {
	return fact{"field": field, "value": map[string]any{"kind": "identifier", "value": v}}
}

func factTime(field string, at time.Time) fact {
	return fact{
		"field":   field,
		"value":   map[string]any{"kind": "datetime", "value": at.UTC().Format("2006-01-02T15:04:05Z"), "zone": "UTC"},
		"display": at.UTC().Format("2006-01-02 15:04"),
	}
}

func recordRef(entityType, id string) map[string]any {
	return map[string]any{"entity_type": entityType, "entity_id": id}
}

type dataVersion struct {
	entityType, id string
	revision       int
}

func (v dataVersion) json() map[string]any {
	return map[string]any{"entity_type": v.entityType, "entity_id": v.id, "version": strconv.Itoa(v.revision)}
}

// snapshotVersion is the digest the application reports as freshness.version: over the
// version of every record the result relied on, in a fixed order. Validate and Prepare
// agree on unchanged data and any change to one of those records changes it.
func snapshotVersion(vs []dataVersion) string {
	lines := make([]string, 0, len(vs))
	for _, v := range vs {
		lines = append(lines, fmt.Sprintf("%s:%s:%d", v.entityType, v.id, v.revision))
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "rv1-" + hex.EncodeToString(sum[:])[:32]
}

func (h *ClinicHost) freshness(vs []dataVersion) map[string]any {
	return map[string]any{"as_of": h.clk.Now().UTC().Format("2006-01-02T15:04:05Z"), "version": snapshotVersion(vs)}
}

func okReply(parts map[string]any) map[string]any {
	out := map[string]any{
		"status": "ok", "identifiers": []any{}, "values": []any{}, "errors": []any{}, "allowed_next_actions": []any{},
	}
	for k, v := range parts {
		out[k] = v
	}
	return out
}

// refusal is a host refusal: a rejected result with one typed error.
type refusal struct {
	code, field, message string
	freshness            map[string]any
}

func refuse(code, field, message string) *refusal {
	return &refusal{code: code, field: field, message: message}
}

func outOfScope(field string) *refusal {
	return refuse("scope_violation", field, "the record is outside the caller's access scope or does not exist")
}

func (r *refusal) reply() map[string]any {
	issue := map[string]any{"code": r.code, "message": r.message}
	if r.field != "" {
		issue["field"] = r.field
	}
	out := map[string]any{
		"status": "rejected", "identifiers": []any{}, "values": []any{}, "errors": []any{issue}, "allowed_next_actions": []any{},
	}
	if r.freshness != nil {
		out["freshness"] = r.freshness
	}
	return out
}

// ---- the caller, reauthorised on every request ----

type caller struct {
	principal string
	role      string
	patientID string
	doctorID  string
}

var rolePermissions = map[string][]string{
	"receptionist": {"availability.read", "patients.read", "patients.update_contact", "appointments.book", "appointments.cancel"},
	"doctor":       {"availability.read", "patients.read", "appointments.cancel"},
	"patient":      {"availability.read", "patients.read", "patients.update_contact", "appointments.book", "appointments.cancel"},
}

// resolveCaller looks the principal up in the host's own directory: the roles the
// identity asserts are never trusted, a principal that belongs somewhere else is a
// scope violation and a role without the permission is permission_denied.
func (h *ClinicHost) resolveCaller(id adapters.Identity, permission string) (caller, *refusal) {
	p, ok := h.principals[id.PrincipalID]
	if id.ApplicationID != clinicApp || id.TenantID != clinicTenant || id.WorkspaceID != clinicSite || !ok {
		return caller{}, refuse("scope_violation", "", "the principal is not known in this tenant and workspace")
	}
	for _, r := range id.Roles {
		if r != p.role {
			return caller{}, refuse("permission_denied", "", "the identity asserts a role the principal does not hold")
		}
	}
	if !slices.Contains(rolePermissions[p.role], permission) {
		return caller{}, refuse("permission_denied", "", "the role does not allow this operation")
	}
	return caller{principal: p.id, role: p.role, patientID: p.patientID, doctorID: p.doctorID}, nil
}

func (h *ClinicHost) visiblePatients(c caller) []*clinicPatient {
	var out []*clinicPatient
	for _, p := range h.patients {
		switch c.role {
		case "receptionist":
			out = append(out, p)
		case "patient":
			if p.id == c.patientID {
				out = append(out, p)
			}
		case "doctor":
			for _, a := range h.appts {
				if a.patientID == p.id && a.doctorID == c.doctorID {
					out = append(out, p)
					break
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b *clinicPatient) int {
		return strings.Compare(a.family+a.given+a.dob+a.id, b.family+b.given+b.dob+b.id)
	})
	return out
}

func (h *ClinicHost) visibleAppointments(c caller) []*clinicAppt {
	var out []*clinicAppt
	for _, a := range h.appts {
		switch c.role {
		case "receptionist":
			out = append(out, a)
		case "doctor":
			if a.doctorID == c.doctorID {
				out = append(out, a)
			}
		case "patient":
			if a.patientID == c.patientID {
				out = append(out, a)
			}
		}
	}
	slices.SortFunc(out, func(a, b *clinicAppt) int {
		if c := a.startsAt.Compare(b.startsAt); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	return out
}

func (h *ClinicHost) getPatient(c caller, id, field string) (*clinicPatient, *refusal) {
	for _, p := range h.visiblePatients(c) {
		if p.id == id {
			return p, nil
		}
	}
	return nil, outOfScope(field)
}

func (h *ClinicHost) getSlot(id, field string) (*clinicSlot, *refusal) {
	if s, ok := h.slots[id]; ok {
		return s, nil
	}
	return nil, outOfScope(field)
}

func (h *ClinicHost) getAppointment(c caller, id, field string) (*clinicAppt, *refusal) {
	for _, a := range h.visibleAppointments(c) {
		if a.id == id {
			return a, nil
		}
	}
	return nil, outOfScope(field)
}

func (p *clinicPatient) name() string { return p.given + " " + p.family }

func normalise(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// matchesName: case is ignored and every word of the query must appear in the name.
func matchesName(query, name string) bool {
	hay := normalise(name)
	words := strings.Fields(normalise(query))
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// ---- field parsing: the host treats everything it receives as untrusted ----

func fieldString(fields map[string]any, name string, max int) (string, *refusal) {
	v, present := fields[name]
	if !present || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", refuse("validation_failed", name, name+" must be text")
	}
	s = strings.TrimSpace(s)
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", refuse("validation_failed", name, name+" contains control characters")
		}
	}
	if len([]rune(s)) > max {
		return "", refuse("validation_failed", name, fmt.Sprintf("%s must have at most %d characters", name, max))
	}
	return s, nil
}

func requiredString(fields map[string]any, name string, min, max int) (string, *refusal) {
	s, r := fieldString(fields, name, max)
	if r != nil {
		return "", r
	}
	if s == "" {
		return "", refuse("missing_fields", name, name+" is required")
	}
	if len([]rune(s)) < min {
		return "", refuse("validation_failed", name, fmt.Sprintf("%s must have at least %d characters", name, min))
	}
	return s, nil
}

func requiredReference(fields map[string]any, name string) (string, *refusal) {
	s, r := requiredString(fields, name, 1, 128)
	if r != nil {
		return "", r
	}
	if strings.ContainsAny(s, " \t*") {
		return "", refuse("validation_failed", name, name+" must be a record identifier")
	}
	return s, nil
}

func optionalInt(fields map[string]any, name string, min, max, fallback int) (int, *refusal) {
	v, present := fields[name]
	if !present || v == nil {
		return fallback, nil
	}
	var n int
	switch x := v.(type) {
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0, refuse("validation_failed", name, name+" must be a whole number")
		}
		n = int(i)
	case float64:
		if x != float64(int(x)) {
			return 0, refuse("validation_failed", name, name+" must be a whole number")
		}
		n = int(x)
	case int:
		n = x
	default:
		return 0, refuse("validation_failed", name, name+" must be a whole number")
	}
	if n < min || n > max {
		return 0, refuse("validation_failed", name, fmt.Sprintf("%s must be between %d and %d", name, min, max))
	}
	return n, nil
}

func optionalDate(fields map[string]any, name string) (string, *refusal) {
	s, r := fieldString(fields, name, 32)
	if r != nil || s == "" {
		return "", r
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return "", refuse("validation_failed", name, name+" must be a calendar date written YYYY-MM-DD")
	}
	return s, nil
}

func allowOnly(fields map[string]any, allowed ...string) *refusal {
	for k := range fields {
		if !slices.Contains(allowed, k) {
			return refuse("validation_failed", k, k+" is not a field of this capability")
		}
	}
	return nil
}

// ---- the operations ----

// handle processes one request and returns the host members of the reply.
func (h *ClinicHost) handle(id adapters.Identity, op adapters.Operation, capabilityID string, fields map[string]any, prepared map[string]any, key string) map[string]any {
	// A resolution is a Read of the capability that declares the field, with the reserved
	// arguments entity_type and mention: the transport maps it to the resolver tool the
	// package declares for the entity type.
	if op == adapters.OpRead && fields["entity_type"] != nil {
		return h.resolve(id, fields)
	}
	switch capabilityID {
	case "availability.list":
		if op == adapters.OpRead {
			return h.availabilityList(id, fields)
		}
	case "patients.find":
		if op == adapters.OpRead {
			return h.patientsFind(id, fields)
		}
	case "appointments.book", "appointments.cancel", "patients.update_contact":
		switch op {
		case adapters.OpValidate, adapters.OpPrepare:
			return h.evaluate(id, capabilityID, op, fields)
		case adapters.OpCommit:
			return h.commit(id, capabilityID, key, prepared)
		case adapters.OpStatus:
			return h.status(id, capabilityID, key)
		}
	}
	return refuse("unknown_capability", "", "the application does not offer this operation for this capability").reply()
}

func (h *ClinicHost) availabilityList(id adapters.Identity, fields map[string]any) map[string]any {
	c, r := h.resolveCaller(id, "availability.read")
	_ = c
	if r != nil {
		return r.reply()
	}
	if r := allowOnly(fields, "date", "days", "doctor", "max_results"); r != nil {
		return r.reply()
	}
	now := h.clk.Now().UTC()
	today := now.Format("2006-01-02")
	first, r := optionalDate(fields, "date")
	if r != nil {
		return r.reply()
	}
	if first == "" {
		first = today
	}
	firstDay, _ := time.Parse("2006-01-02", first)
	todayDay, _ := time.Parse("2006-01-02", today)
	if first < today || firstDay.Sub(todayDay) > maxAheadDays*24*time.Hour {
		return refuse("validation_failed", "date", fmt.Sprintf("date must be between today and %d days ahead", maxAheadDays)).reply()
	}
	days, r := optionalInt(fields, "days", 1, 14, 1)
	if r != nil {
		return r.reply()
	}
	maxResults, r := optionalInt(fields, "max_results", 1, 10, 5)
	if r != nil {
		return r.reply()
	}
	doctorRef, r := fieldString(fields, "doctor", 128)
	if r != nil {
		return r.reply()
	}
	if doctorRef != "" {
		if _, ok := h.doctors[doctorRef]; !ok {
			return outOfScope("doctor").reply()
		}
	}
	earliest := now.Add(minLeadMinutes * time.Minute)
	from := firstDay
	if earliest.After(from) {
		from = earliest
	}
	to := firstDay.AddDate(0, 0, days)
	var free []*clinicSlot
	for _, s := range h.slots {
		if s.status == "free" && !s.startsAt.Before(from) && s.startsAt.Before(to) && (doctorRef == "" || s.doctorID == doctorRef) {
			free = append(free, s)
		}
	}
	slices.SortFunc(free, func(a, b *clinicSlot) int {
		if c := a.startsAt.Compare(b.startsAt); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	more := len(free) > maxResults
	if more {
		free = free[:maxResults]
	}
	summary := "no free appointments"
	if len(free) > 0 {
		parts := make([]string, len(free))
		for i, s := range free {
			parts[i] = s.startsAt.Format("2006-01-02 15:04") + " " + h.doctors[s.doctorID].name
		}
		summary = strings.Join(parts, "; ")
	}
	values := []any{
		factDate("search_from", first), factInt("slot_count", len(free)), factBool("more_available", more), factText("slot_summary", summary),
	}
	var ids, vers []any
	var dvs []dataVersion
	for i, s := range free {
		n := i + 1
		values = append(values, factTime(fmt.Sprintf("slot_%d_starts_at", n), s.startsAt), factText(fmt.Sprintf("slot_%d_doctor", n), h.doctors[s.doctorID].name))
		ids = append(ids, recordRef("slot", s.id))
		dvs = append(dvs, dataVersion{"slot", s.id, s.version})
	}
	_ = vers
	next := []any{}
	if len(free) > 0 && slices.Contains(rolePermissions[c.role], "appointments.book") {
		next = append(next, map[string]any{"action": "invoke_capability", "capability_id": "appointments.book", "label": "Book one of these appointments"})
	}
	if ids == nil {
		ids = []any{}
	}
	return okReply(map[string]any{"identifiers": ids, "values": values, "freshness": h.freshness(dvs), "allowed_next_actions": next})
}

func (h *ClinicHost) patientsFind(id adapters.Identity, fields map[string]any) map[string]any {
	c, r := h.resolveCaller(id, "patients.read")
	if r != nil {
		return r.reply()
	}
	if r := allowOnly(fields, "query", "date_of_birth", "max_results"); r != nil {
		return r.reply()
	}
	query, r := requiredString(fields, "query", 2, 100)
	if r != nil {
		return r.reply()
	}
	dob, r := optionalDate(fields, "date_of_birth")
	if r != nil {
		return r.reply()
	}
	if dob != "" && dob > h.clk.Now().UTC().Format("2006-01-02") {
		return refuse("validation_failed", "date_of_birth", "date_of_birth cannot be in the future").reply()
	}
	maxResults, r := optionalInt(fields, "max_results", 1, 10, 5)
	if r != nil {
		return r.reply()
	}
	var matches []*clinicPatient
	if strings.HasPrefix(query, "pat_") {
		p, rr := h.getPatient(c, query, "query")
		if rr != nil {
			return rr.reply()
		}
		matches = []*clinicPatient{p}
	} else {
		for _, p := range h.visiblePatients(c) {
			if matchesName(query, p.name()) {
				matches = append(matches, p)
			}
		}
	}
	if dob != "" {
		var kept []*clinicPatient
		for _, p := range matches {
			if p.dob == dob {
				kept = append(kept, p)
			}
		}
		matches = kept
	}
	listed := matches
	if len(listed) > maxResults {
		listed = listed[:maxResults]
	}
	summary := "no matching patients"
	if len(listed) > 0 {
		parts := make([]string, len(listed))
		for i, p := range listed {
			parts[i] = fmt.Sprintf("%s (born %s)", p.name(), p.dob)
		}
		summary = strings.Join(parts, "; ")
	}
	values := []any{factInt("patient_count", len(listed)), factBool("more_available", len(matches) > len(listed)), factText("patient_summary", summary)}
	ids := []any{}
	var dvs []dataVersion
	for i, p := range listed {
		values = append(values, factText(fmt.Sprintf("patient_%d_name", i+1), p.name()), factDate(fmt.Sprintf("patient_%d_date_of_birth", i+1), p.dob))
		ids = append(ids, recordRef("patient", p.id))
		dvs = append(dvs, dataVersion{"patient", p.id, p.version})
	}
	next := []any{}
	if len(listed) > 0 {
		if slices.Contains(rolePermissions[c.role], "appointments.book") {
			next = append(next, map[string]any{"action": "invoke_capability", "capability_id": "appointments.book", "label": "Book an appointment for one of these patients"})
		}
		if slices.Contains(rolePermissions[c.role], "patients.update_contact") {
			next = append(next, map[string]any{"action": "invoke_capability", "capability_id": "patients.update_contact", "label": "Update the contact details of one of these patients"})
		}
	}
	return okReply(map[string]any{"identifiers": ids, "values": values, "freshness": h.freshness(dvs), "allowed_next_actions": next})
}

// ---- entity resolvers: reads that decide nothing ----

type candidate struct {
	id      string
	version int
	label   string
}

func (h *ClinicHost) candidates(entityType string, cs []candidate) map[string]any {
	ids := []any{}
	values := []any{factInt("candidate_count", len(cs))}
	var dvs []dataVersion
	for _, c := range cs {
		ids = append(ids, recordRef(entityType, c.id))
		values = append(values, factText("label", c.label))
		dvs = append(dvs, dataVersion{entityType, c.id, c.version})
	}
	return okReply(map[string]any{"identifiers": ids, "values": values, "freshness": h.freshness(dvs)})
}

var (
	nextWords  = []string{"next", "earliest", "soonest", "first", "prossimo", "prossima", "primo", "prima"}
	nextThings = []string{"available", "free", "appointment", "slot", "one", "opening", "disponibile", "libero", "libera", "orario", "appuntamento", "visita"}
	selfWords  = []string{"me", "myself", "i", "my", "io", "mio", "mia"}
)

func asksForNextAvailable(mention string) bool {
	words := strings.FieldsFunc(normalise(mention), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	hasNext, hasThing := false, false
	for _, w := range words {
		hasNext = hasNext || slices.Contains(nextWords, w)
		hasThing = hasThing || slices.Contains(nextThings, w)
	}
	return hasNext && hasThing
}

// resolve answers a resolution read. The candidate_id the model proposed is checked for
// its shape and otherwise ignored when there is a mention: it never narrows several
// matches to one.
func (h *ClinicHost) resolve(id adapters.Identity, fields map[string]any) map[string]any {
	entityType, _ := fields["entity_type"].(string)
	permission := map[string]string{
		"patient": "patients.read", "doctor": "availability.read", "slot": "availability.read", "appointment": "appointments.cancel",
	}[entityType]
	if permission == "" {
		return refuse("validation_failed", "entity_type", "this host resolves no such entity type").reply()
	}
	c, r := h.resolveCaller(id, permission)
	if r != nil {
		return r.reply()
	}
	if r := allowOnly(fields, "entity_type", "mention", "candidate_id"); r != nil {
		return r.reply()
	}
	hint, r := fieldString(fields, "candidate_id", 128)
	if r != nil {
		return r.reply()
	}
	if strings.ContainsAny(hint, " \t*") {
		return refuse("validation_failed", "candidate_id", "candidate_id must be a record identifier").reply()
	}
	mention, r := fieldString(fields, "mention", 100)
	if r != nil {
		return r.reply()
	}
	if mention == "" {
		if hint == "" {
			return refuse("missing_fields", "mention", "mention is required").reply()
		}
		mention = hint
	}
	now := h.clk.Now().UTC()
	switch entityType {
	case "patient":
		visible := h.visiblePatients(c)
		var found []*clinicPatient
		switch {
		case strings.HasPrefix(mention, "pat_"):
			for _, p := range visible {
				if p.id == mention {
					found = append(found, p)
				}
			}
			if len(found) == 0 {
				return outOfScope("mention").reply()
			}
		case c.role == "patient" && slices.Contains(selfWords, normalise(mention)):
			found = visible
		default:
			for _, p := range visible {
				if matchesName(mention, p.name()) {
					found = append(found, p)
				}
			}
		}
		var cs []candidate
		for _, p := range found {
			cs = append(cs, candidate{p.id, p.version, fmt.Sprintf("%s (born %s)", p.name(), p.dob)})
		}
		return h.candidates("patient", cs)
	case "doctor":
		var found []*clinicDoctor
		var all []*clinicDoctor
		for _, d := range h.doctors {
			all = append(all, d)
		}
		slices.SortFunc(all, func(a, b *clinicDoctor) int { return strings.Compare(a.name+a.id, b.name+b.id) })
		if strings.HasPrefix(mention, "doc_") {
			for _, d := range all {
				if d.id == mention {
					found = append(found, d)
				}
			}
			if len(found) == 0 {
				return outOfScope("mention").reply()
			}
		} else {
			cleaned := strings.TrimSpace(strings.NewReplacer("dottoressa", " ", "dottor", " ", "dott.", " ", "doctor", " ", "dr.", " ", "dr", " ").Replace(normalise(mention)))
			if cleaned != "" {
				for _, d := range all {
					if matchesName(cleaned, d.name) {
						found = append(found, d)
					}
				}
			}
		}
		var cs []candidate
		for _, d := range found {
			cs = append(cs, candidate{d.id, 1, fmt.Sprintf("%s (%s)", d.name, d.specialty)})
		}
		return h.candidates("doctor", cs)
	case "slot":
		var free []*clinicSlot
		for _, s := range h.slots {
			if s.status == "free" && s.startsAt.After(now) {
				free = append(free, s)
			}
		}
		slices.SortFunc(free, func(a, b *clinicSlot) int {
			if c := a.startsAt.Compare(b.startsAt); c != 0 {
				return c
			}
			return strings.Compare(a.id, b.id)
		})
		var found []*clinicSlot
		switch {
		case strings.HasPrefix(mention, "slot_"):
			for _, s := range free {
				if s.id == mention {
					found = append(found, s)
				}
			}
			if len(found) == 0 {
				if _, exists := h.slots[mention]; !exists {
					return outOfScope("mention").reply()
				}
			}
		case slotMention(mention):
			day, hm := mention[:10], ""
			if len(mention) > 10 {
				hm = strings.TrimSpace(mention[10:])
				hm = strings.TrimPrefix(hm, "T")
			}
			for _, s := range free {
				shown := s.startsAt.Format("2006-01-02 15:04")
				if (hm == "" && strings.HasPrefix(shown, day)) || (hm != "" && shown == day+" "+hm) {
					found = append(found, s)
				}
			}
		case asksForNextAvailable(mention):
			bookable := now.Add(minLeadMinutes * time.Minute)
			for _, s := range free {
				if !s.startsAt.Before(bookable) {
					found = append(found, s)
					break
				}
			}
		default:
			return refuse("validation_failed", "mention", "a slot is named by its identifier, by YYYY-MM-DD HH:MM or as the next available one").reply()
		}
		var cs []candidate
		for _, s := range found {
			cs = append(cs, candidate{s.id, s.version, fmt.Sprintf("%s with %s", s.startsAt.Format("2006-01-02 15:04"), h.doctors[s.doctorID].name)})
		}
		return h.candidates("slot", cs)
	default: // appointment
		var visible []*clinicAppt
		for _, a := range h.visibleAppointments(c) {
			if a.status == "booked" && a.startsAt.After(now) {
				visible = append(visible, a)
			}
		}
		var found []*clinicAppt
		switch {
		case strings.HasPrefix(mention, "apt_"):
			for _, a := range visible {
				if a.id == mention {
					found = append(found, a)
				}
			}
			if len(found) == 0 {
				return outOfScope("mention").reply()
			}
		case len(mention) == 10 && strings.HasPrefix(strings.ToUpper(mention), "APT-"):
			for _, a := range visible {
				if a.ref == strings.ToUpper(mention) {
					found = append(found, a)
				}
			}
		case slotMention(mention):
			day := mention[:10]
			for _, a := range visible {
				if strings.HasPrefix(a.startsAt.Format("2006-01-02 15:04"), day) {
					found = append(found, a)
				}
			}
		default:
			for _, a := range visible {
				if matchesName(mention, h.patients[a.patientID].name()) {
					found = append(found, a)
				}
			}
		}
		var cs []candidate
		for _, a := range found {
			cs = append(cs, candidate{a.id, a.version, fmt.Sprintf("%s: %s, %s with %s", a.ref, h.patients[a.patientID].name(),
				a.startsAt.Format("2006-01-02 15:04"), h.doctors[a.doctorID].name)})
		}
		return h.candidates("appointment", cs)
	}
}

// slotMention reports whether a mention is YYYY-MM-DD, optionally followed by HH:MM.
func slotMention(s string) bool {
	if len(s) < 10 {
		return false
	}
	if _, err := time.Parse("2006-01-02", s[:10]); err != nil {
		return false
	}
	rest := strings.TrimSpace(s[10:])
	rest = strings.TrimPrefix(rest, "T")
	if rest == "" {
		return len(s) == 10
	}
	_, err := time.Parse("15:04", rest)
	return err == nil
}

// ---- consequential capabilities: validate, prepare, commit, status ----

// loaded is what an action relies on: the records it names and their revisions.
type loaded struct {
	patient *clinicPatient
	slot    *clinicSlot
	doctor  *clinicDoctor
	appt    *clinicAppt
	reason  string
	phone   string
	email   string
	// versions are the records the action relied on, each with its own revision.
	versions []dataVersion
}

var permissionOf = map[string]string{
	"appointments.book": "appointments.book", "appointments.cancel": "appointments.cancel", "patients.update_contact": "patients.update_contact",
}

func (h *ClinicHost) load(c caller, capabilityID string, fields map[string]any) (*loaded, *refusal) {
	l := &loaded{}
	switch capabilityID {
	case "appointments.book":
		if r := allowOnly(fields, "patient", "slot", "reason"); r != nil {
			return nil, r
		}
		pref, r := requiredReference(fields, "patient")
		if r != nil {
			return nil, r
		}
		sref, r := requiredReference(fields, "slot")
		if r != nil {
			return nil, r
		}
		if l.reason, r = fieldString(fields, "reason", 200); r != nil {
			return nil, r
		}
		if l.patient, r = h.getPatient(c, pref, "patient"); r != nil {
			return nil, r
		}
		if l.slot, r = h.getSlot(sref, "slot"); r != nil {
			return nil, r
		}
		l.doctor = h.doctors[l.slot.doctorID]
		l.versions = []dataVersion{{"slot", l.slot.id, l.slot.version}, {"patient", l.patient.id, l.patient.version}}
	case "appointments.cancel":
		if r := allowOnly(fields, "appointment", "reason"); r != nil {
			return nil, r
		}
		aref, r := requiredReference(fields, "appointment")
		if r != nil {
			return nil, r
		}
		reason, r := fieldString(fields, "reason", 64)
		if r != nil {
			return nil, r
		}
		if reason != "" && !slices.Contains([]string{"patient_request", "provider_unavailable", "scheduling_error", "other"}, reason) {
			return nil, refuse("validation_failed", "reason", "reason must be one of: patient_request, provider_unavailable, scheduling_error, other")
		}
		l.reason = reason
		if l.appt, r = h.getAppointment(c, aref, "appointment"); r != nil {
			return nil, r
		}
		l.patient, l.doctor = h.patients[l.appt.patientID], h.doctors[l.appt.doctorID]
		l.versions = []dataVersion{{"appointment", l.appt.id, l.appt.version}}
	case "patients.update_contact":
		if r := allowOnly(fields, "patient", "phone", "email"); r != nil {
			return nil, r
		}
		pref, r := requiredReference(fields, "patient")
		if r != nil {
			return nil, r
		}
		if l.phone, r = fieldString(fields, "phone", 32); r != nil {
			return nil, r
		}
		if l.phone != "" {
			digits := 0
			for _, ch := range l.phone {
				switch {
				case unicode.IsDigit(ch):
					digits++
				case !strings.ContainsRune("+-(). ", ch):
					return nil, refuse("validation_failed", "phone", "phone must hold at least 5 digits and only digits, spaces and + - ( ) .")
				}
			}
			if digits < 5 || len(l.phone) < 5 {
				return nil, refuse("validation_failed", "phone", "phone must hold at least 5 digits and only digits, spaces and + - ( ) .")
			}
		}
		if l.email, r = fieldString(fields, "email", 254); r != nil {
			return nil, r
		}
		if l.email != "" {
			at := strings.Index(l.email, "@")
			if at < 1 || !strings.Contains(l.email[at+1:], ".") || strings.ContainsAny(l.email, " \t") {
				return nil, refuse("validation_failed", "email", "email must be an address such as name@example.test")
			}
			l.email = strings.ToLower(l.email)
		}
		if l.patient, r = h.getPatient(c, pref, "patient"); r != nil {
			return nil, r
		}
		l.versions = []dataVersion{{"patient", l.patient.id, l.patient.version}}
	}
	return l, nil
}

// check runs the business rules. They run on validate, on prepare and again on commit.
func (h *ClinicHost) check(capabilityID string, l *loaded) *refusal {
	now := h.clk.Now().UTC()
	switch capabilityID {
	case "appointments.book":
		lead := l.slot.startsAt.Sub(now)
		if lead < minLeadMinutes*time.Minute || lead > maxAheadDays*24*time.Hour {
			return refuse("validation_failed", "slot", fmt.Sprintf("an appointment can be booked from %d minutes to %d days ahead", minLeadMinutes, maxAheadDays))
		}
		if l.slot.status != "free" {
			return refuse("conflict", "slot", "the appointment slot is no longer free")
		}
		for _, a := range h.appts {
			if a.patientID != l.patient.id || a.status != "booked" {
				continue
			}
			end := a.startsAt.Add(time.Duration(h.slots[a.slotID].minutes) * time.Minute)
			if a.startsAt.Before(l.slot.startsAt.Add(time.Duration(l.slot.minutes)*time.Minute)) && l.slot.startsAt.Before(end) {
				return refuse("conflict", "patient", "the patient already has an appointment at that time")
			}
		}
	case "appointments.cancel":
		if l.appt.status != "booked" {
			return refuse("conflict", "appointment", "the appointment is already cancelled")
		}
		if !l.appt.startsAt.After(now) {
			return refuse("validation_failed", "appointment", "an appointment that has started or passed cannot be cancelled")
		}
	case "patients.update_contact":
		if l.phone == "" && l.email == "" {
			return refuse("validation_failed", "", "give a new phone number, a new email address or both")
		}
	}
	return nil
}

type description struct {
	values  []any
	summary string
	effects []any
	next    []any
	risk    int
	ttl     int
}

func (h *ClinicHost) describe(capabilityID string, l *loaded) description {
	switch capabilityID {
	case "appointments.book":
		name, when := l.patient.name(), l.slot.startsAt.Format("2006-01-02 15:04")
		changes := []any{factIdent("patient_id", l.patient.id), factIdent("slot_id", l.slot.id), factTime("appointment_starts_at", l.slot.startsAt)}
		if l.reason != "" {
			changes = append(changes, factText("reason", l.reason))
		}
		return description{
			values:  []any{factText("patient_name", name), factText("doctor_name", l.doctor.name), factTime("appointment_starts_at", l.slot.startsAt), factText("site_name", clinicSiteName)},
			summary: fmt.Sprintf("Book %s with %s on %s at %s.", name, l.doctor.name, when, clinicSiteName),
			effects: []any{
				map[string]any{"operation": "create", "entity_type": "appointment",
					"description": fmt.Sprintf("Create an appointment for %s with %s on %s at %s.", name, l.doctor.name, when, clinicSiteName),
					"changes":     changes, "external": false},
				map[string]any{"operation": "send", "entity_type": "notification", "description": "Send a booking confirmation message to the patient's contact details on file.",
					"changes": []any{factText("message_kind", "booking_confirmation")}, "external": true},
			},
			next: []any{map[string]any{"action": "confirm"}, map[string]any{"action": "cancel"}}, risk: 2, ttl: bookingTTL,
		}
	case "appointments.cancel":
		name, when := l.patient.name(), l.appt.startsAt.Format("2006-01-02 15:04")
		changes := []any{factText("appointment_status", "cancelled")}
		if l.reason != "" {
			changes = append(changes, factText("cancel_reason", l.reason))
		}
		return description{
			values: []any{factText("appointment_reference", l.appt.ref), factTime("appointment_starts_at", l.appt.startsAt), factText("patient_name", name),
				factText("doctor_name", l.doctor.name), factText("site_name", clinicSiteName)},
			summary: fmt.Sprintf("Cancel appointment %s for %s with %s on %s and release the time slot.", l.appt.ref, name, l.doctor.name, when),
			effects: []any{
				map[string]any{"operation": "update", "entity_type": "appointment", "entity_id": l.appt.id,
					"description": fmt.Sprintf("Cancel appointment %s for %s on %s and release its time slot.", l.appt.ref, name, when),
					"changes":     changes, "external": false},
				map[string]any{"operation": "send", "entity_type": "notification", "description": "Send a cancellation message to the patient's contact details on file.",
					"changes": []any{factText("message_kind", "cancellation_notice")}, "external": true},
			},
			next: []any{map[string]any{"action": "confirm"}, map[string]any{"action": "cancel"}}, risk: 2, ttl: cancelTTL,
		}
	default: // patients.update_contact
		name := l.patient.name()
		var changes []any
		var which []string
		if l.phone != "" {
			changes, which = append(changes, factText("phone", l.phone)), append(which, "phone")
		}
		if l.email != "" {
			changes, which = append(changes, factText("email", l.email)), append(which, "email")
		}
		return description{
			values:  []any{factText("patient_name", name), factText("contact_fields_updated", strings.Join(which, ", "))},
			summary: fmt.Sprintf("Update the %s on file for %s.", strings.Join(which, " and "), name),
			effects: []any{map[string]any{"operation": "update", "entity_type": "patient", "entity_id": l.patient.id,
				"description": fmt.Sprintf("Update the contact details on file for %s.", name), "changes": changes, "external": false}},
			next: []any{map[string]any{"action": "cancel"}}, risk: 1, ttl: contactTTL,
		}
	}
}

func (h *ClinicHost) evaluate(id adapters.Identity, capabilityID string, op adapters.Operation, fields map[string]any) map[string]any {
	c, r := h.resolveCaller(id, permissionOf[capabilityID])
	if r != nil {
		return r.reply()
	}
	l, r := h.load(c, capabilityID, fields)
	if r != nil {
		return r.reply()
	}
	if r := h.check(capabilityID, l); r != nil {
		r.freshness = h.freshness(l.versions)
		return r.reply()
	}
	d := h.describe(capabilityID, l)
	if op == adapters.OpValidate {
		return okReply(map[string]any{"values": d.values, "freshness": h.freshness(l.versions)})
	}
	dvs := make([]any, len(l.versions))
	for i, v := range l.versions {
		dvs[i] = v.json()
	}
	return okReply(map[string]any{
		"values": d.values, "freshness": h.freshness(l.versions), "allowed_next_actions": d.next,
		"preparation": map[string]any{
			"summary": d.summary, "effects": d.effects, "data_versions": dvs, "declared_risk_level": d.risk, "valid_for_seconds": d.ttl,
		},
	})
}

// preparedFields re-derives the candidate fields from the exact effects the user was
// shown: the host reads only what it needs and treats the rest of the prepared action as
// untrusted.
func preparedFields(capabilityID string, prepared map[string]any) (map[string]any, []dataVersion, *refusal) {
	bad := func(msg string) *refusal { return refuse("validation_failed", "prepared", msg) }
	effects, _ := prepared["effects"].([]any)
	versionsRaw, _ := prepared["data_versions"].([]any)
	if len(effects) == 0 || versionsRaw == nil {
		return nil, nil, bad("the prepared action is incomplete")
	}
	var versions []dataVersion
	for _, v := range versionsRaw {
		m, _ := v.(map[string]any)
		et, _ := m["entity_type"].(string)
		eid, _ := m["entity_id"].(string)
		vs, _ := m["version"].(string)
		n, err := strconv.Atoi(vs)
		if et == "" || eid == "" || err != nil {
			return nil, nil, bad("a data version of the prepared action is malformed")
		}
		versions = append(versions, dataVersion{et, eid, n})
	}
	effect := func(op, entityType string) (map[string]any, map[string]any) {
		for _, e := range effects {
			m, _ := e.(map[string]any)
			if m["operation"] == op && m["entity_type"] == entityType {
				changes := map[string]any{}
				cs, _ := m["changes"].([]any)
				for _, ch := range cs {
					cm, _ := ch.(map[string]any)
					vm, _ := cm["value"].(map[string]any)
					if f, ok := cm["field"].(string); ok {
						changes[f] = vm["value"]
					}
				}
				return m, changes
			}
		}
		return nil, nil
	}
	fields := map[string]any{}
	switch capabilityID {
	case "appointments.book":
		m, ch := effect("create", "appointment")
		if m == nil {
			return nil, nil, bad("the prepared action does not hold the effect this capability prepares")
		}
		fields["patient"], fields["slot"] = ch["patient_id"], ch["slot_id"]
		if ch["reason"] != nil {
			fields["reason"] = ch["reason"]
		}
	case "appointments.cancel":
		m, ch := effect("update", "appointment")
		if m == nil {
			return nil, nil, bad("the prepared action does not hold the effect this capability prepares")
		}
		fields["appointment"] = m["entity_id"]
		if ch["cancel_reason"] != nil {
			fields["reason"] = ch["cancel_reason"]
		}
	default:
		m, ch := effect("update", "patient")
		if m == nil {
			return nil, nil, bad("the prepared action does not hold the effect this capability prepares")
		}
		fields["patient"] = m["entity_id"]
		for _, k := range []string{"phone", "email"} {
			if ch[k] != nil {
				fields[k] = ch[k]
			}
		}
	}
	return fields, versions, nil
}

func fingerprintOf(capabilityID, principal string, fields map[string]any) string {
	b, _ := json.Marshal(map[string]any{"capability": capabilityID, "principal": principal, "fields": fields})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (h *ClinicHost) commit(id adapters.Identity, capabilityID, key string, preparedAny map[string]any) map[string]any {
	c, r := h.resolveCaller(id, permissionOf[capabilityID])
	if r != nil {
		return r.reply()
	}
	fields, seenVersions, r := preparedFields(capabilityID, preparedAny)
	if r != nil {
		return r.reply()
	}
	fp := fingerprintOf(capabilityID, c.principal, fields)
	// A repeat answers with the original reply and changes nothing; the same key for
	// anything else is a conflict, never a second effect.
	if prev, seen := h.keys[key]; seen {
		if prev.capability != capabilityID || prev.principal != c.principal || prev.fingerprint != fp {
			return refuse("conflict", "idempotency_key", "the idempotency key was already used for a different action").reply()
		}
		return prev.reply
	}
	if exp, _ := preparedAny["expires_at"].(string); exp != "" {
		if at, err := time.Parse(time.RFC3339, exp); err == nil && !h.clk.Now().Before(at) {
			return refuse("expired", "prepared", "the prepared action expired before it was committed").reply()
		}
	}
	l, r := h.load(c, capabilityID, fields)
	if r != nil {
		return r.reply()
	}
	// First the data the user confirmed, then the rules: a record that moved is stale,
	// whatever else changed with it.
	for _, cur := range l.versions {
		var seen *dataVersion
		for i := range seenVersions {
			if seenVersions[i].entityType == cur.entityType && seenVersions[i].id == cur.id {
				seen = &seenVersions[i]
			}
		}
		if seen == nil {
			return refuse("validation_failed", "prepared", "the prepared action does not record the data it relied on").reply()
		}
		if seen.revision != cur.revision {
			rr := refuse("stale", "", "a record changed after the action was prepared")
			rr.freshness = h.freshness(l.versions)
			return rr.reply()
		}
	}
	if r := h.check(capabilityID, l); r != nil {
		r.freshness = h.freshness(l.versions)
		return r.reply()
	}
	reply := h.apply(capabilityID, l)
	h.keys[key] = clinicKey{capability: capabilityID, principal: c.principal, fingerprint: fp, reply: reply}
	h.effects++
	return reply
}

func (h *ClinicHost) apply(capabilityID string, l *loaded) map[string]any {
	now := h.clk.Now().UTC()
	h.nextID++
	followUp := map[string]any{"reference": fmt.Sprintf("ntf_%010d", h.nextID), "state": "queued", "as_of": now.Format("2006-01-02T15:04:05Z")}
	switch capabilityID {
	case "appointments.book":
		apptID, ref := fmt.Sprintf("apt_new%06d", h.nextID), fmt.Sprintf("APT-Q%05d", h.nextID)
		h.appts[apptID] = &clinicAppt{id: apptID, ref: ref, slotID: l.slot.id, patientID: l.patient.id, doctorID: l.doctor.id,
			startsAt: l.slot.startsAt, status: "booked", reason: l.reason, version: 1}
		l.slot.status, l.slot.apptID = "booked", apptID
		l.slot.version++
		return okReply(map[string]any{
			"identifiers": []any{recordRef("appointment", apptID), recordRef("notification", followUp["reference"].(string))},
			"values": []any{factText("appointment_reference", ref), factText("appointment_status", "booked"), factText("patient_name", l.patient.name()),
				factText("doctor_name", l.doctor.name), factTime("appointment_starts_at", l.slot.startsAt), factText("site_name", clinicSiteName)},
			"freshness":            h.freshness([]dataVersion{{"slot", l.slot.id, l.slot.version}, {"patient", l.patient.id, l.patient.version}}),
			"follow_up":            followUp,
			"allowed_next_actions": []any{map[string]any{"action": "invoke_capability", "capability_id": "appointments.cancel", "label": "Cancel this appointment"}},
		})
	case "appointments.cancel":
		l.appt.status, l.appt.cancelReason = "cancelled", l.reason
		l.appt.version++
		if s := h.slots[l.appt.slotID]; s != nil && s.apptID == l.appt.id {
			s.status, s.apptID = "free", ""
			s.version++
		}
		return okReply(map[string]any{
			"identifiers": []any{recordRef("appointment", l.appt.id), recordRef("notification", followUp["reference"].(string))},
			"values": []any{factText("appointment_reference", l.appt.ref), factText("appointment_status", "cancelled"), factTime("appointment_starts_at", l.appt.startsAt),
				factText("patient_name", l.patient.name()), factText("doctor_name", l.doctor.name)},
			"freshness":            h.freshness([]dataVersion{{"appointment", l.appt.id, l.appt.version}}),
			"follow_up":            followUp,
			"allowed_next_actions": []any{map[string]any{"action": "invoke_capability", "capability_id": "appointments.book", "label": "Book a new appointment"}},
		})
	default:
		var which []string
		if l.phone != "" {
			l.patient.phone = l.phone
			which = append(which, "phone")
		}
		if l.email != "" {
			l.patient.email = l.email
			which = append(which, "email")
		}
		l.patient.version++
		return okReply(map[string]any{
			"identifiers": []any{recordRef("patient", l.patient.id)},
			"values":      []any{factText("patient_name", l.patient.name()), factText("contact_fields_updated", strings.Join(which, ", "))},
			"freshness":   h.freshness([]dataVersion{{"patient", l.patient.id, l.patient.version}}),
		})
	}
}

// status says what was recorded for a key, and "nothing" in the one shape that proves
// nothing was applied: rejected, one validation_failed on idempotency_key, no identifiers.
func (h *ClinicHost) status(id adapters.Identity, capabilityID, key string) map[string]any {
	c, r := h.resolveCaller(id, permissionOf[capabilityID])
	if r != nil {
		return r.reply()
	}
	stored, ok := h.keys[key]
	if !ok {
		return refuse("validation_failed", "idempotency_key", "the application has no record of this idempotency key").reply()
	}
	if stored.principal != c.principal {
		return refuse("permission_denied", "", "the idempotency key belongs to another principal").reply()
	}
	if stored.capability != capabilityID {
		return refuse("conflict", "idempotency_key", "the idempotency key was used for another capability").reply()
	}
	return stored.reply
}

// ---- the adapter ----

// clinicAdapter is an adapters.AppAdapter over the host with no wire: it sends only the
// identity and the request, reads the reply as raw JSON and normalises it, never
// retries, and reports an unreachable host as unavailable. It sits inside adapters.Wrap
// in the tests, as a real transport does in production.
type clinicAdapter struct{ h *ClinicHost }

var _ adapters.AppAdapter = clinicAdapter{}

func (a clinicAdapter) call(ctx context.Context, scope contracts.Scope, op adapters.Operation, capabilityID string, fields map[string]any, prepared *act.PreparedAction, key string) (act.DeterministicResult, error) {
	if err := ctx.Err(); err != nil {
		return act.DeterministicResult{}, err
	}
	h := a.h
	var preparedMap map[string]any
	if prepared != nil {
		b, err := json.Marshal(prepared)
		if err != nil {
			return act.DeterministicResult{}, contracts.Wrap(contracts.ErrInternal, err, "the prepared action cannot be encoded")
		}
		if err := json.Unmarshal(b, &preparedMap); err != nil {
			return act.DeterministicResult{}, contracts.Wrap(contracts.ErrInternal, err, "the prepared action cannot be decoded")
		}
	}
	h.mu.Lock()
	if h.down {
		h.mu.Unlock()
		return act.DeterministicResult{}, contracts.New(contracts.ErrUnavailable, "the application is unreachable")
	}
	cp := map[string]any{}
	for k, v := range fields {
		cp[k] = v
	}
	h.calls = append(h.calls, clinicCall{Operation: op, CapabilityID: capabilityID, Principal: scope.PrincipalID, Fields: cp})
	body := h.handle(adapters.IdentityFrom(scope), op, capabilityID, fields, preparedMap, key)
	raw, err := json.Marshal(body)
	if err != nil {
		h.mu.Unlock()
		return act.DeterministicResult{}, contracts.Wrap(contracts.ErrInternal, err, "the host reply cannot be encoded")
	}
	h.replies = append(h.replies, clinicReply{Operation: op, CapabilityID: capabilityID, Fields: cp, Body: raw})
	h.mu.Unlock()
	return adapters.Normalise(raw, adapters.Expect{Operation: op, CapabilityID: capabilityID, IdempotencyKey: key})
}

func (a clinicAdapter) Describe(context.Context) (adapters.Description, error) {
	ops := func(id string, o ...adapters.Operation) adapters.CapabilityOps {
		return adapters.CapabilityOps{CapabilityID: id, Operations: o}
	}
	return adapters.Description{
		Transport: "native", ContractVersions: []int{adapters.ContractVersion}, ApplicationVersion: "1.0.0",
		Capabilities: []adapters.CapabilityOps{
			ops("availability.list", adapters.OpRead), ops("patients.find", adapters.OpRead),
			ops("appointments.book", adapters.OpRead, adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus),
			ops("appointments.cancel", adapters.OpRead, adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus),
			ops("patients.update_contact", adapters.OpRead, adapters.OpValidate, adapters.OpPrepare, adapters.OpCommit, adapters.OpStatus),
		},
	}, nil
}

func (a clinicAdapter) Read(ctx context.Context, sc contracts.Scope, r adapters.ReadRequest) (act.DeterministicResult, error) {
	return a.call(ctx, sc, adapters.OpRead, r.CapabilityID, r.Fields, nil, "")
}

func (a clinicAdapter) Validate(ctx context.Context, sc contracts.Scope, r adapters.ValidateRequest) (act.DeterministicResult, error) {
	return a.call(ctx, sc, adapters.OpValidate, r.CapabilityID, r.Fields, nil, "")
}

func (a clinicAdapter) Prepare(ctx context.Context, sc contracts.Scope, r adapters.PrepareRequest) (act.DeterministicResult, error) {
	return a.call(ctx, sc, adapters.OpPrepare, r.CapabilityID, r.Fields, nil, "")
}

func (a clinicAdapter) Commit(ctx context.Context, sc contracts.Scope, r adapters.CommitRequest) (act.DeterministicResult, error) {
	p := r.Prepared
	return a.call(ctx, sc, adapters.OpCommit, r.CapabilityID, nil, &p, string(r.IdempotencyKey))
}

func (a clinicAdapter) Status(ctx context.Context, sc contracts.Scope, r adapters.StatusRequest) (act.DeterministicResult, error) {
	return a.call(ctx, sc, adapters.OpStatus, r.CapabilityID, nil, nil, string(r.IdempotencyKey))
}

func (a clinicAdapter) Cancel(context.Context, contracts.Scope, adapters.CancelRequest) (act.DeterministicResult, error) {
	return act.DeterministicResult{}, contracts.New(contracts.ErrUnavailable, "the clinic application has no cancel operation: an applied booking is cancelled by its own capability")
}

func (a clinicAdapter) Health(context.Context) (adapters.HealthReport, error) {
	a.h.mu.Lock()
	defer a.h.mu.Unlock()
	if a.h.down {
		return adapters.HealthReport{Status: adapters.HealthUnavailable, Reason: "unreachable"}, nil
	}
	return adapters.HealthReport{Status: adapters.HealthOK, ApplicationVersion: "1.0.0"}, nil
}

func (a clinicAdapter) Inspect(context.Context, contracts.Scope, adapters.InspectRequest) (adapters.Inspection, error) {
	return adapters.Inspection{}, contracts.New(contracts.ErrUnavailable, "the conversation tests do not inspect the application")
}
