package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
)

// Markers that must never reach an operator's terminal: the stored scope of a preparation, its
// confirmation binding, the prepared action, and what the application returned in a receipt.
const (
	opsInspectScopeMarker   = "SCOPEMARKER7f3a"
	opsInspectMACMarker     = "MACMARKER9c21"
	opsInspectPreparedMark  = "PREPAREDMARKER51d0"
	opsInspectRecordMarker  = "RECORDMARKER88be"
	opsInspectSecretPayload = "PAYLOADSECRET42ee"
)

// opsInspectRun runs one command with an isolated configuration registry, and checks what the
// commands that copy a database promise about the copy (SEC-081): it lives in a directory of the
// operating system's temporary area that only the owner can use (os.MkdirTemp creates 0700), and it
// is gone when the command ends, whether the command succeeded or failed. The temporary area is
// pointed at a directory of the test, so a leftover copy is seen and cannot be another test's.
func opsInspectRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	scratch := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, scratch)
	}
	var out, errOut bytes.Buffer
	env := &Env{
		Stdin:     strings.NewReader(""),
		Stdout:    &out,
		Stderr:    &errOut,
		LookupEnv: func(string) (string, bool) { return "", false },
		Config:    config.NewRegistry(),
	}
	code = Default.Run(context.Background(), args, env)
	if left, err := os.ReadDir(scratch); err != nil || len(left) != 0 {
		var names []string
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("%v (exit %d) left %v in the temporary directory (%v): the copy of the database must not outlive the command", args, code, names, err)
	}
	return code, out.String(), errOut.String()
}

// opsInspectDB opens (creating) the database file with the migrations of every module the binary
// links, as the service does.
func opsInspectDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// opsInspectDigest is the sha256 of the file and the names of everything beside it.
func opsInspectDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return hex.EncodeToString(sum[:]) + " " + strings.Join(names, ",")
}

// opsInspectSeedAudit writes n events into the chain of a tenant, and returns the database file.
func opsInspectSeedAudit(t *testing.T, db *store.DB, lg *audit.Log, app, tenant string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := lg.Append(context.Background(), storetest.Scope(app, tenant, "ws-1"),
			audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"n": i}}); err != nil {
			t.Fatalf("append #%d to %s/%s: %v", i, app, tenant, err)
		}
	}
}

func opsInspectLog(t *testing.T, db *store.DB) *audit.Log {
	t.Helper()
	lg, err := audit.New(db, audit.Options{Clock: clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)), Registry: audit.NewCoreRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	return lg
}

// opsInspectChains decodes a verify-chain report.
type opsInspectChainReport struct {
	Chains       int  `json:"chains"`
	ChainsOK     int  `json:"chains_ok"`
	ChainsFailed int  `json:"chains_failed"`
	Truncated    bool `json:"truncated"`
	Listed       []struct {
		ApplicationID string `json:"application_id"`
		TenantID      string `json:"tenant_id"`
		OK            bool   `json:"ok"`
		Events        int64  `json:"events"`
		Problems      []struct {
			Kind     string `json:"kind"`
			ChainSeq int64  `json:"chain_seq"`
		} `json:"problems"`
	} `json:"listed"`
}

func opsInspectDecode(t *testing.T, out string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("output is not the expected JSON: %v\n%s", err, out)
	}
}

