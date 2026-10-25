package anthropic

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

func TestToolUseReturnedAsProposalsOnly(t *testing.T) {
	const args = `{"patient":"Luca Ferri","slot":"2026-10-14T10:30"}`
	r := newRig(t, replyJSON(toolResponse("toolu_01Fixture", "book", args, 90, 20)), nil)
	req := userReq("Book Luca Ferri for Wednesday.")
	req.Tools = []providers.ToolSpec{bookTool}
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	// The proposal is data: name, id and the arguments exactly as the model wrote them.
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "book" || res.ToolCalls[0].ID != "toolu_01Fixture" ||
		string(res.ToolCalls[0].Arguments) != args || res.FinishReason != providers.FinishToolCalls {
		t.Fatalf("result %+v", res)
	}
	// One call, one request: the adapter never follows a proposal up.
	got := r.api.requests()
	if len(got) != 1 {
		t.Fatalf("%d upstream requests for one call", len(got))
	}
	body := got[0].json(t)
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if len(tools) != 1 || tool["name"] != "book" || tool["strict"] != true || tool["input_schema"] == nil || tool["type"] != nil {
		t.Fatalf("tools %v", tools)
	}
	if tc := body["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Fatalf("tool_choice %v", tc)
	}

	// The same through the stream, with the arguments in pieces.
	kv := map[string]string{"IN": "90", "OUT": "20", "STOP": jstr("tool_use"), "CALL_ID": jstr("toolu_01Fixture"), "NAME": jstr("book")}
	parts := []string{render("stream_start.sse", kv), render("stream_tool_open.sse", kv)}
	for i := 0; i < len(args); i += 10 {
		parts = append(parts, render("stream_tool_delta.sse", map[string]string{"ARGS_DELTA": jstr(args[i:min(i+10, len(args))])}))
	}
	parts = append(parts, render("stream_block_stop.sse", kv), render("stream_end.sse", kv))
	rs := newRig(t, replySSE(parts...), nil)
	st, err := rs.client.GenerateStream(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	sres, err := providers.CollectRich(st, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(sres.ToolCalls) != 1 || string(sres.ToolCalls[0].Arguments) != args || sres.FinishReason != providers.FinishToolCalls {
		t.Fatalf("streamed result %+v", sres)
	}

	for name, tc := range map[string]struct {
		body   string
		choice providers.ToolChoice
	}{
		"a tool that was not offered":            {toolResponse("toolu_02", "delete_all_patients", `{}`, 9, 3), providers.ToolChoiceAuto},
		"a proposal after tools were turned off": {toolResponse("toolu_03", "book", args, 9, 3), providers.ToolChoiceNone},
		"arguments that are not an object":       {toolResponse("toolu_04", "book", `"x"`, 9, 3), providers.ToolChoiceAuto},
		"arguments that are not JSON":            {toolResponse("toolu_05", "book", `{"patient": `, 9, 3), providers.ToolChoiceAuto},
		"a provider-run tool block":              {render("hosted_tool.json", map[string]string{"IN": "9", "OUT": "3"}), providers.ToolChoiceAuto},
		"no proposal when one is required":       {textResponseWith("I need more details.", 9, 3), providers.ToolChoiceRequired},
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

func TestUsageAndCostMapped(t *testing.T) {
	ctx := context.Background()
	t.Run("whole answer, built-in price", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 1000, 500)), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		// claude-haiku-4-5: 1 USD in, 5 USD out per million tokens, in micro-USD.
		u := res.Usage
		if u.ToksIn != 1000 || u.ToksOut != 500 || u.CostMicros != 1000+2500 || u.Currency != "USD" || !u.Estimated {
			t.Fatalf("usage %+v", u)
		}
		if res.TraceID != "req_fixture1" && res.TraceID != "msg_01FixtureText0001" {
			t.Fatalf("trace %q", res.TraceID)
		}
		if res.Model != "claude-haiku-4-5-20251001" {
			t.Fatalf("model %q", res.Model)
		}
	})
	t.Run("cache writes count twice and cache reads once, so the figure stays an upper bound", func(t *testing.T) {
		body := strings.Replace(textResponseWith("ok", 100, 10), `"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0`,
			`"cache_creation_input_tokens": 50, "cache_read_input_tokens": 200`, 1)
		r := newRig(t, replyJSON(body), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Usage.ToksIn != 100+200+2*50 {
			t.Fatalf("usage %+v", res.Usage)
		}
	})
	t.Run("operator price table overrides the built-in one", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 2000, 1000)), func(c *Config) {
			c.Prices = usdTable(2_000_000, 8_000_000)
		})
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Usage.CostMicros != 4000+8000 {
			t.Fatalf("usage %+v", res.Usage)
		}
	})
	t.Run("a model the table does not know has counts and no cost", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 10, 5)), func(c *Config) { c.Model = "claude-future-9" })
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Usage.ToksIn != 10 || res.Usage.ToksOut != 5 || res.Usage.CostMicros != 0 {
			t.Fatalf("usage %+v", res.Usage)
		}
	})
	t.Run("a response without usage is malformed, never a free success", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn"}`
		r := newRig(t, replyJSON(body), nil)
		_, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("streamed: input from message_start, output from message_delta", func(t *testing.T) {
		r := newRig(t, replySSE(streamOf(1000, 500, "end_turn", "Hello ", "there.")...), nil)
		req := userReq("hi")
		st, err := r.client.GenerateStream(ctx, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.CollectRich(st, req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Text() != "Hello there." || res.Usage.ToksIn != 1000 || res.Usage.ToksOut != 500 || res.Usage.CostMicros != 3500 {
			t.Fatalf("result %+v", res)
		}
		if res.TraceID == "" || res.Model != "claude-haiku-4-5-20251001" {
			t.Fatalf("trace %q model %q", res.TraceID, res.Model)
		}
	})
}

func TestRequestShape(t *testing.T) {
	r := newRig(t, replyJSON(textResponseWith("ok", 5, 1)), nil)
	req := providers.RichRequest{
		Messages: []providers.RichMessage{
			{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart("system one")}},
			{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart("system two")}},
			{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("Book both.")}},
			{Role: providers.MsgAssistant, ToolCalls: []providers.ToolCallProposal{
				{ID: "toolu_a", Name: "book", Arguments: json.RawMessage(`{"patient":"A","slot":"1"}`)},
				{ID: "toolu_b", Name: "book", Arguments: json.RawMessage(`{"patient":"B","slot":"2"}`)},
			}},
			{Role: providers.MsgTool, ToolCallID: "toolu_a", Parts: []providers.ContentPart{providers.TextPart("receipt A")}},
			{Role: providers.MsgTool, ToolCallID: "toolu_b", Parts: []providers.ContentPart{providers.TextPart("receipt B")}},
		},
		Tools:       []providers.ToolSpec{bookTool},
		MaxToks:     300,
		Temperature: 0.5,
		Stop:        []string{"END"},
		Output:      providers.SchemaConstraint(json.RawMessage(`{"type":"object","additionalProperties":false,"required":["a"],"properties":{"a":{"type":"string","maxLength":9}}}`)),
	}
	req.ToolChoice = providers.ToolChoiceRequired
	// the answer is JSON text, so the structured path succeeds
	r = newRig(t, replyJSON(textResponseWith(`{"a":"x"}`, 5, 1)), nil)
	// a required proposal is not met by this answer: the call fails, which is the point of the next line
	if _, err := r.client.Generate(context.Background(), testScope(), req); err == nil {
		t.Fatal("a required proposal was satisfied by plain text")
	}
	h := r.api.requests()[0]
	if h.Header.Get("X-Api-Key") != fixtureKey || h.Header.Get("Anthropic-Version") != "2023-06-01" || h.Header.Get("Authorization") != "" {
		t.Fatalf("headers %v", h.Header)
	}
	for k := range h.Header {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-beta") {
			t.Fatalf("a beta header was sent: %s", k)
		}
	}
	body := h.json(t)
	for _, k := range []string{"thinking", "metadata", "service_tier", "container", "mcp_servers", "context_management", "cache_control", "speed", "fallbacks"} {
		if _, ok := body[k]; ok {
			t.Errorf("body carries %q", k)
		}
	}
	if body["model"] != "claude-haiku-4-5" || body["max_tokens"] != float64(300) || body["temperature"] != 0.5 {
		t.Fatalf("body %v", body)
	}
	if fmt.Sprint(body["stop_sequences"]) != "[END]" {
		t.Fatalf("stop_sequences %v", body["stop_sequences"])
	}
	if sys := body["system"].([]any); len(sys) != 2 || sys[1].(map[string]any)["text"] != "system two" {
		t.Fatalf("system %v", body["system"])
	}
	// haiku accepts a forced choice
	if tc := body["tool_choice"].(map[string]any); tc["type"] != "any" {
		t.Fatalf("tool_choice %v", tc)
	}
	oc := body["output_config"].(map[string]any)
	f := oc["format"].(map[string]any)
	if f["type"] != "json_schema" || strings.Contains(fmt.Sprint(f["schema"]), "maxLength") {
		t.Fatalf("output_config %v", oc)
	}
	// both results of the turn travel in ONE user message, as tool_result blocks
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("%d messages: %v", len(msgs), msgs)
	}
	last := msgs[2].(map[string]any)
	blocks := last["content"].([]any)
	if last["role"] != "user" || len(blocks) != 2 || blocks[1].(map[string]any)["tool_use_id"] != "toolu_b" || blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("last message %v", last)
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if len(asst) != 2 || asst[0].(map[string]any)["type"] != "tool_use" || asst[0].(map[string]any)["input"].(map[string]any)["patient"] != "A" {
		t.Fatalf("assistant message %v", asst)
	}
}

