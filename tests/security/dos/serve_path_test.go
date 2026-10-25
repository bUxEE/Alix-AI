package dossec

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/cli"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// SEC-077 on the wired path: the same claim as the package test, made against `alixai serve` itself. A real
// service over a live application, real context tokens through the identity middleware, and the quotas read
// from the configuration file. One tenant floods the request rate and hoards sessions; the other tenant of the
// application still has its whole allowance.

const (
	servePathApp      = "demo-library"
	servePathPackage  = "../../../examples/packages/valid-minimal"
	servePathIssuer   = "https://library.example.test"
	servePathKeyID    = "demo-library-ctx-2026"
	servePathAudience = "alixai"
)

type servePathBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *servePathBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *servePathBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func servePathEnv(out, errb *servePathBuf) *cli.Env {
	return &cli.Env{Stdin: strings.NewReader(""), Stdout: out, Stderr: errb, LookupEnv: func(string) (string, bool) { return "", false }}
}

// servePathSeed puts a live application into the store serve will open: the publisher's and the platform's keys,
// the demo package audited, its warnings accepted and activated.
func servePathSeed(t *testing.T, dataDir string, platformPub ed25519.PublicKey, platform trust.SigningKey) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dataDir, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clk := clock.Real{}
	ts := trust.NewStore(db, clk, trust.AllowTestOnlyKeys())
	if _, err := ts.RegisterKey(ctx, servePathApp, devkeys.KeyID, devkeys.PublicKey(), "test publisher"); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.RegisterKey(ctx, trust.PlatformApplicationID, platform.KeyID, platformPub, "audit report signer"); err != nil {
		t.Fatal(err)
	}
	verifier := trust.NewVerifier(ts, clk)
	core := apppkg.CoreVersionOrDev(version.Get().Version)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: dataDir, Verifier: verifier, CoreVersion: core})
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.New(db, audit.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := integrationaudit.NewRunner(integrationaudit.Config{
		DB: db, Clock: clk, Installer: inst, DataDir: dataDir, Verifier: verifier, CoreVersion: core,
		Signing: &platform, Log: al, Authorizer: integrationaudit.AllowOperators{"alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := inst.Install(ctx, apppkg.FromDir(servePathPackage))
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.Audit(ctx, integrationaudit.RunRequest{ApplicationID: servePathApp, ManifestHash: res.Record.ManifestHash, OperatorID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range run.Signed.Report.Findings {
		if f.Severity != contracts.SeverityWarning {
			continue
		}
		if _, err := runner.AcceptWarning(ctx, integrationaudit.AcceptRequest{
			ApplicationID: servePathApp, ManifestHash: res.Record.ManifestHash, FindingKey: f.Key, OperatorID: "alice", Reason: "reviewed in the test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runner.Activate(ctx, integrationaudit.ActivateRequest{ApplicationID: servePathApp, ManifestHash: res.Record.ManifestHash, OperatorID: "alice"}); err != nil {
		t.Fatal(err)
	}
}

var servePathListening = regexp.MustCompile(`listening on (http://\S+)`)

func servePathStarvation(t *testing.T) {
	// The audit runs the built-in checks only: this build has no adapter for the demo application.
	reg := integrationaudit.NewRegistry()
	for _, c := range integrationaudit.Builtins() {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	previous := integrationaudit.Default
	integrationaudit.Default = reg
	t.Cleanup(func() { integrationaudit.Default = previous })

	dir := t.TempDir()
	data, cfgPath, keyFile := filepath.Join(dir, "data"), filepath.Join(dir, "alixai.yaml"), filepath.Join(dir, "localai-api-key")
	if err := os.WriteFile(keyFile, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`data_dir: %q
server:
  listen: 127.0.0.1:0
localai:
  endpoint: http://127.0.0.1:1
  api_key_ref: %q
inference:
  models: []
identity:
  deployment_id: dep_dos
  audience: %s
quotas:
  defaults:
    requests_per_minute: 60
    burst: 10
    concurrent_sessions: 2
    storage_bytes: 1000
    ingest_bytes_per_day: 150
files:
  ephemeral_context:
    enabled: true
    allowed_mime: [text/plain]
  rag_source:
    enabled: true
    allowed_mime: [text/plain]
`, filepath.ToSlash(data), "file:"+filepath.ToSlash(keyFile), servePathAudience)
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	iss := identitytest.NewIssuer(t, identitytest.IssuerConfig{
		ApplicationID: servePathApp, Issuer: servePathIssuer, KeyID: servePathKeyID, Audience: servePathAudience, Clock: clock.Real{},
	})
	var out, errb servePathBuf
	if code := cli.Run(context.Background(), []string{"identity", "add-issuer-key", "-config", cfgPath, "-package", servePathPackage,
		"-key-id", iss.KeyID, "-public-key", trust.EncodePublicKey(iss.Public)}, servePathEnv(&out, &errb)); code != cli.ExitOK {
		t.Fatalf("identity add-issuer-key = %d\n%s\n%s", code, out.String(), errb.String())
	}
	pub, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	servePathSeed(t, data, pub, trust.SigningKey{KeyID: "dos-signer", Key: priv})

	var sout, serr servePathBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- cli.Run(ctx, []string{"serve", "-config", cfgPath}, servePathEnv(&sout, &serr)) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("serve did not stop")
			}
		})
	}
	t.Cleanup(stop)
	var base string
	for deadline := time.Now().Add(30 * time.Second); base == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := servePathListening.FindStringSubmatch(sout.String()); m != nil {
			base = m[1]
		}
		select {
		case code := <-done:
			t.Fatalf("serve exited with %d before it listened\n%s", code, serr.String())
		default:
		}
	}
	if base == "" {
		t.Fatalf("serve did not listen\n%s", serr.String())
	}

	call := func(tenant, session, method, path string) (int, contracts.Error) {
		t.Helper()
		token := iss.Token(func(d *identitytest.Draft) {
			d.Set(identity.ClaimTenant, tenant)
			d.Set(identity.ClaimSession, session)
		})
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var e contracts.Error
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, e
	}
	const flooder, victim = "ten_flooder", "ten_victim"

	// Requests per minute: the flood is refused at the flooder's burst, with the typed body.
	admitted := 0
	for i := 0; i < 60; i++ {
		// Admitted means the quota let it through: what the handler answers is its own business (this build has
		// no adapter for the demo application, so /v1/health reports it not ready).
		code, e := call(flooder, "ses_f", http.MethodGet, "/v1/health")
		if code != http.StatusTooManyRequests {
			admitted++
			continue
		}
		if e.Code != contracts.ErrRateLimited || e.Details["retry_after_ms"] == nil {
			t.Fatalf("a flood request was refused with %d %+v, want 429 rate_limited with retry_after_ms", code, e)
		}
		break
	}
	if admitted < 10 || admitted > 12 {
		t.Fatalf("the flooder was admitted %d requests, want its burst of 10", admitted)
	}
	// The victim has its burst, in the same minute: eight requests here, two sessions below.
	for i := 0; i < 8; i++ {
		if code, e := call(victim, "ses_v", http.MethodGet, "/v1/health"); code == http.StatusTooManyRequests {
			t.Fatalf("the victim's request %d was refused with %d %+v during the flood", i, code, e)
		}
	}

	// Concurrent sessions: a tenant that hoards them is refused at its own limit; another tenant opens its own.
	const hoarder = "ten_hoarder"
	for _, s := range []string{"ses_1", "ses_2"} {
		if code, e := call(hoarder, s, http.MethodPost, "/v1/sessions"); code != http.StatusOK {
			t.Fatalf("session %s of the hoarder = %d %+v", s, code, e)
		}
	}
	code, e := call(hoarder, "ses_3", http.MethodPost, "/v1/sessions")
	if code != http.StatusServiceUnavailable || e.Code != contracts.ErrResourceExhausted || e.Details["dimension"] != "concurrent_sessions" || e.Details["limit"] != float64(2) {
		t.Fatalf("a third session = %d %+v, want resource_exhausted for concurrent_sessions with limit 2", code, e)
	}
	for _, s := range []string{"ses_1", "ses_2"} {
		if code, e := call(victim, s, http.MethodPost, "/v1/sessions"); code != http.StatusOK {
			t.Fatalf("session %s of the victim = %d %+v while another tenant hoarded", s, code, e)
		}
	}
	// Storage and ingestion, on the same path (M9-01): charged by the files service in the transaction that stores
	// the file. A real multipart upload through the identity middleware and the quota wrapper.
	upload := func(tenant, treatment string, n int) (int, contracts.Error) {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if treatment != "" {
			if err := mw.WriteField("treatment", treatment); err != nil {
				t.Fatal(err)
			}
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="notes.txt"`)
		h.Set("Content-Type", "text/plain")
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(bytes.Repeat([]byte("a"), n))
		_ = mw.Close()
		token := iss.Token(func(d *identitytest.Draft) {
			d.Set(identity.ClaimTenant, tenant)
			d.Set(identity.ClaimSession, "ses_up")
			d.Set(identity.ClaimCapabilityScopes, []string{"settings.user.read"}) // the attachment policy is read as the caller
		})
		req, err := http.NewRequest(http.MethodPost, base+"/v1/files", &body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var e contracts.Error
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, e
	}
	const filler, source, bystander = "ten_filler", "ten_source", "ten_bystander"
	// Attachments are off until the deployment's settings document of the application allows them.
	{
		token := iss.Token(func(d *identitytest.Draft) {
			d.Set(identity.ClaimTenant, "ten_admin")
			d.Set(identity.ClaimSession, "ses_admin")
			d.Set(identity.ClaimCapabilityScopes, []string{"settings.deployment.write"})
		})
		req, err := http.NewRequest(http.MethodPut, base+"/v1/settings/deployment",
			strings.NewReader(`{"expected_version":0,"settings":{"attachments":{"allowed":true,"max_file_mb":1}}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("allowing attachments for the application = %d %s", resp.StatusCode, raw)
		}
	}

	// A tenant that fills its storage is refused with the typed 507 and loses nothing it stored; another tenant, in
	// the same application, still has its own storage.
	for i := 0; i < 2; i++ {
		if code, e := upload(filler, "ephemeral_context", 300); code != http.StatusCreated {
			t.Fatalf("upload %d of the filler = %d %+v", i, code, e)
		}
	}
	code, e = upload(filler, "ephemeral_context", 300)
	if code != http.StatusInsufficientStorage || e.Code != contracts.ErrResourceExhausted || e.Details["dimension"] != "storage_bytes" || e.Details["limit"] != float64(1000) {
		t.Fatalf("an upload past the storage quota = %d %+v, want 507 resource_exhausted for storage_bytes with limit 1000", code, e)
	}
	if code, e := upload(bystander, "ephemeral_context", 300); code != http.StatusCreated {
		t.Fatalf("the bystander's upload = %d %+v while another tenant filled its storage", code, e)
	}
	// The commit path is still reached by the tenant whose storage is full.
	if code, e := call(filler, "ses_up", http.MethodPost, "/v1/actions/prep_01J0000000000000000000A001/confirm"); code == http.StatusTooManyRequests || e.Code == contracts.ErrRateLimited || e.Code == contracts.ErrResourceExhausted {
		t.Fatalf("a confirmation was refused for quota with storage full: %d %+v", code, e)
	}

	// Ingestion: an uploaded source is refused by this build's RAG seam after its transaction committed; the file is
	// withdrawn and its charges given back, so repeating the request cannot use up the storage or the day of the
	// tenant (a reservation that leaked would have blocked the attachments below by the third attempt).
	for i := 0; i < 5; i++ {
		code, e := upload(source, "rag_source", 150)
		if code != http.StatusServiceUnavailable || e.Code != contracts.ErrUnavailable {
			t.Fatalf("source upload %d = %d %+v, want 503 unavailable", i, code, e)
		}
	}
	for i := 0; i < 2; i++ {
		if code, e := upload(source, "ephemeral_context", 300); code != http.StatusCreated {
			t.Fatalf("attachment %d after refused sources = %d %+v", i, code, e)
		}
	}

	// What serve recorded, read from the store after it stopped: the bytes of the files that exist, and no
	// ingestion for a source that was never registered.
	stop()
	db, err := store.Open(context.Background(), store.Options{Path: filepath.Join(data, store.DefaultFileName)})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	counter := func(tenant, dim string) int64 {
		var n int64
		switch err := db.QueryRowContext(context.Background(),
			`SELECT used FROM quota_counters WHERE application_id = ? AND tenant_id = ? AND dimension = ?`, servePathApp, tenant, dim).Scan(&n); {
		case err == sql.ErrNoRows:
			return 0
		case err != nil:
			t.Fatal(err)
		}
		return n
	}
	for _, c := range []struct {
		tenant, dim string
		want        int64
	}{
		{filler, "storage_bytes", 600}, {filler, "ingest_bytes_per_day", 0},
		{source, "storage_bytes", 600}, {source, "ingest_bytes_per_day", 0},
		{bystander, "storage_bytes", 300},
	} {
		if got := counter(c.tenant, c.dim); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.tenant, c.dim, got, c.want)
		}
	}
}
