package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// IssuerKeyStatus is the lifecycle state of a stored issuer key.
type IssuerKeyStatus string

const (
	// IssuerKeyActive keys verify tokens.
	IssuerKeyActive IssuerKeyStatus = "active"
	// IssuerKeyRevoked keys verify nothing, permanently.
	IssuerKeyRevoked IssuerKeyStatus = "revoked"
)

// StoredIssuerKey is an issuer key as the store holds it. It is public key
// material and bookkeeping only: nothing here, and nothing in the store, can
// sign a token.
type StoredIssuerKey struct {
	IssuerKey
	Status IssuerKeyStatus
	Label  string
	// Fingerprint is the lower-case hex SHA-256 of the raw public key.
	Fingerprint string
	// DevKey is true when the operator added the key with the explicit
	// developer flag, the only way a published TEST-ONLY key is held.
	DevKey        bool
	RegisteredAt  time.Time
	RevokedAt     time.Time
	RevokedReason string
}

// ContextExchange is what an application package's manifest states in its
// context_exchange block about the tokens the host will issue. It is plain
// strings: the composition root (internal/cli) reads the manifest, and this
// package, which does not import the package loader, only compares.
type ContextExchange struct {
	// Issuer is the "iss" value the host puts in its tokens.
	Issuer string
	// Audience is the "aud" value the host puts in its tokens.
	Audience string
	// KeyIDs are the key ids that may sign the host's tokens.
	KeyIDs []string
}

// CheckKey reports whether a key with this id and issuer is one the manifest
// declares: the id must be listed in key_ids and the issuer must be exactly
// the manifest's. A manifest that declares no key ids declares none.
func (c ContextExchange) CheckKey(keyID, issuer string) error {
	listed := false
	for _, id := range c.KeyIDs {
		if id == keyID {
			listed = true
			break
		}
	}
	if !listed {
		return contracts.Newf(contracts.ErrValidationFailed,
			"identity: key id %q is not among the key ids the package declares in context_exchange.key_ids (%s)", clipText(keyID), listIDs(c.KeyIDs))
	}
	if c.Issuer == "" || issuer != c.Issuer {
		return contracts.Newf(contracts.ErrValidationFailed,
			"identity: issuer %q is not the issuer the package declares in context_exchange.issuer (%q)", clipText(issuer), clipText(c.Issuer))
	}
	return nil
}

// AudienceMismatch returns a warning when the audience the manifest says the
// host puts in its tokens is not the audience this deployment requires
// (configured, from identity.audience or identity.deployment_id): every such
// token would be refused with wrong_audience. It is empty when they agree.
func (c ContextExchange) AudienceMismatch(configured string) string {
	if configured == "" || c.Audience == configured {
		return ""
	}
	return fmt.Sprintf("the package declares audience %q but this deployment requires %q: tokens that carry the declared audience are refused (set identity.audience or fix the package)",
		clipText(c.Audience), clipText(configured))
}

func listIDs(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = clipText(id)
	}
	return strings.Join(out, ", ")
}

func clipText(s string) string {
	if utf8.RuneCountInString(s) > 80 {
		r := []rune(s)
		return string(r[:80]) + "..."
	}
	return s
}

// IssuerKeyAction names what an IssuerKeyEvent records.
type IssuerKeyAction string

const (
	IssuerKeyActionAdded   IssuerKeyAction = "added"
	IssuerKeyActionRevoked IssuerKeyAction = "revoked"
)

// issuerKeyMaxNote bounds a label and a revocation reason.
const issuerKeyMaxNote = 256

// IssuerKeyEvent is handed to IssuerKeyStoreOptions.Record inside the
// transaction that adds or revokes a key. It carries the key's identity and
// fingerprint, never key bytes.
type IssuerKeyEvent struct {
	Action IssuerKeyAction
	Key    StoredIssuerKey
	// Reason is the operator's reason for a revocation.
	Reason string
}

// IssuerKeyStoreOptions configures an IssuerKeyStore.
type IssuerKeyStoreOptions struct {
	// Logger receives key lifecycle events (never key bytes). Nil discards.
	Logger *slog.Logger
	// Record, when set, is called inside the transaction of every add and
	// revoke. An error rolls the change back: a key is neither added nor
	// revoked unless its record was written. This is where the composition
	// root appends the audit event (SEC-037); this package does not import the
	// audit log.
	Record func(ctx context.Context, tx *store.Tx, ev IssuerKeyEvent) error
}

