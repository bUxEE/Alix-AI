package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
)

// Operator inspection of a database file (master plan 18, 19; M5-25, the gap (b) of the M6-04 drill).
//
//	alixai audit verify-chain   verify the hash chains of every audit chain of the file
//	alixai audit export-checkpoint   print the newest signed head of one chain, to hold outside the database
//	alixai audit list           list the audit events of one application
//	alixai receipts list        list the action receipts of one application
//
// They read a database file with no running service: the restored database of a disaster-recovery
// step, the pre_restore_dir a restore leaves, a backup, a copy taken for an investigation.
//
// # Read only
//
// The commands never open the operator's file for writing. store.Open is a read-write open (it
// forces WAL, creates a missing path, applies migrations, checkpoints on close) and would rewrite
// the header of a backup, whose signed manifest names its hash. So the file (and its -wal, when one
// is beside it) is copied into a private temporary directory, owner-only, opened there without
// applying any migration and removed when the command ends. The original is only ever read. The
// price is the disk space of one copy and the time to make it; the service should be stopped, or the
// file should be a copy, because a copy taken while the service writes is not a consistent snapshot
// (use `backup create` for a running service).
//
// # What they print
//
// An explicit projection of columns, as JSON, in pages of at most opsInspectMaxLimit rows (`more`
// says there are rows after the last one, `next_after` is the cursor for the next page). Never the
// stored scope of a preparation, its confirmation binding, the prepared action or the body of a
// receipt: a receipt is listed by its identifiers, status and capability, and the facts and records
// the application returned stay in the database. An event's payload is printed only with -data,
// is already redacted at write time, is redacted again here and is left out when it is large.
//
// # Scope
//
// The commands that list rows (not verify-chain, whose report carries no event data) require
// -application: there is no "everything" listing. -tenant and -workspace narrow it further. A row of
// another application is never read into the answer.

const (
	// opsInspectDefaultLimit and opsInspectMaxLimit bound the rows of one page.
	opsInspectDefaultLimit = 100
	opsInspectMaxLimit     = 1000
	// opsInspectMaxData is the largest event payload printed with -data.
	opsInspectMaxData = 2048

	opsInspectVerifyUsage  = "alixai audit verify-chain [-db FILE | -data-dir DIR | -config FILE] [-application ID] [-tenant ID | -application-chain] [-require-anchor] [-expected-head FILE] [-limit N]"
	hardeningExportUsage   = "alixai audit export-checkpoint -application ID (-tenant ID | -application-chain) [-db FILE | -data-dir DIR | -config FILE] [-out FILE]"
	opsInspectAuditUsage   = "alixai audit list -application ID [-db FILE | -data-dir DIR | -config FILE] [-tenant ID] [-workspace ID] [-type NAME] [-after SEQ] [-limit N] [-data]"
	opsInspectReceiptUsage = "alixai receipts list -application ID [-db FILE | -data-dir DIR | -config FILE] [-tenant ID] [-workspace ID] [-preparation ID] [-status STATUS] [-after SEQ] [-limit N]"

	opsInspectTimeLayout = "2006-01-02T15:04:05.000Z"
)

// opsInspectReceiptStatuses are the statuses an action receipt can have.
var opsInspectReceiptStatuses = []string{"committed", "rejected", "queued", "partial", "uncertain"}

func init() {
	Register(Command{
		Name:    "audit verify-chain",
		Summary: "verify the hash chains and checkpoints of an audit database file without a running service (read-only)",
		Run:     opsInspectRunVerifyChain,
	})
	Register(Command{
		Name:    "audit export-checkpoint",
		Summary: "print the newest signed head of one audit chain, to hold outside the database and pass to verify-chain -expected-head (read-only)",
		Run:     hardeningRunExportCheckpoint,
	})
	Register(Command{
		Name:    "audit list",
		Summary: "list the audit events of one application from a database file (read-only, bounded, scope-filtered)",
		Run:     opsInspectRunAuditList,
	})
	Register(Command{
		Name:    "receipts list",
		Summary: "list the action receipts of one application from a database file (read-only, bounded, scope-filtered)",
		Run:     opsInspectRunReceiptsList,
	})
}

