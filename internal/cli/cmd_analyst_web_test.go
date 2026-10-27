package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/analyst/web"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

func TestWebTurnBagIntersectsPackagePreferredHosts(t *testing.T) {
	bag := &webTurnBag{deploy: web.HostPolicy{Allow: []string{"gov.it", "europa.eu"}}}
	scope := contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws",
		PrincipalID: "usr", SessionID: "ses", Roles: []string{"r"},
	}
	ctx := context.Background()
	old := analystPackagePreferredHosts
	t.Cleanup(func() { analystPackagePreferredHosts = old })

	analystPackagePreferredHosts = func(context.Context, contracts.Scope) []string { return nil }
	p := bag.policyFor(ctx, scope)
	if !p.HostAllowed("europa.eu") {
		t.Fatal("absent package preferred_web_hosts must not narrow europa.eu")
	}

	analystPackagePreferredHosts = func(context.Context, contracts.Scope) []string { return []string{"gov.it"} }
	p = bag.policyFor(ctx, scope)
	if !p.HostAllowed("gov.it") {
		t.Fatal("gov.it must remain after package intersection")
	}
	if p.HostAllowed("europa.eu") {
		t.Fatal("europa.eu must be refused when package preferred_web_hosts is [gov.it]")
	}
}

func TestWebTurnBagKeyedByTurnAndEndTurn(t *testing.T) {
	bag := &webTurnBag{
		deploy: web.HostPolicy{Allow: []string{"gov.it"}},
		budget: web.Budget{MaxSearches: 1, MaxFetches: 1},
	}
	scope := contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws",
		PrincipalID: "usr", SessionID: "ses", Roles: []string{"r"},
	}
	ctx := context.Background()
	var queries []string
	searcher := webSearcherAdapt{
		inner: web.NewSearcher(fixedSearchProv{queries: &queries, hits: []web.SearchHit{{URL: "https://gov.it/a", Title: "a"}}}),
		turns: bag, allows: func(context.Context, contracts.Scope) bool { return true },
	}
	if _, err := searcher.Search(ctx, scope, loop.SearchInput{Query: "q1", TurnID: "trn_1"}); err != nil {
		t.Fatal(err)
	}
	// Same turn: budget exhausted (max_searches=1).
	if _, err := searcher.Search(ctx, scope, loop.SearchInput{Query: "q1b", TurnID: "trn_1"}); contracts.CodeOf(err) != contracts.ErrAnalystCapExceeded {
		t.Fatalf("turn 1 second search: %v", err)
	}
	// Turn 2 without a search: URL from turn 1 is not fetchable.
	fetcher := webFetcherAdapt{inner: web.NewFetcher(nil, nil, nil), turns: bag}
	if _, err := fetcher.Fetch(ctx, scope, loop.FetchInput{URL: "https://gov.it/a", TurnID: "trn_2"}); contracts.CodeOf(err) != contracts.ErrWebQueryRefused {
		t.Fatalf("cross-turn fetch: %v", err)
	}
	// Fresh turn gets a fresh search budget.
	if _, err := searcher.Search(ctx, scope, loop.SearchInput{Query: "q2", TurnID: "trn_2"}); err != nil {
		t.Fatal(err)
	}
	bag.EndTurn("trn_1")
	bag.EndTurn("trn_2")
	if bag.len() != 0 {
		t.Fatalf("bag should be empty after EndTurn, got %d", bag.len())
	}
}

type fixedSearchProv struct {
	queries *[]string
	hits    []web.SearchHit
}

func (f fixedSearchProv) Name() string { return "fixed" }
func (f fixedSearchProv) Search(_ context.Context, q string) ([]web.SearchHit, error) {
	*f.queries = append(*f.queries, q)
	return f.hits, nil
}

func TestWebSearcherAdaptPassesDatasetsAndPolicy(t *testing.T) {
	var got []string
	bag := &webTurnBag{deploy: web.HostPolicy{Allow: []string{"gov.it"}}, budget: web.Budget{MaxSearches: 3, MaxFetches: 3}}
	adapt := webSearcherAdapt{
		inner:  web.NewSearcher(fixedSearchProv{queries: &got, hits: []web.SearchHit{{URL: "https://gov.it/x", Title: "t", Snippet: "s"}}}),
		turns:  bag,
		allows: func(context.Context, contracts.Scope) bool { return true },
	}
	scope := contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws",
		PrincipalID: "usr", SessionID: "ses", Roles: []string{"r"},
	}
	ds := []an.Dataset{{
		Fields: []an.DatasetDatasetField{{Name: "revenue", Type: an.FieldTypeNumber, DataClass: an.DataClassConfidential}},
		Rows:   []an.DatasetRow{{"revenue": float64(1234567)}},
	}}
	_, err := adapt.Search(context.Background(), scope, loop.SearchInput{
		Query: "apply with revenue 1234567 for XX", TurnID: "trn_a", Datasets: ds,
	})
	if contracts.CodeOf(err) != contracts.ErrWebQueryRefused {
		t.Fatalf("want web_query_refused for confidential leak, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("provider must not be called: %v", got)
	}

	_, err = adapt.Search(context.Background(), scope, loop.SearchInput{
		Query: "Italian 2026 company discount XX", TurnID: "trn_a", Datasets: ds,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "Italian") {
		t.Fatalf("queries = %v", got)
	}

	denied := webSearcherAdapt{inner: adapt.inner, turns: bag, allows: func(context.Context, contracts.Scope) bool { return false }}
	_, err = denied.Search(context.Background(), scope, loop.SearchInput{
		Query: "ok query", TurnID: "trn_b", Datasets: nil,
	})
	if contracts.CodeOf(err) != contracts.ErrWebQueryRefused {
		t.Fatalf("want policy refusal, got %v", err)
	}
}

func TestAnalystVerifierUsesTurnLanguage(t *testing.T) {
	v := analystVerifier{}
	facts := loop.FactSet{
		Datasets: map[an.DatasetID]an.Dataset{
			"ds1": {
				Fields: []an.DatasetDatasetField{{Name: "total", Type: an.FieldTypeNumber, DataClass: an.DataClassInternal}},
				Rows:   []an.DatasetRow{{"total": float64(1234.5)}},
			},
		},
	}
	// Matching Italian decimal form of the fact should verify under language=it.
	ok, err := v.VerifyAnswer(context.Background(), loop.VerifyAnswerInput{
		TextPlan: "Il totale è 1.234,5.", Language: "it", Facts: facts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Verified {
		t.Fatalf("Italian 1.234,5 must parse as 1234.5 and verify: text=%q segments=%v", ok.Text, ok.Segments)
	}
	// Invented figure → Italian refusal/replacement templates.
	out, err := v.VerifyAnswer(context.Background(), loop.VerifyAnswerInput{
		TextPlan: "Il totale è 9.999.999.", Language: "it", Facts: facts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Verified {
		t.Fatalf("invented number should not verify: %+v", out)
	}
	joined := out.Text
	for _, s := range out.Segments {
		joined += " " + s.Text + " " + s.Reason
	}
	if !strings.Contains(joined, "Risposta") && !strings.Contains(strings.ToLower(joined), "sostitu") {
		t.Fatalf("want Italian replacement template, got text=%q segments=%v", out.Text, out.Segments)
	}
}
