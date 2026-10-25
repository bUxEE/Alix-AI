package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/diag"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/metrics"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/resources"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of M3-30 for the operations surface: serve feeds internal/diag, provides it to the
// /v1/ops area, and opens the operator's loopback door only when asked.

var opsMetricsAddrRE = regexp.MustCompile(`"msg":"metrics listener","addr":"([^"]+)"`)

// opsGet is one request to the operator's door.
func opsGet(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set(diag.OperatorTokenHeader, token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// With metrics.enabled serve opens a loopback listener whose /metrics shows the queue, the model
// roles and the governor and whose /diagnostics needs the operator credential; the deployment-wide
// data is on no public route. Without it nothing is opened.
func TestServeProvidesOpsServiceAndTheOperatorMetricsDoor(t *testing.T) {
	f := newFixture(t)
	key := testKey(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", key)
	// A provider that runs last sees what the ops provider left in the registry.
	var saw serveDeps
	registerServeProvider("zz_ops_probe", func(_ context.Context, d serveDeps) error { saw = d; return nil })
	t.Cleanup(func() {
		serveProvidersMu.Lock()
		delete(serveProviders, "zz_ops_probe")
		serveProvidersMu.Unlock()
	})
	cfg, dataDir := serveConfig(t, f, "metrics:\n  enabled: true\n  listen: 127.0.0.1:0\n")
	srv := startServe(t, cfg)

	var addr string
	pollUntil(t, 10*time.Second, "the metrics listener", func() bool {
		m := opsMetricsAddrRE.FindStringSubmatch(srv.stderr.String())
		if m != nil {
			addr = m[1]
		}
		return m != nil
	})
	door := "http://" + addr

	// The host door: /v1/ops answers from a service (it is 503 while none is provided), the same
	// one the operator door reads, built over the governor, the ring and the registries.
	if _, ok := gateway.Lookup[gateway.OpsService](saw.Services); !ok {
		t.Error("serve does not provide gateway.OpsService")
	}
	if _, ok := gateway.Lookup[*diag.Service](saw.Services); !ok {
		t.Error("serve does not provide the diag service")
	}
	if saw.Resources == nil || saw.ErrorRing == nil {
		t.Errorf("the providers were not given the governor and the error ring: %+v", saw)
	}

	// /metrics: Prometheus text of the queue (read once the RAG stack has built it), the roles
	// and the governor.
	pollUntil(t, 10*time.Second, "the queue and the governor in /metrics", func() bool {
		code, body := opsGet(t, door+"/metrics", "")
		return code == http.StatusOK && bytes.Contains(body, []byte("alixai_queue_jobs")) && bytes.Contains(body, []byte("alixai_resource_memory_budget_bytes"))
	})
	_, body := opsGet(t, door+"/metrics", "")
	for _, want := range []string{"alixai_provider_status", "alixai_resource_background_held", "alixai_resource_rss_bytes"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("/metrics has no %s", want)
		}
	}

	// /diagnostics: the operator credential is the admin interface's, nothing else works.
	ep, err := gateway.ReadAdminEndpoint(dataDir)
	if err != nil {
		t.Fatalf("the admin endpoint: %v", err)
	}
	if code, _ := opsGet(t, door+"/diagnostics", ""); code != http.StatusForbidden {
		t.Errorf("/diagnostics without the credential = %d", code)
	}
	if code, _ := opsGet(t, door+"/diagnostics", "not-the-operators-credential-but-long-enough"); code != http.StatusForbidden {
		t.Errorf("/diagnostics with a wrong credential = %d", code)
	}
	code, body := opsGet(t, door+"/diagnostics", ep.Token)
	if code != http.StatusOK {
		t.Fatalf("/diagnostics with the credential = %d: %s", code, body)
	}
	var bundle diag.Bundle
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatalf("the bundle: %v", err)
	}
	if bundle.Format != diag.BundleFormat {
		t.Errorf("format = %q", bundle.Format)
	}
	if bundle.Resources == nil || bundle.Resources.MemoryBudgetBytes == 0 {
		t.Errorf("the bundle has no resource usage: %+v", bundle.Resources)
	}
	if bundle.Queue == nil || len(bundle.Queue.Classes) != len(jobs.Classes()) {
		t.Errorf("the bundle has no queue: %+v", bundle.Queue)
	}
	if len(bundle.Config) == 0 {
		t.Error("the bundle has no configuration")
	}
	// Recent warnings are in it (the API has no identity configured and says so), and no secret is:
	// not the runtime's key, not the operator credential.
	if !strings.Contains(string(body), "identity.deployment_id is not configured") {
		t.Errorf("the bundle has not the warning serve logged: %v", bundle.RecentErrors)
	}
	for _, secret := range []string{key, ep.Token} {
		if strings.Contains(string(body), secret) {
			t.Error("the bundle carries a credential")
		}
	}

	// The deployment-wide view is on no public route.
	for _, p := range []string{"/metrics", "/diagnostics", "/v1/metrics", "/v1/diagnostics"} {
		if code, _, _ := srv.do(t, http.MethodGet, p, ""); code == http.StatusOK {
			t.Errorf("the public listener answers 200 on %s", p)
		}
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	// The listener went with serve.
	if _, err := (&http.Client{Timeout: time.Second}).Get(door + "/metrics"); err == nil {
		t.Error("the metrics listener outlived serve")
	}
}

// metrics.enabled is off by default: serve opens no listener beyond the public one and the
// admin interface.
func TestServeOpensNoMetricsListenerByDefault(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	cfg, _ := serveConfig(t, f, "")
	srv := startServe(t, cfg)
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if opsMetricsAddrRE.MatchString(srv.stderr.String()) {
		t.Errorf("a metrics listener was opened with metrics.enabled unset:\n%s", srv.stderr.String())
	}
}

// The /v1/ops area is provided by wireV1: the host door answers from the queue the RAG stack built
// (its own scope's counts, behind an exact capability), and the same service is what the operator
// door reads, deployment-wide.
func TestOpsServiceIsProvidedAndReadsTheQueue(t *testing.T) {
	ctx := context.Background()
	clk := clock.Real{}
	db := storetest.Open(t, nil)
	w, err := wireV1(ctx, wireInput{
		log: discardLog(), clk: clk, db: db, dataDir: t.TempDir(), providers: providers.NewRegistry(clk),
		off: &inferenceOff{reason: "test", err: errors.New("test")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.adapters.close)

	ops, ok := gateway.Lookup[gateway.OpsService](w.services)
	if !ok {
		t.Fatal("serve does not provide gateway.OpsService: /v1/ops would answer 503")
	}
	svc, ok := gateway.Lookup[*diag.Service](w.services)
	if !ok {
		t.Fatal("serve does not provide the diag service the operator door reads")
	}
	if w.resources == nil || w.resources.jobQueue() == nil {
		t.Fatal("the queue the RAG stack built did not reach the operations surface")
	}

	scope := contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: "app_test", TenantID: "ten_test", WorkspaceID: "wsp_test",
		PrincipalID: "usr_test", Roles: []string{"member"}, SessionID: "ses_test",
		CapabilityScopes: []string{diag.CapabilityQueueRead},
	}
	rep, err := ops.Queue(ctx, scope)
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	if len(rep.Classes) != len(jobs.Classes()) {
		t.Errorf("the queue report has %d classes", len(rep.Classes))
	}
	denied := scope
	denied.CapabilityScopes = []string{"cap.read"}
	if _, err := ops.Queue(ctx, denied); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("a token without %s reads the queue: %v", diag.CapabilityQueueRead, err)
	}

	b, err := svc.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b.Queue == nil || b.Resources == nil {
		t.Errorf("bundle queue = %v, resources = %v", b.Queue, b.Resources)
	}
	for _, p := range b.Problems {
		if strings.HasPrefix(p, "queue") || strings.HasPrefix(p, "resources") {
			t.Errorf("the bundle reports %q", p)
		}
	}
}

// The operator's log.redact_keys hide those keys' values in the bundle's configuration
// snapshot too, not only in the log.
func TestOpsBundleAppliesTheOperatorRedactKeys(t *testing.T) {
	reg := config.NewRegistry()
	if err := reg.LoadFrom(writeConfig(t, "log:\n  redact_keys: [listen]\n"), func(string) (string, bool) { return "", false }); err != nil {
		t.Fatal(err)
	}
	services := gateway.NewServices()
	if err := opsServeProvide(context.Background(), serveDeps{Clock: clock.Real{}, Config: reg, Services: services}); err != nil {
		t.Fatal(err)
	}
	svc, ok := gateway.Lookup[*diag.Service](services)
	if !ok {
		t.Fatal("no diag service")
	}
	b, err := svc.Bundle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m, _ := b.Config["metrics"].(map[string]any)
	if m == nil || m["Listen"] != "[REDACTED]" {
		t.Errorf("the bundle's metrics section = %v, want its listen address redacted", b.Config["metrics"])
	}
}

// The governor's Stats become the plain numbers diag reports, one for one.
func TestOpsResourceUsageAdaptsTheGovernorStats(t *testing.T) {
	w := resourcesTestWiring(t, clock.Real{})
	w.Gov.Refresh(context.Background())
	end := w.Gov.BeginInteractive(jobs.ClassTurn)
	defer end()
	if _, err := w.Gov.Admit(context.Background(), opsResourcesRequest(jobs.ClassTurn, 1<<20)); err != nil {
		t.Fatal(err)
	}

	got := opsResourceUsage(w.Gov)()
	st := w.Gov.Stats()
	if got.MemoryBudgetBytes != st.BudgetBytes || got.RSSBytes != st.RSSBytes || got.ReservedBytes != 1<<20 ||
		got.InteractiveActive != 1 || !got.BackgroundHeld || got.HoldReason != "interactive_active" || got.Admitted != 1 {
		t.Errorf("usage = %+v, stats = %+v", got, st)
	}
}

// The ring sees every warning and error of the logger serve builds, redacted, and the logger still
// writes every record where it did.
func TestOpsWrapLoggerFeedsTheErrorRing(t *testing.T) {
	var out bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&out, nil))
	log, ring := opsWrapLogger(base, nil)
	log.Info("started")
	log.Warn("the disk is slow", slog.String("path", "/data"), slog.String("api_key", "TESTONLY-secret-value"))
	log.Error("it broke")

	got := ring.Recent()
	if len(got) != 2 || got[0].Message != "the disk is slow" || got[1].Level != "ERROR" {
		t.Fatalf("recent = %+v, want the warning and the error and not the info", got)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "TESTONLY-secret-value") {
		t.Errorf("the ring holds a secret: %s", b)
	}
	for _, want := range []string{"started", "the disk is slow", "it broke"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the original handler did not get %q", want)
		}
	}
}

