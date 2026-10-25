package providersuite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func operationsChecks() []Check {
	return []Check{
		{"operations", "creds_never_in_responses_or_logs", checkCredsNeverInResponsesOrLogs},
		{"operations", "quota_and_budget_enforcement", checkQuotaAndBudgetEnforcement},
		{"operations", "provider_disablement", checkProviderDisablement},
		{"operations", "fallback_and_outage_behavior", checkFallbackAndOutageBehavior},
		{"operations", "tenant_isolation", checkTenantIsolation},
		{"operations", "audit_completeness", checkAuditCompleteness},
		{"operations", "terms_and_capability_profile_validation", checkTermsAndCapabilityProfileValidation},
	}
}

// 15.5 credentials never appear in client responses or logs: after successes
// and every kind of failure, the credential (and any fragment of it) is in no
// result, no error message, no error detail, no printed form of the provider
// and no log line. The upstream's error bodies echo the key it was shown, as
// some real services do; a provider that copies a body fails here.
func checkCredsNeverInResponsesOrLogs(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	var results, errs []string
	s.Upstream.Script(Scenario{Kind: ScText, Text: ReplyText, ToksIn: 20, ToksOut: 8})
	if res, err := generate(ctx, s, sc, textReq("hi")); err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	} else {
		results = append(results, dump(res))
	}
	s.Upstream.Script(Scenario{Kind: ScText, Text: ReplyText, ToksIn: 20, ToksOut: 8})
	if res, err := stream(ctx, s, sc, textReq("hi")); err != nil {
		return fail("stream_failed", "the stream failed: code %s", codeOf(err))
	} else {
		results = append(results, dump(res))
	}
	for _, k := range []Kind{ScServerError, ScAuthError, ScMalformed, ScQuota} {
		s.Upstream.Script(Scenario{Kind: k})
		if _, err := generate(ctx, s, sc, textReq("hi")); err != nil {
			errs = append(errs, errText(err))
		}
	}
	s.Upstream.Script(Scenario{Kind: ScServerError})
	if _, err := stream(ctx, s, sc, textReq("hi")); err != nil {
		errs = append(errs, errText(err))
	}
	needles := append(s.Keys.Secrets(), "fixture-credential")
	if n := leaks(strings.Join(results, "\n"), needles); n != "" {
		return fail("credential_in_result", "a credential is in a result")
	}
	if n := leaks(strings.Join(errs, "\n"), needles); n != "" {
		return fail("credential_in_error", "a credential is in an error")
	}
	if n := leaks(fmt.Sprintf("%v %+v %s", s.Generator, s.Generator, s.Generator), needles); n != "" {
		return fail("credential_in_error", "a credential is in the printed form of the provider")
	}
	if n := leaks(s.Logs(), needles); n != "" {
		return fail("credential_in_log", "a credential is in the log")
	}
	return pass()
}

// 15.5 quota and budget enforcement: a quota failure is resource_exhausted,
// not retried (no wait cures it) and not echoed; the output cap the caller set
// reaches the upstream, so the reservation the budget made bounds the spend.
func checkQuotaAndBudgetEnforcement(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	before := len(s.Upstream.Seen())
	s.Upstream.Script(Scenario{Kind: ScQuota, Times: 4})
	_, err := generate(ctx, s, sc, textReq("hi"))
	if err == nil {
		return fail("quota_not_mapped", "a quota failure produced a success")
	}
	if codeOf(err) != contracts.ErrResourceExhausted || kindOf(err) != string(pv.ProviderErrorKindQuotaExceeded) {
		return fail("quota_not_mapped", "a quota failure mapped to code %s kind %q", codeOf(err), kindOf(err))
	}
	if n := len(s.Upstream.Seen()) - before; n != 1 {
		return fail("quota_retried", "a quota failure caused %d upstream requests", n)
	}
	if leaks(errText(err), []string{UpstreamMarker}) != "" {
		return fail("upstream_body_echoed", "the quota error repeats the upstream body")
	}
	req := textReq("hi")
	req.MaxToks = 77
	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1})
	if _, err := generate(ctx, s, sc, req); err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	seen := s.Upstream.Seen()
	if got := seen[len(seen)-1].MaxOutputToks; got != 77 {
		return fail("output_cap_not_forwarded", "the caller capped the output at 77 and the upstream saw %d", got)
	}
	return pass()
}

