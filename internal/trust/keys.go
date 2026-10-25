package trust

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("trust", migrations) }

// PlatformApplicationID is the application ID under which AlixAI's own
// signing keys (integration-audit reports, release manifests, audit chain
// checkpoints) are registered. It satisfies the application ID grammar so the
// same store and verifier serve it. The package loader must refuse a host
// application that declares this ID, or its keys would be trusted for the
// platform's artifacts.
const PlatformApplicationID = "alixai.platform"

// KeyStatus is the lifecycle state of a registered publisher key.
type KeyStatus string

const (
	// KeyActive keys verify signatures.
	KeyActive KeyStatus = "active"
	// KeyRetired keys were rotated out in the normal course: signatures made
	// before RetiredAt still verify until they expire, later ones do not.
	// signed_at is asserted by the signer, so retirement is not a defence
	// against a stolen key; revoke for that.
	KeyRetired KeyStatus = "retired"
	// KeyRevoked keys verify nothing, whatever the signature says about when
	// it was made. Revocation is permanent.
	KeyRevoked KeyStatus = "revoked"
)

// KeyPurpose is the one kind of platform artifact a key may sign (migration
// 0002). Every AlixAI key is registered under PlatformApplicationID, so without
// a purpose any of them would verify any platform artifact: the checkpoint key
// that serve keeps online could sign a release manifest. Verifications that
// name a purpose (PayloadOptions.Purpose) accept only keys registered for it.
type KeyPurpose string

const (
	// PurposeNone is a key with no purpose: every key registered before
	// purposes existed, and every key of a host application. It verifies only
	// what names no purpose (packages, events, integration-audit reports).
	PurposeNone KeyPurpose = ""
	// PurposeRelease keys sign release manifests (tools/release).
	PurposeRelease KeyPurpose = "release"
	// PurposeAuditCheckpoint keys sign audit chain checkpoints and tombstones
	// (`alixai keys init`).
	PurposeAuditCheckpoint KeyPurpose = "audit-checkpoint"
)

// ParseKeyPurpose returns the purpose named s ("" is PurposeNone).
func ParseKeyPurpose(s string) (KeyPurpose, error) {
	switch p := KeyPurpose(s); p {
	case PurposeNone, PurposeRelease, PurposeAuditCheckpoint:
		return p, nil
	}
	return "", fail(contracts.ErrValidationFailed, nil, "key purpose %q is not one of %q, %q", clip(s), PurposeRelease, PurposeAuditCheckpoint)
}

func checkPurpose(applicationID string, p KeyPurpose) error {
	if _, err := ParseKeyPurpose(string(p)); err != nil {
		return err
	}
	if p != PurposeNone && applicationID != PlatformApplicationID {
		return fail(contracts.ErrValidationFailed, nil, "only keys of %q have a purpose", PlatformApplicationID)
	}
	return nil
}

// KeyOption configures a key registration.
type KeyOption func(*keyOptions)

type keyOptions struct{ purpose KeyPurpose }

// WithPurpose registers the key for one purpose (platform keys only).
func WithPurpose(p KeyPurpose) KeyOption { return func(o *keyOptions) { o.purpose = p } }

// Key is a registered publisher key.
type Key struct {
	ApplicationID string
	KeyID         string
	PublicKey     ed25519.PublicKey
	Status        KeyStatus
	// Purpose is what the key may sign; PurposeNone for most keys.
	Purpose      KeyPurpose
	Label        string
	RegisteredAt time.Time
	// RetiredAt and RevokedAt are the zero time when not set.
	RetiredAt     time.Time
	RevokedAt     time.Time
	RevokedReason string
}

// Fingerprint returns the lower-case hex SHA-256 of the raw public key.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// KeyLookup is what the Verifier needs from a trust store.
type KeyLookup interface {
	// LookupKey returns the key registered for the application, or an error
	// that wraps ErrUnknownKey when there is none. Any other error means the
	// store could not answer, and the verification fails closed.
	LookupKey(ctx context.Context, applicationID, keyID string) (Key, error)
}