func TestToolChoiceOnModelsWithoutForcedChoice(t *testing.T) {
	r := newRig(t, replyJSON(toolResponse("toolu_1", "book", `{"patient":"A","slot":"1"}`, 5, 1)), func(c *Config) { c.Model = "claude-opus-5-5" })
	req := userReq("book")
	req.Tools, req.ToolChoice = []providers.ToolSpec{bookTool}, providers.ToolChoiceRequired
	if _, err := r.client.Generate(context.Background(), testScope(), req); err != nil {
		t.Fatal(err)
	}
	if tc := r.api.requests()[0].json(t)["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Fatalf("a model that rejects a forced choice was sent %v", tc)
	}
	// ... and the requirement is still enforced on the answer
	r2 := newRig(t, replyJSON(textResponseWith("no tool", 5, 1)), func(c *Config) { c.Model = "claude-opus-5-5" })
	if _, err := r2.client.Generate(context.Background(), testScope(), req); err == nil {
		t.Fatal("a required proposal was missing and no error was reported")
	}
}

func TestRefusedBeforeSending(t *testing.T) {
	tooMany := make([]providers.ToolSpec, 21)
	for i := range tooMany {
		tooMany[i] = providers.ToolSpec{Name: fmt.Sprintf("t%d", i), Parameters: json.RawMessage(bookSchema)}
	}
	img := userReq("see")
	img.Messages[1].Parts = append(img.Messages[1].Parts, providers.ContentPart{Kind: providers.ModImage, RefID: "ref_1"})
	prefill := userReq("hi")
	prefill.Messages = append(prefill.Messages, providers.RichMessage{Role: providers.MsgAssistant, Parts: []providers.ContentPart{providers.TextPart("Sure, the answer is")}})
	late := userReq("hi")
	late.Messages = append(late.Messages, providers.RichMessage{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart("late")}})
	hot := userReq("hi")
	hot.Temperature = 0.4
	grammar := userReq("hi")
	grammar.Output = providers.OutputConstraint{Grammar: `root ::= "x"`}
	open := userReq("hi")
	open.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object","additionalProperties":{"type":"string"}}`))
	rec := userReq("hi")
	rec.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"n":{"$ref":"#/$defs/node"}},"$defs":{"node":{"type":"object","additionalProperties":false,"properties":{"next":{"$ref":"#/$defs/node"}}}}}`))
	ext := userReq("hi")
	ext.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"n":{"$ref":"https://example.com/x.json"}}}`))
	unknownProfile := userReq("hi")
	unknownProfile.Profile = "nope"

	for name, tc := range map[string]struct {
		req  providers.RichRequest
		mut  func(*Config)
		want string
	}{
		"more strict tools than the API allows":    {func() providers.RichRequest { q := userReq("hi"); q.Tools = tooMany; return q }(), nil, ""},
		"an image input":                           {img, nil, ""},
		"an assistant prefill":                     {prefill, nil, "end with a user turn"},
		"a system message after the first turn":    {late, nil, "system message"},
		"a temperature on a model that rejects it": {hot, func(c *Config) { c.Model = "claude-sonnet-5-5" }, ""},
		"a grammar":                        {grammar, nil, ""},
		"an open map schema":               {open, nil, ""},
		"a recursive schema":               {rec, nil, ""},
		"an external reference":            {ext, nil, ""},
		"an unknown profile hint":          {unknownProfile, nil, "profile"},
		"an effort on a model without one": {userReq("hi"), func(c *Config) { c.Effort = "high" }, "effort"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), tc.mut)
			_, err := r.client.Generate(context.Background(), testScope(), tc.req)
			ce := wantCode(t, err, contracts.ErrValidationFailed)
			if ce.Details["sent"] != false || len(r.api.requests()) != 0 {
				t.Fatalf("details %v, %d requests left", ce.Details, len(r.api.requests()))
			}
			if tc.want != "" && !strings.Contains(ce.Message, tc.want) {
				t.Fatalf("message %q lacks %q", ce.Message, tc.want)
			}
		})
	}
}

