package dr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// core is the data directory opened in-process, for the two things the drill cannot do through the
// binary: create a preparation (see the package comment) and read back what is stored. It is closed
// before the service is started over the same directory.
type core struct {
	t        *testing.T
	d        *deployment
	db       *store.DB
	audit    *audit.Log
	co       *actions.Coordinator
	sess     *sessions.Store
	adapters *drAdapters
}

var configMu sync.Mutex

func noEnv(string) (string, bool) { return "", false }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// openDB opens a database file in-process the way the service does (the same migrations, the same
// pragmas). The file's directory need not be a deployment: a restore's pre_restore_dir is opened
// this way to read what was written after the backup.
func openDB(t *testing.T, cfgPath, path string) *store.DB {
	t.Helper()
	configMu.Lock()
	err := config.Default.LoadFrom(cfgPath, noEnv)
	configMu.Unlock()
	if err != nil {
		t.Fatalf("load the configuration: %v", err)
	}
	opts := store.FromConfig()
	opts.Path = path
	opts.Logger = discardLog()
	db, err := store.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("open the store at %s: %v", path, err)
	}
	return db
}

func (d *deployment) openCore() *core {
	d.t.Helper()
	ctx := context.Background()
	log := discardLog()
	clk := clock.Real{}
	db := openDB(d.t, d.cfgPath, d.dbPath())
	c := &core{t: d.t, d: d, db: db}

	verifier := trust.NewVerifier(trust.NewStore(db, clk), clk)
	var err error
	c.audit, err = audit.New(db, audit.Options{Clock: clk, Registry: audit.DefaultRegistry, Logger: log, Verifier: verifier})
	if err != nil {
		d.t.Fatalf("audit log: %v", err)
	}
	gw := policy.NewGateway(clk, policy.WithSink(audit.NewRecordSink[policy.Record](c.audit, audit.EventPolicyDecision, func(rec policy.Record) (audit.Locator, error) {
		s := rec.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})))
	coreVersion := apppkg.CoreVersionOrDev(version.Get().Version)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: d.dataDir, Verifier: verifier, CoreVersion: coreVersion})
	if err != nil {
		d.t.Fatalf("installer: %v", err)
	}
	apps, err := gateway.NewApplications(inst, apppkg.Options{Verifier: verifier, CoreVersion: coreVersion}, clk, 0)
	if err != nil {
		d.t.Fatalf("applications: %v", err)
	}
	c.adapters = &drAdapters{apps: apps, inst: inst, dataDir: d.dataDir}
	c.co, err = actions.New(actions.Deps{
		DB: db, Policy: gw, Audit: c.audit, Clock: clk, Logger: log,
		Packages: actions.PackageSourceFunc(apps.ActivePackage), Adapters: c.adapters,
	})
	if err != nil {
		d.t.Fatalf("action coordinator: %v", err)
	}
	if c.sess, err = sessions.New(db, clk, sessions.Config{}); err != nil {
		d.t.Fatalf("session store: %v", err)
	}
	_ = ctx
	return c
}

func (c *core) close() {
	if c.adapters != nil {
		c.adapters.close()
	}
	if c.db != nil {
		if err := c.db.Close(); err != nil {
			c.t.Errorf("close the store: %v", err)
		}
		c.db = nil
	}
}

// drAdapters is the actions.AdapterSource of the in-process coordinator: the application's adapter
// built from the installed package by the same builder serve and the audit use, wrapped by
// adapters.Wrap as the coordinator requires.
type drAdapters struct {
	apps    *gateway.Applications
	inst    *apppkg.Installer
	dataDir string
	mu      sync.Mutex
	open    []*cli.PackageAdapter
}

