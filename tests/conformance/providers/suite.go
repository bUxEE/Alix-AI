// Package providersuite is the AlixAI provider conformance suite (additive plan
// section 15): "No provider is production-qualified merely because it responds
// successfully to a basic chat request." Every external provider adapter runs
// the same 30 checks, in the five groups of the plan, and the result is a
// conformance report that validates against schemas/providers/conformance-report
// and that the registry records as last_conformance. It is imported as
//
//	import providersuite "github.com/buxee/alix-ai/tests/conformance/providers"
//
// # Running it
//
// An adapter's test provides a Factory. For each check the suite calls the
// factory for a fresh Subject: the provider exactly as production builds it
// (a providers.MultimodalGenerator), the registry profile it is qualified
// under, and an Upstream that plays the provider's service from recorded
// fixtures.
//
//	func TestConformanceMyProvider(t *testing.T) {
//	    providersuite.RunProviderSuite(t, func(t *testing.T) providersuite.Subject {
//	        return startMyProviderOverFixtures(t) // a loopback server, never the real API
//	    })
//	}
//
// # The scenario vocabulary
//
// The suite never calls a real service. It asks the Upstream to behave in one
// of about fifteen ways (Scenario: a text reply, a tool-call proposal, a
// rate limit, a quota failure, a stall, a truncated stream, ...) and the
// Upstream renders each in the provider's own wire format from recorded
// fixtures. It reports what arrived (Seen: instructions, items by role, tools,
// the output cap, the credential, provider-native features). A check asserts
// something the AlixAI gates rely on and a provider can get wrong; the
// router, the egress guard and the budget service are tested in their own
// packages and are not re-tested here.
//
// # What is checked
//
// The check names are the member names of the conformance report schema, so
// the report reads against the plan one to one. A check returns an Outcome: a
// pass, a fail with a fixed snake_case reason code (never text, because a
// provider's output can echo prompts), or not_applicable. Only the image, file
// and audio checks may be not_applicable, and only for a capability the profile
// does not declare; for an undeclared capability the check still proves that
// the provider refuses the request before anything is sent.
//
//	inference   text_generation_and_streaming bounded_context structured_output
//	            cancellation_and_timeout usage_reporting malformed_and_error_mapping
//	control     typed_task_proposal unknown_capability_refusal argument_schema_compliance
//	            no_direct_adapter_invocation directive_and_receipt_handling
//	            confirmation_and_cancellation
//	rag         authorized_excerpt_handling source_reference_preservation
//	            no_unsupported_citations prompt_injection_resistance
//	            local_only_source_enforcement no_path_or_cred_leakage
//	multimodal  image_input_output file_input_output audio_input_output
//	            cancellation_and_partial_stream sanitized_rich_blocks
//	operations  creds_never_in_responses_or_logs quota_and_budget_enforcement
//	            provider_disablement fallback_and_outage_behavior tenant_isolation
//	            audit_completeness terms_and_capability_profile_validation
//
// # The suite tests itself
//
// suite_test.go runs the reference fake external provider through every check
// (it must pass) and then one deliberately broken variant per check (each must
// fail exactly its own check with its own reason code), so the suite cannot
// pass a provider that violates the contract.
package providersuite

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// SuiteVersion is the version of the check set, recorded in every report. A
// change to what a check asserts raises it.
const SuiteVersion = "1.0.0"

// Result of one check.
type Result string

const (
	Passed        Result = "passed"
	Failed        Result = "failed"
	NotApplicable Result = "not_applicable"
)

// Outcome is what a check reports.
type Outcome struct {
	Result Result
	// Reason is a fixed snake_case code, set when Result is Failed.
	Reason string
	// Detail is a short human explanation for the console and test log. It
	// never reaches the report and never quotes provider output.
	Detail string
}

