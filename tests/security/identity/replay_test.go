package identitysec

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// testCtx is the context of every call: bounded, so no test waits unboundedly.
func testCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// decodeJSON decodes a response body.
func decodeJSON(t testing.TB, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("response is not the expected JSON: %v\n%s", err, raw)
	}
}

// wantReject fails t unless err is a typed refusal with the given code and
// reason. Asserting the reason is what proves the check the test is named for
// fired, and not some earlier one.
func wantReject(t testing.TB, err error, code contracts.ErrCode, reason identity.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("token accepted; want %s (%s)", code, reason)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *contracts.Error", err)
	}
	if ce.Code != code {
		t.Fatalf("code = %s, want %s (error: %v)", ce.Code, code, err)
	}
	if got := identity.ReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q (error: %v)", got, reason, err)
	}
}

// deployment is one AlixAI installation as far as token replay is concerned: a
// database file that outlives every process, one injected clock, and the host
// application's issuer. A "process" is a verifier and a store handle built over
// that file; closing the handle and building a new one is a restart.
type deployment struct {
	t      *testing.T
	path   string
	clk    *clock.Fake
	issuer *identitytest.Issuer
	keys   *identity.KeyRegistry
}

func newDeployment(t *testing.T) *deployment {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	iss := identitytest.NewIssuer(t, identitytest.IssuerConfig{Clock: clk})
	keys := identity.NewKeyRegistry(nil)
	if err := keys.Register(iss.Key()); err != nil {
		t.Fatal(err)
	}
	return &deployment{t: t, path: filepath.Join(t.TempDir(), "alixai.db"), clk: clk, issuer: iss, keys: keys}
}

// sibling is another installation with its own, empty database that trusts the
// same issuer and shares the clock: the "empty store" of the controls.
func (d *deployment) sibling() *deployment {
	return &deployment{t: d.t, path: filepath.Join(d.t.TempDir(), "alixai.db"), clk: d.clk, issuer: d.issuer, keys: d.keys}
}

// process is one run of the service over the deployment's database.
type process struct {
	db       *store.DB
	replay   *identity.SQLiteReplayCache
	verifier *identity.Verifier
	once     sync.Once
}

// boot starts a process: opens the store file and builds a verifier on it.
func (d *deployment) boot() *process {
	d.t.Helper()
	db, err := store.Open(testCtx(d.t), store.Options{Path: d.path})
	if err != nil {
		d.t.Fatalf("store.Open: %v", err)
	}
	replay := identity.NewSQLiteReplayCache(db, d.clk, nil)
	v, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment,
		Keys:         d.keys,
		Replay:       replay,
		Clock:        d.clk,
	})
	if err != nil {
		d.t.Fatalf("NewVerifier: %v", err)
	}
	p := &process{db: db, replay: replay, verifier: v}
	d.t.Cleanup(p.stop)
	return p
}

// stop ends the process: the store is closed, as it is when the service exits.
func (p *process) stop() {
	p.once.Do(func() { _ = p.db.Close() })
}

func (p *process) rows(t testing.TB) int {
	t.Helper()
	n, err := p.replay.Len(testCtx(t))
	if err != nil {
		t.Fatalf("replay Len: %v", err)
	}
	return n
}

// tokenWithID mints a valid token for the default context carrying jti.
func (d *deployment) tokenWithID(jti string) string {
	return d.issuer.Token(func(dr *identitytest.Draft) { dr.Set(identity.ClaimTokenID, jti) })
}

// memReplay is the deliberately weak replay cache of the planted control: it
// keeps used nonces in memory only, which is what a cache that does not
// survive a restart looks like.
type memReplay struct {
	mu   sync.Mutex
	seen map[string]time.Time
	clk  clock.Clock
}

func newMemReplay(clk clock.Clock) *memReplay {
	return &memReplay{seen: map[string]time.Time{}, clk: clk}
}

func (m *memReplay) Consume(_ context.Context, scope contracts.Scope, jti string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope.ApplicationID + "/" + scope.TenantID + "/" + scope.WorkspaceID + "/" + jti
	if until, ok := m.seen[key]; ok && until.After(m.clk.Now()) {
		return contracts.New(contracts.ErrReplayed, "context token already used").WithDetails(identity.DetailReason, string(identity.ReasonReplayed))
	}
	m.seen[key] = expiresAt
	return nil
}

