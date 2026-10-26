package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/memory"
	"github.com/buxee/alix-ai/internal/retention"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Receipt and audit retention, and erasure on request (master plan 19; ADR-0036, M5-26; the gap
// (d) of the M6-04 drill: the days a package declares for receipts and audit events were not
// enforced, and neither could be erased).
//
//	alixai retention run     apply what the installed packages declare, now
//	alixai retention erase   erase the receipts, audit payloads and conversation history of a tenant,
//	                         or of one principal in it
//
// # What is removed
//
// Receipts are removed with the finished preparation they belong to (actions.EraseFinished);
// the payload of an audit event is replaced by a signed tombstone (audit.Log.EraseTx). The audit
// chain keeps every hash and every envelope, so `alixai audit verify-chain` still passes and a
// checkpoint taken before still matches. An erasure also removes the conversation history
// (sessions.EraseHistory): the transcript, the sessions' recent windows and the sessions, active
// ones ended first (retentionEngine.history). All are recorded in the audit chain itself
// (deletion.receipts, audit.erased and deletion.history_cleared), in the same transaction as the
// removal.
//
// # What is never removed
//
// Anything a pending or uncertain action still depends on: its preparation and receipts
// (actions.EraseFinished) and the audit events that name it (actions.PendingGuard). A retention
// sweep skips those and reports them; an erasure request that touches one is refused whole and
// writes nothing. An erasure is also refused when the platform signing key is not provisioned:
// a payload is never erased without its signed tombstone.
//
// # Retention in `alixai serve`
//
// A maintenance job (class 6, on the RAG stack's one queue and pool) per application, queued at
// start and every retentionEvery. The days are the active package's `retention.action_receipt_days`
// and `retention.audit_days`. An application with no active package declares nothing, so nothing
// of it is removed: unknown is never read as "expired". The platform's own chain is not touched.
// A sweep does at most retentionMaxBatches batches per target and carries on at the next period.

const (
	// retentionProviderName is the name of the serve provider. Providers run in name order and this
	// one needs the RAG stack's queue and pool, which "rag" builds.
	retentionProviderName = "rag_retention"

	// retentionJobType is the job type, retentionServiceIdentity the declared service identity it
	// runs under.
	retentionJobType         = "audit.retention"
	retentionServiceIdentity = "audit.retention"
	retentionJobAttempts     = 3

	// retentionOperator is the principal id the sweep's audit events and tombstones carry. The
	// scope of a job names no real tenant: retentionServiceTenant stands in for tenant and
	// workspace, and the handler never reads either.
	retentionOperator      = "svc_audit_retention"
	retentionSession       = "ses_audit_retention"
	retentionServiceTenant = "retention"
	retentionCapability    = "audit.retention"

	// retentionBatch is how many audit events one erasure transaction tombstones (each a signature
	// and its writes on the single writer, so it is kept short enough not to stall prepare, confirm
	// and commit), and retentionMaxBatches how many batches one target (a chain, a tenant's
	// receipts) gets per sweep.
	retentionBatch      = 200
	retentionMaxBatches = 20

	retentionRunUsage   = "alixai retention run [-config FILE] [-application ID] [-dry-run]"
	retentionEraseUsage = "alixai retention erase -application ID -tenant ID -reason TEXT -operator ID [-principal ID] [-tenant-scoped] [-dry-run] [-config FILE]"
)

// Test seams, replaced only by tests.
var (
	// retentionEvery is how often serve queues the jobs.
	retentionEvery = 24 * time.Hour
	// retentionOnCompose sees the job serve builds.
	retentionOnCompose func(*retentionStack)
	// retentionSourceOf is where serve reads the days a package declares.
	retentionSourceOf = retentionInstalledPackages
	// retentionEraseMaxBatches is how many batches of finished preparations one
	// `retention erase` run removes.
	retentionEraseMaxBatches = retentionMaxBatches * 10
)

func init() {
	registerServeProvider(retentionProviderName, retentionProvide)
	Register(Command{
		Name:    "retention run",
		Summary: "apply the receipt and audit retention the installed packages declare: expired receipts are removed, expired audit payloads become signed tombstones",
		Run:     retentionRunRun,
	})
	Register(Command{
		Name:    "retention erase",
		Summary: "erase the receipts, audit payloads and conversation history of a tenant, or of one principal (application-wide GDPR with -principal; add -tenant-scoped for that tenant only)",
		Run:     retentionRunErase,
	})
}

// ---- the days a package declares -------------------------------------------------------

// retentionDays is what an application's active package declares, in days.
type retentionDays struct{ Receipts, Audit int64 }

// retentionSource says what an application's active package declares. ok is false when there is
// no active package, which means nothing is known and nothing is removed.
type retentionSource interface {
	Retention(ctx context.Context, applicationID string) (days retentionDays, ok bool, err error)
}

// retentionPackages reads the manifest of the active installed package.
type retentionPackages struct{ in *apppkg.Installer }

// Retention implements retentionSource. The package is opened with its signature and hashes
// checked, as everywhere else: a package that no longer verifies declares nothing.
func (p retentionPackages) Retention(ctx context.Context, applicationID string) (retentionDays, bool, error) {
	recs, err := p.in.List(ctx, applicationID)
	if err != nil {
		return retentionDays{}, false, err
	}
	hash := ""
	for _, r := range recs {
		if r.State == apppkg.StateActive {
			hash = r.ManifestHash
		}
	}
	if hash == "" {
		return retentionDays{}, false, nil
	}
	pkg, _, err := p.in.Open(ctx, applicationID, hash)
	if err != nil {
		return retentionDays{}, false, err
	}
	r := pkg.Manifest.Retention
	return retentionDays{Receipts: r.ActionReceiptDays, Audit: r.AuditDays}, true, nil
}

func retentionInstalledPackages(d serveDeps) (retentionSource, error) {
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	in, err := apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: d.DB, Clock: clk, DataDir: d.DataDir,
		Verifier: trust.NewVerifier(trust.NewStore(d.DB, clk), clk), CoreVersion: apppkg.CoreVersionOrDev(version.Get().Version),
	})
	if err != nil {
		return nil, err
	}
	return retentionPackages{in: in}, nil
}

