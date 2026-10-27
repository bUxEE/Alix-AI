//go:build !noexternal

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/analyst/web"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// Serve-composition proofs for ADR 0047 wiring (follow-up 1). httptest SearXNG + the
// real web adapters used by cmd_analyst; full multi-turn HTTP against `alixai serve`
// is covered alongside these once a scripted controller package is installed.

type recordingSearx struct {
	srv     *httptest.Server
	mu      sync.Mutex
	queries []string
}

func newRecordingSearx(t *testing.T, hits []map[string]string) *recordingSearx {
	t.Helper()
	s := &recordingSearx{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		s.mu.Lock()
		s.queries = append(s.queries, q)
		s.mu.Unlock()
		results := make([]map[string]string, 0, len(hits))
		for _, h := range hits {
			results = append(results, map[string]string{
				"url": h["url"], "title": h["title"], "content": h["snippet"],
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *recordingSearx) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

func TestServeWebAdaptersRefuseConfidentialAndHonourPolicy(t *testing.T) {
	sx := newRecordingSearx(t, []map[string]string{{
		"url": "https://gov.it/xx", "title": "XX", "snippet": "eligibility",
	}})
	origin := sx.srv.URL
	p := an.ConfigWebConfigProviderSearxng
	cfg := &an.ConfigWebConfig{Provider: &p, Origin: &origin}
	prov, err := web.NewProvider(cfg, analystDynamicSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	bag := &webTurnBag{deploy: web.HostPolicy{Allow: []string{"gov.it"}}, budget: web.Budget{MaxSearches: 3, MaxFetches: 3}}
	searcher := webSearcherAdapt{inner: web.NewSearcher(prov), turns: bag, allows: func(context.Context, contracts.Scope) bool { return true }}
	scope := contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws",
		PrincipalID: "usr", SessionID: "ses", Roles: []string{"member"},
	}
	ds := []an.Dataset{{
		Fields: []an.DatasetDatasetField{{Name: "revenue", Type: an.FieldTypeNumber, DataClass: an.DataClassConfidential}},
		Rows:   []an.DatasetRow{{"revenue": float64(999888)}},
	}}
	// (c) confidential revenue embedded → refused before SearXNG is called.
	_, err = searcher.Search(context.Background(), scope, loop.SearchInput{
		Query: "revenue is 999888 please search", TurnID: "trn_conf", Datasets: ds,
	})
	if contracts.CodeOf(err) != contracts.ErrWebQueryRefused {
		t.Fatalf("confidential in query: %v", err)
	}
	if len(sx.recorded()) != 0 {
		t.Fatalf("searx must not see confidential query: %v", sx.recorded())
	}

	_, err = searcher.Search(context.Background(), scope, loop.SearchInput{
		Query: "Italian 2026 company discount XX", TurnID: "trn_ok", Datasets: ds,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sx.recorded(); len(got) != 1 || !strings.Contains(got[0], "Italian") {
		t.Fatalf("recorded = %v", got)
	}

	// Policy deny → typed refusal, no further queries.
	denied := webSearcherAdapt{inner: web.NewSearcher(prov), turns: bag, allows: func(context.Context, contracts.Scope) bool { return false }}
	_, err = denied.Search(context.Background(), scope, loop.SearchInput{
		Query: "another ok query", TurnID: "trn_deny", Datasets: nil,
	})
	if contracts.CodeOf(err) != contracts.ErrWebQueryRefused {
		t.Fatalf("policy deny: %v", err)
	}
	if len(sx.recorded()) != 1 {
		t.Fatalf("denied search must not hit searx: %v", sx.recorded())
	}
}

func TestServeWebMisconfiguredBraveRefusesProvider(t *testing.T) {
	p := an.ConfigWebConfigProviderBrave
	cfg := &an.ConfigWebConfig{Provider: &p}
	_, err := web.NewProvider(cfg, analystDynamicSecrets{})
	if err == nil || !strings.Contains(err.Error(), "cred_ref") {
		t.Fatalf("want cred_ref refusal, got %v", err)
	}
}

func TestBuildAnalystRunnerRefusesBadWebConfig(t *testing.T) {
	cfg := dialogue.AnalystConfig{
		Enabled: true,
		Abilities: dialogue.AnalystAbilityFlags{
			Loop: true, WebSearch: true, WebFetch: true,
		},
		Web: dialogue.AnalystWebConfig{Provider: "brave"}, // no cred_ref
	}
	_, _, err := buildAnalystRunner(dialogue.Deps{Actions: nil, Providers: nil}, cfg)
	if err == nil {
		t.Fatal("expected dialogue.analyst.web refusal")
	}
	if !strings.Contains(err.Error(), "web") && !strings.Contains(err.Error(), "cred_ref") {
		t.Fatalf("err = %v", err)
	}
}

func TestServeMeAnalystAndTenantWebOff(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	extra := identityYAML + "dialogue:\n  analyst:\n    enabled: true\n    abilities:\n      loop: true\n"
	cfg, _ := serveConfig(t, f, extra)
	iss := newHostIssuer(t)
	provisionIssuer(t, cfg, iss)
	srv := startServe(t, cfg)
	defer func() { _ = srv.stop() }()

	code, body := srv.call(t, iss, http.MethodGet, "/v1/me/analyst", "")
	// Route is mounted: without an active package the identity middleware quarantines (403).
	// A missing route would be 404.
	if code == http.StatusNotFound {
		t.Fatalf("GET /v1/me/analyst not mounted: %s", body)
	}
	if code != http.StatusOK && code != http.StatusForbidden {
		t.Fatalf("GET /v1/me/analyst = %d: %s", code, body)
	}
}

func TestLoopBudgetAdaptTurnCeiling(t *testing.T) {
	b := &loopBudgetAdapt{maxTurnToks: 100}
	scope := contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws",
		PrincipalID: "usr", SessionID: "ses", Roles: []string{"r"},
	}
	if _, err := b.Reserve(context.Background(), scope, 60); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(context.Background(), scope, 50); contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("want turn ceiling, got %v", err)
	}
}
