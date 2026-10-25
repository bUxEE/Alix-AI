package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const (
	tDeploy  = "dep_test"
	tApp     = "app_crm"
	tTenant  = "ten_42"
	tTenant2 = "ten_43"
	tWsp     = "wsp_main"

	registeredRedirect = "https://alix.example.test/oauth/callback"
)

var bg = context.Background()

var tStart = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func credentialsSecret(s string) credentials.Secret { return credentials.Secret(s) }

func scopeOf(app, tenant string) contracts.Scope {
	s := storetest.Scope(app, tenant, tWsp)
	s.DeploymentID = tDeploy
	return s
}

// spyCreds records which credential-service methods the engine calls with a token.
type spyCreds struct {
	Credentials
	mu    sync.Mutex
	calls map[string]int
	// flagErr, when set, is what Flag returns instead of recording the verdict.
	flagErr error
}

func (s *spyCreds) Flag(ctx context.Context, sc contracts.Scope, r providers.CredentialRefID, f credentials.Flag) (providers.CredentialRef, error) {
	s.hit("Flag")
	if s.flagErr != nil {
		return providers.CredentialRef{}, s.flagErr
	}
	return s.Credentials.Flag(ctx, sc, r, f)
}

func (s *spyCreds) hit(name string) {
	s.mu.Lock()
	s.calls[name]++
	s.mu.Unlock()
}

func (s *spyCreds) AddOAuth(ctx context.Context, sc contracts.Scope, g credentials.OAuthGrant) (providers.CredentialRef, error) {
	s.hit("AddOAuth")
	return s.Credentials.AddOAuth(ctx, sc, g)
}

func (s *spyCreds) Rotate(ctx context.Context, sc contracts.Scope, r providers.CredentialRefID, rot credentials.Rotation) (providers.CredentialRef, error) {
	s.hit("Rotate")
	return s.Credentials.Rotate(ctx, sc, r, rot)
}

func (s *spyCreds) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[name]
}

// txFailAudit records uses, but once broken refuses every change recorded inside a transaction: the lifecycle path
// of the credential service (Flag, Revoke) fails while a stored value can still be released.
type txFailAudit struct {
	audit.Appender
	broken atomic.Bool
}

func (a *txFailAudit) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	if a.broken.Load() {
		return audit.Event{}, errors.New("audit down")
	}
	return a.Appender.AppendTx(ctx, tx, loc, ev)
}

// breakAfterAdd breaks the audit of the credential service once a new connection is stored.
type breakAfterAdd struct {
	Credentials
	aud *txFailAudit
}

func (c breakAfterAdd) AddOAuth(ctx context.Context, sc contracts.Scope, g credentials.OAuthGrant) (providers.CredentialRef, error) {
	ref, err := c.Credentials.AddOAuth(ctx, sc, g)
	c.aud.broken.Store(true)
	return ref, err
}

type okValidator struct{}

func (okValidator) ValidateKey(context.Context, credentials.ValidationRequest) (credentials.ValidationResult, error) {
	return credentials.ValidationResult{Accepted: true}, nil
}

type rig struct {
	t      *testing.T
	clk    *clock.Fake
	idp    *idp
	db     *store.DB
	svc    *credentials.Service
	aud    *txFailAudit
	spy    *spyCreds
	eg     *egress.Registry
	eng    *Engine
	scope  contracts.Scope
	logbuf *bytes.Buffer
	cfg    ProviderConfig
}

// testProvider is the provider config of the mock identity provider.
func testProvider(p *idp) ProviderConfig {
	return ProviderConfig{
		ID: "mockidp", DisplayName: "Mock IdP", OAuthSupported: true,
		AuthorizationEndpoint: p.url() + "/authorize", TokenEndpoint: p.url() + "/token", RevocationEndpoint: p.url() + "/revoke",
		ClientID: p.clientID,
		Scopes: []ScopeInfo{
			{Name: "scope.chat", Description: "Send prompts and read replies", DataAccess: "the text you send to the model"},
			{Name: "scope.files", Description: "Read files you attach", DataAccess: "attached files"},
			{Name: "scope.admin", Description: "Manage the account", DataAccess: "everything"},
			{Name: "openid", Description: "Know which account this is", DataAccess: "an account identifier"},
		},
		Capabilities: map[string][]string{
			"chat":       {"scope.chat"},
			"files":      {"scope.files"},
			"chat_files": {"scope.chat", "scope.files"},
		},
		DataAccess:   []string{"Prompts you send are processed by Mock IdP and retained 30 days."},
		RedirectURIs: []string{registeredRedirect},
		Loopback:     true,
		Account:      AccountMapping{AccountID: "account_id", OrganizationID: "org_id"},
	}
}

