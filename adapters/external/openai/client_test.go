package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestChatStructuredOutputStrictSchemaRequestShape(t *testing.T) {
	answer := `{"capability_id":"booking.create","fields":{"date":"2026-10-12","party_size":null,"tags":null},"entity_refs":[],"intent":"task","uncertainty":{"overall":0.8}}`
	r := newRig(t, replyJSON(textResponseWith(answer)), nil)
	req := userReq("book a table for friday")
	req.Output = providers.SchemaConstraint(json.RawMessage(compactTaskProposal))
	req.MaxToks = 300
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	reqs := r.api.requests()
	if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Path != "/v1/responses" {
		t.Fatalf("requests %+v", reqs)
	}
	body := reqs[0].json(t)
	if body["model"] != "gpt-5-mini" || body["store"] != false || body["max_output_tokens"] != float64(300) {
		t.Fatalf("body %v", body)
	}
	if _, has := body["stream"]; has {
		t.Fatal("stream set on a non-streaming call")
	}
	format := body["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true || format["name"] != structuredFormatName {
		t.Fatalf("format %v", format)
	}
	schema, _ := json.Marshal(format["schema"])
	if v := CheckStrict(schema); len(v) > 0 {
		t.Fatalf("the schema sent is not strict: %v", v)
	}
	input := body["input"].([]any)
	if len(input) != 2 || input[0].(map[string]any)["role"] != "developer" || input[1].(map[string]any)["role"] != "user" {
		t.Fatalf("input %v", input)
	}
	// The answer is restored to the shape of the schema the caller owns.
	if err := validates(t, compileSchema(t, compactTaskProposal), res.JSON); err != nil {
		t.Fatalf("restored JSON fails the caller's schema: %v\n%s", err, res.JSON)
	}
	if res.FinishReason != providers.FinishStop || res.Model != "gpt-5-mini-2025-08-07" || res.TraceID != "resp_0123456789abcdef" {
		t.Fatalf("result %+v", res)
	}
}

