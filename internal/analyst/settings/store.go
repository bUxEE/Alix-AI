package settings

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("analyst_settings", migrations) }

// Store persists tenant analyst settings.
type Store struct {
	db    *store.DB
	clk   clock.Clock
	audit *audit.Log
}

// NewStore opens the tenant settings store. audit may be nil in pure unit tests
// that never write.
func NewStore(db *store.DB, clk clock.Clock, alog *audit.Log) *Store {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Store{db: db, clk: clk, audit: alog}
}

// GetTenant returns stored settings, or an empty document when never written.
func (s *Store) GetTenant(ctx context.Context, scope contracts.Scope, tenantID string) (an.TenantSettings, error) {
	if err := scope.Validate(); err != nil {
		return an.TenantSettings{}, err
	}
	if tenantID == "" {
		return an.TenantSettings{}, contracts.New(contracts.ErrValidationFailed, "analyst: tenant_id is required")
	}
	var doc string
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT document, updated_at FROM analyst_tenant_settings
		 WHERE application_id = ? AND tenant_id = ? AND workspace_id = ''`,
		scope.ApplicationID, tenantID).Scan(&doc, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return an.TenantSettings{}, nil
	case err != nil:
		return an.TenantSettings{}, wrapDB(err)
	}
	var out an.TenantSettings
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		return an.TenantSettings{}, contracts.Wrap(contracts.ErrInternal, err, "analyst: stored settings are corrupt")
	}
	ts := common.Timestamp(time.UnixMilli(at).UTC().Format(time.RFC3339Nano))
	out.UpdatedAt = &ts
	return out, nil
}

// PutTenant replaces the tenant document. The caller must already have authorised
// the write. Narrow-only checks against ceilings belong in Service.PutTenant.
func (s *Store) PutTenant(ctx context.Context, scope contracts.Scope, tenantID string, in an.TenantSettings) (an.TenantSettings, error) {
	if err := scope.Validate(); err != nil {
		return an.TenantSettings{}, err
	}
	if tenantID == "" {
		return an.TenantSettings{}, contracts.New(contracts.ErrValidationFailed, "analyst: tenant_id is required")
	}
	if s.audit == nil {
		return an.TenantSettings{}, contracts.New(contracts.ErrUnavailable, "analyst: audit log is not wired")
	}
	now := s.clk.Now().UTC()
	in.UpdatedAt = nil
	raw, err := json.Marshal(in)
	if err != nil {
		return an.TenantSettings{}, contracts.Wrap(contracts.ErrInternal, err, "analyst: cannot encode settings")
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	ts := common.Timestamp(now.Format(time.RFC3339Nano))
	out := in
	out.UpdatedAt = &ts

	loc, err := audit.ForScope(scope)
	if err != nil {
		return an.TenantSettings{}, err
	}
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO analyst_tenant_settings (application_id, tenant_id, workspace_id, document, updated_by, updated_at)
			 VALUES (?, ?, '', ?, ?, ?)
			 ON CONFLICT(application_id, tenant_id) DO UPDATE SET
			   document = excluded.document,
			   updated_by = excluded.updated_by,
			   updated_at = excluded.updated_at`,
			scope.ApplicationID, tenantID, string(raw), scope.PrincipalID, now.UnixMilli())
		if err != nil {
			return err
		}
		payload := TenantSettingsChanged{
			TenantID: tenantID, UpdatedBy: scope.PrincipalID, SHA256: digest, Enabled: in.Enabled,
		}
		_, err = eventTenantWritten.AppendTx(ctx, s.audit, tx, loc, payload)
		return err
	})
	if err != nil {
		return an.TenantSettings{}, wrapDB(err)
	}
	return out, nil
}

func wrapDB(err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "analyst: store failure")
}