// ---- the file --------------------------------------------------------------------------

// opsInspectSource is the three flags that say which database file a command reads.
type opsInspectSource struct {
	db, dataDir, config *string
}

func opsInspectSourceFlags(env *Env, fs *flag.FlagSet) opsInspectSource {
	return opsInspectSource{
		db:      fs.String("db", "", "the database file to read (default <data_dir>/alixai.db); it is copied, never opened for writing"),
		dataDir: fs.String("data-dir", "", "data directory whose alixai.db is read (default: from the configuration)"),
		config:  env.ConfigFlag(fs),
	}
}

// path is the file to read: -db, else the database of -data-dir, else of the configuration.
func (s opsInspectSource) path(env *Env) (string, error) {
	if *s.db != "" {
		return filepath.Abs(*s.db)
	}
	dir, err := backupDataDir(env, *s.config, *s.dataDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, store.DefaultFileName), nil
}

var opsInspectSQLiteHeader = []byte("SQLite format 3\x00")

// opsInspectOpen copies the database file (and its -wal) into a private temporary directory and
// opens the copy with no migration applied. The returned function closes the copy and removes it.
// A path that does not exist, is not a file or is not a SQLite database is an error and nothing is
// created.
func opsInspectOpen(ctx context.Context, path string) (*store.DB, func(), error) {
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return nil, nil, fmt.Errorf("there is no database at %s: %w", path, err)
	case !fi.Mode().IsRegular():
		return nil, nil, fmt.Errorf("%s is not a file", path)
	case fi.Size() == 0:
		return nil, nil, fmt.Errorf("%s is empty", path)
	}
	tmp, err := os.MkdirTemp("", "alixai-inspect-")
	if err != nil {
		return nil, nil, fmt.Errorf("a private directory for the copy cannot be made: %w", err)
	}
	remove := func() { _ = os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, store.DefaultFileName)
	if err := opsInspectCopy(path, dst, true); err != nil {
		remove()
		return nil, nil, err
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		if err := opsInspectCopy(path+"-wal", dst+"-wal", false); err != nil {
			remove()
			return nil, nil, err
		}
	}
	opts := store.Options{Path: dst, Config: store.DefaultConfig(), Migrations: store.NewRegistry()}
	db, err := store.Open(ctx, opts)
	if err != nil {
		remove()
		return nil, nil, fmt.Errorf("open the copy of %s: %w", path, err)
	}
	return db, func() { _ = db.Close(); remove() }, nil
}

// opsInspectCopy copies src to dst (created owner-only). With header set the source must start with
// the SQLite header.
func opsInspectCopy(src, dst string, header bool) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	defer in.Close()
	if header {
		buf := make([]byte, len(opsInspectSQLiteHeader))
		if _, err := io.ReadFull(in, buf); err != nil || string(buf) != string(opsInspectSQLiteHeader) {
			return fmt.Errorf("%s is not a SQLite database", src)
		}
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return out.Close()
}

// opsInspectLimit validates -limit.
func opsInspectLimit(n int, usage string) error {
	if n < 1 || n > opsInspectMaxLimit {
		return Usagef("-limit must be between 1 and %d (usage: %s)", opsInspectMaxLimit, usage)
	}
	return nil
}

// opsInspectStamp is a unix-millisecond column as text.
func opsInspectStamp(ms int64) string { return time.UnixMilli(ms).UTC().Format(opsInspectTimeLayout) }

// ---- audit verify-chain ------------------------------------------------------------------