// Store is the SQLite trust store (module "trust"): publisher keys per
// application. It starts empty and trusts nothing until keys are registered.
type Store struct {
	db            *store.DB
	clk           clock.Clock
	allowTestOnly bool
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// AllowTestOnlyKeys lets the store register the published TEST-ONLY keys
// (IsTestOnlyKey). Only tests and developer composition roots use it: anyone
// can sign with those keys.
func AllowTestOnlyKeys() StoreOption { return func(s *Store) { s.allowTestOnly = true } }

// NewStore returns the trust store on db. The "trust" migrations must have been
// applied (they register themselves with store.Default when this package is
// imported; store.Open applies them). A nil clock means the wall clock.
func NewStore(db *store.DB, clk clock.Clock, opts ...StoreOption) *Store {
	if clk == nil {
		clk = clock.Real{}
	}
	s := &Store{db: db, clk: clk}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ KeyLookup = (*Store)(nil)

func checkApplicationID(id string) error {
	if !appIDRE.MatchString(id) || !validIdentifier(id) {
		return fail(contracts.ErrValidationFailed, nil, "application id %q is not a valid application id", clip(id))
	}
	return nil
}

func checkKeyID(id string) error {
	if !validIdentifier(id) {
		return fail(contracts.ErrValidationFailed, nil, "key id %q is not a valid identifier", clip(id))
	}
	return nil
}

func checkPublicKey(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fail(contracts.ErrValidationFailed, nil, "public key must be %d bytes, got %d", ed25519.PublicKeySize, len(pub))
	}
	return nil
}

// RegisterKey registers pub as an active publisher key for the application.
// It is an operator action; nothing in the verification path registers keys.
//
// It fails with ErrKeyConflict (contracts.ErrConflict) when the key ID is
// already registered for the application (whatever its status, a key is never
// replaced), when the same key material is already registered for the
// application under another ID, or when the key material has been revoked for
// any application: a key revoked as compromised must not return under a new
// name. It fails with ErrTestOnlyKey (contracts.ErrPermissionDenied) for a
// published TEST-ONLY key unless the store was opened with AllowTestOnlyKeys.
// WithPurpose registers a platform key for one purpose.
func (s *Store) RegisterKey(ctx context.Context, applicationID, keyID string, pub ed25519.PublicKey, label string, opts ...KeyOption) (Key, error) {
	if err := checkApplicationID(applicationID); err != nil {
		return Key{}, err
	}
	var o keyOptions
	for _, f := range opts {
		f(&o)
	}
	if err := checkPurpose(applicationID, o.purpose); err != nil {
		return Key{}, err
	}
	if err := checkKeyID(keyID); err != nil {
		return Key{}, err
	}
	if err := checkPublicKey(pub); err != nil {
		return Key{}, err
	}
	if !utf8.ValidString(label) || len(label) > 256 {
		return Key{}, fail(contracts.ErrValidationFailed, nil, "label must be valid UTF-8 of at most 256 bytes")
	}
	if !s.allowTestOnly && IsTestOnlyKey(pub) {
		return Key{}, fail(contracts.ErrPermissionDenied, ErrTestOnlyKey,
			"key %s is a published TEST-ONLY key and cannot be trusted by this store", quoteKeyID(keyID))
	}
	var out Key
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		return s.registerTx(ctx, tx, applicationID, keyID, pub, label, o.purpose, &out)
	})
	if err != nil {
		return Key{}, err
	}
	return out, nil
}

