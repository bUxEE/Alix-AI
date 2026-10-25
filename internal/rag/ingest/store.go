package ingest

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Module is the store module name of this package's tables.
const Module = "ragingest"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the embedded migrations, for a test that builds its own
// registry.
func Migrations() fs.FS { return migrationFiles }

func init() { store.RegisterMigrations(Module, migrationFiles) }

// Availability values of a source row.
const (
	availAvailable   = "available"
	availUnavailable = "unavailable"
	availRemoved     = "removed"
)

// Reasons a source is not available (SourceInfo.Reason).
const (
	// ReasonRootUnreachable: the root cannot be reached; nothing was changed.
	ReasonRootUnreachable = "root_unreachable"
	// ReasonMassDeletion: a scan that would delete too much is held for an
	// operator.
	ReasonMassDeletion = "mass_deletion"
	// ReasonLimitExceeded: the source holds more than its declared limits; the
	// scan applied nothing.
	ReasonLimitExceeded = "limit_exceeded"
	// ReasonRemoved: the source was deleted.
	ReasonRemoved = "removed"
	// ReasonTenantDeleted: the source went with its tenant.
	ReasonTenantDeleted = "tenant_deleted"
)

// Version states.
const (
	vPending    = "pending"
	vParsed     = "parsed"
	vIndexed    = "indexed"
	vFailed     = "failed"
	vSkipped    = "skipped"
	vSuperseded = "superseded"
)

// Embed states of a version ("done" is set where the last batch settles).
const (
	embNone    = "none"
	embPending = "pending"
)

// Deletion modes of a source (the declaration's on_source_removed).
const (
	modePurge   = "purge"
	modeDisable = "disable"
)

type rowScanner interface{ Scan(dest ...any) error }

func nsOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeOf(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- sources

// sourceRow is a row of ragingest_source.
type sourceRow struct {
	ID           string
	Fingerprint  string
	Availability string
	Reason       string
	ErrorCode    string
	Message      string
	Since        time.Time
	SnapshotSeq  int64
	LiveItems    int
	LastScan     time.Time
	LastSuccess  time.Time
	Discovered   int
	Skipped      int
	Failed       int
	Tombstoned   int
	DeletionMode string
	PurgeAfter   time.Duration

	HoldDigest   string
	HoldDeleted  int
	HoldPrevious int
	HoldWhy      string
	HoldAt       time.Time

	ConfirmDigest string
	ConfirmBy     string
	ConfirmUntil  time.Time

	Created time.Time
	Updated time.Time
}

const sourceCols = `source_id, fingerprint, availability, reason, error_code, message, since_ns, snapshot_seq, live_items,
 last_scan_ns, last_success_ns, scan_discovered, scan_skipped, scan_failed, scan_tombstoned, deletion_mode, purge_after_ns,
 hold_digest, hold_deleted, hold_previous, hold_why, hold_ns, confirm_digest, confirm_by, confirm_until_ns, created_ns, updated_ns`

func scanSource(sc rowScanner) (sourceRow, error) {
	var r sourceRow
	var since, lastScan, lastOK, purge, holdNS, confirmUntil, created, updated int64
	err := sc.Scan(&r.ID, &r.Fingerprint, &r.Availability, &r.Reason, &r.ErrorCode, &r.Message, &since, &r.SnapshotSeq, &r.LiveItems,
		&lastScan, &lastOK, &r.Discovered, &r.Skipped, &r.Failed, &r.Tombstoned, &r.DeletionMode, &purge,
		&r.HoldDigest, &r.HoldDeleted, &r.HoldPrevious, &r.HoldWhy, &holdNS, &r.ConfirmDigest, &r.ConfirmBy, &confirmUntil, &created, &updated)
	if err != nil {
		return sourceRow{}, err
	}
	r.Since, r.LastScan, r.LastSuccess = timeOf(since), timeOf(lastScan), timeOf(lastOK)
	r.PurgeAfter = time.Duration(purge)
	r.HoldAt, r.ConfirmUntil = timeOf(holdNS), timeOf(confirmUntil)
	r.Created, r.Updated = timeOf(created), timeOf(updated)
	return r, nil
}

// loadSource reads the source row of the scope. ok is false when the scope has
// no such source.
func loadSource(ctx context.Context, r store.Reader, scope contracts.Scope, id string) (row sourceRow, ok bool, err error) {
	q, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL: `SELECT ` + sourceCols + ` FROM ragingest_source`, Where: `source_id = ?`, WhereArgs: []any{id},
	})
	if err != nil {
		return sourceRow{}, false, err
	}
	row, err = scanSource(q)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sourceRow{}, false, nil
	case err != nil:
		return sourceRow{}, false, err
	}
	return row, true, nil
}

