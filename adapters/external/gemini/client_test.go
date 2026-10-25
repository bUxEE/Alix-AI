package gemini

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

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const bookArgs = `{"patient":"Luca Ferri","slot":"2026-10-14T10:30"}`

func TestFunctionCallsReturnedAsProposalsOnly(t *testing.T) {
	r := newRig(t, replyJSON(toolResponse("call_fx_1", "book", bookArgs, 90, 20)), nil)
	req := userReq("Book Luca Ferri for Wednesday.")
	req.Tools = []providers.ToolSpec{bookTool}
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	// The proposal is data: name, id and the arguments exactly as the model wrote them.
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "book" || res.ToolCalls[0].ID != "call_fx_1" ||
		string(res.ToolCalls[0].Arguments) != bookArgs || res.FinishReason != providers.FinishToolCalls {
		t.Fatalf("result %+v", res)
	}
	// One call, one request: the adapter never follows a proposal up.
	got := r.api.requests()
	if len(got) != 1 {
		t.Fatalf("%d upstream requests for one call", len(got))
	}
	body := got[0].json(t)
	tools := body["tools"].([]any)
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	d := decls[0].(map[string]any)
	if len(tools) != 1 || len(decls) != 1 || d["name"] != "book" || d["parametersJsonSchema"] == nil || d["parameters"] != nil {
		t.Fatalf("tools %v", tools)
	}
	if mode := body["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]; mode != ToolModeValidated {
		t.Fatalf("mode %v", mode)
	}

	// The same through the stream: the call arrives whole in one event.
	rs := newRig(t, replySSE(render("stream_tool.sse", func() map[string]string {
		kv := usageKV(90, 20)
		kv["CALL_ID"], kv["NAME"], kv["ARGS_RAW"] = jstr("call_fx_1"), jstr("book"), bookArgs
		return kv
	}())), nil)
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
		"a tool that was not offered":            {toolResponse("call_2", "delete_all_patients", `{}`, 9, 3), providers.ToolChoiceAuto},
		"a proposal after tools were turned off": {toolResponse("call_3", "book", bookArgs, 9, 3), providers.ToolChoiceNone},
		"arguments that are not an object":       {toolResponse("call_4", "book", `"x"`, 9, 3), providers.ToolChoiceAuto},
		"a code execution part":                  {render("hosted_tool.json", usageKV(9, 3)), providers.ToolChoiceAuto},
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

	// A required proposal is sent as ANY, a cancellation as NONE; neither adds a provider tool.
	for choice, want := range map[providers.ToolChoice]string{providers.ToolChoiceRequired: "ANY", providers.ToolChoiceNone: "NONE"} {
		rr := newRig(t, replyJSON(toolResponse("call_5", "book", bookArgs, 9, 3)), nil)
		q := userReq("book")
		q.Tools, q.ToolChoice = []providers.ToolSpec{bookTool}, choice
		_, _ = rr.client.Generate(context.Background(), testScope(), q)
		b := rr.api.requests()[0].json(t)
		if mode := b["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]; mode != want {
			t.Errorf("choice %q sent mode %v, want %s", choice, mode, want)
		}
	}
}