// ---- the sweep -------------------------------------------------------------------------

// retentionEngine applies retention to the applications of one database.
type retentionEngine struct {
	db  *store.DB
	log *audit.Log
	clk clock.Clock
	src retentionSource
	lg  *slog.Logger
	// dry runs every transaction and rolls it back: the counts are exact for the first batch of
	// each target and nothing is written.
	dry bool
}

// retentionResult is what a sweep of one application did.
type retentionResult struct {
	ApplicationID string `json:"application_id"`
	// Skipped says why nothing was removed for the application.
	Skipped             string `json:"skipped,omitempty"`
	ReceiptDays         int64  `json:"action_receipt_days,omitempty"`
	AuditDays           int64  `json:"audit_days,omitempty"`
	PreparationsRemoved int    `json:"preparations_removed"`
	ReceiptsRemoved     int    `json:"receipts_removed"`
	// PendingKept counts matching preparations kept because an action still depends on them;
	// AuditKept the events kept for the same reason.
	PendingKept int `json:"pending_kept"`
	AuditErased int `json:"audit_events_erased"`
	AuditKept   int `json:"audit_events_kept"`
	// AuditSkipped says why audit payloads were not touched.
	AuditSkipped string `json:"audit_skipped,omitempty"`
	// More is set when a batch limit stopped the sweep: the next one carries on.
	More bool `json:"more,omitempty"`
}

var retentionRolledBack = errors.New("retention: dry run, rolled back")

// tx runs fn in a write transaction, or, for a dry run, in one that is rolled back.
func (e *retentionEngine) tx(ctx context.Context, fn func(tx *store.Tx) error) error {
	err := e.db.Tx(ctx, func(tx *store.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if e.dry {
			return retentionRolledBack
		}
		return nil
	})
	if errors.Is(err, retentionRolledBack) {
		return nil
	}
	return err
}