// IssuerKeyStore is the SQLite KeyResolver of host-application issuer keys
// (module "identity", table identity_issuer_keys). It holds public keys only,
// per application, and starts empty: until an operator adds a key
// (`alixai identity add-issuer-key`) no token is accepted.
//
// Every Lookup reads the table, with no cache, so a key revoked by the CLI is
// refused by a running service on its next token.
type IssuerKeyStore struct {
	db     *store.DB
	clk    clock.Clock
	log    *slog.Logger
	record func(ctx context.Context, tx *store.Tx, ev IssuerKeyEvent) error
}

var _ KeyResolver = (*IssuerKeyStore)(nil)

// NewIssuerKeyStore returns the issuer key store on db. The "identity"
// migrations must have been applied (they register themselves with
// store.Default when this package is imported; store.Open applies them). A nil
// clock means the wall clock.
func NewIssuerKeyStore(db *store.DB, clk clock.Clock, opts IssuerKeyStoreOptions) *IssuerKeyStore {
	if clk == nil {
		clk = clock.Real{}
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &IssuerKeyStore{db: db, clk: clk, log: log, record: opts.Record}
}

// AddIssuerKey is the input of IssuerKeyStore.Add.
type AddIssuerKey struct {
	// Key is the key to register: its id, the application it speaks for, the
	// issuer its tokens carry, the 32-byte public key and an optional NotAfter.
	Key IssuerKey
	// Label is free text for the operator (at most 256 bytes).
	Label string
	// Declared is what the application package's manifest declares about its
	// tokens. It is required: a key is registered only for an id and issuer the
	// package names, so what the operator provisions and what the package says
	// cannot drift apart unnoticed.
	Declared ContextExchange
	// AllowTestOnly is the explicit developer flag. Without it a published
	// TEST-ONLY key (IsPublishedTestOnlyKey) is refused: anyone can sign with
	// it, so it must never become a trusted issuer by accident.
	AllowTestOnly bool
}

// Add registers an issuer key for an application. It fails with
//
//   - validation_failed for a malformed key, a key id or issuer the manifest
//     does not declare, a missing declaration or a NotAfter that is not in the
//     future;
//   - permission_denied for a published TEST-ONLY key without AllowTestOnly;
//   - conflict when the key id is already used (by any application, whatever
//     the key's status: a key id is never reused, because a lookup knows only
//     the id), when the same key material is already registered for the
//     application under another id, or when the material was revoked: a key
//     revoked as compromised must not return under a new name.
func (s *IssuerKeyStore) Add(ctx context.Context, in AddIssuerKey) (StoredIssuerKey, error) {
	k := in.Key
	if err := validateIssuerKey(k); err != nil {
		return StoredIssuerKey{}, err
	}
	if len(in.Declared.KeyIDs) == 0 || in.Declared.Issuer == "" {
		return StoredIssuerKey{}, contracts.New(contracts.ErrValidationFailed,
			"identity: an issuer key is registered against the context_exchange block of the application's package; none was given")
	}
	if err := in.Declared.CheckKey(k.KeyID, k.Issuer); err != nil {
		return StoredIssuerKey{}, err
	}
	if !utf8.ValidString(in.Label) || len(in.Label) > issuerKeyMaxNote {
		return StoredIssuerKey{}, contracts.New(contracts.ErrValidationFailed, "identity: label must be valid UTF-8 of at most 256 bytes")
	}
	now := s.clk.Now().UTC()
	if !k.NotAfter.IsZero() && !k.NotAfter.After(now) {
		return StoredIssuerKey{}, contracts.New(contracts.ErrValidationFailed, "identity: not_after must be in the future")
	}
	testOnly := IsPublishedTestOnlyKey(k.Public)
	if testOnly && !in.AllowTestOnly {
		return StoredIssuerKey{}, contracts.New(contracts.ErrPermissionDenied,
			"identity: this is a published TEST-ONLY key: anyone can sign tokens with it, so it cannot be trusted as an issuer key without the explicit developer flag")
	}

	pub := append(ed25519.PublicKey(nil), k.Public...)
	fp := fingerprintOf(pub)
	var notAfter any
	if !k.NotAfter.IsZero() {
		notAfter = k.NotAfter.UTC().UnixMilli()
	}
	var out StoredIssuerKey
	err := s.db.Tx(ctx, func(tx *store.Tx) error {
		var revoked int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM identity_issuer_keys WHERE fingerprint = ? AND status = 'revoked'`, fp).Scan(&revoked); err != nil {
			return err
		}
		if revoked > 0 {
			return contracts.New(contracts.ErrConflict, "identity: this key material was revoked; a revoked key cannot be registered again")
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO identity_issuer_keys
			   (application_id, tenant_id, workspace_id, key_id, issuer, public_key, fingerprint, status, dev_key, label,
			    registered_at, not_after, revoked_reason)
			 VALUES (?, '', '', ?, ?, ?, ?, 'active', ?, ?, ?, ?, '')`,
			k.ApplicationID, k.KeyID, k.Issuer, base64.StdEncoding.EncodeToString(pub), fp, boolInt(testOnly), in.Label,
			now.UnixMilli(), notAfter)
		if err != nil {
			if store.IsUniqueViolation(err) {
				return contracts.New(contracts.ErrConflict,
					"identity: the key id is already registered (key ids are unique across applications and never reused) or this key material is already registered for the application")
			}
			return err
		}
		out = StoredIssuerKey{
			IssuerKey: IssuerKey{KeyID: k.KeyID, ApplicationID: k.ApplicationID, Issuer: k.Issuer, Public: pub, NotAfter: k.NotAfter.UTC()},
			Status:    IssuerKeyActive, Label: in.Label, Fingerprint: fp, DevKey: testOnly,
			RegisteredAt: time.UnixMilli(now.UnixMilli()).UTC(),
		}
		if !k.NotAfter.IsZero() {
			out.NotAfter = time.UnixMilli(k.NotAfter.UTC().UnixMilli()).UTC()
		}
		if s.record != nil {
			return s.record(ctx, tx, IssuerKeyEvent{Action: IssuerKeyActionAdded, Key: out})
		}
		return nil
	})
	if err != nil {
		return StoredIssuerKey{}, wrapStoreErr(err)
	}
	s.log.Info("issuer key added", "key_id", out.KeyID, "application_id", out.ApplicationID, "issuer", out.Issuer,
		"fingerprint", out.Fingerprint, "dev_key", out.DevKey)
	return out, nil
}

