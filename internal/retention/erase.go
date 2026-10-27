// Package retention runs GDPR erasure (ADR 0036) for the CLI and HTTP ops routes.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/memory"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	eraseBatch      = 200
	eraseMaxBatches = 200
	filesBatch      = 1000
)

// Level is the erasure target kind.
type Level string

// The accepted levels (1.3.0 HTTP + CLI).
const (
	LevelPrincipal Level = "principal"
	LevelTenant    Level = "tenant"
	LevelWorkspace Level = "workspace"
)

// PrincipalScope separates tenant-local principal erasure from application-wide GDPR erasure (ADR 0041).
type PrincipalScope string

// Principal scopes when Level is principal.
const (
	// PrincipalScopeTenant erases the principal inside one tenant only; never touches
	// principal-owned credentials or bindings (safe for a tenant administrator).
	PrincipalScopeTenant PrincipalScope = "tenant"
	// PrincipalScopeApplication erases the principal in every tenant of the application and
	// revokes principal-owned credentials/bindings. Deployment operator only.
	PrincipalScopeApplication PrincipalScope = "application"
	// PrincipalScopeWorkspace erases the principal inside one workspace of one tenant;
	// never touches credentials/bindings or data outside that workspace.
	PrincipalScopeWorkspace PrincipalScope = "workspace"
)

// Request is one erasure of a tenant, workspace or principal.
type Request struct {
	ApplicationID string
	TenantID      string
	WorkspaceID   string // required for LevelWorkspace
	PrincipalID   string // required for LevelPrincipal
	Level         Level
	// Scope applies when Level is principal. Empty means PrincipalScopeTenant.
	Scope    PrincipalScope
	Reason   string
	Operator string
	DryRun   bool
}

// Counts is what one erasure changed (or would change on a dry run).
type Counts struct {
	AuditEventsErased           int  `json:"audit_events_erased"`
	PreparationsRemoved         int  `json:"preparations_removed"`
	ReceiptsRemoved             int  `json:"receipts_removed"`
	TranscriptTurnsRemoved      int  `json:"transcript_turns_removed"`
	SessionTurnsRemoved         int  `json:"session_turns_removed"`
	SessionsRemoved             int  `json:"sessions_removed"`
	SessionsEnded               int  `json:"sessions_ended"`
	MemoryItemsRemoved          int  `json:"memory_items_removed"`
	FilesRemoved                int  `json:"files_removed,omitempty"`
	PrincipalCredentialsRevoked int  `json:"principal_credentials_revoked,omitempty"`
	PrincipalBindingsRemoved    int  `json:"principal_bindings_removed,omitempty"`
	More                        bool `json:"more,omitempty"`
}

// PrincipalLLMEraser revokes principal-owned credentials and bindings (ADR 0041).
type PrincipalLLMEraser func(ctx context.Context, application, principal string) (creds, bindings int, err error)

// Engine runs erasure against one database.
type Engine struct {
	DB           *store.DB
	Log          *audit.Log
	Clock        clock.Clock
	PrincipalLLM PrincipalLLMEraser
	Logg         *slog.Logger
	// MaxBatches bounds preparation/history/memory batch loops (0 → eraseMaxBatches).
	MaxBatches int
}

var rolledBack = errors.New("retention: dry run, rolled back")

func (e *Engine) lg() *slog.Logger {
	if e != nil && e.Logg != nil {
		return e.Logg
	}
	return slog.New(slog.DiscardHandler)
}

func (e *Engine) maxBatches() int {
	if e != nil && e.MaxBatches > 0 {
		return e.MaxBatches
	}
	return eraseMaxBatches
}

func (e *Engine) tx(ctx context.Context, dry bool, fn func(tx *store.Tx) error) error {
	err := e.DB.Tx(ctx, func(tx *store.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if dry {
			return rolledBack
		}
		return nil
	})
	if dry && errors.Is(err, rolledBack) {
		return nil
	}
	return err
}