// 15.5 provider disablement: once the credential is revoked, nothing is sent
// (whole or streamed), the error says nothing left, and re-enabling it works.
func checkProviderDisablement(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1})
	if _, err := generate(ctx, s, sc, textReq("hi")); err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	s.Keys.Revoke(tenantA, true)
	before := len(s.Upstream.Seen())
	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1})
	_, err := generate(ctx, s, sc, textReq("hi"))
	if err == nil {
		return fail("revocation_not_fail_closed", "a call succeeded after the credential was revoked")
	}
	if len(s.Upstream.Seen()) != before {
		return fail("sent_after_revocation", "a request was sent after the credential was revoked")
	}
	if sent, ok := sentOf(err); !ok || sent {
		return fail("sent_flag_wrong", "the refusal does not say that nothing was sent")
	}
	if st, err := s.Generator.GenerateStream(ctx, sc, textReq("hi")); err == nil {
		st.Close()
		return fail("revocation_not_fail_closed", "a stream opened after the credential was revoked")
	}
	if len(s.Upstream.Seen()) != before {
		return fail("sent_after_revocation", "a stream request was sent after the credential was revoked")
	}
	s.Keys.Revoke(tenantA, false)
	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1})
	if _, err := generate(ctx, s, sc, textReq("hi")); err != nil {
		return fail("revocation_sticky", "the provider did not recover when the credential was re-enabled")
	}
	return pass()
}

// 15.5 fallback and outage behavior: an unreachable service is a typed
// unavailable error (never an empty success), Health says so, a dropped
// request is not retried and is marked as possibly processed, and the provider
// recovers. The provider falls back to nothing on its own: the router decides.
func checkFallbackAndOutageBehavior(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	s.Upstream.SetDown(true)
	before := len(s.Upstream.Seen())
	_, err := generate(ctx, s, sc, textReq("hi"))
	if err == nil {
		s.Upstream.SetDown(false)
		return fail("outage_returned_success", "a call to an unreachable service succeeded")
	}
	if c := codeOf(err); c != contracts.ErrUnavailable && c != contracts.ErrTimeout {
		s.Upstream.SetDown(false)
		return fail("outage_wrong_code", "an outage mapped to code %s", c)
	}
	if !errKindOK(err, pv.ProviderErrorKindUnavailable, pv.ProviderErrorKindConnectionLost, pv.ProviderErrorKindTimeout) {
		s.Upstream.SetDown(false)
		return fail("outage_kind_missing", "an outage carries kind %q", kindOf(err))
	}
	if sent, ok := sentOf(err); !ok || (kindOf(err) == string(pv.ProviderErrorKindConnectionLost) && !sent) {
		s.Upstream.SetDown(false)
		return fail("sent_flag_wrong", "the outage error misstates whether the request left")
	}
	if st, err := s.Generator.GenerateStream(ctx, sc, textReq("hi")); err == nil {
		_, rerr := st.Recv()
		st.Close()
		if rerr == nil {
			s.Upstream.SetDown(false)
			return fail("outage_returned_success", "a stream from an unreachable service produced a chunk")
		}
	}
	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	rep, herr := s.Runtime.Health(hctx)
	hcancel()
	if herr == nil && rep.Status == providers.HealthOK {
		s.Upstream.SetDown(false)
		return fail("health_ok_while_down", "Health reports ok while the service is unreachable")
	}
	if n := len(s.Upstream.Seen()) - before; n > 0 {
		s.Upstream.SetDown(false)
		return fail("outage_returned_success", "requests reached an unreachable service")
	}
	s.Upstream.SetDown(false)

	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1})
	if _, err := generate(ctx, s, sc, textReq("hi")); err != nil {
		return fail("no_recovery", "the provider did not recover when the service came back")
	}
	hctx, hcancel = context.WithTimeout(ctx, 10*time.Second)
	rep, herr = s.Runtime.Health(hctx)
	hcancel()
	if herr != nil || rep.Status == providers.HealthUnavailable {
		return fail("no_recovery", "Health did not recover when the service came back")
	}

	// a request that was sent and then lost may have been processed: it must
	// say so and must not be retried by the provider.
	hits := len(s.Upstream.Seen())
	s.Upstream.Script(Scenario{Kind: ScTruncated, Times: 3})
	_, err = generate(ctx, s, sc, textReq("hi"))
	if err == nil {
		return fail("outage_returned_success", "a dropped connection produced a success")
	}
	if sent, ok := sentOf(err); !ok || !sent {
		return fail("sent_flag_wrong", "a request lost after it was sent is marked as not sent")
	}
	if n := len(s.Upstream.Seen()) - hits; n != 1 {
		return fail("dropped_request_retried", "a lost request caused %d upstream requests", n)
	}
	return pass()
}

