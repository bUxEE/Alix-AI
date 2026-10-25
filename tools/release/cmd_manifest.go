package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/release"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/models/registry"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// devKeyWarning is printed whenever the published TEST-ONLY key signs or is accepted.
const devKeyWarning = "WARNING: this is the published TEST-ONLY key (tools/devkeys). Anyone can sign with it. It is for local runs and tests; a real release is signed with the offline key of M5-H02."

func cmdManifest(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("manifest", stderr)
	root := fs.String("C", "", "module root (default: the directory with go.mod above the working directory)")
	version := fs.String("version", "", "release version, semantic (required)")
	commit := fs.String("commit", "", "the 40-character commit id the release is built from (required)")
	createdAt := fs.String("created-at", "", "created_at, RFC 3339 (default: now; CI passes the commit time)")
	var artifacts, images, composes multiFlag
	fs.Var(&artifacts, "artifact", "KIND=FILE: a release file; kind is binary, archive, sbom or checksums (repeatable, at least one)")
	fs.Var(&images, "image", "NAME=REPO[:TAG]@sha256:DIGEST: an image this release builds (repeatable)")
	fs.Var(&composes, "compose", "a compose file whose pinned third-party images are listed (repeatable; default: the container stack and the LocalAI runtime)")
	gallery := fs.String("gallery", "", "the pinned backend catalogue (default: deploy/container/localai/config/backend-gallery.yaml)")
	registryDir := fs.String("registry", "", "a model registry directory (default: the registry embedded in this build)")
	out := fs.String("out", "", "output file (default: standard output)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag("version", *version); err != nil {
		return err
	}
	if err := requireFlag("commit", *commit); err != nil {
		return err
	}
	modRoot, err := moduleRootFlag(*root)
	if err != nil {
		return err
	}

	created := time.Now().UTC()
	if *createdAt != "" {
		if created, err = time.Parse(time.RFC3339, *createdAt); err != nil {
			return usagef("-created-at %q is not RFC 3339", *createdAt)
		}
	}
	in := manifestInputs{Version: *version, Commit: *commit, CreatedAt: created, BuiltImages: images, ComposeFiles: composes, BackendGallery: *gallery}
	for _, a := range artifacts {
		kind, path, ok := strings.Cut(a, "=")
		if !ok || kind == "" || path == "" {
			return usagef("-artifact %q is not KIND=FILE", a)
		}
		in.Artifacts = append(in.Artifacts, artifactInput{Kind: kind, Path: path})
	}
	if len(in.ComposeFiles) == 0 {
		in.ComposeFiles = []string{
			filepath.Join(modRoot, "deploy", "container", "stack", "compose.yaml"),
			filepath.Join(modRoot, "deploy", "container", "localai", "compose.yaml"),
		}
	}
	quadlets, err := filepath.Glob(filepath.Join(modRoot, "deploy", "container", "quadlet", "*.container"))
	if err != nil {
		return err
	}
	in.QuadletFiles = quadlets
	if in.BackendGallery == "" {
		in.BackendGallery = filepath.Join(modRoot, "deploy", "container", "localai", "config", "backend-gallery.yaml")
	}
	if *registryDir == "" {
		in.Registry, err = models.LoadFS(registry.FS)
	} else {
		in.Registry, err = models.LoadDir(*registryDir)
	}
	if err != nil {
		return fmt.Errorf("model registry: %w", err)
	}

	m, err := buildReleaseManifest(in)
	if err != nil {
		return err
	}
	raw, err := marshalIndent(m)
	if err != nil {
		return err
	}
	if _, err := release.Parse(raw); err != nil {
		return fmt.Errorf("built manifest does not parse back: %w", err)
	}
	return writeOutput(*out, raw, stdout)
}

