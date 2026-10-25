package meta

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
)

// llamaHost configures a rig as a hosted-Llama endpoint (the generic dialect).
func llamaHost(c *Config) {
	c.Dialect, c.Model = DialectOpenAICompat, "llama-4-scout"
}

const bookArgs = `{"patient":"Luca Ferri","slot":"2026-10-14T10:30"}`

func TestToolCallsReturnedAsProposalsOnly(t *testing.T) {
	r := newRig(t, replyJSON(toolResponse("call_01Fixture", "book", bookArgs, 90, 20)), nil)
	req := userReq("Book Luca Ferri for Wednesday.")
	req.Tools = []providers.ToolSpec{bookTool}
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	// The proposal is data: name, id and the arguments exactly as the model wrote them.
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "book" || res.ToolCalls[0].ID != "call_01Fixture" ||
		string(res.ToolCalls[0].Arguments) != bookArgs || res.FinishReason != providers.FinishToolCalls {
		t.Fatalf("result %+v", res)
	}
	// One call, one request: the adapter never follows a proposal up.
	got := r.api.requests()
	if len(got) != 1 {
		t.Fatalf("%d upstream requests for one call", len(got))
	}
	body := got[0].json(t)
	tool := body["tools"].([]any)[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "book" || fn["parameters"] == nil || fn["strict"] != nil || body["tool_choice"] != "auto" {
		t.Fatalf("tools %v", body["tools"])
	}

	// The same through the stream, with the arguments in pieces.
	kv := map[string]string{"IN": "90", "OUT": "20", "STOP": jstr("tool_calls"), "CALL_ID": jstr("call_01Fixture"), "NAME": jstr("book")}
	parts := []string{render("stream_tool_open.sse", kv)}
	for i := 0; i < len(bookArgs); i += 10 {
		parts = append(parts, render("stream_tool_delta.sse", map[string]string{"ARGS_DELTA": jstr(bookArgs[i:min(i+10, len(bookArgs))])}))
	}
	parts = append(parts, render("stream_finish.sse", kv), render("stream_usage.sse", kv), render("stream_done.sse", kv))
	rs := newRig(t, replySSE(parts...), nil)
	st, err := rs.client.GenerateStream(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	sres, err := providers.CollectRich(st, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sres.ToolCalls) != 1 || string(sres.ToolCalls[0].Arguments) != bookArgs || sres.FinishReason != providers.FinishToolCalls {
		t.Fatalf("streamed result %+v", sres)
	}

	for name, tc := range map[string]struct {
		body   string
		choice providers.ToolChoice
	}{
		"a tool that was not offered":            {toolResponse("call_02", "delete_all_patients", `{}`, 9, 3), providers.ToolChoiceAuto},
		"a proposal after tools were turned off": {toolResponse("call_03", "book", bookArgs, 9, 3), providers.ToolChoiceNone},
		"arguments that are not an object":       {toolResponse("call_04", "book", `"x"`, 9, 3), providers.ToolChoiceAuto},
		"arguments that are not JSON":            {toolResponse("call_05", "book", `{"patient": `, 9, 3), providers.ToolChoiceAuto},
		"a host-run tool":                        {render("hosted_tool.json", map[string]string{"IN": "9", "OUT": "3"}), providers.ToolChoiceAuto},
		"no proposal when one is required":       {textResponseWith("I need more details.", 9, 3), providers.ToolChoiceRequired},
		"a tool call of another type":            {strings.Replace(toolResponse("call_06", "book", bookArgs, 9, 3), `"type":"function"`, `"type":"custom"`, 1), providers.ToolChoiceAuto},
	} {
		t.Run(name, func(t *testing.T) {
			rr := newRig(t, replyJSON(tc.body), nil)
			q := userReq("book")
			q.Tools, q.ToolChoice = []providers.ToolSpec{bookTool}, tc.choice
			res, err := rr.client.Generate(context.Background(), testScope(), q)
			if err == nil || len(res.ToolCalls) > 0 {
				t.Fatalf("accepted: %+v", res)
			}
		})
	}
}

func TestRequestShape(t *testing.T) {
	r := newRig(t, replyJSON(textResponseWith(`{"answer":"yes"}`, 10, 5)), nil)
	req := userReq("Answer in JSON.")
	req.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`))
	req.MaxToks = 300
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil || string(res.JSON) != `{"answer":"yes"}` {
		t.Fatalf("%+v %v", res, err)
	}
	c := r.api.requests()[0]
	if c.Header.Get("Authorization") != "Bearer "+fixtureKey || c.Path != "/v1/chat/completions" || c.Method != "POST" {
		t.Fatalf("request %s %s", c.Method, c.Path)
	}
	body := c.json(t)
	rf := body["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["name"] != "alixai_output" || js["schema"] == nil || js["strict"] != nil ||
		body["max_completion_tokens"] != float64(300) || body["max_tokens"] != nil || body["model"] != "muse-spark-1.3" {
		t.Fatalf("body %v", body)
	}
	if body["stream"] != nil || body["stream_options"] != nil || body["stop"] != nil || body["reasoning_effort"] != nil {
		t.Fatalf("body %v", body)
	}
	// Only these members are ever sent: no store, user, plugins, routing or other host extension.
	allowed := map[string]bool{"model": true, "messages": true, "max_completion_tokens": true, "max_tokens": true, "reasoning_effort": true, "response_format": true, "stop": true, "stream": true, "stream_options": true, "tools": true, "tool_choice": true, "temperature": true}
	for k := range body {
		if !allowed[k] {
			t.Errorf("unexpected request member %q", k)
		}
	}
	// A streamed request asks for the usage chunk.
	rs := newRig(t, replySSE(streamOf(5, 2, "stop", "ok")...), nil)
	st, err := rs.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providers.CollectRich(st, userReq("hi")); err != nil {
		t.Fatal(err)
	}
	sb := rs.api.requests()[0].json(t)
	if so, _ := sb["stream_options"].(map[string]any); sb["stream"] != true || so["include_usage"] != true {
		t.Fatalf("stream body %v", sb)
	}
	// An assistant turn that proposed a call and a tool answer are sent in the host's shape.
	rt := newRig(t, replyJSON(textResponseWith("done", 5, 1)), nil)
	conv := userReq("book")
	conv.Messages = append(conv.Messages,
		providers.RichMessage{Role: providers.MsgAssistant, ToolCalls: []providers.ToolCallProposal{{ID: "call_9", Name: "book", Arguments: json.RawMessage(bookArgs)}}},
		providers.RichMessage{Role: providers.MsgTool, ToolCallID: "call_9", Parts: []providers.ContentPart{providers.TextPart(`{"ok":true}`)}},
	)
	conv.Tools = []providers.ToolSpec{bookTool}
	if _, err := rt.client.Generate(context.Background(), testScope(), conv); err != nil {
		t.Fatal(err)
	}
	msgs := rt.api.requests()[0].json(t)["messages"].([]any)
	asst := msgs[2].(map[string]any)
	call := asst["tool_calls"].([]any)[0].(map[string]any)
	if asst["role"] != "assistant" || asst["content"] != nil || call["id"] != "call_9" || call["function"].(map[string]any)["arguments"] != bookArgs ||
		msgs[3].(map[string]any)["role"] != "tool" || msgs[3].(map[string]any)["tool_call_id"] != "call_9" {
		t.Fatalf("messages %v", msgs)
	}
	// The Model API accepts only tool_choice "auto": "required" goes out as auto and is
	// enforced on the answer, "none" leaves the tools out.
	rn := newRig(t, replyJSON(textResponseWith("no call", 5, 1)), nil)
	q := userReq("book")
	q.Tools, q.ToolChoice = []providers.ToolSpec{bookTool}, providers.ToolChoiceRequired
	if _, err := rn.client.Generate(context.Background(), testScope(), q); err == nil {
		t.Fatal("a missing required proposal was accepted")
	}
	if rn.api.requests()[0].json(t)["tool_choice"] != "auto" {
		t.Fatal("tool_choice was not downgraded to auto")
	}
	qn := q
	qn.ToolChoice = providers.ToolChoiceNone
	rnone := newRig(t, replyJSON(textResponseWith("fine", 5, 1)), nil)
	if _, err := rnone.client.Generate(context.Background(), testScope(), qn); err != nil {
		t.Fatal(err)
	}
	if b := rnone.api.requests()[0].json(t); b["tools"] != nil || b["tool_choice"] != nil {
		t.Fatalf("tools were sent with tool_choice none: %v", b)
	}
	// stop sequences are refused locally (the service answers 400 on a reasoning model)
	stop := userReq("hi")
	stop.Stop = []string{"END"}
	rs2 := newRig(t, replyJSON(textResponseWith("x", 1, 1)), nil)
	ce := wantCode2(t, "stop", func() error { _, err := rs2.client.Generate(context.Background(), testScope(), stop); return err }())
	if ce.Details["sent"] != false || len(rs2.api.requests()) != 0 {
		t.Fatalf("stop was sent: %v", ce.Details)
	}
	// a configured reasoning effort is sent
	re := newRig(t, replyJSON(textResponseWith("x", 1, 1)), func(c *Config) { c.ReasoningEffort = "low" })
	if _, err := re.client.Generate(context.Background(), testScope(), userReq("hi")); err != nil || re.api.requests()[0].json(t)["reasoning_effort"] != "low" {
		t.Fatalf("reasoning effort: %v", err)
	}

	// A hosted-Llama endpoint gets max_tokens, stop sequences and every tool_choice as asked.
	rl := newRig(t, replyJSON(toolResponse("c", "book", bookArgs, 5, 1)), llamaHost)
	ql := userReq("book")
	ql.MaxToks, ql.Stop = 300, []string{"END"}
	ql.Tools, ql.ToolChoice = []providers.ToolSpec{bookTool}, providers.ToolChoiceRequired
	if _, err := rl.client.Generate(context.Background(), testScope(), ql); err != nil {
		t.Fatal(err)
	}
	lb := rl.api.requests()[0].json(t)
	if lb["max_tokens"] != float64(300) || lb["max_completion_tokens"] != nil || fmt.Sprint(lb["stop"]) != "[END]" || lb["tool_choice"] != "required" || lb["model"] != "llama-4-scout" {
		t.Fatalf("hosted-Llama body %v", lb)
	}
	// ... unless that host rejects forced choices (Config.NoForcedToolChoice)
	rnf := newRig(t, replyJSON(textResponseWith("no call", 5, 1)), func(c *Config) { llamaHost(c); c.NoForcedToolChoice = true })
	if _, err := rnf.client.Generate(context.Background(), testScope(), ql); err == nil || rnf.api.requests()[0].json(t)["tool_choice"] != "auto" {
		t.Fatalf("a downgraded required choice was not enforced: %v", err)
	}
}

func TestUsageAndCostMapped(t *testing.T) {
	r := newRig(t, replyJSON(textResponseWith("ok", 1000, 500)), nil)
	res, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	// 1000 * 1250000 / 1e6 + 500 * 4250000 / 1e6 = 1250 + 2125 micro-USD, flagged as an estimate.
	if res.Usage.ToksIn != 1000 || res.Usage.ToksOut != 500 || res.Usage.CostMicros != 3375 || !res.Usage.Estimated || res.Usage.Currency != "USD" {
		t.Fatalf("usage %+v", res.Usage)
	}
	// no usage in the answer is an error, not a free call
	bad := strings.Replace(textResponseWith("ok", 1, 1), `"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":0}`, `"usage":null`, 1)
	r2 := newRig(t, replyJSON(bad), nil)
	_, err = r2.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrSchemaInvalid)
	// an unknown model has no price: it is metered in tokens, and cannot be budgeted
	r3 := newRig(t, replyJSON(textResponseWith("ok", 7, 3)), func(c *Config) { c.Model = "acme/unknown-llama" })
	res, err = r3.client.Generate(context.Background(), testScope(), userReq("hi"))
	if err != nil || res.Usage.ToksIn != 7 || res.Usage.CostMicros != 0 {
		t.Fatalf("%+v %v", res.Usage, err)
	}
}

func TestFinishReasons(t *testing.T) {
	for stop, want := range map[string]providers.FinishReason{"stop": providers.FinishStop, "length": providers.FinishLength, "content_filter": providers.FinishFiltered} {
		body := strings.Replace(textResponseWith("x", 1, 1), `"finish_reason":"stop"`, `"finish_reason":"`+stop+`"`, 1)
		res, err := newRig(t, replyJSON(body), nil).client.Generate(context.Background(), testScope(), userReq("hi"))
		if err != nil || res.FinishReason != want {
			t.Errorf("%s: %v %v", stop, res.FinishReason, err)
		}
	}
	for _, bad := range []string{`"eos"`, `null`, `"function_call"`} {
		body := strings.Replace(textResponseWith("x", 1, 1), `"finish_reason":"stop"`, `"finish_reason":`+bad, 1)
		_, err := newRig(t, replyJSON(body), nil).client.Generate(context.Background(), testScope(), userReq("hi"))
		wantCode(t, err, contracts.ErrSchemaInvalid)
	}
	// two choices, or an image part in the content, is not what was asked for
	two := strings.Replace(textResponseWith("x", 1, 1), `"choices":[`, `"choices":[{"index":1,"message":{"role":"assistant","content":"y"},"finish_reason":"stop"},`, 1)
	_, err := newRig(t, replyJSON(two), nil).client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrSchemaInvalid)
	_, err = newRig(t, replyJSON(render("active_media.json", map[string]string{"IN": "1", "OUT": "1"})), nil).client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrSchemaInvalid)
}

func TestStreamEdgeCases(t *testing.T) {
	ctx := context.Background()
	collect := func(t *testing.T, parts []string) (providers.RichResult, error) {
		r := newRig(t, replySSE(parts...), nil)
		req := userReq("hi")
		st, err := r.client.GenerateStream(ctx, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		return providers.CollectRich(st, req)
	}
	full := streamOf(5, 5, "stop", "par", "tial") // first, 2 deltas, finish, usage, done
	t.Run("a stream that ends without [DONE] is an error", func(t *testing.T) {
		_, err := collect(t, full[:len(full)-1])
		ce := wantCode(t, err, contracts.ErrUnavailable)
		if ce.Details["provider_error_kind"] != "connection_lost" || ce.Details["sent"] != true {
			t.Fatalf("details %v", ce.Details)
		}
	})
	t.Run("[DONE] without usage is an error: the call cannot be metered", func(t *testing.T) {
		_, err := collect(t, []string{full[0], full[1], full[2], full[3], full[5]})
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("[DONE] without a finish reason is an error", func(t *testing.T) {
		_, err := collect(t, []string{full[0], full[1], full[2], full[4], full[5]})
		wantCode(t, err, contracts.ErrUnavailable)
	})
	t.Run("usage inside the finish chunk is accepted", func(t *testing.T) {
		merged := strings.Replace(full[3], `"finish_reason":"stop"}]`, `"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":5}`, 1)
		res, err := collect(t, []string{full[0], full[1], full[2], merged, full[5]})
		if err != nil || res.Text() != "partial" || res.Usage.ToksIn != 5 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("an error event after a 200 is mapped by its code", func(t *testing.T) {
		_, err := collect(t, []string{full[0], full[1], render("stream_error.sse", map[string]string{"MARKER": "MARK-9"})})
		ce := wantCode(t, err, contracts.ErrUnavailable)
		if ce.Details["provider_error_kind"] != "unavailable" || strings.Contains(fmt.Sprint(ce), "MARK-9") {
			t.Fatalf("error %v %v", ce, ce.Details)
		}
	})
	t.Run("a host-run tool field is rejected", func(t *testing.T) {
		_, err := collect(t, []string{full[0], render("stream_hosted_tool.sse", nil)})
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("a call with no arguments streams {}", func(t *testing.T) {
		kv := map[string]string{"IN": "5", "OUT": "5", "STOP": jstr("tool_calls"), "CALL_ID": jstr("call_9"), "NAME": jstr("ping")}
		r := newRig(t, replySSE(render("stream_tool_open.sse", kv), render("stream_finish.sse", kv), render("stream_usage.sse", kv), render("stream_done.sse", kv)), nil)
		req := userReq("go")
		req.Tools = []providers.ToolSpec{{Name: "ping", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
		st, err := r.client.GenerateStream(ctx, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.CollectRich(st, req)
		if err != nil || len(res.ToolCalls) != 1 || string(res.ToolCalls[0].Arguments) != "{}" {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

func TestErrorMapping(t *testing.T) {
	apiError := func(typ, code string) string {
		return fmt.Sprintf(`{"error":{"message":"BODY-MARK","type":%q,"code":%q}}`, typ, code)
	}
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		code    contracts.ErrCode
		kind    string
		retried bool
	}{
		{"invalid request", 400, apiError("invalid_request_error", "invalid_value"), contracts.ErrValidationFailed, "bad_request", false},
		{"prompt too long by code", 400, apiError("invalid_request_error", "context_length_exceeded"), contracts.ErrValidationFailed, "context_too_large", false},
		{"prompt too long by message", 400, `{"error":{"message":"This model's maximum context length is 131072 tokens","type":"invalid_request_error","code":400}}`, contracts.ErrValidationFailed, "context_too_large", false},
		{"authentication", 401, apiError("invalid_request_error", "invalid_api_key"), contracts.ErrPermissionDenied, "authentication_failed", false},
		{"payment required", 402, apiError("", ""), contracts.ErrResourceExhausted, "quota_exceeded", false},
		{"permission", 403, apiError("permission_error", "forbidden"), contracts.ErrPermissionDenied, "permission_denied", false},
		{"not found", 404, apiError("invalid_request_error", "model_not_found"), contracts.ErrValidationFailed, "model_not_found", false},
		{"quota", 429, apiError("insufficient_quota", "insufficient_quota"), contracts.ErrResourceExhausted, "quota_exceeded", false},
		{"internal", 500, apiError("server_error", "server_error"), contracts.ErrUnavailable, "unavailable", false},
		{"timeout", 504, apiError("", ""), contracts.ErrTimeout, "timeout", false},
		{"overloaded", 503, apiError("server_error", "overloaded"), contracts.ErrUnavailable, "unavailable", true},
		{"unreadable body", 502, `<html>BODY-MARK</html>`, contracts.ErrUnavailable, "unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}, nil)
			_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
			ce := wantCode(t, err, tc.code)
			if ce.Details["provider_error_kind"] != tc.kind || ce.Details["sent"] != true || ce.Details["http_status"] != tc.status {
				t.Fatalf("details %v", ce.Details)
			}
			if strings.Contains(fmt.Sprintf("%v %+v %v", err, err, ce.Details), "BODY-MARK") {
				t.Fatal("the body was copied into the error")
			}
			n := len(r.api.requests())
			if tc.retried && n != 3 || !tc.retried && n != 1 {
				t.Fatalf("%d requests for a retried=%v failure", n, tc.retried)
			}
		})
	}
}

func TestRateLimitMappedWithRetryAfter(t *testing.T) {
	h := func(n int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "4")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.Header().Set("X-Ratelimit-Reset-Requests", "30s")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "12000")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"requests","code":"rate_limit_exceeded"}}`)
	}
	r := newRig(t, h, nil)
	_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrRateLimited)
	info, ok := providers.RateLimitFrom(err)
	if !ok || info.RetryAfter != 4*time.Second || info.Scope != "requests" {
		t.Fatalf("info %+v", info)
	}
	// two bounded retries, each waiting at least the provider's request
	if got := r.sleeps.all(); len(got) != 2 || got[0] < 4*time.Second || got[1] < 4*time.Second {
		t.Fatalf("sleeps %v", got)
	}
	if n := len(r.api.requests()); n != 3 {
		t.Fatalf("%d requests", n)
	}
	// a wait longer than the policy allows is not sat out
	r2 := newRig(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"x","type":"requests","code":"rate_limit_exceeded"}}`)
	}, nil)
	_, err = r2.client.Generate(context.Background(), testScope(), userReq("hi"))
	if info, ok := providers.RateLimitFrom(err); !ok || info.RetryAfter != 10*time.Minute || len(r2.api.requests()) != 1 {
		t.Fatalf("%+v %v requests %d", info, ok, len(r2.api.requests()))
	}
	// a retry that succeeds returns the answer
	r3 := newRig(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
	}, nil)
	if res, err := r3.client.Generate(context.Background(), testScope(), userReq("hi")); err != nil || res.Text() != "ok" || len(r3.api.requests()) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}

// TestCapabilityLimitsDeclared: the capability record the adapter reports at
// runtime is honest (text only, best-effort schema, tools not strict), agrees
// with the shipped profile, and a request beyond it is refused before anything
// is sent.
func TestCapabilityLimitsDeclared(t *testing.T) {
	r := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), nil)
	ctx := context.Background()
	caps, err := r.client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prof := shippedProfile(t)
	if caps.Provider != "meta" || caps.Model != "muse-spark-1.3" || !caps.Streaming || caps.StructuredOutput != providers.StructuredBestEffort ||
		!caps.Tools.Supported || caps.Tools.StrictArguments || caps.Tools.MaxTools != 32 || caps.MaxContextToks != 1048576 || caps.MaxOutputToks != 32768 ||
		caps.Pricing == nil || len(caps.InputModalities) != 1 || caps.InputModalities[0] != providers.ModText ||
		len(caps.OutputModalities) != 1 || caps.ImageGeneration || caps.SpeechToText || caps.TextToSpeech || caps.Grammar {
		t.Fatalf("caps %+v", caps)
	}
	if string(prof.StructuredOutput) != string(caps.StructuredOutput) || int(prof.Limits.MaxContextToks) != caps.MaxContextToks ||
		int(prof.Limits.MaxOutputToks) != caps.MaxOutputToks || prof.ToolCalling.MaxTools == nil || int(*prof.ToolCalling.MaxTools) != caps.Tools.MaxTools ||
		prof.ToolCalling.StrictArguments != caps.Tools.StrictArguments || string(prof.ModelID) != caps.Model ||
		int64(prof.Pricing.Prices.ToksIn.MicroUnits) != caps.Pricing.ToksIn.MicroUnits || int64(prof.Pricing.Prices.ToksOut.MicroUnits) != caps.Pricing.ToksOut.MicroUnits {
		t.Fatalf("the shipped profile and the capability record disagree: %+v", prof)
	}
	if len(r.api.requests()) != 0 {
		t.Fatal("capability discovery made a request")
	}
	// the operator's override reaches the record
	r2 := newRig(t, nil, func(c *Config) { c.MaxOutputToks, c.MaxContextToks, c.MaxTools = 2000, 32000, 3 })
	caps, _ = r2.client.Capabilities(ctx)
	if caps.MaxOutputToks != 2000 || caps.MaxContextToks != 32000 || caps.Tools.MaxTools != 3 {
		t.Fatalf("caps %+v", caps)
	}

	// Requests beyond the declared limits are refused with sent=false and never reach the host.
	over := userReq("hi")
	over.MaxToks = caps.MaxOutputToks + 1
	tooManyTools := userReq("hi")
	for i := 0; i < 4; i++ {
		tooManyTools.Tools = append(tooManyTools.Tools, providers.ToolSpec{Name: fmt.Sprintf("t%d", i), Parameters: json.RawMessage(`{"type":"object","properties":{}}`)})
	}
	image := userReq("hi")
	image.Messages[1].Parts = append(image.Messages[1].Parts, providers.ContentPart{Kind: providers.ModImage, MediaType: "image/png", Data: []byte{1, 2, 3}})
	grammar := userReq("hi")
	grammar.Output = providers.OutputConstraint{Grammar: `root ::= "x"`}
	badSchema := userReq("hi")
	badSchema.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object","properties":{"a":{"$ref":"https://evil.example/s.json"}}}`))
	unknownProfile := userReq("hi")
	unknownProfile.Profile = "no-such-profile"
	for name, q := range map[string]providers.RichRequest{
		"output cap over the model limit": over, "more tools than declared": tooManyTools, "an image part": image,
		"a grammar constraint": grammar, "a schema with an external reference": badSchema, "an unknown profile": unknownProfile,
	} {
		_, err := r2.client.Generate(ctx, testScope(), q)
		ce := wantCode2(t, name, err)
		if ce.Details["sent"] != false {
			t.Errorf("%s: sent=%v", name, ce.Details["sent"])
		}
		if _, err := r2.client.GenerateStream(ctx, testScope(), q); err == nil {
			t.Errorf("%s: the stream was started", name)
		}
	}
	if n := len(r2.api.requests()); n != 0 {
		t.Fatalf("%d over-limit requests reached the host", n)
	}
}