// 15.5 tenant isolation: every call resolves the credential of its own scope
// (nothing is cached across scopes) and one tenant's content never shows up in
// another tenant's request.
func checkTenantIsolation(ctx context.Context, s Subject) Outcome {
	const noteA = "TENANT-A-PRIVATE-NOTE"
	scA, scB := scopeOf(tenantA), scopeOf(tenantB)
	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1}, Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1})
	if _, err := generate(ctx, s, scA, textReq("Remember "+noteA)); err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	if _, err := generate(ctx, s, scB, textReq("Hello")); err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	seen := s.Upstream.Seen()
	if len(seen) != 2 {
		return fail("request_not_seen", "the upstream saw %d requests, want 2", len(seen))
	}
	if seen[0].Key != s.Keys.Secret(tenantA) || seen[1].Key != s.Keys.Secret(tenantB) {
		return fail("credential_reused_across_scopes", "a request presented the credential of another tenant")
	}
	got := s.Keys.Resolved()
	if len(got) != 2 || got[0].TenantID != tenantA || got[1].TenantID != tenantB {
		return fail("credential_not_resolved_per_call", "the key source saw %d resolutions, want one per call", len(got))
	}
	if strings.Contains(seen[1].Body, noteA) {
		return fail("content_crossed_scopes", "one tenant's content appeared in another tenant's request")
	}
	return pass()
}

// 15.5 audit completeness: every failure carries what the router and the audit
// trail need (provider_error_kind, sent) and every success carries a trace id
// and the model that answered.
func checkAuditCompleteness(ctx context.Context, s Subject) Outcome {
	sc := scopeOf(tenantA)
	s.Upstream.Script(Scenario{Kind: ScText, Text: "ok", ToksIn: 10, ToksOut: 1})
	res, err := generate(ctx, s, sc, textReq("hi"))
	if err != nil {
		return fail("generate_failed", "Generate failed: code %s", codeOf(err))
	}
	if res.TraceID == "" {
		return fail("success_without_trace", "the result has no trace id")
	}
	if res.Model == "" {
		return fail("success_without_model", "the result does not name the model that answered")
	}
	s.Upstream.Script(Scenario{Kind: ScText, Text: ReplyText, ToksIn: 10, ToksOut: 1})
	sres, err := stream(ctx, s, sc, textReq("hi"))
	if err != nil {
		return fail("stream_failed", "the stream failed: code %s", codeOf(err))
	}
	if sres.TraceID == "" || sres.Model == "" {
		return fail("success_without_trace", "the streamed result lacks a trace id or model")
	}
	over := textReq("hi")
	over.Output = providers.OutputConstraint{Grammar: "root ::= \"x\""}
	_, lerr := generate(ctx, s, sc, over)
	errs := []error{lerr}
	for _, k := range []Kind{ScServerError, ScAuthError, ScMalformed, ScQuota, ScRateLimit} {
		s.Upstream.Script(Scenario{Kind: k, RetryAfter: time.Second, Times: 8})
		_, err := generate(ctx, s, sc, textReq("hi"))
		errs = append(errs, err)
	}
	for i, e := range errs {
		if e == nil {
			if i == 0 {
				continue // a provider that supports grammar output accepts the request
			}
			return fail("error_without_kind", "a failure produced a success")
		}
		if i > 0 && kindOf(e) == "" {
			return fail("error_without_kind", "a failure carries no provider_error_kind")
		}
		if _, ok := sentOf(e); !ok {
			return fail("error_without_sent", "a failure does not say whether the request left")
		}
	}
	return pass()
}