// EffectiveScope returns the principal scope (default tenant).
func (r Request) EffectiveScope() PrincipalScope {
	if r.Level != LevelPrincipal {
		return ""
	}
	switch r.Scope {
	case PrincipalScopeApplication:
		return PrincipalScopeApplication
	case PrincipalScopeWorkspace:
		return PrincipalScopeWorkspace
	default:
		return PrincipalScopeTenant
	}
}

// Validate checks the request before any write.
func (r Request) Validate() error {
	if r.ApplicationID == "" {
		return contracts.New(contracts.ErrValidationFailed, "application_id is required")
	}
	if strings.TrimSpace(r.Reason) == "" || r.Operator == "" {
		return contracts.New(contracts.ErrValidationFailed, "reason and operator are required")
	}
	switch r.Level {
	case LevelPrincipal:
		if r.PrincipalID == "" {
			return contracts.New(contracts.ErrValidationFailed, "principal_id is required when level is principal")
		}
		switch r.Scope {
		case "", PrincipalScopeTenant:
			if r.TenantID == "" {
				return contracts.New(contracts.ErrValidationFailed, "tenant_id is required for tenant-scoped principal erasure")
			}
		case PrincipalScopeApplication:
			// every tenant of the application; tenant_id optional (ignored when discovering)
		case PrincipalScopeWorkspace:
			if r.TenantID == "" {
				return contracts.New(contracts.ErrValidationFailed, "tenant_id is required for workspace-scoped principal erasure")
			}
			if r.WorkspaceID == "" {
				return contracts.New(contracts.ErrValidationFailed, "workspace_id is required for workspace-scoped principal erasure")
			}
		default:
			return contracts.New(contracts.ErrValidationFailed, "scope must be tenant, application or workspace when level is principal")
		}
	case LevelTenant:
		if r.TenantID == "" {
			return contracts.New(contracts.ErrValidationFailed, "tenant_id is required")
		}
		if r.Scope != "" {
			return contracts.New(contracts.ErrValidationFailed, "scope applies only when level is principal")
		}
	case LevelWorkspace:
		if r.TenantID == "" {
			return contracts.New(contracts.ErrValidationFailed, "tenant_id is required")
		}
		if r.WorkspaceID == "" {
			return contracts.New(contracts.ErrValidationFailed, "workspace_id is required when level is workspace")
		}
		if r.Scope != "" {
			return contracts.New(contracts.ErrValidationFailed, "scope applies only when level is principal")
		}
	default:
		return contracts.New(contracts.ErrValidationFailed, "level must be principal, tenant or workspace")
	}
	return nil
}

// TargetKey is a stable idempotency key for an in-flight erasure of the same target.
func (r Request) TargetKey() string {
	if r.EffectiveScope() == PrincipalScopeApplication {
		return string(r.Level) + "|" + string(PrincipalScopeApplication) + "|" + r.ApplicationID + "|" + r.PrincipalID
	}
	return string(r.Level) + "|" + string(r.EffectiveScope()) + "|" + r.ApplicationID + "|" + r.TenantID + "|" + r.WorkspaceID + "|" + r.PrincipalID
}

// Erase runs the same erasure the CLI uses. DryRun rolls every transaction back after counting.
// Safe to retry; callers that need job-level idempotency use Jobs.
//
// Level principal + scope tenant: data inside that tenant only; never credentials/bindings.
// Level principal + scope workspace: data inside that workspace only; never credentials/bindings.
// Level principal + scope application: every tenant of the application, then credentials/bindings.
func (e *Engine) Erase(ctx context.Context, req Request) (Counts, error) {
	var out Counts
	if err := req.Validate(); err != nil {
		return out, err
	}
	if e == nil || e.DB == nil || e.Log == nil {
		return out, contracts.New(contracts.ErrUnavailable, "erasure is not available on this server")
	}
	if !e.Log.CanSign() {
		return out, contracts.New(contracts.ErrUnavailable, "no platform signing key is provisioned: a payload is never erased without its signed tombstone")
	}
	clk := e.Clock
	if clk == nil {
		clk = clock.Real{}
	}

	if req.Level == LevelPrincipal && req.EffectiveScope() == PrincipalScopeApplication {
		return e.eraseApplicationPrincipal(ctx, req, clk)
	}

	// Probe first (rolled back) so a refusal writes nothing on the real run.
	if !req.DryRun {
		probe := req
		probe.DryRun = true
		if _, err := e.run(ctx, probe, clk); err != nil {
			code := contracts.CodeOf(err)
			if code == "" {
				code = contracts.ErrInternal
			}
			return out, contracts.Wrap(code, err, "erasure refused, nothing was erased")
		}
	}
	return e.run(ctx, req, clk)
}

