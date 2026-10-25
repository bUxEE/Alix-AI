package clm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/adapters/decision/internal/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

func TestCLMRankRequestShape(t *testing.T) {
	t.Run("rank", func(t *testing.T) {
		srv := newServer(t, standard())
		res, err := score(t, newProvider(t, testConfig(srv.URL)), testRequest(3))
		if err != nil {
			t.Fatal(err)
		}
		if res.Disposition != gen.DecisionScoreResultDispositionAccepted || len(res.Selections) != 1 ||
			res.Selections[0].CandidateID != "c2" || float64(res.Selections[0].Score) != 0.97 || float64(res.Selections[0].CalibratedConfidence) != 0.97 {
			t.Fatalf("result = %+v", res)
		}
		if *res.ModelID != "clm-latest" || *res.ModelRevision != "rev1" || res.ProviderID != ProviderID || res.OodStatus != gen.DecisionScoreResultOodStatusInDistribution {
			t.Fatalf("identity = %+v", res)
		}
		var rank *seen
		for _, r := range srv.requests() {
			if r.Path == "/v1/rank" {
				r := r
				rank = &r
			}
		}
		if rank == nil || rank.Method != http.MethodPost || rank.ContentType != "application/json" {
			t.Fatalf("no rank request: %+v", srv.requests())
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(rank.Body, &top); err != nil {
			t.Fatal(err)
		}
		keys := map[string]bool{}
		for k := range top {
			keys[k] = true
		}
		if diff := cmp.Diff(map[string]bool{"context": true, "question": true, "answers": true}, keys); diff != "" {
			t.Fatalf("members (-want +got):\n%s", diff)
		}
		var body rankRequest
		if err := json.Unmarshal(rank.Body, &body); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]string{"Option 1: the first one", "Option 2", "Option 3"}, body.Answers); diff != "" {
			t.Fatalf("answers (-want +got):\n%s", diff)
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(body.Context), &state); err != nil {
			t.Fatalf("context is not the state string: %v", err)
		}
		if diff := cmp.Diff(map[string]any{"decision_type": "item_selection", "locale": "en", "state": map[string]any{"step": "pick", "n": float64(3)}}, state); diff != "" {
			t.Fatalf("context (-want +got):\n%s", diff)
		}
		if body.Question == "" {
			t.Fatal("no question")
		}
		for _, id := range []string{"ten1", "app1", "ses1", "cv1", "2026-10-07", "c1", "c2", "c3"} {
			if bytes.Contains(rank.Body, []byte(id)) {
				t.Fatalf("the rank request carries %q", id)
			}
		}
	})

	t.Run("identical candidate texts are told apart", func(t *testing.T) {
		srv := newServer(t, standard())
		req := testRequest(3)
		req.CandidateSet.Candidates[1].Label = "Same"
		req.CandidateSet.Candidates[2].Label = "Same"
		req.CandidateSet.Candidates[0].Label = "Other"
		req.CandidateSet.Candidates[0].Text = nil
		_, _ = score(t, newProvider(t, testConfig(srv.URL)), req)
		var body rankRequest
		for _, r := range srv.requests() {
			if r.Path == "/v1/rank" {
				_ = json.Unmarshal(r.Body, &body)
			}
		}
		if diff := cmp.Diff([]string{"Other", "Same [c2]", "Same [c3]"}, body.Answers); diff != "" {
			t.Fatalf("answers (-want +got):\n%s", diff)
		}
		// a rank answer naming "Same" alone is ambiguous and is refused, not guessed
		amb := newServer(t, with(standard(), "POST /v1/rank", ok(`{"model":"clm-latest","ranked":[{"rank":1,"candidate":"Same","prob":0.99}]}`)))
		res, err := score(t, newProvider(t, testConfig(amb.URL)), req)
		if got := wire.ReasonOf(res, err); got != gen.FallbackReasonMalformedOutput {
			t.Fatalf("reason = %q (%v)", got, err)
		}
	})

	t.Run("systemone", func(t *testing.T) {
		srv := newServer(t, standard())
		cfg := testConfig(srv.URL)
		cfg.Mode = ModeSystemOne
		res, err := score(t, newProvider(t, cfg), testRequest(3))
		if err != nil || len(res.Selections) != 1 || res.Selections[0].CandidateID != "c2" || float64(res.Selections[0].CalibratedConfidence) != 0.93 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if srv.count("/v1/rank") != 0 {
			t.Fatal("rank was called in systemone mode")
		}
		var body struct {
			Model     string
			State     string
			Questions map[string]question
		}
		for _, r := range srv.requests() {
			if r.Path == "/v1/systemone" {
				if err := json.Unmarshal(r.Body, &body); err != nil {
					t.Fatal(err)
				}
			}
		}
		want := map[string]string{"c1": "Option 1: the first one", "c2": "Option 2", "c3": "Option 3"}
		if body.Model != "clm-latest" || body.Questions["choice"].Type != "choice" || cmp.Diff(want, body.Questions["choice"].Criteria) != "" {
			t.Fatalf("body = %+v", body)
		}
	})

	t.Run("runner-ups need their own probability", func(t *testing.T) {
		both := `{"model":"clm-latest","ranked":[{"rank":1,"candidate":"Option 2","prob":0.6},{"rank":2,"candidate":"Option 1: the first one","prob":0.9},{"rank":3,"candidate":"Option 3","prob":0.01}]}`
		srv := newServer(t, with(standard(), "POST /v1/rank", ok(both)))
		req := testRequest(3)
		req.CandidateSet.MaxSelections = 3
		res, err := score(t, newProvider(t, testConfig(srv.URL)), req)
		// the pick is always rank 1; c1 clears the floor, c3 does not
		if err != nil || len(res.Selections) != 2 || res.Selections[0].CandidateID != "c2" || res.Selections[1].CandidateID != "c1" {
			t.Fatalf("selections = %+v (%v)", res.Selections, err)
		}
	})
}