func TestEffortSentWhenConfigured(t *testing.T) {
	r := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), func(c *Config) { c.Model = "claude-sonnet-5-5"; c.Effort = "low" })
	if _, err := r.client.Generate(context.Background(), testScope(), userReq("hi")); err != nil {
		t.Fatal(err)
	}
	body := r.api.requests()[0].json(t)
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Fatalf("body %v", body)
	}
	if _, ok := body["thinking"]; ok {
		t.Fatal("thinking was sent")
	}
}

func TestThinkingBlocksAreIgnoredNotCarried(t *testing.T) {
	body := `{"id":"msg_1","type":"message","model":"claude-opus-5-5","content":[` +
		`{"type":"thinking","thinking":"secret reasoning","signature":"sig"},{"type":"redacted_thinking","data":"x"},` +
		`{"type":"text","text":"Answer."}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":9}}`
	r := newRig(t, replyJSON(body), nil)
	res, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if res.Text() != "Answer." || strings.Contains(string(b), "secret reasoning") {
		t.Fatalf("result %s", b)
	}
}

func TestStopReasons(t *testing.T) {
	for stop, want := range map[string]providers.FinishReason{
		"end_turn": providers.FinishStop, "stop_sequence": providers.FinishStop, "max_tokens": providers.FinishLength,
		"refusal": providers.FinishFiltered,
	} {
		body := strings.Replace(textResponseWith("x", 1, 1), `"end_turn"`, jstr(stop), 1)
		r := newRig(t, replyJSON(body), nil)
		res, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
		if err != nil || res.FinishReason != want {
			t.Errorf("%s: %v %v", stop, res.FinishReason, err)
		}
	}
	for _, stop := range []string{"pause_turn", "something_new"} {
		body := strings.Replace(textResponseWith("x", 1, 1), `"end_turn"`, jstr(stop), 1)
		r := newRig(t, replyJSON(body), nil)
		_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
		wantCode(t, err, contracts.ErrSchemaInvalid)
	}
	// A refusal under a schema is never a result.
	body := strings.Replace(textResponseWith("I cannot help.", 1, 1), `"end_turn"`, `"refusal"`, 1)
	r := newRig(t, replyJSON(body), nil)
	req := userReq("hi")
	req.Output = providers.SchemaConstraint(json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}}`))
	_, err := r.client.Generate(context.Background(), testScope(), req)
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
	t.Run("a stream that ends before message_stop is an error", func(t *testing.T) {
		parts := streamOf(5, 5, "end_turn", "partial")
		_, err := collect(t, parts[:len(parts)-1][:3])
		ce := wantCode(t, err, contracts.ErrUnavailable)
		if ce.Details["provider_error_kind"] != "connection_lost" || ce.Details["sent"] != true {
			t.Fatalf("details %v", ce.Details)
		}
	})
	t.Run("an error event after a 200 is mapped by its type", func(t *testing.T) {
		parts := append(streamOf(5, 5, "end_turn", "partial")[:3], render("stream_error.sse", map[string]string{"MARKER": "MARK-9"}))
		_, err := collect(t, parts)
		ce := wantCode(t, err, contracts.ErrUnavailable)
		if ce.Details["provider_error_kind"] != "unavailable" || strings.Contains(fmt.Sprint(ce), "MARK-9") {
			t.Fatalf("error %v %v", ce, ce.Details)
		}
	})
	t.Run("a provider-run block is rejected", func(t *testing.T) {
		_, err := collect(t, []string{render("stream_start.sse", map[string]string{"IN": "5"}), render("stream_hosted_tool.sse", nil)})
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("thinking deltas are ignored", func(t *testing.T) {
		kv := map[string]string{"IN": "5", "OUT": "5", "STOP": jstr("end_turn")}
		parts := []string{
			render("stream_start.sse", kv),
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
			render("stream_end.sse", kv),
		}
		res, err := collect(t, parts)
		if err != nil || res.Text() != "Hi" {
			t.Fatalf("%v %v", res, err)
		}
	})
	t.Run("a call with no arguments streams {}", func(t *testing.T) {
		kv := map[string]string{"IN": "5", "OUT": "5", "STOP": jstr("tool_use"), "CALL_ID": jstr("toolu_9"), "NAME": jstr("ping")}
		r := newRig(t, replySSE(render("stream_start.sse", kv), render("stream_tool_open.sse", kv), render("stream_block_stop.sse", kv), render("stream_end.sse", kv)), nil)
		req := userReq("go")
		req.Tools = []providers.ToolSpec{{Name: "ping", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)}}
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
	apiError := func(typ, extra string) string {
		return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"BODY-MARK %s"%s},"request_id":"req_1"}`, typ, extra, extra)
	}
	spend := `{"type":"error","error":{"type":"rate_limit_error","message":"BODY-MARK","details":{"error_code":"enforced_spend_limit_reached"}},"request_id":"req_1"}`
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		code    contracts.ErrCode
		kind    string
		retried bool
	}{
		{"invalid request", 400, apiError("invalid_request_error", ""), contracts.ErrValidationFailed, "bad_request", false},
		{"prompt too long", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 200000 maximum"}}`, contracts.ErrValidationFailed, "context_too_large", false},
		{"operator spend limit", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"You have reached your specified API usage limits."}}`, contracts.ErrResourceExhausted, "quota_exceeded", false},
		{"authentication", 401, apiError("authentication_error", ""), contracts.ErrPermissionDenied, "authentication_failed", false},
		{"billing", 402, apiError("billing_error", ""), contracts.ErrResourceExhausted, "quota_exceeded", false},
		{"permission", 403, apiError("permission_error", ""), contracts.ErrPermissionDenied, "permission_denied", false},
		{"not found", 404, apiError("not_found_error", ""), contracts.ErrValidationFailed, "model_not_found", false},
		{"too large", 413, apiError("request_too_large", ""), contracts.ErrValidationFailed, "context_too_large", false},
		{"spend cap", 429, spend, contracts.ErrResourceExhausted, "quota_exceeded", false},
		{"internal", 500, apiError("api_error", ""), contracts.ErrUnavailable, "unavailable", false},
		{"timeout", 504, apiError("timeout_error", ""), contracts.ErrTimeout, "timeout", false},
		{"overloaded", 529, apiError("overloaded_error", ""), contracts.ErrUnavailable, "unavailable", true},
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
	reset := time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)
	h := func(n int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "4")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "0")
		w.Header().Set("Anthropic-Ratelimit-Requests-Reset", reset)
		w.Header().Set("Anthropic-Ratelimit-Input-Tokens-Remaining", "12000")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
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
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`)
	}, nil)
	_, err = r2.client.Generate(context.Background(), testScope(), userReq("hi"))
	if info, ok := providers.RateLimitFrom(err); !ok || info.RetryAfter != 10*time.Minute || len(r2.api.requests()) != 1 {
		t.Fatalf("%+v %v requests %d", info, ok, len(r2.api.requests()))
	}
}

