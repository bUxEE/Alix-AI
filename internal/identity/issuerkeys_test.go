package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// keyEnv is an issuer key store over a file database that tests can close and
// reopen, with the replay cache and verifier a service would build around it.
type keyEnv struct {
	t      *testing.T
	path   string
	clk    *clock.Fake
	db     *store.DB
	keys   *identity.IssuerKeyStore
	events []identity.IssuerKeyEvent
}

func newKeyEnv(t *testing.T) *keyEnv {
	t.Helper()
	e := &keyEnv{t: t, path: filepath.Join(t.TempDir(), "keys.db"), clk: clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))}
	e.open()
	return e
}

func (e *keyEnv) open() {
	e.t.Helper()
	db, err := store.Open(bg, store.Options{Path: e.path})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = db.Close() })
	e.db = db
	e.keys = e.store()
}

// store returns another store over the same database: what a second process
// (the CLI beside a running service) holds.
func (e *keyEnv) store() *identity.IssuerKeyStore {
	return identity.NewIssuerKeyStore(e.db, e.clk, identity.IssuerKeyStoreOptions{
		Record: func(_ context.Context, _ *store.Tx, ev identity.IssuerKeyEvent) error {
			e.events = append(e.events, ev)
			return nil
		},
	})
}

func (e *keyEnv) reopen() {
	e.t.Helper()
	if err := e.db.Close(); err != nil {
		e.t.Fatal(err)
	}
	e.open()
}

func (e *keyEnv) issuer(app, kid string) *identitytest.Issuer {
	return identitytest.NewIssuer(e.t, identitytest.IssuerConfig{ApplicationID: app, KeyID: kid, Clock: e.clk})
}

