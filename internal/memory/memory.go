package memory

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// Module is the name of this package's migrations.
const Module = "memory"

// IDPrefix is the typed id prefix for memory items (mem_<ULID>).
const IDPrefix = "mem"

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations(Module, migrations) }

// Capabilities the host puts in the context token. The match is exact.
const (
	CapRead    = "memory.read"
	CapWrite   = "memory.write"
	CapDelete  = "memory.delete"
	CapConfirm = "memory.confirm"
	CapExport  = "memory.export"
)

// Action names what an Authorizer is asked about.
type Action string

const (
	ActionRead    Action = "read"
	ActionWrite   Action = "write"
	ActionDelete  Action = "delete"
	ActionConfirm Action = "confirm"
	ActionExport  Action = "export"
	ActionPropose Action = "propose"
)

// Authorizer is an extra check on top of the capability. Nil means capability alone.
type Authorizer func(ctx context.Context, scope contracts.Scope, action Action) error

// Deps is everything New needs.
type Deps struct {
	DB         *store.DB
	Audit      audit.Appender
	Clock      clock.Clock
	Authorizer Authorizer
	Config     Config
	Logger     *slog.Logger
}

// Service is the long-term memory API. Safe for concurrent use.
type Service struct {
	db    *store.DB
	aud   audit.Appender
	clk   clock.Clock
	ids   *ids.Generator
	authz Authorizer
	cfg   Config
	log   *slog.Logger
}

// New validates dependencies.
func New(d Deps) (*Service, error) {
	missing := func(what string) (*Service, error) {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "memory: %s is required", what)
	}
	switch {
	case d.DB == nil:
		return missing("a store")
	case d.Audit == nil:
		return missing("an audit log")
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		db: d.DB, aud: d.Audit, clk: d.Clock, ids: ids.NewGenerator(d.Clock),
		authz: d.Authorizer, cfg: d.Config.withDefaults(), log: d.Logger,
	}, nil
}

func (s *Service) authorize(ctx context.Context, scope contracts.Scope, action Action, capability string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.Permits(capability) {
		return contracts.Newf(contracts.ErrPermissionDenied, "memory: the context does not permit %s", capability)
	}
	if s.authz == nil {
		return nil
	}
	err := s.authz(ctx, scope, action)
	switch {
	case err == nil:
		return nil
	case contracts.CodeOf(err) == contracts.ErrPermissionDenied:
		return err
	}
	return contracts.Wrap(contracts.ErrPermissionDenied, err, "memory: the operation was not authorised")
}

func wrapDB(err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "memory: the store failed")
}

func toMS(t time.Time) int64    { return t.UTC().UnixMilli() }
func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func ts(t time.Time) common.Timestamp {
	return common.Timestamp(t.UTC().Format(time.RFC3339Nano))
}

func parseTS(v *common.Timestamp) (time.Time, error) {
	if v == nil || string(*v) == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, string(*v))
	if err != nil {
		t, err = time.Parse(time.RFC3339, string(*v))
	}
	if err != nil {
		return time.Time{}, contracts.New(contracts.ErrValidationFailed, "memory: expires_at is not a timestamp")
	}
	return t.UTC(), nil
}

func notFound() error {
	return contracts.New(contracts.ErrValidationFailed, "memory not found")
}

func proposalExpired() error {
	return contracts.New(contracts.ErrValidationFailed, "proposal expired").WithDetails("reason", "memory_proposal_expired")
}

func validateText(text string) error {
	n := utf8.RuneCountInString(text)
	if n < 1 || n > MaxTextLen {
		return contracts.Newf(contracts.ErrValidationFailed, "memory: text must be between 1 and %d characters", MaxTextLen)
	}
	return nil
}

func encodeStructured(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	if len(m) > MaxStructuredKeys {
		return "", contracts.Newf(contracts.ErrValidationFailed, "memory: at most %d structured keys", MaxStructuredKeys)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", contracts.Wrap(contracts.ErrValidationFailed, err, "memory: structured facts")
	}
	return string(b), nil
}

