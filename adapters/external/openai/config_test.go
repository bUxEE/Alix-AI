package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestConfigValidation(t *testing.T) {
	ok := testConfig("https://api.openai.com")
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no credential ref": func(c *Config) { c.CredentialRef = "" },
		"no model":          func(c *Config) { c.Model = "" },
		"bad model":         func(c *Config) { c.Model = "gpt 5" },
		"bad profile":       func(c *Config) { c.Profiles = map[string]string{"lite": "bad model"} },
		"bad effort":        func(c *Config) { c.ReasoningEffort = "maximum-overdrive" },
		"negative limit":    func(c *Config) { c.MaxOutputToks = -1 },
		"bad price":         func(c *Config) { c.Prices = &providers.PriceTable{Currency: "dollars"} },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The destination must be a bare origin and the plain-http rule is egress's.
	reg := egress.NewProviderRegistry()
	for _, base := range []string{"https://api.openai.com/v1", "ftp://x", "https://u:p@api.openai.com"} {
		if _, err := New(testConfig(base), reg.Dialer(ProviderName), &fakeKeys{}); err == nil {
			t.Errorf("%s: accepted", base)
		}
	}
	if _, err := New(testConfig(""), reg.Dialer(ProviderName), &fakeKeys{}); err != nil {
		t.Errorf("the default base URL: %v", err)
	}
	if _, err := New(testConfig("https://api.openai.com"), reg.Dialer(ProviderName), nil); err == nil {
		t.Error("a client without a key source")
	}
	// With no allowlist entry the client is built but reaches nothing.
	c, err := New(testConfig("https://api.openai.com"), reg.Dialer(ProviderName), &fakeKeys{key: fixtureKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Generate(context.Background(), testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrPermissionDenied)
	if ce.Details["sent"] != false {
		t.Fatalf("details %v", ce.Details)
	}
}

func TestCapabilitiesAndProfiles(t *testing.T) {
	r := newRig(t, replyJSON(textResponse), func(c *Config) {
		c.Profiles = map[string]string{"lite": "gpt-5-nano"}
		c.EmbeddingModel = ""
	})
	caps, err := r.client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := caps.Validate(); err != nil {
		t.Fatal(err)
	}
	if caps.StructuredOutput != providers.StructuredStrict || !caps.Tools.Supported || !caps.Tools.StrictArguments ||
		caps.Provider != "openai" || caps.Model != "gpt-5-mini" || caps.Pricing == nil || caps.Pricing.Currency != "USD" ||
		len(caps.InputModalities) != 1 {
		t.Fatalf("caps %+v", caps)
	}
	if len(r.api.requests()) != 0 {
		t.Fatal("capabilities made a call")
	}
	// A profile hint selects the configured alternative; an unknown one is refused.
	req := userReq("hi")
	req.Profile = "lite"
	if _, err := r.client.Generate(context.Background(), testScope(), req); err != nil {
		t.Fatal(err)
	}
	if got := r.api.requests()[0].json(t)["model"]; got != "gpt-5-nano" {
		t.Fatalf("model %v", got)
	}
	req.Profile = "mystery"
	if _, err := r.client.Generate(context.Background(), testScope(), req); err == nil {
		t.Fatal("unknown profile accepted")
	}
	// Embeddings are unsupported unless a model is configured.
	_, err = r.client.Embed(context.Background(), testScope(), providers.EmbedRequest{Inputs: []string{"a"}})
	if _, ok := providers.IsUnsupported(err); !ok {
		t.Fatalf("got %v", err)
	}
	// A missing scope fails closed before anything is sent.
	if _, err := r.client.Generate(context.Background(), contracts.Scope{}, userReq("hi")); err == nil {
		t.Fatal("no scope accepted")
	}
	// The max-output cap is part of the capability check.
	big := userReq("hi")
	big.MaxToks = 10_000_000
	if _, err := r.client.Generate(context.Background(), testScope(), big); err == nil {
		t.Fatal("an output cap above the model's limit was sent")
	}
}

func TestHealthClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		want   providers.Health
	}{
		"unauthenticated answer proves reachability": {401, providers.HealthOK},
		"ok":           {200, providers.HealthOK},
		"rate limited": {429, providers.HealthDegraded},
		"server error": {500, providers.HealthUnavailable},
	} {
		r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "" {
				t.Error("the health probe sent a credential")
			}
			w.Header().Set("Retry-After", "600")
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, `{"error":{"message":"x"}}`)
		}, nil)
		h, err := r.client.Health(context.Background())
		if err != nil || h.Status != tc.want {
			t.Errorf("%s: %+v %v", name, h, err)
		}
	}
	api := newFakeAPI(t, nil)
	dead := api.srv.URL
	api.srv.Close()
	r := newRigFor(t, api, testConfig(dead), dead)
	if h, err := r.client.Health(context.Background()); err != nil || h.Status != providers.HealthUnavailable || strings.Contains(h.Reason, "127.0.0.1") {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestModelDiscoveryAndEmbeddings(t *testing.T) {
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/embeddings" {
			_, _ = io.WriteString(w, `{"model":"m","data":[{"index":1,"embedding":[1,0]},{"index":0,"embedding":[0,1]}],"usage":{"prompt_tokens":4}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5-nano"},{"id":"not a model id!"},{"id":"gpt-4o-mini"}]}`)
	}, func(c *Config) { c.EmbeddingModel = "text-embedding-3-small" })
	ids, err := r.client.Models(context.Background(), testScope())
	if err != nil || strings.Join(ids, ",") != "gpt-4o-mini,gpt-5-nano" {
		t.Fatalf("%v %v", ids, err)
	}
	// The configured model (gpt-5-mini) is not offered to this key.
	err = r.client.CheckModel(context.Background(), testScope())
	wantCode(t, err, contracts.ErrValidationFailed)
	res, err := r.client.Embed(context.Background(), testScope(), providers.EmbedRequest{Inputs: []string{"a", "b"}})
	if err != nil || res.Dimensions != 2 || res.Vectors[0][1] != 1 || res.Usage.InputTokens != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	body := r.api.requests()[len(r.api.requests())-1].json(t)
	if body["model"] != "text-embedding-3-small" || body["encoding_format"] != "float" {
		t.Fatalf("body %v", body)
	}
}