// verifier builds the verifier of a deployment whose issuer keys are ks.
func (e *keyEnv) verifier(ks identity.KeyResolver) *identity.Verifier {
	e.t.Helper()
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment, Keys: ks,
		Replay: identity.NewSQLiteReplayCache(e.db, e.clk, nil), Clock: e.clk,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// declared is the context_exchange block of a package whose host is iss.
func declared(iss *identitytest.Issuer) identity.ContextExchange {
	return identity.ContextExchange{Issuer: iss.Issuer, Audience: iss.Audience, KeyIDs: []string{iss.KeyID}}
}

func (e *keyEnv) add(iss *identitytest.Issuer) identity.StoredIssuerKey {
	e.t.Helper()
	k, err := e.keys.Add(bg, identity.AddIssuerKey{Key: iss.Key(), Label: "test " + iss.KeyID, Declared: declared(iss)})
	if err != nil {
		e.t.Fatalf("Add(%s): %v", iss.KeyID, err)
	}
	return k
}

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil || contracts.CodeOf(err) != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestIssuerKeyAddListRevokePersists(t *testing.T) {
	e := newKeyEnv(t)
	a := e.issuer("app_a", "app-a-key-1")
	b := e.issuer("app_b", "app-b-key-1")

	// Nothing is trusted until a key is added.
	if got, err := e.keys.List(bg, ""); err != nil || len(got) != 0 {
		t.Fatalf("List on a new store = %v, %v", got, err)
	}
	if _, err := e.keys.Lookup(bg, a.KeyID); !errors.Is(err, identity.ErrKeyNotFound) {
		t.Fatalf("Lookup on a new store = %v, want ErrKeyNotFound", err)
	}

	added := e.add(a)
	e.clk.Advance(time.Minute)
	e.add(b)
	if added.Status != identity.IssuerKeyActive || added.DevKey || added.Fingerprint == "" || added.Label != "test app-a-key-1" || !added.Public.Equal(a.Public) {
		t.Fatalf("Add returned %+v", added)
	}

	check := func(stage string, wantA identity.IssuerKeyStatus) {
		t.Helper()
		all, err := e.keys.List(bg, "")
		if err != nil || len(all) != 2 || all[0].KeyID != a.KeyID || all[1].KeyID != b.KeyID {
			t.Fatalf("%s: List(all) = %+v, %v", stage, all, err)
		}
		onlyA, err := e.keys.List(bg, "app_a")
		if err != nil || len(onlyA) != 1 || onlyA[0].ApplicationID != "app_a" || onlyA[0].Status != wantA {
			t.Fatalf("%s: List(app_a) = %+v, %v", stage, onlyA, err)
		}
		got, lerr := e.keys.Lookup(bg, a.KeyID)
		if wantA == identity.IssuerKeyActive {
			if lerr != nil || got.ApplicationID != "app_a" || got.Issuer != a.Issuer || !got.Public.Equal(a.Public) {
				t.Fatalf("%s: Lookup = %+v, %v", stage, got, lerr)
			}
		} else if !errors.Is(lerr, identity.ErrKeyNotFound) {
			t.Fatalf("%s: Lookup of a revoked key = %v, want ErrKeyNotFound", stage, lerr)
		}
		if got, err := e.keys.Lookup(bg, b.KeyID); err != nil || !got.Public.Equal(b.Public) {
			t.Fatalf("%s: the other application's key = %+v, %v", stage, got, err)
		}
	}
	check("after add", identity.IssuerKeyActive)

	// A restart changes nothing: the keys are in the database.
	e.reopen()
	check("after reopen", identity.IssuerKeyActive)

	e.clk.Advance(time.Hour)
	rev, changed, err := e.keys.Revoke(bg, "app_a", a.KeyID, "rotated out")
	if err != nil || !changed || rev.Status != identity.IssuerKeyRevoked || rev.RevokedReason != "rotated out" || !rev.RevokedAt.Equal(e.clk.Now()) {
		t.Fatalf("Revoke = %+v, %v, %v", rev, changed, err)
	}
	check("after revoke", identity.IssuerKeyRevoked)
	e.reopen()
	check("revoked, after reopen", identity.IssuerKeyRevoked)

	// Revoking again is a no-op that records nothing.
	again, changed, err := e.keys.Revoke(bg, "app_a", a.KeyID, "twice")
	if err != nil || changed || again.RevokedReason != "rotated out" {
		t.Fatalf("second Revoke = %+v, %v, %v", again, changed, err)
	}

	// The record of the revocation stays, and the id and material never return.
	if _, err := e.db.ExecContext(bg, `DELETE FROM identity_issuer_keys`); err == nil {
		t.Error("a row of identity_issuer_keys was deleted")
	}
	if _, err := e.db.ExecContext(bg, `UPDATE identity_issuer_keys SET status = 'active', revoked_at = NULL WHERE key_id = ?`, a.KeyID); err == nil {
		t.Error("a revoked key was reactivated in the database")
	}
	_, err = e.keys.Add(bg, identity.AddIssuerKey{Key: a.Key(), Declared: declared(a)})
	wantCode(t, err, contracts.ErrConflict)
	renamed := e.issuer("app_a", "app-a-key-2")
	renamed2 := identity.IssuerKey{KeyID: renamed.KeyID, ApplicationID: "app_a", Issuer: renamed.Issuer, Public: a.Public}
	_, err = e.keys.Add(bg, identity.AddIssuerKey{Key: renamed2, Declared: declared(renamed)})
	wantCode(t, err, contracts.ErrConflict)
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("re-adding revoked material = %v, want it to say the material was revoked", err)
	}

	// Revoking an unknown key is an error, not a silent success.
	if _, _, err := e.keys.Revoke(bg, "app_a", "no-such-key", ""); !errors.Is(err, identity.ErrKeyNotFound) {
		t.Errorf("Revoke of an unknown key = %v, want ErrKeyNotFound", err)
	}

	// Every change was recorded in the same transaction: two adds and one
	// revoke, and nothing for the repeat.
	var got []string
	for _, ev := range e.events {
		got = append(got, string(ev.Action)+":"+ev.Key.KeyID)
	}
	if strings.Join(got, ",") != "added:app-a-key-1,added:app-b-key-1,revoked:app-a-key-1" {
		t.Errorf("recorded events = %v", got)
	}
	for _, ev := range e.events {
		if len(ev.Key.Public) == 0 || ev.Key.Fingerprint == "" {
			t.Errorf("event %+v lacks the key identity", ev)
		}
	}
}

// A record that cannot be written aborts the change: a key is neither added nor
// revoked without its audit record (SEC-037).
func TestIssuerKeyChangeNeedsItsRecord(t *testing.T) {
	e := newKeyEnv(t)
	fail := errors.New("the audit log is down")
	failing := identity.NewIssuerKeyStore(e.db, e.clk, identity.IssuerKeyStoreOptions{
		Record: func(context.Context, *store.Tx, identity.IssuerKeyEvent) error { return fail },
	})
	iss := e.issuer("app_a", "app-a-key-1")
	if _, err := failing.Add(bg, identity.AddIssuerKey{Key: iss.Key(), Declared: declared(iss)}); !errors.Is(err, fail) {
		t.Fatalf("Add with a failing record = %v", err)
	}
	if got, _ := e.keys.List(bg, ""); len(got) != 0 {
		t.Fatalf("a key was added although its record failed: %+v", got)
	}

	e.add(iss)
	if _, _, err := failing.Revoke(bg, "app_a", iss.KeyID, "x"); !errors.Is(err, fail) {
		t.Fatalf("Revoke with a failing record = %v", err)
	}
	if _, err := e.keys.Lookup(bg, iss.KeyID); err != nil {
		t.Fatalf("a key was revoked although its record failed: %v", err)
	}
}