func TestCLMOnlyAllowlistedEndpointsCalled(t *testing.T) {
	srv := newServer(t, standard())
	p := newProvider(t, testConfig(srv.URL))
	if err := p.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := score(t, p, testRequest(3)); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, e := range AllowedEndpoints() {
		allowed[e] = true
	}
	if diff := cmp.Diff(map[string]bool{"POST /v1/rank": true, "POST /v1/systemone": true, "GET /v1/models": true, "GET /health": true}, allowed); diff != "" {
		t.Fatalf("allowlist (-want +got):\n%s", diff)
	}
	for _, r := range srv.requests() {
		if !allowed[r.Method+" "+r.Path] {
			t.Errorf("the service received %s %s", r.Method, r.Path)
		}
	}
	before := len(srv.requests())
	for _, c := range [][2]string{
		{http.MethodGet, "/"}, {http.MethodGet, "/docs"}, {http.MethodPost, "/v1/models"}, {http.MethodGet, "/v1/rank"},
		{http.MethodPost, "/v1/embeddings"}, {http.MethodGet, "/health?verbose=1"}, {http.MethodDelete, "/v1/rank"},
	} {
		if _, err := p.client.Do(context.Background(), c[0], c[1], nil); !errors.Is(err, wire.ErrForbiddenEndpoint) {
			t.Errorf("%s %s: %v", c[0], c[1], err)
		}
	}
	if len(srv.requests()) != before {
		t.Fatal("a forbidden request reached the service")
	}
}

