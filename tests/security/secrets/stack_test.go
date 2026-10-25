package secretssec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The in-process AlixAI of SEC-082: a real store, audit log, policy gateway and action coordinator
// over the in-memory host, identity verification, and the gateway serving the public API on a
// loopback port. Every logger of the stack is built by internal/log (the redaction layer) and writes
// to one sink, which also writes the log file; beside each, the same records go to a handler
// with NO redaction, so the test can tell a path that never emits a secret from one whose secret the
// redaction removed.

const (
	appID        = identitytest.DefaultApplicationID
	capRead      = fake.ReadCapability
	capCreate    = fake.ActionCapability
	redactKeyA   = "patient_note"
	redactKeyB   = "ssn"
	hostPolicy   = `{"policy_id":"default-access","role_grants":[{"role":"member","capabilities":["items.get","items.create"]}],"risk_overrides":[],"field_projection":[]}`
	probeTimeout = 10 * time.Second
)

// fanout sends every record to all handlers.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (f fanout) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(a)
	}
	return out
}

func (f fanout) WithGroup(n string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(n)
	}
	return out
}

// logCfg is the production log configuration, with two operator-configured sensitive keys.
func logCfg() config.LogConfig {
	return config.LogConfig{Level: "debug", RedactKeys: []string{redactKeyA, redactKeyB}}
}

// logs is the logging of the stack: the production sink (what an operator's log file holds) and
// the raw one (the same records without the redaction layer, the test's control).
type logs struct {
	prod *sink
	raw  *sink
	dir  string
}

func newLogs(t *testing.T) *logs {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "logs")
	if err := mkdir(dir); err != nil {
		t.Fatal(err)
	}
	return &logs{prod: newSink(t, filepath.Join(dir, "alixai.log")), raw: newSink(t, ""), dir: dir}
}

// logger returns a logger as `alixai serve` builds them (internal/log, from the log section) that
// also feeds the raw sink.
func (l *logs) logger() *slog.Logger {
	return slog.New(fanout{
		alog.NewHandler(logCfg(), l.prod),
		slog.NewJSONHandler(l.raw, &slog.HandlerOptions{Level: slog.LevelDebug}),
	})
}

// credentialEchoingHost wraps the host adapter inside adapters.Wrap, like a transport whose
// application answers a failed commit with the credential it was given.
type credentialEchoingHost struct {
	adapters.AppAdapter
	mu    sync.Mutex
	echo  string
	calls int
}

func (h *credentialEchoingHost) Commit(ctx context.Context, sc contracts.Scope, req adapters.CommitRequest) (act.DeterministicResult, error) {
	h.mu.Lock()
	h.calls++
	echo := h.echo
	h.mu.Unlock()
	if echo != "" {
		return act.DeterministicResult{}, contracts.New(contracts.ErrUnavailable, "the application refused: "+echo)
	}
	return h.AppAdapter.Commit(ctx, sc, req)
}

func (h *credentialEchoingHost) setEcho(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.echo = s
}

// ---- the stack ----

type stack struct {
	t       *testing.T
	logs    *logs
	log     *slog.Logger
	dataDir string
	clk     *clock.Fake
	db      *store.DB
	audit   *audit.Log
	co      *actions.Coordinator
	host    *credentialEchoingHost
	issuer  *identitytest.Issuer
	srv     *gateway.Server
	base    string
	stop    func()
}

