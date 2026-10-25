package localai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The scorer satisfies the engine's provider contract.
var _ decision.DecisionModelProvider = (*Provider)(nil)

// seen is one request a test server received.
type seen struct {
	Method, Path, Auth, ContentType string
	Body                            []byte
}

// fakeServer is a plain-HTTP loopback server (the recorded-fixture rig: its answers
// are constructed from the documentation, not captured from a LocalAI).
type fakeServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seen
}

func (f *fakeServer) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.reqs...)
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), b})
		f.mu.Unlock()
		h(w, r, b)
	}))
	t.Cleanup(f.Close)
	return f
}

// answer replies 200 with a JSON body.
func answer(body string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func status(code int, body string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
}

func testConfig(url string) Config {
	return Config{Endpoint: url, Model: "clm-8b", ModelRevision: "rev1", RequestTimeout: 2 * time.Second}
}

func newProvider(t *testing.T, cfg Config, opts ...Option) *Provider {
	t.Helper()
	p, err := New(cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep1", ApplicationID: "app1", TenantID: "ten1", WorkspaceID: "ws1", PrincipalID: "pri1",
		Roles: []string{"staff"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses1",
	}
}

func strp(s string) *string { return &s }

// testRequest is a request with n candidates c1..cn.
func testRequest(n int) gen.DecisionRequest {
	cands := make([]gen.CandidateSetCandidate, n)
	for i := range cands {
		cands[i] = gen.CandidateSetCandidate{CandidateID: common.Identifier(fmt.Sprintf("c%d", i+1)), Label: fmt.Sprintf("Option %d", i+1)}
	}
	cands[0].Text = strp("the first one")
	return gen.DecisionRequest{
		TenantID: "ten1", ApplicationID: "app1", SessionID: "ses1", ContractVersion: "cv1",
		DecisionType: gen.DecisionTypeItemSelection, SchemaVersion: 1,
		BoundedState:       map[string]gen.BoundedScalar{"step": "pick", "n": 3},
		AllowedNextActions: []actions.PermittedNextAction{},
		CandidateSet:       gen.CandidateSet{Candidates: cands, MaxSelections: 1, BuiltAt: "2026-10-07T10:00:00Z"},
		Locale:             "en", Deadline: "2026-10-07T10:00:01Z",
	}
}

const goodAnswer = `{"answers":{"choice":{"type":"choice","choice":"c2","probabilities":{"c1":0.03,"c2":0.96,"c3":0.01},"confidence":0.94}},"usage":{"prompt_tokens":40,"completion_tokens":1},"latency_ms":31.5}`

func score(t *testing.T, p *Provider, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.ScoreDecision(ctx, testScope(), req)
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
