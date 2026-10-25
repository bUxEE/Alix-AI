package runtimesec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The upgrade half of SEC-072: the real staged upgrade (internal/upgrade) over a real data
// directory, a release manifest written and signed with the production release package, and a
// runtime that records what it is told and reports what runs.

const (
	releaseKeyID = "release-sec"
	backupKeyID  = "backup-sec"
	ledgerV1     = `CREATE TABLE ledger (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w', body TEXT NOT NULL) STRICT;`
	ledgerV2     = `CREATE TABLE ledger_extra (n INTEGER PRIMARY KEY, application_id TEXT NOT NULL DEFAULT 'a', tenant_id TEXT NOT NULL DEFAULT 't', workspace_id TEXT NOT NULL DEFAULT 'w') STRICT;`
)

// The two runtime sets: what release 1.0.0 was qualified with, and what release 2.0.0 is.
var (
	setOld = upgrade.RuntimeSet{Runtime: "localai", Version: "v3.5.0", Backends: map[string]string{"llama-cpp": "b1000"}}
	setNew = upgrade.RuntimeSet{Runtime: "localai", Version: "v3.6.0", Backends: map[string]string{"llama-cpp": "b2000"}}
)

func ledgerRegistry(t *testing.T, withV2 bool) *store.Registry {
	t.Helper()
	files := fstest.MapFS{store.MigrationsDir + "/0001_init.sql": {Data: []byte(ledgerV1)}}
	if withV2 {
		files[store.MigrationsDir+"/0002_extra.sql"] = &fstest.MapFile{Data: []byte(ledgerV2)}
	}
	reg := store.NewRegistry()
	if err := reg.Register("ledger", files); err != nil {
		t.Fatal(err)
	}
	return reg
}

// fakeRuntime is the Runtime of the upgrade: it reports the set that runs and switches it. drift
// makes the runtime come up as something else after it was switched to a given set, which is what
// a backend replaced behind an upgrade's back looks like.
type fakeRuntime struct {
	mu          sync.Mutex
	cur         upgrade.RuntimeSet
	activations []upgrade.RuntimeSet
	drift       func(asked upgrade.RuntimeSet) *upgrade.RuntimeSet
	observed    *upgrade.RuntimeSet
}

func copySet(rs upgrade.RuntimeSet) upgrade.RuntimeSet {
	out := rs
	out.Backends = map[string]string{}
	for k, v := range rs.Backends {
		out.Backends[k] = v
	}
	return out
}

func (r *fakeRuntime) Observe(context.Context) (upgrade.RuntimeSet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.observed != nil {
		return copySet(*r.observed), nil
	}
	return copySet(r.cur), nil
}

func (r *fakeRuntime) Activate(_ context.Context, rs upgrade.RuntimeSet) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = copySet(rs)
	r.activations = append(r.activations, copySet(rs))
	r.observed = nil
	if r.drift != nil {
		r.observed = r.drift(rs)
	}
	return nil
}

func (r *fakeRuntime) running() upgrade.RuntimeSet {
	set, _ := r.Observe(context.Background())
	return set
}

func (r *fakeRuntime) activationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.activations)
}

type upgradeWorld struct {
	t        *testing.T
	dataDir  string
	clk      *clock.Fake
	relPriv  ed25519.PrivateKey
	relKeys  release.StaticKeys
	bkSigner trust.SigningKey
	rt       *fakeRuntime
}