type opsInspectChain struct {
	ApplicationID     string          `json:"application_id"`
	TenantID          string          `json:"tenant_id"`
	OK                bool            `json:"ok"`
	Events            int64           `json:"events"`
	HeadSeq           int64           `json:"head_seq"`
	HeadHash          string          `json:"head_hash"`
	Anchored          bool            `json:"anchored"`
	AnchorSeq         int64           `json:"anchor_seq"`
	EventsAfterAnchor int64           `json:"events_after_anchor"`
	Erased            int64           `json:"erased,omitempty"`
	Problems          []audit.Problem `json:"problems,omitempty"`
	MoreProblems      bool            `json:"more_problems,omitempty"`
	Error             string          `json:"error,omitempty"`
}

// opsInspectRunVerifyChain verifies every chain of the file (or the ones -application and -tenant
// select) and prints a report. The exit code is non-zero when any chain fails, when a chain could
// not be checked, and when no chain matches: verifying nothing is not a success.
func opsInspectRunVerifyChain(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("audit verify-chain")
	src := opsInspectSourceFlags(env, fs)
	app := fs.String("application", "", "verify only the chains of this application")
	tenant := fs.String("tenant", "", "verify only the chain of this tenant")
	appChain := fs.Bool("application-chain", false, "verify only the chains with no tenant (an application's own, the platform's), and accept an -expected-head of one")
	anchor := fs.Bool("require-anchor", false, "a chain with events and no verified, matching signed checkpoint is a failure")
	expectedFile := fs.String("expected-head", "", "a checkpoint file written by audit export-checkpoint and held outside the database: the chain it names must extend it (catches a truncation or rewrite the database alone cannot see)")
	limit := fs.Int("limit", opsInspectDefaultLimit, fmt.Sprintf("the most chains listed in the report, failed ones first (1..%d); every selected chain is verified", opsInspectMaxLimit))
	if err := env.ParseFlags(fs, opsInspectVerifyUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), opsInspectVerifyUsage)
	}
	if *appChain && *tenant != "" {
		return Usagef("-tenant and -application-chain exclude each other (usage: %s)", opsInspectVerifyUsage)
	}
	if err := opsInspectLimit(*limit, opsInspectVerifyUsage); err != nil {
		return err
	}
	var expected *audit.Checkpoint
	if *expectedFile != "" {
		var rerr error
		if expected, rerr = hardeningReadCheckpoint(*expectedFile, *appChain); rerr != nil {
			return rerr
		}
	}
	path, err := src.path(env)
	if err != nil {
		return err
	}
	db, closeDB, err := opsInspectOpen(ctx, path)
	if err != nil {
		return err
	}
	defer closeDB()

	clk := clock.Real{}
	lg, err := audit.New(db, audit.Options{Clock: clk, Verifier: trust.NewVerifier(trust.NewStore(db, clk), clk)})
	if err != nil {
		return err
	}
	keys, err := lg.VerifiableChains(ctx)
	if err != nil {
		return fmt.Errorf("the audit chains cannot be listed: %w", err)
	}
	var failed, passed []opsInspectChain
	expectedChecked := false
	for _, key := range keys {
		if (*app != "" && key.ApplicationID != *app) || (*tenant != "" && key.TenantID != *tenant) || (*appChain && key.TenantID != "") {
			continue
		}
		vopts := audit.VerifyOptions{RequireAnchor: *anchor}
		if expected != nil && expected.Checkpoint.Chain() == key {
			vopts.Expected, expectedChecked = expected, true
		}
		rep, verr := lg.VerifyChain(ctx, key, vopts)
		c := opsInspectChain{
			ApplicationID: key.ApplicationID, TenantID: key.TenantID, Events: rep.Events, HeadSeq: rep.HeadSeq, HeadHash: rep.HeadHash,
			Anchored: rep.Anchored, AnchorSeq: rep.AnchorSeq, EventsAfterAnchor: rep.EventsAfterAnchor, Erased: int64(rep.Erased),
			Problems: rep.Problems, MoreProblems: rep.MoreProblems, OK: verr == nil && rep.OK(),
		}
		if verr != nil && !errors.Is(verr, audit.ErrChainBroken) {
			c.Error = "the chain could not be checked: " + verr.Error()
		}
		if c.OK {
			passed = append(passed, c)
		} else {
			failed = append(failed, c)
		}
	}
	total := len(failed) + len(passed)
	listed := append(append([]opsInspectChain{}, failed...), passed...)
	if len(listed) > *limit {
		listed = listed[:*limit]
	}
	out := map[string]any{
		"database": path, "chains": total, "chains_ok": len(passed), "chains_failed": len(failed),
		"listed": listed, "truncated": total > len(listed),
	}
	if expected != nil {
		out["expected_head"] = map[string]any{
			"file": *expectedFile, "application_id": expected.Checkpoint.ApplicationID, "tenant_id": expected.Checkpoint.TenantID,
			"head_seq": expected.Checkpoint.HeadSeq, "checked": expectedChecked,
		}
	}
	if err := backupEmit(env.Stdout, out); err != nil {
		return err
	}
	switch {
	case expected != nil && !expectedChecked:
		// A chain that is gone, or that -application/-tenant left out, cannot be held to its head:
		// that is not a pass.
		return fmt.Errorf("the chain of -expected-head (application %q, tenant %q) was not among the chains verified: it was removed, or -application/-tenant/-application-chain leave it out",
			expected.Checkpoint.ApplicationID, expected.Checkpoint.TenantID)
	case total == 0:
		return errors.New("no audit chain matches: nothing was verified")
	case len(failed) > 0:
		return fmt.Errorf("%d of %d audit chains failed verification", len(failed), total)
	}
	return nil
}

