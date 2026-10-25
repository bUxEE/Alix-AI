package files

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// insertExpired inserts n live rows of one scope, seq 1..n, that expired in
// that order before the env's clock, each naming blob key printf('%064d', seq).
func (e *env) insertExpired(n int) {
	e.t.Helper()
	now := e.clk.Now().UnixMilli()
	_, err := e.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
INSERT INTO files (seq, id, application_id, tenant_id, workspace_id, owner_id, session_id, name, mime_type, size_bytes,
 content_hash, blob_key, origin, treatment, visibility, permissions, created_at, expires_at)
SELECT i, 'fil_' || i, 'app', 'tenant', 'ws', 'usr', '', 'f.txt', 'text/plain', 1,
 'sha256:x', printf('%064d', i), 'upload', 'application_object', 'owner', 'read', ? - 2 * ? + i, ? - ? + i FROM n`,
		n, now, n, now, n)
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count(q string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// Expiry tombstones in transactions of at most expireBatch rows, so the single
// writer is never held for all of them at once: a failure in a later batch keeps
// what the earlier ones did, and the next run finishes the rest.
func TestCompactExpiresInBatches(t *testing.T) {
	e := newEnv(t)
	const rows = expireBatch + 500
	e.insertExpired(rows)
	if _, err := e.db.ExecContext(ctx, `CREATE TRIGGER expire_fault BEFORE UPDATE OF tombstoned_at ON files
 WHEN new.seq = `+strconv.Itoa(rows)+` BEGIN SELECT RAISE(ABORT, 'injected fault'); END`); err != nil {
		t.Fatal(err)
	}
	rep, err := e.svc.Compact(ctx)
	if err == nil {
		t.Fatal("the injected fault was not reported")
	}
	if got := e.count(`SELECT COUNT(*) FROM files WHERE tombstoned_at IS NOT NULL`); got != expireBatch || rep.Expired != expireBatch {
		t.Fatalf("%d rows tombstoned (reported %d), want the first batch of %d kept", got, rep.Expired, expireBatch)
	}
	if _, err := e.db.ExecContext(ctx, `DROP TRIGGER expire_fault`); err != nil {
		t.Fatal(err)
	}
	if rep, err = e.svc.Compact(ctx); err != nil || rep.Expired != rows-expireBatch {
		t.Fatalf("second run = %+v, %v; want the remaining %d expired", rep, err, rows-expireBatch)
	}
	if got := e.count(`SELECT COUNT(*) FROM files WHERE tombstoned_at IS NULL`); got != 0 {
		t.Errorf("%d expired rows are still live", got)
	}
}

// LiveBlobPaths names, once each, the blobs on disk that live rows use, and not
// the blob only a tombstoned row names.
func TestLiveBlobPathsNamesTheBlobsLiveRowsUse(t *testing.T) {
	e := newEnv(t)
	a := e.mustPut(scopeA, "a.txt", "text/plain", textBytes("first"))
	e.mustPut(scopeA, "a-again.txt", "text/plain", textBytes("first"))
	b := e.mustPut(scopeB, "b.txt", "text/plain", textBytes("second"))
	gone := e.mustPut(scopeA, "gone.txt", "text/plain", textBytes("deleted"))
	if err := e.svc.tombstone(ctx, scopeA, string(gone.FileID)); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := LiveBlobPaths(ctx, e.db, func(p string) error { got = append(got, p); return nil }); err != nil {
		t.Fatal(err)
	}
	onDisk := map[string]bool{}
	for _, p := range diskBlobs(t, e.dir) {
		onDisk[p] = true
	}
	want := map[string]bool{}
	for i, sc := range []contracts.Scope{scopeA, scopeB} {
		key := e.blobKeyOf(string([]conversation.FileRef{a, b}[i].FileID))
		want[path.Join(tenantDirName(sc.ApplicationID, sc.TenantID), key[:2], key)] = true
	}
	if len(got) != 2 {
		t.Fatalf("LiveBlobPaths = %v, want the two live blobs once each", got)
	}
	for _, p := range got {
		if !want[p] || !onDisk[p] {
			t.Errorf("%s is not a live blob on disk (on disk: %v)", p, onDisk)
		}
	}
}

// A blob's rename is made durable: the directory it was renamed into is synced,
// and a failure to sync fails the upload like a failure to sync the file does.
func TestPlaceSyncsTheDirectory(t *testing.T) {
	e := newEnv(t)
	orig := syncDir
	t.Cleanup(func() { syncDir = orig })
	var synced []string
	syncDir = func(r *os.Root, dir string) error {
		if _, err := r.Stat(dir); err != nil {
			t.Errorf("synced %s, which is not there: %v", dir, err)
		}
		synced = append(synced, dir)
		return nil
	}
	ref := e.mustPut(scopeA, "a.txt", "text/plain", textBytes("hello"))
	key := e.blobKeyOf(string(ref.FileID))
	if want := path.Join(tenantDirName(scopeA.ApplicationID, scopeA.TenantID), key[:2]); len(synced) != 1 || synced[0] != want {
		t.Fatalf("synced %v, want [%s]", synced, want)
	}

	syncDir = func(*os.Root, string) error { return errors.New("injected fault") }
	_, err := e.put(scopeA, "b.txt", "text/plain", textBytes("other content"))
	wantCode(t, err, contracts.ErrUnavailable)
	if n := e.rowCount(); n != 1 {
		t.Errorf("%d rows after a failed directory sync, want 1", n)
	}
}

// Recover looks blobs up in batches; across the batch boundaries it removes
// exactly the blobs no row names, and a row awaiting compaction still names its
// blob.
func TestRecoverLooksUpBlobsInBatches(t *testing.T) {
	e := newEnvAt(t, time.Now().UTC())
	const blobs = 2*recoverLookupBatch + 200
	e.insertExpired(blobs)
	dir := filepath.Join(e.dir, "files", tenantDirName("app", "tenant"))
	for i := 1; i <= blobs; i++ {
		key := fmt.Sprintf("%064d", i)
		p := filepath.Join(dir, key[:2], key)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.ExecContext(ctx, `DELETE FROM files WHERE seq % 3 <> 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE files SET tombstoned_at = 1 WHERE seq % 6 = 0`); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Hour)
	rep, err := e.svc.Recover(ctx)
	if want := blobs - blobs/3; err != nil || rep.Orphans != want {
		t.Fatalf("Recover = %+v, %v; want %d orphans", rep, err, want)
	}
	left := diskBlobs(t, e.dir)
	if len(left) != blobs/3 {
		t.Fatalf("%d blobs left, want %d", len(left), blobs/3)
	}
	for _, p := range left {
		var i int
		if _, err := fmt.Sscanf(path.Base(p), "%d", &i); err != nil || i%3 != 0 {
			t.Errorf("blob %s was kept but no row names it (%v)", p, err)
		}
	}
}