func TestToolCallsReturnedAsProposalsOnly(t *testing.T) {
	const withCall = `{
	  "id": "resp_1", "status": "completed", "model": "gpt-5-mini",
	  "output": [
	    {"type": "function_call", "id": "fc_1", "call_id": "call_abc", "name": "booking_create", "arguments": "{\"date\":\"2026-10-12\"}"}
	  ],
	  "usage": {"input_tokens": 10, "output_tokens": 5}
	}`
	tool := providers.ToolSpec{Name: "booking_create", Description: "Propose a booking.",
		Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["date"],"properties":{"date":{"type":"string"}}}`)}
	req := userReq("book friday")
	req.Tools = []providers.ToolSpec{tool}
	r := newRig(t, replyJSON(withCall), nil)
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_abc" || res.ToolCalls[0].Name != "booking_create" ||
		string(res.ToolCalls[0].Arguments) != `{"date":"2026-10-12"}` || res.FinishReason != providers.FinishToolCalls {
		t.Fatalf("result %+v", res)
	}
	// One request, no follow-up carrying a function_call_output: nothing ran.
	reqs := r.api.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	tools := reqs[0].json(t)["tools"].([]any)
	first := tools[0].(map[string]any)
	if len(tools) != 1 || first["type"] != "function" || first["strict"] != true || first["name"] != "booking_create" {
		t.Fatalf("tools %v", tools)
	}
	if strings.Contains(string(reqs[0].Body), "function_call_output") {
		t.Fatal("a tool result was sent")
	}

	// A call to a tool that was not offered rejects the whole answer.
	other := strings.Replace(withCall, "booking_create", "delete_everything", 1)
	r = newRig(t, replyJSON(other), nil)
	_, err = r.client.Generate(context.Background(), testScope(), req)
	wantCode(t, err, contracts.ErrSchemaInvalid)

	// A call while tools are off is rejected too.
	r = newRig(t, replyJSON(withCall), nil)
	_, err = r.client.Generate(context.Background(), testScope(), userReq("hello"))
	wantCode(t, err, contracts.ErrSchemaInvalid)

	// A hosted-tool item in the answer was never requested.
	hosted := strings.Replace(withCall, `"type": "function_call", "id": "fc_1"`, `"type": "web_search_call", "id": "ws_1"`, 1)
	r = newRig(t, replyJSON(hosted), nil)
	_, err = r.client.Generate(context.Background(), testScope(), req)
	wantCode(t, err, contracts.ErrSchemaInvalid)
}

const sseText = `event: response.created
data: {"type":"response.created","response":{"id":"resp_s1","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hel"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"lo."}

event: response.output_text.done
data: {"type":"response.output_text.done","text":"Hello."}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_s1","status":"completed","model":"gpt-5-mini-2025-08-07","usage":{"input_tokens":200,"output_tokens":40}}}

`

func sseHandler(body string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}
}

func TestStreamParsesDeltasAndUsage(t *testing.T) {
	r := newRig(t, sseHandler(sseText), nil)
	st, err := r.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	var kinds []providers.DeltaKind
	var text string
	var last providers.RichChunk
	for {
		c, err := st.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if c.Seq != len(kinds)+1 {
			t.Fatalf("seq %d at position %d", c.Seq, len(kinds))
		}
		kinds = append(kinds, c.Kind)
		text += c.Text
		last = c
	}
	_ = st.Close()
	if fmt.Sprint(kinds) != "[text text done]" || text != "Hello." {
		t.Fatalf("kinds %v text %q", kinds, text)
	}
	if last.Usage == nil || last.Usage.ToksIn != 200 || last.Usage.ToksOut != 40 || !last.Usage.Estimated ||
		last.Usage.CostMicros != 50+80 || last.Usage.Currency != "USD" {
		t.Fatalf("usage %+v", last.Usage)
	}
	if last.FinishReason != providers.FinishStop || last.Model != "gpt-5-mini-2025-08-07" || last.Timings.TimeToFirstToken <= 0 {
		t.Fatalf("final %+v", last)
	}
	body := r.api.requests()[0].json(t)
	if body["stream"] != true || body["store"] != false {
		t.Fatalf("body %v", body)
	}

	// The shared collector accepts the stream, and a tool-call stream too.
	const sseTool = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_9","name":"booking_create","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"date\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"2026-10-12\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"date\":\"2026-10-12\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_s2","status":"completed","model":"gpt-5-mini","usage":{"input_tokens":20,"output_tokens":9}}}

`
	req := userReq("book")
	req.Tools = []providers.ToolSpec{{Name: "booking_create", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["date"],"properties":{"date":{"type":"string"}}}`)}}
	r = newRig(t, sseHandler(sseTool), nil)
	st, err = r.client.GenerateStream(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	res, err := providers.CollectRich(st, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "call_9" || string(res.ToolCalls[0].Arguments) != `{"date":"2026-10-12"}` ||
		res.FinishReason != providers.FinishToolCalls {
		t.Fatalf("collected %+v", res)
	}

	// A stream that ends before response.completed is an error, not a result.
	cut := sseText[:strings.Index(sseText, "event: response.completed")]
	r = newRig(t, sseHandler(cut), nil)
	st, err = r.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providers.CollectRich(st, userReq("hi")); err == nil {
		t.Fatal("a truncated stream was collected")
	} else {
		wantCode(t, err, contracts.ErrUnavailable)
	}
}

func TestUsageAndCostMappedToMicroUnits(t *testing.T) {
	for name, tc := range map[string]struct {
		model    string
		cost     int64
		currency string
		in, out  int
		prices   *providers.PriceTable
	}{
		// gpt-5-mini: 250000 per million in, 2000000 per million out.
		"mini":    {"gpt-5-mini", 250 + 1000, "USD", 1000, 500, nil},
		"nano":    {"gpt-5-nano", 50 + 200, "USD", 1000, 500, nil},
		"rounded": {"gpt-5-mini", 1 + 2, "USD", 1, 1, nil}, // each unit rounds up
		"override": {"gpt-5-mini", 5000, "EUR", 1000, 500, &providers.PriceTable{Currency: "EUR",
			ToksIn: &providers.Price{PerUnits: 1000, MicroUnits: 1000}, ToksOut: &providers.Price{PerUnits: 500, MicroUnits: 4000}}},
		"unknown model has no price": {"acme-9", 0, "", 1000, 500, nil},
	} {
		t.Run(name, func(t *testing.T) {
			resp := strings.NewReplacer(`"input_tokens": 1000`, fmt.Sprintf(`"input_tokens": %d`, tc.in),
				`"output_tokens": 500`, fmt.Sprintf(`"output_tokens": %d`, tc.out)).Replace(textResponse)
			r := newRig(t, replyJSON(resp), func(c *Config) { c.Model, c.Prices = tc.model, tc.prices })
			res, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
			if err != nil {
				t.Fatal(err)
			}
			u := res.Usage
			if u.ToksIn != tc.in || u.ToksOut != tc.out || u.CostMicros != tc.cost || u.Currency != tc.currency || !u.Estimated {
				t.Fatalf("usage %+v, want cost %d %s", u, tc.cost, tc.currency)
			}
			if err := u.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A response without usage cannot be metered: it is not a result.
	noUsage := strings.Replace(textResponse, `"usage"`, `"usage_"`, 1)
	r := newRig(t, replyJSON(noUsage), nil)
	_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrSchemaInvalid)
}

func TestRateLimitMappedWithRetryAfter(t *testing.T) {
	limited := func(retryAfter string) func(int, http.ResponseWriter, *http.Request) {
		return func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", retryAfter)
			w.Header().Set("x-ratelimit-remaining-requests", "0")
			w.Header().Set("x-ratelimit-reset-requests", "6m0s")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached for gpt-5-mini","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
		}
	}
	// A wait beyond the cap is not sat out: the caller gets the retry-after.
	r := newRig(t, limited("120"), nil)
	_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrRateLimited)
	info, ok := providers.RateLimitFrom(err)
	if !ok || info.RetryAfter != 120*time.Second || info.Scope != "requests" {
		t.Fatalf("info %+v %v", info, ok)
	}
	if ce.Details["provider_error_kind"] != string(pv.ProviderErrorKindRateLimited) || ce.Details["sent"] != true {
		t.Fatalf("details %v", ce.Details)
	}
	if n := len(r.api.requests()); n != 1 || len(r.sleeps.all()) != 0 {
		t.Fatalf("%d requests, sleeps %v", n, r.sleeps.all())
	}
	if strings.Contains(err.Error(), "gpt-5-mini") {
		t.Fatalf("the provider's message was copied: %v", err)
	}

	// A short wait is sat out (bounded, retry-after as the minimum) and retried.
	r = newRig(t, func(n int, w http.ResponseWriter, req *http.Request) {
		if n < 3 {
			limited("2")(n, w, req)
			return
		}
		replyJSON(textResponse)(n, w, req)
	}, nil)
	res, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	if err != nil || res.Text() != "Hello there." {
		t.Fatalf("%v %+v", err, res)
	}
	got := r.sleeps.all()
	if len(r.api.requests()) != 3 || len(got) != 2 || got[0] != 2*time.Second || got[1] != 2*time.Second {
		t.Fatalf("requests %d sleeps %v", len(r.api.requests()), got)
	}

	// Retries are bounded: a provider that keeps refusing ends in rate_limited.
	r = newRig(t, limited("1"), nil)
	_, err = r.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrRateLimited)
	if n := len(r.api.requests()); n != 3 { // the first try and two retries
		t.Fatalf("%d requests", n)
	}

	// Quota exhaustion is not a rate limit and is never retried.
	r = newRig(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota"}}`)
	}, nil)
	_, err = r.client.Generate(context.Background(), testScope(), userReq("hi"))
	ce = wantCode(t, err, contracts.ErrResourceExhausted)
	if ce.Details["provider_error_kind"] != string(pv.ProviderErrorKindQuotaExceeded) || len(r.api.requests()) != 1 {
		t.Fatalf("details %v, %d requests", ce.Details, len(r.api.requests()))
	}
}

