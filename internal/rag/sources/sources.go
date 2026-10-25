// Package sources is the durable registry of the sources a deployment serves:
// which declaration of which application package each tenant and workspace
// registered, and the content hash of the package it was bound to.
//
// internal/rag/ingest and internal/rag/query ask an in-memory catalogue (the
// composition root's, internal/cli) which sources exist and may be read. Until
// this package that catalogue lived only as long as the process: after a restart
// the index persisted but no source was registered, so queries returned nothing
// and the pending ingestion jobs were denied as jobs of an unknown source. The
// catalogue now writes every registration here first and reads them back when
// the process starts.
//
// # What is stored
//
// A registration names the declaration, not what it says. The definition of a
// source (its root, rules, limits, deletion behaviour) is rebuilt from the
// active package by source.FromPackage, so a package that was replaced while the
// process was down is read as what it is now, and the host path of an operator's
// mount is never copied into the database. The row keeps what the rebuild
// cannot derive: the source id the core minted, the tenant and workspace the
// definition was bound to (empty for a shared level, not always the scope's), the
// version and content hash of the package, and the scope the source was
// registered under, which is the initiating scope of the work later queued for
// it under the service identity.
//
// # The content hash
//
// ManifestHash is the content address of the package the registration was last
// bound to. It is what makes "the sources of a package that was replaced stop
// being served until they are re-bound" survive a restart: Rebind changes it
// only from the value the caller read (a compare and swap), so two re-audits of
// different package contents cannot overwrite one another, and a registration
// whose hash is not the active package's is stale whatever else the row says.
// This package decides nothing about packages; the catalogue compares.
//
// # Scope
//
// Every table carries application_id, tenant_id and workspace_id and every
// operation but one filters on all three through store.ScopedWhere. The
// exception is LoadAll, the single read of the whole deployment: the process
// calls it once, before it leases a job, because it cannot know which scopes
// have registrations until it has read them, as the catalogue it fills answers
// jobs of every tenant. No request path reaches it, and what it returns is only
// the references above.
package sources

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Module is the store module name of this package's tables.
const Module = "ragsources"

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the embedded migrations, for a test that builds its own
// registry.
func Migrations() fs.FS { return migrationFiles }

func init() { store.RegisterMigrations(Module, migrationFiles) }

// Row states.
const (
	stateActive   = "active"
	stateRemoving = "removing"
)

// maxField bounds the free text a row holds.
const maxField = 256

// Registration is one registered source as the registry holds it.
type Registration struct {
	// Scope is the scope the source was registered under. Its application, tenant
	// and workspace key the row; the rest is the initiating scope of the work
	// queued for the source under the service identity.
	Scope contracts.Scope
	// SourceID is the core-minted src_ id.
	SourceID string
	// PackageSourceID is the slug the package declares the source under: the
	// declaration reference a rebuild starts from.
	PackageSourceID string
	// PackageVersion is the version of the package the registration was bound to.
	PackageVersion string
	// ManifestHash is the content hash of that package.
	ManifestHash string
	// BoundTenantID and BoundWorkspaceID are what the definition names for the
	// two levels (source.Definition.Tenant.ID and Workspace.ID): empty for a
	// shared level.
	BoundTenantID    string
	BoundWorkspaceID string
	// Removing is true from the moment a removal begins until it ends.
	Removing bool
	// CreatedAt is when the source was first registered, UpdatedAt the last change.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate reports whether the registration can be stored. Nothing is defaulted.
func (r Registration) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	bad := func(msg string) error { return contracts.New(contracts.ErrValidationFailed, "ragsources: "+msg) }
	switch {
	case !ids.Valid(r.SourceID, source.IDPrefix):
		return bad("not a source id")
	case !plain(r.PackageSourceID):
		return bad("a registration needs the declaration it was built from (the package-level source id)")
	case r.ManifestHash == "" || !plain(r.ManifestHash):
		return bad("a registration needs the content hash of the package it is bound to")
	case r.PackageVersion != "" && !plain(r.PackageVersion):
		return bad("the package version is not plain text")
	case (r.BoundTenantID != "" && !plain(r.BoundTenantID)) || (r.BoundWorkspaceID != "" && !plain(r.BoundWorkspaceID)):
		return bad("a bound tenant or workspace is not plain text")
	}
	return nil
}

// plain reports whether s is non-empty, short, valid UTF-8 and free of control
// characters and of padding.
func plain(s string) bool {
	if s == "" || len(s) > maxField || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Registry is the durable registry. It is safe for concurrent use.
type Registry struct {
	db  *store.DB
	clk clock.Clock
}

// New returns a Registry over db, which must have this module's migrations
// applied (they are registered with store.Default when this package is
// imported). A nil clock means the wall clock.
func New(db *store.DB, clk clock.Clock) (*Registry, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrInternal, "ragsources: no store")
	}
	if clk == nil {
		clk = clock.Real{}
	}
	return &Registry{db: db, clk: clk}, nil
}