// SEC-037: after a key is revoked, tokens it signed are refused immediately, a
// token minted before the revocation included, and a service that holds its own
// store over the database sees it without a restart (the CLI revokes from
// another process).
func TestRevokedIssuerKeyRejectsToken(t *testing.T) {
	e := newKeyEnv(t)
	iss := e.issuer("app_a", "app-a-key-1")
	e.add(iss)

	service := e.verifier(e.store()) // the running service's own store, never told of the revocation
	if v, err := service.Verify(bg, iss.Token()); err != nil || v.Scope.ApplicationID != "app_a" {
		t.Fatalf("a token of a provisioned key = %+v, %v", v, err)
	}
	early := iss.Token() // minted while the key was valid, not yet presented

	if _, changed, err := e.keys.Revoke(bg, "app_a", iss.KeyID, "key compromised"); err != nil || !changed {
		t.Fatalf("Revoke = %v, %v", changed, err)
	}
	_, err := service.Verify(bg, iss.Token())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
	_, err = service.Verify(bg, early)
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)

	// A key that was never registered is refused the same way.
	stranger := e.issuer("app_a", "stranger-key")
	_, err = service.Verify(bg, stranger.Token())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
}

// A key speaks for one application: its tokens cannot name another, a key id is
// unique across applications (the verifier looks it up before it knows the
// application), and an operator of one application cannot revoke another's key.
func TestIssuerKeyScopedToApplication(t *testing.T) {
	e := newKeyEnv(t)
	a := e.issuer("app_a", "app-a-key-1")
	b := e.issuer("app_b", "app-b-key-1")
	e.add(a)
	e.add(b)
	v := e.verifier(e.keys)

	for name, iss := range map[string]*identitytest.Issuer{"a": a, "b": b} {
		got, err := v.Verify(bg, iss.Token())
		if err != nil || got.Scope.ApplicationID != iss.ApplicationID {
			t.Fatalf("a token of %s's own key = %+v, %v", name, got, err)
		}
	}
	// A's key signs a token that claims B's application: refused, not scoped to B.
	_, err := v.Verify(bg, a.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimApplication, "app_b") }))
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonWrongApplication)

	// B cannot claim A's key id, whatever it registers.
	clash := e.issuer("app_b", a.KeyID)
	_, err = e.keys.Add(bg, identity.AddIssuerKey{Key: clash.Key(), Declared: declared(clash)})
	wantCode(t, err, contracts.ErrConflict)

	// Revoking by the wrong application finds nothing and changes nothing.
	if _, _, err := e.keys.Revoke(bg, "app_b", a.KeyID, "not mine"); !errors.Is(err, identity.ErrKeyNotFound) {
		t.Fatalf("Revoke of another application's key = %v, want ErrKeyNotFound", err)
	}
	if _, err := v.Verify(bg, a.Token()); err != nil {
		t.Fatalf("A's key stopped working after B's operator tried to revoke it: %v", err)
	}
	if onlyB, _ := e.keys.List(bg, "app_b"); len(onlyB) != 1 || onlyB[0].KeyID != b.KeyID {
		t.Errorf("List(app_b) = %+v", onlyB)
	}
	// The scope-column rule of the store: the table has the three columns, and
	// an application-level key holds no tenant or workspace.
	storetest.AssertScopedTables(t, e.db)
	if _, err := e.db.ExecContext(bg, `UPDATE identity_issuer_keys SET tenant_id = 'ten_x' WHERE key_id = ?`, a.KeyID); err == nil {
		t.Error("an issuer key was given a tenant")
	}
}

