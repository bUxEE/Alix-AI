// Package vec is the store-side glue for sqlite-vec, the vector extension the
// pure-Go SQLite driver bundles (modernc.org/sqlite/vec, sqlite-vec v0.1.9).
// It is candidate B of ADR 0011, benchmarked head to head with the exact flat
// scan (option A) and kept available behind the same VectorIndexProvider
// interface, but NOT linked into the service by default.
//
// # Why this is a separate package
//
// ADR 0009 confines the SQLite driver to internal/store. Importing the
// extension is the same kind of act and has two process-wide effects that only
// the composition root should choose: it calls sqlite3_auto_extension, so
// sqlite-vec is loaded into every connection the process opens (about +0.3 MB
// of binary), and it rules out the driver's pluggable page cache
// (sqlite.RegisterPageCache). The flat provider needs neither, so the service
// links this package only when an operator selects the sqlite-vec provider,
// and internal/rag/vector's own production files never import it
// (TestProductionCodeStaysBehindTheStore).
//
// # What it provides
//
//   - the extension (blank import), and Version to prove it is loaded;
//   - the schema, as a migrations module (Register) that is added to a registry
//     only on purpose, never to store.Default, because vec0 virtual tables
//     cannot be created by a process without the extension;
//   - scope-bound operations on the vec0 table (Insert, Remove, Code, KNN):
//     every one takes a validated contracts.Scope and constrains all three
//     scope columns and the generation by equality, which is what makes vec0
//     read only that partition.
//
// The dimension is fixed by the table definition (Dim). A model of another
// dimension needs another table, which is one of the reasons the flat scan is
// the default.
package vec

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"io/fs"

	_ "modernc.org/sqlite/vec" // registers sqlite-vec for every connection of the process

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Dim is the vector dimension of the vec0 table: Granite Embedding 97M
// Multilingual R2 produces 384-dimensional vectors.
const Dim = 384

// Module is the migrations module name of the schema.
const Module = "ragvector_vec0"

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the schema as a filesystem for store.RegisterMigrations
// or Registry.Register.
func Migrations() fs.FS { return migrations }

// Register adds the schema to reg. A nil reg means store.Default. Call it from
// the composition root before opening the store, and only when the sqlite-vec
// provider is selected.
func Register(reg *store.Registry) error {
	if reg == nil {
		reg = store.Default
	}
	return reg.Register(Module, migrations)
}

// ShadowTables lists the tables vec0 creates for itself that are ordinary
// tables to SQLite and therefore to the scope lint, although they hold no
// scope columns. Pass them to storetest.AssertScopedTables.
func ShadowTables() []string { return []string{"vecb_vectors_vector_chunks00"} }

// Version returns the loaded sqlite-vec version, or an error when the
// extension is not available on the connection.
func Version(ctx context.Context, q store.Reader) (string, error) {
	var v string
	if err := q.QueryRowContext(ctx, `SELECT vec_version()`).Scan(&v); err != nil {
		return "", contracts.Wrap(contracts.ErrUnavailable, err, "sqlite-vec is not loaded in this process")
	}
	return v, nil
}

// Neighbour is one KNN result.
type Neighbour struct {
	RowID   int64
	ChunkID string
	// Distance is the cosine distance, 1 - cosine similarity, in [0, 2].
	Distance float64
}

func checkCode(code []byte) error {
	if len(code) != Dim {
		return contracts.Newf(contracts.ErrValidationFailed, "vec: a vector has %d dimensions, the sqlite-vec table is %d", len(code), Dim)
	}
	return nil
}

// Insert adds one int8 vector to the generation of the scope and returns its
// rowid. code holds Dim signed bytes.
func Insert(ctx context.Context, w store.Querier, scope contracts.Scope, generation, chunkID string, code []byte) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := checkCode(code); err != nil {
		return 0, err
	}
	res, err := w.ExecContext(ctx,
		`INSERT INTO vecb_vectors (application_id, tenant_id, workspace_id, generation_id, embedding, chunk_id) VALUES (?, ?, ?, ?, vec_int8(?), ?)`,
		scope.ApplicationID, scope.TenantID, scope.WorkspaceID, generation, code, chunkID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Remove deletes a vector by rowid, after checking that the row belongs to the
// scope (a rowid is not a capability). It reports whether a row was removed.
func Remove(ctx context.Context, w store.Querier, scope contracts.Scope, rowID int64) (bool, error) {
	if err := scope.Validate(); err != nil {
		return false, err
	}
	var app, tenant, ws string
	err := w.QueryRowContext(ctx, `SELECT application_id, tenant_id, workspace_id FROM vecb_vectors WHERE rowid = ?`, rowID).Scan(&app, &tenant, &ws)
	switch {
	case err != nil && isNoRows(err):
		return false, nil
	case err != nil:
		return false, err
	case app != scope.ApplicationID || tenant != scope.TenantID || ws != scope.WorkspaceID:
		return false, contracts.New(contracts.ErrScopeViolation, "vec: the vector belongs to another scope")
	}
	if _, err := w.ExecContext(ctx, `DELETE FROM vecb_vectors WHERE rowid = ?`, rowID); err != nil {
		return false, err
	}
	return true, nil
}

// Code returns the stored int8 vector of a rowid of the scope.
func Code(ctx context.Context, r store.Reader, scope contracts.Scope, rowID int64) ([]byte, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var code []byte
	err := r.QueryRowContext(ctx,
		`SELECT embedding FROM vecb_vectors WHERE rowid = ? AND application_id = ? AND tenant_id = ? AND workspace_id = ?`,
		rowID, scope.ApplicationID, scope.TenantID, scope.WorkspaceID).Scan(&code)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return code, nil
}

// KNN returns the k nearest vectors of the generation of the scope to code, by
// cosine distance, nearest first. All four partition keys are constrained by
// equality, so vec0 reads only that partition; a query without them would scan
// every scope, which is why no caller can build one: there is no function here
// that omits the scope.
func KNN(ctx context.Context, r store.Reader, scope contracts.Scope, generation string, code []byte, k int) ([]Neighbour, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := checkCode(code); err != nil {
		return nil, err
	}
	if k < 1 {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "vec: k %d is not positive", k)
	}
	rows, err := r.QueryContext(ctx,
		`SELECT rowid, distance, chunk_id FROM vecb_vectors
		 WHERE embedding MATCH vec_int8(?) AND k = ?
		   AND application_id = ? AND tenant_id = ? AND workspace_id = ? AND generation_id = ?`,
		code, k, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Neighbour
	for rows.Next() {
		var n Neighbour
		if err := rows.Scan(&n.RowID, &n.Distance, &n.ChunkID); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