// manifest writes and signs a release manifest with the production release package. The release
// is qualified for the runtime version allowed and the backend version backend.
func (w *upgradeWorld) manifest(version, allowed, backend string) []byte {
	w.t.Helper()
	sum := sha256.Sum256([]byte("binary " + version))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	m := release.Manifest{
		Format: release.Format,
		Signed: release.Signed{
			Kind: release.Kind, ApplicationID: trust.PlatformApplicationID, Version: version,
			Commit: strings.Repeat("a", 40), CreatedAt: "2026-10-03T12:00:00Z", HashAlgorithm: release.HashAlgorithm,
			Artifacts: []release.Artifact{{Name: "alixai", Kind: release.KindBinary, OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: 12}},
			Images:    []release.Image{{Name: "local-ai", Source: release.SourceThirdParty, Reference: "quay.io/go-skynet/local-ai:" + allowed, Digest: digest}},
			Backends: []release.Backend{{
				Runtime: "localai", Name: "llama-cpp", Alias: "llama-cpp", Version: backend, Licence: "MIT",
				Reference: "quay.io/go-skynet/local-ai-backends", Digest: digest, Mirrors: []string{},
			}},
			Models: []release.Model{{
				ID: "granite-3b", Name: "Granite", Version: "1", Status: "candidate",
				Upstream: release.ModelUpstream{RepoURL: "https://example.invalid/m", Revision: "r1"},
				Artifact: release.ModelArtifact{Format: "gguf", RepoURL: "https://example.invalid/m", Filename: "m.gguf", Revision: "r1", SizeBytes: 1000, SHA256: hex.EncodeToString(sum[:]), QuantizedBy: "x"},
				Licence:  release.ModelLicence{SPDX: "Apache-2.0", URL: "https://example.invalid/l", RedistributionNotes: "ok"},
				Backend:  release.ModelBackend{Runtime: "localai", Name: "llama-cpp", AllowedRuntimeVersions: []string{allowed}},
			}},
		},
	}
	raw, err := release.MarshalIndent(m)
	if err != nil {
		w.t.Fatal(err)
	}
	signed, err := release.Sign(raw, trust.SigningKey{KeyID: releaseKeyID, Key: w.relPriv}, w.clk.Now(), 365*24*time.Hour)
	if err != nil {
		w.t.Fatalf("release.Sign: %v", err)
	}
	return signed
}

func (w *upgradeWorld) release1() []byte { return w.manifest("1.0.0", "v3.5.0", "b1000") }
func (w *upgradeWorld) release2() []byte { return w.manifest("2.0.0", "v3.6.0", "b2000") }

