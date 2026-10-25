package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/buxee/alix-ai/internal/backup"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/internal/version"
)

func init() {
	Register(Command{
		Name:    "upgrade apply",
		Summary: "staged upgrade: verify the signed release, back up, migrate, probe, roll back automatically on failure",
		Run:     runUpgradeApply,
	})
	Register(Command{
		Name:    "upgrade check",
		Summary: "verify a signed release manifest and whether it qualifies the recorded runtime; changes nothing",
		Run:     runUpgradeCheck,
	})
	Register(Command{
		Name:    "upgrade recover",
		Summary: "finish or roll back an upgrade that was interrupted; rebuild isolated derived indexes",
		Run:     runUpgradeRecover,
	})
	Register(Command{
		Name:    "upgrade rollback",
		Summary: "return to the state before the last upgrade: restore the pre-upgrade backup and the previous runtime set",
		Run:     runUpgradeRollback,
	})
	Register(Command{
		Name:    "upgrade status",
		Summary: "show the recorded release and runtime, an unfinished upgrade and isolated indexes",
		Run:     runUpgradeStatus,
	})
}

const (
	upgradeApplyUsage = "alixai upgrade apply [-config FILE] -manifest FILE [-artifacts DIR] [-runtime-version V [-runtime-name localai] [-backend NAME=VERSION]...] [-models ID,...] [-pubkey ID=FILE]... [-allow-dev-key] [-key FILE] [-key-id ID]"
	upgradeCheckUsage = "alixai upgrade check [-config FILE] -manifest FILE [-artifacts DIR] [-runtime-version V ...] [-pubkey ID=FILE]... [-allow-dev-key]"
	upgradeOpsUsage   = "alixai upgrade (recover|rollback|status) [-config FILE] [-pubkey ID=FILE]... [-allow-dev-key] [-key FILE] [-key-id ID]"
)

// upgradeFlags are the flags the upgrade commands share.
type upgradeFlags struct {
	cfgPath  *string
	keyFile  *string
	keyID    *string
	allowDev *bool
	models   *string
	force    *bool
	pubs     backupMultiFlag
}

func upgradeCommon(env *Env, fs *flag.FlagSet) *upgradeFlags {
	f := &upgradeFlags{}
	f.cfgPath = env.ConfigFlag(fs)
	f.keyFile = fs.String("key", "", "PKCS#8 PEM key that signs the pre-upgrade backup (default <data_dir>/keys/backup.key)")
	f.keyID = fs.String("key-id", backupDefaultKeyID, "id of the backup signing key")
	f.allowDev = fs.Bool("allow-dev-key", false, "accept the published TEST-ONLY key for release manifests (development only: anyone can sign with it)")
	f.models = fs.String("models", "", "registry model ids the runtime must be qualified for (default: inference.models of the configuration, as serve uses; with neither, every model of the release for the runtime)")
	f.force = fs.Bool("force", false, "proceed although the database looks in use (a leftover -shm file): only after the service is stopped or has crashed")
	fs.Var(&f.pubs, "pubkey", "ID=FILE: a public key release manifests may be signed with, besides the built-in release key (default: the release keys of the trust store, see 'alixai trust add-key -app alixai.platform -purpose release')")
	return f
}

// upgradeTrustLookup is a trust.KeyLookup over the database's trust store that
// opens the database for each lookup and closes it again, so no handle is open
// while an upgrade replaces the file. release.Verify narrows it to release keys
// and adds the built-in one.
type upgradeTrustLookup struct {
	dataDir  string
	allowDev bool
}

