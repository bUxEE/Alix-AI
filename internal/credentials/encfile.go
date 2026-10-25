package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/fsperm"
)

// KeySource supplies the 32-byte master key of an encrypted file. It is asked on
// every operation and the key is wiped after it: nothing is cached.
type KeySource interface {
	// MasterKey returns the key. create says that the file holds nothing yet, so
	// a source may make a key; with create false a missing key is ErrNotFound (a
	// file full of records must never be re-keyed by accident).
	MasterKey(ctx context.Context, create bool) ([]byte, error)
}

const masterKeyLen = 32

// OSStoreKeySource keeps the master key as an entry of the OS secret store.
type OSStoreKeySource struct {
	Store OSStore
	// Service and User name the entry; empty means OSStoreService and
	// "master-key".
	Service, User string
}

// MasterKey implements KeySource.
func (k OSStoreKeySource) MasterKey(ctx context.Context, create bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	svc, user := k.Service, k.User
	if svc == "" {
		svc = OSStoreService
	}
	if user == "" {
		user = "master-key"
	}
	v, err := k.Store.Get(svc, user)
	switch {
	case err == nil:
		return decodeKey(v)
	case !errors.Is(err, ErrOSStoreNotFound):
		return nil, errBackend
	case !create:
		return nil, ErrNotFound
	}
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	if k.Store.Set(svc, user, hex.EncodeToString(key)) != nil {
		wipe(key)
		return nil, errBackend
	}
	return key, nil
}

// KeyFileSource keeps the master key in a file that only the owner can read
// (mode 0600, a protected owner-only ACL on Windows), created with a random key
// when the encrypted file is first written. A key file with looser permissions
// is refused. Keep it on another volume or directory than the encrypted file
// where the host allows; where the OS store is available prefer OSStoreKeySource.
type KeyFileSource struct{ Path string }

// MasterKey implements KeySource.
func (k KeyFileSource) MasterKey(ctx context.Context, create bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(k.Path)
	switch {
	case err == nil:
		if err := checkOwnerOnly(k.Path, false); err != nil {
			wipe(raw)
			return nil, err
		}
		key, derr := decodeKey(strings.TrimSpace(string(raw)))
		wipe(raw)
		return key, derr
	case !errors.Is(err, os.ErrNotExist):
		return nil, errBackend
	case !create:
		return nil, ErrNotFound
	}
	// serve and an operator command may both find no key and create one. The lock
	// sits beside the key directory (not inside it) so creators serialize before
	// mkdir: a lock inside the directory raced with ensureSecureDir on Linux, where
	// rename of an empty dir replaces another empty dir and Lock then saw no parent.
	dir := filepath.Dir(k.Path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, errBackend
	}
	unlock, err := fsperm.Lock(dir + ".lock")
	if err != nil {
		return nil, errBackend
	}
	defer func() { _ = unlock() }()
	if err := ensureSecureDir(dir); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(k.Path); err == nil {
		return k.read() // another process made it first
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errBackend
	}
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	if err := writeFileOwnerOnly(k.Path, []byte(hex.EncodeToString(key)+"\n"), true); err != nil {
		wipe(key)
		// Contended create or a lost exclusive link: always prefer the key that won.
		if got, rerr := k.read(); rerr == nil {
			return got, nil
		}
		return nil, errBackend
	}
	return key, nil
}

// read reads the existing key file, which must be owner only.
func (k KeyFileSource) read() ([]byte, error) {
	raw, err := os.ReadFile(k.Path)
	if err != nil {
		return nil, errBackend
	}
	if err := checkOwnerOnly(k.Path, false); err != nil {
		wipe(raw)
		return nil, err
	}
	key, derr := decodeKey(strings.TrimSpace(string(raw)))
	wipe(raw)
	return key, derr
}

func newKey() ([]byte, error) {
	key := make([]byte, masterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, errBackend
	}
	return key, nil
}

