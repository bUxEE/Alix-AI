package localai

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The client plugs into the provider registry and the budget decorators of
// internal/providers: the observed version reaches the role status, and a
// request over budget is validation_failed before the runtime is called.
func TestWorksUnderRegistryAndBudget(t *testing.T) {
	mux := healthMux(readyOK, serveJSON(200, versionBody), serveJSON(200, modelsBody))
	chat := func(w http.ResponseWriter, r *http.Request) { serveJSON(200, chatBody("fine", "stop"))(w, r) }
	rt := newRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			chat(w, r)
		case "/v1/embeddings":
			embedHandler(0)(w, r)
		default:
			mux(w, r)
		}
	})
	c, _ := rt.client(t)

	budget := providers.DefaultBudget()
	gen, err := providers.WithBudget(c, budget)
	if err != nil {
		t.Fatal(err)
	}
	emb, err := providers.WithEmbedBudget(c, budget)
	if err != nil {
		t.Fatal(err)
	}

	reg := providers.NewRegistry(clock.Real{})
	if err := reg.Register(providers.RoleController, c); err != nil {
		t.Fatalf("Register controller: %v", err)
	}
	if err := reg.Register(providers.RoleEmbedder, c); err != nil {
		t.Fatalf("Register embedder: %v", err)
	}
	reg.Refresh(context.Background())
	for _, role := range []providers.Role{providers.RoleController, providers.RoleEmbedder} {
		st := reg.Status(role)
		if st.Status != providers.HealthOK || st.Version != "v4.10.0" || st.Provider != "localai" {
			t.Errorf("status of %s = %+v", role, st)
		}
	}

	if res, err := gen.Chat(bg, testScope(), chatReq("hello")); err != nil || res.Text != "fine" {
		t.Fatalf("Chat through the budget = %+v, %v", res, err)
	}
	if res, err := emb.Embed(bg, testScope(), providers.EmbedRequest{Inputs: []string{"a", "bb"}}); err != nil || len(res.Vectors) != 2 {
		t.Fatalf("Embed through the budget = %+v, %v", res, err)
	}

	before := len(rt.requests())
	_, err = gen.Chat(bg, testScope(), chatReq(strings.Repeat("x", 4*budget.MaxPromptTokens)))
	wantCode(t, err, contracts.ErrValidationFailed)
	if contracts.HasCode(err, contracts.ErrResourceExhausted) {
		t.Error("a budget violation must not be resource_exhausted")
	}
	if len(rt.requests()) != before {
		t.Error("an over-budget request reached the runtime")
	}
}
