package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The synthetic deployment the tests run against. Nothing here is a real key, account or URL.
const (
	tDeploy  = "dep_test"
	tApp     = "app_crm"
	tOther   = "app_other"
	tTenant  = "ten_42"
	tTenant2 = "ten_43"
	tWsp     = "wsp_main"

	// tKey and tCanary are TEST-ONLY canaries that stand for provider keys (not real keys). They must
	// never appear in a log line, an error, a JSON document, the database or the encrypted file.
	tKey    = "TESTONLY-canary-key-7f3a91c2d4e6b805"
	tKey2   = "TESTONLY-canary-key-second-0b1c2d3e4f"
	tCanary = "TESTONLY-canary-leak-9d8c7b6a5f4e3d2c"
)

var bg = context.Background()

var tStart = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// fakeValidator is a scripted provider validation endpoint that records what it is asked.
type fakeValidator struct {
	mu     sync.Mutex
	calls  []ValidationRequest
	res    ValidationResult
	err    error
	hook   func(ValidationRequest)
	deadln []time.Time
}

func (f *fakeValidator) ValidateKey(ctx context.Context, req ValidationRequest) (ValidationResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	if d, ok := ctx.Deadline(); ok {
		f.deadln = append(f.deadln, d)
	}
	hook, res, err := f.hook, f.res, f.err
	f.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	return res, err
}

func (f *fakeValidator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// spyBackend wraps a backend and records the slots it was asked to read.
type spyBackend struct {
	Backend
	mu   sync.Mutex
	gets []string
}

func (s *spyBackend) Get(ctx context.Context, slot Slot) (Secret, error) {
	s.mu.Lock()
	s.gets = append(s.gets, slot.Ref)
	s.mu.Unlock()
	return s.Backend.Get(ctx, slot)
}

// failAudit is an audit log that cannot append.
type failAudit struct {
	audit.Appender
	failAll bool
}

func (f *failAudit) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	if f.failAll {
		return audit.Event{}, errors.New("audit down: " + tCanary)
	}
	return f.Appender.AppendAt(ctx, loc, ev)
}

type rig struct {
	t      *testing.T
	db     *store.DB
	clk    *clock.Fake
	log    *audit.Log
	svc    *Service
	val    *fakeValidator
	enc    *EncryptedFile
	dir    string // the encrypted file's directory
	keyDir string
	logbuf *bytes.Buffer
	scope  contracts.Scope
}

func scopeOf(app, tenant string) contracts.Scope {
	s := storetest.Scope(app, tenant, tWsp)
	s.DeploymentID = tDeploy
	return s
}

func newRig(t *testing.T, mutate ...func(*Config)) *rig {
	t.Helper()
	g := &rig{t: t, clk: clock.NewFake(tStart), logbuf: &bytes.Buffer{}, scope: scopeOf(tApp, tTenant)}
	g.db = storetest.Open(t, nil)
	var err error
	if g.log, err = audit.New(g.db, audit.Options{Clock: g.clk}); err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	root := t.TempDir()
	g.dir = filepath.Join(root, "secrets")
	g.keyDir = filepath.Join(root, "keys")
	if g.enc, err = NewEncryptedFile(filepath.Join(g.dir, FileName), KeyFileSource{Path: filepath.Join(g.keyDir, "master.key")}); err != nil {
		t.Fatal(err)
	}
	g.val = &fakeValidator{res: ValidationResult{Accepted: true, Account: providers.SafeAccount{AccountID: ptr("acct_123")}}}
	cfg := Config{
		DB: g.db, Audit: g.log, DeploymentID: tDeploy, Clock: g.clk, Validator: g.val,
		Backends: []Backend{g.enc}, Default: BackendEncryptedFile,
		Logger: slog.New(slog.NewJSONHandler(g.logbuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	if g.svc, err = New(cfg); err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func ptr[T any](v T) *T { return &v }

func (g *rig) addKey(key string, mutate ...func(*AddAPIKeyRequest)) providers.CredentialRef {
	g.t.Helper()
	req := AddAPIKeyRequest{ProviderID: "fake", Label: "ops key", Key: Secret(key)}
	for _, m := range mutate {
		m(&req)
	}
	ref, err := g.svc.AddAPIKey(bg, g.scope, req)
	if err != nil {
		g.t.Fatalf("AddAPIKey: %v", err)
	}
	return ref
}

func (g *rig) use() Use { return Use{Provider: "fake", Purpose: "inference"} }

// resolve resolves ref in the rig's scope for the fake provider.
func (g *rig) resolve(ref providers.CredentialRefID) (Secret, error) {
	return g.svc.Resolve(bg, g.scope, string(ref), g.use())
}

// auditRows returns the data of the audit events of a type in the chain of (app, tenant).
func (g *rig) auditRows(typ audit.EventType, app, tenant string) []map[string]any {
	g.t.Helper()
	rows, err := g.db.QueryContext(bg, "SELECT data FROM audit_events WHERE event_type = ? AND application_id = ? AND tenant_id = ? ORDER BY seq", string(typ), app, tenant)
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			g.t.Fatal(err)
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(d), &m); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// mustCode fails the test unless err is a typed error with the given code and reason.
func mustCode(t *testing.T, err error, code contracts.ErrCode, reason Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s/%s, got no error", code, reason)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("code = %s, want %s (%v)", got, code, err)
	}
	if got := ReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q (%v)", got, reason, err)
	}
}

// fileBytes returns the bytes of every file under dir (and the database files), for canary scans.
func allBytes(t *testing.T, paths ...string) []byte {
	t.Helper()
	var out []byte
	for _, root := range paths {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, rerr := os.ReadFile(p); rerr == nil {
					out = append(out, b...)
				}
			}
			return nil
		})
	}
	return out
}

// ---- schema validation of every reference the service returns ------------------------------------

type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema reference %q is not one of the loaded schemas", url)
}

var (
	schemaOnce sync.Once
	schemaComp *jsonschema.Compiler
	schemaErr  error
)

func refSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(refuseLoader{})
		for _, dir := range []string{"../../schemas/common", "../../schemas/providers"} {
			files, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
			if err != nil || len(files) == 0 {
				schemaErr = fmt.Errorf("no schemas under %s", dir)
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					schemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					schemaErr = err
					return
				}
				id, _ := doc.(map[string]any)["$id"].(string)
				if err := c.AddResource(id, doc); err != nil {
					schemaErr = err
					return
				}
			}
		}
		schemaComp = c
	})
	if schemaErr != nil {
		t.Fatal(schemaErr)
	}
	s, err := schemaComp.Compile("urn:alixai:schema:providers:credential-ref:v1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// assertSchemaValid validates the JSON of a reference against schemas/providers/credential-ref, whose
// allOf rules check every field combination (a quarantine says why, an active key was validated, ...).
func assertSchemaValid(t testing.TB, ref providers.CredentialRef) {
	t.Helper()
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := refSchema(t).Validate(doc); err != nil {
		t.Fatalf("reference violates its schema: %v\n%s", err, raw)
	}
}

func notContains(t testing.TB, what string, hay []byte, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(string(hay), n) {
			t.Errorf("%s contains the canary %q", what, n)
		}
	}
}