func TestErrorMapping(t *testing.T) {
	const echo = "my private prompt text and " + fixtureKey
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   contracts.ErrCode
		kind   pv.ProviderErrorKind
		sent   bool
		calls  int
	}{
		{"401", 401, `{"error":{"type":"invalid_request_error","code":"invalid_api_key"}}`, contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true, 1},
		{"403", 403, `{"error":{"type":"x","code":"unsupported_country_region_territory"}}`, contracts.ErrPermissionDenied, pv.ProviderErrorKindPermissionDenied, true, 1},
		{"404 model", 404, `{"error":{"code":"model_not_found"}}`, contracts.ErrValidationFailed, pv.ProviderErrorKindModelNotFound, true, 1},
		{"400 context", 400, `{"error":{"code":"context_length_exceeded"}}`, contracts.ErrValidationFailed, pv.ProviderErrorKindContextTooLarge, true, 1},
		{"400 other", 400, `{"error":{"code":"invalid_value"}}`, contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, 1},
		{"400 not json", 400, `<html>` + echo + `</html>`, contracts.ErrValidationFailed, pv.ProviderErrorKindBadRequest, true, 1},
		{"408", 408, `{}`, contracts.ErrTimeout, pv.ProviderErrorKindTimeout, true, 1},
		{"500", 500, `{"error":{"code":"server_error"}}`, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, 1},
		{"502", 502, ``, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, 1},
		{"504", 504, ``, contracts.ErrTimeout, pv.ProviderErrorKindTimeout, true, 1},
		{"503 retried then reported", 503, `{"error":{"type":"service_unavailable_error","code":"server_is_overloaded"}}`, contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, 3},
		{"302 is not followed", 302, ``, contracts.ErrUnavailable, pv.ProviderErrorKindMalformedResponse, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
				// The body repeats the prompt and the Authorization header.
				w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `%s`, strings.Replace(tc.body, `}}`, `,"message":"`+echo+req.Header.Get("Authorization")+`"}}`, 1))
				_, _ = io.WriteString(w, echo)
			}, nil)
			_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
			ce := wantCode(t, err, tc.code)
			if ce.Details["provider_error_kind"] != string(tc.kind) || ce.Details["sent"] != tc.sent || ce.Details["http_status"] != tc.status {
				t.Fatalf("details %v", ce.Details)
			}
			if n := len(r.api.requests()); n != tc.calls {
				t.Fatalf("%d requests, want %d", n, tc.calls)
			}
			dump := fmt.Sprintf("%v|%+v|%#v|%v", err, err, ce, ce.Details)
			for _, leak := range []string{"private prompt", fixtureKey, "Bearer"} {
				if strings.Contains(dump, leak) {
					t.Fatalf("the error repeats %q: %s", leak, dump)
				}
			}
		})
	}

	// Transport failures: a refused dial proves nothing was sent.
	api := newFakeAPI(t, nil)
	dead := api.srv.URL
	api.srv.Close()
	r := newRigFor(t, api, testConfig(dead), dead)
	_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrUnavailable)
	if ce.Details["sent"] != false || ce.Details["provider_error_kind"] != string(pv.ProviderErrorKindUnavailable) {
		t.Fatalf("details %v", ce.Details)
	}

	// A deadline is timeout and keeps context.DeadlineExceeded for errors.Is.
	r = newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) { <-req.Context().Done() }, func(c *Config) { c.RequestTimeout = 80 * time.Millisecond })
	_, err = r.client.Generate(context.Background(), testScope(), userReq("hi"))
	ce = wantCode(t, err, contracts.ErrTimeout)
	if !errors.Is(err, context.DeadlineExceeded) || ce.Details["provider_error_kind"] != string(pv.ProviderErrorKindTimeout) {
		t.Fatalf("%v %v", err, ce.Details)
	}

	// Responses that are not an answer.
	for name, body := range map[string]string{
		"not json":       `nope`,
		"failed":         `{"id":"r","status":"failed","error":{"code":"server_error","message":"` + echo + `"}}`,
		"unknown status": `{"id":"r","status":"queued"}`,
		"incomplete":     `{"id":"r","status":"incomplete","incomplete_details":{"reason":"surprise"}}`,
	} {
		r := newRig(t, replyJSON(body), nil)
		_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
		if err == nil || strings.Contains(err.Error(), "private prompt") {
			t.Errorf("%s: %v", name, err)
		}
	}
	r = newRig(t, replyJSON(`{"id":"r","status":"failed","error":{"code":"rate_limit_exceeded"}}`), nil)
	_, err = r.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrRateLimited)

	// A response cut at the cap is malformed, not partially used.
	r = newRig(t, replyJSON(textResponse), func(c *Config) { c.MaxResponseBytes = 64 })
	_, err = r.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrSchemaInvalid)
}