func (w *upgradeWorld) upgrader(withV2 bool, mutate ...func(*upgrade.Config)) *upgrade.Upgrader {
	w.t.Helper()
	cfg := upgrade.Config{
		DataDir: w.dataDir, Migrations: ledgerRegistry(w.t, withV2), Clock: w.clk,
		ReleaseKeys: w.relKeys, BackupSigner: w.bkSigner, Runtime: w.rt, Version: "2.0.0",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	u, err := upgrade.New(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	return u
}

// newUpgradeWorld builds an installation at release 1.0.0 running the old runtime set, which that
// release was qualified for.
func newUpgradeWorld(t *testing.T) *upgradeWorld {
	t.Helper()
	w := &upgradeWorld{t: t, dataDir: filepath.Join(t.TempDir(), "data"), clk: clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.relPriv = priv
	w.relKeys = release.StaticKeys{Keys: map[string]ed25519.PublicKey{releaseKeyID: pub}}
	_, bpriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.bkSigner = trust.SigningKey{KeyID: backupKeyID, Key: bpriv}
	w.rt = &fakeRuntime{cur: copySet(setOld)}

	// The installed database, with data in it.
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(w.dataDir, store.DefaultFileName), Migrations: ledgerRegistry(t, false), Clock: w.clk})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := db.ExecContext(context.Background(), `INSERT INTO ledger (n, body) VALUES (?, 'row')`, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Release 1.0.0 is installed through the real upgrade, so the state the later steps roll back to
	// is a state the upgrade itself recorded.
	set := copySet(setOld)
	res, err := w.upgrader(false, func(c *upgrade.Config) { c.Version = "1.0.0" }).Apply(context.Background(), upgrade.Request{Manifest: w.release1(), Runtime: &set})
	if err != nil || res.Outcome != upgrade.OutcomeApplied {
		t.Fatalf("installing release 1.0.0: %+v, %v", res, err)
	}
	w.clk.Advance(time.Hour)
	return w
}

// ledgerTables reports which of the ledger tables exist: 1 before the 2.0.0 migration, 2 after.
func (w *upgradeWorld) schemaLevel() int {
	w.t.Helper()
	// An empty registry: reading the schema must not migrate it.
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(w.dataDir, store.DefaultFileName), Migrations: store.NewRegistry(), Clock: w.clk})
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM sqlite_master WHERE name = 'ledger_extra'`).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return 1 + n
}

func (w *upgradeWorld) installed(u *upgrade.Upgrader) upgrade.State {
	w.t.Helper()
	st, err := u.LoadState()
	if err != nil {
		w.t.Fatal(err)
	}
	return st
}

// assertUntouched fails unless the installation is exactly what newUpgradeWorld left: release
// 1.0.0, the old runtime set running and recorded, the old schema.
func (w *upgradeWorld) assertUntouched(u *upgrade.Upgrader, activationsBefore int) {
	w.t.Helper()
	st := w.installed(u)
	if st.ReleaseVersion != "1.0.0" {
		w.t.Errorf("installed release = %q, want 1.0.0", st.ReleaseVersion)
	}
	if st.Runtime == nil || st.Runtime.Version != setOld.Version || st.Runtime.Backends["llama-cpp"] != "b1000" {
		w.t.Errorf("recorded runtime = %+v, want the old set", st.Runtime)
	}
	if got := w.rt.running(); got.Version != setOld.Version || got.Backends["llama-cpp"] != "b1000" {
		w.t.Errorf("the runtime that runs is %+v, want the old set", got)
	}
	if lvl := w.schemaLevel(); lvl != 1 {
		w.t.Errorf("schema level = %d, want 1: the database was migrated", lvl)
	}
	_ = activationsBefore
}

// upgradeHalf is the part of SEC-072 about the staged upgrade.
func upgradeHalf(t *testing.T) {
	ctx := context.Background()

	t.Run("control: the qualified set the old release ran is not qualified by the new release, and the new set is", func(t *testing.T) {
		w := newUpgradeWorld(t)
		v := upgrade.ReleaseVerifier{Keys: w.relKeys, Clock: w.clk}
		rel1, err := v.VerifyRelease(ctx, w.release1())
		if err != nil {
			t.Fatal(err)
		}
		rel2, err := v.VerifyRelease(ctx, w.release2())
		if err != nil {
			t.Fatal(err)
		}
		if err := rel1.Qualify(setOld); err != nil {
			t.Errorf("release 1.0.0 does not qualify the set it ran: %v", err)
		}
		if err := rel2.Qualify(setOld); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("release 2.0.0 qualifies the old set (%v): the rollback target test below would prove nothing", err)
		}
		if err := rel2.Qualify(setNew); err != nil {
			t.Errorf("release 2.0.0 does not qualify its own set: %v", err)
		}
	})

	t.Run("control: a qualified upgrade is applied, switches the runtime and records the new set", func(t *testing.T) {
		w := newUpgradeWorld(t)
		u := w.upgrader(true)
		set := copySet(setNew)
		res, err := u.Apply(ctx, upgrade.Request{Manifest: w.release2(), Runtime: &set})
		if err != nil || res.Outcome != upgrade.OutcomeApplied {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		if got := w.rt.running(); got.Version != setNew.Version {
			t.Errorf("runtime = %+v, want the new set", got)
		}
		if st := w.installed(u); st.ReleaseVersion != "2.0.0" || st.Runtime == nil || st.Runtime.Version != setNew.Version {
			t.Errorf("state = %+v", st)
		}
	})

	t.Run("an unqualified runtime or backend is refused before anything changes", func(t *testing.T) {
		cases := map[string]*upgrade.RuntimeSet{
			"runtime version":         {Runtime: "localai", Version: "v9.9.9", Backends: map[string]string{"llama-cpp": "b2000"}},
			"backend version":         {Runtime: "localai", Version: "v3.6.0", Backends: map[string]string{"llama-cpp": "b9999"}},
			"unknown backend":         {Runtime: "localai", Version: "v3.6.0", Backends: map[string]string{"evil-backend": "b2000"}},
			"another runtime":         {Runtime: "other", Version: "v3.6.0"},
			"no version":              {Runtime: "localai"},
			"the running set, kept":   nil, // 2.0.0 does not qualify v3.5.0
			"the old set, requested":  &setOld,
			"a version that is older": {Runtime: "localai", Version: "v3.4.0", Backends: map[string]string{"llama-cpp": "b2000"}},
		}
		for name, req := range cases {
			t.Run(name, func(t *testing.T) {
				w := newUpgradeWorld(t)
				u := w.upgrader(true)
				before := w.rt.activationCount()
				res, err := u.Apply(ctx, upgrade.Request{Manifest: w.release2(), Runtime: req})
				if !errors.Is(err, upgrade.ErrRefused) || res.Outcome != upgrade.OutcomeRefused {
					t.Fatalf("Apply = %+v, %v, want a refusal", res, err)
				}
				if name != "no version" && contracts.CodeOf(err) != contracts.ErrUnavailable {
					t.Errorf("code = %s, want %s: an unqualified combination reports unavailable", contracts.CodeOf(err), contracts.ErrUnavailable)
				}
				if n := w.rt.activationCount() - before; n != 0 {
					t.Errorf("a refused upgrade switched the runtime %d times", n)
				}
				w.assertUntouched(u, before)
			})
		}
	})

	t.Run("a backend that is not the qualified one after the switch is rolled back to the previous qualified set", func(t *testing.T) {
		w := newUpgradeWorld(t)
		// The new set is activated, and a backend other than the pinned one comes up.
		w.rt.drift = func(asked upgrade.RuntimeSet) *upgrade.RuntimeSet {
			if asked.Version == setNew.Version {
				d := copySet(asked)
				d.Backends["llama-cpp"] = "b9999"
				return &d
			}
			return nil
		}
		u := w.upgrader(true)
		set := copySet(setNew)
		res, err := u.Apply(ctx, upgrade.Request{Manifest: w.release2(), Runtime: &set})
		if !errors.Is(err, upgrade.ErrRolledBack) || res.Outcome != upgrade.OutcomeRolledBack {
			t.Fatalf("Apply = %+v, %v, want a rollback", res, err)
		}
		if contracts.CodeOf(res.Cause) != contracts.ErrUnavailable {
			t.Errorf("cause code = %s (%v), want %s", contracts.CodeOf(res.Cause), res.Cause, contracts.ErrUnavailable)
		}
		w.assertUntouched(u, 0)

		// The set that is back is the one release 1.0.0 qualified, and release 2.0.0 does not: it is
		// the previous qualified set, not merely the previous set.
		v := upgrade.ReleaseVerifier{Keys: w.relKeys, Clock: w.clk}
		rel1, err := v.VerifyRelease(ctx, w.release1())
		if err != nil {
			t.Fatal(err)
		}
		if err := rel1.Qualify(w.rt.running()); err != nil {
			t.Errorf("the runtime that runs after the rollback is not qualified by the release it belongs to: %v", err)
		}
		if n := w.rt.activationCount(); n < 3 {
			t.Errorf("the runtime was activated %d times in all, want the install, the new set and the previous set put back", n)
		}
		// Control: the next attempt, with the drift gone, goes through, so the rollback left the installation usable.
		w.rt.drift = nil
		w.clk.Advance(time.Hour)
		set = copySet(setNew)
		if res, err := w.upgrader(true).Apply(ctx, upgrade.Request{Manifest: w.release2(), Runtime: &set}); err != nil || res.Outcome != upgrade.OutcomeApplied {
			t.Fatalf("second attempt = %+v, %v", res, err)
		}
	})

	t.Run("a failed health probe after the switch puts the previous runtime set back", func(t *testing.T) {
		w := newUpgradeWorld(t)
		u := w.upgrader(true, func(c *upgrade.Config) {
			c.Probe = func(context.Context, *store.DB) error { return errors.New("the application does not answer") }
		})
		set := copySet(setNew)
		res, err := u.Apply(ctx, upgrade.Request{Manifest: w.release2(), Runtime: &set})
		if !errors.Is(err, upgrade.ErrRolledBack) || res.Outcome != upgrade.OutcomeRolledBack {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		w.assertUntouched(u, 0)
	})

	t.Run("an operator rollback after a good upgrade returns to the previous qualified set", func(t *testing.T) {
		w := newUpgradeWorld(t)
		u := w.upgrader(true)
		set := copySet(setNew)
		if res, err := u.Apply(ctx, upgrade.Request{Manifest: w.release2(), Runtime: &set}); err != nil || res.Outcome != upgrade.OutcomeApplied {
			t.Fatalf("Apply = %+v, %v", res, err)
		}
		if w.schemaLevel() != 2 || w.rt.running().Version != setNew.Version {
			t.Fatal("the upgrade did not take effect, so there is nothing to roll back")
		}
		res, err := u.Rollback(ctx)
		if err != nil || res.Outcome != upgrade.OutcomeRolledBack {
			t.Fatalf("Rollback = %+v, %v", res, err)
		}
		w.assertUntouched(u, 0)
	})
}
