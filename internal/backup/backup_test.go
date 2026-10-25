package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
)

const testKeyID = "backup-test"

// env is a data directory with an open store (the default registry: audit and
// trust), a ledger table that tests write to, and a signing key.
type env struct {
	t       *testing.T
	dataDir string
	db      *store.DB
	clk     *clock.Fake
	log     *audit.Log
	signer  trust.SigningKey
	keys    StaticKeys
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, dataDir: t.TempDir(), clk: clock.NewFake(t0)}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.signer = trust.SigningKey{KeyID: testKeyID, Key: priv}
	e.keys = StaticKeys{Keys: map[string]ed25519.PublicKey{testKeyID: pub}}
	e.open()
	t.Cleanup(e.close)
	e.exec(`CREATE TABLE ledger_a (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT`)
	e.exec(`CREATE TABLE ledger_b (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT`)
	return e
}

func (e *env) open() {
	e.t.Helper()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(e.dataDir, DatabaseFile), Clock: e.clk})
	if err != nil {
		e.t.Fatalf("store.Open: %v", err)
	}
	e.db = db
	if e.log, err = audit.New(db, audit.Options{Clock: e.clk}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) close() {
	if e.db != nil {
		_ = e.db.Close()
		e.db = nil
	}
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.db.ExecContext(ctx, q, args...); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

var chain = audit.ChainKey{ApplicationID: "app1", TenantID: "tenant1"}

func (e *env) appendAudit(n int) {
	e.t.Helper()
	sc := storetest.Scope("app1", "tenant1", "ws1")
	for i := 0; i < n; i++ {
		e.clk.Advance(time.Second)
		if _, err := e.log.Append(ctx, sc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"n": i}}); err != nil {
			e.t.Fatalf("audit append: %v", err)
		}
	}
}

// auditHead verifies the chain and returns its length and head hash.
func auditHead(t *testing.T, db *store.DB) (int64, string) {
	t.Helper()
	log, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := log.VerifyChain(ctx, chain, audit.VerifyOptions{})
	if err != nil {
		t.Fatalf("audit chain does not verify: %v", err)
	}
	return rep.Events, rep.HeadHash
}

func (e *env) writeFile(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.dataDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) backup(dir string) Result {
	e.t.Helper()
	res, err := Create(ctx, e.db, Options{Dir: dir, DataDir: e.dataDir, Signer: e.signer, Clock: e.clk, Version: "1.2.3"})
	if err != nil {
		e.t.Fatalf("Create: %v", err)
	}
	return res
}

