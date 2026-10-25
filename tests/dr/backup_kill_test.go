package dr

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/store"
)

// startBackup starts `alixai backup create` as a subprocess the test holds, and returns it with a
// channel that closes when it has exited.
func (d *deployment) startBackup(dir string) (*exec.Cmd, <-chan struct{}) {
	d.t.Helper()
	cmd := exec.Command(d.e.bin, "backup", "create", "-config", d.cfgPath, "-dir", dir)
	cmd.Dir = d.e.moduleRoot
	cmd.Env = cleanEnv()
	if err := cmd.Start(); err != nil {
		d.t.Fatalf("start the backup: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return cmd, done
}

// killBackupMidCopy waits for the backup's working directory to hold part of the file tree and kills
// the backup process, which is the one this test started. It reports false when the backup finished
// before it could be killed.
func killBackupMidCopy(t *testing.T, cmd *exec.Cmd, done <-chan struct{}, backups string) (partial string, killed bool) {
	t.Helper()
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case <-done:
			return "", false
		case <-deadline:
			_ = cmd.Process.Kill()
			<-done
			t.Fatal("the backup never reached its file-tree copy")
		default:
		}
		if m, _ := filepath.Glob(filepath.Join(backups, "*.partial")); len(m) == 1 {
			if fileExists(filepath.Join(m[0], "alixai.db")) && fileExists(filepath.Join(m[0], "tree", "files")) {
				_ = cmd.Process.Kill()
				<-done
				return m[0], true
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDRKillMidBackup kills `alixai backup create` while it copies the file tree: the interrupted
// backup must never look like a finished one (it stays `<id>.partial`, with no manifest, and does not
// verify), the live data and the running service are untouched, and the next backup succeeds and
// verifies.
func TestDRKillMidBackup(t *testing.T) {
	e := sharedEnv(t)
	d := e.newDeployment(t)
	w := seedWorld(t, e.clinic, "midbackup", 1)

	c := d.openCore()
	prep := c.prepareBooking(w, 0)
	c.close()
	srv := d.startServe()
	receipt := bookThroughService(t, srv, w, prep)
	before := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := before.chainsOK(); err != nil {
		t.Fatal(err)
	}

	// A file big enough that the copy of the tree takes longer than the test needs to notice it. If
	// the backup wins the race the file doubles and the drill tries again.
	backups := filepath.Join(d.root, "backups")
	ballast := filepath.Join(d.dataDir, "files", "dr-ballast.bin")
	var partial string
	mib := 128
	for attempt := 1; ; attempt++ {
		writeBallast(t, ballast, mib)
		cmd, done := d.startBackup(backups)
		var killed bool
		if partial, killed = killBackupMidCopy(t, cmd, done, backups); killed {
			break
		}
		if attempt == 4 {
			t.Fatalf("the backup finished before it could be killed with a %d MiB file tree", mib)
		}
		if err := os.RemoveAll(backups); err != nil {
			t.Fatal(err)
		}
		mib *= 2
	}
	copied := int64(-1)
	if fi, err := os.Stat(filepath.Join(partial, "tree", "files", "dr-ballast.bin")); err == nil {
		copied = fi.Size()
	}
	t.Logf("killed the backup while it copied a %d MiB file tree (%d of %d bytes of it were in %s)", mib, copied, int64(mib)<<20, filepath.Base(partial))

	// The interrupted backup is a .partial directory, never a finished backup.
	entries, err := os.ReadDir(backups)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if !strings.HasSuffix(en.Name(), ".partial") {
			t.Errorf("%s in the backup directory does not end in .partial: an interrupted backup looks finished", en.Name())
		}
	}
	if fileExists(filepath.Join(partial, "manifest.json")) {
		t.Error("the interrupted backup has a manifest")
	}
	r := d.run("backup", "verify", "-dir", partial, "-pubkey", backupKeyID+"="+filepath.Join(d.dataDir, "keys", backupKeyID+".pub"))
	if r.Code == 0 {
		t.Fatalf("an interrupted backup verified:\n%s", r)
	}
	if r := d.run("backup", "restore", "-dir", partial, "-data-dir", filepath.Join(d.root, "never"), "-pubkey", backupKeyID+"="+filepath.Join(d.dataDir, "keys", backupKeyID+".pub")); r.Code == 0 || fileExists(filepath.Join(d.root, "never", "alixai.db")) {
		t.Fatalf("an interrupted backup was restored:\n%s", r)
	}

	// The live deployment did not notice: the service answers, serves the same receipt, the database
	// passes its integrity check and nothing in the store changed.
	if status, _, err := srv.call("GET", "/healthz", "", nil); err != nil || status != 200 {
		t.Fatalf("/healthz after the killed backup = %d, %v", status, err)
	}
	if status, body := srv.receipt(t, w, prep.SessionID, prep.ID); status != 200 || string(body) != string(receipt) {
		t.Fatalf("the receipt after the killed backup = %d %s", status, body)
	}
	if err := store.IntegrityCheckFile(t.Context(), d.dbPath()); err != nil {
		t.Fatalf("the live database after the killed backup: %v", err)
	}
	after := takeSnapshot(t, d.cfgPath, d.dbPath())
	if err := sameReceipts(before.Receipts, after.Receipts); err != nil {
		t.Fatal(err)
	}
	if err := after.chainsOK(); err != nil {
		t.Fatal(err)
	}

	// The next backup works, and verifies. The leftover .partial is the operator's to delete: nothing
	// removes it.
	next := d.must("backup", "create", "-dir", backups)
	var created struct {
		Dir string `json:"dir"`
	}
	if err := next.decode(&created); err != nil {
		t.Fatal(err)
	}
	d.must("backup", "verify", "-dir", created.Dir, "-pubkey", backupKeyID+"="+filepath.Join(d.dataDir, "keys", backupKeyID+".pub"))
	if !fileExists(partial) {
		t.Error("the next backup removed the interrupted one's working directory: production.md says the operator deletes it")
	}
	recordTiming("backup create, next backup after a killed one", next.Dur, "%d MiB file tree", mib)
	recordObservation("kill during backup",
		"killed `backup create` while it copied a %d MiB file tree: only `%s` was left, with no manifest, and it does not verify or restore; the service and its database were unaffected (%s); the next backup completed and verified; the leftover .partial directory is removed by the operator",
		mib, filepath.Base(partial), after.Summary())
	srv.kill()
}