func (l upgradeTrustLookup) withStore(ctx context.Context, fn func(*trust.Store) error) error {
	db, err := backupOpenStore(ctx, l.dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	var opts []trust.StoreOption
	if l.allowDev {
		opts = append(opts, trust.AllowTestOnlyKeys())
	}
	return fn(trust.NewStore(db, clock.Real{}, opts...))
}

func (l upgradeTrustLookup) LookupKey(ctx context.Context, applicationID, keyID string) (trust.Key, error) {
	var k trust.Key
	err := l.withStore(ctx, func(s *trust.Store) error {
		var err error
		k, err = s.LookupKey(ctx, applicationID, keyID)
		return err
	})
	return k, err
}

// MaterialRevoked implements trust.RevocationLookup.
func (l upgradeTrustLookup) MaterialRevoked(ctx context.Context, pub ed25519.PublicKey) (bool, error) {
	var revoked bool
	err := l.withStore(ctx, func(s *trust.Store) error {
		var err error
		revoked, err = s.MaterialRevoked(ctx, pub)
		return err
	})
	return revoked, err
}

// AllowsTestOnlyKeys implements trust.TestOnlyPolicy: -allow-dev-key.
func (l upgradeTrustLookup) AllowsTestOnlyKeys() bool { return l.allowDev }

func (f *upgradeFlags) releaseKeys(dataDir string) (trust.KeyLookup, error) {
	if len(f.pubs) > 0 {
		static, err := releasePublicKeys(f.pubs, *f.allowDev)
		if err != nil {
			return nil, err
		}
		return upgradeStaticKeys{StaticKeys: static, store: upgradeTrustLookup{dataDir: dataDir, allowDev: *f.allowDev}}, nil
	}
	return upgradeTrustLookup{dataDir: dataDir, allowDev: *f.allowDev}, nil
}

// upgradeStaticKeys are the -pubkey keys, which still answer to the
// revocations of the data directory's trust store: a key revoked there (the
// built-in release key among them) is not trusted again because it was also
// named on the command line.
type upgradeStaticKeys struct {
	release.StaticKeys
	store upgradeTrustLookup
}

// MaterialRevoked implements trust.RevocationLookup. With no database yet
// (checking a release before anything is installed) nothing is revoked; a
// database that exists and cannot be read is an error, so nothing verifies.
func (k upgradeStaticKeys) MaterialRevoked(ctx context.Context, pub ed25519.PublicKey) (bool, error) {
	if _, err := os.Stat(filepath.Join(k.store.dataDir, store.DefaultFileName)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return k.store.MaterialRevoked(ctx, pub)
}

var _ trust.RevocationLookup = upgradeStaticKeys{}

// releasePublicKeys reads -pubkey ID=FILE specs as release keys: the operator
// named them for verifying this release.
func releasePublicKeys(specs []string, allowDev bool) (release.StaticKeys, error) {
	keys, err := backupPublicKeys("", specs, allowDev)
	if err != nil {
		return release.StaticKeys{}, err
	}
	return release.StaticKeys{Keys: keys.Keys, AllowTestOnly: keys.AllowTestOnly}, nil
}

// modelList is the model ids the runtime must be qualified for: -models, else the
// models the configuration serves (call it after the configuration is loaded).
func (f *upgradeFlags) modelList() []string {
	if strings.TrimSpace(*f.models) == "" {
		return append([]string(nil), inferenceSection.Models...)
	}
	var out []string
	for _, m := range strings.Split(*f.models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// upgradeBuild resolves the data directory and builds the Upgrader. needSigner
// is false for the read-only status command.
func upgradeBuild(env *Env, f *upgradeFlags, needSigner bool) (*upgrade.Upgrader, string, error) {
	dataDir, err := backupDataDir(env, *f.cfgPath, "")
	if err != nil {
		return nil, "", err
	}
	cfg := upgrade.Config{
		DataDir:    dataDir,
		Store:      store.FromConfig().Config,
		Version:    version.Get().Version,
		Runtime:    upgrade.FileRuntime{Path: upgrade.RuntimeFile(dataDir)},
		Models:     f.modelList(),
		AllowInUse: *f.force,
	}
	if needSigner {
		if cfg.BackupSigner, err = backupSigner(env, dataDir, *f.keyFile, *f.keyID, true); err != nil {
			return nil, "", err
		}
		if cfg.ReleaseKeys, err = f.releaseKeys(dataDir); err != nil {
			return nil, "", err
		}
	} else {
		cfg.BackupKeys = backup.StaticKeys{}
	}
	u, err := upgrade.New(cfg)
	return u, dataDir, err
}

func upgradeParseOps(env *Env, args []string, name string) (*upgradeFlags, error) {
	fs := env.FlagSet("upgrade " + name)
	f := upgradeCommon(env, fs)
	if err := env.ParseFlags(fs, upgradeOpsUsage, args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), upgradeOpsUsage)
	}
	return f, nil
}

// upgradeEmit prints a result as JSON: the outcome is stated even when the
// operation failed, so an operator sees "rolled_back" next to the error.
func upgradeEmit(env *Env, res upgrade.Result) error {
	out := map[string]any{
		"outcome": string(res.Outcome), "from": res.From, "to": res.To, "backup_id": res.BackupID, "backup_dir": res.BackupDir,
		"derived_damaged": res.Derived.Damaged, "derived_rebuilt": res.Derived.Rebuilt,
		// Isolated indexes that only the serving process can rebuild (the vector index is re-embedded
		// by `alixai serve` once an embedder role is registered): retrieval says so until then.
		"derived_pending": res.Derived.Pending,
	}
	if res.Cause != nil {
		out["cause"] = res.Cause.Error()
	}
	return backupEmit(env.Stdout, out)
}

func upgradeRuntimeFlags(fs *flag.FlagSet) func() (*upgrade.RuntimeSet, error) {
	name := fs.String("runtime-name", "localai", "the runtime to switch to")
	ver := fs.String("runtime-version", "", "the runtime version to switch to (omit to leave the recorded runtime as it is)")
	var backends backupMultiFlag
	fs.Var(&backends, "backend", "NAME=VERSION: a backend of the runtime set (repeatable)")
	return func() (*upgrade.RuntimeSet, error) {
		if *ver == "" {
			if len(backends) > 0 {
				return nil, Usagef("-backend needs -runtime-version")
			}
			return nil, nil
		}
		rs := &upgrade.RuntimeSet{Runtime: *name, Version: *ver, Backends: map[string]string{}}
		for _, b := range backends {
			n, v, ok := strings.Cut(b, "=")
			if !ok || n == "" || v == "" {
				return nil, Usagef("-backend %q is not NAME=VERSION", b)
			}
			rs.Backends[n] = v
		}
		return rs, nil
	}
}

func runUpgradeApply(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("upgrade apply")
	f := upgradeCommon(env, fs)
	manifest := fs.String("manifest", "", "the signed release manifest written by tools/release (required)")
	artifacts := fs.String("artifacts", "", "a directory whose files must match the manifest's artifacts exactly")
	runtimeSet := upgradeRuntimeFlags(fs)
	if err := env.ParseFlags(fs, upgradeApplyUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *manifest == "" {
		return Usagef("-manifest is required (usage: %s)", upgradeApplyUsage)
	}
	rs, err := runtimeSet()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*manifest)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", *manifest, err)
	}
	u, dataDir, err := upgradeBuild(env, f, true)
	if err != nil {
		return err
	}
	if rs == nil {
		if cur, _ := (upgrade.FileRuntime{Path: upgrade.RuntimeFile(dataDir)}).Observe(ctx); cur.Runtime == "" {
			fmt.Fprintln(env.Stderr, "alixai: note: no runtime is recorded and none was given: the runtime is not qualified by this upgrade (pass -runtime-version and -backend)")
		}
	}
	res, err := u.Apply(ctx, upgrade.Request{Manifest: raw, ArtifactDir: *artifacts, Runtime: rs})
	if eerr := upgradeEmit(env, res); eerr != nil && err == nil {
		err = eerr
	}
	return err
}

func runUpgradeCheck(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("upgrade check")
	f := upgradeCommon(env, fs)
	manifest := fs.String("manifest", "", "the signed release manifest (required)")
	artifacts := fs.String("artifacts", "", "a directory whose files must match the manifest's artifacts exactly")
	runtimeSet := upgradeRuntimeFlags(fs)
	if err := env.ParseFlags(fs, upgradeCheckUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *manifest == "" {
		return Usagef("-manifest is required (usage: %s)", upgradeCheckUsage)
	}
	rs, err := runtimeSet()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*manifest)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", *manifest, err)
	}
	dataDir, err := backupDataDir(env, *f.cfgPath, "")
	if err != nil {
		return err
	}
	keys, err := f.releaseKeys(dataDir)
	if err != nil {
		return err
	}
	rel, err := (upgrade.ReleaseVerifier{Keys: keys}).VerifyRelease(ctx, raw)
	if err != nil {
		return err
	}
	if *artifacts != "" {
		if err := rel.VerifyArtifacts(*artifacts); err != nil {
			return err
		}
	}
	out := map[string]any{
		"version": rel.Version, "commit": rel.Commit, "created_at": rel.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"signed_by": rel.Accepted.KeyID, "artifacts": len(rel.Artifacts), "backends": len(rel.Backends), "models": len(rel.Models),
	}
	// apply refuses a release that is not this binary's; check says so first.
	exe, _ := os.Executable()
	berr := rel.MatchesBinary(version.Get().Version, exe)
	out["binary"] = map[string]any{"version": version.Get().Version, "matches": berr == nil}
	if berr != nil {
		out["binary"].(map[string]any)["error"] = berr.Error()
	}
	if rs == nil {
		if cur, _ := (upgrade.FileRuntime{Path: upgrade.RuntimeFile(dataDir)}).Observe(ctx); cur.Runtime != "" {
			rs = &cur
		}
	}
	if rs != nil {
		qerr := rel.Qualify(*rs, f.modelList()...)
		out["runtime"] = map[string]any{"checked": rs, "qualified": qerr == nil}
		if qerr != nil {
			out["runtime"].(map[string]any)["error"] = qerr.Error()
		}
		if err := backupEmit(env.Stdout, out); err != nil {
			return err
		}
		if qerr != nil {
			return qerr
		}
		return berr
	}
	out["runtime"] = map[string]any{"checked": nil, "qualified": nil}
	if err := backupEmit(env.Stdout, out); err != nil {
		return err
	}
	return berr
}

