package trust

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Key files. A private key is one PKCS#8 PEM block ("PRIVATE KEY") holding an
// Ed25519 key: the format openssl, Node's crypto and Python's cryptography read.
// A public key file is one line of the raw 32 bytes in standard base64, the
// form docs/integration/application-package.md section 13 uses and RegisterKey
// takes.

// GenerateKey returns a new Ed25519 key pair from the system's secure random
// source.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fail(contracts.ErrInternal, err, "cannot generate a key")
	}
	return pub, priv, nil
}

// MarshalPrivateKeyPEM encodes priv as a PKCS#8 PEM block.
func MarshalPrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fail(contracts.ErrValidationFailed, nil, "private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(priv))
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fail(contracts.ErrInternal, err, "cannot encode the private key")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKeyPEM decodes the first PEM block of data, which must be a
// PKCS#8 "PRIVATE KEY" holding an Ed25519 key. Text before the block is
// ignored (the TEST-ONLY key file starts with a warning); anything else is an
// error.
func ParsePrivateKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fail(contracts.ErrValidationFailed, nil, "no PKCS#8 PRIVATE KEY PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fail(contracts.ErrValidationFailed, nil, "private key is not valid PKCS#8")
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok || len(priv) != ed25519.PrivateKeySize {
		return nil, fail(contracts.ErrValidationFailed, nil, "private key is not an Ed25519 key")
	}
	return priv, nil
}

// EncodePublicKey returns pub as standard base64 with padding.
func EncodePublicKey(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// DecodePublicKey reads a public key file's content: the raw 32 bytes in
// standard base64, surrounding whitespace ignored.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fail(contracts.ErrValidationFailed, nil, "public key must be %d bytes in standard base64", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// keyFileNameRE is the grammar of the base name of generated key files: safe
// in a path on every platform and never able to climb out of the directory.
var keyFileNameRE = appIDRE

// WriteKeyFiles writes priv to dir/<name>.key (PKCS#8 PEM, owner only) and its
// public key to dir/<name>.pub (mode 0644). dir is created owner only if it
// does not exist. Neither file is overwritten: an existing file is an error,
// and a failure leaves no half-written pair behind. name must match the
// application ID grammar (lower case, digits, '.', '_', '-').
//
// Owner only is mode 0600/0700 on Unix and, on Windows, a protected ACL that
// grants the current user alone (internal/fsperm), set on the private key file
// before the key is written into it. An existing dir is left as it is.
func WriteKeyFiles(dir, name string, priv ed25519.PrivateKey) (privPath, pubPath string, err error) {
	if !keyFileNameRE.MatchString(name) {
		return "", "", fail(contracts.ErrValidationFailed, nil, "key file name %q must match %s", clip(name), keyFileNameRE)
	}
	pemBytes, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		return "", "", err
	}
	if _, err := fsperm.MkdirOwnerOnly(dir); err != nil {
		return "", "", fail(contracts.ErrUnavailable, err, "cannot create %s", dir)
	}
	privPath = filepath.Join(dir, name+".key")
	pubPath = filepath.Join(dir, name+".pub")
	if err := createExclusive(privPath, pemBytes, 0o600); err != nil {
		return "", "", err
	}
	pub := priv.Public().(ed25519.PublicKey)
	if err := createExclusive(pubPath, []byte(EncodePublicKey(pub)+"\n"), 0o644); err != nil {
		_ = os.Remove(privPath)
		return "", "", err
	}
	return privPath, pubPath, nil
}

// createExclusive creates path, which must not exist, with the given mode and
// content. The mode is applied at creation, and a 0600 file is made owner only
// (fsperm.CreateFile, which sets the Windows ACL too) before the data is
// written, so it is never readable by others, not even for an instant.
func createExclusive(path string, data []byte, mode os.FileMode) error {
	var f *os.File
	var err error
	if mode == 0o600 {
		f, err = fsperm.CreateFile(path)
	} else {
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	}
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(contracts.ErrConflict, nil, "%s already exists; it is never overwritten", path)
		}
		return fail(contracts.ErrUnavailable, err, "cannot create %s", path)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		return fail(contracts.ErrUnavailable, err, "cannot write %s", path)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fail(contracts.ErrUnavailable, err, "cannot write %s", path)
	}
	return nil
}

// maxKeyFileBytes bounds a key file read.
const maxKeyFileBytes = 64 << 10

// LoadPrivateKeyFile reads a PKCS#8 PEM private key file. warnings lists
// problems that do not stop the load: a file that is not owner only (on Unix
// readable or writable by group or others, on Windows an ACL that inherits
// from its directory or grants another user; internal/fsperm). The key itself
// is never in an error message.
func LoadPrivateKeyFile(path string) (key ed25519.PrivateKey, warnings []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fail(contracts.ErrValidationFailed, nil, "cannot open key file %s", path)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, nil, fail(contracts.ErrValidationFailed, nil, "%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil || len(data) > maxKeyFileBytes {
		return nil, nil, fail(contracts.ErrValidationFailed, nil, "cannot read key file %s", path)
	}
	key, err = ParsePrivateKeyPEM(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if w := OwnerOnlyWarning(path); w != "" {
		warnings = append(warnings, w)
	}
	return key, warnings, nil
}

// OwnerOnlyWarning returns why the file at path is not owner only, with the
// fix for the platform, or "" when it is (or cannot be examined: the read that
// follows reports that).
func OwnerOnlyWarning(path string) string {
	err := fsperm.Check(path, false)
	if !errors.Is(err, fsperm.ErrNotOwnerOnly) {
		return ""
	}
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("%s is accessible by other users (%v); restrict it to the account the service runs as: "+
			"icacls \"%s\" /inheritance:r /grant:r \"<account>:F\", then icacls \"%s\" /remove \"<other account>\" for any other account it still lists", path, err, path, path)
	}
	st, serr := os.Stat(path)
	if serr != nil {
		return fmt.Sprintf("%s is accessible by other users; restrict it to the owner (chmod 600)", path)
	}
	return fmt.Sprintf("%s is accessible by other users (mode %04o); restrict it to the owner (chmod 600)", path, st.Mode().Perm())
}