// replaySurvivesRestart is the restart detector: a verifier over the replay
// cache newCache(boot) accepts a token once, is "restarted" (a new verifier over
// a new cache from newCache, which for a persistent cache reopens the same
// file), and the token is presented again. It reports whether the second
// presentation was refused with the typed replay error.
func replaySurvivesRestart(t *testing.T, d *deployment, newCache func(boot int) identity.ReplayCache) bool {
	t.Helper()
	token := d.issuer.Token()
	build := func(boot int) *identity.Verifier {
		v, err := identity.NewVerifier(identity.Options{
			DeploymentID: identitytest.DefaultDeployment,
			Keys:         d.keys,
			Replay:       newCache(boot),
			Clock:        d.clk,
		})
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		return v
	}
	if _, err := build(1).Verify(testCtx(t), token); err != nil {
		t.Fatalf("the first presentation of a valid token was refused: %v", err)
	}
	_, err := build(2).Verify(testCtx(t), token)
	var ce *contracts.Error
	return errors.As(err, &ce) && ce.Code == contracts.ErrReplayed && identity.ReasonOf(err) == identity.ReasonReplayed
}

// SEC-034
//
// A captured token is useless the second time, also after the process that saw
// the first use is gone, and the memory of it does not outlive the token.
func TestSecReplayCacheSurvivesRestart(t *testing.T) {
	t.Run("second use is rejected, in the same process and after restarts", func(t *testing.T) {
		d := newDeployment(t)
		const jti = "sec034-nonce-a-0123456"
		token := d.tokenWithID(jti)

		p1 := d.boot()
		v, err := p1.verifier.Verify(testCtx(t), token)
		if err != nil {
			t.Fatalf("first presentation: %v", err)
		}
		if v.TokenID != jti {
			t.Fatalf("verified jti = %q, want %q", v.TokenID, jti)
		}
		_, err = p1.verifier.Verify(testCtx(t), token)
		wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)
		if n := p1.rows(t); n != 1 {
			t.Fatalf("replay rows = %d, want 1", n)
		}
		p1.stop()

		// Restart: a new process over the same file. The same token is refused,
		// and so is a freshly signed token that reuses the nonce while the first
		// one could still be accepted.
		p2 := d.boot()
		_, err = p2.verifier.Verify(testCtx(t), token)
		wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)
		_, err = p2.verifier.Verify(testCtx(t), d.tokenWithID(jti))
		wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)
		if n := p2.rows(t); n != 1 {
			t.Fatalf("replay rows after the restart = %d, want the one persisted entry", n)
		}

		// Control: refusing the replay is not blanket refusal. A fresh nonce is
		// accepted after the restart, and then refused on its own second use.
		fresh := d.tokenWithID("sec034-nonce-b-0123456")
		if _, err := p2.verifier.Verify(testCtx(t), fresh); err != nil {
			t.Fatalf("a fresh nonce was refused after the restart: %v", err)
		}
		p2.stop()

		// And it keeps holding across a second restart.
		p3 := d.boot()
		for name, tok := range map[string]string{"first token": token, "fresh token": fresh} {
			_, err = p3.verifier.Verify(testCtx(t), tok)
			if err == nil {
				t.Fatalf("%s accepted after the second restart", name)
			}
			wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)
		}
	})

	t.Run("the refusal reaches the HTTP caller as a typed replayed response", func(t *testing.T) {
		d := newDeployment(t)
		token := d.issuer.Token()
		serve := func(p *process) http.Handler {
			return identity.Middleware(p.verifier)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
		}
		call := func(h http.Handler, tok string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec
		}
		p1 := d.boot()
		if rec := call(serve(p1), token); rec.Code != http.StatusNoContent {
			t.Fatalf("first presentation = %d, want 204: %s", rec.Code, rec.Body.String())
		}
		p1.stop()
		p2 := d.boot()
		rec := call(serve(p2), token)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("replay after restart = %d, want 401: %s", rec.Code, rec.Body.String())
		}
		var env contracts.Error
		decodeJSON(t, rec.Body.Bytes(), &env)
		if env.Code != contracts.ErrReplayed {
			t.Fatalf("response code = %q, want %q", env.Code, contracts.ErrReplayed)
		}
		if rec := call(serve(p2), d.issuer.Token()); rec.Code != http.StatusNoContent {
			t.Fatalf("a fresh token after the restart = %d, want 204", rec.Code)
		}
	})

	t.Run("entries expire with the token, and an expired token is refused as expired", func(t *testing.T) {
		d := newDeployment(t)
		const jti = "sec034-nonce-c-0123456"
		token := d.tokenWithID(jti)

		p1 := d.boot()
		if _, err := p1.verifier.Verify(testCtx(t), token); err != nil {
			t.Fatal(err)
		}
		p1.stop()

		// The token's own window is exp + the clock-skew allowance. One second
		// before it closes, the nonce is still held, in a process that never saw
		// the first use.
		p2 := d.boot()
		d.clk.Advance(identitytest.DefaultLifetime + identity.ClockSkew - time.Second)
		_, err := p2.verifier.Verify(testCtx(t), token)
		wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)
		if n := p2.rows(t); n != 1 {
			t.Fatalf("rows inside the window = %d, want 1", n)
		}

		// Past the window the token is expired: a typed expiry, neither accepted
		// nor reported as a replay.
		d.clk.Advance(2 * time.Second)
		_, err = p2.verifier.Verify(testCtx(t), token)
		wantReject(t, err, contracts.ErrExpired, identity.ReasonExpired)

		// The entry is pruned with the token. Prune is the explicit sweep; the
		// service also sweeps on its own about once a minute (checked below).
		n, err := p2.replay.Prune(testCtx(t))
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("Prune removed %d rows, want the 1 expired entry", n)
		}
		if got := p2.rows(t); got != 0 {
			t.Fatalf("rows after the sweep = %d: the nonce is retained after its token expired", got)
		}

		// The nonce is not held forever: a new token that reuses it is accepted,
		// in this process and, once recorded, refused after a restart.
		reuse := d.tokenWithID(jti)
		if _, err := p2.verifier.Verify(testCtx(t), reuse); err != nil {
			t.Fatalf("a nonce past its window was still refused: %v", err)
		}
		p2.stop()
		p3 := d.boot()
		_, err = p3.verifier.Verify(testCtx(t), reuse)
		wantReject(t, err, contracts.ErrReplayed, identity.ReasonReplayed)
	})

	t.Run("the service sweeps expired entries by itself", func(t *testing.T) {
		d := newDeployment(t)
		p1 := d.boot()
		const old = 6
		for range old {
			if _, err := p1.verifier.Verify(testCtx(t), d.issuer.Token()); err != nil {
				t.Fatal(err)
			}
		}
		if n := p1.rows(t); n != old {
			t.Fatalf("rows = %d, want %d", n, old)
		}
		p1.stop()

		p2 := d.boot()
		if n := p2.rows(t); n != old {
			t.Fatalf("rows after the restart = %d, want the %d persisted entries", n, old)
		}
		d.clk.Advance(identitytest.DefaultLifetime + identity.ClockSkew + 2*time.Minute)
		if _, err := p2.verifier.Verify(testCtx(t), d.issuer.Token()); err != nil {
			t.Fatal(err)
		}
		if n := p2.rows(t); n != 1 {
			t.Fatalf("rows = %d, want 1: the %d expired entries should have been swept, only the new nonce kept", n, old)
		}
	})

	t.Run("controls: the rejection is due to the persisted entry", func(t *testing.T) {
		d := newDeployment(t)
		token := d.issuer.Token()

		// A verifier over an EMPTY store accepts the very same token: it is
		// valid, and the refusals above come from the entry a previous process
		// wrote, not from anything about the token.
		p1 := d.boot()
		if _, err := p1.verifier.Verify(testCtx(t), token); err != nil {
			t.Fatal(err)
		}
		p1.stop()
		empty := d.sibling().boot()
		if n := empty.rows(t); n != 0 {
			t.Fatalf("the control store is not empty: %d rows", n)
		}
		if _, err := empty.verifier.Verify(testCtx(t), token); err != nil {
			t.Fatalf("a verifier over an empty store refused a valid first presentation: %v", err)
		}

		// The restart detector sees persistence when there is some...
		file := d.sibling()
		var current *store.DB
		t.Cleanup(func() {
			if current != nil {
				_ = current.Close()
			}
		})
		persistent := func(int) identity.ReplayCache {
			if current != nil {
				_ = current.Close() // the previous process is gone
			}
			db, err := store.Open(testCtx(t), store.Options{Path: file.path})
			if err != nil {
				t.Fatal(err)
			}
			current = db
			return identity.NewSQLiteReplayCache(db, file.clk, nil)
		}
		if !replaySurvivesRestart(t, file, persistent) {
			t.Fatal("the restart detector reports that the SQLite replay cache lost a nonce across a restart")
		}

		// ...and flags a replay cache that keeps its nonces in memory only: a
		// planted violation. If it were not flagged, the check above would
		// prove nothing.
		weak := d.sibling()
		if replaySurvivesRestart(t, weak, func(int) identity.ReplayCache { return newMemReplay(weak.clk) }) {
			t.Fatal("the restart detector did not notice a replay cache that forgets every nonce on restart")
		}
	})
}