func pass() Outcome          { return Outcome{Result: Passed} }
func notApplicable() Outcome { return Outcome{Result: NotApplicable} }
func fail(reason, format string, args ...any) Outcome {
	return Outcome{Result: Failed, Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Check is one bullet of additive plan section 15.
type Check struct {
	// Group is the report group: inference, control, rag, multimodal, operations.
	Group string
	// Name is the report member name.
	Name string
	// Run reports whether the subject conforms. It must return within ctx's
	// deadline plus a fixed grace even if the provider hangs.
	Run func(ctx context.Context, s Subject) Outcome
}

// Kind names what an Upstream is asked to do.
type Kind string

const (
	// ScText answers with Text and the usage counts.
	ScText Kind = "text"
	// ScJSON answers with Text, which is a JSON document (structured output).
	ScJSON Kind = "json"
	// ScToolCall proposes Call. The reply carries no text.
	ScToolCall Kind = "tool_call"
	// ScMedia answers with Text and one produced media part (declared media only).
	ScMedia Kind = "media"
	// ScActiveMedia answers with an unrequested active-content media part
	// (an SVG or HTML "image"): the provider must not pass it on.
	ScActiveMedia Kind = "active_media"
	// ScHostedTool answers with an item run by the provider's own tools
	// (web search, file search, code interpreter): nothing was requested.
	ScHostedTool Kind = "hosted_tool"
	// ScMalformed answers 200 with a body that is not a valid answer.
	ScMalformed Kind = "malformed"
	// ScServerError answers 500.
	ScServerError Kind = "server_error"
	// ScAuthError answers 401.
	ScAuthError Kind = "auth_error"
	// ScRateLimit answers 429 with a Retry-After of RetryAfter (a rate limit
	// that a wait cures). Set Times above the provider's retry bound.
	ScRateLimit Kind = "rate_limit"
	// ScQuota answers 429 insufficient_quota (no wait cures it).
	ScQuota Kind = "quota"
	// ScStall accepts the request and never answers until Release or until
	// the caller gives up.
	ScStall Kind = "stall"
	// ScTruncated starts a streamed answer and cuts the connection after the
	// first piece of text. For a non-streamed call it drops the connection.
	ScTruncated Kind = "truncated"
)

// ToolCall is a proposal an upstream makes.
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// MediaOut is a media part an upstream produces.
type MediaOut struct {
	Kind      providers.Modality
	MediaType string
	Data      []byte
}

// Scenario is one upstream behaviour.
type Scenario struct {
	Kind Kind
	// Text is the reply text (ScText, ScJSON, ScMedia).
	Text string
	Call ToolCall
	// ToksIn and ToksOut are the usage the upstream reports.
	ToksIn, ToksOut int
	RetryAfter      time.Duration
	Media           MediaOut
	// Times is how many consecutive requests the scenario answers (0 means 1).
	Times int
}

// SeenItem is one non-instruction input item as it arrived.
type SeenItem struct {
	// Role is user, assistant or tool.
	Role string
	Text string
	// CallID names the proposal a tool item answers, or an assistant call.
	CallID string
	// Name is the tool name of an assistant call.
	Name string
}

// SeenTool is one tool as it arrived.
type SeenTool struct {
	Name string
	// Kind is the wire type of the tool; AlixAI offers "function" only.
	Kind string
}

// Seen is one request as the upstream received it, in a form every wire format
// maps onto.
type Seen struct {
	Stream bool
	// Key is the credential the request presented.
	Key string
	// System are the instruction texts (system or developer role).
	System []string
	// Items are the other input items in order.
	Items      []SeenItem
	Tools      []SeenTool
	ToolChoice string
	// MaxOutputToks is the output cap the request carried (0: none).
	MaxOutputToks int
	// HasSchema is true when a JSON Schema output constraint arrived.
	HasSchema bool
	// Media are the kinds of media parts that arrived.
	Media []string
	// Native lists provider-native features the request asked for (stored
	// responses, chained or hosted state, hosted tools). AlixAI asks for none.
	Native []string
	// Body is the whole request as text, for leak scans.
	Body string
}

// AllText joins every text the request carried.
func (s Seen) AllText() string {
	var sb strings.Builder
	for _, t := range s.System {
		sb.WriteString(t)
		sb.WriteByte('\n')
	}
	for _, it := range s.Items {
		sb.WriteString(it.Text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// Upstream is the provider's service, played from recorded fixtures.
type Upstream interface {
	// Script sets what the next requests meet, oldest first. It REPLACES the
	// queue: scenarios left over from an earlier call are dropped, so a
	// provider that retried less than the script allowed leaves nothing behind.
	Script(...Scenario)
	// Seen returns every request received, oldest first.
	Seen() []Seen
	// SetDown makes the service unreachable (or reachable again).
	SetDown(down bool)
	// Release unblocks stalled requests.
	Release()
}

// Subject is one provider under test and the rig around it.
type Subject struct {
	// Name labels the subject in messages.
	Name string
	// Generator is the provider as production builds it.
	Generator providers.MultimodalGenerator
	// Caps and Runtime are normally the Generator itself.
	Caps    providers.CapabilityProvider
	Runtime providers.AIRuntimeProvider
	// Profile is the registry profile the report is for.
	Profile pv.ProviderProfile
	// Upstream plays the provider's service.
	Upstream Upstream
	// Keys is the key source the provider resolves its credential from; the
	// suite revokes and switches keys through it.
	Keys *KeyRing
	// Logs returns everything the provider logged so far.
	Logs func() string
	// Natural is true when the upstream is a real service: scripted content
	// cannot be forced, so checks assert structure instead of exact text.
	Natural bool
	// Now is the clock for the profile-review check (nil: wall clock).
	Now func() time.Time
}

// Factory builds a fresh Subject for one check. Register cleanup (closing
// servers) with t.Cleanup.
type Factory func(t *testing.T) Subject

func (s Subject) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// declared reports what the profile declares for the three conditional checks;
// the rule is the registry's (see DeclaredMedia).
func (s Subject) declared() (image, file, audio bool) { return DeclaredMedia(s.Profile) }

// Checks returns the 30 checks in report order.
func Checks() []Check {
	var all []Check
	all = append(all, inferenceChecks()...)
	all = append(all, controlChecks()...)
	all = append(all, ragChecks()...)
	all = append(all, multimodalChecks()...)
	all = append(all, operationsChecks()...)
	return all
}

// grace is how long past its deadline a call may take to return before the
// provider is judged to ignore its context.
var grace = 1500 * time.Millisecond

// checkTimeout bounds one check.
const checkTimeout = 60 * time.Second

// Row is the result of one check, as the runner reports it.
type Row struct {
	Check
	Outcome
}

// Run runs every check, each on a fresh Subject from mk. cleanup (may be nil)
// is called after each check. A factory error fails the check with the
// reason subject_unavailable.
func Run(ctx context.Context, mk func() (Subject, func(), error)) []Row {
	var rows []Row
	for _, c := range Checks() {
		rows = append(rows, Row{Check: c, Outcome: runOne(ctx, c, mk)})
	}
	return rows
}

func runOne(ctx context.Context, c Check, mk func() (Subject, func(), error)) (out Outcome) {
	s, cleanup, err := mk()
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return fail("subject_unavailable", "cannot build the subject: %v", err)
	}
	if s.Generator == nil || s.Upstream == nil || s.Keys == nil || s.Logs == nil {
		return fail("subject_incomplete", "the subject lacks a generator, upstream, key ring or log")
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			out = fail("check_panicked", "%v", r)
		}
	}()
	defer s.Upstream.Release()
	return c.Run(cctx, s)
}

// RunProviderSuite runs every check as a subtest, each on a fresh Subject
// from the factory.
func RunProviderSuite(t *testing.T, factory Factory) {
	t.Helper()
	for _, c := range Checks() {
		t.Run(c.Group+"/"+c.Name, func(t *testing.T) {
			out := runOne(context.Background(), c, func() (Subject, func(), error) { return factory(t), nil, nil })
			switch out.Result {
			case Passed, NotApplicable:
			default:
				t.Fatalf("%s: %s (%s)", c.Name, out.Reason, out.Detail)
			}
		})
	}
}

// KeyRing is the key source the suite hands the provider: one placeholder
// credential per tenant, revocable, recording the scope of every resolution.
// The values are deliberately not shaped like a real key.
type KeyRing struct {
	mint    func(tenant string) string
	mu      sync.Mutex
	keys    map[string]string
	revoked map[string]bool
	seen    []contracts.Scope
}

// NewKeyRing returns a ring that mints "fixture-credential-<tenant>" values.
func NewKeyRing() *KeyRing {
	return &KeyRing{keys: map[string]string{}, revoked: map[string]bool{}}
}

// NewKeyRingWith returns a ring whose credential for every tenant is the one
// secret (live mode: the operator's own key).
func NewKeyRingWith(secret string) *KeyRing {
	k := NewKeyRing()
	k.mint = func(string) string { return secret }
	return k
}

// Secret returns the placeholder credential of a tenant.
func (k *KeyRing) Secret(tenant string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.secretLocked(tenant)
}

func (k *KeyRing) secretLocked(tenant string) string {
	if v, ok := k.keys[tenant]; ok {
		return v
	}
	v := "fixture-credential-" + tenant + "-0001"
	if k.mint != nil {
		v = k.mint(tenant)
	}
	k.keys[tenant] = v
	return v
}

// Resolve is what a provider's key source calls for one request.
func (k *KeyRing) Resolve(scope contracts.Scope) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.seen = append(k.seen, scope)
	if k.revoked[scope.TenantID] {
		return "", contracts.New(contracts.ErrPermissionDenied, "credential is revoked").WithDetails("reason", "credential_revoked")
	}
	return k.secretLocked(scope.TenantID), nil
}

// Revoke disables (or re-enables) the credential of a tenant.
func (k *KeyRing) Revoke(tenant string, revoked bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.revoked[tenant] = revoked
}

// Resolved returns the scope of every resolution so far.
func (k *KeyRing) Resolved() []contracts.Scope {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]contracts.Scope(nil), k.seen...)
}

// Secrets returns every credential minted so far.
func (k *KeyRing) Secrets() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, v := range k.keys {
		out = append(out, v)
	}
	return out
}
