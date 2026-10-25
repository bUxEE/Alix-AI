package providersuite

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// UpstreamMarker is embedded in the body of every scripted upstream failure.
// A provider must classify a failure from the status and the error type and
// must never copy the body into an error, a detail or a log: a body can echo
// prompt text and credentials.
const UpstreamMarker = "UPSTREAM-BODY-MARKER-7f3a"

// ReplyText is the text the inference checks script.
const ReplyText = "Hello from the fixture, streamed in several pieces."

func inferenceChecks() []Check {
	return []Check{
		{"inference", "text_generation_and_streaming", checkTextAndStreaming},
		{"inference", "bounded_context", checkBoundedContext},
		{"inference", "structured_output", checkStructuredOutput},
		{"inference", "cancellation_and_timeout", checkCancellationAndTimeout},
		{"inference", "usage_reporting", checkUsageReporting},
		{"inference", "malformed_and_error_mapping", checkMalformedAndErrorMapping},
	}
}

// 15.1 text generation and streaming: a complete answer arrives intact, once
// whole and once as an ordered stream of pieces.
func checkTextAndStreaming(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	s.Upstream.Script(Scenario{Kind: ScText, Text: ReplyText, ToksIn: 120, ToksOut: 30})
	res, err := generate(ctx, s, sc, textReq("Say hello."))
	if err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	if !wantText(s, ReplyText, res.Text()) {
		return fail("text_mismatch", "the answer is not the one the upstream gave")
	}
	if res.FinishReason != providers.FinishStop {
		return fail("finish_reason_wrong", "finish reason %q for a complete answer", res.FinishReason)
	}
	s.Upstream.Script(Scenario{Kind: ScText, Text: ReplyText, ToksIn: 120, ToksOut: 30})
	sres, err := stream(ctx, s, sc, textReq("Say hello."))
	if err != nil {
		return fail("stream_failed", "the stream failed: code %s", codeOf(err))
	}
	if !wantText(s, ReplyText, sres.Text()) {
		return fail("stream_text_mismatch", "the streamed answer is not the one the upstream gave")
	}
	return pass()
}

// 15.1 bounded context: a request over the model's output limit is refused
// before anything is sent, and a request that names no cap still goes out
// with one.
func checkBoundedContext(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	caps, err := s.Caps.Capabilities(ctx)
	if err != nil {
		return fail("capabilities_unavailable", "Capabilities failed")
	}
	before := len(s.Upstream.Seen())
	over := textReq("hi")
	over.MaxToks = caps.MaxOutputToks + 1
	if _, err := generate(ctx, s, sc, over); err == nil {
		return fail("over_limit_accepted", "a request over the output limit was accepted")
	}
	if len(s.Upstream.Seen()) != before {
		return fail("over_limit_sent", "a request over the output limit reached the upstream")
	}
	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 5, ToksOut: 1})
	open := textReq("hi")
	open.MaxToks = 0
	if _, err := generate(ctx, s, sc, open); err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	seen := s.Upstream.Seen()
	if len(seen) == before {
		return fail("request_not_seen", "the upstream saw nothing")
	}
	if got := seen[len(seen)-1].MaxOutputToks; got <= 0 || got > caps.MaxOutputToks {
		return fail("upstream_unbounded", "the upstream request carried an output cap of %d (limit %d)", got, caps.MaxOutputToks)
	}
	return pass()
}

// 15.1 structured output: a schema-constrained request returns a parsed JSON
// document, and a reply that is not JSON is an error, never a result.
func checkStructuredOutput(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	req := textReq("Answer with a JSON object holding answer (string) and confidence (integer).")
	req.Output = providers.SchemaConstraint(json.RawMessage(answerSchema))
	s.Upstream.Script(Scenario{Kind: ScJSON, Text: `{"answer":"yes","confidence":3}`, ToksIn: 40, ToksOut: 12})
	res, err := generate(ctx, s, sc, req)
	if err != nil {
		return fail("structured_failed", "a structured request failed: code %s", codeOf(err))
	}
	if len(res.JSON) == 0 || !validatesAgainst(answerSchema, res.JSON) {
		return fail("structured_invalid", "the structured result does not satisfy the schema")
	}
	if s.Natural {
		return pass()
	}
	if !sameJSON(res.JSON, []byte(`{"answer":"yes","confidence":3}`)) {
		return fail("structured_invalid", "the structured result is not the document the upstream gave")
	}
	s.Upstream.Script(Scenario{Kind: ScText, Text: "I think the answer is yes.", ToksIn: 40, ToksOut: 8})
	if r, err := generate(ctx, s, sc, req); err == nil {
		_ = r
		return fail("non_json_accepted", "an answer that is not JSON was returned for a schema request")
	}
	return pass()
}

