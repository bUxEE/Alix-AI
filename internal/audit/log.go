package audit

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// DefaultCheckpointValidity is how long a checkpoint signature is valid. A
// signature that expires makes the checkpoint unverifiable, so the default is
// long; see the package comment.
const DefaultCheckpointValidity = 10 * 365 * 24 * time.Hour

// Appender is what a package that records audit events depends on. *Log
// implements it; a test double that fails lets the caller prove it fails
// closed (SEC-087).
type Appender interface {
	// AppendAt appends in its own transaction. It returns only after the
	// event is committed; any error means nothing was written.
	AppendAt(ctx context.Context, loc Locator, ev NewEvent) (Event, error)
	// AppendTx appends inside tx, the caller's transaction: the event becomes
	// durable with the caller's other writes, or with none of them.
	AppendTx(ctx context.Context, tx *store.Tx, loc Locator, ev NewEvent) (Event, error)
}

// Options configures New.
type Options struct {
	// Clock is the only source of time. Nil means clock.Real.
	Clock clock.Clock
	// Registry holds the event types that may be appended. Nil means
	// DefaultRegistry.
	Registry *Registry
	// Signing is the platform key that signs checkpoints; its public half must
	// be registered under trust.PlatformApplicationID in the trust store, for
	// trust.PurposeAuditCheckpoint. Nil
	// disables Checkpoint (it returns unavailable).
	Signing *trust.SigningKey
	// Verifier verifies checkpoints against the trust store. Nil means
	// checkpoints cannot be verified: VerifyChain then reports any stored
	// checkpoint as unverifiable instead of trusting it.
	Verifier *trust.Verifier
	// CheckpointValidity is how long a checkpoint signature is valid. Zero
	// means DefaultCheckpointValidity.
	CheckpointValidity time.Duration
	// CheckpointEvery signs a checkpoint after every Nth event of a chain
	// appended with Append or AppendAt (not AppendTx: the caller's transaction
	// has not committed yet; see MaybeCheckpoint). Zero disables it. It needs
	// Signing.
	CheckpointEvery int64
	// RedactKeys are extra data keys whose values are always redacted, on top
	// of the built-in sensitive-key rule (matched like internal/log does).
	RedactKeys []string
	// MaxDataBytes bounds one event's canonical data. Zero means
	// DefaultMaxDataBytes.
	MaxDataBytes int
	// ExportAuthorizer is an additional check on top of the capability that
	// Export always requires, typically a policy decision. A non-nil error
	// refuses the export.
	ExportAuthorizer func(ctx context.Context, scope contracts.Scope) error
	// Logger receives chain, position and type of what was appended or failed
	// (never data). Nil discards.
	Logger *slog.Logger
}

// Log is the audit log over one store.DB. It is safe for concurrent use.
type Log struct {
	db       *store.DB
	clk      clock.Clock
	ids      *ids.Generator
	reg      *Registry
	red      *alog.Redactor
	signing  *trust.SigningKey
	verifier *trust.Verifier
	validity time.Duration
	every    int64
	maxData  int
	authz    func(ctx context.Context, scope contracts.Scope) error
	logger   *slog.Logger
}

var _ Appender = (*Log)(nil)