func TestOnlyAllowlistedEndpointsAndHostCalled(t *testing.T) {
	want := []string{"POST /v1/responses", "POST /v1/embeddings", "GET /v1/models"}
	if got := AllowedEndpoints(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routes %v", got)
	}
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-5-mini"},{"id":"gpt-5-nano"}]}`)
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"model":"text-embedding-3-small","data":[{"index":0,"embedding":[0.5,0.25]}],"usage":{"prompt_tokens":3}}`)
		default:
			_, _ = io.WriteString(w, textResponse)
		}
	}, func(c *Config) { c.EmbeddingModel = "text-embedding-3-small" })
	ctx := context.Background()
	if _, err := r.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if err := r.client.CheckModel(ctx, testScope()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.Embed(ctx, testScope(), providers.EmbedRequest{Inputs: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if h, err := r.client.Health(ctx); err != nil || h.Status != providers.HealthOK {
		t.Fatalf("%+v %v", h, err)
	}
	seen := map[string]bool{}
	for _, c := range r.api.requests() {
		seen[c.Method+" "+c.Path] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("route %s never used", w)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("routes used %v", seen)
	}

	// Anything else is refused before any I/O, whatever the method.
	n := len(r.api.requests())
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/files"}, {"POST", "/v1/assistants"}, {"POST", "/v1/conversations"}, {"POST", "/v1/vector_stores"},
		{"POST", "/v1/responses/resp_1/cancel"}, {"GET", "/v1/responses/resp_1"}, {"DELETE", "/v1/models/gpt-5-mini"},
		{"GET", "/v1/responses"}, {"POST", "/v1/models"}, {"POST", "/v1/responses?x=1"}, {"POST", "/v1/%72esponses"},
	} {
		req, _ := http.NewRequest(tc.method, r.api.srv.URL+tc.path, strings.NewReader("{}"))
		if resp, err := r.client.hc.Do(req); err == nil {
			resp.Body.Close()
			t.Errorf("%s %s reached the network", tc.method, tc.path)
		}
	}
	if len(r.api.requests()) != n {
		t.Fatal("a refused route hit the server")
	}

	// A host that is not on the egress allowlist is never dialled, even when
	// the configuration names it.
	other := newFakeAPI(t, replyJSON(textResponse))
	cfg := testConfig(other.srv.URL)
	r2 := newRigFor(t, other, cfg, r.api.srv.URL) // allowlist lists the OTHER server
	_, err := r2.client.Generate(ctx, testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrPermissionDenied)
	if ce.Details["sent"] != false || len(other.requests()) != 0 {
		t.Fatalf("details %v, %d requests reached the unlisted host", ce.Details, len(other.requests()))
	}
	// An empty allowlist denies everything.
	if _, err := New(testConfig("https://api.openai.com"), nil, r.keys); err == nil {
		t.Fatal("a client was built without a dialer")
	}
}

func TestAPIKeyNeverLoggedOrReturned(t *testing.T) {
	leaky := func(_ int, w http.ResponseWriter, req *http.Request) {
		// The provider echoes the Authorization header everywhere it can.
		w.Header().Set("X-Echo", req.Header.Get("Authorization"))
		if req.URL.Path == "/v1/models" {
			w.WriteHeader(401)
		} else {
			w.WriteHeader(400)
		}
		fmt.Fprintf(w, `{"error":{"message":"bad key %s","type":"invalid_request_error","code":"invalid_value"}}`, req.Header.Get("Authorization"))
	}
	r := newRig(t, leaky, nil)
	ctx := context.Background()
	var errs []error
	_, err := r.client.Generate(ctx, testScope(), userReq("hi"))
	errs = append(errs, err)
	st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
	if st != nil {
		t.Fatal("a stream was returned for a 400")
	}
	errs = append(errs, err)
	_, err = r.client.Models(ctx, testScope())
	errs = append(errs, err)
	h, herr := r.client.Health(ctx)
	errs = append(errs, herr)

	// A successful call, so its result is checked as well.
	ok := newRig(t, replyJSON(textResponse), nil)
	res, err := ok.client.Generate(ctx, testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	resJSON, _ := json.Marshal(res)

	surfaces := []string{r.logs.String(), ok.logs.String(), string(resJSON), fmt.Sprintf("%+v %#v", h, h),
		r.client.String(), fmt.Sprintf("%+v %v %#v", r.client.cfg, r.client.cfg, r.client.cfg)}
	for _, e := range errs {
		if e != nil {
			surfaces = append(surfaces, fmt.Sprintf("%v|%+v|%#v", e, e, e))
			var ce *contracts.Error
			if errors.As(e, &ce) {
				b, _ := json.Marshal(ce)
				surfaces = append(surfaces, string(b))
			}
		}
	}
	for i, s := range surfaces {
		if strings.Contains(s, fixtureKey) {
			t.Errorf("surface %d holds the key: %.200s", i, s)
		}
	}
	// The key did travel, once per call, in the Authorization header only.
	for _, c := range append(r.api.requests(), ok.api.requests()...) {
		if c.Path == "/v1/models" && c.Header.Get("Authorization") == "" {
			continue // the anonymous health probe
		}
		if c.Header.Get("Authorization") != "Bearer "+fixtureKey {
			t.Errorf("request to %s: Authorization %q", c.Path, c.Header.Get("Authorization"))
		}
		if strings.Contains(string(c.Body), fixtureKey) {
			t.Errorf("the key is in a request body")
		}
	}
	if ok.logs.Len() == 0 || !strings.Contains(ok.logs.String(), `"tok_in":1000`) || strings.Contains(ok.logs.String(), "_tokens") {
		t.Errorf("debug record missing or uses a redacted key name: %s", ok.logs.String())
	}

	// Key resolution fails closed: nothing is sent, no other credential is tried.
	r2 := newRig(t, replyJSON(textResponse), nil)
	r2.keys.err = contracts.New(contracts.ErrPermissionDenied, "credentials: the credential is revoked")
	_, err = r2.client.Generate(ctx, testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrPermissionDenied)
	if ce.Details["sent"] != false || len(r2.api.requests()) != 0 {
		t.Fatalf("details %v, requests %d", ce.Details, len(r2.api.requests()))
	}
	r2.keys.err, r2.keys.key = nil, "has a space"
	if _, err = r2.client.Generate(ctx, testScope(), userReq("hi")); err == nil || len(r2.api.requests()) != 0 {
		t.Fatalf("a malformed key was sent: %v", err)
	}
	// Rotation takes effect on the next call (nothing is cached).
	r2.keys.key = "rotated-fixture-credential-0002"
	if _, err = r2.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if got := r2.api.requests()[0].Header.Get("Authorization"); got != "Bearer rotated-fixture-credential-0002" {
		t.Fatalf("Authorization %q", got)
	}
}

func TestProviderNativeAgentFeaturesNotRequested(t *testing.T) {
	forbidden := []string{"previous_response_id", "conversation", "background", "prompt", "include", "max_tool_calls",
		"metadata", "user", "safety_identifier", "prompt_cache_key", "service_tier", "context_management"}
	tool := providers.ToolSpec{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["q"],"properties":{"q":{"type":"string"}}}`)}
	var reqs []captured
	for name, req := range map[string]providers.RichRequest{
		"text": userReq("hi"),
		"structured": func() providers.RichRequest {
			q := userReq("hi")
			q.Output = providers.SchemaConstraint(json.RawMessage(compactTaskProposal))
			return q
		}(),
		"tools": func() providers.RichRequest {
			q := userReq("hi")
			q.Tools = []providers.ToolSpec{tool}
			q.ToolChoice = providers.ToolChoiceNone
			return q
		}(),
		"tool round trip": {Messages: []providers.RichMessage{
			{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("find it")}},
			{Role: providers.MsgAssistant, ToolCalls: []providers.ToolCallProposal{{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}}},
			{Role: providers.MsgTool, ToolCallID: "call_1", Parts: []providers.ContentPart{providers.TextPart(`{"found":true}`)}},
		}, Tools: []providers.ToolSpec{tool}},
	} {
		r := newRig(t, replyJSON(strings.Replace(textResponse, "Hello there.", `{\"capability_id\":null,\"fields\":{},\"entity_refs\":[],\"intent\":\"help\",\"uncertainty\":{\"overall\":0.5}}`, 1)), nil)
		if _, err := r.client.Generate(context.Background(), testScope(), req); err != nil {
			if name != "tools" && name != "tool round trip" {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if n := len(r.api.requests()); n != 1 {
			t.Fatalf("%s: %d requests for one Generate (no automatic follow-up)", name, n)
		}
		reqs = append(reqs, r.api.requests()...)
	}
	rs := newRig(t, sseHandler(sseText), nil)
	st, err := rs.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	reqs = append(reqs, rs.api.requests()...)

	for i, c := range reqs {
		if c.Path != "/v1/responses" {
			t.Errorf("request %d went to %s", i, c.Path)
		}
		body := c.json(t)
		if body["store"] != false {
			t.Errorf("request %d: store = %v", i, body["store"])
		}
		for _, k := range forbidden {
			if _, has := body[k]; has {
				t.Errorf("request %d sets %q", i, k)
			}
		}
		if tools, ok := body["tools"].([]any); ok {
			for _, x := range tools {
				if x.(map[string]any)["type"] != "function" {
					t.Errorf("request %d offers a %v tool", i, x.(map[string]any)["type"])
				}
			}
		}
		raw := string(c.Body)
		for _, bad := range []string{"file_search", "web_search", "code_interpreter", "mcp", "computer", "vector_store"} {
			if strings.Contains(raw, bad) {
				t.Errorf("request %d mentions %s", i, bad)
			}
		}
	}

	// What the contract cannot express is refused, not dropped.
	r := newRig(t, replyJSON(textResponse), nil)
	ctx := context.Background()
	img := userReq("hi")
	img.Messages[1].Parts = append(img.Messages[1].Parts, providers.ImageRef("img_1", "image/png"))
	stop := userReq("hi")
	stop.Stop = []string{"END"}
	warm := userReq("hi")
	warm.Temperature = 0.7
	gram := userReq("hi")
	gram.Output = providers.GrammarConstraint("root ::= \"a\"")
	for name, bad := range map[string]providers.RichRequest{"image": img, "stop": stop, "temperature on a reasoning model": warm, "grammar": gram} {
		_, err := r.client.Generate(ctx, testScope(), bad)
		ce := wantCode(t, err, contracts.ErrValidationFailed)
		if _, ok := providers.IsUnsupported(err); !ok || ce.Details["sent"] != false {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(r.api.requests()) != 0 {
		t.Fatal("an unsupported request was sent")
	}
	// Streaming a conversion-needing schema is refused; Generate handles it.
	sc := userReq("hi")
	sc.Output = providers.SchemaConstraint(json.RawMessage(compactTaskProposal))
	if _, err := r.client.GenerateStream(ctx, testScope(), sc); err == nil {
		t.Fatal("streamed a schema that needs restoration")
	}
}

func TestCancellationStopsStream(t *testing.T) {
	released := make(chan struct{})
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"one\"}\n\n")
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"two\"}\n\n")
		fl.Flush()
		<-req.Context().Done() // the provider keeps the stream open
		close(released)
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, want := range []string{"one", "two"} {
		c, err := st.Recv()
		if err != nil || c.Text != want {
			t.Fatalf("%+v %v", c, err)
		}
	}
	type recvResult struct {
		c   providers.RichChunk
		err error
	}
	out := make(chan recvResult, 1)
	go func() {
		c, err := st.Recv() // blocks: no event is coming
		out <- recvResult{c, err}
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case res := <-out:
		if !errors.Is(res.err, context.Canceled) || res.c != (providers.RichChunk{}) {
			t.Fatalf("Recv after cancel: %+v %v", res.c, res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not return after the context was cancelled")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection to the provider stayed open")
	}
	// No further chunk is ever produced.
	if c, err := st.Recv(); err == nil || c != (providers.RichChunk{}) {
		t.Fatalf("chunk after cancel: %+v %v", c, err)
	}

	// Close is idempotent and unblocks a pending Recv.
	r = newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}, nil)
	st, err = r.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	out2 := make(chan error, 1)
	go func() { _, err := st.Recv(); out2 <- err }()
	time.Sleep(50 * time.Millisecond)
	_ = st.Close()
	_ = st.Close()
	select {
	case err := <-out2:
		if !errors.Is(err, providers.ErrStreamClosed) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Recv")
	}

	// An expired context before the call sends nothing.
	dead, stop := context.WithCancel(context.Background())
	stop()
	r = newRig(t, replyJSON(textResponse), nil)
	if _, err := r.client.Generate(dead, testScope(), userReq("hi")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(r.api.requests()) != 0 {
		t.Fatal("a cancelled call reached the server")
	}
}