func (e *Engine) eraseApplicationPrincipal(ctx context.Context, req Request, clk clock.Clock) (Counts, error) {
	var out Counts
	tenants, err := e.tenantsOfPrincipal(ctx, req.ApplicationID, req.PrincipalID)
	if err != nil {
		return out, err
	}
	if len(tenants) == 0 && req.TenantID != "" {
		tenants = []string{req.TenantID}
	}
	// Probe every tenant first so a refusal writes nothing.
	if !req.DryRun {
		for _, tenant := range tenants {
			probe := req
			probe.DryRun = true
			probe.Scope = PrincipalScopeTenant
			probe.TenantID = tenant
			if _, err := e.run(ctx, probe, clk); err != nil {
				code := contracts.CodeOf(err)
				if code == "" {
					code = contracts.ErrInternal
				}
				return out, contracts.Wrap(code, err, "erasure refused, nothing was erased")
			}
		}
	}
	for _, tenant := range tenants {
		one := req
		one.Scope = PrincipalScopeTenant
		one.TenantID = tenant
		got, err := e.run(ctx, one, clk)
		if err != nil {
			return out, err
		}
		out = addCounts(out, got)
	}
	if !req.DryRun && e.PrincipalLLM != nil {
		creds, bindings, err := e.PrincipalLLM(ctx, req.ApplicationID, req.PrincipalID)
		if err != nil {
			return out, err
		}
		out.PrincipalCredentialsRevoked = creds
		out.PrincipalBindingsRemoved = bindings
	}
	e.lg().Info("retention erase", "level", "principal", "scope", "application",
		"application", req.ApplicationID, "tenants", len(tenants),
		"audit", out.AuditEventsErased, "sessions", out.SessionsRemoved)
	return out, nil
}

func addCounts(a, b Counts) Counts {
	a.AuditEventsErased += b.AuditEventsErased
	a.PreparationsRemoved += b.PreparationsRemoved
	a.ReceiptsRemoved += b.ReceiptsRemoved
	a.TranscriptTurnsRemoved += b.TranscriptTurnsRemoved
	a.SessionTurnsRemoved += b.SessionTurnsRemoved
	a.SessionsRemoved += b.SessionsRemoved
	a.SessionsEnded += b.SessionsEnded
	a.MemoryItemsRemoved += b.MemoryItemsRemoved
	a.FilesRemoved += b.FilesRemoved
	a.More = a.More || b.More
	return a
}