// New returns a Log on db. The "audit" migrations must be registered with the
// registry db was opened with (importing this package does that for
// store.Default).
func New(db *store.DB, opts Options) (*Log, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "audit: nil store")
	}
	l := &Log{
		db:       db,
		clk:      opts.Clock,
		reg:      opts.Registry,
		red:      alog.NewRedactor(opts.RedactKeys),
		verifier: opts.Verifier,
		validity: opts.CheckpointValidity,
		every:    opts.CheckpointEvery,
		maxData:  opts.MaxDataBytes,
		authz:    opts.ExportAuthorizer,
		logger:   opts.Logger,
	}
	if l.clk == nil {
		l.clk = clock.Real{}
	}
	l.ids = ids.NewGenerator(l.clk)
	if l.reg == nil {
		l.reg = DefaultRegistry
	}
	if l.validity < 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "audit: negative checkpoint validity")
	}
	if l.validity == 0 {
		l.validity = DefaultCheckpointValidity
	}
	if l.maxData < 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "audit: negative MaxDataBytes")
	}
	if l.maxData == 0 {
		l.maxData = DefaultMaxDataBytes
	}
	if l.every < 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "audit: negative CheckpointEvery")
	}
	if l.logger == nil {
		l.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Signing != nil {
		if opts.Signing.KeyID == "" || len(opts.Signing.Key) != ed25519.PrivateKeySize {
			return nil, contracts.New(contracts.ErrValidationFailed, "audit: the checkpoint signing key is not an Ed25519 private key with an id")
		}
		k := *opts.Signing
		l.signing = &k
	}
	if l.every > 0 && l.signing == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "audit: CheckpointEvery needs a signing key")
	}
	return l, nil
}

// Append appends an event to the tenant chain of a verified scope, in the
// scope's workspace and attributed to its principal and session. See AppendAt.
func (l *Log) Append(ctx context.Context, scope contracts.Scope, ev NewEvent) (Event, error) {
	loc, err := ForScope(scope)
	if err != nil {
		return Event{}, err
	}
	return l.AppendAt(ctx, loc, ev)
}

// AppendAt appends ev to the chain loc addresses in its own transaction and
// returns the stored event. It returns only after the event is committed, and
// any error means nothing was written: callers that need the record before a
// consequential step must treat an error as "do not proceed". An event type
// that is not registered, a chain level its type does not allow, a missing
// required key or data that cannot be canonicalised is refused with
// validation_failed or schema_invalid; a store failure is unavailable.
func (l *Log) AppendAt(ctx context.Context, loc Locator, ev NewEvent) (Event, error) {
	var out Event
	err := l.db.Tx(ctx, func(tx *store.Tx) error {
		e, err := l.AppendTx(ctx, tx, loc, ev)
		if err != nil {
			return err
		}
		out = e
		return nil
	})
	if err != nil {
		return Event{}, l.appendFailed(loc, ev.Type, err)
	}
	l.MaybeCheckpoint(ctx, out)
	return out, nil
}

// appendFailed types an error from a failed append and logs it.
func (l *Log) appendFailed(loc Locator, t EventType, err error) error {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		err = contracts.Wrap(contracts.ErrUnavailable, err, "audit: the event could not be written")
	}
	l.logger.Warn("audit append failed", "chain", loc.Chain().String(), "type", string(t), "code", string(contracts.CodeOf(err)))
	return err
}

