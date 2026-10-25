package dr

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDRMonitoringSignals reads, from a running service, every signal the production guide tells an
// operator to watch: liveness, readiness, the Prometheus text and the diagnostics bundle behind the
// operator credential. It is what makes the monitoring section a checked claim.
func TestDRMonitoringSignals(t *testing.T) {
	e := sharedEnv(t)
	d := e.newDeployment(t)
	metricsAddr, err := freeAddr()
	if err != nil {
		t.Fatal(err)
	}
	d.extra = fmt.Sprintf("metrics:\n  enabled: true\n  listen: %q\n", metricsAddr)
	srv := d.startServe()

	if status, body, err := srv.call("GET", "/healthz", "", nil); err != nil || status != 200 {
		t.Fatalf("/healthz = %d, %v: %s", status, err, body)
	}
	// Ready is false by design on a machine with no activatable model: the body names the inference
	// path, which is what an operator reads.
	status, body, err := srv.call("GET", "/readyz", "", nil)
	if err != nil || status != http.StatusServiceUnavailable || !strings.Contains(string(body), `"name":"inference"`) {
		t.Fatalf("/readyz = %d, %v: %s", status, err, body)
	}
	if status, _, _ := srv.call("GET", "/v1/actions/prep_none", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("a /v1 route without a context token answers %d, want 401", status)
	}

	get := func(path string, header map[string]string) (int, string) {
		req, err := http.NewRequest("GET", "http://"+metricsAddr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := serveHTTP.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	status2, text := get("/metrics", nil)
	if status2 != 200 {
		t.Fatalf("/metrics = %d: %s", status2, text)
	}
	for _, family := range []string{"alixai_build_info", "alixai_queue_", "alixai_resource_", "alixai_process_", "alixai_scrape_collector_up"} {
		if !strings.Contains(text, family) {
			t.Errorf("/metrics has no %s family", family)
		}
	}

	var endpoint struct {
		Token string `json:"token"`
	}
	mustJSON(t, mustRead(t, filepath.Join(d.dataDir, "admin-endpoint.json")), &endpoint)
	if status, _ := get("/diagnostics", nil); status != http.StatusForbidden {
		t.Fatalf("/diagnostics without the operator credential = %d, want 403", status)
	}
	status3, bundle := get("/diagnostics", map[string]string{"X-Alixai-Admin-Token": endpoint.Token})
	if status3 != 200 || !json.Valid([]byte(bundle)) {
		t.Fatalf("/diagnostics with the operator credential = %d: %.200s", status3, bundle)
	}
	for _, secret := range []string{endpoint.Token, e.signerKey} {
		if strings.Contains(bundle, secret) {
			t.Errorf("the diagnostics bundle contains %q", secret)
		}
	}
	logs := srv.stderrText()
	if !strings.Contains(logs, `"msg":"serving"`) || !strings.Contains(logs, "the inference path is off") {
		t.Errorf("the service log lacks its start lines:\n%s", logs)
	}
	srv.kill()
	recordObservation("monitoring", "/healthz 200; /readyz 503 naming the inference path; /metrics carries the alixai_build_info, queue, resource and process families; /diagnostics is 403 without the operator credential and a redacted JSON bundle with it")
}

// TestDRKeyRotation runs the rotation procedures of the production guide that the drill can run
// without a second host application: the audit checkpoint key (a new key is created and used, the old
// one revoked, and a revoked key stops the service from starting) and the backup key (a new key signs
// new backups, and every older backup still verifies with the public key it was made with).
func TestDRKeyRotation(t *testing.T) {
	e := sharedEnv(t)
	d := e.newDeployment(t)
	keys := filepath.Join(d.dataDir, "keys")

	// ---- the backup key ----
	backups := filepath.Join(d.root, "backups")
	var first struct {
		Dir string `json:"dir"`
	}
	if err := d.must("backup", "create", "-dir", backups).decode(&first); err != nil {
		t.Fatal(err)
	}
	oldPub := filepath.Join(keys, backupKeyID+".pub")
	newKeys := filepath.Join(d.root, "backup-key-2")
	if r := e.runNoConfig("package", "keygen", "-key-id", "backup-2", "-out", newKeys); r.Code != 0 {
		t.Fatalf("package keygen: %s", r)
	}
	time.Sleep(1100 * time.Millisecond) // backup ids are ULIDs: keep the two backups apart in time
	var second struct {
		Dir   string `json:"dir"`
		KeyID string `json:"key_id"`
	}
	if err := d.must("backup", "create", "-dir", backups, "-key", filepath.Join(newKeys, "backup-2.key"), "-key-id", "backup-2").decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.KeyID != "backup-2" {
		t.Fatalf("the new backup was signed with %q", second.KeyID)
	}
	newPub := filepath.Join(newKeys, "backup-2.pub")
	d.must("backup", "verify", "-dir", second.Dir, "-pubkey", "backup-2="+newPub)
	d.must("backup", "verify", "-dir", first.Dir, "-pubkey", backupKeyID+"="+oldPub)
	if r := d.run("backup", "verify", "-dir", second.Dir, "-pubkey", backupKeyID+"="+oldPub); r.Code == 0 {
		t.Fatalf("a backup signed with the new key verified with the old public key:\n%s", r)
	}
	// Both public keys together verify both backups: what an operator keeps after a rotation.
	d.must("backup", "verify", "-dir", first.Dir, "-pubkey", backupKeyID+"="+oldPub, "-pubkey", "backup-2="+newPub)
	d.must("backup", "verify", "-dir", second.Dir, "-pubkey", backupKeyID+"="+oldPub, "-pubkey", "backup-2="+newPub)

	// ---- the audit checkpoint key ----
	const newID = "audit-checkpoint-2"
	r := d.must("keys", "init", "-key-id", newID)
	if !strings.Contains(r.Stdout, newID) {
		t.Fatalf("keys init did not say it created %s:\n%s", newID, r)
	}
	list := d.must("trust", "list", "-app", "alixai.platform")
	for _, id := range []string{"audit-checkpoint", newID} {
		if !strings.Contains(list.Stdout, id) {
			t.Fatalf("the platform keys do not list %s after the rotation:\n%s", id, list.Stdout)
		}
	}
	useKey := func(id string) {
		d.extra = fmt.Sprintf("keys:\n  checkpoint_key_file: %q\n  checkpoint_key_id: %s\n", filepath.ToSlash(filepath.Join(keys, id+".key")), id)
	}

	// The service signs checkpoints with the new key once the configuration names it.
	useKey(newID)
	srv := d.startServe()
	if logs := srv.stderrText(); !strings.Contains(logs, "audit checkpoints are signed") || !strings.Contains(logs, `"key_id":"`+newID+`"`) {
		t.Fatalf("the service does not sign checkpoints with %s:\n%s", newID, logs)
	}
	srv.kill()

	// The old key is revoked (it was compromised): a service configured with it refuses to start,
	// and the one configured with the new key still starts.
	d.must("trust", "revoke-key", "-app", "alixai.platform", "-key-id", "audit-checkpoint", "-reason", "rotation drill: pretend the key leaked")
	useKey("audit-checkpoint")
	exited, code, logs := d.startServeExpectingFailure(30 * time.Second)
	if !exited || code == 0 || !strings.Contains(logs, "checkpoint signing key") {
		t.Fatalf("the service started with a revoked checkpoint key (exited %v, code %d):\n%s", exited, code, logs)
	}
	useKey(newID)
	srv = d.startServe()
	srv.kill()
	recordObservation("key rotation", "backup key: a new key signs new backups, each backup verifies with the public key it was made with and not with the other, and both public keys together verify both; checkpoint key: `keys init -key-id` made a second key, the service signed with it once named, the old key was revoked and a service configured with it refused to start (`%s`)", firstLine(logs))
}
