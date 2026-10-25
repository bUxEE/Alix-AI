package identitysec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// The host application of the operator-path test is the one the shipped example
// package describes (examples/packages/valid-minimal): its manifest declares the
// issuer, the audience and the one key id its tokens carry, and the operator
// registers a key for it with `alixai identity add-issuer-key -package`, exactly
// as in production. Key pairs are generated per run.
const (
	hostPackage  = "examples/packages/valid-minimal"
	hostApp      = "demo-library"
	hostIssuer   = "https://library.example.test"
	hostKeyID    = "demo-library-ctx-2026"
	hostSpareKey = "demo-library-ctx-2027"
	hostAudience = "alixai"
	hostDeploy   = "dep_sec037"
)

// cliMu serialises the in-process operator commands: the configuration registry
// they read is the process's own.
var cliMu sync.Mutex

// runCLI runs one `alixai` command in-process, as the e2e gate does.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cliMu.Lock()
	defer cliMu.Unlock()
	var out, errb bytes.Buffer
	code = cli.Run(testCtx(t), args, &cli.Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		LookupEnv: func(string) (string, bool) { return "", false },
		Config:    config.Default,
	})
	return code, out.String(), errb.String()
}

// fingerprint is the lower-case hex SHA-256 of a raw public key, which is what
// the store and the audit event call the fingerprint.
func fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// auditEvent is one row of the audit log.
type auditEvent struct {
	typ, application, tenant string
	data                     map[string]any
	raw                      string
}

