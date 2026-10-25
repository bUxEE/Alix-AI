package trust

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// t0 is the fake clock's start: inside the validity of the fixtures below.
var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

const testApp = "demo-library"

// keyPair is a generated test key (never a fixture secret on disk).
type keyPair struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKey(t testing.TB, id string) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keyPair{id: id, pub: pub, priv: priv}
}

func (k keyPair) signing() SigningKey { return SigningKey{KeyID: k.id, Key: k.priv} }

// newTrustStore opens a throwaway database with only the trust module.
func newTrustStore(t testing.TB, clk clock.Clock, opts ...StoreOption) (*Store, *store.DB) {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register("trust", migrations); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t, reg)
	return NewStore(db, clk, opts...), db
}

// writeFiles writes name -> content below dir, creating directories.
func writeFiles(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(files[n]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// samplePackage is a small package in the shape of the demo-library example.
func samplePackage() map[string]string {
	return map[string]string{
		"alixai.yaml":                              "format: alixai.application/v1\napplication:\n  id: demo-library\n",
		"capabilities/catalog-find-title.yaml":     "id: catalog.find_title\n",
		"capabilities/reservations-create.yaml":    "id: reservations.create\n",
		"documentation/index.yaml":                 "entries: []\n",
		"documentation/catalog/find-title.md":      "# Find a title\n",
		"schemas/find-title.result.schema.json":    "{\"type\":\"object\"}\n",
		"tests/seed/library.json":                  "{\"books\":[]}\n",
		"conversation-contracts/a.b.en.yaml":       "x: 1\n",
		"conversation-contracts/a/b.en.yaml":       "x: 2\n",
		"documentation/policies/loan-period.md":    "Loans last 21 days.\n",
		"terminology/library-terms.yaml":           "terms: []\n",
		"policies/default-access.yaml":             "rules: []\n",
		"rag-sources/loan-policies.yaml":           "id: loan-policies\n",
		"schemas/adapters/library-mcp.yaml":        "transport: mcp\n",
		"schemas/reservation.prepared.schema.json": "{}\n",
	}
}

// fixture is a signed package with a trust store that trusts its key.
type fixture struct {
	t     *testing.T
	dir   string
	clk   *clock.Fake
	store *Store
	db    *store.DB
	key   keyPair
	v     *Verifier
	opts  PackageOptions
	sm    *SignatureManifest
}

// newFixture signs samplePackage with a fresh key registered for testApp. The
// signature is valid for 30 days from t0.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(t0)
	st, db := newTrustStore(t, clk)
	key := newKey(t, "demo-library-publisher-2026")
	if _, err := st.RegisterKey(context.Background(), testApp, key.id, key.pub, "test publisher"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFiles(t, dir, samplePackage())
	sm, err := SignPackageDir(dir, SignOptions{
		PackageIdentity: PackageIdentity{ApplicationID: testApp, PackageVersion: "1.0.0"},
		Keys:            []SigningKey{key.signing()},
		Now:             t0,
		ExpiresAt:       t0.Add(30 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("SignPackageDir: %v", err)
	}
	return &fixture{
		t: t, dir: dir, clk: clk, store: st, db: db, key: key, sm: sm,
		v: NewVerifier(st, clk),
		opts: PackageOptions{
			ApplicationID:   testApp,
			PackageVersion:  "1.0.0",
			PublisherKeyIDs: []string{key.id},
		},
	}
}

func (f *fixture) verify() (*PackageVerification, error) {
	f.t.Helper()
	return f.v.VerifyPackageDir(context.Background(), f.dir, f.opts)
}

// mustVerify fails the test unless the package verifies.
func (f *fixture) mustVerify() *PackageVerification {
	f.t.Helper()
	pv, err := f.verify()
	if err != nil {
		f.t.Fatalf("package should verify: %v", err)
	}
	return pv
}

// wantFail asserts err is a failure with the given contracts code and wraps
// every listed sentinel. A bare non-nil error is not enough: an unrelated
// failure must not make a test pass.
func wantFail(t testing.TB, err error, code contracts.ErrCode, sentinels ...error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a failure with code %s, got success", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Errorf("error code = %s, want %s (err: %v)", got, code, err)
	}
	for _, s := range sentinels {
		if !errors.Is(err, s) {
			t.Errorf("error does not wrap %q (err: %v)", s, err)
		}
	}
}

// recordingFS records every name opened, so a test can prove a path was never
// touched.
type recordingFS struct {
	fs.FS
	mu     sync.Mutex
	opened []string
}

func (r *recordingFS) Open(name string) (fs.File, error) {
	r.mu.Lock()
	r.opened = append(r.opened, name)
	r.mu.Unlock()
	return r.FS.Open(name)
}

func (r *recordingFS) wasOpened(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.opened {
		if n == name {
			return true
		}
	}
	return false
}

// marshalUnchecked encodes sm without validating it, so a test can write the
// malformed or hostile manifests Marshal would refuse.
func marshalUnchecked(sm *SignatureManifest) ([]byte, error) {
	return json.MarshalIndent(sm, "", "  ")
}
