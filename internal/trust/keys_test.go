package trust

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestTrustStoreSchema: the module's tables follow the store's rules. The
// publisher-key table is exempt from the tenant/workspace columns on purpose:
// a publisher key is registered per application and authenticates that
// application's packages for all its tenants; it holds no tenant data, and
// every query filters on application_id.
func TestTrustStoreSchema(t *testing.T) {
	_, db := newTrustStore(t, nil)
	storetest.AssertScopedTables(t, db, "trust_publisher_keys")

	ctx := context.Background()
	var cols []string
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('trust_publisher_keys') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, n)
	}
	if len(cols) == 0 || cols[0] != "application_id" {
		t.Errorf("table columns = %v; application_id must lead", cols)
	}

	t.Run("the store starts empty: nothing is trusted implicitly", func(t *testing.T) {
		st, _ := newTrustStore(t, nil)
		for _, app := range []string{testApp, PlatformApplicationID} {
			keys, err := st.ListKeys(ctx, app)
			if err != nil || len(keys) != 0 {
				t.Errorf("%s: %v, %v", app, keys, err)
			}
		}
	})
}

func TestRegisterKey(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	st, _ := newTrustStore(t, clk)
	k := newKey(t, "pub-2026")

	got, err := st.RegisterKey(ctx, testApp, k.id, k.pub, "release signing")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != KeyActive || !got.RegisteredAt.Equal(t0) || got.Label != "release signing" || !got.PublicKey.Equal(k.pub) {
		t.Errorf("registered key = %+v", got)
	}
	back, err := st.LookupKey(ctx, testApp, k.id)
	if err != nil {
		t.Fatal(err)
	}
	if !back.PublicKey.Equal(k.pub) || back.Status != KeyActive || !back.RetiredAt.IsZero() || !back.RevokedAt.IsZero() {
		t.Errorf("looked-up key = %+v", back)
	}

	t.Run("scoped per application", func(t *testing.T) {
		_, err := st.LookupKey(ctx, "another-app", k.id)
		wantFail(t, err, contracts.ErrUnauthenticated, ErrUnknownKey)
		other := newKey(t, k.id) // the same key id under another application is an unrelated key
		if _, err := st.RegisterKey(ctx, "another-app", other.id, other.pub, ""); err != nil {
			t.Fatal(err)
		}
		a, _ := st.LookupKey(ctx, testApp, k.id)
		b, _ := st.LookupKey(ctx, "another-app", k.id)
		if a.PublicKey.Equal(b.PublicKey) {
			t.Error("two applications share one key row")
		}
		keys, _ := st.ListKeys(ctx, testApp)
		if len(keys) != 1 {
			t.Errorf("ListKeys leaks another application's keys: %+v", keys)
		}
	})

	t.Run("conflicts", func(t *testing.T) {
		_, err := st.RegisterKey(ctx, testApp, k.id, newKey(t, "x").pub, "")
		wantFail(t, err, contracts.ErrConflict, ErrKeyConflict) // same id, other key: never replaced
		_, err = st.RegisterKey(ctx, testApp, k.id, k.pub, "")
		wantFail(t, err, contracts.ErrConflict, ErrKeyConflict) // same id, same key
		_, err = st.RegisterKey(ctx, testApp, "alias", k.pub, "")
		wantFail(t, err, contracts.ErrConflict, ErrKeyConflict) // same key, other id
		back, _ := st.LookupKey(ctx, testApp, k.id)
		if !back.PublicKey.Equal(k.pub) {
			t.Error("a refused registration changed the key")
		}
	})

	t.Run("validation", func(t *testing.T) {
		good := newKey(t, "g").pub
		for name, call := range map[string]func() error{
			"empty application":      func() error { _, e := st.RegisterKey(ctx, "", "k", good, ""); return e },
			"upper-case application": func() error { _, e := st.RegisterKey(ctx, "App", "k", good, ""); return e },
			"wildcard application":   func() error { _, e := st.RegisterKey(ctx, "*", "k", good, ""); return e },
			"empty key id":           func() error { _, e := st.RegisterKey(ctx, testApp, "", good, ""); return e },
			"key id with space":      func() error { _, e := st.RegisterKey(ctx, testApp, "a b", good, ""); return e },
			"wildcard key id":        func() error { _, e := st.RegisterKey(ctx, testApp, "*", good, ""); return e },
			"nil public key":         func() error { _, e := st.RegisterKey(ctx, testApp, "k", nil, ""); return e },
			"short public key":       func() error { _, e := st.RegisterKey(ctx, testApp, "k", good[:31], ""); return e },
			"long public key":        func() error { _, e := st.RegisterKey(ctx, testApp, "k", append(good[:32:32], 0), ""); return e },
			"huge label":             func() error { _, e := st.RegisterKey(ctx, testApp, "k", good, strings.Repeat("x", 300)); return e },
			"label not UTF-8":        func() error { _, e := st.RegisterKey(ctx, testApp, "k", good, "\xff"); return e },
		} {
			if err := call(); err == nil {
				t.Errorf("%s: accepted", name)
			} else if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("%s: code %s (%v)", name, contracts.CodeOf(err), err)
			}
		}
	})

	t.Run("rotation registers the new key and retires the old one atomically", func(t *testing.T) {
		old := newKey(t, "rot-old")
		if _, err := st.RegisterKey(ctx, "rot-app", old.id, old.pub, ""); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Hour)
		next := newKey(t, "rot-new")
		if _, err := st.RotateKey(ctx, "rot-app", old.id, next.id, next.pub, "next"); err != nil {
			t.Fatal(err)
		}
		o, _ := st.LookupKey(ctx, "rot-app", old.id)
		n, _ := st.LookupKey(ctx, "rot-app", next.id)
		if o.Status != KeyRetired || !o.RetiredAt.Equal(t0.Add(time.Hour)) || n.Status != KeyActive {
			t.Errorf("old %+v new %+v", o, n)
		}

		// A rotation that fails halfway changes nothing: the new key id is taken.
		third := newKey(t, "rot-third")
		_, err := st.RotateKey(ctx, "rot-app", next.id, next.id, third.pub, "")
		wantFail(t, err, contracts.ErrConflict, ErrKeyConflict)
		again, _ := st.LookupKey(ctx, "rot-app", next.id)
		if again.Status != KeyActive {
			t.Errorf("failed rotation retired the old key: %+v", again)
		}
		// Rotating an unknown key registers nothing.
		_, err = st.RotateKey(ctx, "rot-app", "ghost", "rot-fourth", third.pub, "")
		wantFail(t, err, contracts.ErrValidationFailed, ErrUnknownKey)
		if _, err := st.LookupKey(ctx, "rot-app", "rot-fourth"); err == nil {
			t.Error("rotation of an unknown key registered the new key anyway")
		}
		// Retiring twice keeps the first time.
		clk.Advance(time.Hour)
		if err := st.RetireKey(ctx, "rot-app", old.id); err != nil {
			t.Fatal(err)
		}
		o2, _ := st.LookupKey(ctx, "rot-app", old.id)
		if !o2.RetiredAt.Equal(o.RetiredAt) {
			t.Error("retiring twice moved the retirement time")
		}
	})
}

