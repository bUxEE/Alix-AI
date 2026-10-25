package sdkgo_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/decision"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

func decisionHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.claims = sdktest.Claims{CapabilityScopes: []string{sdkgo.DecisionStatusCapability}}
	return h
}

func TestDecisionStatusReadsTheFixtureAndOnlyTheStatus(t *testing.T) {
	h := decisionHarness(t)
	h.fake.SetDecisionStatus(sdktest.DecisionStatusFixture(decision.DecisionStatusModeAdvisory, 7,
		map[decision.FallbackReason]int64{decision.FallbackReasonLowConfidence: 2, decision.FallbackReasonTimeout: 1}))
	got := must[*decision.DecisionStatus](t, "status")(h.client.Decision().Status(context.Background()))
	if got.Mode != decision.DecisionStatusModeAdvisory || got.AcceptedCount != 7 || got.RejectedCount != 3 || len(got.FallbackCounts) != 2 {
		t.Fatalf("unexpected status %+v", got)
	}
	if got.ModelID == nil || got.ModelRevision == nil {
		t.Fatal("a configured model is reported with its revision")
	}
	reqs := h.fake.RequestsOf("getDecisionStatus")
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/v1/decision/status" || reqs[0].RawQuery != "" || !reqs[0].Authenticated {
		t.Fatalf("one authenticated GET of the status route and nothing else, got %+v", reqs)
	}
	if tr := h.tokenRequests(); len(tr) != 1 || tr[0].Operation != "getDecisionStatus" {
		t.Fatalf("one fresh token for the status operation, got %+v", tr)
	}
}

func TestDecisionStatusDefaultIsTheLayerOff(t *testing.T) {
	h := decisionHarness(t)
	got := must[*decision.DecisionStatus](t, "status")(h.client.Decision().Status(context.Background()))
	if got.Mode != decision.DecisionStatusModeOff || got.AcceptedCount != 0 || got.RejectedCount != 0 || got.ModelID != nil {
		t.Fatalf("an unconfigured fake reports the layer off with no model, got %+v", got)
	}
}

func TestDecisionStatusFailsClosed(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t) // the default claims do not carry the capability
	_, err := h.client.Decision().Status(ctx)
	hasCode(t, "no capability", err, contracts.ErrPermissionDenied)

	h = decisionHarness(t)
	h.fake.SetDecisionUnavailable(true)
	_, err = h.client.Decision().Status(ctx)
	hasCode(t, "no decision service is unavailable, never an empty status", err, contracts.ErrUnavailable)

	h = decisionHarness(t)
	h.fake.SetApplicationStatus("quarantined")
	_, err = h.client.Decision().Status(ctx)
	hasCode(t, "a quarantined application", err, contracts.ErrQuarantined)

	h = decisionHarness(t)
	h.fake.FailNext("getDecisionStatus", http.StatusServiceUnavailable, contracts.ErrUnavailable, "down")
	_, err = h.client.Decision().Status(ctx)
	hasCode(t, "a queued failure", err, contracts.ErrUnavailable)
}

func TestDecisionStatusRefusesAnInconsistentBody(t *testing.T) {
	ok := sdktest.DecisionStatusFixture(decision.DecisionStatusModeShadow, 1, map[decision.FallbackReason]int64{decision.FallbackReasonTimeout: 2})
	mutations := map[string]func(*decision.DecisionStatus){
		"rejected_count is not the sum": func(s *decision.DecisionStatus) { s.RejectedCount = 5 },
		"a negative accepted count":     func(s *decision.DecisionStatus) { s.AcceptedCount = -1 },
		"a reason twice": func(s *decision.DecisionStatus) {
			s.FallbackCounts = append(s.FallbackCounts, decision.DecisionStatusFallbackCount{Reason: decision.FallbackReasonTimeout, Count: 0})
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			h := decisionHarness(t)
			bad := ok
			bad.FallbackCounts = append([]decision.DecisionStatusFallbackCount(nil), ok.FallbackCounts...)
			mutate(&bad)
			h.fake.SetDecisionStatus(bad)
			_, err := h.client.Decision().Status(context.Background())
			hasCode(t, name, err, contracts.ErrInternal)
			if !strings.Contains(err.Error(), "unexpected shape") {
				t.Fatalf("want an unexpected-shape error, got %v", err)
			}
		})
	}
}

// A score must never look like an authorisation: no exported name of the decision client says so, and its
// documentation states that the data is advisory.
func TestDecisionClientNamesAndDocsAreAdvisory(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "decision.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		var name string
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name = d.Name.Name
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					name = ts.Name.Name
				}
			}
		}
		for _, banned := range []string{"Authoris", "Authoriz", "Allow", "Permit", "Approve", "Grant"} {
			if strings.Contains(name, banned) {
				t.Errorf("%s: a decision name must not read as an authorisation", name)
			}
		}
	}
	doc := file.Doc.Text() + docOf(file, "DecisionClient") + docOf(file, "Status")
	for _, want := range []string{"advisory", "never an authorisation"} {
		if !strings.Contains(strings.ToLower(doc), want) {
			t.Errorf("the documentation of the decision client must say %q", want)
		}
	}
}

func docOf(file *ast.File, name string) string {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				return d.Doc.Text()
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == name {
					return d.Doc.Text()
				}
			}
		}
	}
	return ""
}