// The published TEST-ONLY keys are not trusted as issuers unless the operator
// says so with the developer flag, and a service refuses one that is in the
// database without it.
func TestTestOnlyIssuerKeyRefusedWithoutDeveloperFlag(t *testing.T) {
	e := newKeyEnv(t)
	for _, b64 := range identity.PublishedTestOnlyKeys() {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			t.Fatalf("published key %q is malformed", b64)
		}
		if !identity.IsPublishedTestOnlyKey(raw) {
			t.Fatalf("%s is not recognised as a published TEST-ONLY key", b64)
		}
		published := identity.IssuerKey{KeyID: "dev-key", ApplicationID: "app_a", Issuer: "https://dev.example.test", Public: raw}
		decl := identity.ContextExchange{Issuer: published.Issuer, KeyIDs: []string{published.KeyID}}

		_, err = e.keys.Add(bg, identity.AddIssuerKey{Key: published, Declared: decl})
		wantCode(t, err, contracts.ErrPermissionDenied)
		if !strings.Contains(err.Error(), "TEST-ONLY") {
			t.Errorf("the refusal does not say why: %v", err)
		}
		if got, _ := e.keys.List(bg, ""); len(got) != 0 {
			t.Fatalf("a refused TEST-ONLY key was stored: %+v", got)
		}
	}

	// With the flag it is stored, marked, and serves tokens.
	raw, _ := base64.StdEncoding.DecodeString(identity.PublishedTestOnlyKeys()[0])
	published := identity.IssuerKey{KeyID: "dev-key", ApplicationID: "app_a", Issuer: "https://dev.example.test", Public: raw}
	decl := identity.ContextExchange{Issuer: published.Issuer, KeyIDs: []string{published.KeyID}}
	stored, err := e.keys.Add(bg, identity.AddIssuerKey{Key: published, Declared: decl, AllowTestOnly: true})
	if err != nil || !stored.DevKey {
		t.Fatalf("Add with the developer flag = %+v, %v", stored, err)
	}
	if _, err := e.keys.Lookup(bg, "dev-key"); err != nil {
		t.Fatalf("a TEST-ONLY key added with the flag is not served: %v", err)
	}
}

// The unmarked row is produced by a direct INSERT, which is what a database
// restored from elsewhere or edited by hand would hold.
func TestUnmarkedTestOnlyRowIsNeverServed(t *testing.T) {
	e := newKeyEnv(t)
	raw, _ := base64.StdEncoding.DecodeString(identity.PublishedTestOnlyKeys()[0])
	sum := sha256.Sum256(raw)
	fp := hex.EncodeToString(sum[:])
	if _, err := e.db.ExecContext(bg,
		`INSERT INTO identity_issuer_keys (application_id, key_id, issuer, public_key, fingerprint, status, dev_key, label, registered_at, revoked_reason)
		 VALUES ('app_a', 'dev-key', 'https://dev.example.test', ?, ?, 'active', 0, '', 0, '')`,
		base64.StdEncoding.EncodeToString(raw), fp); err != nil {
		t.Fatal(err)
	}
	if _, err := e.keys.Lookup(bg, "dev-key"); !errors.Is(err, identity.ErrKeyNotFound) {
		t.Fatalf("Lookup of an unmarked TEST-ONLY row = %v, want ErrKeyNotFound", err)
	}
}

// The key id and the issuer must be the ones the package's context_exchange
// block declares; a manifest that declares none registers none.
func TestIssuerKeyMustMatchTheDeclaredContextExchange(t *testing.T) {
	e := newKeyEnv(t)
	iss := e.issuer("app_a", "app-a-key-1")
	decl := declared(iss)

	cases := map[string]struct {
		mod  func(*identity.AddIssuerKey)
		want contracts.ErrCode
	}{
		"key id not declared":  {func(in *identity.AddIssuerKey) { in.Declared.KeyIDs = []string{"another-key"} }, contracts.ErrValidationFailed},
		"no key ids declared":  {func(in *identity.AddIssuerKey) { in.Declared.KeyIDs = nil }, contracts.ErrValidationFailed},
		"issuer differs":       {func(in *identity.AddIssuerKey) { in.Key.Issuer = "https://evil.example.test" }, contracts.ErrValidationFailed},
		"no issuer declared":   {func(in *identity.AddIssuerKey) { in.Declared.Issuer = "" }, contracts.ErrValidationFailed},
		"case differs":         {func(in *identity.AddIssuerKey) { in.Declared.KeyIDs = []string{strings.ToUpper(iss.KeyID)} }, contracts.ErrValidationFailed},
		"not_after in past":    {func(in *identity.AddIssuerKey) { in.Key.NotAfter = e.clk.Now().Add(-time.Second) }, contracts.ErrValidationFailed},
		"label not UTF-8":      {func(in *identity.AddIssuerKey) { in.Label = "bad\xff" }, contracts.ErrValidationFailed},
		"public key too short": {func(in *identity.AddIssuerKey) { in.Key.Public = in.Key.Public[:31] }, contracts.ErrValidationFailed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := identity.AddIssuerKey{Key: iss.Key(), Declared: decl}
			c.mod(&in)
			_, err := e.keys.Add(bg, in)
			wantCode(t, err, c.want)
			if got, _ := e.keys.List(bg, ""); len(got) != 0 {
				t.Fatalf("a refused key was stored: %+v", got)
			}
		})
	}

	// The audience note: tokens carrying the manifest's audience are refused by a
	// deployment that requires another.
	if note := decl.AudienceMismatch(iss.Audience); note != "" {
		t.Errorf("matching audiences warned: %s", note)
	}
	if note := decl.AudienceMismatch("another-deployment"); !strings.Contains(note, "another-deployment") {
		t.Errorf("a differing audience was not reported: %q", note)
	}
	if note := decl.AudienceMismatch(""); note != "" {
		t.Errorf("an unconfigured audience warned: %q", note)
	}
}