func TestCallWithoutIDGetsSynthesisedIDNotSentBack(t *testing.T) {
	noID := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"book","args":` + bookArgs + `}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3},"modelVersion":"m","responseId":"abc-123_xyz"}`
	r := newRig(t, replyJSON(noID), nil)
	req := userReq("book")
	req.Tools = []providers.ToolSpec{bookTool}
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	id := res.ToolCalls[0].ID
	if id != "gem_abc123xyz_0" {
		t.Fatalf("id %q", id)
	}
	// Echoed back with its result, the synthesised id is not sent to the API.
	follow := req
	follow.Messages = append(append([]providers.RichMessage(nil), req.Messages...),
		providers.RichMessage{Role: providers.MsgAssistant, ToolCalls: res.ToolCalls},
		providers.RichMessage{Role: providers.MsgTool, ToolCallID: id, Parts: []providers.ContentPart{providers.TextPart("done")}},
	)
	r2 := newRig(t, replyJSON(textResponseWith("ok", 9, 1)), nil)
	if _, err := r2.client.Generate(context.Background(), testScope(), follow); err != nil {
		t.Fatal(err)
	}
	contents := r2.api.requests()[0].json(t)["contents"].([]any)
	call := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
	resp := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if _, has := call["id"]; has || call["name"] != "book" {
		t.Fatalf("call %v", call)
	}
	if _, has := resp["id"]; has || resp["name"] != "book" || resp["response"].(map[string]any)["result"] != "done" {
		t.Fatalf("response %v", resp)
	}
}

func TestThoughtSignatureEchoedWithItsCall(t *testing.T) {
	r := newRig(t, replyJSON(toolResponse("call_sig_1", "book", bookArgs, 90, 20)), nil)
	req := userReq("book")
	req.Tools = []providers.ToolSpec{bookTool}
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	follow := req
	follow.Messages = append(append([]providers.RichMessage(nil), req.Messages...),
		providers.RichMessage{Role: providers.MsgAssistant, ToolCalls: res.ToolCalls},
		providers.RichMessage{Role: providers.MsgTool, ToolCallID: "call_sig_1", Parts: []providers.ContentPart{providers.TextPart("done")}},
	)
	r.api.handler = replyJSON(textResponseWith("ok", 9, 1))
	if _, err := r.client.Generate(context.Background(), testScope(), follow); err != nil {
		t.Fatal(err)
	}
	reqs := r.api.requests()
	part := reqs[1].json(t)["contents"].([]any)[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if part["thoughtSignature"] != "c2lnbmF0dXJlLWZpeHR1cmU=" || part["functionCall"].(map[string]any)["id"] != "call_sig_1" {
		t.Fatalf("part %v", part)
	}
	// another tenant does not receive it
	other := testScope()
	other.TenantID = "ten_other"
	if _, err := r.client.Generate(context.Background(), other, follow); err != nil {
		t.Fatal(err)
	}
	part = r.api.requests()[2].json(t)["contents"].([]any)[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if _, has := part["thoughtSignature"]; has {
		t.Fatal("a signature crossed tenants")
	}
	// and it never reaches a result or a log
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "c2lnbmF0") || strings.Contains(r.logs.String(), "c2lnbmF0") {
		t.Fatal("the signature leaked")
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
		// gemini-3.5-flash-lite: 0.30 USD in, 2.50 USD out per million tokens, in micro-USD.
		u := res.Usage
		if u.ToksIn != 1000 || u.ToksOut != 500 || u.CostMicros != 300+1250 || u.Currency != "USD" || !u.Estimated {
			t.Fatalf("usage %+v", u)
		}
		if res.TraceID != "FixtureResp0001" || res.Model != "gemini-3.5-flash-lite" {
			t.Fatalf("trace %q model %q", res.TraceID, res.Model)
		}
	})
	t.Run("thinking tokens bill as output", func(t *testing.T) {
		body := strings.Replace(textResponseWith("ok", 1000, 500), `"candidatesTokenCount"`, `"thoughtsTokenCount":200,"candidatesTokenCount"`, 1)
		r := newRig(t, replyJSON(body), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Usage.ToksOut != 700 || res.Usage.CostMicros != 300+1750 {
			t.Fatalf("usage %+v", res.Usage)
		}
	})
	t.Run("tool-use prompt tokens count as input", func(t *testing.T) {
		body := strings.Replace(textResponseWith("ok", 1000, 500), `"candidatesTokenCount"`, `"toolUsePromptTokenCount":40,"candidatesTokenCount"`, 1)
		r := newRig(t, replyJSON(body), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.Usage.ToksIn != 1040 {
			t.Fatalf("%+v %v", res.Usage, err)
		}
	})
	t.Run("a zero output count may be absent from the JSON", func(t *testing.T) {
		body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12},"modelVersion":"m","responseId":"r"}`
		r := newRig(t, replyJSON(body), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.Usage.ToksIn != 12 || res.Usage.ToksOut != 0 {
			t.Fatalf("%+v %v", res.Usage, err)
		}
	})
	t.Run("operator price table overrides the built-in one", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 2000, 1000)), func(c *Config) { c.Prices = usdTable(2_000_000, 8_000_000) })
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.Usage.CostMicros != 4000+8000 {
			t.Fatalf("%+v %v", res.Usage, err)
		}
	})
	t.Run("a model the table does not know has counts and no cost", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 10, 5)), func(c *Config) { c.Model = "gemini-future-9" })
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.Usage.ToksIn != 10 || res.Usage.ToksOut != 5 || res.Usage.CostMicros != 0 {
			t.Fatalf("%+v %v", res.Usage, err)
		}
	})
	t.Run("a response without usage is malformed, never a free success", func(t *testing.T) {
		body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],"modelVersion":"m","responseId":"r"}`
		r := newRig(t, replyJSON(body), nil)
		_, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("streamed: the last cumulative usage wins", func(t *testing.T) {
		r := newRig(t, replySSE(streamOf(1000, 500, "STOP", "Hello ", "there.")...), nil)
		req := userReq("hi")
		st, err := r.client.GenerateStream(ctx, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.CollectRich(st, req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Text() != "Hello there." || res.Usage.ToksIn != 1000 || res.Usage.ToksOut != 500 || res.Usage.CostMicros != 1550 {
			t.Fatalf("result %+v", res)
		}
		if res.TraceID == "" || res.Model != "gemini-3.5-flash-lite" {
			t.Fatalf("trace %q model %q", res.TraceID, res.Model)
		}
	})
}

func TestRequestShape(t *testing.T) {
	r := newRig(t, replyJSON(textResponseWith(`{"answer":"yes","confidence":3}`, 5, 5)), nil)
	req := userReq("hi")
	req.MaxToks, req.Temperature, req.Stop = 321, 0.5, []string{"END"}
	req.Output = providers.OutputConstraint{JSONSchema: json.RawMessage(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string","pattern":"^y"},"confidence":{"type":"integer","minimum":0}}}`)}
	res, err := r.client.Generate(context.Background(), testScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.JSON) != `{"answer":"yes","confidence":3}` {
		t.Fatalf("json %s", res.JSON)
	}
	c := r.api.requests()[0]
	if c.Method != "POST" || c.Path != "/v1beta/models/gemini-3.5-flash-lite:generateContent" || c.Query != "" {
		t.Fatalf("request %s %s ?%s", c.Method, c.Path, c.Query)
	}
	b := c.json(t)
	gc := b["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(321) || gc["temperature"] != 0.5 || gc["responseMimeType"] != "application/json" || gc["stopSequences"].([]any)[0] != "END" {
		t.Fatalf("generationConfig %v", gc)
	}
	schema := gc["responseJsonSchema"].(map[string]any)
	if schema["additionalProperties"] != false || strings.Contains(fmt.Sprint(schema), "pattern") {
		t.Fatalf("schema %v", schema)
	}
	si := b["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if si["text"] != "You propose typed tasks." {
		t.Fatalf("systemInstruction %v", si)
	}
	contents := b["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("contents %v", contents)
	}
	// no provider-native feature, safety override, thinking or cached content
	for _, k := range []string{"safetySettings", "cachedContent", "labels", "serviceTier", "store"} {
		if _, has := b[k]; has {
			t.Errorf("%s is set", k)
		}
	}
	if _, has := gc["thinkingConfig"]; has {
		t.Error("thinkingConfig is set")
	}
	if c.Header.Get("X-Goog-Api-Key") != fixtureKey || c.Header.Get("Authorization") != "" || c.Header.Get("X-Goog-User-Project") != "" {
		t.Fatalf("headers %v", c.Header)
	}

	// no cap from the caller: the default bounds the call
	r2 := newRig(t, replyJSON(textResponseWith("x", 1, 1)), nil)
	if _, err := r2.client.Generate(context.Background(), testScope(), userReq("hi")); err != nil {
		t.Fatal(err)
	}
	if got := r2.api.requests()[0].json(t)["generationConfig"].(map[string]any)["maxOutputTokens"]; got != float64(4096) {
		t.Fatalf("default cap %v", got)
	}

	// a profile hint selects another model of the same configuration, and only that
	r3 := newRig(t, replyJSON(textResponseWith("x", 1, 1)), func(c *Config) { c.Profiles = map[string]string{"big": "gemini-3.8-flash"} })
	q := userReq("hi")
	q.Profile = "big"
	if _, err := r3.client.Generate(context.Background(), testScope(), q); err != nil {
		t.Fatal(err)
	}
	if p := r3.api.requests()[0].Path; p != "/v1beta/models/gemini-3.8-flash:generateContent" {
		t.Fatalf("path %s", p)
	}

	// streaming uses the stream route with the constant alt=sse query and nothing else
	r4 := newRig(t, replySSE(streamOf(5, 5, "STOP", "x")...), nil)
	st, err := r4.client.GenerateStream(context.Background(), testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if c := r4.api.requests()[0]; c.Path != "/v1beta/models/gemini-3.5-flash-lite:streamGenerateContent" || c.Query != "alt=sse" || c.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("stream request %s ?%s", c.Path, c.Query)
	}
}