const cols = `application_id, tenant_id, workspace_id, source_id, package_source_id, package_version, manifest_hash,
 bound_tenant_id, bound_workspace_id, scope_json, state, created_ns, updated_ns`

type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (Registration, error) {
	var (
		reg                  Registration
		app, tenant, ws      string
		scopeJSON, state     string
		createdNS, updatedNS int64
	)
	if err := row.Scan(&app, &tenant, &ws, &reg.SourceID, &reg.PackageSourceID, &reg.PackageVersion, &reg.ManifestHash,
		&reg.BoundTenantID, &reg.BoundWorkspaceID, &scopeJSON, &state, &createdNS, &updatedNS); err != nil {
		return Registration{}, err
	}
	if err := json.Unmarshal([]byte(scopeJSON), &reg.Scope); err != nil {
		return Registration{}, corrupt(reg.SourceID, "the stored scope cannot be read")
	}
	// The columns key the row and the JSON is what work is queued under: a row
	// whose two disagree is never served (a scope the row does not belong to
	// would otherwise carry its jobs).
	if reg.Scope.ApplicationID != app || reg.Scope.TenantID != tenant || reg.Scope.WorkspaceID != ws || reg.Scope.Validate() != nil {
		return Registration{}, corrupt(reg.SourceID, "the stored scope is not the scope of the row")
	}
	switch state {
	case stateActive:
	case stateRemoving:
		reg.Removing = true
	default:
		return Registration{}, corrupt(reg.SourceID, "the stored state is unknown")
	}
	reg.CreatedAt, reg.UpdatedAt = time.Unix(0, createdNS).UTC(), time.Unix(0, updatedNS).UTC()
	return reg, nil
}

func corrupt(sourceID, what string) error {
	return contracts.Newf(contracts.ErrInternal, "ragsources: the registration of %s is unreadable: %s", sourceID, what)
}

// errNotFound is the one answer for a source this scope never registered and a
// source of another scope: they cannot be told apart.
func errNotFound() error {
	return contracts.New(contracts.ErrValidationFailed, "ragsources: no such source").WithDetails("reason", "not_found")
}

// IsNotFound reports whether err is the answer for a source that is not
// registered in the scope.
func IsNotFound(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil || ce.Code != contracts.ErrValidationFailed {
		return false
	}
	reason, _ := ce.Details["reason"].(string)
	return reason == "not_found"
}

// Put records a registration, replacing the definition reference, package
// binding and scope of the one the scope already holds under the same source id
// (registering again is idempotent); the creation time is kept. It refuses, with
// contracts.ErrConflict, a source whose removal began (finish the removal first)
// and a second source id for a declaration the scope already registered.
func (r *Registry) Put(ctx context.Context, reg Registration) error {
	if err := reg.Validate(); err != nil {
		return err
	}
	scopeJSON, err := json.Marshal(reg.Scope)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "ragsources: the scope cannot be stored")
	}
	now := r.clk.Now().UnixNano()
	var changed int64
	err = r.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO ragsources_registration (`+cols+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)
			 ON CONFLICT (application_id, tenant_id, workspace_id, source_id) DO UPDATE SET
			   package_source_id = excluded.package_source_id, package_version = excluded.package_version,
			   manifest_hash = excluded.manifest_hash, bound_tenant_id = excluded.bound_tenant_id,
			   bound_workspace_id = excluded.bound_workspace_id, scope_json = excluded.scope_json,
			   updated_ns = excluded.updated_ns
			 WHERE ragsources_registration.state = 'active'`,
			reg.Scope.ApplicationID, reg.Scope.TenantID, reg.Scope.WorkspaceID, reg.SourceID, reg.PackageSourceID, reg.PackageVersion,
			reg.ManifestHash, reg.BoundTenantID, reg.BoundWorkspaceID, string(scopeJSON), now, now)
		if err != nil {
			return err
		}
		changed, err = res.RowsAffected()
		return err
	})
	switch {
	case store.IsUniqueViolation(err):
		return contracts.New(contracts.ErrConflict, "ragsources: the scope already registered this declaration under another source id")
	case err != nil:
		return storeErr(err, "the registration cannot be recorded")
	case changed == 0:
		return contracts.New(contracts.ErrConflict, "ragsources: the source is being removed; finish the removal first")
	}
	return nil
}

// Get returns the registration of a source in the scope. ok is false for a
// source the scope never registered and for another scope's: the two are the
// same answer.
func (r *Registry) Get(ctx context.Context, scope contracts.Scope, sourceID string) (reg Registration, ok bool, err error) {
	row, err := store.QueryRowScoped(ctx, r.db, scope, store.ScopedStmt{
		SQL: `SELECT ` + cols + ` FROM ragsources_registration`, Where: `source_id = ?`, WhereArgs: []any{sourceID},
	})
	if err != nil {
		return Registration{}, false, err
	}
	reg, err = scan(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Registration{}, false, nil
	case err != nil:
		return Registration{}, false, storeErr(err, "the registration cannot be read")
	}
	return reg, true, nil
}

