package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/buxee/alix-ai/internal/backup"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
)

func init() {
	Register(Command{
		Name:    "backup create",
		Summary: "make a consistent, signed backup of the database and file trees while the service runs",
		Run:     runBackupCreate,
	})
	Register(Command{
		Name:    "backup verify",
		Summary: "check a backup's signature, files and database without changing anything",
		Run:     runBackupVerify,
	})
	Register(Command{
		Name:    "backup restore",
		Summary: "verify a backup and replace the data directory's database and file trees with it (service stopped)",
		Run:     runBackupRestore,
	})
}

const (
	backupCreateUsage  = "alixai backup create [-config FILE] -dir DIR [-trees files,packages] [-key FILE] [-key-id ID]"
	backupVerifyUsage  = "alixai backup verify -dir BACKUP [-config FILE | -data-dir DIR] [-pubkey ID=FILE]... [-allow-dev-key]"
	backupRestoreUsage = "alixai backup restore -dir BACKUP [-config FILE | -data-dir DIR] [-pubkey ID=FILE]... [-allow-dev-key] [-force] [-skip-newer-revocations]"

	// backupDefaultKeyID is the id, and the file stem, of the backup signing key
	// created under <data_dir>/keys on first use.
	backupDefaultKeyID = "backup"
	backupKeysDirName  = "keys"
)

// backupMultiFlag is a repeatable string flag.
type backupMultiFlag []string

func (m *backupMultiFlag) String() string     { return strings.Join(*m, ",") }
func (m *backupMultiFlag) Set(v string) error { *m = append(*m, v); return nil }

func backupEmit(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// backupDataDir is the data directory: the override when given, else the one the
// configuration names.
func backupDataDir(env *Env, configPath, override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if err := env.Config.LoadFrom(configPath, env.LookupEnv); err != nil {
		return "", err
	}
	return filepath.Abs(env.Config.Core().DataDir.String())
}

// backupOpenStore opens the database WITHOUT applying any migration, so a backup
// is of the state the files are in, whatever binary takes it.
func backupOpenStore(ctx context.Context, dataDir string) (*store.DB, error) {
	if err := backup.InterruptedRestore(dataDir); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, store.DefaultFileName)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("there is no database at %s: %w", path, err)
	}
	opts := store.FromConfig()
	opts.Path = path
	opts.Migrations = store.NewRegistry()
	db, err := store.Open(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("open the store: %w", err)
	}
	return db, nil
}

// backupSigner returns the key that signs backups: the one named by -key, else
// <data_dir>/keys/backup.key, created owner-only on first use when it does not
// exist (create is set). The public half is what verifies a backup; it must be
// kept somewhere other than the machine the backups protect.
func backupSigner(env *Env, dataDir, keyFile, keyID string, create bool) (trust.SigningKey, error) {
	if keyID == "" {
		keyID = backupDefaultKeyID
	}
	if keyFile == "" {
		dir := filepath.Join(dataDir, backupKeysDirName)
		keyFile = filepath.Clean(filepath.Join(dir, keyID+".key"))
		if _, err := os.Lstat(keyFile); errors.Is(err, os.ErrNotExist) { // #nosec G703 -- keyFile is under data_dir/keys with a fixed stem
			if !create {
				return trust.SigningKey{}, Usagef("no backup signing key at %s: pass -key FILE", keyFile)
			}
			_, priv, err := trust.GenerateKey()
			if err != nil {
				return trust.SigningKey{}, err
			}
			_, pubPath, err := trust.WriteKeyFiles(dir, keyID, priv)
			if err != nil {
				return trust.SigningKey{}, err
			}
			fmt.Fprintf(env.Stderr, "alixai: created the backup signing key %s and its public half %s\nalixai: copy the .pub file somewhere that is NOT this machine: restoring after the data directory is lost needs it\n", keyFile, pubPath)
		}
	}
	key, warnings, err := trust.LoadPrivateKeyFile(keyFile)
	if err != nil {
		return trust.SigningKey{}, err
	}
	if len(warnings) > 0 {
		// A signing key other users can read lets them forge a backup (as loadCheckpointKey).
		return trust.SigningKey{}, fmt.Errorf("the backup signing key cannot be used: %s", warnings[0])
	}
	if trust.IsTestOnlyKey(key.Public().(ed25519.PublicKey)) {
		return trust.SigningKey{}, Usagef("the signing key is a published TEST-ONLY key that anyone can sign with; use your own")
	}
	return trust.SigningKey{KeyID: keyID, Key: key}, nil
}