func TestCLMStateOverEncoderLimitReportsUnsupportedShape(t *testing.T) {
	srv := newServer(t, standard())
	p := newProvider(t, testConfig(srv.URL))

	req := testRequest(3)
	req.BoundedState = map[string]gen.BoundedScalar{"note": strings.Repeat("a", 3*DefaultMaxStateTokens)}
	res, err := score(t, p, req)
	if err != nil || wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape || res.OodStatus != gen.DecisionScoreResultOodStatusUnsupportedShape || len(res.Selections) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("%d requests reached the service for a state it would truncate", n)
	}

	// non-ASCII text counts more per byte than ASCII: the estimate leans high on purpose
	req.BoundedState = map[string]gen.BoundedScalar{"note": strings.Repeat("é", DefaultMaxStateTokens)}
	if res, err = score(t, p, req); wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape {
		t.Fatalf("non-ascii: %v %v", res.FallbackReason, err)
	}

	// the boundary: a state of exactly the limit is sent, one token more is not
	fit := func(tokens int) gen.DecisionRequest {
		r := testRequest(3)
		r.BoundedState = map[string]gen.BoundedScalar{"note": ""}
		empty, _ := wire.StateString(r)
		r.BoundedState = map[string]gen.BoundedScalar{"note": strings.Repeat("a", 3*tokens-len(empty))}
		return r
	}
	at := fit(DefaultMaxStateTokens)
	if s, _ := wire.StateString(at); EstimateTokens(s) != DefaultMaxStateTokens {
		t.Fatalf("test arithmetic: %d tokens", EstimateTokens(s))
	}
	if res, err = score(t, p, at); err != nil || res.Disposition != gen.DecisionScoreResultDispositionAccepted {
		t.Fatalf("a state of exactly %d estimated tokens: res=%+v err=%v", DefaultMaxStateTokens, res, err)
	}
	over := fit(DefaultMaxStateTokens + 1)
	if res, err = score(t, p, over); wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape {
		t.Fatalf("one token over: %v %v", res.FallbackReason, err)
	}

	// other shapes the service cannot take
	for name, r := range map[string]gen.DecisionRequest{
		"one candidate": testRequest(1),
		"33 candidates": testRequest(33),
		"duplicate ids": func() gen.DecisionRequest {
			r := testRequest(3)
			r.CandidateSet.Candidates[1].CandidateID = "c1"
			return r
		}(),
		"oversized body": func() gen.DecisionRequest {
			r := testRequest(3)
			r.CandidateSet.Candidates[0].Text = strp(strings.Repeat("a", DefaultMaxBodyBytes))
			return r
		}(),
	} {
		res, err := score(t, p, r)
		if got := wire.ReasonOf(res, err); got != gen.FallbackReasonOodUnsupportedShape {
			t.Errorf("%s: reason = %q (%v)", name, got, err)
		}
	}
	if n := srv.count("/v1/rank"); n != 1 {
		t.Fatalf("rank calls = %d (only the state at the limit)", n)
	}
}

func TestModelOrRevisionMismatchFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		replies map[string]reply
		cfg     func(*Config)
	}{
		{"model not listed", with(standard(), "GET /v1/models", ok(`{"data":[{"id":"other","revision":"rev1"}]}`)), nil},
		{"other revision", with(standard(), "GET /v1/models", ok(`{"data":[{"id":"clm-latest","revision":"rev2"}]}`)), nil},
		{"revision not reported while pinned", with(standard(), "GET /v1/models", ok(`{"data":[{"id":"clm-latest"}]}`)), nil},
		{"empty listing", with(standard(), "GET /v1/models", ok(`{"data":[]}`)), nil},
		{"rank answers as another model", with(standard(), "POST /v1/rank", ok(`{"model":"impostor","ranked":[{"rank":1,"candidate":"Option 2","prob":0.99}]}`)), nil},
		{"systemone answers as another model", with(standard(), "POST /v1/systemone", ok(`{"model":"impostor","answers":{"choice":{"choice":"c2","confidence":0.99}}}`)), func(c *Config) { c.Mode = ModeSystemOne }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t, c.replies)
			cfg := testConfig(srv.URL)
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			res, err := score(t, newProvider(t, cfg), testRequest(3))
			if err != nil || res.Disposition != gen.DecisionScoreResultDispositionFallback || wire.ReasonOf(res, err) != gen.FallbackReasonModelMismatch || len(res.Selections) != 0 {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if strings.HasPrefix(c.name, "rank") || strings.HasPrefix(c.name, "systemone") {
				return
			}
			if srv.count("/v1/rank") != 0 {
				t.Fatal("the state was sent to a service that failed the model check")
			}
		})
	}

	t.Run("listing shapes", func(t *testing.T) {
		for name, body := range map[string]string{
			"models key": `{"models":[{"name":"clm-latest","version":"rev1"}]}`,
			"bare array": `[{"id":"clm-latest","revision":"rev1"}]`,
		} {
			srv := newServer(t, with(standard(), "GET /v1/models", ok(body)))
			if res, err := score(t, newProvider(t, testConfig(srv.URL)), testRequest(3)); err != nil || res.Disposition != gen.DecisionScoreResultDispositionAccepted {
				t.Errorf("%s: res=%+v err=%v", name, res, err)
			}
		}
	})

	t.Run("an unpinned revision checks the model only and is reported as unverified", func(t *testing.T) {
		srv := newServer(t, with(standard(), "GET /v1/models", ok(`{"data":[{"id":"clm-latest","revision":"anything"}]}`)))
		cfg := testConfig(srv.URL)
		cfg.ModelRevision = ""
		p := newProvider(t, cfg)
		if rev := p.Manifest().ModelRevision; rev != UnverifiedRevision {
			t.Fatalf("revision = %q", rev)
		}
		if res, err := score(t, p, testRequest(3)); err != nil || res.Disposition != gen.DecisionScoreResultDispositionAccepted {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("a passed check is reused for the ttl, a failed one never", func(t *testing.T) {
		now := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
		srv := newServer(t, standard())
		p := newProvider(t, testConfig(srv.URL), WithNow(func() time.Time { return now }))
		for i := 0; i < 3; i++ {
			if _, err := score(t, p, testRequest(3)); err != nil {
				t.Fatal(err)
			}
		}
		if n := srv.count("/v1/models"); n != 1 {
			t.Fatalf("model checks = %d, want 1", n)
		}
		now = now.Add(DefaultModelCheckTTL + time.Second)
		if _, err := score(t, p, testRequest(3)); err != nil {
			t.Fatal(err)
		}
		if n := srv.count("/v1/models"); n != 2 {
			t.Fatalf("model checks after the ttl = %d, want 2", n)
		}
	})
}

func TestTimeoutAndMalformedOutputMapToFallbackReasons(t *testing.T) {
	cases := []struct {
		name    string
		replies map[string]reply
		cfg     func(*Config)
		want    gen.FallbackReason
	}{
		{"slow service", with(standard(), "POST /v1/rank", reply{hang: true}), func(c *Config) { c.RequestTimeout = 80 * time.Millisecond }, gen.FallbackReasonTimeout},
		{"slow model listing", with(standard(), "GET /v1/models", reply{hang: true}), func(c *Config) { c.RequestTimeout = 80 * time.Millisecond }, gen.FallbackReasonTimeout},
		{"gateway timeout", with(standard(), "POST /v1/rank", reply{code: 504}), nil, gen.FallbackReasonTimeout},
		{"not json", with(standard(), "POST /v1/rank", ok(`<html>`)), nil, gen.FallbackReasonMalformedOutput},
		{"truncated json", with(standard(), "POST /v1/rank", ok(`{"model":"clm-latest","ranked":[{"rank":1`)), nil, gen.FallbackReasonMalformedOutput},
		{"empty ranking", with(standard(), "POST /v1/rank", ok(`{"model":"clm-latest","ranked":[]}`)), nil, gen.FallbackReasonMalformedOutput},
		{"candidate not offered", with(standard(), "POST /v1/rank", ok(`{"model":"clm-latest","ranked":[{"rank":1,"candidate":"Invented","prob":0.99}]}`)), nil, gen.FallbackReasonMalformedOutput},
		{"candidate repeated", with(standard(), "POST /v1/rank", ok(`{"model":"clm-latest","ranked":[{"rank":1,"candidate":"Option 2","prob":0.6},{"rank":2,"candidate":"Option 2","prob":0.3}]}`)), nil, gen.FallbackReasonMalformedOutput},
		{"probability out of range", with(standard(), "POST /v1/rank", ok(`{"model":"clm-latest","ranked":[{"rank":1,"candidate":"Option 2","prob":1.7}]}`)), nil, gen.FallbackReasonMalformedOutput},
		{"entry without prob", with(standard(), "POST /v1/rank", ok(`{"model":"clm-latest","ranked":[{"rank":1,"candidate":"Option 2"}]}`)), nil, gen.FallbackReasonMalformedOutput},
		{"systemone without choice", with(standard(), "POST /v1/systemone", ok(`{"answers":{"choice":{"confidence":0.9}}}`)), func(c *Config) { c.Mode = ModeSystemOne }, gen.FallbackReasonMalformedOutput},
		{"systemone invented choice", with(standard(), "POST /v1/systemone", ok(`{"answers":{"choice":{"choice":"zzz","confidence":0.9}}}`)), func(c *Config) { c.Mode = ModeSystemOne }, gen.FallbackReasonMalformedOutput},
		{"systemone probability for invented id", with(standard(), "POST /v1/systemone", ok(`{"answers":{"choice":{"choice":"c1","probabilities":{"zzz":0.5}}}}`)), func(c *Config) { c.Mode = ModeSystemOne }, gen.FallbackReasonMalformedOutput},
		{"models listing not json", with(standard(), "GET /v1/models", ok(`nope`)), nil, gen.FallbackReasonMalformedOutput},
		{"response over the cap", with(standard(), "POST /v1/rank", ok(`{"ranked":[],"pad":"`+strings.Repeat("x", 4096)+`"}`)), func(c *Config) { c.MaxResponseBytes = 1024 }, gen.FallbackReasonMalformedOutput},
		{"busy", with(standard(), "POST /v1/rank", reply{code: 503}), nil, gen.FallbackReasonResourceGate},
		{"too many requests", with(standard(), "POST /v1/rank", reply{code: 429}), nil, gen.FallbackReasonResourceGate},
		{"server error", with(standard(), "POST /v1/rank", reply{code: 500, body: `{"detail":"boom"}`}), nil, gen.FallbackReasonUnavailable},
		{"request rejected", with(standard(), "POST /v1/rank", reply{code: 422, body: `{"detail":[{"msg":"field required"}]}`}), nil, gen.FallbackReasonUnavailable},
		{"models listing 500", with(standard(), "GET /v1/models", reply{code: 500}), nil, gen.FallbackReasonUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t, c.replies)
			cfg := testConfig(srv.URL)
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			res, err := score(t, newProvider(t, cfg), testRequest(3))
			if got := wire.ReasonOf(res, err); got != c.want {
				t.Fatalf("reason = %q, want %q (err=%v)", got, c.want, err)
			}
			if err == nil && (res.Disposition != gen.DecisionScoreResultDispositionFallback || len(res.Selections) != 0) {
				t.Fatalf("a failure came back looking like a success: %+v", res)
			}
		})
	}
	t.Run("service down", func(t *testing.T) {
		srv := newServer(t, standard())
		url := srv.URL
		srv.Close()
		res, err := score(t, newProvider(t, testConfig(url)), testRequest(3))
		if got := wire.ReasonOf(res, err); got != gen.FallbackReasonUnavailable {
			t.Fatalf("reason = %q (%v)", got, err)
		}
		if err := newProvider(t, testConfig(url)).Health(context.Background()); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("health: %v", err)
		}
	})
	t.Run("caller deadline", func(t *testing.T) {
		srv := newServer(t, with(standard(), "POST /v1/rank", reply{hang: true}))
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		res, err := newProvider(t, testConfig(srv.URL)).ScoreDecision(ctx, testScope(), testRequest(3))
		if got := wire.ReasonOf(res, err); got != gen.FallbackReasonTimeout {
			t.Fatalf("reason = %q (%v)", got, err)
		}
	})
	t.Run("missing scope", func(t *testing.T) {
		_, err := newProvider(t, testConfig("http://127.0.0.1:1")).ScoreDecision(context.Background(), contracts.Scope{}, testRequest(3))
		if !contracts.HasCode(err, contracts.ErrScopeMissing) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("health", func(t *testing.T) {
		srv := newServer(t, standard())
		p := newProvider(t, testConfig(srv.URL))
		if err := p.Health(context.Background()); err != nil {
			t.Fatal(err)
		}
		sick := newServer(t, with(standard(), "GET /health", reply{code: 503}))
		if err := newProvider(t, testConfig(sick.URL)).Health(context.Background()); !contracts.HasCode(err, contracts.ErrResourceExhausted) {
			t.Fatalf("503 health: %v", err)
		}
	})
}

func TestRejectsNonLoopbackWithoutTLS(t *testing.T) {
	for _, ep := range []string{
		"http://192.168.1.10:8700", "http://10.0.0.5:8700", "http://clm.example.com:8700", "http://clm.internal",
		"http://0.0.0.0:8700", "http://[::]:8700", "http://169.254.169.254", "ftp://127.0.0.1:8700",
		"http://user:pw@127.0.0.1:8700", "http://127.0.0.1:8700/prefix", "http://127.0.0.1:8700/?x=1",
		"unix://host/clm.sock", "unix:///", "unix://",
	} {
		if _, err := New(testConfig(ep)); err == nil {
			t.Errorf("%q was accepted", ep)
		} else if !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%q: code = %v", ep, contracts.CodeOf(err))
		}
	}
	for _, ep := range []string{
		"http://127.0.0.1:8700", "http://localhost:8700", "http://[::1]:8700", "https://clm.example.com", "https://10.0.0.5:8443", "unix:///run/clm/clm.sock",
	} {
		if _, err := New(testConfig(ep)); err != nil {
			t.Errorf("%q refused: %v", ep, err)
		}
	}
	if _, err := New(Config{Model: "m", TLS: TLSConfig{ServerName: "x"}}); err == nil {
		t.Error("tls settings on a plaintext endpoint were accepted")
	}
	if _, err := New(Config{Model: "m", MaxStateTokens: DefaultMaxStateTokens + 1}); err == nil {
		t.Error("a state limit above the encoder limit was accepted")
	}
	if _, err := New(Config{Model: "m", Mode: "chat"}); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if _, err := New(Config{}); err == nil {
		t.Error("a config without a model was accepted")
	}
	if _, err := New(Config{Model: "m"}); err != nil {
		t.Errorf("default endpoint: %v", err)
	}
}