// AppendTx appends ev inside tx, the caller's write transaction, and returns
// the stored event. It is the write-ahead primitive: the caller's own state
// change and the audit event commit together or not at all, so a failed
// append aborts the step that needed it (SEC-087). The same refusals as
// AppendAt apply; several AppendTx calls in one transaction chain in order.
// The caller's function may be re-run by store.DB.Tx on a busy database, so
// AppendTx mints a new id and reads the clock on every call.
func (l *Log) AppendTx(ctx context.Context, tx *store.Tx, loc Locator, ev NewEvent) (Event, error) {
	if tx == nil {
		return Event{}, contracts.New(contracts.ErrInternal, "audit: AppendTx needs a transaction")
	}
	level := loc.Level()
	if level == 0 {
		return Event{}, contracts.New(contracts.ErrScopeMissing, "audit: no chain to append to (a Locator comes from ForScope, ForIDs, ForApplication or Platform)")
	}
	spec, ok := l.reg.Lookup(ev.Type)
	if !ok {
		return Event{}, contracts.Newf(contracts.ErrValidationFailed, "audit: event type %q is not registered", clipName(string(ev.Type)))
	}
	if spec.levels()&level == 0 {
		return Event{}, contracts.Newf(contracts.ErrValidationFailed, "audit: event type %s may not be appended to this chain level", spec.Name)
	}
	data, err := prepareData(l.red, ev.Data, l.maxData)
	if err != nil {
		return Event{}, err
	}
	if string(data.canonical) == ErasedMarker {
		// The payload of an erased event; an event that carries it from the start
		// could never be told from one.
		return Event{}, contracts.New(contracts.ErrValidationFailed, "audit: event data is the reserved erased marker")
	}
	for _, k := range spec.Required {
		if _, ok := data.top[k]; !ok {
			return Event{}, contracts.Newf(contracts.ErrValidationFailed, "audit: event type %s requires data key %q", spec.Name, k)
		}
	}
	if spec.Validate != nil {
		if err := spec.Validate(data.top); err != nil {
			return Event{}, contracts.Wrap(contracts.ErrValidationFailed, err, "audit: event data refused by its type")
		}
	}

	key := loc.Chain()
	prevSeq, prevHash, err := chainHead(ctx, tx, key)
	if err != nil {
		return Event{}, wrapDB(err)
	}
	if prevSeq == 0 {
		prevHash = GenesisHash(key)
	}
	e := Event{
		ID:            l.ids.New(IDPrefix),
		ApplicationID: loc.application,
		TenantID:      loc.tenant,
		WorkspaceID:   loc.workspace,
		ChainSeq:      prevSeq + 1,
		Type:          ev.Type,
		OccurredAt:    l.clk.Now().UTC().Truncate(time.Millisecond),
		PrincipalID:   loc.principal,
		SessionID:     loc.session,
		Data:          data.canonical,
		PrevHash:      prevHash,
	}
	if e.Hash, err = e.ComputeHash(); err != nil {
		return Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events
		   (id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ApplicationID, e.TenantID, e.WorkspaceID, e.ChainSeq, string(e.Type), e.OccurredAt.UnixMilli(),
		e.PrincipalID, e.SessionID, string(e.Data), e.PrevHash, e.Hash); err != nil {
		return Event{}, wrapDB(err)
	}
	return e, nil
}

// chainHead returns the position and hash of the newest event of a chain, or
// (0, "") when the chain is empty. Chain-level on purpose: the chain spans the
// workspaces of its tenant, so this filters on application and tenant only.
func chainHead(ctx context.Context, q store.Reader, k ChainKey) (seq int64, hash string, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT chain_seq, hash FROM audit_events WHERE application_id = ? AND tenant_id = ? ORDER BY chain_seq DESC LIMIT 1`,
		k.ApplicationID, k.TenantID).Scan(&seq, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return seq, hash, err
}

// Chains lists every chain that has at least one event, sorted. It is for
// operators (CheckpointAll).
func (l *Log) Chains(ctx context.Context) ([]ChainKey, error) {
	return l.listChains(ctx, false)
}

// VerifiableChains lists every chain that has an event, a checkpoint or a
// tombstone, sorted. A verification sweep uses it rather than Chains: a chain
// whose events were all deleted still has its signed checkpoints, so it must be
// verified (and found truncated), never skipped.
func (l *Log) VerifiableChains(ctx context.Context) ([]ChainKey, error) {
	return l.listChains(ctx, true)
}

func (l *Log) listChains(ctx context.Context, withEvidence bool) ([]ChainKey, error) {
	var out []ChainKey
	err := l.db.View(ctx, func(tx *store.Tx) error {
		out = out[:0]
		query := `SELECT DISTINCT application_id, tenant_id FROM audit_events ORDER BY application_id, tenant_id`
		if withEvidence {
			query = `SELECT application_id, tenant_id FROM audit_events
				UNION SELECT application_id, tenant_id FROM audit_checkpoints`
			hasTombstones, err := tombstoneTableExists(ctx, tx)
			if err != nil {
				return err
			}
			if hasTombstones {
				query += ` UNION SELECT application_id, tenant_id FROM audit_tombstones`
			}
			query += ` ORDER BY 1, 2`
		}
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k ChainKey
			if err := rows.Scan(&k.ApplicationID, &k.TenantID); err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, wrapDB(err)
	}
	return out, nil
}

// wrapDB types a store failure as unavailable unless it already carries a
// code.
func wrapDB(err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "audit: the store failed")
}