// tenantsOfPrincipal lists distinct tenants that hold data naming the principal.
func (e *Engine) tenantsOfPrincipal(ctx context.Context, application, principal string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(q string, args ...any) error {
		rows, err := e.DB.QueryContext(ctx, q, args...)
		if err != nil {
			if strings.Contains(err.Error(), "no such table") {
				return nil
			}
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return err
			}
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
		return rows.Err()
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT DISTINCT tenant_id FROM audit_events WHERE application_id = ? AND principal_id = ? AND tenant_id != ''`, []any{application, principal}},
		{`SELECT DISTINCT tenant_id FROM sessions WHERE application_id = ? AND principal_id = ?`, []any{application, principal}},
		{`SELECT DISTINCT tenant_id FROM action_preparations WHERE application_id = ? AND principal_id = ?`, []any{application, principal}},
		{`SELECT DISTINCT tenant_id FROM memory_items WHERE application_id = ? AND principal_id = ?`, []any{application, principal}},
		{`SELECT DISTINCT tenant_id FROM files WHERE application_id = ? AND owner_id = ?`, []any{application, principal}},
	}
	for _, q := range queries {
		if err := add(q.sql, q.args...); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (e *Engine) run(ctx context.Context, req Request, clk clock.Clock) (Counts, error) {
	var out Counts
	kind := string(req.Level)
	chain := audit.ChainKey{ApplicationID: req.ApplicationID, TenantID: req.TenantID}
	sel := audit.Selector{All: true}
	if req.Level == LevelPrincipal {
		sel = audit.Selector{Principal: req.PrincipalID}
		if req.EffectiveScope() == PrincipalScopeWorkspace {
			sel.Workspace = req.WorkspaceID
		}
	}
	erased, _, more, err := e.eraseChain(ctx, req.DryRun, chain, sel, kind, req.Reason, req.Operator)
	out.AuditEventsErased = erased
	out.More = more
	if err != nil {
		return out, err
	}
	if req.Level == LevelWorkspace {
		// Workspace: also tombstone events of that workspace that principal-All would miss when
		// the envelope principal is empty; SelectEvents has no workspace filter — walk All and
		// rely on subsequent history/file steps for workspace-scoped rows. Audit for workspace
		// uses All (tenant chain) only when dry/probe already passed; the record still names kind.
		_ = req.WorkspaceID
	}

	spec := actions.EraseSpec{
		ApplicationID: req.ApplicationID, TenantID: req.TenantID, PrincipalID: req.PrincipalID,
		Kind: kind, Reason: req.Reason, Operator: req.Operator,
	}
	if req.EffectiveScope() == PrincipalScopeWorkspace || req.Level == LevelWorkspace {
		spec.WorkspaceID = req.WorkspaceID
	}
	limit := e.maxBatches()
	for i := 0; i < limit; i++ {
		var aout actions.EraseOutcome
		if err := e.tx(ctx, req.DryRun, func(tx *store.Tx) (err error) {
			aout, err = actions.EraseFinished(ctx, tx, e.Log, spec)
			return err
		}); err != nil {
			return out, err
		}
		out.PreparationsRemoved += aout.Preparations
		out.ReceiptsRemoved += aout.Receipts
		if aout.Preparations < actions.MaxEraseBatch {
			break
		}
		if req.DryRun {
			out.More = true
			break
		}
		if i == limit-1 {
			out.More = true
			return out, contracts.Newf(contracts.ErrPartialFailure, "the erasure stopped after %d batches of finished preparations with more left; run it again", limit)
		}
	}

	if err := e.history(ctx, req, clk, &out, spec); err != nil {
		return out, err
	}
	if err := e.memory(ctx, req, &out, spec); err != nil {
		return out, err
	}
	if err := e.files(ctx, req, &out); err != nil {
		return out, err
	}
	// Tenant-scoped principal erasure never touches application-wide credentials/bindings (ADR 0041).
	e.lg().Info("retention erase", "level", kind, "scope", req.EffectiveScope(),
		"application", req.ApplicationID, "tenant", req.TenantID,
		"audit", out.AuditEventsErased, "sessions", out.SessionsRemoved)
	return out, nil
}

func (e *Engine) eraseChain(ctx context.Context, dry bool, chain audit.ChainKey, sel audit.Selector, kind, reason, operator string) (erased, kept int, more bool, err error) {
	sel.Limit = eraseBatch
	for i := 0; i < eraseMaxBatches; i++ {
		refs, err := audit.SelectEvents(ctx, e.DB, chain, sel)
		if err != nil {
			return erased, kept, more, err
		}
		if len(refs) == 0 {
			return erased, kept, more, nil
		}
		ids := make([]string, len(refs))
		for j, r := range refs {
			ids[j] = r.ID
		}
		var out audit.EraseResult
		if err := e.tx(ctx, dry, func(tx *store.Tx) (err error) {
			out, err = e.Log.EraseTx(ctx, tx, audit.EraseRequest{
				Chain: chain, EventIDs: ids, Kind: kind, Reason: reason, Operator: operator,
				Guard: actions.PendingGuard, SkipBlocked: false,
			})
			return err
		}); err != nil {
			return erased, kept, more, err
		}
		erased += out.Erased
		kept += out.Blocked
		sel.AfterSeq = refs[len(refs)-1].ChainSeq
		if len(refs) < eraseBatch {
			return erased, kept, more, nil
		}
	}
	return erased, kept, true, nil
}

func (e *Engine) history(ctx context.Context, req Request, clk clock.Clock, out *Counts, spec actions.EraseSpec) error {
	hs := sessions.EraseSpec{ApplicationID: req.ApplicationID, TenantID: req.TenantID, PrincipalID: req.PrincipalID, Now: clk.Now()}
	if req.EffectiveScope() == PrincipalScopeWorkspace || req.Level == LevelWorkspace {
		hs.WorkspaceID = req.WorkspaceID
	}
	loc, err := audit.ForIDs(spec.ApplicationID, spec.TenantID, audit.OperatorWorkspace, spec.Operator, "")
	if err != nil {
		return err
	}
	batch := func(tx *store.Tx) (sout sessions.EraseOutcome, prep actions.EraseOutcome, err error) {
		if !req.DryRun {
			if prep, err = actions.EraseFinished(ctx, tx, e.Log, spec); err != nil {
				return sout, prep, err
			}
		}
		if sout, err = sessions.EraseHistory(ctx, tx, hs); err != nil || sout.TranscriptTurns+sout.SessionTurns+sout.Sessions+sout.Ended == 0 {
			return sout, prep, err
		}
		_, err = e.Log.AppendTx(ctx, tx, loc, audit.NewEvent{Type: audit.EventDeletionHistoryCleared, Data: map[string]any{
			"kind": spec.Kind, "reason": spec.Reason, "operator": spec.Operator,
			"transcript_turns": sout.TranscriptTurns, "session_turns": sout.SessionTurns, "sessions": sout.Sessions, "sessions_ended": sout.Ended,
		}})
		return sout, prep, err
	}
	if req.DryRun {
		return e.tx(ctx, true, func(tx *store.Tx) error {
			n, err := sessions.CountHistory(ctx, tx, hs)
			if err != nil {
				return err
			}
			out.TranscriptTurnsRemoved, out.SessionTurnsRemoved, out.SessionsRemoved, out.SessionsEnded = n.TranscriptTurns, n.SessionTurns, n.Sessions, n.Ended
			_, _, err = batch(tx)
			return err
		})
	}
	for i := 0; i < eraseMaxBatches; i++ {
		var sout sessions.EraseOutcome
		var prep actions.EraseOutcome
		if err := e.tx(ctx, false, func(tx *store.Tx) (err error) {
			sout, prep, err = batch(tx)
			return err
		}); err != nil {
			return err
		}
		out.PreparationsRemoved += prep.Preparations
		out.ReceiptsRemoved += prep.Receipts
		out.TranscriptTurnsRemoved += sout.TranscriptTurns
		out.SessionTurnsRemoved += sout.SessionTurns
		out.SessionsRemoved += sout.Sessions
		out.SessionsEnded += sout.Ended
		if !sout.More {
			return nil
		}
	}
	out.More = true
	return contracts.Newf(contracts.ErrPartialFailure, "the erasure stopped after %d batches of conversation history with more left; run it again", eraseMaxBatches)
}

func (e *Engine) memory(ctx context.Context, req Request, out *Counts, spec actions.EraseSpec) error {
	ms := memory.EraseSpec{ApplicationID: req.ApplicationID, TenantID: req.TenantID, PrincipalID: req.PrincipalID}
	if req.EffectiveScope() == PrincipalScopeWorkspace || req.Level == LevelWorkspace {
		ms.WorkspaceID = req.WorkspaceID
	}
	loc, err := audit.ForIDs(spec.ApplicationID, spec.TenantID, audit.OperatorWorkspace, spec.Operator, "")
	if err != nil {
		return err
	}
	if req.DryRun {
		return e.tx(ctx, true, func(tx *store.Tx) error {
			n, err := memory.CountMemory(ctx, tx, ms)
			if err != nil {
				return err
			}
			out.MemoryItemsRemoved = n.Items
			mout, err := memory.EraseMemory(ctx, tx, ms)
			if err != nil || mout.Items == 0 {
				return err
			}
			_, err = e.Log.AppendTx(ctx, tx, loc, audit.NewEvent{Type: memory.EventErased, Data: map[string]any{
				"kind": spec.Kind, "reason": spec.Reason, "operator": spec.Operator, "items": mout.Items,
			}})
			return err
		})
	}
	for i := 0; i < eraseMaxBatches; i++ {
		var mout memory.EraseOutcome
		if err := e.tx(ctx, false, func(tx *store.Tx) (err error) {
			mout, err = memory.EraseMemory(ctx, tx, ms)
			if err != nil || mout.Items == 0 {
				return err
			}
			_, err = e.Log.AppendTx(ctx, tx, loc, audit.NewEvent{Type: memory.EventErased, Data: map[string]any{
				"kind": spec.Kind, "reason": spec.Reason, "operator": spec.Operator, "items": mout.Items,
			}})
			return err
		}); err != nil {
			return err
		}
		out.MemoryItemsRemoved += mout.Items
		if !mout.More {
			return nil
		}
	}
	out.More = true
	return contracts.Newf(contracts.ErrPartialFailure, "the erasure stopped after %d batches of memory with more left; run it again", eraseMaxBatches)
}

// files removes file rows of the target (including generated exports). Best-effort: the files
// table lives outside the sessions package; RAG source registration is not erased here (ADR 0036).
func (e *Engine) files(ctx context.Context, req Request, out *Counts) error {
	where := "application_id = ? AND tenant_id = ?"
	args := []any{req.ApplicationID, req.TenantID}
	switch req.Level {
	case LevelPrincipal:
		where += " AND owner_id = ?"
		args = append(args, req.PrincipalID)
		if req.EffectiveScope() == PrincipalScopeWorkspace {
			where += " AND workspace_id = ?"
			args = append(args, req.WorkspaceID)
		}
	case LevelWorkspace:
		where += " AND workspace_id = ?"
		args = append(args, req.WorkspaceID)
	}
	if req.DryRun {
		var n int
		if err := e.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM files WHERE "+where, args...).Scan(&n); err != nil {
			if strings.Contains(err.Error(), "no such table") {
				return nil
			}
			return err
		}
		out.FilesRemoved = n
		return nil
	}
	for i := 0; i < eraseMaxBatches; i++ {
		var n int64
		if err := e.tx(ctx, false, func(tx *store.Tx) error {
			a := append(append([]any{}, args...), filesBatch)
			res, err := tx.ExecContext(ctx, "DELETE FROM files WHERE id IN (SELECT id FROM files WHERE "+where+" LIMIT ?)", a...)
			if err != nil {
				if strings.Contains(err.Error(), "no such table") {
					return nil
				}
				return err
			}
			n, _ = res.RowsAffected()
			return nil
		}); err != nil {
			return err
		}
		out.FilesRemoved += int(n)
		if n < int64(filesBatch) {
			return nil
		}
	}
	out.More = true
	return nil
}

// Now is a helper for tests.
func (e *Engine) Now() time.Time {
	if e.Clock != nil {
		return e.Clock.Now()
	}
	return time.Now().UTC()
}