// backupPublicKeys reads -pubkey ID=FILE specs (a file holding the base64 of the
// raw 32 bytes, what 'alixai package keygen' and this command write as .pub).
// With none, it falls back to <data_dir>/keys/backup.pub when that exists.
func backupPublicKeys(dataDir string, specs []string, allowDev bool) (backup.StaticKeys, error) {
	keys := backup.StaticKeys{Keys: map[string]ed25519.PublicKey{}, AllowTestOnly: allowDev}
	for _, spec := range specs {
		id, file, ok := strings.Cut(spec, "=")
		if !ok || id == "" || file == "" {
			return keys, Usagef("-pubkey %q is not ID=FILE", spec)
		}
		file = filepath.Clean(file)
		raw, err := os.ReadFile(file) // #nosec G703 -- file is the operator's -pubkey path, cleaned
		if err != nil {
			return keys, fmt.Errorf("cannot read %s: %w", file, err)
		}
		pub, err := trust.DecodePublicKey(string(raw))
		if err != nil {
			return keys, Usagef("%s: %v", file, err)
		}
		keys.Keys[id] = pub
	}
	if len(specs) == 0 && dataDir != "" {
		file := filepath.Join(dataDir, backupKeysDirName, backupDefaultKeyID+".pub")
		if raw, err := os.ReadFile(file); err == nil {
			if pub, err := trust.DecodePublicKey(string(raw)); err == nil {
				keys.Keys[backupDefaultKeyID] = pub
			}
		}
	}
	if len(keys.Keys) == 0 {
		return keys, Usagef("no public key to verify with: pass -pubkey ID=FILE (the backup's .pub file)")
	}
	return keys, nil
}

func runBackupCreate(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("backup create")
	cfgPath := env.ConfigFlag(fs)
	dir := fs.String("dir", "", "directory that receives the backup (required; the backup goes in a new subdirectory)")
	trees := fs.String("trees", strings.Join(backup.DefaultTrees, ","), "file trees below the data directory to include, comma separated, among files and packages (empty: database only)")
	keyFile := fs.String("key", "", "PKCS#8 PEM signing key (default <data_dir>/keys/backup.key, created on first use)")
	keyID := fs.String("key-id", backupDefaultKeyID, "id of the signing key")
	if err := env.ParseFlags(fs, backupCreateUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q (usage: %s)", fs.Arg(0), backupCreateUsage)
	}
	if *dir == "" {
		return Usagef("-dir is required: where backups are kept is the operator's decision (usage: %s)", backupCreateUsage)
	}
	dataDir, err := backupDataDir(env, *cfgPath, "")
	if err != nil {
		return err
	}
	signer, err := backupSigner(env, dataDir, *keyFile, *keyID, true)
	if err != nil {
		return err
	}
	var treeList []string
	for _, t := range strings.Split(*trees, ",") {
		if t = strings.TrimSpace(t); t != "" {
			treeList = append(treeList, t)
		}
	}
	if treeList == nil {
		treeList = []string{}
	}
	db, err := backupOpenStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := backup.Create(ctx, db, backup.Options{Dir: *dir, DataDir: dataDir, Trees: treeList, Signer: signer, Version: version.Get().Version})
	if err != nil {
		return err
	}
	out := map[string]any{
		"backup_id": res.ID, "dir": res.Dir, "files": len(res.Manifest.Signed.Files), "trees": res.Manifest.Signed.Trees,
		"key_id": signer.KeyID, "created_at": res.Manifest.Signed.CreatedAt,
	}
	if len(res.LostBlobs) > 0 {
		out["lost_blobs"] = res.LostBlobs
		fmt.Fprintf(env.Stderr, "alixai: warning: the data directory had already lost %d stored file(s) that live records name; the backup holds it as it is (see lost_blobs)\n", len(res.LostBlobs))
	}
	return backupEmit(env.Stdout, out)
}

