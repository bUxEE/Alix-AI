package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func init() {
	config.Register(keysSectionName, &keysSection)
	Register(Command{
		Name:    "keys init",
		Summary: "create the platform signing key for audit checkpoints under the data directory and register its public half",
		Run:     runKeysInit,
	})
}

const (
	keysSectionName = "keys"
	// keysDirName is the directory below data_dir that holds the platform keys.
	keysDirName = "keys"
	// defaultCheckpointKeyID is the id, and the file stem, of the platform
	// checkpoint signing key `keys init` creates and `serve` looks for.
	defaultCheckpointKeyID = "audit-checkpoint"
	keyFileSuffix          = ".key"
)

// keysConfig is the "keys" section: where the platform checkpoint signing key
// is and how often signed audit checkpoints are made.
//
//	keys:
//	  checkpoint_key_file: ""    # PKCS#8 PEM private key; empty means <data_dir>/keys/audit-checkpoint.key if it exists
//	  checkpoint_key_id: ""      # its id in the trust store; empty means the file name without .key
//	  checkpoint_every: 100      # a signed checkpoint after every N events of a chain (0: only the sweeps)
//	  checkpoint_interval: 5m    # how often the periodic sweep signs the chains whose head moved
//
// A key that is configured but cannot be used (missing, not a regular file,
// not an Ed25519 PKCS#8 key, readable by other users, not registered or
// revoked in the trust store) stops `serve` from starting: a service that was
// told to sign checkpoints never quietly runs without them. With nothing
// configured and no key file at the default place, `serve` runs and says that
// checkpoints are not signed.
type keysConfig struct {
	CheckpointKeyFile  string        `yaml:"checkpoint_key_file"`
	CheckpointKeyID    string        `yaml:"checkpoint_key_id"`
	CheckpointEvery    int64         `yaml:"checkpoint_every"`
	CheckpointInterval time.Duration `yaml:"checkpoint_interval"`
}

var keysSection = keysConfig{CheckpointEvery: 100, CheckpointInterval: 5 * time.Minute}

// keyIDRE is the grammar of a key id that is also a file name (the one
// trust.WriteKeyFiles requires).
var keyIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Validate implements the section validation hook.
func (c keysConfig) Validate() error {
	if strings.ContainsRune(c.CheckpointKeyFile, 0) {
		return errors.New("checkpoint_key_file must not contain NUL")
	}
	if c.CheckpointKeyID != "" && !keyIDRE.MatchString(c.CheckpointKeyID) {
		return fmt.Errorf("checkpoint_key_id must match %s", keyIDRE)
	}
	if c.CheckpointEvery < 0 {
		return errors.New("checkpoint_every must not be negative")
	}
	if c.CheckpointInterval <= 0 {
		return errors.New("checkpoint_interval must be above zero")
	}
	return nil
}

// keysDir is where the platform keys live: <data_dir>/keys.
func keysDir(dataDir string) string { return filepath.Join(dataDir, keysDirName) }

// checkpointKey is a loaded platform checkpoint signing key and where it came
// from.
type checkpointKey struct {
	signing trust.SigningKey
	path    string
}

// loadCheckpointKey reads the platform checkpoint signing key named by the keys
// section, or found at the default place. It returns (nil, nil) only when
// nothing is configured and there is no file at the default place: not
// provisioned. Everything else that is wrong is an error, whatever it is: the
// service refuses to start rather than run unsigned when it was told to sign.
//
// It does not touch the database; checkCheckpointKeyRegistered does.
func loadCheckpointKey(cfg keysConfig, dataDir string) (*checkpointKey, error) {
	path := cfg.CheckpointKeyFile
	if path == "" {
		path = filepath.Join(keysDir(dataDir), defaultCheckpointKeyID+keyFileSuffix)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
	}
	id := cfg.CheckpointKeyID
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(path), keyFileSuffix)
		if !keyIDRE.MatchString(id) {
			return nil, fmt.Errorf("the checkpoint signing key file %s does not name a key id: set keys.checkpoint_key_id", path)
		}
	}
	priv, warnings, err := trust.LoadPrivateKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("the checkpoint signing key cannot be used: %w", err)
	}
	if len(warnings) > 0 {
		// A signing key other users can read lets them forge checkpoints, which
		// is exactly what a signed checkpoint exists to rule out (SEC-081, SEC-086).
		return nil, fmt.Errorf("the checkpoint signing key cannot be used: %s", warnings[0])
	}
	return &checkpointKey{signing: trust.SigningKey{KeyID: id, Key: priv}, path: path}, nil
}

