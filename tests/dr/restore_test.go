package dr

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// ballastMiB is the size of the file the drill puts in the files tree, so that a backup and a
	// restore copy something more than a small database.
	ballastMiB = 64
	// backupKeyID is the id the backup key is created under.
	backupKeyID = "backup"
)

// drState is a deployment that has done real work, has a verified backup in the middle of it, and
// has done more work after the backup: the moment a disaster is most expensive.
type drState struct {
	d *deployment
	w *world

	r1, r2 prepared
	// receipt bodies as the service served them before the disaster.
	r1Body, r2Body []byte

	backupID, backupDir string
	// offMachine holds what the operator kept somewhere that is not this machine.
	offMachine string
	backupPub  string

	atBackup  snapshot // the database when the backup was taken: receipt 1
	afterWork snapshot // the database before the disaster: receipts 1 and 2

	backupDur, verifyDur time.Duration
	backupBytes          int64
	srv                  *serveProc
}

func dirSize(path string) int64 {
	var n int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

func writeBallast(t *testing.T, path string, mib int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i * 7)
	}
	for i := 0; i < mib; i++ {
		chunk[0] = byte(i)
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

// bookThroughService confirms a preparation through the running service and returns the receipt as
// the service serves it afterwards.
func bookThroughService(t *testing.T, srv *serveProc, w *world, p prepared) []byte {
	t.Helper()
	status, body, err := srv.confirm(t, w, p.SessionID, p.ID, p.Secret)
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), `"status":"committed"`) {
		t.Fatalf("confirm %s = %d, %v: %s\n%s", p.ID, status, err, body, srv.logs())
	}
	status, raw := srv.receipt(t, w, p.SessionID, p.ID)
	if status != http.StatusOK {
		t.Fatalf("receipt of %s = %d: %s", p.ID, status, raw)
	}
	return raw
}

// setupBackedUp builds the state of a deployment that was in use: a booking, a transcript and some
// files, a signed backup taken while the service serves and verified against a public key kept off
// the machine, and a second booking after the backup. The service is still running.
func setupBackedUp(t *testing.T, label string) *drState {
	t.Helper()
	e := sharedEnv(t)
	s := &drState{d: e.newDeployment(t)}
	d := s.d
	s.w = seedWorld(t, e.clinic, label, 2)

	c := d.openCore()
	s.r1 = c.prepareBooking(s.w, 0)
	c.seedTranscript(s.w, transcriptTurns, indexMarker)
	c.close()
	writeBallast(t, filepath.Join(d.dataDir, "files", "dr-ballast.bin"), ballastMiB)

	s.srv = d.startServe()
	s.r1Body = bookThroughService(t, s.srv, s.w, s.r1)
	s.atBackup = takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := s.atBackup.chainsOK(); err != nil {
		t.Fatal(err)
	}

	// The backup, while the service is up.
	backups := filepath.Join(d.root, "backups")
	var created struct {
		ID    string `json:"backup_id"`
		Dir   string `json:"dir"`
		Files int    `json:"files"`
	}
	r := d.must("backup", "create", "-dir", backups)
	if err := r.decode(&created); err != nil {
		t.Fatal(err)
	}
	s.backupID, s.backupDir, s.backupDur = created.ID, created.Dir, r.Dur
	s.backupBytes = dirSize(created.Dir)
	if fi, err := os.Stat(filepath.Join(created.Dir, "alixai.db")); err == nil {
		recordEnv("Database after "+fmt.Sprint(transcriptTurns)+" transcript turns, in a backup (VACUUM INTO, so compact)", fmt.Sprintf("%.1f MiB, including the history index and the audit chains", float64(fi.Size())/(1<<20)))
	}

	// What the operator keeps off the machine: the backup's public key, and the checkpoint key.
	s.offMachine = filepath.Join(d.root, "off-machine")
	s.backupPub = filepath.Join(s.offMachine, "backup.pub")
	copyFile(t, filepath.Join(d.dataDir, "keys", backupKeyID+".pub"), s.backupPub)
	copyFile(t, filepath.Join(d.dataDir, "keys", "audit-checkpoint.key"), filepath.Join(s.offMachine, "audit-checkpoint.key"))
	copyFile(t, filepath.Join(d.dataDir, "keys", "audit-checkpoint.pub"), filepath.Join(s.offMachine, "audit-checkpoint.pub"))

	var verified struct {
		Verified bool `json:"verified"`
	}
	v := d.must("backup", "verify", "-dir", s.backupDir, "-pubkey", backupKeyID+"="+s.backupPub)
	if err := v.decode(&verified); err != nil || !verified.Verified {
		t.Fatalf("backup verify: %v\n%s", err, v)
	}
	s.verifyDur = v.Dur
	recordTiming("backup create, while the service serves", s.backupDur, "database with %d transcript turns, %d receipt, %d MiB file tree; %.1f MiB of backup", transcriptTurns, 1, ballastMiB, float64(s.backupBytes)/(1<<20))
	recordTiming("backup verify", s.verifyDur, "signature, every file's SHA-256, integrity check of the database copy")

	// More work after the backup: this is what a restore to the backup puts at risk.
	c = d.openCore()
	s.r2 = c.prepareBooking(s.w, 1)
	c.close()
	s.r2Body = bookThroughService(t, s.srv, s.w, s.r2)
	s.afterWork = takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := s.afterWork.chainsOK(); err != nil {
		t.Fatal(err)
	}
	if len(s.afterWork.Receipts) != 2 {
		t.Fatalf("setup: %s", s.afterWork)
	}
	return s
}