// With metrics disabled nothing is opened, whatever else is given; enabled with a non-loopback
// address it is refused, not served.
func TestOpsStartMetricsRefusesWhatIsNotLoopback(t *testing.T) {
	ctx := context.Background()
	stop, err := opsStartMetrics(ctx, discardLog(), opsMetricsConfig(false, "0.0.0.0:9464"), nil, "")
	if err != nil {
		t.Fatalf("disabled: %v", err)
	}
	stop()

	services := gateway.NewServices()
	svc, err := diag.New(diag.Sources{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.Provide(services, svc); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 32)
	if _, err := opsStartMetrics(ctx, discardLog(), opsMetricsConfig(true, "0.0.0.0:9464"), services, token); err == nil {
		t.Error("a wildcard address was served")
	}
	if _, err := opsStartMetrics(ctx, discardLog(), opsMetricsConfig(true, "127.0.0.1:0"), services, "short"); err == nil {
		t.Error("a credential that is too short was accepted")
	}
	if _, err := opsStartMetrics(ctx, discardLog(), opsMetricsConfig(true, "127.0.0.1:0"), gateway.NewServices(), token); err == nil {
		t.Error("metrics were served with no operations service")
	}
}

func opsMetricsConfig(enabled bool, listen string) metrics.Config {
	return metrics.Config{Enabled: enabled, Listen: listen}
}

func opsResourcesRequest(class jobs.Class, bytes uint64) resources.Request {
	return resources.Request{Class: class, Bytes: bytes, Label: "test"}
}