func wantCode2(t testing.TB, name string, err error) *contracts.Error {
	t.Helper()
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("%s: error %v is not a typed error", name, err)
	}
	return ce
}

func TestHealthAndCheckModel(t *testing.T) {
	ctx := context.Background()
	models := `{"object":"list","data":[{"id":"muse-spark-1.3"},{"id":"muse-spark-1.2"}]}`
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"Missing API key","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		_, _ = io.WriteString(w, models)
	}, nil)
	if h, err := r.client.Health(ctx); err != nil || h.Status != providers.HealthOK {
		t.Fatalf("%+v %v", h, err)
	}
	if err := r.client.CheckModel(ctx, testScope()); err != nil {
		t.Fatal(err)
	}
	if ids, err := r.client.Models(ctx, testScope()); err != nil || len(ids) != 2 || ids[0] != "muse-spark-1.2" {
		t.Fatalf("%v %v", ids, err)
	}
	got := r.api.requests()
	if got[0].Path != "/v1/status" || got[0].Header.Get("Authorization") != "" || got[1].Header.Get("Authorization") != "Bearer "+fixtureKey {
		t.Fatalf("requests %+v", got)
	}
	// a model the key cannot use is model_not_found
	r2 := newRig(t, replyJSON(`{"data":[{"id":"something-else"}]}`), nil)
	ce := wantCode(t, r2.client.CheckModel(ctx, testScope()), contracts.ErrValidationFailed)
	if ce.Details["provider_error_kind"] != "model_not_found" {
		t.Fatalf("details %v", ce.Details)
	}
	// a hosted-Llama endpoint is probed with its anonymous model list
	rl := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"meta-llama/Llama-3.3-70B-Instruct-Turbo"},{"id":"llama-4-scout"}]}`)
	}, llamaHost)
	if h, err := rl.client.Health(ctx); err != nil || h.Status != providers.HealthOK || rl.api.requests()[0].Path != "/v1/models" {
		t.Fatalf("%+v %v", h, err)
	}
	if err := rl.client.CheckModel(ctx, testScope()); err != nil {
		t.Fatal(err)
	}
	// an unreachable service is unavailable, not ok
	r3 := newRig(t, nil, nil)
	r3.api.srv.Close()
	if h, _ := r3.client.Health(ctx); h.Status != providers.HealthUnavailable {
		t.Fatalf("health %+v", h)
	}
}

// TestOnlyAllowlistedEndpointsCalled: the client can reach exactly two routes
// under the configured prefix, on the configured origin, with no query string.
func TestOnlyAllowlistedEndpointsCalled(t *testing.T) {
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/status":
			_, _ = io.WriteString(w, `{"status":"operational"}`)
		case "/v1/models":
			if req.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":{"message":"x","type":"invalid_request_error","code":"invalid_api_key"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"muse-spark-1.3"}]}`)
		default:
			_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
		}
	}, nil)
	want := []string{"POST /v1/chat/completions", "GET /v1/models", "GET /v1/status"}
	if got := r.client.AllowedEndpoints(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routes %v", got)
	}
	ctx := context.Background()
	if _, err := r.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if err := r.client.CheckModel(ctx, testScope()); err != nil {
		t.Fatal(err)
	}
	if h, err := r.client.Health(ctx); err != nil || h.Status != providers.HealthOK {
		t.Fatalf("%+v %v", h, err)
	}
	seen := map[string]bool{}
	for _, c := range r.api.requests() {
		if c.Query != "" {
			t.Errorf("a query string was sent: %s", c.Query)
		}
		seen[c.Method+" "+c.Path] = true
	}
	if len(seen) != 3 || !seen["POST /v1/chat/completions"] || !seen["GET /v1/models"] || !seen["GET /v1/status"] {
		t.Errorf("routes used %v", seen)
	}

	// Anything else is refused before any I/O, whatever the method: completions,
	// embeddings, files, batches, fine-tuning, assistants, other prefixes.
	n := len(r.api.requests())
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/completions"}, {"POST", "/v1/embeddings"}, {"GET", "/v1/files"}, {"POST", "/v1/files"}, {"POST", "/v1/batches"},
		{"POST", "/v1/fine_tuning/jobs"}, {"POST", "/v1/responses"}, {"POST", "/v1/assistants"}, {"GET", "/v1/models/muse-spark-1.3"},
		{"POST", "/v1/status"}, {"POST", "/v1/messages"}, {"POST", "/v1/search"}, {"POST", "/v1/images/generations"}, {"POST", "/v1/audio/transcriptions"},
		{"DELETE", "/v1/models/muse-spark-1.3"}, {"GET", "/v1/chat/completions"}, {"POST", "/v1/models"}, {"POST", "/v2/chat/completions"},
		{"POST", "/chat/completions"}, {"POST", "/v1/chat/completions?beta=true"}, {"POST", "/v1/%63hat/completions"},
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

	// A different API prefix moves the whole route table with it.
	rp := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), func(c *Config) { c.APIPrefix = "/openai/v1" })
	if _, err := rp.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if got := rp.api.requests()[0].Path; got != "/openai/v1/chat/completions" {
		t.Fatalf("path %s", got)
	}
	req, _ := http.NewRequest("POST", rp.api.srv.URL+"/v1/chat/completions", strings.NewReader("{}"))
	if resp, err := rp.client.hc.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("the default prefix stayed reachable after a custom one was configured")
	}

	// A hosted-Llama endpoint has the model list as its probe, and no /status route.
	rl := newRig(t, replyJSON(`{"data":[{"id":"llama-4-scout"}]}`), llamaHost)
	if got := rl.client.AllowedEndpoints(); fmt.Sprint(got) != fmt.Sprint([]string{"POST /v1/chat/completions", "GET /v1/models"}) {
		t.Fatalf("hosted-Llama routes %v", got)
	}
	if req, _ := http.NewRequest("GET", rl.api.srv.URL+"/v1/status", nil); true {
		if resp, err := rl.client.hc.Do(req); err == nil {
			resp.Body.Close()
			t.Fatal("/v1/status is reachable on a hosted-Llama endpoint")
		}
	}

	// A host that is not on the egress allowlist is never dialled, even when
	// the configuration names it.
	other := newFakeAPI(t, replyJSON(textResponseWith("ok", 1, 1)))
	r2 := newRigFor(t, other, testConfig(other.srv.URL), r.api.srv.URL) // allowlist lists the OTHER server
	_, err := r2.client.Generate(ctx, testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrPermissionDenied)
	if ce.Details["sent"] != false || len(other.requests()) != 0 {
		t.Fatalf("details %v, %d requests reached the unlisted host", ce.Details, len(other.requests()))
	}
	// A client is never built without a dialer.
	if _, err := New(testConfig("https://api.meta.ai"), nil, r.keys); err == nil {
		t.Fatal("a client was built without a dialer")
	}
}