// restoreOutcome is what `alixai backup restore` printed.
type restoreOutcome struct {
	BackupID      string `json:"backup_id"`
	DataDir       string `json:"data_dir"`
	PreRestoreDir string `json:"pre_restore_dir"`
}

func (s *drState) restore(t *testing.T, args ...string) (restoreOutcome, result) {
	t.Helper()
	full := append([]string{"backup", "restore", "-dir", s.backupDir, "-pubkey", backupKeyID + "=" + s.backupPub}, args...)
	r := s.d.run(full...)
	var out restoreOutcome
	if r.Code == 0 {
		if err := r.decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return out, r
}

// checkRestored is what every restore must give back: the receipts of the backup, byte for byte, as
// the database holds them and as the restarted service serves them, a transcript that is the
// backup's, and audit chains that verify. It returns the restored database's snapshot.
func (s *drState) checkRestored(t *testing.T, label string) snapshot {
	t.Helper()
	d := s.d
	restored := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := restored.chainsOK(); err != nil {
		t.Fatal(err)
	}
	if err := sameReceipts(s.atBackup.Receipts, restored.Receipts); err != nil {
		t.Fatalf("the restored receipts are not the backup's: %v", err)
	}
	if restored.Turns != s.atBackup.Turns || restored.TurnsDigest != s.atBackup.TurnsDigest {
		t.Fatalf("the restored transcript is not the backup's: %d turns, want %d", restored.Turns, s.atBackup.Turns)
	}
	for _, b := range s.atBackup.Chains {
		a, ok := restored.chain(b.Chain)
		if !ok || a.Events != b.Events || a.HeadHash != b.HeadHash {
			t.Fatalf("audit chain %s: restored %+v, backup had %d events head %.12s", b.Chain, a, b.Events, b.HeadHash)
		}
	}
	if f := ftsState(t, d, d.dbPath()); f.Damaged || f.Hits != 1 {
		t.Fatalf("the restored history index is %+v, want healthy and finding the marker", f)
	}
	if fi, err := os.Stat(filepath.Join(d.dataDir, "files", "dr-ballast.bin")); err != nil || fi.Size() != ballastMiB<<20 {
		t.Fatalf("the restored files tree lacks the %d MiB file: %v", ballastMiB, err)
	}

	srv := d.startServe()
	recordTiming("service start on the restored data directory ("+label+")", srv.ready, "start to listening")
	status, body := srv.receipt(t, s.w, s.r1.SessionID, s.r1.ID)
	if status != http.StatusOK || string(body) != string(s.r1Body) {
		t.Fatalf("the restarted service serves receipt 1 as %d %s, want the bytes it served before the disaster:\n%s", status, body, s.r1Body)
	}
	if status, raw := srv.receipt(t, s.w, s.r2.SessionID, s.r2.ID); status == http.StatusOK {
		t.Fatalf("the restored service has a receipt for the booking made after the backup: %s", raw)
	}
	srv.kill()
	return restored
}

// TestDRRestoreKeepsReceiptsAndAuditChain restores a signed backup after three different disasters
// and checks master plan 18 and 19: every receipt the backup holds is there, byte for byte, the audit
// chains verify, the service serves the receipts again, and what was written after the backup is not
// silently thrown away.
func TestDRRestoreKeepsReceiptsAndAuditChain(t *testing.T) {
	t.Run("restore_after_a_crash_keeps_what_came_after_the_backup", func(t *testing.T) {
		// The operator rolls back to the backup (a bad upgrade, a wrong change) after the service
		// crashed. The database is intact: what was written after the backup must survive in
		// pre_restore_dir.
		s := setupBackedUp(t, "restore")
		d := s.d
		s.srv.kill()
		if !fileExists(d.dbPath() + "-shm") {
			t.Fatal("a killed service left no -shm file: the drill is not simulating a crash")
		}

		// Restore refuses while the database looks in use, and says why.
		if out, r := s.restore(t); r.Code == 0 || !strings.Contains(r.Stderr, "alixai.db-shm exists") {
			t.Fatalf("restore over a crashed database without -force = %+v\n%s", out, r)
		}
		if f := fileExists(d.dbPath()); !f {
			t.Fatal("the refused restore removed the database")
		}

		out, r := s.restore(t, "-force")
		if r.Code != 0 || out.PreRestoreDir == "" {
			t.Fatalf("restore -force: %+v\n%s", out, r)
		}
		recordTiming("backup restore over a crashed database (-force)", r.Dur, "verify, stage, swap; %.1f MiB of backup", float64(s.backupBytes)/(1<<20))

		s.checkRestored(t, "after a crash")

		// What was in place before is kept, whole: both receipts and every audit event.
		kept := takeSnapshot(t, d.cfgPath, filepath.Join(out.PreRestoreDir, "alixai.db"))
		if err := kept.chainsOK(); err != nil {
			t.Fatalf("pre_restore_dir: %v", err)
		}
		if err := sameReceipts(s.afterWork.Receipts, kept.Receipts); err != nil {
			t.Fatalf("pre_restore_dir does not hold the receipts written before the restore: %v", err)
		}
		if _, ok := kept.receipt(s.r2.ID); !ok {
			t.Fatal("the booking made after the backup is not in pre_restore_dir")
		}
		if !fileExists(filepath.Join(out.PreRestoreDir, "files", "dr-ballast.bin")) {
			t.Error("the files tree that was in place is not in pre_restore_dir")
		}
		recordObservation("restore after a crash",
			"restored backup %s over a crashed database (-shm left behind): receipts %d -> %d, audit chains verify, receipt 1 served byte for byte; the booking made after the backup (preparation %s) is in pre_restore_dir (%s)",
			s.backupID, len(s.afterWork.Receipts), len(s.atBackup.Receipts), s.r2.ID, kept.Summary())
	})

	t.Run("restore_over_a_destroyed_database", func(t *testing.T) {
		// The database file is destroyed (a failing disk). The service refuses to start on it; the
		// backup brings back everything up to the backup. What was written after it lived only in
		// the destroyed file: that is the cost of the backup interval, and the application still
		// holds that booking.
		s := setupBackedUp(t, "destroyed")
		d := s.d
		s.srv.kill()
		f, err := os.OpenFile(d.dbPath(), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		garbage := make([]byte, 8192)
		for i := range garbage {
			garbage[i] = byte(i*31 + 7)
		}
		if _, err := f.WriteAt(garbage, 0); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		exited, code, logs := d.startServeExpectingFailure(30 * time.Second)
		if !exited || code == 0 {
			t.Fatalf("the service did not refuse to start on a destroyed database (exited %v, code %d):\n%s", exited, code, logs)
		}
		recordObservation("restore over a destroyed database", "the service refuses to start on the destroyed file (exit %d: %s)", code, firstLine(logs))

		// The key revocations of the destroyed database cannot be read: the restore refuses to
		// go on without them unless the operator says so (L3; production.md runbook D).
		if _, r := s.restore(t, "-force"); r.Code == 0 || !strings.Contains(r.Stderr, "skip-newer-revocations") {
			t.Fatalf("restore over a destroyed database without -skip-newer-revocations went ahead or did not say why:\n%s", r)
		}
		out, r := s.restore(t, "-force", "-skip-newer-revocations")
		if r.Code != 0 || out.PreRestoreDir == "" {
			t.Fatalf("restore -force -skip-newer-revocations: %+v\n%s", out, r)
		}
		recordTiming("backup restore over a destroyed database (-force)", r.Dur, "verify, stage, swap; %.1f MiB of backup", float64(s.backupBytes)/(1<<20))
		restored := s.checkRestored(t, "after the database was destroyed")
		if _, ok := restored.receipt(s.r2.ID); ok {
			t.Fatal("receipt 2 exists in a database restored from a backup made before it")
		}
		if !fileExists(filepath.Join(out.PreRestoreDir, "alixai.db")) {
			t.Error("the destroyed database was not kept in pre_restore_dir")
		}
		recordObservation("restore over a destroyed database",
			"restored backup %s: receipts %d -> %d (the booking made after the backup is gone from AlixAI's records; the application still holds it), audit chains verify (%s)", s.backupID, len(s.afterWork.Receipts), len(s.atBackup.Receipts), restored.Summary())
	})

	t.Run("restore_into_an_empty_data_directory", func(t *testing.T) {
		// The machine is lost: the data directory is empty. The operator has the backup, the
		// backup's public key and the checkpoint key from off the machine.
		s := setupBackedUp(t, "empty")
		d := s.d
		s.srv.kill()
		if err := os.RemoveAll(d.dataDir); err != nil {
			t.Fatal(err)
		}

		// A public key that is not the backup's is refused, and nothing is created.
		wrong := filepath.Join(s.offMachine, "wrong.pub")
		copyFile(t, filepath.Join(d.e.moduleRoot, "tools", "devkeys", "test-only-publisher.pub"), wrong)
		bad := d.run("backup", "restore", "-dir", s.backupDir, "-pubkey", backupKeyID+"="+wrong)
		if bad.Code == 0 || fileExists(d.dbPath()) {
			t.Fatalf("restore with a key that did not sign the backup: %s", bad)
		}

		out, r := s.restore(t)
		if r.Code != 0 {
			t.Fatalf("restore into an empty directory: %s", r)
		}
		if out.PreRestoreDir != "" {
			t.Errorf("pre_restore_dir = %q for a directory that was empty", out.PreRestoreDir)
		}
		recordTiming("backup restore into an empty data directory", r.Dur, "verify, stage, move into place; %.1f MiB of backup", float64(s.backupBytes)/(1<<20))

		// The checkpoint key goes back from where it was kept; the restored trust store already
		// has its public half, so the service signs checkpoints that verify against it.
		mustWrite(t, filepath.Join(d.dataDir, "keys", "audit-checkpoint.key"), mustRead(t, filepath.Join(s.offMachine, "audit-checkpoint.key")))
		s.checkRestored(t, "into an empty data directory")
		srv := d.startServe()
		if !strings.Contains(srv.stderrText(), "audit checkpoints are signed") {
			t.Errorf("the restored service does not sign audit checkpoints:\n%s", srv.stderrText())
		}
		srv.kill()

		// The next backup creates a new backup key (the old one was not in the backup); the old backup
		// still verifies with the old public key.
		next := d.must("backup", "create", "-dir", filepath.Join(d.root, "backups-after"))
		var created struct {
			Dir string `json:"dir"`
		}
		if err := next.decode(&created); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(next.Stderr, "created the backup signing key") {
			t.Errorf("the first backup after a restore into an empty directory did not create a new key:\n%s", next.Stderr)
		}
		d.must("backup", "verify", "-dir", created.Dir)
		d.must("backup", "verify", "-dir", s.backupDir, "-pubkey", backupKeyID+"="+s.backupPub)

		// A rehearsal, which is how an operator proves a backup before needing it: the same backup
		// restored into a scratch directory beside the live one, and a service started on it.
		rehearsal := d.sibling("rehearsal")
		rr := d.run("backup", "restore", "-dir", s.backupDir, "-pubkey", backupKeyID+"="+s.backupPub, "-data-dir", rehearsal.dataDir)
		if rr.Code != 0 {
			t.Fatalf("restore into a scratch directory: %s", rr)
		}
		recordTiming("backup restore into a scratch directory (rehearsal, -data-dir)", rr.Dur, "verify, stage, move into place; %.1f MiB of backup", float64(s.backupBytes)/(1<<20))
		rsrv := rehearsal.startServe()
		if status, body := rsrv.receipt(t, s.w, s.r1.SessionID, s.r1.ID); status != http.StatusOK || string(body) != string(s.r1Body) {
			t.Fatalf("the rehearsal service serves receipt 1 as %d %s", status, body)
		}
		rsrv.kill()
		recordObservation("restore into an empty data directory",
			"restored backup %s into an empty directory with the off-machine public key; a wrong key is refused; the checkpoint key put back signs checkpoints; the next backup made a new key and both backups verify; a rehearsal restore into a scratch directory with -data-dir served the same receipt byte for byte", s.backupID)
	})
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if len(l) > 240 {
			l = l[:240] + "..."
		}
		return l
	}
	return ""
}

var (
	_ = json.Marshal
	_ = fmt.Sprintf
)