// writeSource inserts the row (create) or updates every mutable column.
func writeSource(ctx context.Context, tx *store.Tx, scope contracts.Scope, r sourceRow, create bool) error {
	if create {
		_, err := store.InsertScoped(ctx, tx, scope, "ragingest_source",
			[]string{"source_id", "fingerprint", "availability", "reason", "error_code", "message", "since_ns", "snapshot_seq", "live_items",
				"last_scan_ns", "last_success_ns", "scan_discovered", "scan_skipped", "scan_failed", "scan_tombstoned", "deletion_mode", "purge_after_ns",
				"hold_digest", "hold_deleted", "hold_previous", "hold_why", "hold_ns", "confirm_digest", "confirm_by", "confirm_until_ns", "created_ns", "updated_ns"},
			r.ID, r.Fingerprint, r.Availability, r.Reason, r.ErrorCode, r.Message, nsOf(r.Since), r.SnapshotSeq, r.LiveItems,
			nsOf(r.LastScan), nsOf(r.LastSuccess), r.Discovered, r.Skipped, r.Failed, r.Tombstoned, r.DeletionMode, int64(r.PurgeAfter),
			r.HoldDigest, r.HoldDeleted, r.HoldPrevious, r.HoldWhy, nsOf(r.HoldAt), r.ConfirmDigest, r.ConfirmBy, nsOf(r.ConfirmUntil),
			nsOf(r.Created), nsOf(r.Updated))
		return err
	}
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `UPDATE ragingest_source SET fingerprint = ?, availability = ?, reason = ?, error_code = ?, message = ?, since_ns = ?,
 snapshot_seq = ?, live_items = ?, last_scan_ns = ?, last_success_ns = ?, scan_discovered = ?, scan_skipped = ?, scan_failed = ?,
 scan_tombstoned = ?, deletion_mode = ?, purge_after_ns = ?, hold_digest = ?, hold_deleted = ?, hold_previous = ?, hold_why = ?,
 hold_ns = ?, confirm_digest = ?, confirm_by = ?, confirm_until_ns = ?, updated_ns = ?`,
		Args: []any{r.Fingerprint, r.Availability, r.Reason, r.ErrorCode, r.Message, nsOf(r.Since),
			r.SnapshotSeq, r.LiveItems, nsOf(r.LastScan), nsOf(r.LastSuccess), r.Discovered, r.Skipped, r.Failed,
			r.Tombstoned, r.DeletionMode, int64(r.PurgeAfter), r.HoldDigest, r.HoldDeleted, r.HoldPrevious, r.HoldWhy,
			nsOf(r.HoldAt), r.ConfirmDigest, r.ConfirmBy, nsOf(r.ConfirmUntil), nsOf(r.Updated)},
		Where: `source_id = ?`, WhereArgs: []any{r.ID},
	})
	return err
}