func (s *Store) registerTx(ctx context.Context, tx *store.Tx, applicationID, keyID string, pub ed25519.PublicKey, label string, purpose KeyPurpose, out *Key) error {
	fp := Fingerprint(pub)
	var revoked int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trust_publisher_keys WHERE fingerprint = ? AND status = 'revoked'`, fp).Scan(&revoked); err != nil {
		return wrapDB(err)
	}
	if revoked > 0 {
		return fail(contracts.ErrConflict, ErrKeyConflict, "this key material was revoked; a revoked key cannot be registered again")
	}
	now := s.clk.Now().UTC()
	_, err := tx.ExecContext(ctx,
		`INSERT INTO trust_publisher_keys
		   (application_id, key_id, public_key, fingerprint, status, label, registered_at, revoked_reason, purpose)
		 VALUES (?, ?, ?, ?, 'active', ?, ?, '', ?)`,
		applicationID, keyID, base64.StdEncoding.EncodeToString(pub), fp, label, now.UnixMilli(), string(purpose))
	if err != nil {
		if store.IsUniqueViolation(err) {
			return fail(contracts.ErrConflict, ErrKeyConflict,
				"key id %s or this key material is already registered for application %q", quoteKeyID(keyID), applicationID)
		}
		return wrapDB(err)
	}
	*out = Key{
		ApplicationID: applicationID, KeyID: keyID, PublicKey: append(ed25519.PublicKey(nil), pub...),
		Status: KeyActive, Purpose: purpose, Label: label, RegisteredAt: time.UnixMilli(now.UnixMilli()).UTC(),
	}
	return nil
}

// RotateKey registers newPub as newKeyID and retires oldKeyID in one
// transaction. The old key's earlier signatures stay valid until they expire,
// which is what lets installed packages keep verifying while publishers
// re-sign with the new key; a package signed by both keys verifies on either.
// The new key has the old key's purpose.
func (s *Store) RotateKey(ctx context.Context, applicationID, oldKeyID, newKeyID string, newPub ed25519.PublicKey, label string) (Key, error) {
	if err := checkApplicationID(applicationID); err != nil {
		return Key{}, err
	}
	if err := checkKeyID(oldKeyID); err != nil {
		return Key{}, err
	}
	if err := checkKeyID(newKeyID); err != nil {
		return Key{}, err
	}
	if err := checkPublicKey(newPub); err != nil {
		return Key{}, err
	}
	if !utf8.ValidString(label) || len(label) > 256 {
		return Key{}, fail(contracts.ErrValidationFailed, nil, "label must be valid UTF-8 of at most 256 bytes")
	}
	if !s.allowTestOnly && IsTestOnlyKey(newPub) {
		return Key{}, fail(contracts.ErrPermissionDenied, ErrTestOnlyKey,
			"key %s is a published TEST-ONLY key and cannot be trusted by this store", quoteKeyID(newKeyID))
	}
	var out Key
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		var purpose string
		err := tx.QueryRowContext(ctx,
			`SELECT purpose FROM trust_publisher_keys WHERE application_id = ? AND key_id = ?`, applicationID, oldKeyID).Scan(&purpose)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return wrapDB(err)
		}
		if err := s.retireTx(ctx, tx, applicationID, oldKeyID); err != nil {
			return err
		}
		return s.registerTx(ctx, tx, applicationID, newKeyID, newPub, label, KeyPurpose(purpose), &out)
	})
	if err != nil {
		return Key{}, err
	}
	return out, nil
}

// RetireKey marks an active key retired as of now. Retiring a retired key is a
// no-op; retiring a revoked key is a conflict.
func (s *Store) RetireKey(ctx context.Context, applicationID, keyID string) error {
	if err := checkApplicationID(applicationID); err != nil {
		return err
	}
	if err := checkKeyID(keyID); err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx *store.Tx) error { return s.retireTx(ctx, tx, applicationID, keyID) })
}

func (s *Store) retireTx(ctx context.Context, tx *store.Tx, applicationID, keyID string) error {
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM trust_publisher_keys WHERE application_id = ? AND key_id = ?`, applicationID, keyID).Scan(&status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fail(contracts.ErrValidationFailed, ErrUnknownKey, "key %s is not registered for application %q", quoteKeyID(keyID), applicationID)
	case err != nil:
		return wrapDB(err)
	}
	switch KeyStatus(status) {
	case KeyRetired:
		return nil
	case KeyRevoked:
		return fail(contracts.ErrConflict, ErrRevokedKey, "key %s is revoked and cannot be retired", quoteKeyID(keyID))
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE trust_publisher_keys SET status = 'retired', retired_at = ?
		  WHERE application_id = ? AND key_id = ? AND status = 'active'`,
		s.clk.Now().UTC().UnixMilli(), applicationID, keyID)
	return wrapDB(err)
}

// RevokeKey revokes a key permanently: from now on no signature by it
// verifies, whenever the signature claims to have been made. Revoking a
// revoked key keeps the first revocation. reason is recorded for the
// operator; it must be valid UTF-8 of at most 512 bytes.
func (s *Store) RevokeKey(ctx context.Context, applicationID, keyID, reason string) error {
	if err := checkApplicationID(applicationID); err != nil {
		return err
	}
	if err := checkKeyID(keyID); err != nil {
		return err
	}
	if !utf8.ValidString(reason) || len(reason) > 512 {
		return fail(contracts.ErrValidationFailed, nil, "reason must be valid UTF-8 of at most 512 bytes")
	}
	return s.db.Tx(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE trust_publisher_keys SET status = 'revoked', revoked_at = ?, revoked_reason = ?
			  WHERE application_id = ? AND key_id = ? AND status <> 'revoked'`,
			s.clk.Now().UTC().UnixMilli(), reason, applicationID, keyID)
		if err != nil {
			return wrapDB(err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var status string
		err = tx.QueryRowContext(ctx,
			`SELECT status FROM trust_publisher_keys WHERE application_id = ? AND key_id = ?`, applicationID, keyID).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fail(contracts.ErrValidationFailed, ErrUnknownKey, "key %s is not registered for application %q", quoteKeyID(keyID), applicationID)
		case err != nil:
			return wrapDB(err)
		}
		return nil // already revoked
	})
}