func (s *drAdapters) Adapter(ctx context.Context, applicationID string) (adapters.AppAdapter, error) {
	pkg, err := s.apps.Package(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	hash := pkg.ContentHash()
	opened, rec, err := s.inst.Open(ctx, applicationID, hash)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "the installed package could not be opened")
	}
	pa, err := cli.BuildPackageAdapter(ctx, cli.BuildRequest{
		ApplicationID: applicationID, Package: opened, ManifestHash: hash,
		Dir: filepath.Join(s.dataDir, filepath.FromSlash(rec.Path)),
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.open = append(s.open, pa)
	s.mu.Unlock()
	return adapters.Wrap(pa.Adapter(), adapters.Options{ApplicationID: applicationID})
}

func (s *drAdapters) close() {
	s.mu.Lock()
	open := s.open
	s.open = nil
	s.mu.Unlock()
	for _, pa := range open {
		_ = pa.Close()
	}
}

// prepared is a booking that was prepared and awaits its confirmation.
type prepared struct {
	ID        string
	Secret    string
	SessionID string
}

// prepareBooking prepares the booking of one slot for the seeded patient, as the dialogue engine
// would after a turn that named the patient and the slot: the real coordinator, the real adapter,
// the receptionist's scope exactly as the application's context token states it.
func (c *core) prepareBooking(w *world, slot int) prepared {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sessionID := "ses_dr_" + randomSuffix()
	roles, scopes, err := tokenClaims(w.hostToken(c.t, sessionID))
	if err != nil {
		c.t.Fatalf("the host's context token: %v", err)
	}
	scope := contracts.Scope{
		DeploymentID: deploymentID, ApplicationID: clinicApplication, TenantID: w.Tenant, WorkspaceID: w.Site,
		PrincipalID: w.Reception, Roles: roles, CapabilityScopes: scopes, SessionID: sessionID,
	}
	sess, err := c.sess.Open(ctx, scope)
	if err != nil {
		c.t.Fatalf("open the session: %v", err)
	}
	out, err := c.co.Prepare(ctx, scope, actions.PrepareInput{
		Session: sess.Scope, CapabilityID: "appointments.book",
		Fields: map[string]any{"patient": w.PatientID, "slot": w.Slots[slot]},
	})
	if err != nil {
		c.t.Fatalf("prepare the booking: %v", err)
	}
	if out.Confirmation.IsZero() {
		c.t.Fatalf("the booking needs an explicit confirmation and Prepare returned none (mode %s)", out.Action.Confirmation.Mode)
	}
	return prepared{ID: string(out.Action.PreparationID), Secret: out.Confirmation.Reveal(), SessionID: sessionID}
}

// seedTranscript writes n transcript turns to a session of the world in one transaction, with the
// marker text on the last. The history index follows the rows through its triggers, as it does for
// turns written by the dialogue engine. The marker is upper case in the table and lower case in
// the index, so its lower-case bytes occur only in the derived index.
func (c *core) seedTranscript(w *world, n int, marker string) {
	c.t.Helper()
	ctx := context.Background()
	sessionID := "ses_dr_" + randomSuffix()
	_, scopes, err := tokenClaims(w.hostToken(c.t, sessionID))
	if err != nil {
		c.t.Fatal(err)
	}
	scope := contracts.Scope{
		DeploymentID: deploymentID, ApplicationID: clinicApplication, TenantID: w.Tenant, WorkspaceID: w.Site,
		PrincipalID: w.Reception, Roles: []string{"receptionist"}, CapabilityScopes: scopes, SessionID: sessionID,
	}
	if _, err := c.sess.Open(ctx, scope); err != nil {
		c.t.Fatalf("open the session: %v", err)
	}
	cols := []string{"session_id", "principal_id", "turn_id", "n", "role", "act", "body", "clipped", "created_at"}
	words := []string{"booking", "reschedule", "reminder", "invoice", "referral", "cancellation", "availability", "contact", "followup", "insurance"}
	err = c.db.Tx(ctx, func(tx *store.Tx) error {
		now := time.Now().UnixMilli()
		for i := 1; i <= n; i++ {
			body := fmt.Sprintf("patient asked about %s %s number %d on behalf of the front desk", words[i%len(words)], words[(i/10)%len(words)], i)
			if i == n {
				body = marker + " is the word the drill searches for"
			}
			if _, err := store.InsertScoped(ctx, tx, scope, "transcript_turns", cols,
				sessionID, w.Reception, ids.New("turn"), i, "user", "", body, 0, now+int64(i)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		c.t.Fatalf("seed the transcript: %v", err)
	}
}

// ---- reading back what is stored ---------------------------------------------------------------

type receiptRow struct {
	ID, PreparationID, Status, JSON string
}

type chainFact struct {
	Chain    string
	Events   int64
	HeadHash string
	OK       bool
	Problems []string
}

// snapshot is what must survive a disaster: every receipt, the state of every preparation, and every
// audit chain with its head, verified.
type snapshot struct {
	Receipts     []receiptRow
	Preparations map[string]string
	Chains       []chainFact
	EventTypes   map[string]int
	Turns        int
	TurnsDigest  string
}

func (s snapshot) chainsOK() error {
	for _, c := range s.Chains {
		if !c.OK {
			return fmt.Errorf("the audit chain %s does not verify: %v", c.Chain, c.Problems)
		}
	}
	if len(s.Chains) == 0 {
		return errors.New("there is no audit chain to verify")
	}
	return nil
}

func (s snapshot) chain(name string) (chainFact, bool) {
	for _, c := range s.Chains {
		if c.Chain == name {
			return c, true
		}
	}
	return chainFact{}, false
}

func (s snapshot) receipt(prepID string) (receiptRow, bool) {
	for _, r := range s.Receipts {
		if r.PreparationID == prepID {
			return r, true
		}
	}
	return receiptRow{}, false
}

// takeSnapshot opens the database file in-process, reads it and verifies every audit chain.
func takeSnapshot(t *testing.T, cfgPath, dbPath string) snapshot {
	t.Helper()
	db := openDB(t, cfgPath, dbPath)
	defer db.Close()
	return snapshotOf(t, db)
}

func snapshotOf(t *testing.T, db *store.DB) snapshot {
	t.Helper()
	ctx := context.Background()
	s := snapshot{Preparations: map[string]string{}, EventTypes: map[string]int{}}

	rows, err := db.QueryContext(ctx, `SELECT id, preparation_id, status, receipt_json FROM action_receipts ORDER BY seq`)
	if err != nil {
		t.Fatalf("read the receipts: %v", err)
	}
	for rows.Next() {
		var r receiptRow
		if err := rows.Scan(&r.ID, &r.PreparationID, &r.Status, &r.JSON); err != nil {
			t.Fatal(err)
		}
		s.Receipts = append(s.Receipts, r)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err = db.QueryContext(ctx, `SELECT id, state FROM action_preparations ORDER BY seq`)
	if err != nil {
		t.Fatalf("read the preparations: %v", err)
	}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		s.Preparations[id] = state
	}
	_ = rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT event_type, count(*) FROM audit_events GROUP BY event_type`)
	if err != nil {
		t.Fatalf("read the audit events: %v", err)
	}
	for rows.Next() {
		var typ string
		var n int
		if err := rows.Scan(&typ, &n); err != nil {
			t.Fatal(err)
		}
		s.EventTypes[typ] = n
	}
	_ = rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT body FROM transcript_turns ORDER BY seq`)
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	h := sha256.New()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		s.Turns++
		h.Write([]byte(body))
		h.Write([]byte{0})
	}
	_ = rows.Close()
	s.TurnsDigest = hex.EncodeToString(h.Sum(nil))

	clk := clock.Real{}
	verifier := trust.NewVerifier(trust.NewStore(db, clk), clk)
	log, err := audit.New(db, audit.Options{Clock: clk, Registry: audit.DefaultRegistry, Logger: discardLog(), Verifier: verifier})
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	chains, err := log.Chains(ctx)
	if err != nil {
		t.Fatalf("list the audit chains: %v", err)
	}
	for _, key := range chains {
		rep, err := log.VerifyChain(ctx, key, audit.VerifyOptions{})
		f := chainFact{Chain: key.String(), Events: rep.Events, HeadHash: rep.HeadHash, OK: err == nil && rep.OK()}
		for _, p := range rep.Problems {
			f.Problems = append(f.Problems, string(p.Kind)+": "+p.Detail)
		}
		if err != nil && len(f.Problems) == 0 {
			f.Problems = append(f.Problems, err.Error())
		}
		s.Chains = append(s.Chains, f)
	}
	return s
}

// eventsOf lists the audit event types that mention a preparation, oldest first.
func eventsOf(t *testing.T, db *store.DB, prepID string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT event_type FROM audit_events WHERE data LIKE ? ORDER BY seq`, "%"+prepID+"%")
	if err != nil {
		t.Fatalf("read the audit events of %s: %v", prepID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		out = append(out, typ)
	}
	return out
}

func hasEvent(events []string, typ string) bool {
	for _, e := range events {
		if e == typ {
			return true
		}
	}
	return false
}

// sameReceipts reports the first difference between two sets of receipts, comparing every column.
func sameReceipts(a, b []receiptRow) error {
	if len(a) != len(b) {
		return fmt.Errorf("%d receipts became %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			return fmt.Errorf("receipt %d differs:\n  %+v\n  %+v", i, a[i], b[i])
		}
	}
	return nil
}

// lateAuditTypes may land on a chain after a request returns and before a live
// backup finishes; they are not part of a receipt's guarantee.
var lateAuditTypes = map[string]bool{
	"credentials.use": true,
}

// assertBackupHoldsBefore checks that the backup kept everything that existed
// when backup create started. Extra events after the pre-backup head are only
// allowed when every one is a lateAuditTypes background write.
func assertBackupHoldsBefore(t *testing.T, cfgPath, backupDB string, before, fromBackup snapshot) {
	t.Helper()
	if err := sameReceipts(before.Receipts, fromBackup.Receipts); err != nil {
		t.Fatalf("the backup's receipts are not what existed before backup create: %v", err)
	}
	if fromBackup.Turns != before.Turns || fromBackup.TurnsDigest != before.TurnsDigest {
		t.Fatalf("the backup's transcript is not what existed before backup create: %d turns digest %s, want %d %s",
			fromBackup.Turns, fromBackup.TurnsDigest, before.Turns, before.TurnsDigest)
	}
	db := openDB(t, cfgPath, backupDB)
	defer db.Close()
	for _, b := range before.Chains {
		a, ok := fromBackup.chain(b.Chain)
		if !ok {
			t.Fatalf("the backup lost audit chain %s", b.Chain)
		}
		if a.Events < b.Events {
			t.Fatalf("the backup dropped events on %s: %d, had %d before backup create", b.Chain, a.Events, b.Events)
		}
		got, err := eventHashAt(db, b.Chain, b.Events)
		if err != nil {
			t.Fatalf("read the pre-backup head of %s in the backup: %v", b.Chain, err)
		}
		if got != b.HeadHash {
			t.Fatalf("the backup's chain %s does not keep the pre-backup head: at seq %d hash %s, want %s",
				b.Chain, b.Events, got, b.HeadHash)
		}
		if a.Events == b.Events {
			continue
		}
		extra, err := eventTypesAfter(db, b.Chain, b.Events)
		if err != nil {
			t.Fatalf("list the extra events of %s in the backup: %v", b.Chain, err)
		}
		for typ := range extra {
			if !lateAuditTypes[typ] {
				t.Fatalf("the backup grew chain %s after the pre-backup snapshot with non-background events: %v", b.Chain, extra)
			}
		}
	}
}

func splitChainName(name string) (app, tenant string, err error) {
	app, ten, ok := strings.Cut(name, "/")
	if !ok || app == "" {
		return "", "", fmt.Errorf("bad chain %q", name)
	}
	if ten == "-" {
		ten = ""
	}
	return app, ten, nil
}

func eventHashAt(db *store.DB, chain string, seq int64) (string, error) {
	app, tenant, err := splitChainName(chain)
	if err != nil {
		return "", err
	}
	var hash string
	err = db.QueryRowContext(context.Background(),
		`SELECT hash FROM audit_events WHERE application_id = ? AND tenant_id = ? AND chain_seq = ?`,
		app, tenant, seq).Scan(&hash)
	return hash, err
}

func eventTypesAfter(db *store.DB, chain string, afterSeq int64) (map[string]int, error) {
	app, tenant, err := splitChainName(chain)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(context.Background(),
		`SELECT event_type, count(*) FROM audit_events
		 WHERE application_id = ? AND tenant_id = ? AND chain_seq > ?
		 GROUP BY event_type ORDER BY event_type`,
		app, tenant, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var typ string
		var n int
		if err := rows.Scan(&typ, &n); err != nil {
			return nil, err
		}
		out[typ] = n
	}
	return out, rows.Err()
}

// Summary is the one line the evidence report gives for a database: counts, and that the chains verify.
func (s snapshot) Summary() string {
	events := int64(0)
	ok := 0
	for _, c := range s.Chains {
		events += c.Events
		if c.OK {
			ok++
		}
	}
	return fmt.Sprintf("%d receipts, %d preparations, %d transcript turns, %d audit chains of %d events in all, %d of %d verify",
		len(s.Receipts), len(s.Preparations), s.Turns, len(s.Chains), events, ok, len(s.Chains))
}

func (s snapshot) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d receipts, %d preparations, %d transcript turns", len(s.Receipts), len(s.Preparations), s.Turns)
	for _, c := range s.Chains {
		fmt.Fprintf(&b, "; chain %s: %d events head %.12s ok=%v", c.Chain, c.Events, c.HeadHash, c.OK)
	}
	return b.String()
}