func TestRateLimitHeadersOnSuccess(t *testing.T) {
	now := time.Now()
	h := func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "998")
		w.Header().Set("Anthropic-Ratelimit-Input-Tokens-Remaining", "0")
		w.Header().Set("Anthropic-Ratelimit-Input-Tokens-Reset", now.Add(20*time.Second).UTC().Format(time.RFC3339))
		w.Header().Set("Anthropic-Ratelimit-Output-Tokens-Remaining", "50000")
		_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
	}
	r := newRig(t, h, nil)
	res, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	rl := res.RateLimit
	if rl == nil || rl.RemainingRequests == nil || *rl.RemainingRequests != 998 || rl.RemainingToks == nil || *rl.RemainingToks != 0 || rl.Scope != "toks" {
		t.Fatalf("rate limit %+v", rl)
	}
	if rl.RetryAfter < 15*time.Second || rl.RetryAfter > 21*time.Second {
		t.Fatalf("retry after %v", rl.RetryAfter)
	}
}

func TestCapabilitiesDeclaredAtRuntime(t *testing.T) {
	r := newRig(t, nil, nil)
	caps, err := r.client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Provider != "anthropic" || caps.Model != "claude-haiku-4-5" || !caps.Streaming || caps.StructuredOutput != providers.StructuredStrict ||
		!caps.Tools.Supported || caps.Tools.MaxTools != 20 || caps.MaxOutputToks != 32000 || caps.Pricing == nil ||
		len(caps.InputModalities) != 1 || caps.ImageGeneration || caps.Grammar {
		t.Fatalf("caps %+v", caps)
	}
	if len(r.api.requests()) != 0 {
		t.Fatal("capability discovery made a request")
	}
	// the operator's override reaches the record
	r2 := newRig(t, nil, func(c *Config) { c.MaxOutputToks = 8000 })
	caps, _ = r2.client.Capabilities(context.Background())
	if caps.MaxOutputToks != 8000 {
		t.Fatalf("caps %+v", caps)
	}
}

