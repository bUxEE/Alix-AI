package m1

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// person is a principal of the clinic's directory: who a token is for.
type person struct {
	ID        string
	Workspace string
	Role      string
	// Tenant is the clinic group the principal belongs to.
	Tenant string
}

// world is a disposable tenant of the clinic application, created and seeded through the
// application's own test tools: one clinic group with two sites, the principals of each role at
// the first, one named patient and three free slots with the group's doctor.
type world struct {
	clinic *clinicApp
	Tenant string

	SiteA, SiteB string
	Reception    *person // receptionist of site A
	Doctor       *person // doctor of site A
	Patient      *person // patient of site A: sees only themselves
	ReceptionB   *person // receptionist of site B

	// PatientName and PatientID are the seeded patient Alma Gatefield of site A.
	PatientName string
	PatientID   string
	// DoctorName is the name the application gives the doctor principal's record.
	DoctorName string
	// Slots are the free slots of site A, earliest first, with the doctor.
	Slots []string
}

func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type worldOption func(*worldConfig)

type worldConfig struct{ slots int }

// withSlots seeds n free slots at site A instead of three.
func withSlots(n int) worldOption { return func(c *worldConfig) { c.slots = n } }

// seedWorld creates a disposable tenant and seeds it. The label only makes the identifiers
// readable in a failure.
func seedWorld(t tb, clinic *clinicApp, label string, opts ...worldOption) *world {
	t.Helper()
	cfg := worldConfig{slots: 3}
	for _, o := range opts {
		o(&cfg)
	}
	suffix := randomSuffix()
	w := &world{
		clinic: clinic, Tenant: "tnt_" + label + "_" + suffix,
		SiteA: "ws_" + label + "_a_" + suffix, SiteB: "ws_" + label + "_b_" + suffix,
		PatientName: "Alma Gatefield",
	}
	mk := func(name, workspace, role string) *person {
		return &person{ID: "usr_" + label + "_" + name + "_" + suffix, Workspace: workspace, Role: role, Tenant: w.Tenant}
	}
	w.Reception = mk("reception", w.SiteA, "receptionist")
	w.Doctor = mk("doctor", w.SiteA, "doctor")
	w.Patient = mk("patient", w.SiteA, "patient")
	w.ReceptionB = mk("receptionb", w.SiteB, "receptionist")

	principals := []map[string]any{}
	for _, p := range []*person{w.Reception, w.Doctor, w.Patient, w.ReceptionB} {
		principals = append(principals, map[string]any{
			"principal_id": p.ID, "workspace_id": p.Workspace, "role": p.Role, "display_name": "Gate " + p.Role,
		})
	}
	clinic.mustTool(t, "test_create_tenant", map[string]any{
		"tenant_id": w.Tenant, "name": "Gate Clinic Group " + label,
		"workspaces": []map[string]any{
			{"workspace_id": w.SiteA, "name": "Gate Site A", "timezone": "Europe/Rome"},
			{"workspace_id": w.SiteB, "name": "Gate Site B", "timezone": "Europe/Rome"},
		},
		"principals": principals,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = clinic.callTool(ctx, "test_cleanup", map[string]any{"tenant_id": w.Tenant})
	})

	records := []map[string]any{
		{"ref": "alma", "entity_type": "patient", "workspace_id": w.SiteA, "given_name": "Alma", "family_name": "Gatefield"},
	}
	for i := 1; i <= cfg.slots; i++ {
		records = append(records, map[string]any{
			"ref": fmt.Sprintf("slot%d", i), "entity_type": "slot", "workspace_id": w.SiteA, "principal_id": w.Doctor.ID,
		})
	}
	raw := clinic.mustTool(t, "test_seed", map[string]any{"tenant_id": w.Tenant, "records": records})
	var seeded struct {
		Records []struct {
			Ref      string `json:"ref"`
			EntityID string `json:"entity_id"`
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &seeded); err != nil {
		t.Fatalf("test_seed: %v: %s", err, raw)
	}
	byRef := map[string]string{}
	for _, r := range seeded.Records {
		byRef[r.Ref] = r.EntityID
	}
	w.PatientID = byRef["alma"]
	if w.PatientID == "" {
		t.Fatalf("test_seed did not return the patient: %s", raw)
	}
	for i := 1; i <= cfg.slots; i++ {
		id := byRef[fmt.Sprintf("slot%d", i)]
		if id == "" {
			t.Fatalf("test_seed did not return slot %d: %s", i, raw)
		}
		w.Slots = append(w.Slots, id)
	}
	w.DoctorName = "Dr. Gate doctor"
	return w
}

// token asks the host for a context token of the principal, for the session. Every token is
// single use: every request of a test asks for its own.
func (w *world) token(t tb, p *person, sessionID string) string {
	t.Helper()
	return w.clinic.hostToken(t, p.Tenant, p.Workspace, p.ID, sessionID, 900)
}

// tokenScoped is token with a lifetime in seconds and the capability scopes the host grants.
func (w *world) tokenScoped(t tb, p *person, sessionID string, lifetime int, scopes []string) string {
	t.Helper()
	return w.clinic.hostTokenScoped(t, p.Tenant, p.Workspace, p.ID, sessionID, lifetime, scopes)
}

func (w *world) String() string {
	return fmt.Sprintf("tenant %s (sites %s, %s)", w.Tenant, w.SiteA, w.SiteB)
}