func scopeOfAlice() contracts.Scope {
	return contracts.Scope{
		DeploymentID: identitytest.DefaultDeployment, ApplicationID: appID, TenantID: identitytest.DefaultTenant,
		WorkspaceID: identitytest.DefaultWorkspace, PrincipalID: identitytest.DefaultPrincipal,
		Roles: []string{"member"}, CapabilityScopes: []string{capRead, capCreate}, SessionID: identitytest.DefaultSession,
	}
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{t: t, logs: newLogs(t), clk: clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))}
	s.log = s.logs.logger()
	s.dataDir = filepath.Join(t.TempDir(), "data")

	ctx, cancel := context.WithCancel(context.Background())
	var (
		closers []func()
		once    sync.Once
	)
	s.stop = func() {
		once.Do(func() {
			cancel()
			for i := len(closers) - 1; i >= 0; i-- {
				closers[i]()
			}
		})
	}
	t.Cleanup(s.stop)

	var err error
	s.db, err = store.Open(ctx, store.Options{Path: filepath.Join(s.dataDir, store.DefaultFileName), Clock: s.clk, Logger: s.log})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	closers = append(closers, func() { _ = s.db.Close() })

	s.audit, err = audit.New(s.db, audit.Options{Clock: s.clk, Logger: s.log, RedactKeys: []string{redactKeyA, redactKeyB}})
	if err != nil {
		t.Fatal(err)
	}
	sinkP := audit.NewRecordSink[policy.Record](s.audit, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		sub := r.Subject
		return audit.ForIDs(sub.ApplicationID, sub.TenantID, sub.WorkspaceID, sub.PrincipalID, sub.SessionID)
	})
	gw := policy.NewGateway(s.clk, policy.WithSink(sinkP))

	hp, err := policy.ParseHostPolicy([]byte(hostPolicy))
	if err != nil {
		t.Fatal(err)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: appID, Version: "1.0.0", Status: policy.StatusActive, Roles: []string{"member"},
		Capabilities: []policy.CapabilityDecl{
			{ID: capRead, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
				Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
			{ID: capCreate, Audience: policy.AudienceUser, EffectClass: policy.EffectExternal, RiskLevel: contracts.RiskConsequential,
				Confirmation: policy.ConfirmExplicit, Fields: []policy.FieldDecl{{Name: "name", Required: true}}},
		},
		Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("policy.NewPackage: %v", err)
	}

	s.host = &credentialEchoingHost{AppAdapter: fake.NewAdapter(fake.NewApp(fake.WithClock(s.clk)))}
	guard, err := adapters.Wrap(s.host, adapters.Options{ApplicationID: appID})
	if err != nil {
		t.Fatal(err)
	}
	s.co, err = actions.New(actions.Deps{
		DB: s.db, Policy: gw, Audit: s.audit, Clock: s.clk, Logger: s.log,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return pp, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}

	s.issuer = identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: appID, Clock: s.clk})
	keys := identity.NewKeyRegistry(s.log)
	if err := keys.Register(s.issuer.Key()); err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment, Keys: keys,
		Replay: identity.NewSQLiteReplayCache(s.db, s.clk, s.log), Clock: s.clk, Logger: s.log,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The product's /v1 areas need the whole dialogue stack to mount; this stack carries the
	// credentials through the real identity middleware and the real coordinator by routes of its own
	// (canaryArea), under /v1 so that they sit behind the same middleware.
	s.srv, err = gateway.NewServer(gateway.Options{
		Logger: s.log, Clock: s.clk, V1Auth: identity.Middleware(verifier, identity.WithLogger(s.log)),
		ExtraAreas: map[string]gateway.AreaFunc{"canary": s.canaryArea},
	})
	if err != nil {
		t.Fatalf("gateway.NewServer: %v", err)
	}
	ln, err := gateway.Listen("127.0.0.1:0", s.srv.Policy())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- s.srv.Serve(ctx, ln) }()
	closers = append(closers, func() {
		select {
		case <-served:
		case <-time.After(probeTimeout):
			t.Errorf("the gateway did not stop")
		}
	})
	s.base = "http://" + ln.Addr().String()
	waitListening(t, ln.Addr())
	return s
}

// canaryArea mounts the routes this stack adds to the gateway.
//
//	GET  /v1/canary/action/{id}          the preparation, as the caller's verified scope sees it
//	POST /v1/canary/action/{id}/confirm  confirm and commit it; strict body {"token": ...} like the product's route
//	POST /canary/echo                    application code that logs what it receives, with the headers it came with
//	GET  /canary/panic?m=...             application code that panics with the message it is given
//
// The first two are behind the identity middleware and call the real action coordinator with the scope the
// middleware verified; the last two stand for code the product does not have.
func (s *stack) canaryArea(rt *gateway.Router, d *gateway.Deps) {
	v1 := rt.V1()
	v1.HandleFunc("GET /canary/action/{id}", func(w http.ResponseWriter, r *http.Request) {
		sc, err := identity.RequireScope(r.Context())
		if err != nil {
			http.Error(w, "no scope", http.StatusUnauthorized)
			return
		}
		pa, err := s.co.Get(r.Context(), sc, r.PathValue("id"))
		if err != nil {
			http.Error(w, "error", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(pa)
	})
	v1.HandleFunc("POST /canary/action/{id}/confirm", func(w http.ResponseWriter, r *http.Request) {
		sc, err := identity.RequireScope(r.Context())
		if err != nil {
			http.Error(w, "no scope", http.StatusUnauthorized)
			return
		}
		var req struct {
			Token string `json:"token"`
		}
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "request body is not valid JSON for this endpoint", http.StatusBadRequest)
			return
		}
		tok, err := actions.ParseConfirmationToken(req.Token)
		if err != nil {
			http.Error(w, "malformed token", http.StatusBadRequest)
			return
		}
		if _, err := s.co.Confirm(r.Context(), sc, actions.ConfirmInput{PreparationID: r.PathValue("id"), Token: tok}); err != nil {
			http.Error(w, "confirm refused", http.StatusConflict)
			return
		}
		if _, err := s.co.Commit(r.Context(), sc, r.PathValue("id")); err != nil {
			http.Error(w, "commit failed", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rt.HandleFunc("POST /canary/echo", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		d.Logger.Info("canary route received a request",
			"body", body,
			"headers", r.Header,
			"authorization_header", r.Header.Get("Authorization"),
			"cookie_header", r.Header.Get("Cookie"),
			"raw_header_text", "Cookie: "+r.Header.Get("Cookie")+"; Authorization: "+r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	})
	rt.HandleFunc("GET /canary/panic", func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New(r.URL.Query().Get("m")))
	})
}

// waitListening returns once the address accepts a connection (the readiness the clocks start after).
func waitListening(t *testing.T, addr net.Addr) {
	t.Helper()
	deadline := time.Now().Add(probeTimeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s does not accept connections", addr)
}

func (s *stack) token(mods ...func(*identitytest.Draft)) string {
	base := []func(*identitytest.Draft){func(d *identitytest.Draft) {
		d.Set(identity.ClaimCapabilityScopes, []string{capRead, capCreate})
	}}
	return s.issuer.Token(append(base, mods...)...)
}