// ---- audit list --------------------------------------------------------------------------

type opsInspectEvent struct {
	Seq           int64           `json:"seq"`
	ID            string          `json:"id"`
	ApplicationID string          `json:"application_id"`
	TenantID      string          `json:"tenant_id"`
	WorkspaceID   string          `json:"workspace_id"`
	ChainSeq      int64           `json:"chain_seq"`
	Type          string          `json:"type"`
	OccurredAt    string          `json:"occurred_at"`
	PrincipalID   string          `json:"principal_id"`
	SessionID     string          `json:"session_id"`
	Hash          string          `json:"hash"`
	Data          json.RawMessage `json:"data,omitempty"`
	DataOmitted   string          `json:"data_omitted,omitempty"`
}

func opsInspectRunAuditList(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("audit list")
	src := opsInspectSourceFlags(env, fs)
	app := fs.String("application", "", "the application whose events are listed (required)")
	tenant := fs.String("tenant", "", "only the events of this tenant")
	workspace := fs.String("workspace", "", "only the events of this workspace")
	typ := fs.String("type", "", "only events of this type, e.g. action.committed")
	after := fs.Int64("after", 0, "list the events after this seq (the next_after of the previous page)")
	limit := fs.Int("limit", opsInspectDefaultLimit, fmt.Sprintf("the most events listed (1..%d)", opsInspectMaxLimit))
	withData := fs.Bool("data", false, fmt.Sprintf("include each event's payload (redacted; left out above %d bytes)", opsInspectMaxData))
	if err := env.ParseFlags(fs, opsInspectAuditUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *app == "" {
		return Usagef("-application is required (usage: %s)", opsInspectAuditUsage)
	}
	if err := opsInspectLimit(*limit, opsInspectAuditUsage); err != nil {
		return err
	}
	if *after < 0 {
		return Usagef("-after must not be negative (usage: %s)", opsInspectAuditUsage)
	}
	path, err := src.path(env)
	if err != nil {
		return err
	}
	db, closeDB, err := opsInspectOpen(ctx, path)
	if err != nil {
		return err
	}
	defer closeDB()

	where, wargs := opsInspectWhere("", map[string]string{"application_id": *app, "tenant_id": *tenant, "workspace_id": *workspace, "event_type": *typ})
	rows, err := db.QueryContext(ctx,
		`SELECT seq, id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, hash, data
		   FROM audit_events WHERE `+where+` AND seq > ? ORDER BY seq LIMIT ?`,
		append(wargs, *after, *limit+1)...)
	if err != nil {
		return fmt.Errorf("the audit events cannot be read: %w", err)
	}
	defer rows.Close()
	red := alog.NewRedactor(nil)
	var events []opsInspectEvent
	more := false
	for rows.Next() {
		var e opsInspectEvent
		var ms int64
		var data string
		if err := rows.Scan(&e.Seq, &e.ID, &e.ApplicationID, &e.TenantID, &e.WorkspaceID, &e.ChainSeq, &e.Type, &ms, &e.PrincipalID, &e.SessionID, &e.Hash, &data); err != nil {
			return fmt.Errorf("the audit events cannot be read: %w", err)
		}
		if len(events) == *limit {
			more = true
			break
		}
		e.OccurredAt = opsInspectStamp(ms)
		if *withData {
			e.Data, e.DataOmitted = opsInspectPayload(red, data)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("the audit events cannot be read: %w", err)
	}
	return opsInspectEmitPage(env, path, "events", events, len(events), more, func(i int) int64 { return events[i].Seq })
}

// opsInspectPayload is an event's stored payload, redacted again and bounded; the second value says
// why there is none.
func opsInspectPayload(red *alog.Redactor, stored string) (json.RawMessage, string) {
	var v any
	if err := json.Unmarshal([]byte(stored), &v); err != nil {
		return nil, "unreadable"
	}
	b, err := json.Marshal(opsInspectRedact(red, v))
	switch {
	case err != nil:
		return nil, "unreadable"
	case len(b) > opsInspectMaxData:
		return nil, fmt.Sprintf("larger than %d bytes", opsInspectMaxData)
	}
	return b, ""
}

// opsInspectRedact replaces the value under every sensitive key and scrubs strings, as the audit
// log does when it writes.
func opsInspectRedact(red *alog.Redactor, x any) any {
	switch t := x.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			if red.SensitiveKey(k) {
				out[k] = audit.Redacted
			} else {
				out[k] = opsInspectRedact(red, v)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = opsInspectRedact(red, v)
		}
		return out
	case string:
		return red.String(t)
	}
	return x
}

// opsInspectWhere is the equality conditions of the filters that are set, joined with AND. The
// column names are the constants of the callers, never input.
func opsInspectWhere(alias string, filters map[string]string) (string, []any) {
	var conds []string
	var args []any
	for _, col := range []string{"application_id", "tenant_id", "workspace_id", "preparation_id", "status", "event_type"} {
		if v, ok := filters[col]; ok && v != "" {
			conds = append(conds, alias+col+" = ?")
			args = append(args, v)
		}
	}
	return strings.Join(conds, " AND "), args
}

// opsInspectEmitPage prints one page: the rows, whether there are more and the cursor for the next.
func opsInspectEmitPage(env *Env, path, key string, rows any, n int, more bool, seqAt func(int) int64) error {
	out := map[string]any{"database": path, key: rows, "count": n, "more": more}
	if n == 0 {
		out[key] = []any{}
	}
	if more && n > 0 {
		out["next_after"] = seqAt(n - 1)
	}
	return backupEmit(env.Stdout, out)
}

// ---- receipts list -----------------------------------------------------------------------

type opsInspectReceipt struct {
	Seq              int64    `json:"seq"`
	ReceiptID        string   `json:"receipt_id"`
	PreparationID    string   `json:"preparation_id"`
	ApplicationID    string   `json:"application_id"`
	TenantID         string   `json:"tenant_id"`
	WorkspaceID      string   `json:"workspace_id"`
	Status           string   `json:"status"`
	CapabilityID     string   `json:"capability_id"`
	IssuedAt         string   `json:"issued_at"`
	PreparationState string   `json:"preparation_state,omitempty"`
	ErrorCodes       []string `json:"error_codes,omitempty"`
	Unreadable       bool     `json:"receipt_unreadable,omitempty"`
}

func opsInspectRunReceiptsList(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("receipts list")
	src := opsInspectSourceFlags(env, fs)
	app := fs.String("application", "", "the application whose receipts are listed (required)")
	tenant := fs.String("tenant", "", "only the receipts of this tenant")
	workspace := fs.String("workspace", "", "only the receipts of this workspace")
	prep := fs.String("preparation", "", "only the receipts of this preparation")
	status := fs.String("status", "", "only receipts with this status ("+strings.Join(opsInspectReceiptStatuses, ", ")+")")
	after := fs.Int64("after", 0, "list the receipts after this seq (the next_after of the previous page)")
	limit := fs.Int("limit", opsInspectDefaultLimit, fmt.Sprintf("the most receipts listed (1..%d)", opsInspectMaxLimit))
	if err := env.ParseFlags(fs, opsInspectReceiptUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *app == "" {
		return Usagef("-application is required (usage: %s)", opsInspectReceiptUsage)
	}
	if err := opsInspectLimit(*limit, opsInspectReceiptUsage); err != nil {
		return err
	}
	if *after < 0 {
		return Usagef("-after must not be negative (usage: %s)", opsInspectReceiptUsage)
	}
	if *status != "" && !slices.Contains(opsInspectReceiptStatuses, *status) {
		return Usagef("-status %q is not one of %s", *status, strings.Join(opsInspectReceiptStatuses, ", "))
	}
	path, err := src.path(env)
	if err != nil {
		return err
	}
	db, closeDB, err := opsInspectOpen(ctx, path)
	if err != nil {
		return err
	}
	defer closeDB()

	where, wargs := opsInspectWhere("r.", map[string]string{
		"application_id": *app, "tenant_id": *tenant, "workspace_id": *workspace, "preparation_id": *prep, "status": *status,
	})
	rows, err := db.QueryContext(ctx,
		`SELECT r.seq, r.id, r.preparation_id, r.application_id, r.tenant_id, r.workspace_id, r.status, r.receipt_json, r.issued_at,
		        COALESCE((SELECT p.state FROM action_preparations p WHERE p.id = r.preparation_id AND p.application_id = r.application_id
		                  AND p.tenant_id = r.tenant_id AND p.workspace_id = r.workspace_id), '')
		   FROM action_receipts r WHERE `+where+` AND r.seq > ? ORDER BY r.seq LIMIT ?`,
		append(wargs, *after, *limit+1)...)
	if err != nil {
		return fmt.Errorf("the receipts cannot be read: %w", err)
	}
	defer rows.Close()
	var receipts []opsInspectReceipt
	more := false
	for rows.Next() {
		var r opsInspectReceipt
		var body string
		var ms int64
		if err := rows.Scan(&r.Seq, &r.ReceiptID, &r.PreparationID, &r.ApplicationID, &r.TenantID, &r.WorkspaceID, &r.Status, &body, &ms, &r.PreparationState); err != nil {
			return fmt.Errorf("the receipts cannot be read: %w", err)
		}
		if len(receipts) == *limit {
			more = true
			break
		}
		r.IssuedAt = opsInspectStamp(ms)
		// Only two members of the stored receipt are read: the capability and the codes of its issues.
		var rc struct {
			CapabilityID string `json:"capability_id"`
			Errors       []struct {
				Code string `json:"code"`
			} `json:"errors"`
		}
		if err := json.Unmarshal([]byte(body), &rc); err != nil {
			r.Unreadable = true
		} else {
			r.CapabilityID = rc.CapabilityID
			for _, e := range rc.Errors {
				r.ErrorCodes = append(r.ErrorCodes, e.Code)
			}
		}
		receipts = append(receipts, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("the receipts cannot be read: %w", err)
	}
	return opsInspectEmitPage(env, path, "receipts", receipts, len(receipts), more, func(i int) int64 { return receipts[i].Seq })
}

// ---- audit export-checkpoint and verify-chain -expected-head (SEC-086, backlog gap G6) ------

// hardeningMaxCheckpointBytes bounds a checkpoint file: a signed head is a few hundred bytes.
const hardeningMaxCheckpointBytes = 64 << 10

// hardeningReadCheckpoint reads a checkpoint exported by `audit export-checkpoint`. It does not
// verify it: VerifyChain does, against the trust store, and a head that does not verify is a
// problem in the report, never an anchor. A head of a chain with no tenant is read only with
// appChain (-application-chain), and then only such a head.
func hardeningReadCheckpoint(path string, appChain bool) (*audit.Checkpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("-expected-head: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, hardeningMaxCheckpointBytes+1))
	if err != nil {
		return nil, fmt.Errorf("-expected-head: read %s: %w", path, err)
	}
	if len(raw) > hardeningMaxCheckpointBytes {
		return nil, fmt.Errorf("-expected-head: %s is larger than %d bytes: it is not a checkpoint", path, hardeningMaxCheckpointBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cp audit.Checkpoint
	if err := dec.Decode(&cp); err != nil {
		return nil, fmt.Errorf("-expected-head: %s is not a checkpoint: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("-expected-head: %s holds more than one document", path)
	}
	if cp.Checkpoint.ApplicationID == "" || cp.Checkpoint.HeadSeq < 1 {
		return nil, fmt.Errorf("-expected-head: %s does not name a chain and a head", path)
	}
	switch {
	case cp.Checkpoint.TenantID == "" && !appChain:
		return nil, fmt.Errorf("-expected-head: %s is the head of a chain with no tenant (application %q): add -application-chain", path, cp.Checkpoint.ApplicationID)
	case cp.Checkpoint.TenantID != "" && appChain:
		return nil, fmt.Errorf("-expected-head: %s is the head of the chain of tenant %q, not of a chain with no tenant: drop -application-chain", path, cp.Checkpoint.TenantID)
	}
	return &cp, nil
}

// hardeningRunExportCheckpoint prints the newest stored checkpoint of one chain after checking that
// it verifies, so the copy an operator keeps outside the database (a backup manifest, another
// machine) is one that can be relied on later. It gives audit.ExportCheckpoint its production caller.
func hardeningRunExportCheckpoint(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("audit export-checkpoint")
	src := opsInspectSourceFlags(env, fs)
	app := fs.String("application", "", "the application of the chain (required)")
	tenant := fs.String("tenant", "", "the tenant of the chain (required unless -application-chain)")
	appChain := fs.Bool("application-chain", false, "the chain with no tenant: the application's own (package lifecycle, integration-audit runs), or the platform's with -application "+trust.PlatformApplicationID)
	out := fs.String("out", "", "write the checkpoint to this new file (owner-only) instead of standard output")
	if err := env.ParseFlags(fs, hardeningExportUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *app == "" || (*tenant == "") != *appChain {
		return Usagef("-application and one of -tenant or -application-chain are required (usage: %s)", hardeningExportUsage)
	}
	path, err := src.path(env)
	if err != nil {
		return err
	}
	db, closeDB, err := opsInspectOpen(ctx, path)
	if err != nil {
		return err
	}
	defer closeDB()
	clk := clock.Real{}
	lg, err := audit.New(db, audit.Options{Clock: clk, Verifier: trust.NewVerifier(trust.NewStore(db, clk), clk)})
	if err != nil {
		return err
	}
	key := audit.ChainKey{ApplicationID: *app, TenantID: *tenant}
	cp, err := lg.ExportCheckpoint(ctx, key)
	if err != nil {
		return fmt.Errorf("the checkpoint of the chain cannot be exported: %w", err)
	}
	if err := lg.VerifyCheckpoint(ctx, cp, key); err != nil {
		return fmt.Errorf("the newest stored checkpoint does not verify, so it is not exported: %w", err)
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if *out == "" {
		_, err = env.Stdout.Write(raw)
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("-out: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return fmt.Errorf("-out: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("-out: %w", err)
	}
	return backupEmit(env.Stdout, map[string]any{
		"file": *out, "application_id": *app, "tenant_id": *tenant, "head_seq": cp.Checkpoint.HeadSeq, "head_hash": cp.Checkpoint.HeadHash,
	})
}