// checkCheckpointKeyRegistered requires the public half of the loaded key to be
// the active key the trust store holds for the platform under the same id, for
// the audit-checkpoint purpose: without it audit.VerifyChain could not believe a
// single checkpoint the key signs, and a revoked key would go on signing.
//
// A key registered before purposes existed (by `alixai trust add-key`, which the
// migration cannot recognise) has none: the configured checkpoint key file is
// the operator saying what it is for, so it is classified here, once. A key
// registered for another purpose is refused: a release key never signs
// checkpoints (H1).
func checkCheckpointKeyRegistered(ctx context.Context, db *store.DB, clk clock.Clock, k *checkpointKey) error {
	ts := trust.NewStore(db, clk)
	stored, err := ts.LookupKey(ctx, trust.PlatformApplicationID, k.signing.KeyID)
	switch {
	case errors.Is(err, trust.ErrUnknownKey):
		return fmt.Errorf("the checkpoint signing key %q is not registered in the trust store; run `alixai keys init` to register it", k.signing.KeyID)
	case err != nil:
		return fmt.Errorf("the trust store could not be read for the checkpoint signing key: %w", err)
	case stored.Status != trust.KeyActive:
		return fmt.Errorf("the checkpoint signing key %q is %s in the trust store; create a new one with `alixai keys init -key-id ...` and set keys.checkpoint_key_file", k.signing.KeyID, stored.Status)
	case !stored.PublicKey.Equal(k.signing.Key.Public().(ed25519.PublicKey)):
		return fmt.Errorf("the checkpoint signing key file does not match the key %q registered in the trust store", k.signing.KeyID)
	case stored.Purpose == trust.PurposeNone:
		if err := ts.SetKeyPurpose(ctx, trust.PlatformApplicationID, k.signing.KeyID, trust.PurposeAuditCheckpoint); err != nil {
			return fmt.Errorf("the checkpoint signing key %q cannot be marked as the checkpoint key: %w", k.signing.KeyID, err)
		}
	case stored.Purpose != trust.PurposeAuditCheckpoint:
		return fmt.Errorf("the checkpoint signing key %q is registered for %q in the trust store, not for audit checkpoints; create a checkpoint key with `alixai keys init -key-id ...`", k.signing.KeyID, stored.Purpose)
	}
	return nil
}

// operatorStore loads the configuration the way `serve` does and opens the
// store it names: the start of every operator command that works on the
// database. The caller closes the store.
func operatorStore(ctx context.Context, env *Env, configPath string) (*store.DB, config.CoreConfig, error) {
	if err := env.Config.LoadFrom(configPath, env.LookupEnv); err != nil {
		return nil, config.CoreConfig{}, err
	}
	core := env.Config.Core()
	opts := store.FromConfig()
	opts.Path = filepath.Join(core.DataDir.String(), store.DefaultFileName)
	opts.Logger = slog.New(slog.DiscardHandler)
	db, err := store.Open(ctx, opts)
	if err != nil {
		return nil, core, fmt.Errorf("open the store: %w", err)
	}
	return db, core, nil
}

const keysInitUsage = "alixai keys init [-config file.yaml] [-key-id ID]"