func newRig(t *testing.T, mutate ...func(*rig, *ProviderConfig)) *rig {
	t.Helper()
	g := &rig{t: t, clk: clock.NewFake(tStart), logbuf: &bytes.Buffer{}, scope: scopeOf(tApp, tTenant)}
	g.idp = newIDP(t, g.clk)
	g.db = storetest.Open(t, nil)
	alog, err := audit.New(g.db, audit.Options{Clock: g.clk})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	enc, err := credentials.NewEncryptedFile(filepath.Join(root, "secrets", credentials.FileName),
		credentials.KeyFileSource{Path: filepath.Join(root, "keys", "master.key")})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(g.logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	g.aud = &txFailAudit{Appender: alog}
	g.svc, err = credentials.New(credentials.Config{DB: g.db, Audit: g.aud, DeploymentID: tDeploy, Clock: g.clk,
		Validator: okValidator{}, Backends: []credentials.Backend{enc}, Default: credentials.BackendEncryptedFile, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	g.spy = &spyCreds{Credentials: g.svc, calls: map[string]int{}}
	g.cfg = testProvider(g.idp)
	for _, m := range mutate {
		m(g, &g.cfg)
	}
	g.eg = egress.NewRegistry()
	g.allow(tApp)
	g.eng, err = New(Config{Credentials: g.spy, Egress: g.eg, Providers: []ProviderConfig{g.cfg}, Clock: g.clk, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// allow puts the mock identity provider on the egress allowlist of an application.
func (g *rig) allow(app string) {
	g.t.Helper()
	host, port := g.idp.hostPort()
	al, err := egress.NewAllowlist(egress.Destination{Host: host, Ports: []int{port}, Scheme: "http"})
	if err != nil {
		g.t.Fatal(err)
	}
	if err := g.eg.Set(app, al); err != nil {
		g.t.Fatal(err)
	}
}

// begin prepares and begins a flow for the rig's provider and returns the authorization.
func (g *rig) begin(req PrepareRequest, redirect string) Authorization {
	g.t.Helper()
	c, err := g.eng.Prepare(bg, g.scope, req)
	if err != nil {
		g.t.Fatalf("Prepare: %v", err)
	}
	a, err := g.eng.Begin(bg, g.scope, BeginRequest{PrepareRequest: req, ConsentDigest: c.Digest, RedirectURI: redirect, Label: "test"})
	if err != nil {
		g.t.Fatalf("Begin: %v", err)
	}
	return a
}

func (g *rig) req(caps ...string) PrepareRequest {
	return PrepareRequest{ProviderID: g.cfg.ID, Capabilities: caps}
}

// connect runs the whole flow against the mock identity provider and returns the connection.
func (g *rig) connect(caps ...string) Connection {
	g.t.Helper()
	a := g.begin(g.req(caps...), registeredRedirect)
	conn, err := g.eng.Complete(bg, g.scope, browse(g.t, a.URL, registeredRedirect))
	if err != nil {
		g.t.Fatalf("Complete: %v", err)
	}
	return conn
}

func (g *rig) token(ref providers.CredentialRefID) (credentials.Secret, error) {
	return g.eng.Token(bg, g.scope, TokenRequest{Ref: ref})
}

func mustReason(t *testing.T, err error, code contracts.ErrCode, reason Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s/%s, got no error", code, reason)
	}
	var ce *contracts.Error
	if !asContracts(err, &ce) || ce.Code != code || ReasonOf(err) != reason {
		t.Fatalf("want %s/%s, got %v (reason %q)", code, reason, err, ReasonOf(err))
	}
}

func asContracts(err error, target **contracts.Error) bool {
	for err != nil {
		if e, ok := err.(*contracts.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stateOf(t *testing.T, authURL string) string {
	t.Helper()
	i := strings.Index(authURL, "state=")
	if i < 0 {
		t.Fatal("no state in the authorization URL")
	}
	s := authURL[i+len("state="):]
	if j := strings.IndexByte(s, '&'); j >= 0 {
		s = s[:j]
	}
	return s
}
