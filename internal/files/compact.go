package files

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Deleting is two steps (master plan 19: "deletion tombstones ... before
// physical compaction"):
//
//  1. A tombstone, written in one statement. From that moment the file does not
//     resolve, for anyone, in any scope: lookup filters tombstoned rows. A
//     deleted file, an expired one and every file of a deleted tenant all end up
//     here.
//  2. Compaction, in the background, removes a tombstoned row's blob once no
//     live row of the same scope still uses it (identical content stored twice in
//     a scope is one blob) and then the row. It is idempotent and safe to stop at
//     any point: a blob already gone is not an error, a row left behind is seen
//     again on the next run.
//
// Compact and DeleteTenant work across tenants (or on a whole one) by nature, so
// they are not part of the tenant-facing interface of the gateway: only the
// composition root, running them as service jobs, can reach them.

// compactBatch bounds one page of work so a sweep never holds the writer long.
const compactBatch = 200

// expireBatch bounds the rows one expiry transaction tombstones (a single
// statement each, so a larger batch than compactBatch is still short).
const expireBatch = 1000

// CompactReport is what one compaction did.
type CompactReport struct {
	// Expired counts references that passed their expiry and were tombstoned.
	Expired int
	// Rows counts tombstoned rows removed.
	Rows int
	// Blobs counts blobs removed from disk.
	Blobs int
	// Temp counts stale upload files removed.
	Temp int
}

// Compact expires what is due, removes the blobs and rows of what is deleted
// (and past the tombstone grace) and clears stale upload residue. It returns the
// work it did even when it also returns an error: one file that cannot be
// removed or withdrawn does not stop the others, and is tried again next time.
func (s *Service) Compact(ctx context.Context) (CompactReport, error) {
	var rep CompactReport
	var errs []error
	now := s.clk.Now()

	// 1. Expiry: a due reference is tombstoned. (It already stopped resolving at
	// its expiry; this makes it eligible for removal.) A RAG source is withdrawn
	// from the pipeline first, and stays live until that works.
	// Without a RAG ingestion wired there is nothing to withdraw a source from, so
	// every treatment is tombstoned here.
	// One transaction per expireBatch rows, so the single writer is never held for
	// all of them at once.
	expire := `UPDATE files SET tombstoned_at = ? WHERE seq IN (SELECT seq FROM files WHERE tombstoned_at IS NULL AND expires_at <= ?`
	if s.rag != nil {
		expire += ` AND treatment <> 'rag_source'`
	}
	expire += ` LIMIT ?)`
	for ctx.Err() == nil {
		res, err := s.db.ExecContext(ctx, expire, now.UnixMilli(), now.UnixMilli(), expireBatch)
		if err != nil {
			errs = append(errs, dbFault("expire references", err))
			break
		}
		n, _ := res.RowsAffected()
		rep.Expired += int(n)
		if n < expireBatch {
			break
		}
	}
	if s.rag != nil {
		n, err := s.withdrawRAG(ctx, `tombstoned_at IS NULL AND expires_at <= ? AND treatment = 'rag_source'`, []any{now.UnixMilli()}, now)
		rep.Expired += n
		if err != nil {
			errs = append(errs, err)
		}
	}

	// 2. Physical removal of what has been tombstoned for at least the grace.
	cutoff := now.Add(-s.config().CompactGrace).UnixMilli()
	last := ""
	for ctx.Err() == nil {
		blobs, err := s.tombstonedBlobs(ctx, cutoff, last)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if len(blobs) == 0 {
			break
		}
		for _, b := range blobs {
			last = b.key
			rows, removed, err := s.compactBlob(ctx, b, cutoff)
			rep.Rows += rows
			if removed {
				rep.Blobs++
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
	}

	// 3. Upload residue.
	n, err := s.blobs.sweepTmp(now.Add(-tmpMaxAge))
	rep.Temp = n
	if err != nil {
		errs = append(errs, err)
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return rep, errors.Join(errs...)
}

// withdrawRAG withdraws and then tombstones the RAG-source rows that match
// where. A row whose withdrawal fails stays live and is retried by the next run.
func (s *Service) withdrawRAG(ctx context.Context, where string, args []any, now time.Time) (int, error) {
	var errs []error
	done := 0
	var after int64
	for ctx.Err() == nil {
		rows, err := s.db.QueryContext(ctx,
			`SELECT seq, id, application_id, tenant_id, workspace_id FROM files WHERE seq > ? AND `+where+` ORDER BY seq LIMIT ?`,
			append(append([]any{after}, args...), compactBatch)...)
		if err != nil {
			return done, dbFault("list RAG sources", err)
		}
		type due struct {
			seq                  int64
			id, app, tenant, wsp string
		}
		var batch []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.seq, &d.id, &d.app, &d.tenant, &d.wsp); err != nil {
				rows.Close()
				return done, dbFault("list RAG sources", err)
			}
			batch = append(batch, d)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return done, dbFault("list RAG sources", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, d := range batch {
			after = d.seq
			if err := s.rag.RemoveFile(ctx, Location{ApplicationID: d.app, TenantID: d.tenant, WorkspaceID: d.wsp}, d.id); err != nil {
				s.log.Warn("files: a RAG source could not be withdrawn; it stays until the next run", slog.String("file_id", d.id))
				errs = append(errs, ragFault(err))
				continue
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE files SET tombstoned_at = ? WHERE seq = ? AND tombstoned_at IS NULL`, now.UnixMilli(), d.seq); err != nil {
				errs = append(errs, dbFault("tombstone a file", err))
				continue
			}
			done++
		}
	}
	return done, errors.Join(errs...)
}

// tombstonedBlob names the blob of a tombstoned row.
type tombstonedBlob struct {
	app, tenant, workspace, key string
}

// tombstonedBlobs lists the next page of blobs that have tombstoned rows due for
// removal, after key last.
func (s *Service) tombstonedBlobs(ctx context.Context, cutoff int64, last string) ([]tombstonedBlob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT application_id, tenant_id, workspace_id, blob_key FROM files
 WHERE tombstoned_at IS NOT NULL AND tombstoned_at <= ? AND blob_key > ? ORDER BY blob_key LIMIT ?`,
		cutoff, last, compactBatch)
	if err != nil {
		return nil, dbFault("list tombstones", err)
	}
	defer rows.Close()
	var out []tombstonedBlob
	for rows.Next() {
		var b tombstonedBlob
		if err := rows.Scan(&b.app, &b.tenant, &b.workspace, &b.key); err != nil {
			return nil, dbFault("list tombstones", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, dbFault("list tombstones", err)
	}
	return out, nil
}

// compactBlob removes the due tombstoned rows of one blob and the blob itself if
// no live row still uses it. The check and the removal are one transaction on the
// single writer, the same one Put's insert runs in, so a file stored again at
// this moment either already counts as live here or finds the blob gone and
// stores it afresh.
func (s *Service) compactBlob(ctx context.Context, b tombstonedBlob, cutoff int64) (rows int, removed bool, err error) {
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		rows, removed = 0, false
		var live int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE blob_key = ? AND tombstoned_at IS NULL`, b.key).Scan(&live); err != nil {
			return dbFault("check a blob's references", err)
		}
		if live == 0 {
			dir := tenantDirName(b.app, b.tenant)
			removed = s.blobs.exists(dir, b.key)
			if err := s.blobs.remove(dir, b.key); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM files WHERE blob_key = ? AND tombstoned_at IS NOT NULL AND tombstoned_at <= ?`, b.key, cutoff)
		if err != nil {
			return dbFault("remove tombstoned rows", err)
		}
		n, _ := res.RowsAffected()
		rows = int(n)
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return rows, removed, nil
}

// DeleteTenant deletes every file of the application's tenant named by scope,
// all its workspaces: every one is tombstoned in a single statement, so nothing
// of the tenant resolves from this moment, and Compact removes the bytes (master
// plan 19). Files registered as RAG sources are withdrawn from the pipeline
// first; if one cannot be, DeleteTenant stops with an error and can simply be
// run again. It returns how many files it tombstoned.
//
// The tenant is the one in scope, and the statement never touches another
// application or tenant. The scope is what the lifecycle operation that deletes
// a tenant acts under; there is no request path to this method.
func (s *Service) DeleteTenant(ctx context.Context, scope contracts.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	now := s.clk.Now()
	done := 0
	if s.rag != nil {
		n, err := s.withdrawRAG(ctx, `application_id = ? AND tenant_id = ? AND tombstoned_at IS NULL AND treatment = 'rag_source'`,
			[]any{scope.ApplicationID, scope.TenantID}, now)
		done += n
		if err != nil {
			return done, err
		}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE files SET tombstoned_at = ? WHERE application_id = ? AND tenant_id = ? AND tombstoned_at IS NULL`,
		now.UnixMilli(), scope.ApplicationID, scope.TenantID)
	if err != nil {
		return done, dbFault("tombstone a tenant's files", err)
	}
	n, _ := res.RowsAffected()
	return done + int(n), nil
}

// RecoverReport is what Recover cleaned.
type RecoverReport struct {
	// Temp counts stale upload files removed.
	Temp int
	// Orphans counts blobs on disk that no row names, removed.
	Orphans int
}

// Recover clears what a crash can leave: upload files that were never placed
// and blobs that were placed but whose row was never committed. Only files older
// than an hour are touched, so an upload in flight is safe. It is for start-up,
// not for every compaction: it walks the whole blob tree.
func (s *Service) Recover(ctx context.Context) (RecoverReport, error) {
	var rep RecoverReport
	cutoff := s.clk.Now().Add(-tmpMaxAge)
	n, err := s.blobs.sweepTmp(cutoff)
	rep.Temp = n
	if err != nil {
		return rep, err
	}
	err = s.blobs.walk(cutoff, recoverLookupBatch, func(batch []blobRef) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		named, err := s.namedBlobs(ctx, batch)
		if err != nil {
			return err
		}
		for _, b := range batch {
			if named[b.key] {
				continue // named by a row, live or awaiting compaction
			}
			if err := s.blobs.remove(b.dir, b.key); err != nil {
				return err
			}
			rep.Orphans++
		}
		return nil
	})
	return rep, err
}

// recoverLookupBatch bounds the blobs Recover looks up in one query.
const recoverLookupBatch = 500

// namedBlobs returns the keys of batch that a row names, live or awaiting
// compaction, in one query.
func (s *Service) namedBlobs(ctx context.Context, batch []blobRef) (map[string]bool, error) {
	named := make(map[string]bool, len(batch))
	if len(batch) == 0 {
		return named, nil
	}
	args := make([]any, len(batch))
	for i, b := range batch {
		args[i] = b.key
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT blob_key FROM files WHERE blob_key IN (?`+strings.Repeat(", ?", len(batch)-1)+`)`, args...)
	if err != nil {
		return nil, dbFault("look up blobs", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, dbFault("look up blobs", err)
		}
		named[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, dbFault("look up blobs", err)
	}
	return named, nil
}

// LiveBlobPaths calls fn, once each, with the path of every blob that a live
// (not tombstoned) row of the database q names: relative to the files directory
// of the data directory, slash-separated. A database without the files table
// names none. It is for a backup, which checks that its copy of the files tree
// holds every blob its copy of the database still needs.
func LiveBlobPaths(ctx context.Context, q store.Reader, fn func(path string) error) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'files'`).Scan(&n); err != nil {
		return dbFault("look up the files table", err)
	}
	if n == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT application_id, tenant_id, blob_key FROM files WHERE tombstoned_at IS NULL`)
	if err != nil {
		return dbFault("list live blobs", err)
	}
	defer rows.Close()
	for rows.Next() {
		var app, tenant, key string
		if err := rows.Scan(&app, &tenant, &key); err != nil {
			return dbFault("list live blobs", err)
		}
		p, err := blobPath(tenantDirName(app, tenant), key)
		if err != nil {
			return err
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return dbFault("list live blobs", err)
	}
	return nil
}

// RunCompaction runs Compact every compact_every until ctx is done. onError, if
// not nil, receives each failed run; the loop never stops for one.
func (s *Service) RunCompaction(ctx context.Context, onError func(error)) {
	t := time.NewTicker(s.config().CompactEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Compact(ctx); err != nil && onError != nil && ctx.Err() == nil {
				onError(err)
			}
		}
	}
}