func decodeStructured(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

type row struct {
	id, app, ten, ws, prin, text, structured, origin, status, class string
	expires                                                         sql.NullInt64
	created, updated                                                int64
}

func (r row) item() mem.MemoryItem {
	it := mem.MemoryItem{
		ID:            mem.MemoryID(r.id),
		ApplicationID: common.Identifier(r.app),
		TenantID:      common.Identifier(r.ten),
		WorkspaceID:   common.Identifier(r.ws),
		PrincipalID:   common.Identifier(r.prin),
		Text:          mem.MemoryText(r.text),
		Structured:    decodeStructured(r.structured),
		Origin:        mem.MemoryOrigin(r.origin),
		Status:        mem.MemoryStatus(r.status),
		DataClass:     mem.MemoryItemDataClass(r.class),
		CreatedAt:     ts(fromMS(r.created)),
		UpdatedAt:     ts(fromMS(r.updated)),
	}
	if r.expires.Valid {
		t := ts(fromMS(r.expires.Int64))
		it.ExpiresAt = &t
	}
	return it
}

const itemCols = `id, application_id, tenant_id, workspace_id, principal_id, text, structured_json, origin, status, data_class, expires_at, created_at, updated_at`

func scanRow(sc interface{ Scan(dest ...any) error }) (row, error) {
	var r row
	err := sc.Scan(&r.id, &r.app, &r.ten, &r.ws, &r.prin, &r.text, &r.structured, &r.origin, &r.status, &r.class, &r.expires, &r.created, &r.updated)
	return r, err
}

func (s *Service) get(ctx context.Context, scope contracts.Scope, id string) (row, error) {
	qr, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL:   "SELECT " + itemCols + " FROM memory_items",
		Where: "id = ? AND principal_id = ?", WhereArgs: []any{id, scope.PrincipalID},
	})
	if err != nil {
		return row{}, wrapDB(err)
	}
	r, err := scanRow(qr)
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, notFound()
	}
	if err != nil {
		return row{}, wrapDB(err)
	}
	return r, nil
}

func (s *Service) countPrincipal(ctx context.Context, scope contracts.Scope) (int, error) {
	qr, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: "SELECT COUNT(*) FROM memory_items", Where: "principal_id = ?", WhereArgs: []any{scope.PrincipalID},
	})
	if err != nil {
		return 0, wrapDB(err)
	}
	var n int
	if err := qr.Scan(&n); err != nil {
		return 0, wrapDB(err)
	}
	return n, nil
}

func (s *Service) audit(ctx context.Context, scope contracts.Scope, typ audit.EventType, data map[string]any) error {
	loc, err := audit.ForScope(scope)
	if err != nil {
		return err
	}
	switch typ {
	case EventCreated:
		_, err = eventCreated.Append(ctx, s.aud, loc, data)
	case EventUpdated:
		_, err = eventUpdated.Append(ctx, s.aud, loc, data)
	case EventDeleted:
		_, err = eventDeleted.Append(ctx, s.aud, loc, data)
	case EventConfirmed:
		_, err = eventConfirmed.Append(ctx, s.aud, loc, data)
	case EventRejected:
		_, err = eventRejected.Append(ctx, s.aud, loc, data)
	case EventProposed:
		_, err = eventProposed.Append(ctx, s.aud, loc, data)
	case EventExported:
		_, err = eventExported.Append(ctx, s.aud, loc, data)
	case EventErased:
		_, err = eventErased.Append(ctx, s.aud, loc, data)
	default:
		_, err = s.aud.AppendAt(ctx, loc, audit.NewEvent{Type: typ, Data: data})
	}
	return err
}

func dataClassOf(in *mem.MemoryCreateDataClass) mem.MemoryItemDataClass {
	if in == nil || *in == "" {
		return mem.MemoryItemDataClassInternal
	}
	return mem.MemoryItemDataClass(string(*in))
}

func clipQuery(q string) string {
	q = strings.TrimSpace(q)
	if utf8.RuneCountInString(q) > 256 {
		return string([]rune(q)[:256])
	}
	return q
}
