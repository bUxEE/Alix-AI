package clm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

type seen struct {
	Method, Path, ContentType string
	Body                      []byte
}

// fakeServer is a plain-HTTP loopback server. Its answers are constructed from the
// documentation examples (README of Contrastive-LM/CLM, read 2026-10-07), not
// captured from a running service.
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

func (f *fakeServer) count(path string) int {
	n := 0
	for _, r := range f.requests() {
		if r.Path == path {
			n++
		}
	}
	return n
}

// reply is one canned answer.
type reply struct {
	code int
	body string
	// hang makes the handler wait for the client to give up.
	hang bool
}

func ok(body string) reply { return reply{code: 200, body: body} }

// newServer serves the replies by "METHOD /path"; any other route is a 404.
func newServer(t *testing.T, replies map[string]reply) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, seen{r.Method, r.URL.Path, r.Header.Get("Content-Type"), b})
		f.mu.Unlock()
		rp, found := replies[r.Method+" "+r.URL.Path]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"detail":"Not Found"}`)
			return
		}
		if rp.hang {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			return
		}
		w.WriteHeader(rp.code)
		fmt.Fprint(w, rp.body)
	}))
	t.Cleanup(f.Close)
	return f
}

const (
	modelsOK = `{"data":[{"id":"clm-latest","revision":"rev1","description":"CLM v0.1 8B","release_date":"2026-09-01"},{"id":"other","revision":"x"}]}`
	rankOK   = `{"model":"clm-latest","ranked":[{"rank":1,"candidate":"Option 2","prob":0.97},{"rank":2,"candidate":"Option 1: the first one","prob":0.02},{"rank":3,"candidate":"Option 3","prob":0.01}]}`
	systemOK = `{"answers":{"choice":{"choice":"c2","probabilities":{"c1":0.02,"c2":0.97,"c3":0.01},"confidence":0.93}},"usage":{"input_tokens":38,"billing_units":3},"latency_ms":58.1}`
	healthOK = `{"cache":{"entries":0,"hit_rate":0}}`
)

// standard is a healthy service.
func standard() map[string]reply {
	return map[string]reply{
		"GET /v1/models":     ok(modelsOK),
		"GET /health":        ok(healthOK),
		"POST /v1/rank":      ok(rankOK),
		"POST /v1/systemone": ok(systemOK),
	}
}

func with(base map[string]reply, key string, r reply) map[string]reply {
	out := map[string]reply{}
	for k, v := range base {
		out[k] = v
	}
	out[key] = r
	return out
}

func testConfig(url string) Config {
	return Config{Endpoint: url, Model: "clm-latest", ModelRevision: "rev1", RequestTimeout: 2 * time.Second}
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

func score(t *testing.T, p *Provider, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.ScoreDecision(ctx, testScope(), req)
}
