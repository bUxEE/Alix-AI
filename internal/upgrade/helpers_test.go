package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/backup"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
)

const (
	releaseKeyID = "alixai-release-test"
	backupKeyID  = "backup-test"
)

const ledgerV1 = `
CREATE TABLE ledger (
    n              INTEGER PRIMARY KEY,
    application_id TEXT NOT NULL DEFAULT 'a',
    tenant_id      TEXT NOT NULL DEFAULT 't',
    workspace_id   TEXT NOT NULL DEFAULT 'w',
    body           TEXT NOT NULL
) STRICT;
`

const ledgerV2 = `
CREATE TABLE ledger_extra (
    n              INTEGER PRIMARY KEY,
    application_id TEXT NOT NULL DEFAULT 'a',
    tenant_id      TEXT NOT NULL DEFAULT 't',
    workspace_id   TEXT NOT NULL DEFAULT 'w'
) STRICT;
INSERT INTO ledger_extra (n) SELECT n FROM ledger;
`

// notesMigration is an external-content FTS5 index kept in step by triggers,
// the pattern internal/rag and internal/history use.
const notesMigration = `
CREATE TABLE notes (
    id             TEXT PRIMARY KEY,
    application_id TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    workspace_id   TEXT NOT NULL,
    body           TEXT NOT NULL
);

CREATE VIRTUAL TABLE notes_fts USING fts5 (
    body,
    application_id UNINDEXED, tenant_id UNINDEXED, workspace_id UNINDEXED,
    content = 'notes', content_rowid = 'rowid',
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER notes_ai AFTER INSERT ON notes BEGIN
    INSERT INTO notes_fts (rowid, body, application_id, tenant_id, workspace_id)
    VALUES (new.rowid, new.body, new.application_id, new.tenant_id, new.workspace_id);
END;
CREATE TRIGGER notes_ad AFTER DELETE ON notes BEGIN
    INSERT INTO notes_fts (notes_fts, rowid, body, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.rowid, old.body, old.application_id, old.tenant_id, old.workspace_id);
END;
CREATE TRIGGER notes_au AFTER UPDATE ON notes BEGIN
    INSERT INTO notes_fts (notes_fts, rowid, body, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.rowid, old.body, old.application_id, old.tenant_id, old.workspace_id);
    INSERT INTO notes_fts (rowid, body, application_id, tenant_id, workspace_id)
    VALUES (new.rowid, new.body, new.application_id, new.tenant_id, new.workspace_id);
END;
`

