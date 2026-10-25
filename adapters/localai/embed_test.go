package localai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// embedHandler answers like LocalAI's /v1/embeddings (core/http/endpoints/
// openai/embeddings.go): one data item per input with its index, usage zeroed
// unless tokens is set. The items come back in reverse order so that callers
// that trust the position instead of the index are caught. An input's vector
// is [len(input), index, 0.5].
func embedHandler(tokens int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		var items []string
		for i := len(in.Input) - 1; i >= 0; i-- {
			items = append(items, fmt.Sprintf(`{"index":%d,"object":"embedding","embedding":[%d,%d,0.5]}`, i, len(in.Input[i]), i))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"created":1790000000,"object":"list","id":"emb-1","model":%q,"data":[%s],"usage":{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}}`,
			in.Model, strings.Join(items, ","), tokens, tokens)
	}
}

func TestEmbeddingsBatch(t *testing.T) {
	rt := newRuntime(t, embedHandler(10))
	c, key := rt.client(t, func(cfg *Config) {
		cfg.EmbedBatchSize = 2
		cfg.EmbedQueryPrefix = "query: "
		cfg.EmbedDocumentPrefix = "passage: "
	})

	inputs := []string{"a", "bb", "ccc", "dddd", "eeeee"}
	res, err := c.Embed(bg, testScope(), providers.EmbedRequest{Inputs: inputs, Purpose: providers.EmbedDocument})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	reqs := rt.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests for 5 inputs at batch size 2, want 3", len(reqs))
	}
	var sizes []int
	var seen []string
	for _, r := range reqs {
		if r.Method != "POST" || r.Path != "/v1/embeddings" {
			t.Errorf("request = %s %s", r.Method, r.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("embeddings request lacks the bearer key")
		}
		body := decodeObject(t, r.Body)
		if diff := cmp.Diff([]string{"encoding_format", "input", "model"}, keysOf(body)); diff != "" {
			t.Errorf("request fields (-want +got):\n%s", diff)
		}
		if body["model"] != "embed-model" || body["encoding_format"] != "float" {
			t.Errorf("model/encoding_format = %v/%v", body["model"], body["encoding_format"])
		}
		in, _ := body["input"].([]any)
		sizes = append(sizes, len(in))
		for _, x := range in {
			seen = append(seen, x.(string))
		}
	}
	if diff := cmp.Diff([]int{2, 2, 1}, sizes); diff != "" {
		t.Errorf("batch sizes (-want +got):\n%s", diff)
	}
	var wantSeen []string
	for _, in := range inputs {
		wantSeen = append(wantSeen, "passage: "+in)
	}
	if diff := cmp.Diff(wantSeen, seen); diff != "" {
		t.Errorf("inputs sent, in order (-want +got):\n%s", diff)
	}

	if len(res.Vectors) != 5 || res.Dimensions != 3 {
		t.Fatalf("vectors = %d, dims = %d", len(res.Vectors), res.Dimensions)
	}
	for i, v := range res.Vectors {
		// vector[0] is the length of the prefixed input: the proof that the
		// reversed responses were put back in input order.
		if want := float32(len("passage: ") + len(inputs[i])); v[0] != want {
			t.Errorf("vector %d starts with %v, want %v (indexes must decide the order)", i, v[0], want)
		}
	}
	if res.Usage.InputTokens != 30 || res.Model != "embed-model" || res.TraceID != "emb-1" {
		t.Errorf("usage/model/trace = %+v / %q / %q", res.Usage, res.Model, res.TraceID)
	}
	if err := res.CheckResult(providers.EmbedRequest{Inputs: inputs}); err != nil {
		t.Errorf("result fails the provider contract check: %v", err)
	}
}

func TestEmbeddingsPurposeAndProfile(t *testing.T) {
	rt := newRuntime(t, embedHandler(0))
	c, _ := rt.client(t, func(cfg *Config) {
		cfg.EmbedQueryPrefix = "q: "
		cfg.EmbedDocumentPrefix = "d: "
		cfg.Profiles["lite"] = ModelSet{Embedder: "embed-lite"}
	})
	for _, tt := range []struct {
		purpose providers.EmbedPurpose
		profile string
		want    string
		model   string
	}{
		{providers.EmbedQuery, "", "q: x", "embed-model"},
		{providers.EmbedDocument, "", "d: x", "embed-model"},
		{"", "", "x", "embed-model"},
		{"", "lite", "x", "embed-lite"},
	} {
		if _, err := c.Embed(bg, testScope(), providers.EmbedRequest{Inputs: []string{"x"}, Purpose: tt.purpose, Profile: tt.profile}); err != nil {
			t.Fatal(err)
		}
		reqs := rt.requests()
		body := decodeObject(t, reqs[len(reqs)-1].Body)
		if in := body["input"].([]any); in[0] != tt.want || body["model"] != tt.model {
			t.Errorf("purpose %q profile %q: sent %v to %v, want %q to %s", tt.purpose, tt.profile, in, body["model"], tt.want, tt.model)
		}
	}
}

func TestEmbeddingsRejectsBadCallsAndAnswers(t *testing.T) {
	scope := testScope()
	t.Run("invalid calls never reach the runtime", func(t *testing.T) {
		rt := newRuntime(t, embedHandler(0))
		c, _ := rt.client(t)
		for name, tt := range map[string]struct {
			scope contracts.Scope
			req   providers.EmbedRequest
			code  contracts.ErrCode
		}{
			"zero scope":      {contracts.Scope{}, providers.EmbedRequest{Inputs: []string{"a"}}, contracts.ErrScopeMissing},
			"no inputs":       {scope, providers.EmbedRequest{}, contracts.ErrValidationFailed},
			"blank input":     {scope, providers.EmbedRequest{Inputs: []string{"a", "  "}}, contracts.ErrValidationFailed},
			"unknown purpose": {scope, providers.EmbedRequest{Inputs: []string{"a"}, Purpose: "x"}, contracts.ErrValidationFailed},
			"unknown profile": {scope, providers.EmbedRequest{Inputs: []string{"a"}, Profile: "huge"}, contracts.ErrValidationFailed},
		} {
			_, err := c.Embed(bg, tt.scope, tt.req)
			if !contracts.HasCode(err, tt.code) {
				t.Errorf("%s: err = %v, want %s", name, err, tt.code)
			}
		}
		if n := len(rt.requests()); n != 0 {
			t.Fatalf("%d invalid calls reached the runtime", n)
		}
	})

	req := providers.EmbedRequest{Inputs: []string{"a", "bb", "ccc"}}
	answers := []struct {
		name string
		body string
		code contracts.ErrCode
	}{
		{"fewer vectors than inputs", `{"data":[{"index":0,"embedding":[1]}]}`, contracts.ErrSchemaInvalid},
		{"repeated index", `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[1]},{"index":2,"embedding":[1]}]}`, contracts.ErrSchemaInvalid},
		{"index out of range", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1]},{"index":3,"embedding":[1]}]}`, contracts.ErrSchemaInvalid},
		{"missing index", `{"data":[{"embedding":[1]},{"embedding":[1]},{"embedding":[1]}]}`, contracts.ErrSchemaInvalid},
		{"empty embedding", `{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[1]},{"index":2,"embedding":[1]}]}`, contracts.ErrSchemaInvalid},
		{"base64 embedding", `{"data":[{"index":0,"embedding":"AAAA"},{"index":1,"embedding":"AAAA"},{"index":2,"embedding":"AAAA"}]}`, contracts.ErrSchemaInvalid},
		{"vectors of different sizes", `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[1]},{"index":2,"embedding":[1,2]}]}`, contracts.ErrSchemaInvalid},
		{"negative usage", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1]},{"index":2,"embedding":[1]}],"usage":{"prompt_tokens":-3}}`, contracts.ErrSchemaInvalid},
		{"not JSON", `nope`, contracts.ErrSchemaInvalid},
		{"error object", `{"error":{"message":"out of memory","type":"server_error"}}`, contracts.ErrResourceExhausted},
	}
	for _, tt := range answers {
		t.Run(tt.name, func(t *testing.T) {
			rt := newRuntime(t, serveJSON(200, tt.body))
			c, _ := rt.client(t)
			res, err := c.Embed(bg, scope, req)
			wantCode(t, err, tt.code)
			if res.Vectors != nil {
				t.Error("a failed call returned vectors")
			}
		})
	}

	t.Run("a failing later batch fails the whole call", func(t *testing.T) {
		var calls atomic.Int32
		rt := newRuntime(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 2 {
				serveJSON(500, `{"error":{"message":"could not load model","code":500}}`)(w, r)
				return
			}
			embedHandler(0)(w, r)
		})
		c, _ := rt.client(t, func(cfg *Config) { cfg.EmbedBatchSize = 1 })
		res, err := c.Embed(bg, scope, req)
		wantCode(t, err, contracts.ErrResourceExhausted)
		if res.Vectors != nil {
			t.Error("partial vectors returned")
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("%d requests, want it to stop at the failing batch (2)", n)
		}
	})

	t.Run("vectors of different sizes across batches", func(t *testing.T) {
		var calls atomic.Int32
		rt := newRuntime(t, func(w http.ResponseWriter, r *http.Request) {
			dim := int(calls.Add(1)) + 1
			vec := strings.TrimSuffix(strings.Repeat("1,", dim), ",")
			serveJSON(200, fmt.Sprintf(`{"data":[{"index":0,"embedding":[%s]}]}`, vec))(w, r)
		})
		c, _ := rt.client(t, func(cfg *Config) { cfg.EmbedBatchSize = 1 })
		_, err := c.Embed(bg, scope, req)
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
}