// 15.1 cancellation and timeout: a cancelled context sends nothing, and a
// service that never answers is cut off at the caller's deadline, for a whole
// call and for a stream.
func checkCancellationAndTimeout(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	before := len(s.Upstream.Seen())
	if _, err := generate(cctx, s, sc, textReq("hi")); err == nil {
		return fail("cancelled_context_succeeded", "a call with a cancelled context succeeded")
	}
	if len(s.Upstream.Seen()) != before {
		return fail("cancelled_context_sent", "a call with a cancelled context reached the upstream")
	}

	const deadline = 300 * time.Millisecond
	s.Upstream.Script(Scenario{Kind: ScStall})
	var gerr error
	dctx, dcancel := context.WithTimeout(ctx, deadline)
	returned := callWithin(deadline+grace, func() {
		_, gerr = generate(dctx, s, sc, textReq("hi"))
	})
	dcancel()
	if !returned {
		s.Upstream.Release()
		return fail("stall_not_cut_off", "a stalled call outlived its deadline by more than %s", grace)
	}
	if gerr == nil {
		return fail("stall_returned_success", "a stalled call returned a result")
	}
	if !errors.Is(gerr, context.DeadlineExceeded) && codeOf(gerr) != contracts.ErrTimeout {
		return fail("timeout_not_mapped", "a deadline was not reported as a timeout (code %s)", codeOf(gerr))
	}

	s.Upstream.Script(Scenario{Kind: ScStall})
	var serr error
	sctx, scancel := context.WithTimeout(ctx, deadline)
	returned = callWithin(deadline+grace, func() {
		st, err := s.Generator.GenerateStream(sctx, sc, textReq("hi"))
		if err != nil {
			serr = err
			return
		}
		defer st.Close()
		_, serr = st.Recv()
	})
	scancel()
	if !returned {
		s.Upstream.Release()
		return fail("stream_stall_not_cut_off", "a stalled stream outlived its deadline by more than %s", grace)
	}
	if serr == nil {
		return fail("stall_returned_success", "a stalled stream produced a chunk")
	}
	return pass()
}

// 15.1 usage reporting: the counts the upstream reports reach the caller, for
// a whole call and at the end of a stream, so budgets meter what was used.
func checkUsageReporting(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	s.Upstream.Script(Scenario{Kind: ScText, Text: ReplyText, ToksIn: 1000, ToksOut: 500})
	res, err := generate(ctx, s, sc, textReq("Say hello."))
	if err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	if res.Usage.ToksIn == 0 && res.Usage.ToksOut == 0 {
		return fail("usage_missing", "the result reports no usage")
	}
	if err := res.Usage.Validate(); err != nil {
		return fail("usage_invalid", "the usage record is not valid")
	}
	if !s.Natural && (res.Usage.ToksIn != 1000 || res.Usage.ToksOut != 500) {
		return fail("usage_wrong", "usage %d/%d, the upstream reported 1000/500", res.Usage.ToksIn, res.Usage.ToksOut)
	}
	s.Upstream.Script(Scenario{Kind: ScText, Text: ReplyText, ToksIn: 1000, ToksOut: 500})
	sres, err := stream(ctx, s, sc, textReq("Say hello."))
	if err != nil {
		return fail("stream_failed", "the stream failed: code %s", codeOf(err))
	}
	if sres.Usage.ToksIn == 0 && sres.Usage.ToksOut == 0 {
		return fail("usage_missing_in_stream", "the stream reports no usage")
	}
	if !s.Natural && (sres.Usage.ToksIn != 1000 || sres.Usage.ToksOut != 500) {
		return fail("usage_wrong_in_stream", "streamed usage %d/%d, the upstream reported 1000/500", sres.Usage.ToksIn, sres.Usage.ToksOut)
	}
	return pass()
}

// 15.1 malformed and error mapping: each failure of the service is a typed
// error with the right code and kind, never an empty success, never a retry
// storm, and never a copy of the body the service sent.
func checkMalformedAndErrorMapping(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	type row struct {
		sc      Scenario
		codes   []contracts.ErrCode
		kind    pv.ProviderErrorKind
		maxHits int
	}
	rows := []row{
		{Scenario{Kind: ScMalformed}, []contracts.ErrCode{contracts.ErrSchemaInvalid}, pv.ProviderErrorKindMalformedResponse, 1},
		{Scenario{Kind: ScServerError}, []contracts.ErrCode{contracts.ErrUnavailable}, pv.ProviderErrorKindUnavailable, 1},
		{Scenario{Kind: ScAuthError}, []contracts.ErrCode{contracts.ErrPermissionDenied, contracts.ErrUnauthenticated}, pv.ProviderErrorKindAuthenticationFailed, 1},
		{Scenario{Kind: ScRateLimit, RetryAfter: 7 * time.Second, Times: 8}, []contracts.ErrCode{contracts.ErrRateLimited}, pv.ProviderErrorKindRateLimited, 6},
	}
	for _, r := range rows {
		before := len(s.Upstream.Seen())
		s.Upstream.Script(r.sc)
		_, err := generate(ctx, s, sc, textReq("hi"))
		if err == nil {
			return fail("error_swallowed", "a %s failure produced a success", r.sc.Kind)
		}
		okCode := false
		for _, c := range r.codes {
			okCode = okCode || codeOf(err) == c
		}
		if !okCode {
			return fail("error_code_wrong", "a %s failure mapped to code %s", r.sc.Kind, codeOf(err))
		}
		if kindOf(err) != string(r.kind) {
			return fail("error_kind_missing", "a %s failure carries kind %q, want %q", r.sc.Kind, kindOf(err), r.kind)
		}
		if leaks(errText(err), []string{UpstreamMarker}) != "" {
			return fail("upstream_body_echoed", "the %s error repeats the upstream body", r.sc.Kind)
		}
		if n := len(s.Upstream.Seen()) - before; n > r.maxHits {
			return fail("retries_unbounded", "a %s failure caused %d upstream requests", r.sc.Kind, n)
		}
		if r.sc.Kind == ScRateLimit {
			if info, ok := providers.RateLimitFrom(err); !ok || info.RetryAfter != r.sc.RetryAfter {
				return fail("retry_after_lost", "the rate limit error does not carry the upstream's retry-after")
			}
		}
	}
	// the stream path maps failures too
	s.Upstream.Script(Scenario{Kind: ScServerError})
	if _, err := stream(ctx, s, sc, textReq("hi")); err == nil {
		return fail("error_swallowed", "a failure before the stream produced a success")
	}
	return pass()
}

var _ = io.EOF
