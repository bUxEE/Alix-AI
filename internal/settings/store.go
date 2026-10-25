package settings

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("settings", migrations) }

// key addresses one document. It is built only from a verified scope (keyOf), so
// no request value ever reaches it. The columns follow the store's scope rule:
// the coarser levels leave the finer columns empty, which the table's CHECK
// enforces, and every statement below names the level and all five columns.
type key struct {
	level                                   Level
	application, tenant, workspace, subject string
}

// keyOf validates the scope and returns the key of the document of the level
// that scope addresses: the application's (deployment), the tenant's, or the
// principal's own in the scope's workspace (user). There is no way to name
// another tenant, workspace, principal or application.
func keyOf(level Level, scope contracts.Scope) (key, error) {
	if err := scope.Validate(); err != nil {
		return key{}, err
	}
	switch level {
	case LevelDeployment:
		return key{level: level, application: scope.ApplicationID}, nil
	case LevelTenant:
		return key{level: level, application: scope.ApplicationID, tenant: scope.TenantID}, nil
	case LevelUser:
		return key{level: level, application: scope.ApplicationID, tenant: scope.TenantID, workspace: scope.WorkspaceID, subject: scope.PrincipalID}, nil
	}
	return key{}, contracts.New(contracts.ErrValidationFailed, "settings: unknown settings level")
}

// where is the predicate of a key and its arguments, in column order.
const keyWhere = "application_id = ? AND tenant_id = ? AND workspace_id = ? AND level = ? AND subject_id = ?"

func (k key) args() []any {
	return []any{k.application, k.tenant, k.workspace, string(k.level), k.subject}
}

// row is one stored document.
type row struct {
	version   int64
	schemaID  string
	document  []byte
	updatedBy string
	updatedAt time.Time
}

// readRow reads the document at k, or found=false. A row stored under another
// schema than this binary understands is an internal fault: it is never read as
// something it is not.
func readRow(ctx context.Context, q store.Reader, k key) (r row, found bool, err error) {
	var doc string
	var at int64
	err = q.QueryRowContext(ctx,
		"SELECT version, schema_id, document, updated_by, updated_at FROM settings_documents WHERE "+keyWhere,
		k.args()...).Scan(&r.version, &r.schemaID, &doc, &r.updatedBy, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return row{}, false, nil
	case err != nil:
		return row{}, false, err
	}
	if r.schemaID != k.level.SchemaID() {
		return row{}, false, contracts.New(contracts.ErrInternal, "settings: a stored document is of a schema this version does not understand")
	}
	r.document, r.updatedAt = []byte(doc), time.UnixMilli(at).UTC()
	return r, true, nil
}

func (r row) record(level Level) Record {
	at := r.updatedAt
	return Record{Level: level, Version: r.version, Schema: r.schemaID, UpdatedAt: &at, UpdatedBy: visibleUpdater(level, r.updatedBy), Settings: r.document}
}

// visibleUpdater is who a record says last changed it. A deployment document is
// read by every tenant of the application, so naming the principal who wrote it
// would show one tenant the principals of another: it is kept in the store and in
// the audit chain, and left out of the record. A tenant document names its writer
// to the tenant that owns it, and a user document to its owner.
func visibleUpdater(level Level, principal string) string {
	if level == LevelDeployment {
		return ""
	}
	return principal
}

// emptyRecord is what a level with no document returns: version 0 and the empty
// document, which a first write expects.
func emptyRecord(level Level) Record {
	return Record{Level: level, Version: 0, Schema: level.SchemaID(), Settings: []byte("{}")}
}

// wrapStore types a store failure that is not already typed.
func wrapStore(err error, msg string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, msg)
}