func retentionCutoff(now time.Time, days int64) time.Time {
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

// applications lists the applications that hold receipts or audit events, but not the platform's.
func (e *retentionEngine) applications(ctx context.Context) ([]string, error) {
	rows, err := e.db.QueryContext(ctx,
		`SELECT application_id FROM action_preparations UNION SELECT application_id FROM audit_events ORDER BY 1`)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "retention: the applications cannot be listed")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, contracts.Wrap(contracts.ErrUnavailable, err, "retention: the applications cannot be listed")
		}
		if a != trust.PlatformApplicationID {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// retentionStrings reads one column of text from a query.
func retentionStrings(ctx context.Context, db *store.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "retention: the targets cannot be listed")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, contracts.Wrap(contracts.ErrUnavailable, err, "retention: the targets cannot be listed")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// pass sweeps every application, or only `only` when it is set. A failed application does not
// stop the others; the first error is returned after all were tried.
func (e *retentionEngine) pass(ctx context.Context, only string) ([]retentionResult, error) {
	apps := []string{only}
	if only == "" {
		var err error
		if apps, err = e.applications(ctx); err != nil {
			return nil, err
		}
	}
	out := []retentionResult{}
	var first error
	for _, app := range apps {
		res, err := e.application(ctx, app)
		out = append(out, res)
		if err != nil && first == nil {
			first = fmt.Errorf("%s: %w", app, err)
		}
	}
	return out, first
}

// application applies the retention the application's active package declares.
func (e *retentionEngine) application(ctx context.Context, app string) (retentionResult, error) {
	res := retentionResult{ApplicationID: app}
	if app == "" || app == trust.PlatformApplicationID {
		res.Skipped = "the platform's own chain has no package to declare a retention"
		return res, nil
	}
	days, ok, err := e.src.Retention(ctx, app)
	switch {
	case err != nil:
		res.Skipped = "the declared retention cannot be read: " + string(contracts.CodeOf(err))
		return res, err
	case !ok:
		res.Skipped = "no active package declares a retention: nothing is removed"
		return res, nil
	case days.Receipts < 1 || days.Audit < 1:
		// The schema's minimum is 1: anything else is a package this build does not understand.
		res.Skipped = "the declared retention is below the schema's minimum of 1 day: nothing is removed"
		return res, nil
	}
	res.ReceiptDays, res.AuditDays = days.Receipts, days.Audit
	now := e.clk.Now()
	if err := e.receipts(ctx, app, retentionCutoff(now, days.Receipts), days.Receipts, &res); err != nil {
		return res, err
	}
	return res, e.auditPayloads(ctx, app, retentionCutoff(now, days.Audit), days.Audit, &res)
}

// receipts removes the finished preparations, with their receipts, that are past the cutoff, for
// every tenant of the application.
func (e *retentionEngine) receipts(ctx context.Context, app string, cutoff time.Time, days int64, res *retentionResult) error {
	tenants, err := retentionStrings(ctx, e.db,
		`SELECT DISTINCT tenant_id FROM action_preparations WHERE application_id = ? AND updated_at < ? ORDER BY 1`, app, cutoff.UnixMilli())
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		spec := actions.EraseSpec{
			ApplicationID: app, TenantID: tenant, Before: cutoff, SkipPending: true,
			Kind: "retention", Reason: fmt.Sprintf("action receipt retention of %d days", days), Operator: retentionOperator,
		}
		for i := 0; i < retentionMaxBatches; i++ {
			var out actions.EraseOutcome
			if err := e.tx(ctx, func(tx *store.Tx) (err error) {
				out, err = actions.EraseFinished(ctx, tx, e.log, spec)
				return err
			}); err != nil {
				return err
			}
			res.PreparationsRemoved += out.Preparations
			res.ReceiptsRemoved += out.Receipts
			if i == 0 {
				res.PendingKept += out.Pending
			}
			if out.Preparations < actions.MaxEraseBatch {
				break
			}
			if e.dry || i == retentionMaxBatches-1 {
				res.More = true
				break
			}
		}
	}
	return nil
}

// auditPayloads replaces the payload of every audit event older than the cutoff by a signed
// tombstone, on every chain of the application, except the events a pending action depends on.
func (e *retentionEngine) auditPayloads(ctx context.Context, app string, cutoff time.Time, days int64, res *retentionResult) error {
	if !e.log.CanSign() {
		res.AuditSkipped = "no platform signing key is provisioned (run `alixai keys init`): audit payloads are not erased without a signed tombstone"
		return nil
	}
	tenants, err := retentionStrings(ctx, e.db,
		`SELECT DISTINCT tenant_id FROM audit_events WHERE application_id = ? AND occurred_at < ? ORDER BY 1`, app, cutoff.UnixMilli())
	if err != nil {
		return err
	}
	for _, tenant := range tenants {
		chain := audit.ChainKey{ApplicationID: app, TenantID: tenant}
		erased, kept, more, err := e.eraseChain(ctx, chain, audit.Selector{Before: cutoff}, "retention",
			fmt.Sprintf("audit retention of %d days", days), retentionOperator, true, retentionMaxBatches)
		res.AuditErased += erased
		res.AuditKept += kept
		res.More = res.More || more
		if err != nil {
			return err
		}
	}
	return nil
}

// eraseChain tombstones the events of one chain that sel picks, in batches of retentionBatch, with
// the pending-action guard. With skipBlocked the events a pending action depends on stay and are
// counted; without it one such event refuses the batch. It stops after retentionMaxBatches (more
// is then true); 0 means no limit. A dry run goes through every batch too, rolled back, with the
// cursor moving past each, so a refusal anywhere in the request is found before anything is written.
func (e *retentionEngine) eraseChain(ctx context.Context, chain audit.ChainKey, sel audit.Selector, kind, reason, operator string, skipBlocked bool, maxBatches int) (erased, kept int, more bool, err error) {
	sel.Limit = retentionBatch
	for i := 0; maxBatches == 0 || i < maxBatches; i++ {
		refs, err := audit.SelectEvents(ctx, e.db, chain, sel)
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
		if err := e.tx(ctx, func(tx *store.Tx) (err error) {
			out, err = e.log.EraseTx(ctx, tx, audit.EraseRequest{
				Chain: chain, EventIDs: ids, Kind: kind, Reason: reason, Operator: operator,
				Guard: actions.PendingGuard, SkipBlocked: skipBlocked,
			})
			return err
		}); err != nil {
			return erased, kept, more, err
		}
		erased += out.Erased
		kept += out.Blocked
		// The cursor moves past what was looked at, so events that stay (blocked) are not read again.
		sel.AfterSeq = refs[len(refs)-1].ChainSeq
		if len(refs) < retentionBatch {
			return erased, kept, more, nil
		}
	}
	return erased, kept, true, nil
}