// snapshotDir returns path -> sha256 of every regular file below dir.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *store.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// resign rewrites the manifest of dir: it re-hashes the listed files that still
// exist and signs the result with signer. It is what an attacker who holds the
// key (or a bug) could produce, so the checks below the signature are tested too.
func resign(t *testing.T, dir string, signer trust.SigningKey, mutate func(*Signed)) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	files := m.Signed.Files[:0]
	for _, f := range m.Signed.Files {
		sum, size, err := hashFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			continue
		}
		files = append(files, FileEntry{Path: f.Path, Size: size, SHA256: sum})
	}
	m.Signed.Files = files
	if mutate != nil {
		mutate(&m.Signed)
	}
	sig, err := trust.SignPayload(m.Signed, signer.KeyID, signer.Key, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m.Signatures = []trust.Signature{sig}
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// SEC-075: a backup restores to exactly the state it was taken in, and what was
// in place before the restore is kept, so receipts and audit events written
// after the backup survive it.
func TestBackupRestoreRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.appendAudit(5)
	for i := 1; i <= 20; i++ {
		e.exec(`INSERT INTO ledger_a (n) VALUES (?)`, i)
		e.exec(`INSERT INTO ledger_b (n) VALUES (?)`, i)
	}
	e.writeFile("files/ab/blob-one", "first blob")
	e.writeFile("files/cd/blob-two", "second blob")
	e.writeFile("packages/app1/hash1/alixai.yaml", "name: app1\n")
	wantEvents, wantHead := auditHead(t, e.db)
	if wantEvents != 5 {
		t.Fatalf("events = %d, want 5", wantEvents)
	}
	wantTrees := snapshotDir(t, filepath.Join(e.dataDir, "files"))

	backups := t.TempDir()
	res := e.backup(backups)

	// The service keeps going: more audit events, more rows, a blob deleted.
	e.appendAudit(3)
	e.exec(`INSERT INTO ledger_a (n) VALUES (21)`)
	e.exec(`DELETE FROM ledger_b WHERE n <= 10`)
	if err := os.Remove(filepath.Join(e.dataDir, "files", "ab", "blob-one")); err != nil {
		t.Fatal(err)
	}
	newerEvents, newerHead := auditHead(t, e.db)
	if newerEvents != 8 {
		t.Fatalf("events = %d, want 8", newerEvents)
	}

	if m, err := Verify(ctx, res.Dir, VerifyOptions{Keys: e.keys, Clock: e.clk}); err != nil {
		t.Fatalf("Verify: %v", err)
	} else if m.Signed.BackupID != res.ID || m.Signed.Version != "1.2.3" {
		t.Errorf("manifest = %+v", m.Signed)
	}

	e.close() // the service is stopped
	rr, err := Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: e.dataDir, Keys: e.keys, Clock: e.clk})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rr.BackupID != res.ID || rr.PreRestoreDir == "" {
		t.Fatalf("result = %+v", rr)
	}

	e.open()
	if n := count(t, e.db, `SELECT count(*) FROM ledger_a`); n != 20 {
		t.Errorf("ledger_a rows = %d, want 20", n)
	}
	if n := count(t, e.db, `SELECT count(*) FROM ledger_b`); n != 20 {
		t.Errorf("ledger_b rows = %d, want 20", n)
	}
	events, head := auditHead(t, e.db)
	if events != wantEvents || head != wantHead {
		t.Errorf("audit chain after restore = %d events head %.12s, want %d events head %.12s", events, head, wantEvents, wantHead)
	}
	if err := e.db.IntegrityCheck(ctx); err != nil {
		t.Errorf("integrity after restore: %v", err)
	}
	gotTrees := snapshotDir(t, filepath.Join(e.dataDir, "files"))
	if fmt.Sprint(gotTrees) != fmt.Sprint(wantTrees) {
		t.Errorf("files tree after restore = %v, want %v", gotTrees, wantTrees)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "packages", "app1", "hash1", "alixai.yaml")); err != nil {
		t.Errorf("package tree not restored: %v", err)
	}

	// What was in place before is still there, complete: its audit chain has
	// all eight events, and the deleted blob is in it.
	old, err := store.Open(ctx, store.Options{Path: filepath.Join(rr.PreRestoreDir, DatabaseFile)})
	if err != nil {
		t.Fatalf("open the pre-restore database: %v", err)
	}
	defer old.Close()
	if events, head := auditHead(t, old); events != newerEvents || head != newerHead {
		t.Errorf("pre-restore audit chain = %d events head %.12s, want %d events head %.12s", events, head, newerEvents, newerHead)
	}
	if _, err := os.Stat(filepath.Join(rr.PreRestoreDir, "files", "cd", "blob-two")); err != nil {
		t.Errorf("pre-restore files tree missing: %v", err)
	}

	// A restore into an empty data directory works the same, with nothing to move aside.
	fresh := t.TempDir()
	rr2, err := Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: filepath.Join(fresh, "data"), Keys: e.keys, Clock: e.clk})
	if err != nil {
		t.Fatalf("Restore into an empty directory: %v", err)
	}
	if rr2.PreRestoreDir != "" {
		t.Errorf("PreRestoreDir = %q for an empty data directory", rr2.PreRestoreDir)
	}
	db2, err := store.Open(ctx, store.Options{Path: filepath.Join(fresh, "data", DatabaseFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if events, head := auditHead(t, db2); events != wantEvents || head != wantHead {
		t.Errorf("fresh restore audit chain = %d events head %.12s", events, head)
	}
}

// SEC-081: a backup is created owner-only.
func TestBackupIsOwnerOnly(t *testing.T) {
	e := newEnv(t)
	e.writeFile("files/ab/blob", "x")
	root := filepath.Join(t.TempDir(), "backups")
	res := e.backup(root)
	// Owner only on every platform (M2): on Windows the backup directory gets a
	// protected owner-only ACL that everything in it inherits, instead of what
	// the destination's parent grants.
	if err := fsperm.Check(root, true); err != nil {
		t.Errorf("the backup root it created: %v", err)
	}
	if err := fsperm.Check(res.Dir, true); err != nil {
		t.Errorf("the backup directory: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	err := filepath.WalkDir(res.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", p, st.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// SEC-075: a modified, truncated, incomplete, unlisted-file or wrongly signed
// backup is refused before any live data is replaced.
func TestRestoreRejectsTamperedBackup(t *testing.T) {
	e := newEnv(t)
	e.appendAudit(2)
	e.exec(`INSERT INTO ledger_a (n) VALUES (1)`)
	e.writeFile("files/ab/blob-one", "first blob")
	e.writeFile("packages/app1/hash1/alixai.yaml", "name: app1\n")
	good := e.backup(t.TempDir())
	e.close()

	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other := trust.SigningKey{KeyID: "other-key", Key: otherPriv}

	cases := []struct {
		name   string
		tamper func(t *testing.T, dir string)
	}{
		{"database byte flipped", func(t *testing.T, dir string) {
			p := filepath.Join(dir, DatabaseFile)
			b, _ := os.ReadFile(p)
			b[len(b)/2] ^= 0xff
			os.WriteFile(p, b, 0o600)
		}},
		{"database truncated", func(t *testing.T, dir string) {
			p := filepath.Join(dir, DatabaseFile)
			b, _ := os.ReadFile(p)
			os.WriteFile(p, b[:len(b)/2], 0o600)
		}},
		{"tree file modified", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "tree", "files", "ab", "blob-one"), []byte("evil blob"), 0o600)
		}},
		{"tree file modified, same size", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "tree", "files", "ab", "blob-one"), []byte("FIRST BLOB"), 0o600)
		}},
		{"tree file removed", func(t *testing.T, dir string) {
			os.Remove(filepath.Join(dir, "tree", "files", "ab", "blob-one"))
		}},
		{"unlisted file added", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "tree", "files", "ab", "extra"), []byte("x"), 0o600)
		}},
		{"stale wal beside the database", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, DatabaseFile+"-wal"), []byte("x"), 0o600)
		}},
		{"manifest missing", func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, ManifestFile)) }},
		{"manifest edited, signature kept", func(t *testing.T, dir string) {
			p := filepath.Join(dir, ManifestFile)
			b, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.Replace(string(b), `"version": "1.2.3"`, `"version": "9.9.9"`, 1)), 0o600)
		}},
		{"signatures stripped", func(t *testing.T, dir string) {
			p := filepath.Join(dir, ManifestFile)
			var m Manifest
			b, _ := os.ReadFile(p)
			json.Unmarshal(b, &m)
			m.Signatures = []trust.Signature{}
			b, _ = json.Marshal(m)
			os.WriteFile(p, b, 0o600)
		}},
		{"resigned with an unregistered key", func(t *testing.T, dir string) { resign(t, dir, other, nil) }},
		{"signed for another kind of payload", func(t *testing.T, dir string) {
			resign(t, dir, e.signer, func(s *Signed) { s.Kind = "release-manifest" })
		}},
		{"signed manifest lists a path that climbs out", func(t *testing.T, dir string) {
			resign(t, dir, e.signer, func(s *Signed) {
				s.Files = append(s.Files, FileEntry{Path: "tree/files/../../evil", Size: 1, SHA256: strings.Repeat("a", 64)})
			})
		}},
		{"signed manifest lists an absolute path", func(t *testing.T, dir string) {
			resign(t, dir, e.signer, func(s *Signed) {
				s.Files = append(s.Files, FileEntry{Path: "/etc/passwd", Size: 1, SHA256: strings.Repeat("a", 64)})
			})
		}},
		{"signed manifest lists a file outside every tree", func(t *testing.T, dir string) {
			resign(t, dir, e.signer, func(s *Signed) {
				s.Files = append(s.Files, FileEntry{Path: "tree/keys/audit.key", Size: 1, SHA256: strings.Repeat("a", 64)})
				sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
			})
		}},
		// L4: whoever holds the backup key cannot replace the signing keys, the
		// external-mode secrets or anything else outside files and packages.
		{"signed manifest names the keys tree", func(t *testing.T, dir string) { plantTree(t, dir, e.signer, "keys", "audit-checkpoint.key") }},
		{"signed manifest names the external tree", func(t *testing.T, dir string) { plantTree(t, dir, e.signer, "external", "secrets.enc") }},
		{"signed manifest names an unknown tree", func(t *testing.T, dir string) { plantTree(t, dir, e.signer, "upgrade", "state.json") }},
		{"validly signed but the database is not a database", func(t *testing.T, dir string) {
			junk := make([]byte, 8192)
			rand.Read(junk)
			os.WriteFile(filepath.Join(dir, DatabaseFile), junk, 0o600)
			resign(t, dir, e.signer, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "bkp")
			copyDir(t, good.Dir, bad)
			tc.tamper(t, bad)

			// A live data directory with state that must not change.
			live := t.TempDir()
			os.WriteFile(filepath.Join(live, DatabaseFile), []byte("live database"), 0o600)
			os.MkdirAll(filepath.Join(live, "files"), 0o700)
			os.WriteFile(filepath.Join(live, "files", "keep"), []byte("keep"), 0o600)
			before := snapshotDir(t, live)

			_, err := Restore(ctx, RestoreOptions{BackupDir: bad, DataDir: live, Keys: e.keys, Clock: e.clk})
			if err == nil {
				t.Fatal("Restore accepted a tampered backup")
			}
			if after := snapshotDir(t, live); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Errorf("a refused restore changed the live data directory:\nbefore %v\nafter  %v", before, after)
			}
			if _, verr := Verify(ctx, bad, VerifyOptions{Keys: e.keys, Clock: e.clk}); verr == nil {
				t.Error("Verify accepted a tampered backup")
			}
			t.Logf("refused: %v", err)
		})
	}

	t.Run("untampered backup restores", func(t *testing.T) {
		if _, err := Restore(ctx, RestoreOptions{BackupDir: good.Dir, DataDir: t.TempDir(), Keys: e.keys, Clock: e.clk}); err != nil {
			t.Fatalf("control: %v", err)
		}
	})
	t.Run("no keys refuses", func(t *testing.T) {
		if _, err := Restore(ctx, RestoreOptions{BackupDir: good.Dir, DataDir: t.TempDir()}); err == nil {
			t.Fatal("Restore without keys accepted a backup")
		}
	})
	t.Run("expired signature refuses", func(t *testing.T) {
		late := clock.NewFake(t0.Add(DefaultValidity + time.Hour))
		_, err := Restore(ctx, RestoreOptions{BackupDir: good.Dir, DataDir: t.TempDir(), Keys: e.keys, Clock: late})
		if got := contracts.CodeOf(err); got != contracts.ErrExpired {
			t.Fatalf("code = %s (%v), want %s", got, err, contracts.ErrExpired)
		}
	})
	t.Run("a database that looks in use is not replaced", func(t *testing.T) {
		live := t.TempDir()
		os.WriteFile(filepath.Join(live, DatabaseFile), []byte("live"), 0o600)
		os.WriteFile(filepath.Join(live, DatabaseFile+"-shm"), []byte("shm"), 0o600)
		_, err := Restore(ctx, RestoreOptions{BackupDir: good.Dir, DataDir: live, Keys: e.keys, Clock: e.clk})
		if got := contracts.CodeOf(err); got != contracts.ErrConflict {
			t.Fatalf("code = %s (%v), want %s", got, err, contracts.ErrConflict)
		}
		if b, _ := os.ReadFile(filepath.Join(live, DatabaseFile)); string(b) != "live" {
			t.Error("the live database was touched")
		}
	})
}