// registry builds a store registry from module -> file name -> SQL.
func registry(t *testing.T, modules map[string]map[string]string, order ...string) *store.Registry {
	t.Helper()
	reg := store.NewRegistry()
	for _, name := range order {
		m := fstest.MapFS{}
		for file, body := range modules[name] {
			m[store.MigrationsDir+"/"+file] = &fstest.MapFile{Data: []byte(body)}
		}
		if err := reg.Register(name, m); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	return reg
}

// regV1 is the installed schema: ledger 0001 and the notes index.
func regV1(t *testing.T) *store.Registry {
	return registry(t, map[string]map[string]string{
		"ledger": {"0001_init.sql": ledgerV1},
		"notes":  {"0001_init.sql": notesMigration},
	}, "ledger", "notes")
}

// regV2 is the new binary's: ledger gains 0002 (and, when v3 is not empty, 0003).
func regV2(t *testing.T, v3 string) *store.Registry {
	files := map[string]string{"0001_init.sql": ledgerV1, "0002_extra.sql": ledgerV2}
	if v3 != "" {
		files["0003_more.sql"] = v3
	}
	return registry(t, map[string]map[string]string{
		"ledger": files,
		"notes":  {"0001_init.sql": notesMigration},
	}, "ledger", "notes")
}

// fixture is a data directory holding an installed v1 database, with the keys
// and a runtime fake.
type fixture struct {
	t        *testing.T
	dataDir  string
	clk      *clock.Fake
	relPriv  ed25519.PrivateKey
	relKeys  release.StaticKeys
	bkSigner trust.SigningKey
	rt       *fakeRuntime
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dataDir: filepath.Join(t.TempDir(), "data"), clk: clock.NewFake(t0)}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.relPriv = priv
	f.relKeys = release.StaticKeys{Keys: map[string]ed25519.PublicKey{releaseKeyID: pub}}
	_, bpriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.bkSigner = trust.SigningKey{KeyID: backupKeyID, Key: bpriv}
	f.rt = &fakeRuntime{cur: RuntimeSet{Runtime: "localai", Version: "v3.5.0", Backends: map[string]string{"llama-cpp": "b1000"}}}

	// The installed database: audit and trust tables (store.Default), then the
	// ledger and notes modules of v1, with data, and an audit chain.
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(f.dataDir, store.DefaultFileName), Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.New(db, audit.Options{Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	sc := storetest.Scope("app1", "tenant1", "ws1")
	for i := 0; i < 4; i++ {
		f.clk.Advance(time.Second)
		if _, err := log.Append(ctx, sc, audit.NewEvent{Type: audit.EventSettingsChanged, Data: map[string]any{"n": i}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = f.openWith(regV1(t))
	for i := 1; i <= 10; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO ledger (n, body) VALUES (?, ?)`, i, "row"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) openWith(reg *store.Registry) *store.DB {
	f.t.Helper()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(f.dataDir, store.DefaultFileName), Migrations: reg, Clock: f.clk})
	if err != nil {
		f.t.Fatalf("store.Open: %v", err)
	}
	return db
}

// upgrader returns an Upgrader for the new binary's registry.
func (f *fixture) upgrader(reg *store.Registry, mutate ...func(*Config)) *Upgrader {
	f.t.Helper()
	cfg := Config{
		DataDir: f.dataDir, Migrations: reg, Clock: f.clk,
		ReleaseKeys: f.relKeys, BackupSigner: f.bkSigner, Runtime: f.rt, Version: "2.0.0",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	u, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// release builds and signs a release manifest in the shape tools/release writes.
func (f *fixture) release(version string, allowed []string, backendVersion string) []byte {
	return f.releaseWith(version, allowed, backendVersion, nil)
}

func (f *fixture) releaseWith(version string, allowed []string, backendVersion string, mutate func(signed map[string]any)) []byte {
	f.t.Helper()
	sum := sha256.Sum256([]byte("binary " + version))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	signed := map[string]any{
		"kind": "release-manifest", "application_id": "alixai.platform", "version": version,
		"commit": strings.Repeat("a", 40), "created_at": "2026-10-03T12:00:00Z", "hash_algorithm": "sha256",
		"artifacts": []any{map[string]any{"name": "alixai", "kind": "binary", "os": "linux", "arch": "amd64", "sha256": hex.EncodeToString(sum[:]), "size": 12}},
		"images":    []any{map[string]any{"name": "local-ai", "source": "third-party", "reference": "quay.io/go-skynet/local-ai:v3.6.0", "digest": digest}},
		"backends": []any{map[string]any{
			"runtime": "localai", "name": "llama-cpp", "alias": "llama-cpp", "version": backendVersion, "licence": "MIT",
			"reference": "quay.io/go-skynet/local-ai-backends", "digest": digest, "mirrors": []any{},
		}},
		"models": []any{map[string]any{
			"id": "granite-3b", "name": "Granite", "version": "1", "status": "candidate",
			"upstream": map[string]any{"repo_url": "https://example.invalid/m", "revision": "r1"},
			"artifact": map[string]any{"format": "gguf", "repo_url": "https://example.invalid/m", "filename": "m.gguf", "revision": "r1", "size_bytes": 1000, "sha256": hex.EncodeToString(sum[:]), "quantized_by": "x", "first_party": false},
			"licence":  map[string]any{"spdx": "Apache-2.0", "url": "https://example.invalid/l", "redistribution_notes": "ok"},
			"backend":  map[string]any{"runtime": "localai", "name": "llama-cpp", "allowed_runtime_versions": allowed},
		}},
	}
	if mutate != nil {
		mutate(signed)
	}
	return f.sign(signed, releaseKeyID, f.relPriv)
}

func (f *fixture) sign(signed map[string]any, keyID string, priv ed25519.PrivateKey) []byte {
	f.t.Helper()
	sig, err := trust.SignPayload(signed, keyID, priv, t0, t0.Add(365*24*time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := json.MarshalIndent(map[string]any{"format": ReleaseFormat, "signed": signed, "signatures": []any{sig}}, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}

// fakeRuntime is a Runtime that records what it is told.
type fakeRuntime struct {
	mu          sync.Mutex
	cur         RuntimeSet
	activations []RuntimeSet
	// drift, when set, is what Observe reports instead of cur: a backend that
	// changed behind the upgrade's back.
	drift *RuntimeSet
	// afterActivate, when set, returns the drift to apply after a set was activated.
	afterActivate func(rs RuntimeSet) *RuntimeSet
}

func (r *fakeRuntime) Observe(context.Context) (RuntimeSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drift != nil {
		return clone(*r.drift), nil
	}
	return clone(r.cur), nil
}

func (r *fakeRuntime) Activate(_ context.Context, rs RuntimeSet) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = clone(rs)
	r.activations = append(r.activations, clone(rs))
	r.drift = nil
	if r.afterActivate != nil {
		r.drift = r.afterActivate(rs)
	}
	return nil
}

func (r *fakeRuntime) current() RuntimeSet {
	r.mu.Lock()
	defer r.mu.Unlock()
	return clone(r.cur)
}

func (r *fakeRuntime) activationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.activations)
}

func clone(rs RuntimeSet) RuntimeSet {
	out := rs
	out.Backends = map[string]string{}
	for k, v := range rs.Backends {
		out.Backends[k] = v
	}
	return out
}

var chain = audit.ChainKey{ApplicationID: "app1", TenantID: "tenant1"}

// auditHead verifies the audit chain of the database and returns its length and head.
func auditHead(t *testing.T, db *store.DB) (int64, string) {
	t.Helper()
	log, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := log.VerifyChain(ctx, chain, audit.VerifyOptions{})
	if err != nil {
		t.Fatalf("audit chain does not verify: %v", err)
	}
	return rep.Events, rep.HeadHash
}

func count(t *testing.T, db *store.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func applied(t *testing.T, db *store.DB) int {
	return count(t, db, `SELECT count(*) FROM schema_migrations WHERE module = 'ledger'`)
}

func hasTable(t *testing.T, db *store.DB, name string) bool {
	t.Helper()
	return count(t, db, `SELECT count(*) FROM sqlite_master WHERE name = '`+name+`'`) == 1
}

// bkKeys verifies backups signed by the fixture's backup signer.
func (f *fixture) bkKeys() backup.StaticKeys {
	return backup.StaticKeys{Keys: map[string]ed25519.PublicKey{backupKeyID: f.bkSigner.Key.Public().(ed25519.PublicKey)}}
}

// binary makes the upgrader the binary of release version (Release.MatchesBinary).
func binary(version string) func(*Config) { return func(c *Config) { c.Version = version } }