func TestAPIKeyNeverLoggedOrReturned(t *testing.T) {
	leaky := func(_ int, w http.ResponseWriter, req *http.Request) {
		// The provider echoes the key everywhere it can.
		auth := req.Header.Get("Authorization")
		w.Header().Set("X-Echo", auth)
		if req.URL.Path == "/v1/models" || req.URL.Path == "/v1/status" {
			w.WriteHeader(401)
		} else {
			w.WriteHeader(400)
		}
		fmt.Fprintf(w, `{"error":{"message":"bad key %s","type":"invalid_request_error","code":"req_%s"}}`, auth, auth)
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
	errs = append(errs, r.client.CheckModel(ctx, testScope()))
	h, herr := r.client.Health(ctx)
	errs = append(errs, herr)

	// A successful call, so its result is checked as well.
	ok := newRig(t, replyJSON(textResponseWith("ok", 1000, 500)), nil)
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
	// The key did travel, once per authenticated call, in the Authorization header only.
	for _, c := range append(r.api.requests(), ok.api.requests()...) {
		if (c.Path == "/v1/models" || c.Path == "/v1/status") && c.Header.Get("Authorization") == "" {
			continue // the anonymous health probe
		}
		if c.Header.Get("Authorization") != "Bearer "+fixtureKey || c.Header.Get("X-Api-Key") != "" {
			t.Errorf("request to %s: authorization %q", c.Path, c.Header.Get("Authorization"))
		}
		if strings.Contains(string(c.Body), fixtureKey) || strings.Contains(c.Query, fixtureKey) {
			t.Errorf("the key is in a request body or URL")
		}
	}
	if ok.logs.Len() == 0 || !strings.Contains(ok.logs.String(), `"tok_in":1000`) || strings.Contains(ok.logs.String(), "_tokens") {
		t.Errorf("debug record missing or uses a redacted key name: %s", ok.logs.String())
	}

	// Key resolution fails closed: nothing is sent, no other credential is tried.
	r2 := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), nil)
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
		t.Fatalf("authorization %q", got)
	}
}

func TestCancellationStopsStream(t *testing.T) {
	block := make(chan struct{})
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, render("stream_first.sse", nil))
		w.(http.Flusher).Flush()
		select {
		case <-req.Context().Done():
		case <-block:
		}
	}, nil)
	t.Cleanup(func() { close(block) })
	ctx, cancel := context.WithCancel(context.Background())
	st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(100*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() { _, err := st.Recv(); done <- err }()
	select {
	case err := <-done:
		if err == nil || err == io.EOF {
			t.Fatalf("Recv returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Recv outlived its cancelled context")
	}
	_ = st.Close()
}