func runUpgradeRecover(ctx context.Context, env *Env, args []string) error {
	f, err := upgradeParseOps(env, args, "recover")
	if err != nil {
		return err
	}
	u, _, err := upgradeBuild(env, f, true)
	if err != nil {
		return err
	}
	res, err := u.Recover(ctx)
	if eerr := upgradeEmit(env, res); eerr != nil && err == nil {
		err = eerr
	}
	return err
}

func runUpgradeRollback(ctx context.Context, env *Env, args []string) error {
	f, err := upgradeParseOps(env, args, "rollback")
	if err != nil {
		return err
	}
	u, _, err := upgradeBuild(env, f, true)
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stderr, "alixai: the service must be stopped; what was written since the upgrade stays in <data_dir>/.pre-restore-* and is not merged back")
	res, err := u.Rollback(ctx)
	if eerr := upgradeEmit(env, res); eerr != nil && err == nil {
		err = eerr
	}
	return err
}

func runUpgradeStatus(_ context.Context, env *Env, args []string) error {
	f, err := upgradeParseOps(env, args, "status")
	if err != nil {
		return err
	}
	u, _, err := upgradeBuild(env, f, false)
	if err != nil {
		return err
	}
	st, err := u.Status()
	if err != nil {
		return err
	}
	return backupEmit(env.Stdout, map[string]any{"state": st.State, "unfinished": st.Unfinished, "quarantined": st.Quarantined})
}