// List returns the registrations of the scope, ordered by source id.
func (r *Registry) List(ctx context.Context, scope contracts.Scope) ([]Registration, error) {
	rows, err := store.QueryScoped(ctx, r.db, scope, store.ScopedStmt{
		SQL: `SELECT ` + cols + ` FROM ragsources_registration`, Tail: `ORDER BY source_id`,
	})
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// LoadAll returns every registration of the deployment, in a stable order: the
// one read across scopes (see the package comment). The process makes it once at
// start, before any job is leased, to fill the catalogue the jobs and queries
// are answered from.
func (r *Registry) LoadAll(ctx context.Context) ([]Registration, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+cols+` FROM ragsources_registration ORDER BY application_id, tenant_id, workspace_id, source_id`)
	if err != nil {
		return nil, storeErr(err, "the registrations cannot be read")
	}
	return collect(rows)
}

func collect(rows *sql.Rows) ([]Registration, error) {
	defer rows.Close()
	var out []Registration
	for rows.Next() {
		reg, err := scan(rows)
		if err != nil {
			var ce *contracts.Error
			if errors.As(err, &ce) {
				return nil, err
			}
			return nil, storeErr(err, "the registrations cannot be read")
		}
		out = append(out, reg)
	}
	if err := rows.Err(); err != nil {
		return nil, storeErr(err, "the registrations cannot be read")
	}
	return out, nil
}

// Rebind binds a registration to another package content: it changes the
// content hash (and the version) only if the registration is active and still
// bound to from, the value the caller read. It reports whether it did. A caller
// that lost the race (another re-audit moved the registration, or the source was
// removed) gets false and changes nothing.
func (r *Registry) Rebind(ctx context.Context, scope contracts.Scope, sourceID, from, to, version string) (bool, error) {
	switch {
	case !plain(from) || !plain(to):
		return false, contracts.New(contracts.ErrValidationFailed, "ragsources: a rebind needs the hash it moves from and the hash it moves to")
	case version != "" && !plain(version):
		return false, contracts.New(contracts.ErrValidationFailed, "ragsources: the package version is not plain text")
	}
	res, err := store.ExecScoped(ctx, r.db, scope, store.ScopedStmt{
		SQL:   `UPDATE ragsources_registration SET manifest_hash = ?, package_version = ?, updated_ns = ?`,
		Args:  []any{to, version, r.clk.Now().UnixNano()},
		Where: `source_id = ? AND manifest_hash = ? AND state = 'active'`, WhereArgs: []any{sourceID, from},
	})
	if err != nil {
		return false, storeErr(err, "the registration cannot be re-bound")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, storeErr(err, "the registration cannot be re-bound")
	}
	return n == 1, nil
}

// MarkRemoving records that the removal of a source began: from now on it is
// read back as removing and never served. It reports whether the source is
// registered; marking one that is already removing is not an error.
func (r *Registry) MarkRemoving(ctx context.Context, scope contracts.Scope, sourceID string) (bool, error) {
	res, err := store.ExecScoped(ctx, r.db, scope, store.ScopedStmt{
		SQL: `UPDATE ragsources_registration SET state = 'removing', updated_ns = ?`, Args: []any{r.clk.Now().UnixNano()},
		Where: `source_id = ?`, WhereArgs: []any{sourceID},
	})
	if err != nil {
		return false, storeErr(err, "the removal cannot be recorded")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, storeErr(err, "the removal cannot be recorded")
	}
	return n == 1, nil
}

// Delete removes the registration of a source in the scope: the end of a
// removal. It reports whether there was one.
func (r *Registry) Delete(ctx context.Context, scope contracts.Scope, sourceID string) (bool, error) {
	res, err := store.ExecScoped(ctx, r.db, scope, store.ScopedStmt{
		SQL: `DELETE FROM ragsources_registration`, Where: `source_id = ?`, WhereArgs: []any{sourceID},
	})
	if err != nil {
		return false, storeErr(err, "the registration cannot be deleted")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, storeErr(err, "the registration cannot be deleted")
	}
	return n == 1, nil
}

// storeErr gives a store failure its contracts code: a busy or locked store is
// unavailable (the caller may retry), the rest is internal. Nothing of the SQL
// reaches the message.
func storeErr(err error, msg string) error {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	if store.IsBusy(err) {
		return contracts.Wrap(contracts.ErrUnavailable, err, "ragsources: "+msg)
	}
	return contracts.Wrap(contracts.ErrInternal, err, "ragsources: "+msg)
}
