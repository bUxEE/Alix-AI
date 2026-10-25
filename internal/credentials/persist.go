package credentials

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("credentials", migrations) }

// Every statement of the module carries this predicate: a reference is bound to a tenant of an
// application, the workspace column is the empty string, and the application and tenant come from
// the verified scope. There is no statement without it.
const tenantWhere = "application_id = ? AND tenant_id = ? AND workspace_id = ''"

// row is one stored reference: the typed document the host may see and the private routing facts
// (where the value lives) that it may not.
type row struct {
	ref        providers.CredentialRef
	backend    BackendName
	locator    string
	hasRefresh bool
}

func (r row) slot() Slot {
	return Slot{Ref: string(r.ref.RefID), Locator: r.locator, Binding: bindingOf(r.ref.Binding)}
}

// bindingOf is the canonical binding string: provider, deployment, application and tenant.
func bindingOf(b providers.CredentialRefBinding) string {
	return binding(string(b.ProviderID), string(b.DeploymentID), string(b.ApplicationID), string(b.TenantID))
}

func decodeRef(doc string) (providers.CredentialRef, error) {
	var r providers.CredentialRef
	dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return providers.CredentialRef{}, fail(contracts.ErrInternal, ReasonCorrupt, "credentials: a stored reference cannot be decoded")
	}
	return r, nil
}

func wrapStore(err error, msg string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.New(contracts.ErrUnavailable, msg)
}

const rowColumns = "document, deployment_id, provider_id, kind, status, backend, locator, has_refresh"

type scanner interface{ Scan(dest ...any) error }

func scanRow(sc scanner, app, tenant string) (row, error) {
	var (
		doc, dep, prov, kind, status, backend, locator string
		hasRefresh                                     int
	)
	if err := sc.Scan(&doc, &dep, &prov, &kind, &status, &backend, &locator, &hasRefresh); err != nil {
		return row{}, err
	}
	ref, err := decodeRef(doc)
	if err != nil {
		return row{}, err
	}
	// The columns the module selects on and the document must say the same thing, or the row has been
	// edited behind the module's back: refused, never repaired.
	if string(ref.Binding.ApplicationID) != app || string(ref.Binding.TenantID) != tenant ||
		string(ref.Binding.DeploymentID) != dep || string(ref.Binding.ProviderID) != prov ||
		string(ref.Kind) != kind || string(ref.Status) != status {
		return row{}, fail(contracts.ErrInternal, ReasonCorrupt, "credentials: a stored reference does not match its row")
	}
	return row{ref: ref, backend: BackendName(backend), locator: locator, hasRefresh: hasRefresh == 1}, nil
}

// loadRow reads one reference of the tenant, or found=false.
func loadRow(ctx context.Context, q store.Reader, app, tenant, id string) (r row, found bool, err error) {
	r, err = scanRow(q.QueryRowContext(ctx,
		"SELECT "+rowColumns+" FROM credentials_refs WHERE "+tenantWhere+" AND ref_id = ?", app, tenant, id), app, tenant)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return row{}, false, nil
	case err != nil:
		return row{}, false, wrapStore(err, "credentials: reading a reference failed")
	}
	if string(r.ref.RefID) != id {
		return row{}, false, fail(contracts.ErrInternal, ReasonCorrupt, "credentials: a stored reference does not match its row")
	}
	return r, true, nil
}

// listRows reads the references of the tenant, oldest first; provider "" means every provider.
func listRows(ctx context.Context, q store.Reader, app, tenant string, provider providers.ProviderID) ([]row, error) {
	query := "SELECT " + rowColumns + " FROM credentials_refs WHERE " + tenantWhere
	args := []any{app, tenant}
	if provider != "" {
		query += " AND provider_id = ?"
		args = append(args, string(provider))
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY seq", args...)
	if err != nil {
		return nil, wrapStore(err, "credentials: listing references failed")
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		r, err := scanRow(rows, app, tenant)
		if err != nil {
			return nil, wrapStore(err, "credentials: reading a reference failed")
		}
		out = append(out, r)
	}
	return out, wrapStore(rows.Err(), "credentials: listing references failed")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func insertRow(ctx context.Context, q store.Querier, r row, now time.Time) error {
	doc, err := json.Marshal(r.ref)
	if err != nil {
		return fail(contracts.ErrInternal, ReasonCorrupt, "credentials: a reference cannot be encoded")
	}
	b := r.ref.Binding
	_, err = q.ExecContext(ctx,
		`INSERT INTO credentials_refs
		   (application_id, tenant_id, workspace_id, ref_id, deployment_id, provider_id, kind, status,
		    backend, locator, has_refresh, document, created_at, updated_at)
		 VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(b.ApplicationID), string(b.TenantID), string(r.ref.RefID), string(b.DeploymentID), string(b.ProviderID),
		string(r.ref.Kind), string(r.ref.Status), string(r.backend), r.locator, b2i(r.hasRefresh), string(doc),
		now.UnixMilli(), now.UnixMilli())
	return wrapStore(err, "credentials: storing a reference failed")
}

func updateRow(ctx context.Context, q store.Querier, r row, now time.Time) error {
	doc, err := json.Marshal(r.ref)
	if err != nil {
		return fail(contracts.ErrInternal, ReasonCorrupt, "credentials: a reference cannot be encoded")
	}
	b := r.ref.Binding
	res, err := q.ExecContext(ctx,
		`UPDATE credentials_refs SET status = ?, has_refresh = ?, document = ?, updated_at = ?
		  WHERE `+tenantWhere+` AND ref_id = ?`,
		string(r.ref.Status), b2i(r.hasRefresh), string(doc), now.UnixMilli(),
		string(b.ApplicationID), string(b.TenantID), string(r.ref.RefID))
	if err != nil {
		return wrapStore(err, "credentials: updating a reference failed")
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return deny(ReasonNotBound, "credentials: no such reference")
	}
	return nil
}

// ts renders a time as the schema's timestamp: UTC, millisecond precision.
func ts(t time.Time) common.Timestamp {
	return common.Timestamp(t.UTC().Format("2006-01-02T15:04:05.000Z"))
}

func tsPtr(t time.Time) *common.Timestamp { v := ts(t); return &v }

func parseTS(p *common.Timestamp) (time.Time, bool) {
	if p == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, string(*p))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