// Verify hashes the trees where they are and copies only the database, which
// SQLite must not open inside the backup; a restore copies everything it places.
func TestVerifyCopiesOnlyTheDatabase(t *testing.T) {
	e := newEnv(t)
	e.writeFile("files/ab/blob", strings.Repeat("blob ", 1000))
	e.writeFile("packages/app1/hash1/alixai.yaml", "name: app1\n")
	res := e.backup(t.TempDir())
	for _, copyTrees := range []bool{false, true} {
		scratch := t.TempDir()
		if err := stage(ctx, res.Dir, res.Manifest, scratch, copyTrees); err != nil {
			t.Fatalf("copyTrees=%v: %v", copyTrees, err)
		}
		got := snapshotDir(t, scratch)
		want := 1
		if copyTrees {
			want = len(res.Manifest.Signed.Files)
		}
		if _, ok := got[DatabaseFile]; !ok || len(got) != want {
			t.Errorf("copyTrees=%v: the scratch directory holds %v, want %d files with the database", copyTrees, got, want)
		}
	}
	if runtime.GOOS != "windows" {
		// A tree file replaced by a link to the same bytes is refused in place too.
		bad := filepath.Join(t.TempDir(), "bkp")
		copyDir(t, res.Dir, bad)
		blob := filepath.Join(bad, "tree", "files", "ab", "blob")
		real := filepath.Join(t.TempDir(), "blob")
		if err := os.Rename(blob, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, blob); err == nil {
			if err := stage(ctx, bad, res.Manifest, t.TempDir(), false); err == nil {
				t.Error("a linked tree file was hashed through the link")
			}
		}
	}
}

