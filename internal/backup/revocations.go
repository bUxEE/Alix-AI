package backup

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// A backup is a database as it was when it was taken. Restoring an older one
// would bring back as active every publisher key (internal/trust) and issuer
// key (internal/identity) that was revoked after it was taken: a key revoked as
// compromised would verify again. So a restore carries the revocations of the
// database it replaces into the one it puts in place, before the swap (security
// finding L3). Revocation is permanent in both stores (triggers refuse to undo
// one); carrying it over only ever revokes more.
//
// Only the columns of the first migration of each table are read and written,
// so a backup made by an older version, or a live database migrated by a newer
// one (an upgrade rolling back), works the same.

// keyTable describes one table of revocable keys.
type keyTable struct {
	name string
	// columns are copied for a revoked row the backup does not hold at all.
	columns []string
	// revoke selects the backup's rows a revoked live row revokes, by its key
	// material: in the trust store a revocation covers the material under every
	// application (trust.Store.LookupKey), for issuer keys within the
	// application. Its arguments are the application id and the fingerprint.
	revoke string
	// present selects the rows that keep a revoked row from being added: its
	// key id is taken, or its material is registered. Its arguments are the
	// application id, the key id and the fingerprint.
	present string
}

var revocableKeyTables = []keyTable{
	{
		name: "trust_publisher_keys",
		columns: []string{"application_id", "key_id", "public_key", "fingerprint", "status", "label",
			"registered_at", "retired_at", "revoked_at", "revoked_reason"},
		revoke:  "fingerprint = ?2",
		present: "(application_id = ?1 AND key_id = ?2) OR (application_id = ?1 AND fingerprint = ?3)",
	},
	{
		name: "identity_issuer_keys",
		columns: []string{"application_id", "key_id", "issuer", "public_key", "fingerprint", "status", "dev_key",
			"label", "registered_at", "not_after", "revoked_at", "revoked_reason"},
		revoke:  "application_id = ?1 AND fingerprint = ?2",
		present: "key_id = ?2 OR (application_id = ?1 AND fingerprint = ?3)",
	},
}

// revokedRow is one revoked key of the live database, every column of the
// table's first migration, in keyTable.columns order.
type revokedRow struct {
	values                       []any
	applicationID, keyID, finger string
	revokedAt                    int64
	reason                       string
}

// liveRevocations reads the revoked keys of the database at dbPath, through a
// copy (with its -wal, which may hold the newest revocations) made in scratch:
// the live database itself is never opened, so nothing about it changes before
// the swap. A missing database has none.
func liveRevocations(ctx context.Context, dbPath, scratch string) (map[string][]revokedRow, error) {
	if !exists(dbPath) {
		return nil, nil
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	copyPath := filepath.Join(scratch, DatabaseFile)
	for _, suffix := range []string{"", "-wal"} {
		if !exists(dbPath + suffix) {
			continue
		}
		if err := copyPlain(dbPath+suffix, copyPath+suffix); err != nil {
			return nil, err
		}
	}
	db, err := store.Open(ctx, store.Options{Path: copyPath, Migrations: store.NewRegistry()})
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := map[string][]revokedRow{}
	for _, t := range revocableKeyTables {
		ok, err := tableExists(ctx, db, t.name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		rows, err := readRevoked(ctx, db, t)
		if err != nil {
			return nil, err
		}
		out[t.name] = rows
	}
	return out, nil
}

func readRevoked(ctx context.Context, db *store.DB, t keyTable) ([]revokedRow, error) {
	q := "SELECT " + joinColumns(t.columns) + " FROM " + t.name + " WHERE status = 'revoked' ORDER BY revoked_at, key_id"
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []revokedRow
	for rows.Next() {
		vals := make([]any, len(t.columns))
		ptrs := make([]any, len(t.columns))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := revokedRow{values: vals}
		for i, c := range t.columns {
			switch c {
			case "application_id":
				r.applicationID, _ = vals[i].(string)
			case "key_id":
				r.keyID, _ = vals[i].(string)
			case "fingerprint":
				r.finger, _ = vals[i].(string)
			case "revoked_at":
				r.revokedAt, _ = vals[i].(int64)
			case "revoked_reason":
				r.reason, _ = vals[i].(string)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// applyRevocations revokes, in the staged database at dbPath, every key the
// live database revoked: rows of the same key id or the same key material that
// are not revoked yet take the live revocation (time and reason), and a revoked
// key the backup does not hold at all is added as the revoked record, so its
// material cannot be registered again. It returns how many keys it revoked or
// added. The staged database is left as one file: no -wal beside it, since the
// swap moves only the main file.
func applyRevocations(ctx context.Context, dbPath string, live map[string][]revokedRow) (int, error) {
	total := 0
	for _, rows := range live {
		total += len(rows)
	}
	if total == 0 {
		return 0, nil
	}
	db, err := store.Open(ctx, store.Options{Path: dbPath, Migrations: store.NewRegistry()})
	if err != nil {
		return 0, err
	}
	changed := 0
	err = db.Tx(ctx, func(tx *store.Tx) error {
		for _, t := range revocableKeyTables {
			rows := live[t.name]
			if len(rows) == 0 {
				continue
			}
			ok, err := tableExists(ctx, tx, t.name)
			if err != nil {
				return err
			}
			if !ok {
				continue // a backup from before the table existed: it holds none of these keys
			}
			for _, r := range rows {
				n, err := revokeOne(ctx, tx, t, r)
				if err != nil {
					return err
				}
				changed += n
			}
		}
		return nil
	})
	cerr := db.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if st, serr := os.Stat(dbPath + "-wal"); serr == nil && st.Size() > 0 {
		return 0, errors.New("the staged database kept a write-ahead log after the revocations were applied")
	}
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	return changed, nil
}

func revokeOne(ctx context.Context, tx *store.Tx, t keyTable, r revokedRow) (int, error) {
	res, err := tx.ExecContext(ctx, "UPDATE "+t.name+" SET status = 'revoked', revoked_at = ?3, revoked_reason = ?4 WHERE status <> 'revoked' AND "+t.revoke,
		r.applicationID, r.finger, r.revokedAt, r.reason)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	var present int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.name+" WHERE "+t.present, r.applicationID, r.keyID, r.finger).Scan(&present); err != nil {
		return 0, err
	}
	if present == 0 {
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+t.name+" ("+joinColumns(t.columns)+") VALUES ("+placeholders(len(t.columns))+")", r.values...); err != nil {
			return 0, err
		}
		n++
	}
	return int(n), nil
}

func tableExists(ctx context.Context, q store.Reader, name string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func joinColumns(cols []string) string {
	s := ""
	for i, c := range cols {
		if i > 0 {
			s += ", "
		}
		s += c
	}
	return s
}

func placeholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ", "
		}
		s += "?"
	}
	return s
}

// copyPlain copies the regular file src to dst (which must not exist).
func copyPlain(src, dst string) error {
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return contracts.Newf(contracts.ErrValidationFailed, "%s is a link or special file", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