// SetKeyPurpose gives a registered platform key a purpose. It is how an
// operator classifies a key registered before purposes existed: a key with no
// purpose gets one, the same purpose again is a no-op, and a key that already
// has another purpose is a conflict (ErrKeyConflict): a purpose is set once.
// The key's status does not matter; a revoked key stays revoked.
func (s *Store) SetKeyPurpose(ctx context.Context, applicationID, keyID string, purpose KeyPurpose) error {
	if err := checkApplicationID(applicationID); err != nil {
		return err
	}
	if err := checkKeyID(keyID); err != nil {
		return err
	}
	if err := checkPurpose(applicationID, purpose); err != nil {
		return err
	}
	if purpose == PurposeNone {
		return fail(contracts.ErrValidationFailed, nil, "a purpose cannot be removed")
	}
	return s.db.Tx(ctx, func(tx *store.Tx) error {
		var cur string
		err := tx.QueryRowContext(ctx,
			`SELECT purpose FROM trust_publisher_keys WHERE application_id = ? AND key_id = ?`, applicationID, keyID).Scan(&cur)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fail(contracts.ErrValidationFailed, ErrUnknownKey, "key %s is not registered for application %q", quoteKeyID(keyID), applicationID)
		case err != nil:
			return wrapDB(err)
		case KeyPurpose(cur) == purpose:
			return nil
		case cur != "":
			return fail(contracts.ErrConflict, ErrKeyConflict, "key %s is registered for purpose %q; a purpose is never changed", quoteKeyID(keyID), cur)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE trust_publisher_keys SET purpose = ? WHERE application_id = ? AND key_id = ? AND purpose = ''`,
			string(purpose), applicationID, keyID)
		return wrapDB(err)
	})
}

const keyColumns = `application_id, key_id, public_key, status, label, registered_at, retired_at, revoked_at, revoked_reason, purpose`

// keyColumnsUnmigrated reads a table migration 0002 has not reached yet (a
// database an upgrade verifies its release against before it migrates it, the
// copy of a database being inspected): the purpose is derived the way the
// migration would set it, without writing anything.
const keyColumnsUnmigrated = `application_id, key_id, public_key, status, label, registered_at, retired_at, revoked_at, revoked_reason,
	CASE WHEN application_id = 'alixai.platform' AND label = 'audit checkpoint signing key' THEN 'audit-checkpoint' ELSE '' END`

// selectColumns is keyColumns, or keyColumnsUnmigrated when the table has no
// purpose column. It is asked on every read: the answer changes when the
// database is migrated while the store is open.
func (s *Store) selectColumns(ctx context.Context) (string, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('trust_publisher_keys') WHERE name = 'purpose'`).Scan(&n); err != nil {
		return "", wrapDB(err)
	}
	if n == 0 {
		return keyColumnsUnmigrated, nil
	}
	return keyColumns, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanKey(r rowScanner) (Key, error) {
	var (
		k                Key
		pubB64, status   string
		purpose          string
		registered       int64
		retired, revoked sql.NullInt64
	)
	if err := r.Scan(&k.ApplicationID, &k.KeyID, &pubB64, &status, &k.Label, &registered, &retired, &revoked, &k.RevokedReason, &purpose); err != nil {
		return Key{}, err
	}
	// A purpose this version does not know matches no verification (they
	// compare it exactly), so it is kept as it is rather than refused.
	k.Purpose = KeyPurpose(purpose)
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Key{}, fail(contracts.ErrInternal, nil, "stored key %s is corrupt", quoteKeyID(k.KeyID))
	}
	k.PublicKey = pub
	k.Status = KeyStatus(status)
	switch k.Status {
	case KeyActive, KeyRetired, KeyRevoked:
	default:
		return Key{}, fail(contracts.ErrInternal, nil, "stored key %s has unknown status %q", quoteKeyID(k.KeyID), clip(status))
	}
	k.RegisteredAt = time.UnixMilli(registered).UTC()
	if retired.Valid {
		k.RetiredAt = time.UnixMilli(retired.Int64).UTC()
	}
	if revoked.Valid {
		k.RevokedAt = time.UnixMilli(revoked.Int64).UTC()
	}
	return k, nil
}

// LookupKey returns the key registered for the application. A key that is not
// registered is an error wrapping ErrUnknownKey; a stored row that cannot be
// read back is contracts.ErrInternal, never a usable key. Key material that was
// revoked for any application is returned as revoked for every application it
// is registered for: a key revoked as compromised verifies nothing anywhere.
func (s *Store) LookupKey(ctx context.Context, applicationID, keyID string) (Key, error) {
	if err := checkApplicationID(applicationID); err != nil {
		return Key{}, fail(contracts.ErrUnauthenticated, ErrUnknownKey, "application id %q is not valid", clip(applicationID))
	}
	cols, err := s.selectColumns(ctx)
	if err != nil {
		return Key{}, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM trust_publisher_keys WHERE application_id = ? AND key_id = ?`, applicationID, keyID)
	k, err := scanKey(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Key{}, fail(contracts.ErrUnauthenticated, ErrUnknownKey, "key %s is not registered for application %q", quoteKeyID(keyID), applicationID)
	case err != nil:
		return Key{}, wrapDB(err)
	}
	if err := s.revokedElsewhere(ctx, &k); err != nil {
		return Key{}, err
	}
	return k, nil
}

// revokedElsewhere marks k revoked when its key material was revoked for any
// application.
func (s *Store) revokedElsewhere(ctx context.Context, k *Key) error {
	if k.Status == KeyRevoked {
		return nil
	}
	var revokedAt sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(revoked_at) FROM trust_publisher_keys WHERE fingerprint = ? AND status = 'revoked'`,
		Fingerprint(k.PublicKey)).Scan(&revokedAt); err != nil {
		return wrapDB(err)
	}
	if revokedAt.Valid {
		k.Status, k.RevokedAt = KeyRevoked, time.UnixMilli(revokedAt.Int64).UTC()
		k.RevokedReason = "this key material was revoked for another application"
	}
	return nil
}

// ApplicationsSharingKey lists, sorted, the other applications that registered
// the key material of the application's key: revoking it revokes it for them
// too (see LookupKey).
func (s *Store) ApplicationsSharingKey(ctx context.Context, applicationID, keyID string) ([]string, error) {
	if err := checkApplicationID(applicationID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT o.application_id FROM trust_publisher_keys o
		   JOIN trust_publisher_keys k ON o.fingerprint = k.fingerprint
		  WHERE k.application_id = ? AND k.key_id = ? AND o.application_id <> k.application_id
		  ORDER BY o.application_id`, applicationID, keyID)
	if err != nil {
		return nil, wrapDB(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var app string
		if err := rows.Scan(&app); err != nil {
			return nil, wrapDB(err)
		}
		out = append(out, app)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDB(err)
	}
	return out, nil
}

// ListKeys returns every key registered for the application, oldest first.
func (s *Store) ListKeys(ctx context.Context, applicationID string) ([]Key, error) {
	if err := checkApplicationID(applicationID); err != nil {
		return nil, err
	}
	cols, err := s.selectColumns(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cols+` FROM trust_publisher_keys WHERE application_id = ? ORDER BY registered_at, key_id`, applicationID)
	if err != nil {
		return nil, wrapDB(err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, wrapDB(err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDB(err)
	}
	// Listed as LookupKey answers: material revoked elsewhere is revoked here too.
	for i := range out {
		if err := s.revokedElsewhere(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// wrapDB classifies a database failure: a typed error passes through, anything
// else is contracts.ErrUnavailable (the store could not answer, so the caller
// fails closed).
func wrapDB(err error) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "trust: store failure")
}

// AllowsTestOnlyKeys reports whether the store was opened with
// AllowTestOnlyKeys. A verification that must never accept the published
// TEST-ONLY keys unless told to (release manifests) asks its key lookup this
// (TestOnlyPolicy), because a key registered in a development deployment stays
// in the store after it is opened normally again.
func (s *Store) AllowsTestOnlyKeys() bool { return s.allowTestOnly }

// MaterialRevoked reports whether the key material pub was revoked for any
// application of the store (RevocationLookup).
func (s *Store) MaterialRevoked(ctx context.Context, pub ed25519.PublicKey) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trust_publisher_keys WHERE fingerprint = ? AND status = 'revoked'`, Fingerprint(pub)).Scan(&n); err != nil {
		return false, wrapDB(err)
	}
	return n > 0, nil
}

// TestOnlyPolicy is implemented by a KeyLookup that says whether it was
// explicitly allowed to serve the published TEST-ONLY keys. A lookup that does
// not implement it is taken as not allowed.
type TestOnlyPolicy interface {
	AllowsTestOnlyKeys() bool
}

// RevocationLookup is implemented by a KeyLookup that can say whether key
// material was revoked, whatever key id it was registered under: how a key
// built into the binary (a pinned release key) stays revocable.
type RevocationLookup interface {
	MaterialRevoked(ctx context.Context, pub ed25519.PublicKey) (bool, error)
}

var (
	_ TestOnlyPolicy   = (*Store)(nil)
	_ RevocationLookup = (*Store)(nil)
)