func TestRequestBodiesNeverInErrorsOrLogs(t *testing.T) {
	const canary = "CANARY-7f3a91-do-not-leak"
	req := testRequest(3)
	req.BoundedState["note"] = canary + "-state"
	req.CandidateSet.Candidates[1].Label = canary + "-label"
	req.CandidateSet.Candidates[2].Text = strp(canary + "-text")

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var texts []string
	record := func(res gen.DecisionScoreResult, err error) {
		texts = append(texts, fmt.Sprintf("%v|%+v|%#v|%s", err, err, err, mustJSON(res)))
		var ce *contracts.Error
		if errors.As(err, &ce) {
			texts = append(texts, fmt.Sprintf("%v", ce.Details))
		}
	}
	// every failure body carries the canary, and so does an otherwise valid answer
	for _, code := range []int{400, 404, 422, 500, 501, 503} {
		for _, mode := range []string{ModeRank, ModeSystemOne} {
			srv := newServer(t, map[string]reply{
				"GET /v1/models":     ok(modelsOK),
				"POST /v1/rank":      {code: code, body: `{"detail":"` + canary + `","echo":"` + canary + `-x"}`},
				"POST /v1/systemone": {code: code, body: `{"error":{"message":"` + canary + `"}}`},
			})
			cfg := testConfig(srv.URL)
			cfg.Mode = mode
			res, err := score(t, newProvider(t, cfg, WithLogger(logger)), req)
			if err == nil && res.Disposition == gen.DecisionScoreResultDispositionAccepted {
				t.Fatalf("HTTP %d was accepted", code)
			}
			record(res, err)
		}
	}
	for _, body := range []string{
		`{"model":"clm-latest","ranked":[{"rank":1,"candidate":"` + canary + `-candidate","prob":0.9}]}`,
		`{"model":"` + canary + `-model","ranked":[]}`,
		`not json ` + canary,
	} {
		srv := newServer(t, with(standard(), "POST /v1/rank", ok(body)))
		res, err := score(t, newProvider(t, testConfig(srv.URL), WithLogger(logger)), req)
		record(res, err)
	}
	listing := newServer(t, with(standard(), "GET /v1/models", ok(`{"data":[{"id":"`+canary+`-listed","revision":"`+canary+`"}]}`)))
	res, err := score(t, newProvider(t, testConfig(listing.URL), WithLogger(logger)), req)
	record(res, err)
	big := testRequest(3)
	big.CandidateSet.Candidates[0].Text = strp(canary + strings.Repeat("a", DefaultMaxBodyBytes))
	res, err = score(t, newProvider(t, testConfig(listing.URL), WithLogger(logger)), big)
	record(res, err)
	dead := newServer(t, standard())
	url := dead.URL
	dead.Close()
	res, err = score(t, newProvider(t, testConfig(url), WithLogger(logger)), req)
	record(res, err)

	if !strings.Contains(logs.String(), "path=/v1/rank") {
		t.Fatalf("the debug log has no request records: the check would pass by seeing nothing:\n%.500s", logs.String())
	}
	texts = append(texts, logs.String())
	for _, s := range texts {
		if strings.Contains(s, canary) {
			t.Fatalf("a request or response body leaked: %.300s", s)
		}
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func TestUnsupportedRuntimeIsReportedNotAssumed(t *testing.T) {
	t.Run("constructing the provider assumes nothing and calls nothing", func(t *testing.T) {
		srv := newServer(t, standard())
		p := newProvider(t, testConfig(srv.URL))
		_ = p.Manifest()
		if n := len(srv.requests()); n != 0 {
			t.Fatalf("%d requests before anything was asked", n)
		}
	})
	for name, replies := range map[string]map[string]reply{
		"rank route absent":             with(standard(), "POST /v1/rank", reply{code: 404, body: `{"detail":"Not Found"}`}),
		"rank route not allowed":        with(standard(), "POST /v1/rank", reply{code: 405}),
		"rank not implemented":          with(standard(), "POST /v1/rank", reply{code: 501}),
		"model listing absent":          with(standard(), "GET /v1/models", reply{code: 404, body: `{"detail":"Not Found"}`}),
		"a service that is not the CLM": {},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newServer(t, replies)
			p := newProvider(t, testConfig(srv.URL))
			res, err := score(t, p, testRequest(3))
			if err != nil || wire.ReasonOf(res, err) != gen.FallbackReasonUnsupportedRuntime || len(res.Selections) != 0 {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if perr := p.Probe(context.Background()); !errors.Is(perr, ErrUnsupportedRuntime) {
				t.Fatalf("probe = %v", perr)
			}
		})
	}
	t.Run("Ready names the condition", func(t *testing.T) {
		srv := newServer(t, with(standard(), "GET /v1/models", reply{code: 404}))
		if err := newProvider(t, testConfig(srv.URL)).Ready(context.Background()); !errors.Is(err, ErrUnsupportedRuntime) {
			t.Fatalf("err = %v", err)
		}
		bad := newServer(t, with(standard(), "GET /v1/models", ok(`{"data":[{"id":"x"}]}`)))
		if err := newProvider(t, testConfig(bad.URL)).Ready(context.Background()); !errors.Is(err, ErrModelMismatch) {
			t.Fatalf("err = %v", err)
		}
		good := newServer(t, standard())
		if err := newProvider(t, testConfig(good.URL)).Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