// auditEvents reads every event of a type. It is the detector of the audit
// half of SEC-037: the controls below run it against a revocation that was
// deliberately not audited and it must find nothing.
func auditEvents(t *testing.T, db *store.DB, eventType string) []auditEvent {
	t.Helper()
	q, err := db.QueryContext(testCtx(t),
		`SELECT event_type, application_id, tenant_id, data FROM audit_events WHERE event_type = ? ORDER BY seq`, eventType)
	if err != nil {
		t.Fatalf("reading the audit log: %v", err)
	}
	defer q.Close()
	var out []auditEvent
	for q.Next() {
		var e auditEvent
		if err := q.Scan(&e.typ, &e.application, &e.tenant, &e.raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(e.raw), &e.data); err != nil {
			t.Fatalf("audit data is not JSON: %v", err)
		}
		out = append(out, e)
	}
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// containsKeyBytes reports whether s carries the public key in any of the
// encodings an event might use.
func containsKeyBytes(s string, pub ed25519.PublicKey) bool {
	for _, enc := range []string{
		base64.StdEncoding.EncodeToString(pub),
		base64.RawStdEncoding.EncodeToString(pub),
		base64.URLEncoding.EncodeToString(pub),
		base64.RawURLEncoding.EncodeToString(pub),
		hex.EncodeToString(pub),
	} {
		if strings.Contains(s, enc) {
			return true
		}
	}
	return false
}

// hostIssuerFor is a host application's signing identity: a fresh key pair under
// a key id, stamping tokens with clk.
func hostIssuerFor(t *testing.T, keyID string, clk clock.Clock) *identitytest.Issuer {
	t.Helper()
	return identitytest.NewIssuer(t, identitytest.IssuerConfig{
		ApplicationID: hostApp, Issuer: hostIssuer, KeyID: keyID, Audience: hostAudience, Clock: clk,
	})
}

// SEC-037
//
// A key that is revoked or rotated out stops verifying at once, the removal is
// an audit event, and the overlap of a rotation ends when it was declared to.
func TestSecRevokedIssuerKeyRejected(t *testing.T) {
	t.Run("operator revocation: refused from the next token on, audited, the other key unaffected", func(t *testing.T) {
		root := t.TempDir()
		dataDir := filepath.Join(root, "data")
		cfg := filepath.Join(root, "alixai.yaml")
		writeFile(t, cfg, fmt.Sprintf("data_dir: %q\nidentity:\n  deployment_id: %s\n  audience: %s\n",
			filepath.ToSlash(dataDir), hostDeploy, hostAudience))
		pkgDir := filepath.Join(secutil.RepoRoot(t), filepath.FromSlash(hostPackage))

		rtClock := clock.Real{}
		revoked := hostIssuerFor(t, hostKeyID, rtClock)
		spare := hostIssuerFor(t, hostSpareKey, rtClock)

		// Provision: the revoked-to-be key through the operator command (the
		// production path, which writes the audit event in the same
		// transaction).
		code, out, errOut := runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-package", pkgDir,
			"-key-id", hostKeyID, "-public-key", trust.EncodePublicKey(revoked.Public))
		if code != cli.ExitOK {
			t.Fatalf("add-issuer-key = %d\n%s\n%s", code, out, errOut)
		}

		db, err := store.Open(testCtx(t), store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		keys := identity.NewIssuerKeyStore(db, rtClock, identity.IssuerKeyStoreOptions{})

		// The still-active key. The shipped package declares one key id, so the
		// second key is provisioned through the store API against a declaration
		// that lists it; it plays no part in what is proved, it is the control
		// that revoking one key leaves the others alone.
		if _, err := keys.Add(testCtx(t), identity.AddIssuerKey{
			Key:      spare.Key(),
			Declared: identity.ContextExchange{Issuer: hostIssuer, Audience: hostAudience, KeyIDs: []string{hostSpareKey}},
		}); err != nil {
			t.Fatalf("provisioning the second key: %v", err)
		}

		// The service as `alixai serve` builds it: the verifier over the issuer
		// key store (read on every token, no cache) and the SQLite replay
		// cache, behind the identity middleware.
		verifier, err := identity.NewVerifier(identity.Options{
			DeploymentID: hostDeploy, Audience: hostAudience, Keys: keys,
			Replay: identity.NewSQLiteReplayCache(db, rtClock, nil), Clock: rtClock,
		})
		if err != nil {
			t.Fatal(err)
		}
		handler := identity.Middleware(verifier)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		serve := func(token string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec
		}

		// Tokens signed by the key that will be revoked, minted while it is
		// still trusted and not yet presented: they are valid for minutes.
		beforeRevoke := revoked.Token()
		stillValid := revoked.Token()
		afterRevokeClaim := revoked.Token()
		spareBefore := spare.Token()
		spareAfter := spare.Token()

		// CONTROL: before the revocation the same key's tokens are accepted, so
		// the refusals below are caused by the revocation and by nothing else.
		v, err := verifier.Verify(testCtx(t), beforeRevoke)
		if err != nil {
			t.Fatalf("a token signed by the key was refused before the revocation: %v", err)
		}
		if v.KeyID != hostKeyID || v.Scope.ApplicationID != hostApp {
			t.Fatalf("verified key %q application %q, want %q for %q", v.KeyID, v.Scope.ApplicationID, hostKeyID, hostApp)
		}
		if rec := serve(afterRevokeClaim); rec.Code != http.StatusNoContent {
			t.Fatalf("a valid token over HTTP before the revocation = %d: %s", rec.Code, rec.Body.String())
		}
		if _, err := verifier.Verify(testCtx(t), spareBefore); err != nil {
			t.Fatalf("a token of the second key was refused before the revocation: %v", err)
		}
		// Detector baseline: one key was added through the operator command and
		// nothing has been revoked, so the revocation event does not exist yet.
		if n := len(auditEvents(t, db, "identity.issuer_key_added")); n != 1 {
			t.Fatalf("issuer_key_added events = %d, want 1 (the operator command audits an addition)", n)
		}
		if n := len(auditEvents(t, db, "identity.issuer_key_revoked")); n != 0 {
			t.Fatalf("issuer_key_revoked events before any revocation = %d, want 0", n)
		}

		// Revoke through the operator command.
		code, out, errOut = runCLI(t, "identity", "revoke-issuer-key", "-config", cfg, "-app", hostApp,
			"-key-id", hostKeyID, "-reason", "sec-037 key compromised")
		if code != cli.ExitOK || !strings.Contains(out, "revoked") {
			t.Fatalf("revoke-issuer-key = %d\n%s\n%s", code, out, errOut)
		}

		// Immediately, with no waiting and no restart of anything: a token that
		// was minted before the revocation and has minutes left is refused, as
		// is one minted after it.
		if ttl := time.Until(tokenExpiry(t, stillValid)); ttl < time.Minute {
			t.Fatalf("the token under test has %s left, the check below would prove nothing", ttl)
		}
		_, err = verifier.Verify(testCtx(t), stillValid)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
		_, err = verifier.Verify(testCtx(t), revoked.Token())
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
		if rec := serve(revoked.Token()); rec.Code != http.StatusUnauthorized {
			t.Fatalf("a token of the revoked key over HTTP = %d, want 401: %s", rec.Code, rec.Body.String())
		}

		// CONTROL: the other key keeps working through the revocation, both for
		// a token minted before it and for a new one.
		if _, err := verifier.Verify(testCtx(t), spareAfter); err != nil {
			t.Fatalf("the still-active key stopped working when the other was revoked: %v", err)
		}
		if _, err := verifier.Verify(testCtx(t), spare.Token()); err != nil {
			t.Fatalf("the still-active key refuses new tokens after the other was revoked: %v", err)
		}

		// The removal is an audit event of the application the key spoke for,
		// naming the key and its fingerprint, with the operator's reason and no
		// key material.
		events := auditEvents(t, db, "identity.issuer_key_revoked")
		if len(events) != 1 {
			t.Fatalf("issuer_key_revoked events = %d, want exactly 1", len(events))
		}
		ev := events[0]
		if ev.application != hostApp || ev.tenant != "" {
			t.Errorf("the event is on chain %q/%q, want the application chain of %q", ev.application, ev.tenant, hostApp)
		}
		if ev.data["key_id"] != hostKeyID || ev.data["fingerprint"] != fingerprint(revoked.Public) || ev.data["reason"] != "sec-037 key compromised" {
			t.Errorf("event data = %v, want key %q, fingerprint %q and the operator's reason", ev.data, hostKeyID, fingerprint(revoked.Public))
		}
		if containsKeyBytes(ev.raw, revoked.Public) {
			t.Errorf("the audit event carries key material: %s", ev.raw)
		}
		alog, err := audit.New(db, audit.Options{Clock: rtClock})
		if err != nil {
			t.Fatal(err)
		}
		rep, err := alog.VerifyChain(testCtx(t), audit.ChainKey{ApplicationID: hostApp}, audit.VerifyOptions{})
		if err != nil || !rep.OK() {
			t.Fatalf("the application's audit chain does not verify after the revocation: %v %+v", err, rep.Problems)
		}

		// A rotated-out key does not come back under the same name.
		code, _, errOut = runCLI(t, "identity", "add-issuer-key", "-config", cfg, "-package", pkgDir,
			"-key-id", hostKeyID, "-public-key", trust.EncodePublicKey(revoked.Public))
		if code != cli.ExitError {
			t.Errorf("re-adding a revoked key = %d, want ExitError: %s", code, errOut)
		}
		_, err = verifier.Verify(testCtx(t), revoked.Token())
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
		if n := len(auditEvents(t, db, "identity.issuer_key_revoked")); n != 1 {
			t.Errorf("issuer_key_revoked events = %d after a refused re-add, want still 1", n)
		}
	})

	t.Run("control: a revocation made without an audit hook leaves no event, and the detector sees that", func(t *testing.T) {
		db := storetest.Open(t, nil)
		clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
		iss := hostIssuerFor(t, hostKeyID, clk)
		keys := identity.NewIssuerKeyStore(db, clk, identity.IssuerKeyStoreOptions{}) // no Record hook: the planted violation
		if _, err := keys.Add(testCtx(t), identity.AddIssuerKey{
			Key:      iss.Key(),
			Declared: identity.ContextExchange{Issuer: hostIssuer, Audience: hostAudience, KeyIDs: []string{hostKeyID}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := keys.Revoke(testCtx(t), hostApp, hostKeyID, "unaudited"); err != nil || !changed {
			t.Fatalf("Revoke = changed %v, err %v", changed, err)
		}
		// The key is gone (the revocation itself works)...
		if _, err := keys.Lookup(testCtx(t), hostKeyID); err == nil {
			t.Fatal("the revoked key is still served by the store")
		}
		// ...and the detector that the test above relies on reports that no
		// audit event exists for it.
		if n := len(auditEvents(t, db, "identity.issuer_key_revoked")); n != 0 {
			t.Fatalf("an unaudited revocation produced %d events", n)
		}
	})

	t.Run("a rotation overlap is bounded by the declared end of the old key", func(t *testing.T) {
		db := storetest.Open(t, nil)
		clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
		oldKey := hostIssuerFor(t, "host-key-old", clk)
		newKey := hostIssuerFor(t, "host-key-new", clk)
		keys := identity.NewIssuerKeyStore(db, clk, identity.IssuerKeyStoreOptions{})
		declared := identity.ContextExchange{Issuer: hostIssuer, Audience: hostAudience, KeyIDs: []string{"host-key-old", "host-key-new"}}
		overlapEnd := clk.Now().Add(30 * time.Minute)
		oldReg := oldKey.Key()
		oldReg.NotAfter = overlapEnd
		for _, k := range []identity.IssuerKey{oldReg, newKey.Key()} {
			if _, err := keys.Add(testCtx(t), identity.AddIssuerKey{Key: k, Declared: declared}); err != nil {
				t.Fatalf("Add %s: %v", k.KeyID, err)
			}
		}
		verifier, err := identity.NewVerifier(identity.Options{
			DeploymentID: hostDeploy, Audience: hostAudience, Keys: keys,
			Replay: identity.NewSQLiteReplayCache(db, clk, nil), Clock: clk,
		})
		if err != nil {
			t.Fatal(err)
		}
		boundedOverlap(t, clk, verifier, oldKey, newKey, overlapEnd)
	})

	t.Run("the in-memory registry is held to the same rules, and logs the removal", func(t *testing.T) {
		clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
		var logBuf bytes.Buffer
		reg := identity.NewKeyRegistry(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		one := hostIssuerFor(t, "host-key-one", clk)
		two := hostIssuerFor(t, "host-key-two", clk)
		for _, i := range []*identitytest.Issuer{one, two} {
			if err := reg.Register(i.Key()); err != nil {
				t.Fatal(err)
			}
		}
		db := storetest.Open(t, nil)
		verifier, err := identity.NewVerifier(identity.Options{
			DeploymentID: hostDeploy, Audience: hostAudience, Keys: reg,
			Replay: identity.NewSQLiteReplayCache(db, clk, nil), Clock: clk,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Baseline for the log detector: two registrations, no removal.
		if n := len(logRecords(t, &logBuf, "issuer key registered")); n != 2 {
			t.Fatalf("registration records = %d, want 2", n)
		}
		if n := len(logRecords(t, &logBuf, "issuer key revoked")); n != 0 {
			t.Fatalf("revocation records before any revocation = %d, want 0", n)
		}

		pending := one.Token() // minted while trusted, not yet presented
		if _, err := verifier.Verify(testCtx(t), one.Token()); err != nil {
			t.Fatalf("control: a token of the key before revocation: %v", err)
		}
		if !reg.Revoke("host-key-one") {
			t.Fatal("Revoke reported that the key did not exist")
		}
		_, err = verifier.Verify(testCtx(t), pending)
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
		if _, err := verifier.Verify(testCtx(t), two.Token()); err != nil {
			t.Fatalf("control: the other key stopped working: %v", err)
		}
		if reg.Revoke("host-key-one") {
			t.Error("revoking a key twice reported a second removal")
		}

		recs := logRecords(t, &logBuf, "issuer key revoked")
		if len(recs) != 1 || recs[0]["key_id"] != "host-key-one" || recs[0]["application_id"] != hostApp {
			t.Fatalf("revocation log records = %v, want exactly one naming host-key-one of %s", recs, hostApp)
		}
		if containsKeyBytes(logBuf.String(), one.Public) || containsKeyBytes(logBuf.String(), two.Public) {
			t.Errorf("the registry log carries key material:\n%s", logBuf.String())
		}

		// RevokeApplication is the response to a compromised host: every key of
		// the application goes at once.
		if n := reg.RevokeApplication(hostApp); n != 1 {
			t.Fatalf("RevokeApplication removed %d keys, want the 1 left", n)
		}
		_, err = verifier.Verify(testCtx(t), two.Token())
		wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonUnknownKey)
		if n := len(logRecords(t, &logBuf, "issuer key revoked")); n != 2 {
			t.Errorf("revocation log records = %d after RevokeApplication, want 2", n)
		}

		// Retire bounds an overlap in the registry exactly as NotAfter does in
		// the store.
		reg2 := identity.NewKeyRegistry(nil)
		oldKey := hostIssuerFor(t, "host-key-old", clk)
		newKey := hostIssuerFor(t, "host-key-new", clk)
		for _, i := range []*identitytest.Issuer{oldKey, newKey} {
			if err := reg2.Register(i.Key()); err != nil {
				t.Fatal(err)
			}
		}
		if err := reg2.Retire("host-key-missing", clk.Now()); err == nil {
			t.Error("Retire accepted a key that is not registered")
		}
		overlapEnd := clk.Now().Add(30 * time.Minute)
		if err := reg2.Retire("host-key-old", overlapEnd); err != nil {
			t.Fatal(err)
		}
		v2, err := identity.NewVerifier(identity.Options{
			DeploymentID: hostDeploy, Audience: hostAudience, Keys: reg2,
			Replay: identity.NewSQLiteReplayCache(db, clk, nil), Clock: clk,
		})
		if err != nil {
			t.Fatal(err)
		}
		boundedOverlap(t, clk, v2, oldKey, newKey, overlapEnd)
	})

	t.Run("control: the no-grace detector sees a resolver that keeps serving a revoked key", func(t *testing.T) {
		if !refusedRightAfterRevoke(t, func(r *identity.KeyRegistry) identity.KeyResolver { return r }) {
			t.Fatal("the production registry served a revoked key")
		}
		if refusedRightAfterRevoke(t, func(r *identity.KeyRegistry) identity.KeyResolver { return &cachingResolver{inner: r} }) {
			t.Fatal("the detector did not notice a resolver that keeps serving a revoked key from its cache")
		}
	})
}

// boundedOverlap is the rotation check shared by the store and the registry: the
// old key verifies until overlapEnd and not from that instant, even for a token
// that still has minutes to live, and the new key verifies throughout.
func boundedOverlap(t *testing.T, clk *clock.Fake, v *identity.Verifier, oldKey, newKey *identitytest.Issuer, overlapEnd time.Time) {
	t.Helper()
	// During the overlap both keys work.
	if _, err := v.Verify(testCtx(t), oldKey.Token()); err != nil {
		t.Fatalf("the old key was refused inside its overlap: %v", err)
	}
	if _, err := v.Verify(testCtx(t), newKey.Token()); err != nil {
		t.Fatalf("the new key was refused inside the overlap: %v", err)
	}

	// One second before the declared end: still accepted. The token is minted
	// there and lives past the end, which is the case that matters.
	clk.Set(overlapEnd.Add(-time.Second))
	crossing := oldKey.Token() // exp is about five minutes after the end
	crossing2 := oldKey.Token()
	if _, err := v.Verify(testCtx(t), crossing); err != nil {
		t.Fatalf("the old key was refused one second before the end of its overlap: %v", err)
	}

	// From the end on: refused as retired, although the token is unexpired.
	clk.Set(overlapEnd)
	_, err := v.Verify(testCtx(t), crossing2)
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonKeyRetired)
	_, err = v.Verify(testCtx(t), oldKey.Token())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonKeyRetired)

	// And it stays refused later; the new key is accepted throughout.
	clk.Advance(time.Hour)
	_, err = v.Verify(testCtx(t), oldKey.Token())
	wantReject(t, err, contracts.ErrUnauthenticated, identity.ReasonKeyRetired)
	if _, err := v.Verify(testCtx(t), newKey.Token()); err != nil {
		t.Fatalf("the new key was refused after the old one retired: %v", err)
	}
}

// logRecords returns the JSON log records with the given message.
func logRecords(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tokenExpiry reads the exp claim of a token without verifying it, to prove a
// test token still has time to live.
func tokenExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return time.Unix(claims.Exp, 0)
}

// cachingResolver is the planted violation of the "no grace" control: a key
// resolver that remembers the keys it served, so a revocation in the registry
// behind it is not seen until the cache is dropped.
type cachingResolver struct {
	inner identity.KeyResolver

	mu    sync.Mutex
	cache map[string]identity.IssuerKey
}

func (c *cachingResolver) Lookup(ctx context.Context, keyID string) (identity.IssuerKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.cache[keyID]; ok {
		return k, nil
	}
	k, err := c.inner.Lookup(ctx, keyID)
	if err == nil {
		if c.cache == nil {
			c.cache = map[string]identity.IssuerKey{}
		}
		c.cache[keyID] = k
	}
	return k, err
}

// refusedRightAfterRevoke is the "no grace" detector: it serves one token
// through resolverOf(registry), revokes the key, and reports whether the very
// next token of that key was refused.
func refusedRightAfterRevoke(t *testing.T, resolverOf func(*identity.KeyRegistry) identity.KeyResolver) bool {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	reg := identity.NewKeyRegistry(nil)
	iss := hostIssuerFor(t, "host-key-grace", clk)
	if err := reg.Register(iss.Key()); err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t, nil)
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: hostDeploy, Audience: hostAudience, Keys: resolverOf(reg),
		Replay: identity.NewSQLiteReplayCache(db, clk, nil), Clock: clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(testCtx(t), iss.Token()); err != nil {
		t.Fatalf("a token of an active key was refused: %v", err)
	}
	reg.Revoke("host-key-grace")
	_, err = v.Verify(testCtx(t), iss.Token())
	return err != nil && identity.ReasonOf(err) == identity.ReasonUnknownKey
}
