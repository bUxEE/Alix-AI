package dr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDROperatorCommandsInspectAndRepair runs the commands the drill found missing (M5-25), as the
// real binary, against a deployment that has committed a booking: the audit chains are verified and
// the receipts and audit events listed from the database file with the service stopped, a tampered
// copy is found, the operator's own file is never changed, and a damaged derived index is repaired
// by one command that is not an upgrade, with nothing that cannot be rebuilt touched.
func TestDROperatorCommandsInspectAndRepair(t *testing.T) {
	e := sharedEnv(t)
	d := e.newDeployment(t)
	w := seedWorld(t, e.clinic, "opsinspect", 1)

	c := d.openCore()
	prep := c.prepareBooking(w, 0)
	c.seedTranscript(w, 2000, indexMarker)
	c.close()
	srv := d.startServe()
	bookThroughService(t, srv, w, prep)
	srv.kill()

	// What the drill itself reads from the file, in-process: the facts the commands must agree with.
	// (Opening and closing the file also folds the -wal a killed service left into it.)
	want := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := want.chainsOK(); err != nil {
		t.Fatal(err)
	}
	wantReceipt, ok := want.receipt(prep.ID)
	if !ok || wantReceipt.Status != "committed" {
		t.Fatalf("setup: the booking has no committed receipt: %s", want)
	}
	before := fileDigest(t, d.dbPath())

	// ---- verify-chain: every chain of the file, no service ----
	var chains struct {
		Chains       int `json:"chains"`
		ChainsOK     int `json:"chains_ok"`
		ChainsFailed int `json:"chains_failed"`
		Listed       []struct {
			ApplicationID string `json:"application_id"`
			TenantID      string `json:"tenant_id"`
			OK            bool   `json:"ok"`
			Events        int64  `json:"events"`
		} `json:"listed"`
	}
	r := d.must("audit", "verify-chain")
	if err := r.decode(&chains); err != nil {
		t.Fatal(err)
	}
	if chains.Chains != len(want.Chains) || chains.ChainsOK != chains.Chains || chains.ChainsFailed != 0 {
		t.Fatalf("audit verify-chain = %+v, want the %d chains of the file all verified\n%s", chains, len(want.Chains), r)
	}
	for _, l := range chains.Listed {
		key := l.ApplicationID + "/" + l.TenantID
		if l.TenantID == "" {
			key = l.ApplicationID + "/-"
		}
		if f, ok := want.chain(key); !ok || f.Events != l.Events {
			t.Errorf("chain %s: the command says %d events, the drill read %+v", key, l.Events, f)
		}
	}
	recordTiming("audit verify-chain, every chain of the database", r.Dur, "%d chains, %d events, read with the service stopped", chains.Chains, totalEvents(want))

	// ---- receipts list and audit list: the booking, and nothing that is secret ----
	var receipts struct {
		Receipts []struct {
			ReceiptID     string `json:"receipt_id"`
			PreparationID string `json:"preparation_id"`
			Status        string `json:"status"`
			CapabilityID  string `json:"capability_id"`
		} `json:"receipts"`
	}
	r = d.must("receipts", "list", "-application", clinicApplication, "-tenant", w.Tenant)
	if err := r.decode(&receipts); err != nil {
		t.Fatal(err)
	}
	if len(receipts.Receipts) != 1 || receipts.Receipts[0].ReceiptID != wantReceipt.ID || receipts.Receipts[0].PreparationID != prep.ID ||
		receipts.Receipts[0].Status != "committed" || receipts.Receipts[0].CapabilityID != "appointments.book" {
		t.Fatalf("receipts list = %+v, want the one committed booking %s of preparation %s\n%s", receipts.Receipts, wantReceipt.ID, prep.ID, r)
	}
	if strings.Contains(r.Stdout, prep.Secret) {
		t.Fatalf("receipts list printed the confirmation secret:\n%s", r.Stdout)
	}
	// Another tenant's name is an empty answer and never an error that confirms it exists.
	if r = d.must("receipts", "list", "-application", clinicApplication, "-tenant", "tnt_no_such_tenant"); strings.Contains(r.Stdout, prep.ID) {
		t.Fatalf("receipts list of another tenant names this tenant's preparation:\n%s", r.Stdout)
	}
	var events struct {
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
		More bool `json:"more"`
	}
	r = d.must("audit", "list", "-application", clinicApplication, "-tenant", w.Tenant, "-type", "action.committed", "-data")
	if err := r.decode(&events); err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 || events.Events[0].Type != "action.committed" || events.More {
		t.Fatalf("audit list -type action.committed = %+v\n%s", events, r)
	}
	if strings.Contains(r.Stdout, prep.Secret) {
		t.Fatalf("audit list printed the confirmation secret:\n%s", r.Stdout)
	}
	if after := fileDigest(t, d.dbPath()); after != before {
		t.Fatalf("the inspection commands changed the database file: %s became %s", before, after)
	}

	// ---- a tampered copy is found; the original still verifies ----
	copyDir := filepath.Join(d.root, "tampered")
	copyFile(t, d.dbPath(), filepath.Join(copyDir, "alixai.db"))
	db := openDB(t, d.cfgPath, filepath.Join(copyDir, "alixai.db"))
	for _, stmt := range []string{
		`DROP TRIGGER audit_events_no_update`,
		fmt.Sprintf(`UPDATE audit_events SET principal_id = 'usr_someone_else' WHERE id = (SELECT id FROM audit_events WHERE application_id = '%s' AND tenant_id = '%s' ORDER BY chain_seq DESC LIMIT 1)`, clinicApplication, w.Tenant),
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if bad := d.run("audit", "verify-chain", "-db", filepath.Join(copyDir, "alixai.db")); bad.Code == 0 || !strings.Contains(bad.Stdout, "hash_mismatch") {
		t.Fatalf("audit verify-chain on a tampered copy = %s", bad)
	}
	d.must("audit", "verify-chain", "-db", d.dbPath())

	// ---- index repair: the damaged history index, with no release to apply ----
	if f := ftsState(t, d, d.dbPath()); f.Damaged || f.Hits != 1 {
		t.Fatalf("before the damage the index is %+v", f)
	}
	var healthy struct {
		Outcome string `json:"outcome"`
	}
	if err := d.must("index", "repair", "-index", "history_fts").decode(&healthy); err != nil || healthy.Outcome != "healthy" {
		t.Fatalf("index repair of a healthy index = %+v, %v", healthy, err)
	}
	damageIndexBytes(t, d.dbPath())
	if f := ftsState(t, d, d.dbPath()); !f.Damaged || f.Hits != 0 {
		t.Fatalf("the damaged index is %+v", f)
	}
	var repaired struct {
		Index   string `json:"index"`
		Outcome string `json:"outcome"`
		Damaged bool   `json:"damaged"`
	}
	r = d.must("index", "repair", "-index", "history_fts")
	if err := r.decode(&repaired); err != nil {
		t.Fatal(err)
	}
	if repaired.Outcome != "rebuilt" || !repaired.Damaged || repaired.Index != "history_fts" {
		t.Fatalf("index repair = %+v\n%s", repaired, r)
	}
	recordTiming("index repair of the damaged history index (no release applied)", r.Dur, "isolate, rebuild from the transcript, check, release; %d transcript turns", 2000)
	if f := ftsState(t, d, d.dbPath()); f.Damaged || f.Hits != 1 {
		t.Fatalf("after the repair the index is %+v, want healthy and finding the marker once", f)
	}
	var status struct {
		Quarantined []any `json:"quarantined"`
	}
	if err := d.must("upgrade", "status").decode(&status); err != nil || len(status.Quarantined) != 0 {
		t.Fatalf("an index is still isolated after the repair: %+v, %v", status, err)
	}
	after := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := after.chainsOK(); err != nil {
		t.Fatal(err)
	}
	if err := sameReceipts(want.Receipts, after.Receipts); err != nil {
		t.Fatalf("the repair changed the receipts: %v", err)
	}
	if after.Turns != want.Turns || after.TurnsDigest != want.TurnsDigest {
		t.Fatalf("the repair changed the transcript: %d turns became %d", want.Turns, after.Turns)
	}
	if _, err := os.Stat(filepath.Join(d.dataDir, "upgrade", "quarantine")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(d.dataDir, "upgrade", "quarantine"))
		if len(entries) != 0 {
			t.Errorf("the quarantine still holds %d records after the repair", len(entries))
		}
	}
}

func totalEvents(s snapshot) int64 {
	var n int64
	for _, c := range s.Chains {
		n += c.Events
	}
	return n
}

// fileDigest is the SHA-256 of a file, as text.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(mustRead(t, path))
	return hex.EncodeToString(sum[:])
}