// ---- operator commands -----------------------------------------------------------------

// retentionOpen opens the store the way serve does and builds the engine over it: the platform
// signing key, when there is one, is loaded and must be the one registered in the trust store.
func retentionOpen(ctx context.Context, env *Env, cfgPath string) (*retentionEngine, func(), error) {
	db, core, err := operatorStore(ctx, env, cfgPath)
	if err != nil {
		return nil, nil, err
	}
	clk := clock.Real{}
	opts := audit.Options{Clock: clk, Verifier: trust.NewVerifier(trust.NewStore(db, clk), clk)}
	key, err := loadCheckpointKey(keysSection, core.DataDir.String())
	if err == nil && key != nil {
		err = checkCheckpointKeyRegistered(ctx, db, clk, key)
		signing := key.signing
		opts.Signing = &signing
	}
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	lg, err := audit.New(db, opts)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	in, err := retentionInstalledPackages(serveDeps{DB: db, Clock: clk, DataDir: core.DataDir.String()})
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return &retentionEngine{db: db, log: lg, clk: clk, src: in, lg: slog.New(slog.DiscardHandler)}, func() { _ = db.Close() }, nil
}

// retentionRunRun is `retention run`: one sweep over every application (or one), now.
func retentionRunRun(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("retention run")
	cfgPath := env.ConfigFlag(fs)
	app := fs.String("application", "", "sweep only this application (default: every application that holds receipts or audit events)")
	dry := fs.Bool("dry-run", false, "count what a sweep would remove and write nothing (receipts are counted one batch per tenant)")
	if err := env.ParseFlags(fs, retentionRunUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), retentionRunUsage)
	}
	eng, closeDB, err := retentionOpen(ctx, env, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	eng.dry = *dry
	results, perr := eng.pass(ctx, *app)
	if err := backupEmit(env.Stdout, map[string]any{"dry_run": *dry, "applications": results}); err != nil {
		return err
	}
	return perr
}

// retentionEraseReport is what `retention erase` prints.
type retentionEraseReport struct {
	ApplicationID       string `json:"application_id"`
	TenantID            string `json:"tenant_id"`
	PrincipalID         string `json:"principal_id,omitempty"`
	DryRun              bool   `json:"dry_run"`
	AuditEventsErased   int    `json:"audit_events_erased"`
	PreparationsRemoved int    `json:"preparations_removed"`
	ReceiptsRemoved     int    `json:"receipts_removed"`
	// The conversation history (sessions.EraseHistory): transcript rows, rows of the sessions'
	// recent windows and session rows removed, and how many of those sessions were active and were
	// ended first.
	TranscriptTurnsRemoved int `json:"transcript_turns_removed"`
	SessionTurnsRemoved    int `json:"session_turns_removed"`
	SessionsRemoved        int `json:"sessions_removed"`
	SessionsEnded          int `json:"sessions_ended"`
	// MemoryItemsRemoved is long-term memory erased with the principal/tenant (ADR 0042).
	MemoryItemsRemoved int `json:"memory_items_removed"`
	// PrincipalCredentialsRevoked / PrincipalBindingsRemoved are per-principal LLM account
	// erasure (ADR 0041); only set for application-wide principal erasure (`-principal` without `-tenant-scoped`).
	PrincipalCredentialsRevoked int `json:"principal_credentials_revoked,omitempty"`
	PrincipalBindingsRemoved    int `json:"principal_bindings_removed,omitempty"`
	// Scope is "application" or "tenant" when a principal was named.
	Scope string `json:"scope,omitempty"`
	// More is set on a dry run that saw only the first batch of the receipts, and on a run that
	// stopped at its batch limit with receipts or history left (it then fails: run it again).
	More bool `json:"more,omitempty"`
}