func TestHealthAndCheckModel(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Api-Key") == "" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"x-api-key header is required"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"claude-haiku-4-5","type":"model","display_name":"Claude Haiku 4.5"}`)
	}, nil)
	if h, err := r.client.Health(ctx); err != nil || h.Status != providers.HealthOK {
		t.Fatalf("%+v %v", h, err)
	}
	if err := r.client.CheckModel(ctx, testScope()); err != nil {
		t.Fatal(err)
	}
	got := r.api.requests()
	if got[0].Path != "/v1/models" || got[0].Header.Get("X-Api-Key") != "" || got[1].Path != "/v1/models/claude-haiku-4-5" || got[1].Header.Get("X-Api-Key") != fixtureKey {
		t.Fatalf("requests %+v", got)
	}
	// an unknown model is model_not_found
	r2 := newRig(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"model: claude-haiku-4-5"}}`)
	}, nil)
	ce := wantCode(t, r2.client.CheckModel(ctx, testScope()), contracts.ErrValidationFailed)
	if ce.Details["provider_error_kind"] != "model_not_found" {
		t.Fatalf("details %v", ce.Details)
	}
	// an unreachable service is unavailable, not ok
	r3 := newRig(t, nil, nil)
	r3.api.srv.Close()
	if h, _ := r3.client.Health(ctx); h.Status != providers.HealthUnavailable {
		t.Fatalf("health %+v", h)
	}
}

func TestOnlyAllowlistedEndpointsCalled(t *testing.T) {
	want := []string{"POST /v1/messages", "GET /v1/models", "GET /v1/models/{configured model}"}
	if got := AllowedEndpoints(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routes %v", got)
	}
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v1/models":
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"x"}}`)
		case "/v1/models/claude-haiku-4-5":
			_, _ = io.WriteString(w, `{"id":"claude-haiku-4-5","type":"model"}`)
		default:
			_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
		}
	}, nil)
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
	for _, w := range []string{"POST /v1/messages", "GET /v1/models", "GET /v1/models/claude-haiku-4-5"} {
		if !seen[w] {
			t.Errorf("route %s never used", w)
		}
	}
	if len(seen) != 3 {
		t.Errorf("routes used %v", seen)
	}

	// Anything else is refused before any I/O, whatever the method: batches,
	// files, skills, agents, sessions, memory, the admin API, other models.
	n := len(r.api.requests())
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/messages/batches"}, {"POST", "/v1/messages/count_tokens"}, {"GET", "/v1/files"}, {"POST", "/v1/files"},
		{"GET", "/v1/skills"}, {"POST", "/v1/agents"}, {"POST", "/v1/sessions"}, {"POST", "/v1/memory_stores"},
		{"GET", "/v1/organizations/api_keys"}, {"POST", "/v1/complete"}, {"GET", "/v1/models/claude-opus-5-5"},
		{"DELETE", "/v1/models/claude-haiku-4-5"}, {"GET", "/v1/messages"}, {"POST", "/v1/models"},
		{"POST", "/v1/messages?beta=true"}, {"POST", "/v1/%6dessages"},
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
	other := newFakeAPI(t, replyJSON(textResponseWith("ok", 1, 1)))
	r2 := newRigFor(t, other, testConfig(other.srv.URL), r.api.srv.URL) // allowlist lists the OTHER server
	_, err := r2.client.Generate(ctx, testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrPermissionDenied)
	if ce.Details["sent"] != false || len(other.requests()) != 0 {
		t.Fatalf("details %v, %d requests reached the unlisted host", ce.Details, len(other.requests()))
	}
	// A client is never built without a dialer.
	if _, err := New(testConfig("https://api.anthropic.com"), nil, r.keys); err == nil {
		t.Fatal("a client was built without a dialer")
	}
}