// TestTestOnlyKeysAreRefused: the published TEST-ONLY keys (anyone can sign
// with them) cannot be trusted by a store that did not opt in.
func TestTestOnlyKeysAreRefused(t *testing.T) {
	ctx := context.Background()
	for name, b64 := range map[string]string{"tools/devkeys": DevKeysPublicKeyB64, "schema example": SchemaExamplePublicKeyB64} {
		pub, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		if !IsTestOnlyKey(pub) {
			t.Errorf("%s: IsTestOnlyKey = false", name)
		}
		st, _ := newTrustStore(t, nil)
		_, err = st.RegisterKey(ctx, testApp, "test-only", pub, "")
		wantFail(t, err, contracts.ErrPermissionDenied, ErrTestOnlyKey)
		_, err = st.RotateKey(ctx, testApp, "x", "test-only", pub, "")
		wantFail(t, err, contracts.ErrPermissionDenied, ErrTestOnlyKey)
		if keys, _ := st.ListKeys(ctx, testApp); len(keys) != 0 {
			t.Errorf("%s: a refused key was stored: %+v", name, keys)
		}

		opted, _ := newTrustStore(t, nil, AllowTestOnlyKeys())
		if _, err := opted.RegisterKey(ctx, testApp, "test-only", pub, ""); err != nil {
			t.Errorf("%s: opted-in store refused the key: %v", name, err)
		}
	}
	if IsTestOnlyKey(newKey(t, "x").pub) || IsTestOnlyKey(nil) || IsTestOnlyKey(ed25519.PublicKey("short")) {
		t.Error("a generated key counts as test-only")
	}
}

// TestVerificationFailsClosedWhenTheStoreCannotAnswer: an unavailable trust
// store is a failure, never a pass and never "unknown key, try the next".
func TestVerificationFailsClosedWhenTheStoreCannotAnswer(t *testing.T) {
	f := newFixture(t)
	f.mustVerify()
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := f.verify()
	if err == nil {
		t.Fatal("package verified with the trust store closed")
	}
	if contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("code = %s, want %s (%v)", contracts.CodeOf(err), contracts.ErrUnavailable, err)
	}

	t.Run("a verifier with no store", func(t *testing.T) {
		v := NewVerifier(nil, nil)
		_, err := v.VerifyPackage(context.Background(), fstest.MapFS{}, PackageOptions{ApplicationID: testApp, PackageVersion: "1.0.0", PublisherKeyIDs: []string{"k"}})
		if err == nil {
			t.Error("verified without a trust store")
		}
	})
}