// NotAfter bounds a key made for a rotation overlap: the verifier refuses it
// from that instant, with no further action.
func TestIssuerKeyNotAfterRetiresTheKey(t *testing.T) {
	e := newKeyEnv(t)
	iss := e.issuer("app_a", "app-a-key-1")
	key := iss.Key()
	key.NotAfter = e.clk.Now().Add(10 * time.Minute)
	if _, err := e.keys.Add(bg, identity.AddIssuerKey{Key: key, Declared: declared(iss)}); err != nil {
		t.Fatal(err)
	}
	v := e.verifier(e.keys)
	if _, err := v.Verify(bg, iss.Token()); err != nil {
		t.Fatalf("before not_after: %v", err)
	}
	tok := iss.Token()
	e.clk.Advance(11 * time.Minute)
	_, err := v.Verify(bg, tok)
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonKeyRetired)
	listed, _ := e.keys.List(bg, "app_a")
	if len(listed) != 1 || listed[0].NotAfter.IsZero() {
		t.Errorf("not_after was not stored: %+v", listed)
	}
}

// A store that cannot answer is not "unknown key": the verifier refuses with
// unavailable, and nothing is accepted.
func TestIssuerKeyStoreFailureRefusesTheToken(t *testing.T) {
	e := newKeyEnv(t)
	iss := e.issuer("app_a", "app-a-key-1")
	e.add(iss)
	v := e.verifier(e.keys)
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := v.Verify(bg, iss.Token())
	if err == nil {
		t.Fatal("a token was accepted with the key store closed")
	}
	if errors.Is(err, identity.ErrKeyNotFound) || contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if _, err := e.keys.Lookup(bg, iss.KeyID); errors.Is(err, identity.ErrKeyNotFound) || err == nil {
		t.Fatalf("Lookup with the store closed = %v, want a non-NotFound error", err)
	}
}

// Public keys only: nothing in the table, the types or the listing can hold a
// private key.
func TestIssuerKeyStoreHoldsNoPrivateKey(t *testing.T) {
	e := newKeyEnv(t)
	iss := e.issuer("app_a", "app-a-key-1")
	e.add(iss)
	priv := iss.PrivateKey()
	seed := priv.Seed()
	rows, err := e.db.QueryContext(bg, `SELECT public_key, fingerprint, label, issuer FROM identity_issuer_keys`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cols [4]string
		if err := rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3]); err != nil {
			t.Fatal(err)
		}
		for _, c := range cols {
			if strings.Contains(c, base64.StdEncoding.EncodeToString(priv)) || strings.Contains(c, base64.StdEncoding.EncodeToString(seed)) {
				t.Errorf("a stored column holds private key material: %q", c)
			}
		}
	}
	// A 64-byte private key is not a public key.
	_, err = e.keys.Add(bg, identity.AddIssuerKey{
		Key:      identity.IssuerKey{KeyID: iss.KeyID + "-b", ApplicationID: "app_a", Issuer: iss.Issuer, Public: ed25519.PublicKey(priv)},
		Declared: identity.ContextExchange{Issuer: iss.Issuer, KeyIDs: []string{iss.KeyID + "-b"}},
	})
	wantCode(t, err, contracts.ErrValidationFailed)
}
