package providersuite

// Broken is one deliberately broken variant of the reference fake external
// provider: it breaks exactly the behaviour one check asserts and must fail
// that check with Reason. There is one per check, so a check that stops
// asserting what it claims is caught.
type Broken struct {
	// Check is the report member name of the check it must fail.
	Check string
	// Reason is the reason code the check must report.
	Reason string
	// What says in a few words how the provider is broken.
	What string
	// Options builds the provider.
	Options FakeOptions
}

func broken(check, reason, what string, media ...string) Broken {
	o := FakeOptions{Defect: check}
	for _, m := range media {
		switch m {
		case "image":
			o.Image = true
		case "file":
			o.File = true
		case "audio":
			o.Audio = true
		}
	}
	return Broken{Check: check, Reason: reason, What: what, Options: o}
}

// BrokenProviders returns the 30 broken variants, in report order.
func BrokenProviders() []Broken {
	return []Broken{
		broken("text_generation_and_streaming", "stream_text_mismatch", "a stream drops its last piece of text"),
		broken("bounded_context", "over_limit_sent", "the output limit of the model is not enforced"),
		broken("structured_output", "non_json_accepted", "an answer that is not JSON is returned for a schema request"),
		broken("cancellation_and_timeout", "stall_not_cut_off", "the caller's context is ignored while waiting"),
		broken("usage_reporting", "usage_missing_in_stream", "a stream ends without usage"),
		broken("malformed_and_error_mapping", "error_swallowed", "a server error becomes an empty success"),

		broken("typed_task_proposal", "no_proposal", "a tool call is returned as text, not as a proposal"),
		broken("unknown_capability_refusal", "unknown_tool_accepted", "a tool that was not offered is passed on"),
		broken("argument_schema_compliance", "arguments_repaired", "arguments are repaired and coerced on the way"),
		broken("no_direct_adapter_invocation", "followed_up_proposal", "a proposal is executed and the model called again"),
		broken("directive_and_receipt_handling", "receipt_promoted", "a tool result is sent as an instruction"),
		broken("confirmation_and_cancellation", "proposal_after_cancel_accepted", "tool choice none is ignored"),

		broken("authorized_excerpt_handling", "excerpt_altered", "an excerpt is cut at 1 KB"),
		broken("source_reference_preservation", "source_ref_lost", "source tags are stripped from the answer"),
		broken("no_unsupported_citations", "citation_added", "a citation of a source nobody supplied is appended"),
		broken("prompt_injection_resistance", "injection_promoted", "untrusted evidence is merged into the instructions"),
		broken("local_only_source_enforcement", "earlier_content_replayed", "the previous request is replayed into the next"),
		broken("no_path_or_cred_leakage", "path_in_request", "a file system path is added to the instructions"),

		broken("image_input_output", "image_input_dropped", "a declared image input is dropped", "image"),
		broken("file_input_output", "file_output_invalid", "a declared file output is an HTML page", "file"),
		broken("audio_input_output", "audio_output_invalid", "a declared audio output is empty", "audio"),
		broken("cancellation_and_partial_stream", "partial_stream_completed", "a stream cut mid-answer is reported complete"),
		broken("sanitized_rich_blocks", "active_content_accepted", "an unrequested SVG is passed on as an image"),

		broken("creds_never_in_responses_or_logs", "credential_in_log", "the credential is logged"),
		broken("quota_and_budget_enforcement", "output_cap_not_forwarded", "the caller's output cap is replaced by the model maximum"),
		broken("provider_disablement", "revocation_not_fail_closed", "the credential is cached past its revocation"),
		broken("fallback_and_outage_behavior", "health_ok_while_down", "Health reports ok while the service is down"),
		broken("tenant_isolation", "credential_reused_across_scopes", "the first credential is reused for every tenant"),
		broken("audit_completeness", "success_without_trace", "results carry no trace id and no model"),
		broken("terms_and_capability_profile_validation", "capabilities_disagree_with_profile", "the capability record claims audio input the profile does not declare"),
	}
}