// retentionRunErase is `retention erase`: the receipts, audit payloads and conversation history of a
// tenant, or of one principal in it, are erased. Everything that could refuse is checked first, in transactions that
// are rolled back (the same code, so the same refusals), so a refused request changes nothing; then
// the real run goes through.
func retentionRunErase(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("retention erase")
	cfgPath := env.ConfigFlag(fs)
	app := fs.String("application", "", "the application (required)")
	tenant := fs.String("tenant", "", "the tenant whose data is erased (required); with -principal without -tenant-scoped, every tenant of the application is erased and this names the report")
	principal := fs.String("principal", "", "GDPR erase this principal across every tenant of the application (and revoke their LLM credentials); add -tenant-scoped to limit to -tenant only")
	tenantScoped := fs.Bool("tenant-scoped", false, "with -principal: erase only that principal's data inside -tenant; never revoke application-wide credentials or bindings")
	reason := fs.String("reason", "", "why, in a few words and with no personal data; it is signed into every tombstone (required)")
	operator := fs.String("operator", "", "who is erasing: your operator id; it is signed into every tombstone (required)")
	dry := fs.Bool("dry-run", false, "check the request and count what it would erase; write nothing")
	if err := env.ParseFlags(fs, retentionEraseUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *app == "" || *tenant == "" || strings.TrimSpace(*reason) == "" || *operator == "" {
		return Usagef("-application, -tenant, -reason and -operator are required (usage: %s)", retentionEraseUsage)
	}
	if *tenantScoped && *principal == "" {
		return Usagef("-tenant-scoped requires -principal (usage: %s)", retentionEraseUsage)
	}
	eng, closeDB, err := retentionOpen(ctx, env, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	if !eng.log.CanSign() {
		return errors.New("retention erase: no platform signing key is provisioned (run `alixai keys init`): a payload is never erased without its signed tombstone, so nothing was erased")
	}
	level := retention.LevelTenant
	var pScope retention.PrincipalScope
	if *principal != "" {
		level = retention.LevelPrincipal
		if *tenantScoped {
			pScope = retention.PrincipalScopeTenant
		} else {
			pScope = retention.PrincipalScopeApplication
		}
	}
	shared := &retention.Engine{
		DB: eng.db, Log: eng.log, Clock: eng.clk, Logg: eng.lg, MaxBatches: retentionEraseMaxBatches,
		PrincipalLLM: func(ctx context.Context, application, principal string) (int, int, error) {
			return retentionErasePrincipalLLM(ctx, eng, application, principal)
		},
	}
	req := retention.Request{
		ApplicationID: *app, TenantID: *tenant, PrincipalID: *principal,
		Level: level, Scope: pScope, Reason: *reason, Operator: *operator, DryRun: *dry,
	}
	counts, err := shared.Erase(ctx, req)
	rep := retentionEraseReport{
		ApplicationID: *app, TenantID: *tenant, PrincipalID: *principal, DryRun: *dry,
		Scope:             string(req.EffectiveScope()),
		AuditEventsErased: counts.AuditEventsErased, PreparationsRemoved: counts.PreparationsRemoved,
		ReceiptsRemoved: counts.ReceiptsRemoved, TranscriptTurnsRemoved: counts.TranscriptTurnsRemoved,
		SessionTurnsRemoved: counts.SessionTurnsRemoved, SessionsRemoved: counts.SessionsRemoved,
		SessionsEnded: counts.SessionsEnded, MemoryItemsRemoved: counts.MemoryItemsRemoved,
		PrincipalCredentialsRevoked: counts.PrincipalCredentialsRevoked,
		PrincipalBindingsRemoved:    counts.PrincipalBindingsRemoved, More: counts.More,
	}
	if err != nil {
		if rep.More && !*dry {
			_ = backupEmit(env.Stdout, rep)
			return fmt.Errorf("retention erase: %w", err)
		}
		wrote := counts.AuditEventsErased+counts.PreparationsRemoved+counts.SessionsRemoved+counts.MemoryItemsRemoved > 0
		if !*dry && wrote {
			return fmt.Errorf("retention erase: %w", err)
		}
		return fmt.Errorf("retention erase refused, nothing was erased: %w", err)
	}
	return backupEmit(env.Stdout, rep)
}

// erase is one pass of the erasure of a tenant or principal: audit payloads first (so the records of
// the removals stay readable), then the receipts, then the conversation history.
func (e *retentionEngine) erase(ctx context.Context, rep *retentionEraseReport, reason, operator string) error {
	chain := audit.ChainKey{ApplicationID: rep.ApplicationID, TenantID: rep.TenantID}
	sel, kind := audit.Selector{All: true}, "tenant"
	if rep.PrincipalID != "" {
		sel, kind = audit.Selector{Principal: rep.PrincipalID}, "principal"
	}
	erased, _, _, err := e.eraseChain(ctx, chain, sel, kind, reason, operator, false, 0)
	rep.AuditEventsErased = erased
	if err != nil {
		return err
	}
	spec := actions.EraseSpec{
		ApplicationID: rep.ApplicationID, TenantID: rep.TenantID, PrincipalID: rep.PrincipalID,
		Kind: kind, Reason: reason, Operator: operator,
	}
	for i := 0; i < retentionEraseMaxBatches; i++ {
		var out actions.EraseOutcome
		if err := e.tx(ctx, func(tx *store.Tx) (err error) {
			out, err = actions.EraseFinished(ctx, tx, e.log, spec)
			return err
		}); err != nil {
			return err
		}
		rep.PreparationsRemoved += out.Preparations
		rep.ReceiptsRemoved += out.Receipts
		if out.Preparations < actions.MaxEraseBatch {
			return e.history(ctx, rep, spec)
		}
		if e.dry {
			rep.More = true
			return e.history(ctx, rep, spec)
		}
	}
	// The last batch was full: there may be more than one run removes.
	rep.More = true
	return contracts.Newf(contracts.ErrPartialFailure, "the erasure stopped after %d batches of finished preparations with more left; run it again to erase the rest", retentionEraseMaxBatches)
}

// history erases the conversation history of the tenant or principal (sessions.EraseHistory): the
// transcript, so history search finds none of it, the sessions' recent windows, and the sessions.
//
// Active sessions are ended and erased with the ended ones, not refused: a session nobody ends stays
// active for good, and an erasure that waited for it would never run. They are ended the way
// sessions.Store.End leaves a session, without voiding a preparation, because every batch runs the
// receipts' step (actions.EraseFinished, same spec) again in its own transaction: it refuses while a
// pending or uncertain action matches, so none can be confirmable in a session that is ended, and none
// can be prepared between that check and the end.
//
// A batch is one transaction of at most 1000 rows, recorded in it on the tenant's chain
// (deletion.history_cleared: kind, reason, operator and counts, never whose). A dry run counts with the
// conditions the batches delete by, and runs one batch, rolled back.
func (e *retentionEngine) history(ctx context.Context, rep *retentionEraseReport, spec actions.EraseSpec) error {
	hs := sessions.EraseSpec{ApplicationID: spec.ApplicationID, TenantID: spec.TenantID, PrincipalID: spec.PrincipalID, Now: e.clk.Now()}
	loc, err := audit.ForIDs(spec.ApplicationID, spec.TenantID, audit.OperatorWorkspace, spec.Operator, "")
	if err != nil {
		return err
	}
	batch := func(tx *store.Tx) (out sessions.EraseOutcome, prep actions.EraseOutcome, err error) {
		// A dry run has just checked the receipts' step of this pass, in a transaction rolled back.
		if !e.dry {
			if prep, err = actions.EraseFinished(ctx, tx, e.log, spec); err != nil {
				return out, prep, err
			}
		}
		if out, err = sessions.EraseHistory(ctx, tx, hs); err != nil || out.TranscriptTurns+out.SessionTurns+out.Sessions+out.Ended == 0 {
			return out, prep, err
		}
		_, err = e.log.AppendTx(ctx, tx, loc, audit.NewEvent{Type: audit.EventDeletionHistoryCleared, Data: map[string]any{
			"kind": spec.Kind, "reason": spec.Reason, "operator": spec.Operator,
			"transcript_turns": out.TranscriptTurns, "session_turns": out.SessionTurns, "sessions": out.Sessions, "sessions_ended": out.Ended,
		}})
		return out, prep, err
	}
	if e.dry {
		if err := e.tx(ctx, func(tx *store.Tx) error {
			n, err := sessions.CountHistory(ctx, tx, hs)
			if err != nil {
				return err
			}
			rep.TranscriptTurnsRemoved, rep.SessionTurnsRemoved, rep.SessionsRemoved, rep.SessionsEnded = n.TranscriptTurns, n.SessionTurns, n.Sessions, n.Ended
			_, _, err = batch(tx)
			return err
		}); err != nil {
			return err
		}
		return e.memory(ctx, rep, spec)
	}
	for i := 0; i < retentionEraseMaxBatches; i++ {
		var out sessions.EraseOutcome
		var prep actions.EraseOutcome
		if err := e.tx(ctx, func(tx *store.Tx) (err error) {
			out, prep, err = batch(tx)
			return err
		}); err != nil {
			return err
		}
		rep.PreparationsRemoved += prep.Preparations
		rep.ReceiptsRemoved += prep.Receipts
		rep.TranscriptTurnsRemoved += out.TranscriptTurns
		rep.SessionTurnsRemoved += out.SessionTurns
		rep.SessionsRemoved += out.Sessions
		rep.SessionsEnded += out.Ended
		if !out.More {
			return e.memory(ctx, rep, spec)
		}
	}
	rep.More = true
	return contracts.Newf(contracts.ErrPartialFailure, "the erasure stopped after %d batches of conversation history with more left; run it again to erase the rest", retentionEraseMaxBatches)
}

// memory erases long-term memory items of the tenant or principal (ADR 0042).
func (e *retentionEngine) memory(ctx context.Context, rep *retentionEraseReport, spec actions.EraseSpec) error {
	ms := memory.EraseSpec{ApplicationID: spec.ApplicationID, TenantID: spec.TenantID, PrincipalID: spec.PrincipalID}
	loc, err := audit.ForIDs(spec.ApplicationID, spec.TenantID, audit.OperatorWorkspace, spec.Operator, "")
	if err != nil {
		return err
	}
	if e.dry {
		if err := e.tx(ctx, func(tx *store.Tx) error {
			n, err := memory.CountMemory(ctx, tx, ms)
			if err != nil {
				return err
			}
			rep.MemoryItemsRemoved = n.Items
			out, err := memory.EraseMemory(ctx, tx, ms)
			if err != nil {
				return err
			}
			if out.Items == 0 {
				return nil
			}
			_, err = e.log.AppendTx(ctx, tx, loc, audit.NewEvent{Type: memory.EventErased, Data: map[string]any{
				"kind": spec.Kind, "reason": spec.Reason, "operator": spec.Operator, "items": out.Items,
			}})
			return err
		}); err != nil {
			return err
		}
		return e.principalLLM(ctx, rep)
	}
	for i := 0; i < retentionEraseMaxBatches; i++ {
		var out memory.EraseOutcome
		if err := e.tx(ctx, func(tx *store.Tx) (err error) {
			out, err = memory.EraseMemory(ctx, tx, ms)
			if err != nil || out.Items == 0 {
				return err
			}
			_, err = e.log.AppendTx(ctx, tx, loc, audit.NewEvent{Type: memory.EventErased, Data: map[string]any{
				"kind": spec.Kind, "reason": spec.Reason, "operator": spec.Operator, "items": out.Items,
			}})
			return err
		}); err != nil {
			return err
		}
		rep.MemoryItemsRemoved += out.Items
		if !out.More {
			return e.principalLLM(ctx, rep)
		}
	}
	rep.More = true
	return contracts.Newf(contracts.ErrPartialFailure, "the erasure stopped after %d batches of memory with more left; run it again to erase the rest", retentionEraseMaxBatches)
}

// principalLLM revokes principal-owned credentials and deletes LLM bindings (ADR 0041).
// Skipped on dry-run: those stores sit outside the retention DB transaction.
func (e *retentionEngine) principalLLM(ctx context.Context, rep *retentionEraseReport) error {
	if rep.PrincipalID == "" || e.dry {
		return nil
	}
	creds, bindings, err := retentionErasePrincipalLLM(ctx, e, rep.ApplicationID, rep.PrincipalID)
	if err != nil {
		return err
	}
	rep.PrincipalCredentialsRevoked = creds
	rep.PrincipalBindingsRemoved = bindings
	return nil
}

// ---- the job in `alixai serve` ---------------------------------------------------------

// retentionStack is the retention job of the process.
type retentionStack struct {
	engine     *retentionEngine
	queue      *jobs.Queue
	every      time.Duration
	deployment string
}

// retentionPayload is the payload of the job, and all of it.
type retentionPayload struct {
	ApplicationID string `json:"application_id"`
}

func retentionDecode(raw json.RawMessage) (retentionPayload, error) {
	var p retentionPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || p.ApplicationID == "" || dec.More() {
		return p, contracts.New(contracts.ErrPermissionDenied, "the retention job names no application")
	}
	return p, nil
}

// retentionServiceScope is the scope of the job of one application: the real deployment and
// application, the service principal, and placeholders for tenant and workspace that nothing reads.
func retentionServiceScope(deployment, application string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: deployment, ApplicationID: application, TenantID: retentionServiceTenant, WorkspaceID: retentionServiceTenant,
		PrincipalID: retentionOperator, Roles: []string{"service"}, CapabilityScopes: []string{retentionCapability}, SessionID: retentionSession,
	}
}

// retentionProvide puts the retention job on the RAG stack's queue and pool and starts the loop that
// queues it. Without a deployment id there is no valid scope for a job, which is said, never silent.
func retentionProvide(_ context.Context, d serveDeps) error {
	if d.DB == nil || d.Audit == nil {
		return nil
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	rs, ok := gateway.Lookup[*ragStack](d.Services)
	if !ok {
		// Said, never silent; serve still starts (data is only ever kept longer, never removed early).
		d.Log.Warn("retention: the retention job is not scheduled, so receipts and audit events are not removed by the days a package declares: serve has no job queue")
		return nil
	}
	dep := identity.CurrentConfig().DeploymentID
	if dep == "" {
		d.Log.Warn("retention: the retention job is not scheduled, so receipts and audit events are not removed by the days a package declares: identity.deployment_id is not set")
		return nil
	}
	src, err := retentionSourceOf(d)
	if err != nil {
		return err
	}
	if !d.Audit.CanSign() {
		d.Log.Warn("retention: audit payloads are not erased by retention until the platform signing key is provisioned (run `alixai keys init`); receipts still are")
	}
	r := &retentionStack{
		engine:     &retentionEngine{db: d.DB, log: d.Audit, clk: d.Clock, src: src, lg: d.Log},
		queue:      rs.queue,
		every:      retentionEvery,
		deployment: dep,
	}
	if err := rs.queue.Register(jobs.TypeSpec{Name: retentionJobType, Class: jobs.ClassMaintenance, MaxAttempts: retentionJobAttempts}); err != nil {
		return err
	}
	if err := rs.shareJobs(retentionServiceIdentity, retentionAuthority{}, func(p *jobs.Pool) error {
		return p.Handle(retentionJobType, r.handle)
	}); err != nil {
		return err
	}
	if d.AddService != nil {
		d.AddService(r.start)
	}
	if retentionOnCompose != nil {
		retentionOnCompose(r)
	}
	return nil
}

// start is the service of the process (serveDeps.AddService): it queues the jobs at once and after
// every period until ctx ends. The function it returns stops the loop and waits for it.
func (r *retentionStack) start(ctx context.Context) (stop func()) {
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(r.every)
		defer tick.Stop()
		for {
			if n, err := r.enqueue(rctx); err != nil && rctx.Err() == nil {
				r.engine.lg.Warn("retention: queueing the retention jobs failed; the next pass goes on",
					slog.String("code", string(contracts.CodeOf(err))), slog.String("err_text", err.Error()))
			} else if n > 0 {
				r.engine.lg.Info("retention: jobs queued", slog.Int("applications", n))
			}
			select {
			case <-rctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// enqueue queues one job per application that holds receipts or audit events, keyed by the period,
// so a restart inside a period queues nothing a second time. It returns how many were queued.
func (r *retentionStack) enqueue(ctx context.Context) (int, error) {
	apps, err := r.engine.applications(ctx)
	if err != nil {
		return 0, err
	}
	period := r.engine.clk.Now().Truncate(r.every).UnixMilli()
	queued := 0
	for _, app := range apps {
		payload, _ := json.Marshal(retentionPayload{ApplicationID: app})
		out, err := r.queue.Enqueue(ctx, retentionServiceScope(r.deployment, app), jobs.EnqueueRequest{
			Type: retentionJobType, Payload: payload, ServiceIdentity: retentionServiceIdentity,
			IdempotencyKey: fmt.Sprintf("retention:%s:%d", app, period),
		})
		switch {
		case err != nil:
			// A full class (resource_exhausted) stops the pass; a later one queues the rest.
			return queued, err
		case !out.Duplicate:
			queued++
		}
	}
	return queued, nil
}

// handle is the handler of the job: apply the retention of the application the payload names, and
// nothing else.
func (r *retentionStack) handle(ctx context.Context, job jobs.Job) error {
	if job.ServiceIdentity != retentionServiceIdentity {
		return jobs.Permanent(contracts.New(contracts.ErrPermissionDenied, "the retention job needs its service identity"))
	}
	p, err := retentionDecode(job.Payload)
	if err != nil {
		return jobs.Permanent(err)
	}
	res, err := r.engine.application(ctx, p.ApplicationID)
	if err != nil {
		return err
	}
	if res.Skipped != "" {
		r.engine.lg.Info("retention: nothing removed", slog.String("application_id", p.ApplicationID), slog.String("why", res.Skipped))
		return nil
	}
	r.engine.lg.Info("retention applied", slog.String("application_id", p.ApplicationID),
		slog.Int("preparations_removed", res.PreparationsRemoved), slog.Int("receipts_removed", res.ReceiptsRemoved),
		slog.Int("audit_events_erased", res.AuditErased), slog.Int("pending_kept", res.PendingKept+res.AuditKept), slog.Bool("more", res.More))
	if res.AuditSkipped != "" {
		r.engine.lg.Warn("retention: audit payloads were not erased", slog.String("application_id", p.ApplicationID), slog.String("why", res.AuditSkipped))
	}
	return nil
}

// retentionAuthority is the pool's authority for retentionServiceIdentity: that identity, the
// retention job type, a payload that names one application and nothing else, and a scope of that
// application that is the retention service's own. A declared identity alone grants nothing.
type retentionAuthority struct{}

// AuthorizeService implements jobs.ServiceAuthorizer.
func (retentionAuthority) AuthorizeService(_ context.Context, identity string, job jobs.Job) error {
	switch {
	case identity != retentionServiceIdentity:
		return contracts.New(contracts.ErrPermissionDenied, "unknown service identity")
	case job.Type != retentionJobType:
		return contracts.New(contracts.ErrPermissionDenied, "the service identity does not run this job type")
	}
	if err := job.Scope.Validate(); err != nil {
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "the retention job has no valid scope")
	}
	p, err := retentionDecode(job.Payload)
	if err != nil {
		return err
	}
	s := job.Scope
	if p.ApplicationID != s.ApplicationID || s.TenantID != retentionServiceTenant || s.WorkspaceID != retentionServiceTenant ||
		s.PrincipalID != retentionOperator || s.SessionID != retentionSession {
		return contracts.New(contracts.ErrPermissionDenied, "the retention job's scope is not the retention service's scope for its application")
	}
	return nil
}