// updateSource reads the source row in a write transaction, lets fn change it
// and writes it back. fn returns false to leave the row alone. A row that
// does not exist is passed to fn as a fresh available one when create is set,
// and is an ErrSourceGone otherwise.
func (s *Service) updateSource(ctx context.Context, scope contracts.Scope, id string, create bool, fn func(tx *store.Tx, r *sourceRow) (bool, error)) (sourceRow, error) {
	var out sourceRow
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		now := s.clk.Now()
		r, ok, err := loadSource(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if !ok {
			if !create {
				return ErrSourceGone
			}
			r = sourceRow{ID: id, Availability: availAvailable, Since: now, DeletionMode: modePurge, Created: now}
		}
		write, err := fn(tx, &r)
		if err != nil || !write {
			out = r
			return err
		}
		r.Updated = now
		if err := writeSource(ctx, tx, scope, r, !ok); err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------- snapshot

func recordOf(key string, size, modNS int64, hash, mime string, seenNS int64) source.Record {
	return source.Record{Key: key, Size: size, ModTime: timeOf(modNS), Hash: hash, MIME: mime, SeenAt: timeOf(seenNS)}
}

// loadSnapshot reads the live records of a source: what the provider's
// Discover takes as its previous argument.
func (s *Service) loadSnapshot(ctx context.Context, r store.Reader, scope contracts.Scope, sourceID string) ([]source.Record, error) {
	rows, err := store.QueryScoped(ctx, r, scope, store.ScopedStmt{
		SQL:       `SELECT item_key, size, mod_ns, hash, mime, seen_ns FROM ragingest_item`,
		Where:     `source_id = ?`,
		WhereArgs: []any{sourceID},
		Tail:      `ORDER BY item_key`,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []source.Record
	for rows.Next() {
		var key, hash, mime string
		var size, modNS, seenNS int64
		if err := rows.Scan(&key, &size, &modNS, &hash, &mime, &seenNS); err != nil {
			return nil, err
		}
		out = append(out, recordOf(key, size, modNS, hash, mime, seenNS))
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- versions

// versionRow is a row of ragingest_version.
type versionRow struct {
	ID           string
	SourceID     string
	GenerationID string
	Key          string
	DocumentID   string
	Hash         string
	Size         int64
	ModNS        int64
	MIME         string
	Format       string
	State        string
	ErrCode      string
	ErrReason    string
	ErrMessage   string
	Chunks       int
	ChunkIDs     []string
	VecOwed      bool
	EmbedState   string
	JobID        string
	Requeues     int
	Created      time.Time
	Updated      time.Time
}

const versionCols = `version_id, source_id, generation_id, item_key, document_id, hash, size, mod_ns, mime, format, state,
 error_code, error_reason, error_message, chunks, chunk_ids, vec_owed, embed_state, job_id, requeues, created_ns, updated_ns`

func scanVersion(sc rowScanner) (versionRow, error) {
	var v versionRow
	var ids string
	var owed int
	var created, updated int64
	err := sc.Scan(&v.ID, &v.SourceID, &v.GenerationID, &v.Key, &v.DocumentID, &v.Hash, &v.Size, &v.ModNS, &v.MIME, &v.Format, &v.State,
		&v.ErrCode, &v.ErrReason, &v.ErrMessage, &v.Chunks, &ids, &owed, &v.EmbedState, &v.JobID, &v.Requeues, &created, &updated)
	if err != nil {
		return versionRow{}, err
	}
	v.ChunkIDs = decodeIDs(ids)
	v.VecOwed = owed == 1
	v.Created, v.Updated = timeOf(created), timeOf(updated)
	return v, nil
}

func decodeIDs(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func encodeIDs(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// getVersion reads one version of the scope.
func getVersion(ctx context.Context, r store.Reader, scope contracts.Scope, id string) (versionRow, error) {
	q, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL: `SELECT ` + versionCols + ` FROM ragingest_version`, Where: `version_id = ?`, WhereArgs: []any{id},
	})
	if err != nil {
		return versionRow{}, err
	}
	v, err := scanVersion(q)
	if errors.Is(err, sql.ErrNoRows) {
		return versionRow{}, ErrSourceGone
	}
	return v, err
}

// queryVersions reads the versions matching a where clause.
func queryVersions(ctx context.Context, r store.Reader, scope contracts.Scope, where string, args []any, tail string) ([]versionRow, error) {
	rows, err := store.QueryScoped(ctx, r, scope, store.ScopedStmt{
		SQL: `SELECT ` + versionCols + ` FROM ragingest_version`, Where: where, WhereArgs: args, Tail: tail,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []versionRow
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func insertVersion(ctx context.Context, tx *store.Tx, scope contracts.Scope, v versionRow) error {
	_, err := store.InsertScoped(ctx, tx, scope, "ragingest_version",
		[]string{"version_id", "source_id", "generation_id", "item_key", "document_id", "hash", "size", "mod_ns", "mime", "format", "state",
			"error_code", "error_reason", "error_message", "chunks", "chunk_ids", "vec_owed", "embed_state", "job_id", "requeues", "created_ns", "updated_ns"},
		v.ID, v.SourceID, v.GenerationID, v.Key, v.DocumentID, v.Hash, v.Size, v.ModNS, v.MIME, v.Format, v.State,
		v.ErrCode, v.ErrReason, v.ErrMessage, v.Chunks, encodeIDs(v.ChunkIDs), boolInt(v.VecOwed), v.EmbedState, v.JobID, v.Requeues,
		nsOf(v.Created), nsOf(v.Updated))
	return err
}

// setVersion updates columns of one version, guarded by the states it may be in
// (empty means any), and reports whether a row changed.
func setVersion(ctx context.Context, tx store.Querier, scope contracts.Scope, id string, set string, args []any, from ...string) (bool, error) {
	where := `version_id = ?`
	wargs := []any{id}
	if len(from) > 0 {
		where += ` AND state IN (` + placeholders(len(from)) + `)`
		for _, f := range from {
			wargs = append(wargs, f)
		}
	}
	res, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `UPDATE ragingest_version SET ` + set, Args: args, Where: where, WhereArgs: wargs,
	})
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// countVersions counts versions matching a where clause.
func countVersions(ctx context.Context, r store.Reader, scope contracts.Scope, where string, args ...any) (int, error) {
	q, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL: `SELECT count(*) FROM ragingest_version`, Where: where, WhereArgs: args,
	})
	if err != nil {
		return 0, err
	}
	var n int
	return n, q.Scan(&n)
}

// ---------------------------------------------------------------- generations

// genRow is a row of ragingest_generation.
type genRow struct {
	ID             string
	SourceID       string
	Lane           lane
	Seeded         bool
	VectorsDropped bool
	Created        time.Time
}

func scanGen(sc rowScanner) (genRow, error) {
	var g genRow
	var ln string
	var seeded, dropped int
	var created int64
	if err := sc.Scan(&g.ID, &g.SourceID, &ln, &seeded, &dropped, &created); err != nil {
		return genRow{}, err
	}
	g.Lane, g.Seeded, g.VectorsDropped, g.Created = lane(ln), seeded == 1, dropped == 1, timeOf(created)
	return g, nil
}

const genCols = `generation_id, source_id, lane, seeded, vectors_dropped, created_ns`

func loadGen(ctx context.Context, r store.Reader, scope contracts.Scope, id string) (genRow, bool, error) {
	q, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL: `SELECT ` + genCols + ` FROM ragingest_generation`, Where: `generation_id = ?`, WhereArgs: []any{id},
	})
	if err != nil {
		return genRow{}, false, err
	}
	g, err := scanGen(q)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return genRow{}, false, nil
	case err != nil:
		return genRow{}, false, err
	}
	return g, true, nil
}

func listGens(ctx context.Context, r store.Reader, scope contracts.Scope, sourceID string) ([]genRow, error) {
	rows, err := store.QueryScoped(ctx, r, scope, store.ScopedStmt{
		SQL: `SELECT ` + genCols + ` FROM ragingest_generation`, Where: `source_id = ?`, WhereArgs: []any{sourceID}, Tail: `ORDER BY created_ns, generation_id`,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []genRow
	for rows.Next() {
		g, err := scanGen(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ensureGen records a generation this module is feeding; it is idempotent.
func (s *Service) ensureGen(ctx context.Context, tx *store.Tx, scope contracts.Scope, sourceID, id string, ln lane, seeded bool) error {
	_, ok, err := loadGen(ctx, tx, scope, id)
	if err != nil || ok {
		return err
	}
	_, err = store.InsertScoped(ctx, tx, scope, "ragingest_generation",
		[]string{"source_id", "generation_id", "lane", "seeded", "vectors_dropped", "created_ns"},
		sourceID, id, string(ln), boolInt(seeded), 0, nsOf(s.clk.Now()))
	return err
}

// ---------------------------------------------------------------- stage and embed rows

func putStage(ctx context.Context, tx *store.Tx, scope contracts.Scope, versionID string, body []byte, now time.Time) error {
	if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `DELETE FROM ragingest_stage`, Where: `version_id = ?`, WhereArgs: []any{versionID},
	}); err != nil {
		return err
	}
	_, err := store.InsertScoped(ctx, tx, scope, "ragingest_stage", []string{"version_id", "body", "created_ns"}, versionID, body, nsOf(now))
	return err
}

func getStage(ctx context.Context, r store.Reader, scope contracts.Scope, versionID string) ([]byte, bool, error) {
	q, err := store.QueryRowScoped(ctx, r, scope, store.ScopedStmt{
		SQL: `SELECT body FROM ragingest_stage`, Where: `version_id = ?`, WhereArgs: []any{versionID},
	})
	if err != nil {
		return nil, false, err
	}
	var body []byte
	switch err := q.Scan(&body); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return body, true, nil
}

func deleteStage(ctx context.Context, tx store.Querier, scope contracts.Scope, versionID string) error {
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `DELETE FROM ragingest_stage`, Where: `version_id = ?`, WhereArgs: []any{versionID},
	})
	return err
}

func deleteEmbedRows(ctx context.Context, tx store.Querier, scope contracts.Scope, versionID string) error {
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `DELETE FROM ragingest_embed`, Where: `version_id = ?`, WhereArgs: []any{versionID},
	})
	return err
}
