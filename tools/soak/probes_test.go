package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

// What internal/diag's operator door prints (the names and labels are the ones
// of internal/diag/collectors.go).
const metricsText = `# HELP alixai_queue_jobs Jobs per priority class and state.
# TYPE alixai_queue_jobs gauge
alixai_queue_jobs{class="turn",state="pending"} 1
alixai_queue_jobs{class="turn",state="running"} 1
alixai_queue_jobs{class="ingest",state="pending"} 4
alixai_queue_jobs{class="ingest",state="running"} 1
alixai_queue_jobs{class="ingest",state="succeeded"} 90
alixai_provider_configured{role="controller"} 1
alixai_provider_configured{role="router"} 0
alixai_provider_configured{role="embedder"} 1
alixai_provider_status{role="controller",status="ok"} 1
alixai_provider_status{role="controller",status="degraded"} 0
alixai_provider_status{role="controller",status="unavailable"} 0
alixai_provider_status{role="router",status="ok"} 0
alixai_provider_status{role="router",status="degraded"} 0
alixai_provider_status{role="router",status="unavailable"} 1
alixai_provider_status{role="embedder",status="ok"} 0
alixai_provider_status{role="embedder",status="degraded"} 1
alixai_provider_status{role="embedder",status="unavailable"} 0
alixai_resource_model_unloads_total 7
alixai_resource_model_reloads_total 2
alixai_resource_rss_bytes 123
`

func TestParseMetrics(t *testing.T) {
	st, err := parseMetrics(metricsText)
	if err != nil {
		t.Fatal(err)
	}
	want := StackStats{
		Roles: map[string]RoleState{
			"controller": {true, "ok"}, "router": {false, "unavailable"}, "embedder": {true, "degraded"},
		},
		QueueKnown: true, QueuePending: 5, QueueRunning: 2, QueueIngestPending: 4,
		ReloadsKnown: true, Reloads: 2, Unloads: 7,
	}
	if diff := cmp.Diff(want, st); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	// a group that is absent stays unknown, it is not zero
	st, err = parseMetrics("alixai_queue_jobs{class=\"turn\",state=\"pending\"} 3\n")
	if err != nil || st.Roles != nil || st.ReloadsKnown || !st.QueueKnown || st.QueuePending != 3 {
		t.Errorf("%+v, %v", st, err)
	}
	// a role with no series at 1 is not ok
	st, _ = parseMetrics("alixai_provider_configured{role=\"controller\"} 1\n")
	if st.Roles["controller"].Status != "unavailable" {
		t.Errorf("role without a status: %+v", st.Roles)
	}
	// text that is not metrics is an error
	for _, bad := range []string{"", "<html>hello</html>", "# only a comment\n"} {
		if _, err := parseMetrics(bad); err == nil {
			t.Errorf("parseMetrics(%q) did not fail", bad)
		}
	}
}

func TestParseQueueReport(t *testing.T) {
	st, err := parseQueueReport([]byte(`{"at":"2026-10-03T10:00:00Z","classes":[
		{"class":"turn","priority":2,"workers":1,"max_depth":8,"pending":1,"running":1,"succeeded":5,"failed":0,"cancelled":0},
		{"class":"ingest","priority":5,"workers":1,"max_depth":64,"pending":7,"running":1,"succeeded":0,"failed":2,"cancelled":0}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !st.QueueKnown || st.QueuePending != 8 || st.QueueRunning != 2 || st.QueueIngestPending != 7 || st.ReloadsKnown || st.Roles != nil {
		t.Errorf("%+v", st)
	}
	for _, bad := range []string{`{}`, `{"classes":[]}`, `nope`} {
		if _, err := parseQueueReport([]byte(bad)); err == nil {
			t.Errorf("parseQueueReport(%q) did not fail", bad)
		}
	}
}

func TestCheckTargetURL(t *testing.T) {
	good := []struct {
		url      string
		loopback bool
	}{
		{"http://127.0.0.1:7470", true}, {"http://localhost:9470/metrics", true}, {"http://[::1]:7470", true},
		{"https://alixai.example.com", false}, {"https://127.0.0.1:7470", true},
	}
	for _, c := range good {
		if _, err := checkTargetURL(c.url, c.loopback); err != nil {
			t.Errorf("%s refused: %v", c.url, err)
		}
	}
	bad := []struct {
		url      string
		loopback bool
	}{
		{"http://example.com", false}, {"http://192.168.1.5:7470", false}, {"https://example.com/metrics", true},
		{"ftp://127.0.0.1", false}, {"http://user:pw@127.0.0.1", false}, {"http://127.0.0.1?x=1", false}, {"127.0.0.1:7470", false}, {"", false},
	}
	for _, c := range bad {
		if _, err := checkTargetURL(c.url, c.loopback); err == nil {
			t.Errorf("%s was accepted", c.url)
		}
	}
}

// httpProbeServer serves the paths of a stack's two doors and records the
// Authorization header it saw.
func httpProbeServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/ops/queue":
			_, _ = w.Write([]byte(`{"at":"2026-10-03T10:00:00Z","classes":[{"class":"ingest","pending":3,"running":1}]}`))
		case "/metrics":
			_, _ = w.Write([]byte(metricsText))
		case "/moved":
			http.Redirect(w, r, "/metrics", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

func TestOpsQueueProbe(t *testing.T) {
	srv, auth := httpProbeServer(t)
	minted := 0
	p, err := newOpsQueueProbe(srv.URL, func(context.Context) (string, error) { minted++; return "tok" + string(rune('0'+minted)), nil })
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Stats(context.Background())
	if err != nil || !st.QueueKnown || st.QueuePending != 3 || st.QueueIngestPending != 3 {
		t.Fatalf("%+v, %v", st, err)
	}
	if _, err := p.Stats(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a fresh single-use token for every request, sent as a bearer credential only
	if diff := cmp.Diff([]string{"Bearer tok1", "Bearer tok2"}, *auth); diff != "" {
		t.Errorf("authorization headers (-want +got):\n%s", diff)
	}
	// a route the stack does not serve is an error, never an empty queue
	p.url = strings.TrimSuffix(p.url, "/queue") + "/nothing"
	if _, err := p.Stats(context.Background()); err == nil {
		t.Error("a 404 was read as a queue")
	}
}

func TestMetricsProbe(t *testing.T) {
	srv, auth := httpProbeServer(t)
	p, err := newMetricsProbe(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Stats(context.Background())
	if err != nil || !st.ReloadsKnown || st.Reloads != 2 || st.Roles["controller"].Status != "ok" {
		t.Fatalf("%+v, %v", st, err)
	}
	if len(*auth) != 1 || (*auth)[0] != "" {
		t.Errorf("the metrics door got credentials: %v", *auth)
	}
	// a redirect is never followed
	p.url = srv.URL + "/moved"
	if _, err := p.Stats(context.Background()); err == nil {
		t.Error("a redirect was followed")
	}
	if _, err := newMetricsProbe("http://example.com/metrics"); err == nil {
		t.Error("a non-loopback metrics URL was accepted")
	}
}

// readyz is read through the SDK, from the fake API.
func TestReadyzProbe(t *testing.T) {
	f := sdktest.New()
	defer f.Close()
	c, err := f.Client(sdktest.Claims{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := readyzProbe{c: c}.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Roles == nil || st.QueueKnown || st.ReloadsKnown {
		t.Errorf("%+v: readyz knows the roles and nothing else", st)
	}
	if got := (readyzOpsProbe{}).Kind(); got != "readyz+ops" {
		t.Errorf("kind %q", got)
	}
}