func decodeKey(s string) ([]byte, error) {
	key, err := hex.DecodeString(s)
	if err != nil || len(key) != masterKeyLen {
		return nil, errBackend
	}
	return key, nil
}

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// EncryptedFile is a Backend that keeps every secret as one record of a single
// JSON file, each sealed with AES-256-GCM (authenticated encryption) under the
// master key, with a fresh random 96-bit nonce per write. The record's slot key
// and the slot's binding are the additional authenticated data, so a record
// copied to another reference or another provider, deployment, application or
// tenant fails to open, and so does a record that was altered.
//
// The directory is created owner only and the file is rewritten atomically
// (temporary file, fsync, rename), owner only from its first byte. Nothing is
// cached: every Get reads and opens the file again, which is what makes a
// rotation or a revocation effective on the very next call without a restart.
type EncryptedFile struct {
	path string
	keys KeySource
	mu   sync.Mutex
}

var _ Backend = (*EncryptedFile)(nil)

// FileName is the default name of the encrypted file inside its directory.
const FileName = "credentials.enc.json"

const encFormat = "alixai.credfile/v1"

type encDoc struct {
	Format  string               `json:"format"`
	Entries map[string]encRecord `json:"entries"`
}

type encRecord struct {
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

// NewEncryptedFile returns the backend for the file at path (its directory is
// created owner only on the first write).
func NewEncryptedFile(path string, keys KeySource) (*EncryptedFile, error) {
	if path == "" || keys == nil {
		return nil, errBackend
	}
	return &EncryptedFile{path: path, keys: keys}, nil
}

// Name implements Backend.
func (*EncryptedFile) Name() BackendName { return BackendEncryptedFile }

// ReadOnly implements Backend.
func (*EncryptedFile) ReadOnly() bool { return false }

func aadFor(s Slot) []byte { return []byte(encFormat + "\x00" + slotKey(s) + "\x00" + s.Binding) }

// load reads the file; a missing file is an empty document.
func (e *EncryptedFile) load() (encDoc, error) {
	raw, err := os.ReadFile(e.path)
	if errors.Is(err, os.ErrNotExist) {
		return encDoc{Format: encFormat, Entries: map[string]encRecord{}}, nil
	}
	if err != nil {
		return encDoc{}, errBackend
	}
	if err := checkOwnerOnly(e.path, false); err != nil {
		return encDoc{}, err
	}
	var d encDoc
	if json.Unmarshal(raw, &d) != nil || d.Format != encFormat {
		return encDoc{}, errBackend
	}
	if d.Entries == nil {
		d.Entries = map[string]encRecord{}
	}
	return d, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, errBackend
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, errBackend
	}
	return g, nil
}

// Get implements Backend.
func (e *EncryptedFile) Get(ctx context.Context, slot Slot) (Secret, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.load()
	if err != nil {
		return "", err
	}
	rec, ok := d.Entries[slotKey(slot)]
	if !ok {
		return "", ErrNotFound
	}
	key, err := e.keys.MasterKey(ctx, false)
	if err != nil {
		return "", err
	}
	defer wipe(key)
	g, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce, err1 := base64.StdEncoding.DecodeString(rec.Nonce)
	ct, err2 := base64.StdEncoding.DecodeString(rec.CT)
	if err1 != nil || err2 != nil || len(nonce) != g.NonceSize() {
		return "", errBackend
	}
	pt, err := g.Open(nil, nonce, ct, aadFor(slot))
	if err != nil {
		return "", errBackend // altered, copied to another slot, or the wrong master key
	}
	v := Secret(pt)
	wipe(pt)
	return v, nil
}

// Put implements Backend.
func (e *EncryptedFile) Put(ctx context.Context, slot Slot, value Secret) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ensureSecureDir(filepath.Dir(e.path)); err != nil {
		return err
	}
	d, err := e.load()
	if err != nil {
		return err
	}
	key, err := e.keys.MasterKey(ctx, len(d.Entries) == 0)
	if err != nil {
		return err
	}
	defer wipe(key)
	g, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return errBackend
	}
	d.Entries[slotKey(slot)] = encRecord{
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(g.Seal(nil, nonce, []byte(value.Reveal()), aadFor(slot))),
	}
	return e.save(d)
}

// Delete implements Backend. A record that is already absent is not an error.
func (e *EncryptedFile) Delete(_ context.Context, slot Slot) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.load()
	if err != nil {
		return err
	}
	if _, ok := d.Entries[slotKey(slot)]; !ok {
		return nil
	}
	delete(d.Entries, slotKey(slot))
	return e.save(d)
}

func (e *EncryptedFile) save(d encDoc) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return errBackend
	}
	return writeFileOwnerOnly(e.path, raw, false)
}