func TestToolModeAutoWhenConfigured(t *testing.T) {
	r := newRig(t, replyJSON(textResponseWith("x", 1, 1)), func(c *Config) { c.ToolMode = ToolModeAuto })
	req := userReq("hi")
	req.Tools = []providers.ToolSpec{bookTool}
	if _, err := r.client.Generate(context.Background(), testScope(), req); err != nil {
		t.Fatal(err)
	}
	b := r.api.requests()[0].json(t)
	if mode := b["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]; mode != "AUTO" {
		t.Fatalf("mode %v", mode)
	}
	// the capability record stops claiming strict arguments
	caps, _ := r.client.Capabilities(context.Background())
	if caps.Tools.StrictArguments {
		t.Fatal("AUTO mode claims strict arguments")
	}
}

func TestRefusedBeforeSending(t *testing.T) {
	text := func(s string) []providers.ContentPart { return []providers.ContentPart{providers.TextPart(s)} }
	user := providers.RichMessage{Role: providers.MsgUser, Parts: text("hi")}
	bad := map[string]providers.RichRequest{
		"an image input": {Messages: []providers.RichMessage{{Role: providers.MsgUser, Parts: []providers.ContentPart{providers.TextPart("x"), providers.ImageRef("img_1", "image/png")}}}},
		"a grammar": func() providers.RichRequest {
			q := userReq("hi")
			q.Output = providers.OutputConstraint{Grammar: `root ::= "x"`}
			return q
		}(),
		"a temperature above 2": func() providers.RichRequest {
			q := userReq("hi")
			q.Temperature = 2.5
			return q
		}(),
		"six stop sequences": func() providers.RichRequest {
			q := userReq("hi")
			q.Stop = []string{"a", "b", "c", "d", "e", "f"}
			return q
		}(),
		"an unknown profile":                              func() providers.RichRequest { q := userReq("hi"); q.Profile = "nope"; return q }(),
		"a conversation that ends with an assistant turn": {Messages: []providers.RichMessage{user, {Role: providers.MsgAssistant, Parts: text("x")}}},
		"a system message after the first turn":           {Messages: []providers.RichMessage{user, {Role: providers.MsgSystem, Trusted: true, Parts: text("late")}, user}},
		"a tool result that answers nothing": {Messages: []providers.RichMessage{user, {Role: providers.MsgTool, ToolCallID: "call_x", Parts: text("done")}, user},
			Tools: []providers.ToolSpec{bookTool}},
		"structured output together with tools": func() providers.RichRequest {
			q := userReq("hi")
			q.Tools = []providers.ToolSpec{bookTool}
			q.Output = providers.OutputConstraint{JSONSchema: json.RawMessage(bookSchema)}
			return q
		}(),
		"an open map in a tool schema": func() providers.RichRequest {
			q := userReq("hi")
			q.Tools = []providers.ToolSpec{{Name: "t", Parameters: json.RawMessage(`{"type":"object","additionalProperties":{"type":"string"}}`)}}
			return q
		}(),
		"more output than the model has": func() providers.RichRequest { q := userReq("hi"); q.MaxToks = 1 << 20; return q }(),
	}
	for name, req := range bad {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, replyJSON(textResponseWith("x", 1, 1)), nil)
			_, err := r.client.Generate(context.Background(), testScope(), req)
			ce := wantCode(t, err, contracts.ErrValidationFailed)
			if ce.Details["sent"] != false || len(r.api.requests()) != 0 || r.keys.calls != 0 {
				t.Fatalf("details %v, %d requests, %d key resolutions", ce.Details, len(r.api.requests()), r.keys.calls)
			}
			if st, err := r.client.GenerateStream(context.Background(), testScope(), req); err == nil {
				_ = st.Close()
				t.Fatal("the stream path accepted it")
			}
		})
	}
	// a request with no scope is refused before the credential is read
	r := newRig(t, replyJSON(textResponseWith("x", 1, 1)), nil)
	if _, err := r.client.Generate(context.Background(), contracts.Scope{}, userReq("hi")); err == nil || r.keys.calls != 0 {
		t.Fatalf("scope-less call: %v", err)
	}
}