// putFile records a live file row and writes its blob where internal/files
// keeps it; it returns the blob's path inside the backup.
func (e *env) putFile(id, blobKey string) string {
	e.t.Helper()
	e.exec(`INSERT INTO files (id, application_id, tenant_id, workspace_id, owner_id, session_id, name, mime_type, size_bytes,
 content_hash, blob_key, origin, treatment, visibility, permissions, created_at, expires_at)
 VALUES (?, 'app1', 'tenant1', 'ws1', 'usr', '', 'a.txt', 'text/plain', 4, 'sha256:x', ?, 'upload', 'application_object', 'owner', 'read', 0, 1)`,
		id, blobKey)
	var rel string
	err := files.LiveBlobPaths(ctx, e.db, func(p string) error {
		if strings.HasSuffix(p, blobKey) {
			rel = p
		}
		return nil
	})
	if err != nil || rel == "" {
		e.t.Fatalf("LiveBlobPaths: %q, %v", rel, err)
	}
	e.writeFile("files/"+rel, "blob")
	return TreeRoot + "/files/" + rel
}

// SEC-075: the database snapshot is taken before the trees are copied, and
// compaction in the running service may remove a blob whose row it tombstoned
// after the snapshot. A backup whose copy of the files tree misses a blob that a
// live row of its database names is refused, never published.
func TestBackupRefusesASnapshotWhoseBlobVanished(t *testing.T) {
	t.Cleanup(func() { afterSnapshot = nil })
	e := newEnv(t)
	kept := e.putFile("fil_01J000000000000000000000AA", strings.Repeat("a", 64))
	gone := e.putFile("fil_01J000000000000000000000AB", strings.Repeat("b", 64))
	e.writeFile("files/"+strings.TrimPrefix(gone, TreeRoot+"/files/")+"-unnamed", "x") // a file no row names is fine

	res := e.backup(t.TempDir())
	listed := map[string]bool{}
	for _, f := range res.Manifest.Signed.Files {
		listed[f.Path] = true
	}
	if !listed[kept] || !listed[gone] {
		t.Fatalf("the backup does not list the blobs: %v", res.Manifest.Signed.Files)
	}
	if _, err := Verify(ctx, res.Dir, VerifyOptions{Keys: e.keys, Clock: e.clk}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Compaction removes a blob between the snapshot and the copy of the trees.
	afterSnapshot = func() {
		e.exec(`UPDATE files SET tombstoned_at = 1 WHERE blob_key = ?`, strings.Repeat("b", 64))
		if err := os.Remove(filepath.Join(e.dataDir, filepath.FromSlash(strings.TrimPrefix(gone, TreeRoot+"/")))); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	_, err := Create(ctx, e.db, Options{Dir: dir, DataDir: e.dataDir, Signer: e.signer, Clock: e.clk})
	if !errors.Is(err, ErrBlobMissing) {
		t.Fatalf("Create = %v, want ErrBlobMissing", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused backup left %v behind", entries)
	}

	// The next backup sees the row tombstoned and goes through.
	afterSnapshot = nil
	res = e.backup(t.TempDir())
	if _, err := Verify(ctx, res.Dir, VerifyOptions{Keys: e.keys, Clock: e.clk}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// liveState is what a restore replaces in dir: the database and the default trees.
func liveState(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for p, sum := range snapshotDir(t, dir) {
		if p == DatabaseFile || strings.HasPrefix(p, "files/") || strings.HasPrefix(p, "packages/") {
			out[p] = sum
		}
	}
	return out
}

// A restore cut at any point of its swap leaves a record: the data directory is
// refused until the next restore, which puts the previous state back first, so
// nothing that was in place is lost and the result is the backup's.
func TestRestoreInterruptedMidSwapIsUndone(t *testing.T) {
	t.Cleanup(func() { swapStep = nil })
	for k := 1; k <= 6; k++ { // three files moved aside, three placed
		t.Run(fmt.Sprint("crash before rename ", k), func(t *testing.T) {
			e := newEnv(t)
			e.exec(`INSERT INTO ledger_a (n) VALUES (1)`)
			e.writeFile("files/ab/blob-one", "first blob")
			e.writeFile("packages/app1/hash1/alixai.yaml", "name: app1\n")
			res := e.backup(t.TempDir())
			e.exec(`INSERT INTO ledger_a (n) VALUES (2)`)
			e.writeFile("files/cd/blob-two", "written after the backup")
			e.close()
			before := liveState(t, e.dataDir)

			calls := 0
			swapStep = func(from, to string) error {
				if calls++; calls == k {
					panic("crash")
				}
				return nil
			}
			func() {
				defer func() { _ = recover() }()
				_, _ = Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: e.dataDir, Keys: e.keys, Clock: e.clk})
				t.Fatal("the swap was not cut")
			}()
			swapStep = nil

			if err := InterruptedRestore(e.dataDir); contracts.CodeOf(err) != contracts.ErrConflict {
				t.Fatalf("InterruptedRestore = %v, want a conflict", err)
			}
			if err := undoInterruptedRestore(e.dataDir); err != nil {
				t.Fatalf("undo: %v", err)
			}
			if got := liveState(t, e.dataDir); fmt.Sprint(got) != fmt.Sprint(before) {
				t.Fatalf("after the undo the data directory holds\n%v\nwant\n%v", got, before)
			}
			if err := InterruptedRestore(e.dataDir); err != nil {
				t.Fatalf("InterruptedRestore after the undo: %v", err)
			}
			rr, err := Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: e.dataDir, Keys: e.keys, Clock: e.clk})
			if err != nil {
				t.Fatalf("Restore again: %v", err)
			}
			if got := liveState(t, rr.PreRestoreDir); fmt.Sprint(got) != fmt.Sprint(before) {
				t.Errorf("the pre-restore directory holds\n%v\nwant what was in place\n%v", got, before)
			}
			e.open()
			if n := count(t, e.db, `SELECT count(*) FROM ledger_a`); n != 1 {
				t.Errorf("ledger_a rows = %d, want the backup's 1", n)
			}
		})
	}
}

// A crash between the moves is undone by the next Restore itself, and a failed
// swap that cannot be put back says so and keeps its record.
func TestRestoreUndoIsReported(t *testing.T) {
	t.Cleanup(func() { swapStep = nil })
	e := newEnv(t)
	e.writeFile("files/ab/blob-one", "first blob")
	res := e.backup(t.TempDir())
	e.writeFile("files/cd/blob-two", "written after the backup")
	e.close()
	before := liveState(t, e.dataDir)

	calls := 0
	swapStep = func(from, to string) error {
		if calls++; calls == 2 {
			panic("crash")
		}
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: e.dataDir, Keys: e.keys, Clock: e.clk})
	}()
	swapStep = nil
	rr, err := Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: e.dataDir, Keys: e.keys, Clock: e.clk})
	if err != nil {
		t.Fatalf("Restore after the crash: %v", err)
	}
	if got := liveState(t, rr.PreRestoreDir); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("the pre-restore directory holds\n%v\nwant what was in place\n%v", got, before)
	}

	// The swap fails while placing, and what was moved aside has gone meanwhile:
	// the undo cannot complete, which the error says, and the record stays.
	live := t.TempDir()
	os.WriteFile(filepath.Join(live, DatabaseFile), []byte("live database"), 0o600)
	os.MkdirAll(filepath.Join(live, "files"), 0o700)
	os.WriteFile(filepath.Join(live, "files", "keep"), []byte("keep"), 0o600)
	swapStep = func(from, to string) error {
		if strings.HasSuffix(to, filepath.Join(live, "files")) {
			mk, _, _ := readRestoreMarker(live)
			os.RemoveAll(filepath.Join(live, mk.PreRestore, "files"))
			return errors.New("injected fault")
		}
		return nil
	}
	// The "live database" here is not a database: its revocations cannot be read.
	_, err = Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: live, Keys: e.keys, Clock: e.clk, SkipNewerRevocations: true})
	swapStep = nil
	if err == nil || !strings.Contains(err.Error(), "could not be fully put back") || !strings.Contains(err.Error(), "files was in place") {
		t.Fatalf("Restore = %v, want the undo failure reported", err)
	}
	if InterruptedRestore(live) == nil {
		t.Error("the record of the swap that was not put back is gone")
	}
	if b, _ := os.ReadFile(filepath.Join(live, DatabaseFile)); string(b) != "live database" {
		t.Error("the database that could be put back was not")
	}
}