// 15.5 terms and capability profile validation: the profile is a valid
// provider profile with its terms recorded, the provider's own capability
// record agrees with it, and a capability the profile does not declare is
// refused before anything is sent.
func checkTermsAndCapabilityProfileValidation(ctx context.Context, s Subject) Outcome {
	raw, err := json.Marshal(s.Profile)
	if err != nil {
		return fail("profile_schema_invalid", "the profile cannot be encoded")
	}
	if err := ValidateAgainst(SchemaProviderProfile, raw); err != nil {
		return fail("profile_schema_invalid", "%v", err)
	}
	var prof map[string]any
	_ = json.Unmarshal(raw, &prof)
	terms, _ := prof["terms"].(map[string]any)
	if terms["retention"] == nil || terms["training_use"] == nil || terms["terms_hash"] == nil {
		return fail("terms_missing", "the profile does not record retention, training use and the terms hash")
	}
	if rev, _ := prof["last_policy_review"].(string); rev != "" {
		if d, err := time.Parse("2006-01-02", rev); err == nil && d.After(s.now().AddDate(0, 0, 1)) {
			return fail("policy_review_in_future", "the policy review date is in the future")
		}
	}
	caps, err := s.Caps.Capabilities(ctx)
	if err != nil {
		return fail("capabilities_unavailable", "Capabilities failed")
	}
	if !capsAgree(caps, prof) {
		return fail("capabilities_disagree_with_profile", "the provider's capability record does not match its profile")
	}
	before := len(s.Upstream.Seen())
	req := textReq("hi")
	switch {
	case !caps.Grammar:
		req.Output = providers.OutputConstraint{Grammar: "root ::= \"x\""}
	case !caps.Tools.Supported:
		req.Tools = []providers.ToolSpec{bookTool}
	default:
		return pass()
	}
	if _, err := generate(ctx, s, scopeOf(tenantA), req); err == nil {
		return fail("undeclared_capability_accepted", "a request needing an undeclared capability was accepted")
	}
	if len(s.Upstream.Seen()) != before {
		return fail("undeclared_capability_accepted", "a request needing an undeclared capability reached the upstream")
	}
	return pass()
}

// capsAgree compares the provider's capability record with its profile:
// modalities, streaming, structured output and tool support must be the same,
// and the provider must not claim more room than the profile declares.
func capsAgree(c providers.ModelCapabilities, prof map[string]any) bool {
	strs := func(v any) map[string]bool {
		out := map[string]bool{}
		if l, ok := v.([]any); ok {
			for _, x := range l {
				if s, ok := x.(string); ok {
					out[s] = true
				}
			}
		}
		return out
	}
	same := func(a map[string]bool, b []providers.Modality) bool {
		if len(a) != len(b) {
			return false
		}
		for _, m := range b {
			if !a[string(m)] {
				return false
			}
		}
		return true
	}
	mod, _ := prof["modalities"].(map[string]any)
	if !same(strs(mod["input"]), c.InputModalities) || !same(strs(mod["output"]), c.OutputModalities) {
		return false
	}
	if st, _ := prof["streaming"].(bool); st != c.Streaming {
		return false
	}
	if so, _ := prof["structured_output"].(string); so != string(c.StructuredOutput) {
		return false
	}
	tc, _ := prof["tool_calling"].(map[string]any)
	if sup, _ := tc["supported"].(bool); sup != c.Tools.Supported {
		return false
	}
	lim, _ := prof["limits"].(map[string]any)
	ctxToks, _ := lim["max_context_toks"].(float64)
	outToks, _ := lim["max_output_toks"].(float64)
	return float64(c.MaxContextToks) <= ctxToks && float64(c.MaxOutputToks) <= outToks
}
