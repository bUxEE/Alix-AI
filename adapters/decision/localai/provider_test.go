package localai

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

func TestLocalAIDecisionsRequestShapeAndLimits(t *testing.T) {
	t.Run("request shape", func(t *testing.T) {
		srv := newServer(t, answer(goodAnswer))
		p := newProvider(t, testConfig(srv.URL), WithAPIKey("k-test-123"))
		res, err := score(t, p, testRequest(3))
		if err != nil {
			t.Fatal(err)
		}
		if res.Disposition != gen.DecisionScoreResultDispositionAccepted || len(res.Selections) != 1 ||
			res.Selections[0].CandidateID != "c2" || float64(res.Selections[0].CalibratedConfidence) != 0.94 || float64(res.Selections[0].Score) != 0.96 {
			t.Fatalf("result = %+v", res)
		}
		if res.OodStatus != gen.DecisionScoreResultOodStatusInDistribution || *res.ModelID != "clm-8b" || *res.ModelRevision != "rev1" || res.ProviderID != ProviderID {
			t.Fatalf("identity = %+v", res)
		}
		reqs := srv.requests()
		if len(reqs) != 1 {
			t.Fatalf("%d requests", len(reqs))
		}
		r := reqs[0]
		if r.Method != http.MethodPost || r.Path != "/v1/systemone" || r.ContentType != "application/json" || r.Auth != "Bearer k-test-123" {
			t.Fatalf("request = %s %s %q auth=%q", r.Method, r.Path, r.ContentType, r.Auth)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(r.Body, &top); err != nil {
			t.Fatal(err)
		}
		keys := map[string]bool{}
		for k := range top {
			keys[k] = true
		}
		if diff := cmp.Diff(map[string]bool{"model": true, "state": true, "questions": true}, keys); diff != "" {
			t.Fatalf("top-level members (-want +got):\n%s", diff)
		}
		var body struct {
			Model     string
			State     string
			Questions map[string]Question
		}
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "clm-8b" || len(body.Questions) != 1 {
			t.Fatalf("body = %+v", body)
		}
		q := body.Questions["choice"]
		want := map[string]string{"c1": "Option 1: the first one", "c2": "Option 2", "c3": "Option 3"}
		if q.Type != "choice" || q.Instructions == "" || cmp.Diff(want, q.Criteria) != "" {
			t.Fatalf("question = %+v", q)
		}
		// Only the bounded state and the candidates travel: never an identity of the caller.
		var state map[string]any
		if err := json.Unmarshal([]byte(body.State), &state); err != nil {
			t.Fatalf("state is not the JSON string it should be: %v", err)
		}
		if diff := cmp.Diff(map[string]any{"decision_type": "item_selection", "locale": "en", "state": map[string]any{"step": "pick", "n": float64(3)}}, state); diff != "" {
			t.Fatalf("state (-want +got):\n%s", diff)
		}
		for _, id := range []string{"ten1", "app1", "ses1", "cv1", "2026-10-07"} {
			if bytes.Contains(r.Body, []byte(id)) {
				t.Fatalf("the request carries %q", id)
			}
		}
	})

	t.Run("limits of the typed request", func(t *testing.T) {
		options := func(n int) map[string]string {
			m := map[string]string{}
			for i := 0; i < n; i++ {
				m[fmt.Sprintf("o%d", i)] = "x"
			}
			return m
		}
		choice := func(n int) map[string]Question {
			return map[string]Question{"q": {Type: TypeChoice, Criteria: options(n)}}
		}
		cases := []struct {
			name string
			qs   map[string]Question
			ok   bool
		}{
			{"26 options", choice(26), true},
			{"27 options", choice(27), false},
			{"2 options", choice(2), true},
			{"1 option", choice(1), false},
			{"score with 2 levels", map[string]Question{"q": {Type: TypeScore, Criteria: options(2)}}, true},
			{"score with 1 level", map[string]Question{"q": {Type: TypeScore, Criteria: options(1)}}, false},
			{"unknown type", map[string]Question{"q": {Type: "noul", Criteria: options(2)}}, false},
			{"no questions", map[string]Question{}, false},
		}
		for _, c := range cases {
			_, err := encodeSystemOne("m", "state", c.qs, MaxChoiceOptions)
			if (err == nil) != c.ok {
				t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
			}
		}
		many := map[string]Question{}
		for i := 0; i < 64; i++ {
			many[fmt.Sprintf("q%d", i)] = Question{Type: TypeChoice, Criteria: options(2)}
		}
		if _, err := encodeSystemOne("m", "s", many, MaxChoiceOptions); err != nil {
			t.Errorf("64 questions: %v", err)
		}
		many["q64"] = Question{Type: TypeChoice, Criteria: options(2)}
		if _, err := encodeSystemOne("m", "s", many, MaxChoiceOptions); !errors.Is(err, errShape) {
			t.Errorf("65 questions: %v", err)
		}
		if _, err := encodeSystemOne("m", "  ", choice(2), MaxChoiceOptions); !errors.Is(err, errShape) {
			t.Errorf("blank state: %v", err)
		}
		if _, err := encodeSystemOne("m", "s", choice(5), 4); !errors.Is(err, errShape) {
			t.Errorf("operator cap of 4 options: %v", err)
		}
		// The body cap is exact: pad the state until the encoded body is MaxBodyBytes.
		base, err := encodeSystemOne("m", "", map[string]Question{"q": {Type: TypeChoice, Criteria: options(2)}}, 26)
		if err == nil {
			t.Fatal("blank state accepted")
		}
		probe, _ := encodeSystemOne("m", "x", choice(2), MaxChoiceOptions)
		pad := MaxBodyBytes - len(probe) + 1
		if b, err := encodeSystemOne("m", strings.Repeat("x", pad), choice(2), MaxChoiceOptions); err != nil || len(b) != MaxBodyBytes {
			t.Errorf("a body of exactly 64 KiB: len=%d err=%v (base %v)", len(b), err, base)
		}
		if _, err := encodeSystemOne("m", strings.Repeat("x", pad+1), choice(2), MaxChoiceOptions); !errors.Is(err, errShape) {
			t.Errorf("a body of 64 KiB + 1: %v", err)
		}
	})

	t.Run("provider enforces the limits before any connection", func(t *testing.T) {
		srv := newServer(t, answer(goodAnswer))
		p := newProvider(t, testConfig(srv.URL))

		over := testRequest(27)
		res, err := score(t, p, over)
		if err != nil || wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape || res.OodStatus != gen.DecisionScoreResultOodStatusUnsupportedShape || len(res.Selections) != 0 {
			t.Fatalf("27 candidates: res=%+v err=%v", res, err)
		}
		one := testRequest(1)
		if res, err = score(t, p, one); err != nil || wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape {
			t.Fatalf("1 candidate: %v %v", res.FallbackReason, err)
		}
		big := testRequest(3)
		big.CandidateSet.Candidates[0].Text = strp(strings.Repeat("a", MaxBodyBytes))
		if res, err = score(t, p, big); err != nil || wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape {
			t.Fatalf("oversized body: %v %v", res.FallbackReason, err)
		}
		dup := testRequest(3)
		dup.CandidateSet.Candidates[2].CandidateID = "c1"
		if res, err = score(t, p, dup); err != nil || wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape {
			t.Fatalf("duplicate id: %v %v", res.FallbackReason, err)
		}
		if n := len(srv.requests()); n != 0 {
			t.Fatalf("%d requests reached the runtime for shapes it cannot carry", n)
		}

		cfg := testConfig(srv.URL)
		cfg.MaxOptions = 2
		low := newProvider(t, cfg)
		if res, err = score(t, low, testRequest(3)); err != nil || wire.ReasonOf(res, err) != gen.FallbackReasonOodUnsupportedShape {
			t.Fatalf("operator cap of 2: %v %v", res.FallbackReason, err)
		}
		if n := len(srv.requests()); n != 0 {
			t.Fatalf("%d requests", n)
		}
	})

	t.Run("selections", func(t *testing.T) {
		both := `{"answers":{"choice":{"choice":"c2","probabilities":{"c1":0.9,"c2":0.95,"c3":0.01}}}}`
		srv := newServer(t, answer(both))
		p := newProvider(t, testConfig(srv.URL))
		req := testRequest(3)
		req.CandidateSet.MaxSelections = 2
		res, err := score(t, p, req)
		if err != nil || len(res.Selections) != 2 || res.Selections[0].CandidateID != "c2" || res.Selections[1].CandidateID != "c1" {
			t.Fatalf("two strong options: %+v %v", res.Selections, err)
		}
		// a weak runner-up is not selected: it would fail the whole result at the gate
		req.CandidateSet.MaxSelections = 3
		if res, _ = score(t, p, req); len(res.Selections) != 2 {
			t.Fatalf("selections = %+v", res.Selections)
		}
		// without probabilities the confidence stands for both
		conf := newServer(t, answer(`{"answers":{"choice":{"choice":"c3","confidence":0.91}}}`))
		pc := newProvider(t, testConfig(conf.URL))
		if res, err = score(t, pc, testRequest(3)); err != nil || res.Selections[0].CandidateID != "c3" || float64(res.Selections[0].Score) != 0.91 {
			t.Fatalf("confidence only: %+v %v", res.Selections, err)
		}
	})
}

func TestLocalAIOnlyAllowlistedEndpointsCalled(t *testing.T) {
	srv := newServer(t, answer(goodAnswer))
	p := newProvider(t, testConfig(srv.URL), WithAPIKey("secret-key-1"))
	ctx := context.Background()
	if err := p.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(ctx); err != nil {
		// the fake answers the probe with the choice c2, which was not offered (a and b)
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrSchemaInvalid {
			t.Fatal(err)
		}
	}
	if _, err := score(t, p, testRequest(3)); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, e := range AllowedEndpoints() {
		allowed[e] = true
	}
	if diff := cmp.Diff(map[string]bool{"POST /v1/systemone": true, "GET /readyz": true}, allowed); diff != "" {
		t.Fatalf("allowlist (-want +got):\n%s", diff)
	}
	for _, r := range srv.requests() {
		if !allowed[r.Method+" "+r.Path] {
			t.Errorf("the runtime received %s %s", r.Method, r.Path)
		}
		if r.Path == "/readyz" && r.Auth != "" {
			t.Errorf("the key was sent to the anonymous readiness route")
		}
		if r.Path == "/v1/systemone" && r.Auth != "Bearer secret-key-1" {
			t.Errorf("the key was not sent to the authenticated route: %q", r.Auth)
		}
	}

	// Everything else is refused before any I/O, whatever the caller asks.
	before := len(srv.requests())
	for _, c := range [][2]string{
		{http.MethodPost, "/v1/systemone/permute"},
		{http.MethodPost, "/v1/systemone/separate"},
		{http.MethodGet, "/v1/systemone"},
		{http.MethodGet, "/v1/models"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/models/apply"},
		{http.MethodGet, "/v1/systemone?x=1"},
		{http.MethodPost, "/v1/systemone/"},
		{http.MethodPost, "/V1/systemone"},
	} {
		_, err := p.client.Do(ctx, c[0], c[1], []byte("{}"))
		if !contracts.HasCode(err, contracts.ErrInternal) || !errors.Is(err, wire.ErrForbiddenEndpoint) {
			t.Errorf("%s %s: err = %v", c[0], c[1], err)
		}
	}
	if after := len(srv.requests()); after != before {
		t.Fatalf("%d forbidden requests reached the runtime", after-before)
	}
}

func TestModelOrRevisionMismatchFailsClosed(t *testing.T) {
	cases := []struct{ name, body string }{
		{"other model", `{"model":"another-model","answers":{"choice":{"choice":"c2","confidence":0.97}}}`},
		{"other revision", `{"model":"clm-8b","model_revision":"rev2","answers":{"choice":{"choice":"c2","confidence":0.97}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t, answer(c.body))
			p := newProvider(t, testConfig(srv.URL))
			res, err := score(t, p, testRequest(3))
			if err != nil || res.Disposition != gen.DecisionScoreResultDispositionFallback || wire.ReasonOf(res, err) != gen.FallbackReasonModelMismatch || len(res.Selections) != 0 {
				t.Fatalf("res=%+v err=%v", res, err)
			}
		})
	}
	t.Run("matching model and revision are accepted", func(t *testing.T) {
		srv := newServer(t, answer(`{"model":"clm-8b","model_revision":"rev1","answers":{"choice":{"choice":"c2","confidence":0.97}}}`))
		res, err := score(t, newProvider(t, testConfig(srv.URL)), testRequest(3))
		if err != nil || res.Disposition != gen.DecisionScoreResultDispositionAccepted {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("unpinned revision is reported as unverified", func(t *testing.T) {
		cfg := testConfig("http://127.0.0.1:1")
		cfg.ModelRevision = ""
		if rev := newProvider(t, cfg).Manifest().ModelRevision; rev != UnverifiedRevision {
			t.Fatalf("revision = %q", rev)
		}
	})
}

func TestTimeoutAndMalformedOutputMapToFallbackReasons(t *testing.T) {
	hang := func(w http.ResponseWriter, r *http.Request, _ []byte) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}
	cases := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, []byte)
		cfg     func(*Config)
		want    gen.FallbackReason
	}{
		{"slow runtime", hang, func(c *Config) { c.RequestTimeout = 80 * time.Millisecond }, gen.FallbackReasonTimeout},
		{"gateway timeout", status(504, `{"error":{"message":"upstream"}}`), nil, gen.FallbackReasonTimeout},
		{"not json", answer(`<html>oops</html>`), nil, gen.FallbackReasonMalformedOutput},
		{"truncated json", answer(`{"answers":{"choice":{"choice":"c2"`), nil, gen.FallbackReasonMalformedOutput},
		{"two json values", answer(goodAnswer + goodAnswer), nil, gen.FallbackReasonMalformedOutput},
		{"no choice answer", answer(`{"answers":{"other":{"choice":"c2","confidence":0.9}}}`), nil, gen.FallbackReasonMalformedOutput},
		{"choice not offered", answer(`{"answers":{"choice":{"choice":"zzz","confidence":0.99}}}`), nil, gen.FallbackReasonMalformedOutput},
		{"probability for an invented id", answer(`{"answers":{"choice":{"choice":"c1","probabilities":{"c1":0.9,"zzz":0.1}}}}`), nil, gen.FallbackReasonMalformedOutput},
		{"probability out of range", answer(`{"answers":{"choice":{"choice":"c1","probabilities":{"c1":1.5}}}}`), nil, gen.FallbackReasonMalformedOutput},
		{"confidence out of range", answer(`{"answers":{"choice":{"choice":"c1","confidence":-0.2}}}`), nil, gen.FallbackReasonMalformedOutput},
		{"neither probabilities nor confidence", answer(`{"answers":{"choice":{"choice":"c1"}}}`), nil, gen.FallbackReasonMalformedOutput},
		{"wrong answer type", answer(`{"answers":{"choice":{"type":"score","choice":"c1","confidence":0.9}}}`), nil, gen.FallbackReasonMalformedOutput},
		{"response over the cap", answer(`{"answers":{},"pad":"` + strings.Repeat("x", 4096) + `"}`), func(c *Config) { c.MaxResponseBytes = 1024 }, gen.FallbackReasonMalformedOutput},
		{"admission ceiling", status(503, `{"error":{"message":"too many concurrent requests"}}`), nil, gen.FallbackReasonResourceGate},
		{"server error", status(500, `{"error":{"message":"boom"}}`), nil, gen.FallbackReasonUnavailable},
		{"model failed to load", status(500, `{"error":{"message":"rpc error: could not load model"}}`), nil, gen.FallbackReasonUnavailable},
		{"request rejected", status(400, `{"error":{"message":"state must not be blank","type":"invalid_request"}}`), nil, gen.FallbackReasonUnavailable},
		{"redirect", func(w http.ResponseWriter, r *http.Request, _ []byte) {
			http.Redirect(w, r, "http://127.0.0.2/x", http.StatusFound)
		}, nil, gen.FallbackReasonUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t, c.handler)
			cfg := testConfig(srv.URL)
			if c.cfg != nil {
				c.cfg(&cfg)
			}
			res, err := score(t, newProvider(t, cfg), testRequest(3))
			got := wire.ReasonOf(res, err)
			if got != c.want {
				t.Fatalf("reason = %q, want %q (err=%v)", got, c.want, err)
			}
			if err == nil && (res.Disposition != gen.DecisionScoreResultDispositionFallback || len(res.Selections) != 0) {
				t.Fatalf("a failure came back looking like a success: %+v", res)
			}
		})
	}

	t.Run("runtime down", func(t *testing.T) {
		srv := newServer(t, answer(goodAnswer))
		url := srv.URL
		srv.Close()
		res, err := score(t, newProvider(t, testConfig(url)), testRequest(3))
		if got := wire.ReasonOf(res, err); got != gen.FallbackReasonUnavailable {
			t.Fatalf("reason = %q, err=%v", got, err)
		}
	})
	t.Run("caller deadline", func(t *testing.T) {
		srv := newServer(t, hang)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		res, err := newProvider(t, testConfig(srv.URL)).ScoreDecision(ctx, testScope(), testRequest(3))
		if got := wire.ReasonOf(res, err); got != gen.FallbackReasonTimeout {
			t.Fatalf("reason = %q, err=%v", got, err)
		}
	})
	t.Run("missing scope", func(t *testing.T) {
		_, err := newProvider(t, testConfig("http://127.0.0.1:1")).ScoreDecision(context.Background(), contracts.Scope{}, testRequest(3))
		if !contracts.HasCode(err, contracts.ErrScopeMissing) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRejectsNonLoopbackWithoutTLS(t *testing.T) {
	refused := []string{
		"http://192.168.1.10:8080", "http://10.0.0.5:8080", "http://example.com:8080", "http://localai.internal",
		"http://0.0.0.0:8080", "http://[::]:8080", "http://169.254.169.254", "http://8.8.8.8",
		"ftp://127.0.0.1:8080", "http://user:pw@127.0.0.1:8080", "http://127.0.0.1:8080/prefix", "http://127.0.0.1:8080/?x=1",
		"unix://host/path.sock", "unix:///", "unix://",
	}
	for _, ep := range refused {
		cfg := testConfig(ep)
		if _, err := New(cfg); err == nil {
			t.Errorf("%q was accepted", ep)
		} else if !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("%q: code = %v", ep, contracts.CodeOf(err))
		}
	}
	for _, ep := range []string{
		"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080", "http://127.5.5.5:1",
		"https://decisions.example.com", "https://10.0.0.5:8443", "unix:///var/run/localai.sock",
	} {
		if _, err := New(testConfig(ep)); err != nil {
			t.Errorf("%q refused: %v", ep, err)
		}
	}
	// TLS settings belong to an https endpoint, and a key is a reference, never a value.
	if _, err := New(Config{Endpoint: "http://127.0.0.1:8080", Model: "m", TLS: TLSConfig{ServerName: "x"}}); err == nil {
		t.Error("tls settings on a plaintext endpoint were accepted")
	}
	if _, err := New(Config{Model: "m", APIKeyRef: "sk-literal-key-value"}); err == nil || strings.Contains(err.Error(), "sk-literal") {
		t.Errorf("a literal key: %v", err)
	}
	if _, err := New(Config{}); err == nil {
		t.Error("a config without a model was accepted")
	}
	// The default endpoint is loopback.
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

	// A hostile runtime that echoes the request body in every kind of failure.
	echo := func(code int) func(http.ResponseWriter, *http.Request, []byte) {
		return func(w http.ResponseWriter, _ *http.Request, body []byte) {
			w.WriteHeader(code)
			_, _ = w.Write(body)
		}
	}
	var texts []string
	record := func(res gen.DecisionScoreResult, err error) {
		texts = append(texts, fmt.Sprintf("%v|%+v|%#v|%s", err, err, err, mustJSON(res)))
		var ce *contracts.Error
		if errors.As(err, &ce) {
			texts = append(texts, fmt.Sprintf("%v", ce.Details))
		}
	}
	for _, code := range []int{200, 400, 404, 500, 501, 503} {
		srv := newServer(t, echo(code))
		res, err := score(t, newProvider(t, testConfig(srv.URL), WithLogger(logger), WithAPIKey("key-"+canary)), req)
		if err == nil && res.Disposition == gen.DecisionScoreResultDispositionAccepted {
			t.Fatalf("HTTP %d echo was accepted", code)
		}
		record(res, err)
	}
	// echoed inside an otherwise valid answer
	bad := newServer(t, func(w http.ResponseWriter, _ *http.Request, body []byte) {
		fmt.Fprintf(w, `{"answers":{"choice":{"choice":%q,"confidence":0.9}}}`, canary+"-choice")
	})
	res, err := score(t, newProvider(t, testConfig(bad.URL), WithLogger(logger)), req)
	record(res, err)
	// the shape verdicts
	big := req
	big.CandidateSet = gen.CandidateSet{Candidates: append([]gen.CandidateSetCandidate(nil), req.CandidateSet.Candidates...), MaxSelections: 1, BuiltAt: req.CandidateSet.BuiltAt}
	big.CandidateSet.Candidates[0].Text = strp(canary + strings.Repeat("a", MaxBodyBytes))
	res, err = score(t, newProvider(t, testConfig(bad.URL), WithLogger(logger)), big)
	record(res, err)
	// a connection failure and a timeout
	dead := newServer(t, answer(goodAnswer))
	url := dead.URL
	dead.Close()
	res, err = score(t, newProvider(t, testConfig(url), WithLogger(logger)), req)
	record(res, err)

	if logs.Len() == 0 || !strings.Contains(logs.String(), "path=/v1/systemone") {
		t.Fatalf("the debug log is empty: the check would pass by seeing nothing:\n%s", logs.String())
	}
	texts = append(texts, logs.String())
	for _, s := range texts {
		if strings.Contains(s, canary) {
			t.Fatalf("a request or response body leaked: %.300s", s)
		}
	}
	// The key is never printed either.
	p := newProvider(t, testConfig("http://127.0.0.1:1"), WithAPIKey("key-"+canary))
	if s := fmt.Sprintf("%v %+v %#v", p, p, p.client); strings.Contains(s, canary) {
		t.Fatalf("the key leaked: %s", s)
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
		srv := newServer(t, answer(goodAnswer))
		p := newProvider(t, testConfig(srv.URL))
		_ = p.Manifest()
		if n := len(srv.requests()); n != 0 {
			t.Fatalf("%d requests before anything was asked", n)
		}
	})
	runtimes := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, []byte)
	}{
		{"route absent (404)", status(404, `{"message":"Not Found"}`)},
		{"method not allowed (405)", status(405, `{"message":"Method Not Allowed"}`)},
		{"backend does not support decisions (501)", status(501, `{"error":{"message":"backend does not support decisions","type":"not_implemented"}}`)},
		{"model is not a decisions model (400)", status(400, `{"error":{"message":"unsupported model: it does not declare the decisions usecase","type":"invalid_request"}}`)},
	}
	for _, rt := range runtimes {
		t.Run(rt.name, func(t *testing.T) {
			srv := newServer(t, rt.handler)
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
	t.Run("a missing model is unavailable, not unsupported", func(t *testing.T) {
		srv := newServer(t, status(404, `{"error":{"message":"model clm-8b not found"}}`))
		res, err := score(t, newProvider(t, testConfig(srv.URL)), testRequest(3))
		if got := wire.ReasonOf(res, err); got != gen.FallbackReasonUnavailable {
			t.Fatalf("reason = %q (%v)", got, err)
		}
	})
	t.Run("a runtime that does support it is not reported as unsupported", func(t *testing.T) {
		srv := newServer(t, answer(`{"answers":{"choice":{"choice":"a","confidence":0.9}}}`))
		if err := newProvider(t, testConfig(srv.URL)).Probe(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