func cmdSign(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("sign", stderr)
	manifest := fs.String("manifest", "", "the release manifest to sign (required)")
	out := fs.String("out", "", "where to write the signed manifest (default: replace -manifest)")
	keyFile := fs.String("key", "", "the Ed25519 private key, a PKCS#8 PEM file (as `alixai package keygen` writes)")
	keyID := fs.String("key-id", "", "the id the public key is published under (required with -key)")
	dev := fs.Bool("dev-key", false, "sign with the published TEST-ONLY key; for local runs and tests only")
	validFor := fs.Duration("valid-for", release.DefaultSignatureValidity, "how long the signature stays valid")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag("manifest", *manifest); err != nil {
		return err
	}
	var key trust.SigningKey
	switch {
	case *dev && (*keyFile != "" || *keyID != ""):
		return usagef("-dev-key cannot be combined with -key or -key-id")
	case *dev:
		key = devkeys.SigningKey()
		fmt.Fprintln(stderr, devKeyWarning)
	case *keyFile == "" || *keyID == "":
		return usagef("give -key FILE and -key-id ID, or -dev-key for a local run")
	default:
		priv, warnings, err := trust.LoadPrivateKeyFile(*keyFile)
		if err != nil {
			return err
		}
		for _, w := range warnings {
			fmt.Fprintln(stderr, "warning:", w)
		}
		if trust.IsTestOnlyKey(priv.Public().(ed25519.PublicKey)) {
			return fmt.Errorf("%s is a published TEST-ONLY key; use -dev-key if a local test signature is what you want", *keyFile)
		}
		key = trust.SigningKey{KeyID: *keyID, Key: priv}
	}
	raw, err := os.ReadFile(*manifest)
	if err != nil {
		return err
	}
	signed, err := release.Sign(raw, key, clock.Real{}.Now(), *validFor)
	if err != nil {
		return err
	}
	dst := *out
	if dst == "" {
		dst = *manifest
	}
	if err := writeFileAtomic(dst, signed); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s with key %s\n", dst, key.KeyID)
	return nil
}

func cmdVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("verify", stderr)
	manifest := fs.String("manifest", "", "the release manifest (required)")
	var pubkeys multiFlag
	fs.Var(&pubkeys, "pubkey", "ID=FILE: a trusted release public key, one base64 line (repeatable)")
	allowDev := fs.Bool("allow-dev-key", false, "also trust the published TEST-ONLY key; for local runs and tests only")
	dir := fs.String("dir", "", "a directory of release files to check against the manifest")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireFlag("manifest", *manifest); err != nil {
		return err
	}
	keys := release.StaticKeys{Keys: map[string]ed25519.PublicKey{}, AllowTestOnly: *allowDev}
	for _, kv := range pubkeys {
		id, file, ok := strings.Cut(kv, "=")
		if !ok || id == "" || file == "" {
			return usagef("-pubkey %q is not ID=FILE", kv)
		}
		text, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		pub, err := trust.DecodePublicKey(string(text))
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		keys.Keys[id] = pub
	}
	if *allowDev {
		if _, taken := keys.Keys[devkeys.KeyID]; !taken {
			keys.Keys[devkeys.KeyID] = devkeys.PublicKey()
		}
	}
	if len(keys.Keys) == 0 {
		return usagef("no trusted key: give -pubkey ID=FILE")
	}
	raw, err := os.ReadFile(*manifest)
	if err != nil {
		return err
	}
	m, acc, err := release.Verify(ctx, raw, keys, clock.Real{})
	if err != nil {
		return explainTestOnlyRefusal(err, raw, keys)
	}
	if *dir != "" {
		abs, _ := filepath.Abs(*manifest)
		skip := ""
		if filepath.Dir(abs) == mustAbs(*dir) {
			skip = filepath.Base(abs)
		}
		if err := m.VerifyFiles(*dir, skip); err != nil {
			return err
		}
	}
	if trust.IsTestOnlyKey(keys.Keys[acc.KeyID]) {
		fmt.Fprintln(stderr, devKeyWarning)
	}
	fmt.Fprintf(stdout, "VERIFIED release %s (commit %s) signed by %s, valid until %s: %d artifacts, %d images, %d backends, %d models\n",
		m.Signed.Version, m.Signed.Commit, acc.KeyID, acc.ExpiresAt.Format(time.RFC3339),
		len(m.Signed.Artifacts), len(m.Signed.Images), len(m.Signed.Backends), len(m.Signed.Models))
	return nil
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// explainTestOnlyRefusal adds a hint to a failed verification when the manifest was signed with a published
// TEST-ONLY key that was named with -pubkey but is not trusted: the verifier reports that as an unknown key.
func explainTestOnlyRefusal(err error, raw []byte, keys release.StaticKeys) error {
	if keys.AllowTestOnly {
		return err
	}
	m, perr := release.Parse(raw)
	if perr != nil {
		return err
	}
	for _, s := range m.Signatures {
		if pub, ok := keys.Keys[s.KeyID]; ok && trust.IsTestOnlyKey(pub) {
			return fmt.Errorf("%w\nnote: key %s is a published TEST-ONLY key and is trusted only with -allow-dev-key", err, s.KeyID)
		}
	}
	return err
}