func TestFinishReasons(t *testing.T) {
	ctx := context.Background()
	reply := func(parts, finish string) string {
		f := ""
		if finish != "" {
			f = `,"finishReason":"` + finish + `"`
		}
		return `{"candidates":[{"content":{"role":"model","parts":[` + parts + `]}` + f + `}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3},"modelVersion":"m","responseId":"r"}`
	}
	for finish, want := range map[string]providers.FinishReason{
		"STOP": providers.FinishStop, "MAX_TOKENS": providers.FinishLength, "SAFETY": providers.FinishFiltered,
		"RECITATION": providers.FinishFiltered, "PROHIBITED_CONTENT": providers.FinishFiltered,
	} {
		r := newRig(t, replyJSON(reply(`{"text":"x"}`, finish)), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.FinishReason != want {
			t.Errorf("%s: %+v %v", finish, res.FinishReason, err)
		}
	}
	for _, finish := range []string{"MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "MISSING_THOUGHT_SIGNATURE", "OTHER", "SOMETHING_NEW", ""} {
		r := newRig(t, replyJSON(reply(`{"text":"x"}`, finish)), nil)
		_, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		wantCode(t, err, contracts.ErrSchemaInvalid)
	}
	t.Run("a refused prompt is filtered, with its usage", func(t *testing.T) {
		body := `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":9},"modelVersion":"m","responseId":"r"}`
		r := newRig(t, replyJSON(body), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.FinishReason != providers.FinishFiltered || res.Usage.ToksIn != 9 || res.Text() != "" {
			t.Fatalf("%+v %v", res, err)
		}
		// under a schema it is an error, never a result
		q := userReq("hi")
		q.Output = providers.OutputConstraint{JSONSchema: json.RawMessage(bookSchema)}
		_, err = r.client.Generate(ctx, testScope(), q)
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("thought parts are not carried", func(t *testing.T) {
		r := newRig(t, replyJSON(reply(`{"thought":true,"text":"hmm"},{"text":"Hi","thoughtSignature":"c2ln"}`, "STOP")), nil)
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.Text() != "Hi" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("parts and metadata that were not requested reject the answer", func(t *testing.T) {
		for name, body := range map[string]string{
			"inline data":    reply(`{"inlineData":{"mimeType":"image/png","data":"AA=="}}`, "STOP"),
			"file data":      reply(`{"fileData":{"mimeType":"application/pdf","fileUri":"x"}}`, "STOP"),
			"two candidates": `{"candidates":[{"content":{"parts":[{"text":"a"}]},"finishReason":"STOP"},{"content":{"parts":[{"text":"b"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1}}`,
			"grounding":      `{"candidates":[{"content":{"parts":[{"text":"a"}]},"finishReason":"STOP","groundingMetadata":{"webSearchQueries":["x"]}}],"usageMetadata":{"promptTokenCount":1}}`,
			"no candidate":   `{"usageMetadata":{"promptTokenCount":1}}`,
		} {
			r := newRig(t, replyJSON(body), nil)
			_, err := r.client.Generate(ctx, testScope(), userReq("hi"))
			if err == nil {
				t.Errorf("%s accepted", name)
			}
		}
	})
	t.Run("a truncated structured answer fails to parse", func(t *testing.T) {
		r := newRig(t, replyJSON(reply(`{"text":"{\"answer\": \"ye"}`, "MAX_TOKENS")), nil)
		q := userReq("hi")
		q.Output = providers.OutputConstraint{JSONSchema: json.RawMessage(bookSchema)}
		_, err := r.client.Generate(ctx, testScope(), q)
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
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
	t.Run("a stream that ends before a finishReason is an error", func(t *testing.T) {
		parts := streamOf(5, 5, "STOP", "partial")
		_, err := collect(t, parts[:1])
		ce := wantCode(t, err, contracts.ErrUnavailable)
		if ce.Details["provider_error_kind"] != "connection_lost" || ce.Details["sent"] != true {
			t.Fatalf("details %v", ce.Details)
		}
	})
	t.Run("an error object after a 200 is mapped by its status", func(t *testing.T) {
		parts := append(streamOf(5, 5, "STOP", "partial")[:1], render("stream_error.sse", map[string]string{"MARKER": "MARK-9"}))
		_, err := collect(t, parts)
		ce := wantCode(t, err, contracts.ErrUnavailable)
		if ce.Details["provider_error_kind"] != "unavailable" || strings.Contains(fmt.Sprint(ce), "MARK-9") {
			t.Fatalf("error %v %v", ce, ce.Details)
		}
	})
	t.Run("a code execution part is rejected", func(t *testing.T) {
		_, err := collect(t, []string{render("stream_hosted_tool.sse", usageKV(5, 0))})
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("carriage-return line ends and comments are read", func(t *testing.T) {
		var parts []string
		for _, p := range streamOf(5, 5, "STOP", "Hi ", "there") {
			parts = append(parts, strings.ReplaceAll(p, "\n", "\r\n"))
		}
		res, err := collect(t, append([]string{": keep-alive\r\n\r\n"}, parts...))
		if err != nil || res.Text() != "Hi there" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("a blocked prompt ends the stream filtered", func(t *testing.T) {
		res, err := collect(t, []string{`data: {"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":9},"modelVersion":"m","responseId":"r"}` + "\n\n"})
		if err != nil || res.FinishReason != providers.FinishFiltered {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("a stream without usage is malformed", func(t *testing.T) {
		_, err := collect(t, []string{`data: {"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}` + "\n\n"})
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("a call with no arguments streams {}", func(t *testing.T) {
		ev := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1},"modelVersion":"m","responseId":"r9"}` + "\n\n"
		r := newRig(t, replySSE(ev), nil)
		req := userReq("go")
		req.Tools = []providers.ToolSpec{{Name: "ping", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)}}
		st, err := r.client.GenerateStream(ctx, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		res, err := providers.CollectRich(st, req)
		if err != nil || len(res.ToolCalls) != 1 || string(res.ToolCalls[0].Arguments) != "{}" || res.ToolCalls[0].ID != "gem_r9_0" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("close is idempotent and ends Recv", func(t *testing.T) {
		r := newRig(t, replySSE(streamOf(5, 5, "STOP", "x")...), nil)
		st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
		if err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		_ = st.Close()
		if _, err := st.Recv(); !errors.Is(err, providers.ErrStreamClosed) {
			t.Fatalf("Recv after Close: %v", err)
		}
	})
}

func TestErrorMapping(t *testing.T) {
	body := func(code int, status, extra string) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":"MARK-7 detail","status":%q%s}}`, code, status, extra)
	}
	info := func(reason string) string {
		return `,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"` + reason + `"}]`
	}
	for name, tc := range map[string]struct {
		status int
		body   string
		code   contracts.ErrCode
		kind   string
		hits   int
	}{
		"an invalid key is a 400 with a reason": {400, body(400, "INVALID_ARGUMENT", info("API_KEY_INVALID")), contracts.ErrPermissionDenied, "authentication_failed", 1},
		"an expired token":                      {401, body(401, "UNAUTHENTICATED", ""), contracts.ErrPermissionDenied, "authentication_failed", 1},
		"a leaked key blocked":                  {403, body(403, "PERMISSION_DENIED", ""), contracts.ErrPermissionDenied, "permission_denied", 1},
		"a project without billing or region":   {400, body(400, "FAILED_PRECONDITION", ""), contracts.ErrPermissionDenied, "permission_denied", 1},
		"an unknown model":                      {404, body(404, "NOT_FOUND", ""), contracts.ErrValidationFailed, "model_not_found", 1},
		"a bad request":                         {400, body(400, "INVALID_ARGUMENT", ""), contracts.ErrValidationFailed, "bad_request", 1},
		"a prompt over the token limit": {400, `{"error":{"code":400,"message":"The input token count (2000000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
			contracts.ErrValidationFailed, "context_too_large", 1},
		"a daily quota, never retried": {429, body(429, "RESOURCE_EXHAUSTED", `,"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel"}]}]`),
			contracts.ErrResourceExhausted, "quota_exceeded", 1},
		"depleted prepayment credits":              {429, `{"error":{"code":429,"message":"Your prepayment credits are depleted.","status":"RESOURCE_EXHAUSTED"}}`, contracts.ErrResourceExhausted, "quota_exceeded", 1},
		"a rate limit is retried within the bound": {429, body(429, "RESOURCE_EXHAUSTED", ""), contracts.ErrRateLimited, "rate_limited", 3},
		"overload is retried within the bound":     {503, body(503, "UNAVAILABLE", ""), contracts.ErrUnavailable, "unavailable", 3},
		"an internal error is not retried":         {500, body(500, "INTERNAL", ""), contracts.ErrUnavailable, "unavailable", 1},
		"a deadline":                               {504, body(504, "DEADLINE_EXCEEDED", ""), contracts.ErrTimeout, "timeout", 1},
		"a redirect is not followed":               {302, ``, contracts.ErrUnavailable, "malformed_response", 1},
		"an unreadable error body":                 {400, `<html>MARK-7</html>`, contracts.ErrValidationFailed, "bad_request", 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, replyStatus(tc.status, tc.body), nil)
			_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
			ce := wantCode(t, err, tc.code)
			if ce.Details["provider_error_kind"] != tc.kind || ce.Details["sent"] != true {
				t.Fatalf("details %v", ce.Details)
			}
			if n := len(r.api.requests()); n != tc.hits {
				t.Fatalf("%d requests, want %d", n, tc.hits)
			}
			if strings.Contains(fmt.Sprintf("%v %+v %v", ce, ce, ce.Details), "MARK-7") {
				t.Fatal("the provider's message was copied into the error")
			}
		})
	}
}

func TestRateLimitMappedWithRetryAfter(t *testing.T) {
	r := newRig(t, replyStatus(429, render("error_rate_limit.json", map[string]string{"MARKER": "M", "RETRY_DELAY": "7.5s"})), nil)
	_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrRateLimited)
	info, ok := providers.RateLimitFrom(err)
	if !ok || info.RetryAfter != 7500*time.Millisecond {
		t.Fatalf("info %+v", info)
	}
	// each retry waited at least the provider's request
	ss := r.sleeps.all()
	if len(ss) != 2 || ss[0] < 7500*time.Millisecond || ss[1] < 7500*time.Millisecond {
		t.Fatalf("sleeps %v", ss)
	}
	// a wait longer than the bound is not sat out
	r2 := newRig(t, replyStatus(429, render("error_rate_limit.json", map[string]string{"MARKER": "M", "RETRY_DELAY": "120s"})), nil)
	_, err = r2.client.Generate(context.Background(), testScope(), userReq("hi"))
	wantCode(t, err, contracts.ErrRateLimited)
	if len(r2.api.requests()) != 1 || len(r2.sleeps.all()) != 0 {
		t.Fatalf("%d requests, sleeps %v", len(r2.api.requests()), r2.sleeps.all())
	}
	// a Retry-After header wins over the body
	r3 := newRig(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, render("error_rate_limit.json", map[string]string{"MARKER": "M", "RETRY_DELAY": "9s"}))
	}, nil)
	_, err = r3.client.Generate(context.Background(), testScope(), userReq("hi"))
	if info, _ := providers.RateLimitFrom(err); info.RetryAfter != 3*time.Second {
		t.Fatalf("info %+v", info)
	}
}

func TestRetryRecoversAndDoesNotRetryAfterBytesSent(t *testing.T) {
	r := newRig(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, render("error_unavailable.json", map[string]string{"MARKER": "M"}))
			return
		}
		_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
	}, nil)
	res, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	if err != nil || res.Text() != "ok" || len(r.api.requests()) != 2 {
		t.Fatalf("%+v %v %d", res, err, len(r.api.requests()))
	}
	// a dropped connection after the request was sent is not retried here
	r2 := newRig(t, func(_ int, w http.ResponseWriter, _ *http.Request) { panic(http.ErrAbortHandler) }, nil)
	_, err = r2.client.Generate(context.Background(), testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrUnavailable)
	if ce.Details["sent"] != true || len(r2.api.requests()) != 1 {
		t.Fatalf("details %v, %d requests", ce.Details, len(r2.api.requests()))
	}
}

func TestCapabilitiesDeclaredAtRuntime(t *testing.T) {
	r := newRig(t, nil, nil)
	caps, err := r.client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Provider != "gemini" || caps.Model != testModel || !caps.Streaming || caps.StructuredOutput != providers.StructuredStrict ||
		!caps.Tools.Supported || !caps.Tools.StrictArguments || caps.Tools.MaxTools != 32 || caps.MaxOutputToks != 65536 || caps.MaxContextToks != 1_048_576 ||
		caps.Pricing == nil || len(caps.InputModalities) != 1 || caps.ImageGeneration || caps.Grammar {
		t.Fatalf("caps %+v", caps)
	}
	if len(r.api.requests()) != 0 {
		t.Fatal("capability discovery made a request")
	}
	r2 := newRig(t, nil, func(c *Config) { c.MaxOutputToks = 8000 })
	caps, _ = r2.client.Capabilities(context.Background())
	if caps.MaxOutputToks != 8000 {
		t.Fatalf("caps %+v", caps)
	}
}

func TestHealthAndCheckModel(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Goog-Api-Key") == "" {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"unregistered callers","status":"PERMISSION_DENIED"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"models/gemini-3.5-flash-lite","displayName":"x"}`)
	}, nil)
	if h, err := r.client.Health(ctx); err != nil || h.Status != providers.HealthOK {
		t.Fatalf("%+v %v", h, err)
	}
	if err := r.client.CheckModel(ctx, testScope()); err != nil {
		t.Fatal(err)
	}
	got := r.api.requests()
	if got[0].Path != "/v1beta/models" || got[0].Header.Get("X-Goog-Api-Key") != "" || got[1].Path != "/v1beta/models/gemini-3.5-flash-lite" || got[1].Header.Get("X-Goog-Api-Key") != fixtureKey {
		t.Fatalf("requests %+v", got)
	}
	r2 := newRig(t, replyStatus(404, `{"error":{"code":404,"message":"models/x is not found","status":"NOT_FOUND"}}`), nil)
	ce := wantCode(t, r2.client.CheckModel(ctx, testScope()), contracts.ErrValidationFailed)
	if ce.Details["provider_error_kind"] != "model_not_found" {
		t.Fatalf("details %v", ce.Details)
	}
	// a rate-limited probe is degraded; an unreachable service is unavailable, not ok
	r4 := newRig(t, replyStatus(429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`), nil)
	if h, _ := r4.client.Health(ctx); h.Status != providers.HealthDegraded {
		t.Fatalf("health %+v", h)
	}
	r3 := newRig(t, nil, nil)
	r3.api.srv.Close()
	if h, _ := r3.client.Health(ctx); h.Status != providers.HealthUnavailable {
		t.Fatalf("health %+v", h)
	}
}

func TestOAuthAndAPIKeyBothSupported(t *testing.T) {
	// The adapter declares both kinds, browser OAuth first (additive plan 11.1).
	if got := AuthSupport(); len(got) != 2 || got[0] != AuthOAuth || got[1] != AuthAPIKey {
		t.Fatalf("AuthSupport %v", got)
	}
	ctx := context.Background()

	t.Run("API key", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), nil)
		if r.client.Auth() != AuthAPIKey {
			t.Fatalf("auth %s", r.client.Auth())
		}
		if _, err := r.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
			t.Fatal(err)
		}
		h := r.api.requests()[0].Header
		if h.Get("X-Goog-Api-Key") != fixtureKey || h.Get("Authorization") != "" || h.Get("X-Goog-User-Project") != "" {
			t.Fatalf("headers %v", h)
		}
		if len(r.tokens.forced) != 0 {
			t.Fatal("the OAuth engine was asked for an API-key connection")
		}
	})

	t.Run("OAuth", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), func(c *Config) { c.Auth, c.UserProject = AuthOAuth, "my-clinic-project" })
		if r.client.Auth() != AuthOAuth {
			t.Fatalf("auth %s", r.client.Auth())
		}
		if _, err := r.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
			t.Fatal(err)
		}
		h := r.api.requests()[0].Header
		if h.Get("Authorization") != "Bearer "+fixtureToken || h.Get("X-Goog-User-Project") != "my-clinic-project" || h.Get("X-Goog-Api-Key") != "" {
			t.Fatalf("headers %v", h)
		}
		// the smallest scope set the vendor names, and no forced refresh on a normal call
		if len(r.tokens.scopes) != 1 || fmt.Sprint(r.tokens.scopes[0]) != fmt.Sprint(InferenceScopes) || r.tokens.forced[0] {
			t.Fatalf("token requests %v %v", r.tokens.scopes, r.tokens.forced)
		}
		if r.keys.calls != 0 {
			t.Fatal("the API-key source was used for an OAuth connection")
		}
		// the token is in the header and nowhere else
		c := r.api.requests()[0]
		if strings.Contains(string(c.Body), fixtureToken) || strings.Contains(c.Query, fixtureToken) || strings.Contains(r.logs.String(), fixtureToken) {
			t.Fatal("the token leaked")
		}
	})

	t.Run("OAuth: a 401 forces one refresh and one retry", func(t *testing.T) {
		r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer "+fixtureToken+"-fresh" {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, render("error_unauth.json", map[string]string{"MARKER": "M", "KEY": "k"}))
				return
			}
			_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
		}, func(c *Config) { c.Auth = AuthOAuth })
		res, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		if err != nil || res.Text() != "ok" {
			t.Fatalf("%+v %v", res, err)
		}
		if fmt.Sprint(r.tokens.forced) != "[false true]" || len(r.api.requests()) != 2 {
			t.Fatalf("forced %v, %d requests", r.tokens.forced, len(r.api.requests()))
		}
		// a second 401 after the refresh is final, not a loop
		r2 := newRig(t, replyStatus(401, render("error_unauth.json", map[string]string{"MARKER": "M", "KEY": "k"})), func(c *Config) { c.Auth = AuthOAuth })
		_, err = r2.client.Generate(ctx, testScope(), userReq("hi"))
		ce := wantCode(t, err, contracts.ErrPermissionDenied)
		if ce.Details["provider_error_kind"] != "authentication_failed" || len(r2.api.requests()) != 2 {
			t.Fatalf("details %v, %d requests", ce.Details, len(r2.api.requests()))
		}
	})

	t.Run("OAuth: only the attempt right after a 401 forces a refresh", func(t *testing.T) {
		r := newRig(t, func(n int, w http.ResponseWriter, _ *http.Request) {
			switch n {
			case 1:
				w.WriteHeader(401)
				_, _ = io.WriteString(w, render("error_unauth.json", map[string]string{"MARKER": "M", "KEY": "k"}))
			case 2:
				w.WriteHeader(429)
				_, _ = io.WriteString(w, render("error_rate_limit.json", map[string]string{"MARKER": "M", "RETRY_DELAY": "1s"}))
			default:
				_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
			}
		}, func(c *Config) { c.Auth = AuthOAuth })
		if _, err := r.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(r.tokens.forced) != "[false true false]" {
			t.Fatalf("forced %v", r.tokens.forced)
		}
	})

	t.Run("OAuth: an engine refusal fails closed and sends nothing", func(t *testing.T) {
		r := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), func(c *Config) { c.Auth = AuthOAuth })
		r.tokens.err = contracts.New(contracts.ErrPermissionDenied, "reauthorization required")
		_, err := r.client.Generate(ctx, testScope(), userReq("hi"))
		ce := wantCode(t, err, contracts.ErrPermissionDenied)
		if ce.Details["sent"] != false || ce.Details["provider_error_kind"] != "authentication_failed" || len(r.api.requests()) != 0 {
			t.Fatalf("details %v, %d requests", ce.Details, len(r.api.requests()))
		}
	})

	t.Run("construction needs the matching credential source", func(t *testing.T) {
		api := newFakeAPI(t, nil)
		dest, err := egress.DestinationFromURL(api.srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		al, _ := egress.NewAllowlist(dest)
		reg := egress.NewProviderRegistry()
		_ = reg.Set(ProviderName, al)
		oauthCfg := testConfig(api.srv.URL)
		oauthCfg.Auth = AuthOAuth
		if _, err := New(oauthCfg, reg.Dialer(ProviderName), &fakeKeys{key: fixtureKey}); err == nil {
			t.Error("an OAuth connection was built with only a key source")
		}
		if _, err := New(testConfig(api.srv.URL), reg.Dialer(ProviderName), nil, WithTokenSource(&fakeTokens{})); err == nil {
			t.Error("an API key connection was built with only a token source")
		}
		if _, err := New(testConfig(api.srv.URL), nil, &fakeKeys{key: fixtureKey}); err == nil {
			t.Error("a client was built without an egress dialer")
		}
	})
}

func TestAPIKeyNeverLoggedOrReturned(t *testing.T) {
	for _, auth := range []AuthKind{AuthAPIKey, AuthOAuth} {
		t.Run(string(auth), func(t *testing.T) {
			secret := fixtureKey
			if auth == AuthOAuth {
				secret = fixtureToken
			}
			leaky := func(_ int, w http.ResponseWriter, req *http.Request) {
				// The provider echoes the credential everywhere it can.
				echo := req.Header.Get("X-Goog-Api-Key") + strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
				w.Header().Set("X-Echo", echo)
				if req.URL.Path == "/v1beta/models" {
					w.WriteHeader(403)
				} else {
					w.WriteHeader(400)
				}
				fmt.Fprintf(w, `{"error":{"code":400,"message":"bad credential %s","status":"INVALID_ARGUMENT"}}`, echo)
			}
			r := newRig(t, leaky, func(c *Config) { c.Auth = auth })
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
			ok := newRig(t, replyJSON(textResponseWith("ok", 1000, 500)), func(c *Config) { c.Auth = auth })
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
				if strings.Contains(s, secret) {
					t.Errorf("surface %d holds the credential: %.200s", i, s)
				}
			}
			// The credential did travel, once per call, in its header only.
			for _, c := range append(r.api.requests(), ok.api.requests()...) {
				if c.Path == "/v1beta/models" && c.Header.Get("X-Goog-Api-Key") == "" && c.Header.Get("Authorization") == "" {
					continue // the anonymous health probe
				}
				carried := c.Header.Get("X-Goog-Api-Key")
				other := c.Header.Get("Authorization")
				if auth == AuthOAuth {
					carried, other = strings.TrimPrefix(c.Header.Get("Authorization"), "Bearer "), c.Header.Get("X-Goog-Api-Key")
				}
				if carried != secret || other != "" {
					t.Errorf("request to %s: credential %q other header %q", c.Path, carried, other)
				}
				if strings.Contains(string(c.Body), secret) || strings.Contains(c.Query, secret) || strings.Contains(c.Path, secret) {
					t.Errorf("the credential is in a request body or URL")
				}
			}
			if auth == AuthAPIKey {
				// a refused credential reads as authentication failure with nothing sent
				r.keys.err = contracts.New(contracts.ErrPermissionDenied, "revoked")
				_, err = r.client.Generate(ctx, testScope(), userReq("hi"))
				ce := wantCode(t, err, contracts.ErrPermissionDenied)
				if ce.Details["sent"] != false {
					t.Fatalf("details %v", ce.Details)
				}
			}
		})
	}
	// a value that is not shaped like a credential is refused, not sent
	r := newRig(t, replyJSON(textResponseWith("ok", 1, 1)), nil)
	r.keys.key = "has a space\nand a newline"
	_, err := r.client.Generate(context.Background(), testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrPermissionDenied)
	if ce.Details["sent"] != false || len(r.api.requests()) != 0 {
		t.Fatalf("details %v", ce.Details)
	}
	_ = credentials.Secret("")
}

func TestOnlyAllowlistedEndpointsCalled(t *testing.T) {
	want := []string{
		"POST /v1beta/models/{configured model}:generateContent",
		"POST /v1beta/models/{configured model}:streamGenerateContent?alt=sse",
		"GET /v1beta/models",
		"GET /v1beta/models/{configured model}",
	}
	if got := AllowedEndpoints(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("routes %v", got)
	}
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/v1beta/models":
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"x","status":"PERMISSION_DENIED"}}`)
		case req.URL.Path == "/v1beta/models/"+testModel:
			_, _ = io.WriteString(w, `{"name":"models/`+testModel+`"}`)
		case strings.HasSuffix(req.URL.Path, ":streamGenerateContent"):
			_, _ = io.WriteString(w, strings.Join(streamOf(1, 1, "STOP", "ok"), ""))
		default:
			_, _ = io.WriteString(w, textResponseWith("ok", 1, 1))
		}
	}, nil)
	ctx := context.Background()
	if _, err := r.client.Generate(ctx, testScope(), userReq("hi")); err != nil {
		t.Fatal(err)
	}
	st, err := r.client.GenerateStream(ctx, testScope(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providers.CollectRich(st, userReq("hi")); err != nil {
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
		if c.Query != "" && c.Query != "alt=sse" {
			t.Errorf("an unexpected query string was sent: %s", c.Query)
		}
		seen[c.Method+" "+c.Path+"?"+c.Query] = true
	}
	for _, w := range []string{
		"POST /v1beta/models/" + testModel + ":generateContent?",
		"POST /v1beta/models/" + testModel + ":streamGenerateContent?alt=sse",
		"GET /v1beta/models?",
		"GET /v1beta/models/" + testModel + "?",
	} {
		if !seen[w] {
			t.Errorf("route %s never used", w)
		}
	}
	if len(seen) != 4 {
		t.Errorf("routes used %v", seen)
	}

	// Anything else is refused before any I/O, whatever the method: cached
	// contents, files, tuned models, batches, embeddings, the live API, other
	// models, a key in the query string, and an encoded path.
	n := len(r.api.requests())
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1beta/cachedContents"}, {"GET", "/v1beta/files"}, {"POST", "/upload/v1beta/files"}, {"GET", "/v1beta/tunedModels"},
		{"POST", "/v1beta/models/" + testModel + ":batchGenerateContent"}, {"POST", "/v1beta/models/" + testModel + ":embedContent"},
		{"POST", "/v1beta/models/" + testModel + ":countTokens"}, {"POST", "/v1beta/models/gemini-2.5-pro:generateContent"},
		{"GET", "/v1beta/models/gemini-2.5-pro"}, {"DELETE", "/v1beta/models/" + testModel}, {"GET", "/v1beta/models/" + testModel + ":generateContent"},
		{"POST", "/v1beta/models/" + testModel + ":generateContent?key=" + fixtureKey}, {"POST", "/v1beta/models/" + testModel + ":generateContent?alt=sse"},
		{"POST", "/v1beta/models/" + testModel + ":streamGenerateContent"}, {"POST", "/v1beta/models/" + testModel + ":streamGenerateContent?alt=sse&key=x"},
		{"POST", "/v1alpha/models/" + testModel + ":generateContent"}, {"POST", "/v1beta/models/%67emini-3.5-flash-lite:generateContent"},
		{"POST", "/v1beta/models"}, {"POST", "/v1beta/interactions"},
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
	_, err = r2.client.Generate(ctx, testScope(), userReq("hi"))
	ce := wantCode(t, err, contracts.ErrPermissionDenied)
	if ce.Details["sent"] != false || len(other.requests()) != 0 {
		t.Fatalf("details %v, %d requests reached the unlisted host", ce.Details, len(other.requests()))
	}
}

func TestCancellationStopsStream(t *testing.T) {
	block := make(chan struct{})
	r := newRig(t, func(_ int, w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		kv := usageKV(5, 0)
		kv["TEXT"] = jstr("a")
		_, _ = io.WriteString(w, render("stream_text.sse", kv))
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
	if ch, err := st.Recv(); err != nil || ch.Text != "a" {
		t.Fatalf("first chunk %+v %v", ch, err)
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
