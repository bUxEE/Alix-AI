package cli

import (
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/trust"
)

func init() {
	Register(Command{
		Name:    "package sign",
		Summary: "list and hash a package directory and sign it with Ed25519 keys",
		Run:     runPackageSign,
	})
	Register(Command{
		Name:    "package keygen",
		Summary: "generate a development Ed25519 publisher key pair",
		Run:     runPackageKeygen,
	})
}

// defaultSignatureLifetime is how long a signature made by "package sign" is
// valid unless -valid-for or -expires-at say otherwise.
const defaultSignatureLifetime = 365 * 24 * time.Hour

// keyFlags collects the repeatable -key KEYID=FILE flag.
type keyFlags []string

func (k *keyFlags) String() string     { return strings.Join(*k, ",") }
func (k *keyFlags) Set(v string) error { *k = append(*k, v); return nil }

const signUsage = "alixai package sign -app ID -version V -key KEYID=KEYFILE [-key ...] [-valid-for 8760h | -expires-at TIME] [-signed-at TIME] [-append] DIR"

// runPackageSign signs the package in DIR: it lists every file outside
// signatures/ with its SHA-256 (refusing symbolic links, unsafe paths and
// stray files in signatures/), signs the canonical manifest with each key and
// writes DIR/signatures/signature-manifest.json. It needs no database: whether
// a key is trusted is decided later, where the package is installed.
//
// The application ID and version are flags, not read from alixai.yaml, so the
// signature binds exactly what the publisher states; the loader later checks
// that they equal the manifest's own.
func runPackageSign(_ context.Context, env *Env, args []string) error {
	fs := env.FlagSet("package sign")
	app := fs.String("app", "", "application ID the package is for (required)")
	version := fs.String("version", "", "package version, semver (required)")
	var keys keyFlags
	fs.Var(&keys, "key", "KEYID=KEYFILE: a publisher key ID and its PKCS#8 PEM private key file; repeat to sign with several keys (at most 4)")
	validFor := fs.Duration("valid-for", defaultSignatureLifetime, "how long the signatures stay valid, from the signing time")
	expiresAt := fs.String("expires-at", "", "UTC time (RFC 3339, ending in Z) when the signatures expire; replaces -valid-for")
	signedAt := fs.String("signed-at", "", "UTC time (RFC 3339, ending in Z) to record as the signing time; default now. For reproducible builds and tests")
	appendSigs := fs.Bool("append", false, "add the signatures to the existing signature manifest instead of replacing it (key rotation overlap); the package must be unchanged")
	if err := env.ParseFlags(fs, signUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return Usagef("need exactly one package directory (usage: %s)", signUsage)
	}
	if *app == "" || *version == "" {
		return Usagef("-app and -version are required")
	}
	if len(keys) == 0 {
		return Usagef("at least one -key KEYID=KEYFILE is required")
	}
	if *expiresAt != "" && flagSet(fs, "valid-for") {
		return Usagef("-valid-for and -expires-at are mutually exclusive")
	}
	if *validFor <= 0 {
		return Usagef("-valid-for must be positive")
	}

	now := clock.Real{}.Now()
	if *signedAt != "" {
		t, err := time.Parse(time.RFC3339, *signedAt)
		if err != nil || !strings.HasSuffix(*signedAt, "Z") {
			return Usagef("-signed-at %q is not an RFC 3339 UTC time ending in Z", *signedAt)
		}
		now = t.UTC()
	}
	expires := now.Add(*validFor)
	if *expiresAt != "" {
		t, err := time.Parse(time.RFC3339, *expiresAt)
		if err != nil || !strings.HasSuffix(*expiresAt, "Z") {
			return Usagef("-expires-at %q is not an RFC 3339 UTC time ending in Z", *expiresAt)
		}
		expires = t.UTC()
	}

	signing := make([]trust.SigningKey, 0, len(keys))
	for _, spec := range keys {
		id, path, ok := strings.Cut(spec, "=")
		if !ok || id == "" || path == "" {
			return Usagef("-key %q is not KEYID=KEYFILE", spec)
		}
		priv, warnings, err := trust.LoadPrivateKeyFile(path)
		if err != nil {
			return err
		}
		for _, w := range warnings {
			fmt.Fprintf(env.Stderr, "alixai: warning: %s\n", w)
		}
		if trust.IsTestOnlyKey(priv.Public().(ed25519.PublicKey)) {
			fmt.Fprintf(env.Stderr, "alixai: note: key %s is a published TEST-ONLY key; only test trust stores accept it\n", id)
		}
		signing = append(signing, trust.SigningKey{KeyID: id, Key: priv})
	}

	sm, err := trust.SignPackageDir(fs.Arg(0), trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: *app, PackageVersion: *version},
		Keys:            signing,
		Now:             now,
		ExpiresAt:       expires,
		Append:          *appendSigs,
	})
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(sm.Signatures))
	for _, s := range sm.Signatures {
		ids = append(ids, s.KeyID)
	}
	_, err = fmt.Fprintf(env.Stdout, "signed %s %s: %d files, %d signature(s) by %s, expires %s -> %s\n",
		sm.Signed.ApplicationID, sm.Signed.PackageVersion, len(sm.Signed.Files), len(sm.Signatures),
		strings.Join(ids, ", "), sm.Signatures[len(sm.Signatures)-1].ExpiresAt, trust.SignatureManifestPath)
	return err
}

// flagSet reports whether the flag was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

const keygenUsage = "alixai package keygen -key-id ID [-out DIR]"

// runPackageKeygen writes a development key pair. It is a convenience for
// local work and tests: real publishers generate and guard their keys under
// their own key management, and register the public half with the trust store
// of every deployment that should trust them.
func runPackageKeygen(_ context.Context, env *Env, args []string) error {
	fs := env.FlagSet("package keygen")
	keyID := fs.String("key-id", "", "key ID, also the file name: lower case letters, digits, '.', '_' and '-' (required)")
	out := fs.String("out", ".", "directory for ID.key (private, mode 0600) and ID.pub (public); created with mode 0700 if missing")
	if err := env.ParseFlags(fs, keygenUsage, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Usagef("unexpected argument %q", fs.Arg(0))
	}
	if *keyID == "" {
		return Usagef("-key-id is required")
	}

	_, priv, err := trust.GenerateKey()
	if err != nil {
		return err
	}
	privPath, pubPath, err := trust.WriteKeyFiles(*out, *keyID, priv)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "alixai: DEVELOPMENT KEY. Keep %s secret and out of version control; it signs packages as %q. "+
		"Production publishers generate and guard their keys under their own key management.\n", privPath, *keyID)
	_, err = fmt.Fprintf(env.Stdout, "key id:      %s\nprivate key: %s\npublic key:  %s\npublic key (base64, for the trust store): %s\n",
		*keyID, privPath, pubPath, trust.EncodePublicKey(priv.Public().(ed25519.PublicKey)))
	return err
}