// runKeysInit creates the platform signing key that signs audit chain-head
// checkpoints (internal/audit, SEC-086): a new Ed25519 key pair under
// <data_dir>/keys (the directory 0700, the private key file 0600 and created
// that way, never widened afterwards), and the registration of its public half
// in the trust store under the platform application, which is what lets
// `alixai serve` and an audit verification believe a checkpoint. The key is
// registered for the audit-checkpoint purpose only: it never verifies a release
// manifest or any other platform artifact (H1).
//
// It is idempotent and never overwrites: when the key file is already there it
// is checked and only registered, which also repairs a fresh database that sits
// next to a restored key. The private key is written to its file and nowhere
// else; it is not printed and not logged.
func runKeysInit(ctx context.Context, env *Env, args []string) error {
	fs := env.FlagSet("keys init")
	cfgPath := env.ConfigFlag(fs)
	keyID := fs.String("key-id", defaultCheckpointKeyID, "id of the key, also its file name: lower case letters, digits, '.', '_' and '-'")
	if err := env.ParseFlags(fs, keysInitUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if !keyIDRE.MatchString(*keyID) {
		return Usagef("-key-id %q must match %s", *keyID, keyIDRE)
	}

	// The store first: it creates the data directory owner-only.
	db, core, err := operatorStore(ctx, env, *cfgPath)
	if err != nil {
		return err
	}
	defer db.Close()
	dir := keysDir(core.DataDir.String())
	privPath := filepath.Join(dir, *keyID+keyFileSuffix)
	pubPath := filepath.Join(dir, *keyID+".pub")

	created := false
	var priv ed25519.PrivateKey
	if _, statErr := os.Lstat(privPath); statErr == nil {
		var warnings []string
		if priv, warnings, err = trust.LoadPrivateKeyFile(privPath); err != nil {
			return fmt.Errorf("keys init: the existing key file cannot be used: %w", err)
		}
		if len(warnings) > 0 {
			return fmt.Errorf("keys init: the existing key file cannot be used: %s", warnings[0])
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		if _, priv, err = trust.GenerateKey(); err != nil {
			return err
		}
		if _, _, err = trust.WriteKeyFiles(dir, *keyID, priv); err != nil {
			return err
		}
		created = true
	} else {
		return fmt.Errorf("keys init: %s cannot be examined: %w", privPath, statErr)
	}
	pub := priv.Public().(ed25519.PublicKey)

	if err := registerPlatformKey(ctx, db, *keyID, pub); err != nil {
		if created {
			// Leave nothing half made: a rerun starts clean.
			_ = os.Remove(privPath)
			_ = os.Remove(pubPath)
		}
		return fmt.Errorf("keys init: register the public key in the trust store: %w", err)
	}

	state := "already present"
	if created {
		state = "created"
	}
	_, err = fmt.Fprintf(env.Stdout, "platform checkpoint key %s (%s)\nkey id:      %s\nprivate key: %s (owner-only; never leaves this machine)\npublic key:  %s\n"+
		"public key (base64): %s\nregistered in the trust store for %s (purpose audit-checkpoint)\n",
		*keyID, state, *keyID, privPath, pubPath, trust.EncodePublicKey(pub), trust.PlatformApplicationID)
	return err
}

// registerPlatformKey registers pub under the platform application for the
// audit-checkpoint purpose, or confirms that the very same key is already
// registered and active (giving it that purpose when it was registered before
// purposes existed). A key registered for another purpose is a conflict.
func registerPlatformKey(ctx context.Context, db *store.DB, keyID string, pub ed25519.PublicKey) error {
	ts := trust.NewStore(db, clock.Real{})
	_, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, keyID, pub, "audit checkpoint signing key", trust.WithPurpose(trust.PurposeAuditCheckpoint))
	if err == nil {
		return nil
	}
	if !errors.Is(err, trust.ErrKeyConflict) {
		return err
	}
	stored, lerr := ts.LookupKey(ctx, trust.PlatformApplicationID, keyID)
	if lerr == nil && stored.Status == trust.KeyActive && stored.PublicKey.Equal(pub) {
		switch stored.Purpose {
		case trust.PurposeAuditCheckpoint:
			return nil
		case trust.PurposeNone:
			return ts.SetKeyPurpose(ctx, trust.PlatformApplicationID, keyID, trust.PurposeAuditCheckpoint)
		}
		return contracts.Newf(contracts.ErrConflict, "key %q is registered for %q, not for audit checkpoints; choose another -key-id", keyID, stored.Purpose)
	}
	return contracts.Wrap(contracts.ErrConflict, err, "a different key is registered under this id, or this key was revoked; choose another -key-id")
}