// Revoke revokes an application's key: from the next verification on, tokens it
// signed are refused however long they have left to live. The row stays, as the
// record that the key was revoked, and the key id and material can never be
// registered again. The key must belong to applicationID: an operator cannot
// revoke (or probe for) another application's key by id. It reports whether the
// key was active; revoking a key that is already revoked changes nothing and
// records nothing.
func (s *IssuerKeyStore) Revoke(ctx context.Context, applicationID, keyID, reason string) (key StoredIssuerKey, changed bool, err error) {
	if !validKeyID(keyID) || !validIdentifier(applicationID) {
		return StoredIssuerKey{}, false, contracts.New(contracts.ErrValidationFailed, "identity: key id or application id is malformed")
	}
	if !utf8.ValidString(reason) || len(reason) > issuerKeyMaxNote {
		return StoredIssuerKey{}, false, contracts.New(contracts.ErrValidationFailed, "identity: reason must be valid UTF-8 of at most 256 bytes")
	}
	now := s.clk.Now().UTC()
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		cur, err := scanStored(tx.QueryRowContext(ctx, selectStored+` WHERE key_id = ? AND application_id = ?`, keyID, applicationID))
		if errors.Is(err, sql.ErrNoRows) {
			return contracts.Wrap(contracts.ErrValidationFailed, ErrKeyNotFound, "identity: no such issuer key for this application")
		}
		if err != nil {
			return err
		}
		if cur.Status == IssuerKeyRevoked {
			key = cur
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE identity_issuer_keys SET status = 'revoked', revoked_at = ?, revoked_reason = ? WHERE key_id = ? AND application_id = ?`,
			now.UnixMilli(), reason, keyID, applicationID); err != nil {
			return err
		}
		cur.Status, cur.RevokedAt, cur.RevokedReason = IssuerKeyRevoked, time.UnixMilli(now.UnixMilli()).UTC(), reason
		key, changed = cur, true
		if s.record != nil {
			return s.record(ctx, tx, IssuerKeyEvent{Action: IssuerKeyActionRevoked, Key: cur, Reason: reason})
		}
		return nil
	})
	if err != nil {
		return StoredIssuerKey{}, false, wrapStoreErr(err)
	}
	if changed {
		s.log.Warn("issuer key revoked", "key_id", key.KeyID, "application_id", key.ApplicationID, "fingerprint", key.Fingerprint)
	}
	return key, changed, nil
}

// List returns the keys of an application, oldest first; an empty
// applicationID lists every application's keys. Revoked keys are included.
func (s *IssuerKeyStore) List(ctx context.Context, applicationID string) ([]StoredIssuerKey, error) {
	query, args := selectStored+` ORDER BY registered_at, key_id`, []any(nil)
	if applicationID != "" {
		if !validIdentifier(applicationID) {
			return nil, contracts.New(contracts.ErrValidationFailed, "identity: application id is malformed")
		}
		query, args = selectStored+` WHERE application_id = ? ORDER BY registered_at, key_id`, []any{applicationID}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	defer rows.Close()
	var out []StoredIssuerKey
	for rows.Next() {
		k, err := scanStored(rows)
		if err != nil {
			return nil, wrapStoreErr(err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapStoreErr(err)
	}
	return out, nil
}

// Lookup implements KeyResolver. It answers ErrKeyNotFound for an unknown key,
// a revoked key and a published TEST-ONLY key that was not added with the
// developer flag (a row put there by other means), and any other error when the
// store cannot answer, which the verifier turns into a refusal.
func (s *IssuerKeyStore) Lookup(ctx context.Context, keyID string) (IssuerKey, error) {
	if !validKeyID(keyID) {
		return IssuerKey{}, ErrKeyNotFound
	}
	k, err := scanStored(s.db.QueryRowContext(ctx, selectStored+` WHERE key_id = ?`, keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return IssuerKey{}, ErrKeyNotFound
	}
	if err != nil {
		return IssuerKey{}, fmt.Errorf("identity: reading issuer key: %w", err)
	}
	if k.Status != IssuerKeyActive {
		return IssuerKey{}, ErrKeyNotFound
	}
	if IsPublishedTestOnlyKey(k.Public) && !k.DevKey {
		s.log.Error("a published TEST-ONLY key is stored without the developer flag; refusing it", "key_id", k.KeyID, "application_id", k.ApplicationID)
		return IssuerKey{}, ErrKeyNotFound
	}
	return k.IssuerKey, nil
}

const selectStored = `SELECT application_id, key_id, issuer, public_key, fingerprint, status, dev_key, label, registered_at, not_after, revoked_at, revoked_reason
  FROM identity_issuer_keys`

type rowScanner interface{ Scan(dest ...any) error }

func scanStored(r rowScanner) (StoredIssuerKey, error) {
	var (
		k                     StoredIssuerKey
		pubB64, status        string
		dev                   int
		registered            int64
		notAfter, revokedAtMs sql.NullInt64
	)
	if err := r.Scan(&k.ApplicationID, &k.KeyID, &k.Issuer, &pubB64, &k.Fingerprint, &status, &dev, &k.Label,
		&registered, &notAfter, &revokedAtMs, &k.RevokedReason); err != nil {
		return StoredIssuerKey{}, err
	}
	pub, err := base64.StdEncoding.Strict().DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize || fingerprintOf(pub) != k.Fingerprint {
		// A row that does not hold the key it claims is corruption, never a key.
		return StoredIssuerKey{}, errors.New("identity: a stored issuer key is damaged")
	}
	k.Public = ed25519.PublicKey(pub)
	k.Status = IssuerKeyStatus(status)
	k.DevKey = dev == 1
	k.RegisteredAt = time.UnixMilli(registered).UTC()
	if notAfter.Valid {
		k.NotAfter = time.UnixMilli(notAfter.Int64).UTC()
	}
	if revokedAtMs.Valid {
		k.RevokedAt = time.UnixMilli(revokedAtMs.Int64).UTC()
	}
	return k, nil
}

func fingerprintOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// wrapStoreErr leaves typed errors alone and makes a raw database failure an
// unavailable error that does not echo SQL.
func wrapStoreErr(err error) error {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "identity: the issuer key store is not available")
}