func TestAPIKeyNeverLoggedOrReturned(t *testing.T) {
	leaky := func(_ int, w http.ResponseWriter, req *http.Request) {
		// The provider echoes the key everywhere it can.
		w.Header().Set("X-Echo", req.Header.Get("X-Api-Key"))
		if req.URL.Path == "/v1/models" {
			w.WriteHeader(401)
		} else {
			w.WriteHeader(400)
		}
		fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad key %s"},"request_id":"req_%s"}`, req.Header.Get("X-Api-Key"), req.Header.Get("X-Api-Key"))
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
	// The key did travel, once per call, in the x-api-key header only.
	for _, c := range append(r.api.requests(), ok.api.requests()...) {
		if c.Path == "/v1/models" && c.Header.Get("X-Api-Key") == "" {
			continue // the anonymous health probe
		}
		if c.Header.Get("X-Api-Key") != fixtureKey || c.Header.Get("Authorization") != "" {
			t.Errorf("request to %s: x-api-key %q authorization %q", c.Path, c.Header.Get("X-Api-Key"), c.Header.Get("Authorization"))
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
	if got := r2.api.requests()[0].Header.Get("X-Api-Key"); got != "rotated-fixture-credential-0002" {
		t.Fatalf("x-api-key %q", got)
	}
}

func TestCancellationStopsStream(t *testing.T) {
	block := make(chan struct{})
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, render("stream_start.sse", map[string]string{"IN": "5"}))
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
	if err := st.Close(); err != nil && !errors.Is(err, io.EOF) {
		_ = err // closing twice or after cancel is allowed to report the closed connection
	}
}