func runBackupVerify(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("backup verify")
	cfgPath := env.ConfigFlag(fs)
	dir := fs.String("dir", "", "the backup directory (required)")
	dataDirFlag := fs.String("data-dir", "", "data directory, for the default public key (default: from the configuration)")
	allowDev := fs.Bool("allow-dev-key", false, "accept the published TEST-ONLY key (development only: anyone can sign with it)")
	var pubs backupMultiFlag
	fs.Var(&pubs, "pubkey", "ID=FILE: a public key the manifest may be signed with (repeatable)")
	if err := env.ParseFlags(fs, backupVerifyUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *dir == "" {
		return Usagef("-dir is required (usage: %s)", backupVerifyUsage)
	}
	dataDir := ""
	if len(pubs) == 0 {
		var err error
		if dataDir, err = backupDataDir(env, *cfgPath, *dataDirFlag); err != nil {
			return err
		}
	}
	keys, err := backupPublicKeys(dataDir, pubs, *allowDev)
	if err != nil {
		return err
	}
	m, err := backup.Verify(ctx, *dir, backup.VerifyOptions{Keys: keys})
	if err != nil {
		return err
	}
	return backupEmit(env.Stdout, map[string]any{
		"backup_id": m.Signed.BackupID, "created_at": m.Signed.CreatedAt, "version": m.Signed.Version,
		"files": len(m.Signed.Files), "trees": m.Signed.Trees, "verified": true,
	})
}

func runBackupRestore(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("backup restore")
	cfgPath := env.ConfigFlag(fs)
	dir := fs.String("dir", "", "the backup directory (required)")
	dataDirFlag := fs.String("data-dir", "", "data directory to restore into (default: from the configuration)")
	allowDev := fs.Bool("allow-dev-key", false, "accept the published TEST-ONLY key (development only: anyone can sign with it)")
	force := fs.Bool("force", false, "restore although the database looks in use (a leftover -shm file): only after the service is stopped or has crashed")
	skipRevocations := fs.Bool("skip-newer-revocations", false, "restore the publisher and issuer keys exactly as the backup holds them, even ones the database in place has revoked since (only when that database is known to be wrong)")
	var pubs backupMultiFlag
	fs.Var(&pubs, "pubkey", "ID=FILE: a public key the manifest may be signed with (repeatable)")
	if err := env.ParseFlags(fs, backupRestoreUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *dir == "" {
		return Usagef("-dir is required (usage: %s)", backupRestoreUsage)
	}
	dataDir, err := backupDataDir(env, *cfgPath, *dataDirFlag)
	if err != nil {
		return err
	}
	keys, err := backupPublicKeys(dataDir, pubs, *allowDev)
	if err != nil {
		return err
	}
	res, err := backup.Restore(ctx, backup.RestoreOptions{BackupDir: *dir, DataDir: dataDir, Keys: keys, Force: *force, SkipNewerRevocations: *skipRevocations})
	if err != nil {
		return err
	}
	fmt.Fprintln(env.Stderr, "alixai: restored; what was in place before is kept in the pre_restore_dir below, with any audit records and receipts written after the backup")
	if res.CarriedRevocations > 0 {
		fmt.Fprintf(env.Stderr, "alixai: %d key revocation(s) made after the backup were kept: those keys stay revoked\n", res.CarriedRevocations)
	}
	if *skipRevocations {
		fmt.Fprintln(env.Stderr, "alixai: warning: keys revoked after the backup was taken are active again; revoke them again with 'alixai trust revoke-key' and 'alixai identity revoke-issuer-key'")
	}
	return backupEmit(env.Stdout, map[string]any{"backup_id": res.BackupID, "data_dir": dataDir, "pre_restore_dir": res.PreRestoreDir, "carried_revocations": res.CarriedRevocations})
}