// A published TEST-ONLY key signs a backup nobody should trust unless the
// operator says so.
func TestDevKeyBackupNeedsExplicitAllow(t *testing.T) {
	e := newEnv(t)
	e.signer = devkeys.SigningKey()
	res := e.backup(t.TempDir())
	keys := StaticKeys{Keys: map[string]ed25519.PublicKey{devkeys.KeyID: devkeys.PublicKey()}}
	if _, err := Verify(ctx, res.Dir, VerifyOptions{Keys: keys, Clock: e.clk}); err == nil {
		t.Fatal("a TEST-ONLY key verified a backup without the explicit allow")
	}
	keys.AllowTestOnly = true
	if _, err := Verify(ctx, res.Dir, VerifyOptions{Keys: keys, Clock: e.clk}); err != nil {
		t.Fatalf("with the explicit allow: %v", err)
	}
}

// A deleted row's bytes are not in the backup (VACUUM INTO copies live pages
// only), which is the secure-delete answer for backups.
func TestBackupDropsDeletedBytes(t *testing.T) {
	e := newEnv(t)
	e.exec(`CREATE TABLE secrets_t (id INTEGER PRIMARY KEY, application_id TEXT NOT NULL, tenant_id TEXT NOT NULL, workspace_id TEXT NOT NULL, body TEXT NOT NULL) STRICT`)
	marker := "deleted-tenant-marker-" + strings.Repeat("Z", 40)
	for i := 0; i < 50; i++ {
		e.exec(`INSERT INTO secrets_t (application_id, tenant_id, workspace_id, body) VALUES ('a','t','w',?)`, fmt.Sprintf("%s-%d", marker, i))
	}
	e.exec(`INSERT INTO secrets_t (application_id, tenant_id, workspace_id, body) VALUES ('a','t','w','kept row')`)
	e.exec(`DELETE FROM secrets_t WHERE body LIKE 'deleted-tenant-marker-%'`)
	res := e.backup(t.TempDir())
	e.close() // folds the WAL into the main file
	live, _ := os.ReadFile(filepath.Join(e.dataDir, DatabaseFile))
	if !strings.Contains(string(live), marker) {
		t.Skip("the live file no longer holds the deleted bytes on this build; nothing to compare")
	}
	copyBytes, err := os.ReadFile(filepath.Join(res.Dir, DatabaseFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(copyBytes), marker) {
		t.Error("the backup still holds the bytes of deleted rows")
	}
	if !strings.Contains(string(copyBytes), "kept row") {
		t.Error("the backup lost a live row")
	}
}

// The service keeps writing during the backup: writers are not blocked, and
// the snapshot is consistent (each transaction wrote two rows; the copy never
// holds half of one).
func TestBackupWhileServing(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 2000; i++ {
		e.exec(`INSERT INTO ledger_a (n) VALUES (?)`, i)
		e.exec(`INSERT INTO ledger_b (n) VALUES (?)`, i)
	}
	e.writeFile("files/ab/blob", "blob")

	var (
		next      atomic.Int64
		writes    atomic.Int64
		maxWrite  atomic.Int64
		stop      = make(chan struct{})
		wg        sync.WaitGroup
		werr      error
		werrMu    sync.Mutex
		duringOne atomic.Int64
		inBackup  atomic.Bool
	)
	next.Store(2000)
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				n := next.Add(1)
				start := time.Now()
				err := e.db.Tx(ctx, func(tx *store.Tx) error {
					if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_a (n) VALUES (?)`, n); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, `INSERT INTO ledger_b (n) VALUES (?)`, n)
					return err
				})
				if err != nil {
					werrMu.Lock()
					werr = err
					werrMu.Unlock()
					return
				}
				if d := time.Since(start).Milliseconds(); d > maxWrite.Load() {
					maxWrite.Store(d)
				}
				writes.Add(1)
				if inBackup.Load() {
					duringOne.Add(1)
				}
			}
		}()
	}
	for writes.Load() < 50 { // writers are up
		time.Sleep(time.Millisecond)
	}
	inBackup.Store(true)
	res, err := Create(ctx, e.db, Options{Dir: t.TempDir(), DataDir: e.dataDir, Signer: e.signer, Clock: e.clk})
	inBackup.Store(false)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Create while serving: %v", err)
	}
	werrMu.Lock()
	defer werrMu.Unlock()
	if werr != nil {
		t.Fatalf("a writer failed while the backup ran: %v", werr)
	}
	t.Logf("writes total %d, during the backup %d, slowest write %d ms", writes.Load(), duringOne.Load(), maxWrite.Load())
	if maxWrite.Load() > 5000 {
		t.Errorf("a write took %d ms: writers were blocked by the backup", maxWrite.Load())
	}

	restored := filepath.Join(t.TempDir(), "data")
	if _, err := Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: restored, Keys: e.keys, Clock: e.clk}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(restored, DatabaseFile)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Fatalf("restored copy fails its integrity check: %v", err)
	}
	a, b := count(t, db, `SELECT count(*) FROM ledger_a`), count(t, db, `SELECT count(*) FROM ledger_b`)
	if a != b {
		t.Fatalf("torn snapshot: ledger_a has %d rows, ledger_b %d", a, b)
	}
	if a < 2000 {
		t.Fatalf("snapshot has %d rows, fewer than were committed before the backup began", a)
	}
	if n := count(t, db, `SELECT count(*) FROM ledger_a x LEFT JOIN ledger_b y ON x.n = y.n WHERE y.n IS NULL`); n != 0 {
		t.Fatalf("%d rows of ledger_a have no partner in ledger_b", n)
	}
	var maxN int
	if err := db.QueryRowContext(ctx, `SELECT max(n) FROM ledger_a`).Scan(&maxN); err != nil || maxN < a {
		t.Fatalf("max n = %d (%v), rows %d: the snapshot is not a prefix of the writes", maxN, err, a)
	}
}

// An upload being received (files/.tmp) and a package install being staged
// (packages/<app>/.staging-*) are scratch that no committed row names: they are
// left out instead of refusing the backup, and nothing else that is hidden is.
func TestBackupSkipsTransientDirs(t *testing.T) {
	e := newEnv(t)
	e.writeFile("files/ab/blob", "blob")
	e.writeFile("files/.tmp/0123456789abcdef", "upload in progress")
	e.writeFile("packages/app1/hash1/alixai.yaml", "name: app1\n")
	e.writeFile("packages/app1/.staging-123/alixai.yaml", "crashed install\n")
	res := e.backup(t.TempDir())
	for _, f := range res.Manifest.Signed.Files {
		if strings.Contains(f.Path, ".tmp") || strings.Contains(f.Path, ".staging-") {
			t.Errorf("transient file %s is in the backup", f.Path)
		}
	}
	if _, err := Verify(ctx, res.Dir, VerifyOptions{Keys: e.keys, Clock: e.clk}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "data")
	if _, err := Restore(ctx, RestoreOptions{BackupDir: res.Dir, DataDir: restored, Keys: e.keys, Clock: e.clk}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, p := range []string{"files/ab/blob", "packages/app1/hash1/alixai.yaml"} {
		if _, err := os.Stat(filepath.Join(restored, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s not restored: %v", p, err)
		}
	}

	for _, hidden := range []string{"files/.other/x", "packages/app1/hash1/.hidden"} {
		e := newEnv(t)
		e.writeFile(hidden, "x")
		if _, err := Create(ctx, e.db, Options{Dir: t.TempDir(), DataDir: e.dataDir, Signer: e.signer, Clock: e.clk}); err == nil {
			t.Errorf("%s did not refuse the backup", hidden)
		}
	}
}

func TestCreateRefusals(t *testing.T) {
	e := newEnv(t)
	for name, o := range map[string]Options{
		"no destination":      {DataDir: e.dataDir, Signer: e.signer},
		"no signing key":      {Dir: t.TempDir(), DataDir: e.dataDir},
		"bad tree name":       {Dir: t.TempDir(), DataDir: e.dataDir, Signer: e.signer, Trees: []string{"../x"}},
		"trees but no data":   {Dir: t.TempDir(), Signer: e.signer},
		"destination in tree": {Dir: filepath.Join(e.dataDir, "files", "bkp"), DataDir: e.dataDir, Signer: e.signer},
	} {
		if _, err := Create(ctx, e.db, o); err == nil {
			t.Errorf("%s: Create succeeded", name)
		}
	}
	// Database only needs no data directory.
	if _, err := Create(ctx, e.db, Options{Dir: t.TempDir(), Signer: e.signer, Trees: []string{}}); err != nil {
		t.Errorf("database-only backup: %v", err)
	}
	// A link in a tree refuses the backup rather than being skipped.
	if runtime.GOOS != "windows" {
		e.writeFile("files/ab/real", "x")
		if err := os.Symlink(filepath.Join(e.dataDir, "files", "ab", "real"), filepath.Join(e.dataDir, "files", "ab", "link")); err == nil {
			if _, err := Create(ctx, e.db, Options{Dir: t.TempDir(), DataDir: e.dataDir, Signer: e.signer}); err == nil {
				t.Error("a symbolic link inside a tree did not refuse the backup")
			}
		}
	}
}

// plantTree adds tree/<name>/<file> to the backup in dir and re-signs a
// manifest that lists it as a tree of its own.
func plantTree(t *testing.T, dir string, signer trust.SigningKey, name, file string) {
	t.Helper()
	p := filepath.Join(dir, TreeRoot, name, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, size, err := hashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	resign(t, dir, signer, func(s *Signed) {
		s.Trees = append(s.Trees, name)
		sort.Strings(s.Trees)
		s.Files = append(s.Files, FileEntry{Path: TreeRoot + "/" + name + "/" + file, Size: size, SHA256: sum})
		sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
	})
}

// L4: a backup is made of files and packages only.
func TestCreateRefusesTreesARestoreWouldNotTake(t *testing.T) {
	e := newEnv(t)
	for _, tree := range []string{"keys", "external", "upgrade", "voice-packs"} {
		if _, err := Create(ctx, e.db, Options{Dir: t.TempDir(), DataDir: e.dataDir, Signer: e.signer, Trees: []string{"files", tree}}); err == nil {
			t.Errorf("a backup of the %s tree was made", tree)
		}
	}
	if _, err := Create(ctx, e.db, Options{Dir: t.TempDir(), DataDir: e.dataDir, Signer: e.signer, Trees: []string{"packages", "files"}}); err != nil {
		t.Errorf("files and packages: %v", err)
	}
}