// An operator verifies the chains of a database file with no service running: a clean file passes,
// a changed event is found at its position and fails the command, a filter selects chains, a
// selection that matches nothing is not a success, and the file the operator named is never touched.
func TestAuditVerifyChainDetectsATamperedChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alixai.db")
	db := opsInspectDB(t, path)
	lg := opsInspectLog(t, db)
	opsInspectSeedAudit(t, db, lg, "app-a", "tenant-1", 5)
	opsInspectSeedAudit(t, db, lg, "app-a", "tenant-2", 3)
	opsInspectSeedAudit(t, db, lg, "app-b", "tenant-1", 2)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	before := opsInspectDigest(t, path)
	code, out, errOut := opsInspectRun(t, "audit", "verify-chain", "-db", path)
	if code != ExitOK {
		t.Fatalf("verify-chain on a clean file = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	var rep opsInspectChainReport
	opsInspectDecode(t, out, &rep)
	if rep.Chains != 3 || rep.ChainsOK != 3 || rep.ChainsFailed != 0 {
		t.Fatalf("clean report = %+v", rep)
	}
	if after := opsInspectDigest(t, path); after != before {
		t.Fatalf("verify-chain changed the file or left something beside it:\nbefore %s\nafter  %s", before, after)
	}

	// Someone with write access to the file drops the append-only trigger and changes event 3 of one
	// chain. (The chain, not the trigger, is what must find it.)
	db = opsInspectDB(t, path)
	for _, stmt := range []string{
		`DROP TRIGGER audit_events_no_update`,
		`UPDATE audit_events SET data = '{"n":99}' WHERE application_id = 'app-a' AND tenant_id = 'tenant-1' AND chain_seq = 3`,
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	before = opsInspectDigest(t, path)
	code, out, errOut = opsInspectRun(t, "audit", "verify-chain", "-db", path)
	if code != ExitError {
		t.Fatalf("verify-chain on a tampered file = %d, want %d\nstdout: %s\nstderr: %s", code, ExitError, out, errOut)
	}
	opsInspectDecode(t, out, &rep)
	if rep.Chains != 3 || rep.ChainsOK != 2 || rep.ChainsFailed != 1 {
		t.Fatalf("tampered report = %+v", rep)
	}
	first := rep.Listed[0] // failed chains are listed first
	if first.OK || first.ApplicationID != "app-a" || first.TenantID != "tenant-1" || len(first.Problems) != 1 ||
		first.Problems[0].Kind != "hash_mismatch" || first.Problems[0].ChainSeq != 3 {
		t.Fatalf("the failing chain is reported as %+v, want a hash_mismatch at position 3 of app-a/tenant-1", first)
	}
	if !strings.Contains(errOut, "1 of 3 audit chains failed verification") {
		t.Errorf("stderr = %q", errOut)
	}
	if strings.Contains(out, `"n":99`) || strings.Contains(out, `"n": 99`) {
		t.Errorf("the report carries event data:\n%s", out)
	}
	if after := opsInspectDigest(t, path); after != before {
		t.Fatalf("verify-chain changed the tampered file:\nbefore %s\nafter  %s", before, after)
	}

	// A filter verifies only what it selects; a clean chain next to a broken one still passes.
	if code, out, errOut := opsInspectRun(t, "audit", "verify-chain", "-db", path, "-application", "app-a", "-tenant", "tenant-2"); code != ExitOK {
		t.Errorf("the intact chain of another tenant = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if code, _, errOut := opsInspectRun(t, "audit", "verify-chain", "-db", path, "-application", "app-a"); code != ExitError {
		t.Errorf("the application whose chain is broken = %d (%s), want a failure", code, errOut)
	}
	// Verifying nothing is not a success.
	if code, _, errOut := opsInspectRun(t, "audit", "verify-chain", "-db", path, "-application", "no-such-app"); code != ExitError || !strings.Contains(errOut, "nothing was verified") {
		t.Errorf("no chain matches = %d (%s), want a failure that says nothing was verified", code, errOut)
	}
	// -require-anchor: no checkpoint was ever signed in this file, so no chain is anchored.
	if code, _, _ := opsInspectRun(t, "audit", "verify-chain", "-db", path, "-application", "app-b", "-require-anchor"); code != ExitError {
		t.Errorf("-require-anchor on an unanchored chain = %d, want a failure", code)
	}
	if code, _, _ := opsInspectRun(t, "audit", "verify-chain", "-db", path, "-limit", "0"); code != ExitUsage {
		t.Errorf("-limit 0 = %d, want a usage error", code)
	}
}

// A path that is not a database is an error and creates nothing; the file the operator names is not
// opened for writing even when it cannot be written.
func TestAuditInspectNeverCreatesOrWritesTheFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nothing.db")
	for _, args := range [][]string{
		{"audit", "verify-chain", "-db", missing},
		{"audit", "list", "-db", missing, "-application", "app-a"},
		{"receipts", "list", "-db", missing, "-application", "app-a"},
	} {
		if code, _, errOut := opsInspectRun(t, args...); code != ExitError || !strings.Contains(errOut, "there is no database at") {
			t.Errorf("%v on a missing file = %d (%s)", args, code, errOut)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("a command created %s", missing)
	}
	notDB := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notDB, []byte("this is not a database, it is a text file of some length"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := opsInspectRun(t, "audit", "verify-chain", "-db", notDB); code != ExitError || !strings.Contains(errOut, "not a SQLite database") {
		t.Errorf("a text file = %d (%s)", code, errOut)
	}

	// A database that cannot be written (a restored backup on read-only media).
	path := filepath.Join(dir, "alixai.db")
	db := opsInspectDB(t, path)
	opsInspectSeedAudit(t, db, opsInspectLog(t, db), "app-a", "tenant-1", 2)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	before := opsInspectDigest(t, path)
	if code, out, errOut := opsInspectRun(t, "audit", "verify-chain", "-db", path); code != ExitOK {
		t.Fatalf("verify-chain on a read-only file = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if after := opsInspectDigest(t, path); after != before {
		t.Fatalf("the file changed:\nbefore %s\nafter  %s", before, after)
	}
}

func opsInspectInsertPreparation(t *testing.T, db *store.DB, id, app, tenant, ws, state string) {
	t.Helper()
	scopeJSON := fmt.Sprintf(`{"deployment_id":"dep","application_id":%q,"tenant_id":%q,"workspace_id":%q,"principal_id":"usr_a","roles":["member"],"capability_scopes":["%s"],"session_id":"ses_a"}`,
		app, tenant, ws, opsInspectScopeMarker)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO action_preparations (id, application_id, tenant_id, workspace_id, principal_id, session_id, scope_json, capability_id, state,
		   risk_level, confirmation_mode, confirmation_mac, idempotency_key, fields_json, prepared_json, snapshot_version, data_digest,
		   prepared_at, expires_at, updated_at)
		 VALUES (?, ?, ?, ?, 'usr_a', 'ses_a', ?, 'items.create', ?, 2, 'explicit', ?, ?, '{}', ?, 'v0', ?, 1000, 2000, 1000)`,
		id, app, tenant, ws, scopeJSON, state, opsInspectMACMarker, "idem-"+id, `{"summary":"`+opsInspectPreparedMark+`"}`, strings.Repeat("d", 64)); err != nil {
		t.Fatalf("insert preparation %s: %v", id, err)
	}
}

func opsInspectInsertReceipt(t *testing.T, db *store.DB, id, prep, app, tenant, ws, status string, issued int64) {
	t.Helper()
	errs := `[]`
	if status == "uncertain" {
		errs = `[{"code":"uncertain_outcome","message":"the outcome is unknown"}]`
	}
	body := fmt.Sprintf(`{"receipt_id":%q,"preparation_id":%q,"capability_id":"items.create","status":%q,"records":[{"entity_type":"item","entity_id":"%s"}],"facts":[],"errors":%s}`,
		id, prep, status, opsInspectRecordMarker, errs)
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO action_receipts (id, application_id, tenant_id, workspace_id, preparation_id, status, receipt_json, issued_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, app, tenant, ws, prep, status, body, issued); err != nil {
		t.Fatalf("insert receipt %s: %v", id, err)
	}
}

type opsInspectReceiptPage struct {
	Receipts []struct {
		ReceiptID        string   `json:"receipt_id"`
		PreparationID    string   `json:"preparation_id"`
		ApplicationID    string   `json:"application_id"`
		TenantID         string   `json:"tenant_id"`
		Status           string   `json:"status"`
		CapabilityID     string   `json:"capability_id"`
		PreparationState string   `json:"preparation_state"`
		ErrorCodes       []string `json:"error_codes"`
	} `json:"receipts"`
	Count     int   `json:"count"`
	More      bool  `json:"more"`
	NextAfter int64 `json:"next_after"`
}

func (p opsInspectReceiptPage) ids() string {
	var ids []string
	for _, r := range p.Receipts {
		ids = append(ids, r.ReceiptID)
	}
	return strings.Join(ids, ",")
}

// receipts list reads a database file without changing it, lists only the application (and tenant,
// workspace) asked for, in bounded pages, and prints the identifiers, status and capability of a
// receipt and never the stored scope, the confirmation binding, the prepared action or what the
// application returned.
func TestReceiptsListIsReadOnlyAndScopeFiltered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alixai.db")
	db := opsInspectDB(t, path)
	rows := []struct{ rcpt, prep, app, tenant, ws, status string }{
		{"rcpt_01", "prep_01", "app-a", "t1", "w1", "committed"},
		{"rcpt_02", "prep_02", "app-a", "t1", "w1", "uncertain"},
		{"rcpt_03", "prep_02", "app-a", "t1", "w1", "committed"}, // the reconciled one
		{"rcpt_04", "prep_03", "app-a", "t1", "w2", "rejected"},
		{"rcpt_05", "prep_04", "app-a", "t2", "w1", "committed"},
		{"rcpt_06", "prep_05", "app-b", "t1", "w1", "committed"},
	}
	opsInspectInsertPreparation(t, db, "prep_01", "app-a", "t1", "w1", "committed")
	opsInspectInsertPreparation(t, db, "prep_02", "app-a", "t1", "w1", "committed")
	opsInspectInsertPreparation(t, db, "prep_03", "app-a", "t1", "w2", "rejected")
	opsInspectInsertPreparation(t, db, "prep_04", "app-a", "t2", "w1", "committed")
	opsInspectInsertPreparation(t, db, "prep_05", "app-b", "t1", "w1", "committed")
	for i, r := range rows {
		opsInspectInsertReceipt(t, db, r.rcpt, r.prep, r.app, r.tenant, r.ws, r.status, int64(1_700_000_000_000+i*1000))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// What an operator might leave behind it: the file is not theirs to change.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	before := opsInspectDigest(t, path)

	list := func(args ...string) (opsInspectReceiptPage, string, int, string) {
		t.Helper()
		code, out, errOut := opsInspectRun(t, append([]string{"receipts", "list", "-db", path}, args...)...)
		var page opsInspectReceiptPage
		if code == ExitOK {
			opsInspectDecode(t, out, &page)
		}
		for _, marker := range []string{opsInspectScopeMarker, opsInspectMACMarker, opsInspectPreparedMark, opsInspectRecordMarker, "confirmation", "scope_json", "prepared_json"} {
			if strings.Contains(out, marker) {
				t.Errorf("receipts list %v prints %q:\n%s", args, marker, out)
			}
		}
		return page, out, code, errOut
	}

	// One tenant of one application: nothing of another tenant or application.
	page, out, code, errOut := list("-application", "app-a", "-tenant", "t1")
	if code != ExitOK {
		t.Fatalf("receipts list = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if got, want := page.ids(), "rcpt_01,rcpt_02,rcpt_03,rcpt_04"; got != want || page.More {
		t.Fatalf("app-a/t1 lists %s (more %v), want %s", got, page.More, want)
	}
	byID := map[string]int{}
	for i, r := range page.Receipts {
		byID[r.ReceiptID] = i
	}
	unc := page.Receipts[byID["rcpt_02"]]
	if unc.Status != "uncertain" || unc.CapabilityID != "items.create" || len(unc.ErrorCodes) != 1 || unc.ErrorCodes[0] != "uncertain_outcome" || unc.PreparationState != "committed" {
		t.Errorf("the uncertain receipt is listed as %+v", unc)
	}
	// A workspace narrows further; a tenant of the same application is another listing.
	if page, _, _, _ := list("-application", "app-a", "-tenant", "t1", "-workspace", "w2"); page.ids() != "rcpt_04" {
		t.Errorf("workspace w2 lists %s", page.ids())
	}
	if page, _, _, _ := list("-application", "app-a"); page.ids() != "rcpt_01,rcpt_02,rcpt_03,rcpt_04,rcpt_05" {
		t.Errorf("app-a lists %s: another application's receipt must never appear", page.ids())
	}
	if page, _, _, _ := list("-application", "app-b"); page.ids() != "rcpt_06" {
		t.Errorf("app-b lists %s", page.ids())
	}
	if page, _, _, _ := list("-application", "app-a", "-preparation", "prep_02"); page.ids() != "rcpt_02,rcpt_03" {
		t.Errorf("preparation prep_02 lists %s, want its two receipts", page.ids())
	}
	if page, _, _, _ := list("-application", "app-a", "-status", "uncertain"); page.ids() != "rcpt_02" {
		t.Errorf("status uncertain lists %s", page.ids())
	}
	if page, _, _, _ := list("-application", "no-such-app"); page.Count != 0 || page.ids() != "" {
		t.Errorf("an unknown application lists %+v", page)
	}

	// No scope, no listing; bad filters are usage errors.
	for _, args := range [][]string{{}, {"-tenant", "t1"}, {"-application", "app-a", "-status", "done"}, {"-application", "app-a", "-limit", "0"}, {"-application", "app-a", "-limit", "100000"}, {"-application", "app-a", "-after", "-1"}} {
		if code, out, _ := opsInspectRun(t, append([]string{"receipts", "list", "-db", path}, args...)...); code != ExitUsage || strings.Contains(out, "rcpt_") {
			t.Errorf("receipts list %v = %d, want a usage error and no receipt", args, code)
		}
	}

	// Pages are bounded and chain by their cursor.
	p1, _, _, _ := list("-application", "app-a", "-limit", "2")
	if p1.ids() != "rcpt_01,rcpt_02" || !p1.More || p1.NextAfter == 0 {
		t.Fatalf("first page = %s more %v next %d", p1.ids(), p1.More, p1.NextAfter)
	}
	p2, _, _, _ := list("-application", "app-a", "-limit", "2", "-after", fmt.Sprint(p1.NextAfter))
	p3, _, _, _ := list("-application", "app-a", "-limit", "2", "-after", fmt.Sprint(p2.NextAfter))
	if p2.ids() != "rcpt_03,rcpt_04" || !p2.More || p3.ids() != "rcpt_05" || p3.More {
		t.Fatalf("pages = %s (more %v), %s (more %v)", p2.ids(), p2.More, p3.ids(), p3.More)
	}

	if after := opsInspectDigest(t, path); after != before {
		t.Fatalf("receipts list changed the file or left something beside it:\nbefore %s\nafter  %s", before, after)
	}
}

type opsInspectEventPage struct {
	Events []struct {
		Seq         int64           `json:"seq"`
		TenantID    string          `json:"tenant_id"`
		Type        string          `json:"type"`
		Data        json.RawMessage `json:"data"`
		DataOmitted string          `json:"data_omitted"`
	} `json:"events"`
	More      bool  `json:"more"`
	NextAfter int64 `json:"next_after"`
}

// audit list is bounded and scope-filtered like receipts list, and prints a payload only on request,
// redacted again and never a large one.
func TestAuditListIsBoundedScopeFilteredAndRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alixai.db")
	db := opsInspectDB(t, path)
	lg := opsInspectLog(t, db)
	opsInspectSeedAudit(t, db, lg, "app-a", "tenant-1", 4)
	opsInspectSeedAudit(t, db, lg, "app-a", "tenant-2", 2)
	opsInspectSeedAudit(t, db, lg, "app-b", "tenant-1", 2)
	// Rows written behind the log's back, with a payload the log would have redacted and one it would
	// have refused for its size: the command must not print either.
	big := `{"note":"` + strings.Repeat("x", opsInspectMaxData) + `"}`
	for i, data := range []string{`{"token":"` + opsInspectSecretPayload + `","note":"visible"}`, big} {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO audit_events (id, application_id, tenant_id, workspace_id, chain_seq, event_type, occurred_at, principal_id, session_id, data, prev_hash, hash)
			 VALUES (?, 'app-c', 'tenant-1', 'ws-1', ?, 'settings.changed', 1000, 'usr_a', 'ses_a', ?, ?, ?)`,
			fmt.Sprintf("aud_0000000000000000000000000%d", i), i+1, data, strings.Repeat(fmt.Sprint(i+1), 64), strings.Repeat(fmt.Sprint(i+3), 64)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := opsInspectDigest(t, path)

	list := func(args ...string) (opsInspectEventPage, string) {
		t.Helper()
		code, out, errOut := opsInspectRun(t, append([]string{"audit", "list", "-db", path}, args...)...)
		if code != ExitOK {
			t.Fatalf("audit list %v = %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
		}
		var page opsInspectEventPage
		opsInspectDecode(t, out, &page)
		return page, out
	}
	if page, _ := list("-application", "app-a", "-tenant", "tenant-2"); len(page.Events) != 2 || page.Events[0].TenantID != "tenant-2" {
		t.Errorf("app-a/tenant-2 lists %+v", page.Events)
	}
	if page, _ := list("-application", "app-a"); len(page.Events) != 6 {
		t.Errorf("app-a lists %d events, want its 6 and none of app-b", len(page.Events))
	}
	if page, _ := list("-application", "app-a", "-type", "no.such.type"); len(page.Events) != 0 {
		t.Errorf("an unknown type lists %d events", len(page.Events))
	}
	p1, _ := list("-application", "app-a", "-limit", "4")
	p2, _ := list("-application", "app-a", "-limit", "4", "-after", fmt.Sprint(p1.NextAfter))
	if len(p1.Events) != 4 || !p1.More || len(p2.Events) != 2 || p2.More || p2.Events[0].Seq <= p1.Events[3].Seq {
		t.Errorf("pages: %d events more %v, then %d events more %v", len(p1.Events), p1.More, len(p2.Events), p2.More)
	}
	if _, out := list("-application", "app-a", "-data"); !strings.Contains(out, `"n": 1`) {
		t.Errorf("-data does not print the payload:\n%s", out)
	}
	if page, out := list("-application", "app-a"); strings.Contains(out, `"n": 1`) || len(page.Events[0].Data) != 0 {
		t.Errorf("a payload is printed without -data:\n%s", out)
	}
	page, out := list("-application", "app-c", "-data")
	if strings.Contains(out, opsInspectSecretPayload) {
		t.Errorf("a sensitive payload value reached the terminal:\n%s", out)
	}
	if len(page.Events) != 2 || !strings.Contains(string(page.Events[0].Data), "visible") || page.Events[1].DataOmitted == "" || len(page.Events[1].Data) != 0 {
		t.Errorf("app-c events = %+v", page.Events)
	}
	if code, _, _ := opsInspectRun(t, "audit", "list", "-db", path); code != ExitUsage {
		t.Errorf("audit list without -application = %d, want a usage error", code)
	}
	if after := opsInspectDigest(t, path); after != before {
		t.Fatalf("audit list changed the file:\nbefore %s\nafter  %s", before, after)
	}
}

// G6 / SEC-086: an operator exports the signed head of a chain, keeps it outside the database, and
// verify-chain -expected-head holds the chain to it. A tail truncation that also removes the stored
// checkpoints leaves a chain that is internally consistent: only the held head sees it.
func TestVerifyChainExpectedHead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alixai.db")
	db := opsInspectDB(t, path)
	clk := clock.Real{}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.NewStore(db, clk).RegisterKey(ctx, trust.PlatformApplicationID, "hardening-test-key", pub, "test", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
		t.Fatal(err)
	}
	lg, err := audit.New(db, audit.Options{
		Clock: clk, Registry: audit.NewCoreRegistry(),
		Signing:  &trust.SigningKey{KeyID: "hardening-test-key", Key: priv},
		Verifier: trust.NewVerifier(trust.NewStore(db, clk), clk),
	})
	if err != nil {
		t.Fatal(err)
	}
	opsInspectSeedAudit(t, db, lg, "app-a", "tenant-1", 8)
	opsInspectSeedAudit(t, db, lg, "app-a", "tenant-2", 3)
	if _, err := lg.Checkpoint(ctx, audit.ChainKey{ApplicationID: "app-a", TenantID: "tenant-1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// A chain with no stored checkpoint cannot be exported.
	if code, _, _ := opsInspectRun(t, "audit", "export-checkpoint", "-db", path, "-application", "app-a", "-tenant", "tenant-2"); code != ExitError {
		t.Errorf("export of a chain without a checkpoint = %d, want a failure", code)
	}
	if code, _, _ := opsInspectRun(t, "audit", "export-checkpoint", "-db", path, "-application", "app-a"); code != ExitUsage {
		t.Errorf("export without -tenant = %d, want a usage error", code)
	}
	held := filepath.Join(t.TempDir(), "held-head.json")
	code, out, errOut := opsInspectRun(t, "audit", "export-checkpoint", "-db", path, "-application", "app-a", "-tenant", "tenant-1", "-out", held)
	if code != ExitOK {
		t.Fatalf("export-checkpoint = %d\n%s\n%s", code, out, errOut)
	}
	if code, _, _ := opsInspectRun(t, "audit", "export-checkpoint", "-db", path, "-application", "app-a", "-tenant", "tenant-1", "-out", held); code != ExitError {
		t.Errorf("export over an existing file = %d, want a refusal", code)
	}
	if code, std, _ := opsInspectRun(t, "audit", "export-checkpoint", "-db", path, "-application", "app-a", "-tenant", "tenant-1"); code != ExitOK || !strings.Contains(std, `"head_seq": 8`) {
		t.Errorf("export to standard output = %d\n%s", code, std)
	}

	verify := func(extra ...string) (int, opsInspectChainReport, string) {
		t.Helper()
		code, out, errOut := opsInspectRun(t, append([]string{"audit", "verify-chain", "-db", path}, extra...)...)
		var rep opsInspectChainReport
		if out != "" {
			opsInspectDecode(t, out, &rep)
		}
		return code, rep, out + errOut
	}

	// The intact chain, before any attack, extends its held head.
	if code, _, all := verify("-application", "app-a", "-tenant", "tenant-1", "-expected-head", held); code != ExitOK || !strings.Contains(all, `"checked": true`) {
		t.Fatalf("an intact chain against its held head = %d\n%s", code, all)
	}

	// The attack: the newest three events and every stored checkpoint are removed.
	db = opsInspectDB(t, path)
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'trigger' AND tbl_name LIKE 'audit\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		triggers = append(triggers, n)
	}
	_ = rows.Close()
	if len(triggers) == 0 {
		t.Fatal("no append-only triggers: the setup of the attack is stale")
	}
	for _, n := range triggers {
		if _, err := db.ExecContext(ctx, `DROP TRIGGER `+n); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`DELETE FROM audit_checkpoints`,
		`DELETE FROM audit_events WHERE application_id = 'app-a' AND tenant_id = 'tenant-1' AND chain_seq > 5`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Control: on its own the truncated chain looks intact. This is the attack.
	if code, _, all := verify("-application", "app-a", "-tenant", "tenant-1"); code != ExitOK {
		t.Fatalf("the truncated chain should look intact without a held head (control) = %d\n%s", code, all)
	}
	// With the held head it is a failure, reported as a truncation.
	code, rep, all := verify("-application", "app-a", "-tenant", "tenant-1", "-expected-head", held)
	if code != ExitError || rep.ChainsFailed != 1 || len(rep.Listed) == 0 || len(rep.Listed[0].Problems) == 0 || rep.Listed[0].Problems[0].Kind != string(audit.ProblemTruncated) {
		t.Fatalf("the truncated chain against its held head = %d, want a truncated problem\n%s", code, all)
	}
	// The head only holds the chain it names: another chain is not failed by it.
	if code, _, all := verify("-application", "app-a", "-tenant", "tenant-2", "-expected-head", held); code != ExitError || !strings.Contains(all, "was not among the chains verified") {
		t.Errorf("a selection that leaves the held chain out = %d, want a refusal\n%s", code, all)
	}

	// A held head that cannot be read, is not a checkpoint, or does not verify is never a pass.
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var forged map[string]any
	raw, err := os.ReadFile(held)
	if err != nil {
		t.Fatal(err)
	}
	opsInspectDecode(t, string(raw), &forged)
	forged["checkpoint"].(map[string]any)["head_seq"] = 2 // the signature no longer covers the body
	forgedJSON, _ := json.Marshal(forged)
	for name, p := range map[string]string{
		"a missing file":          filepath.Join(dir, "absent.json"),
		"garbage":                 write("garbage.json", "not json"),
		"an empty document":       write("empty.json", "{}"),
		"an unknown member":       write("extra.json", strings.Replace(string(raw), `"checkpoint"`, `"extra": 1, "checkpoint"`, 1)),
		"a head that was altered": write("forged.json", string(forgedJSON)),
		"two documents in one":    write("two.json", string(raw)+string(raw)),
	} {
		if code, _, all := verify("-expected-head", p); code == ExitOK {
			t.Errorf("%s was accepted as a held head:\n%s", name, all)
		}
	}
}

// The chains with no tenant (an application's own: package lifecycle, integration-audit runs; the
// platform's) are held to an exported head too, with -application-chain: a truncation that also
// removes the stored checkpoints is seen only through the head held outside the database.
func TestExportCheckpointApplicationChain(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alixai.db")
	db := opsInspectDB(t, path)
	clk := clock.Real{}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.NewStore(db, clk).RegisterKey(ctx, trust.PlatformApplicationID, "hardening-test-key", pub, "test", trust.WithPurpose(trust.PurposeAuditCheckpoint)); err != nil {
		t.Fatal(err)
	}
	lg, err := audit.New(db, audit.Options{
		Clock: clk, Registry: audit.NewCoreRegistry(),
		Signing:  &trust.SigningKey{KeyID: "hardening-test-key", Key: priv},
		Verifier: trust.NewVerifier(trust.NewStore(db, clk), clk),
	})
	if err != nil {
		t.Fatal(err)
	}
	appLoc, err := audit.ForApplication("app-a", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		for _, loc := range []audit.Locator{appLoc, audit.Platform()} {
			if _, err := lg.AppendAt(ctx, loc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"n": i}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	opsInspectSeedAudit(t, db, lg, "app-a", "tenant-1", 3)
	for _, key := range []audit.ChainKey{{ApplicationID: "app-a"}, {ApplicationID: trust.PlatformApplicationID}} {
		if _, err := lg.Checkpoint(ctx, key); err != nil {
			t.Fatalf("checkpoint %s: %v", key, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	export := func(args ...string) (int, string) {
		t.Helper()
		code, out, errOut := opsInspectRun(t, append([]string{"audit", "export-checkpoint", "-db", path}, args...)...)
		return code, out + errOut
	}
	// The flag names the chain with no tenant: it replaces -tenant and is never combined with it.
	if code, all := export("-application", "app-a", "-tenant", "tenant-1", "-application-chain"); code != ExitUsage {
		t.Errorf("-tenant with -application-chain = %d, want a usage error\n%s", code, all)
	}
	if code, all := export("-application-chain"); code != ExitUsage {
		t.Errorf("-application-chain without -application = %d, want a usage error\n%s", code, all)
	}
	held := filepath.Join(t.TempDir(), "held-app-head.json")
	if code, all := export("-application", "app-a", "-application-chain", "-out", held); code != ExitOK {
		t.Fatalf("export of the application chain = %d\n%s", code, all)
	}
	raw, err := os.ReadFile(held)
	if err != nil {
		t.Fatal(err)
	}
	var cp audit.Checkpoint
	opsInspectDecode(t, string(raw), &cp)
	if cp.Checkpoint.ApplicationID != "app-a" || cp.Checkpoint.TenantID != "" || cp.Checkpoint.HeadSeq != 6 {
		t.Fatalf("the held head names %+v, want the application chain of app-a at 6", cp.Checkpoint)
	}
	if code, all := export("-application", trust.PlatformApplicationID, "-application-chain"); code != ExitOK || !strings.Contains(all, `"head_seq": 6`) {
		t.Errorf("export of the platform chain = %d\n%s", code, all)
	}

	verify := func(extra ...string) (int, opsInspectChainReport, string) {
		t.Helper()
		code, out, errOut := opsInspectRun(t, append([]string{"audit", "verify-chain", "-db", path}, extra...)...)
		var rep opsInspectChainReport
		if out != "" {
			opsInspectDecode(t, out, &rep)
		}
		return code, rep, out + errOut
	}
	// -application-chain selects the chains with no tenant, and only those.
	if code, rep, all := verify("-application", "app-a", "-application-chain"); code != ExitOK || rep.Chains != 1 || rep.Listed[0].TenantID != "" {
		t.Fatalf("verify-chain -application-chain = %d\n%s", code, all)
	}
	if code, rep, all := verify("-application-chain"); code != ExitOK || rep.Chains != 2 {
		t.Fatalf("verify-chain -application-chain over every application = %d, want the 2 chains with no tenant\n%s", code, all)
	}
	if code, _, all := verify("-application-chain", "-tenant", "tenant-1"); code != ExitUsage {
		t.Errorf("-tenant with -application-chain = %d, want a usage error\n%s", code, all)
	}
	// A held head of a chain with no tenant is read only when the flag says that is what it is.
	if code, _, all := verify("-application", "app-a", "-expected-head", held); code != ExitError || !strings.Contains(all, "-application-chain") {
		t.Errorf("a held application head without -application-chain = %d, want a refusal naming the flag\n%s", code, all)
	}
	if code, _, all := verify("-application", "app-a", "-application-chain", "-expected-head", held); code != ExitOK || !strings.Contains(all, `"checked": true`) {
		t.Fatalf("the intact application chain against its held head = %d\n%s", code, all)
	}

	// The attack: the newest events of the application chain and every stored checkpoint are removed.
	db = opsInspectDB(t, path)
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'trigger' AND tbl_name LIKE 'audit\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		triggers = append(triggers, n)
	}
	_ = rows.Close()
	for _, n := range triggers {
		if _, err := db.ExecContext(ctx, `DROP TRIGGER `+n); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`DELETE FROM audit_checkpoints`,
		`DELETE FROM audit_events WHERE application_id = 'app-a' AND tenant_id = '' AND chain_seq > 4`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Control: on its own the truncated chain looks intact.
	if code, _, all := verify("-application", "app-a", "-application-chain"); code != ExitOK {
		t.Fatalf("the truncated application chain should look intact without a held head (control) = %d\n%s", code, all)
	}
	code, rep, all := verify("-application", "app-a", "-application-chain", "-expected-head", held)
	if code != ExitError || rep.ChainsFailed != 1 || len(rep.Listed) == 0 || len(rep.Listed[0].Problems) == 0 || rep.Listed[0].Problems[0].Kind != string(audit.ProblemTruncated) {
		t.Fatalf("the truncated application chain against its held head = %d, want a truncated problem\n%s", code, all)
	}
}
