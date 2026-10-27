package sdkgo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/decision"
	memgen "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	"github.com/buxee/alix-ai/pkg/contracts/gen/voice"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

// The SDK, the fake server and schemas/api/openapi.yaml must agree. The OpenAPI document is the contract:
// when it changes (or when the SDK or the fake drifts from it) this file fails.

func TestOperationTableMatchesOpenAPI(t *testing.T) {
	s := loadSpec(t)
	for _, op := range sdkgo.Operations() {
		declared, ok := s.find(op.ID)
		if !ok {
			t.Errorf("%s is not in the OpenAPI document", op.ID)
			continue
		}
		got := fmt.Sprintf("%s %s auth=%v", op.Method, op.Path, op.Authenticated)
		want := fmt.Sprintf("%s %s auth=%v", declared.method, declared.path, declared.authenticated)
		if got != want {
			t.Errorf("%s: the SDK has %q, the document has %q", op.ID, got, want)
		}
		if op.Safe != (declared.method == http.MethodGet) && op.ID != "searchRag" && op.ID != "answerRag" {
			t.Errorf("%s: Safe=%v does not follow the method %s (only the two RAG reads are safe POSTs)", op.ID, op.Safe, declared.method)
		}
	}
}

func TestOperationsCoverTheImplementedAreas(t *testing.T) {
	s := loadSpec(t)
	implemented := map[string]bool{"root": true, "sessions": true, "actions": true, "apps": true, "rag": true, "providers": true, "oauth": true, "me": true, "memory": true, "exports": true}
	// The decision area is spoken in part on purpose: the read-only status. The operator-only configuration
	// (getDecisionConfig, putDecisionConfig) is not part of the SDK surface.
	// Voice is spoken in part: dictation HTTP only (not the WebSocket openVoiceSession).
	// Ops is spoken in part: erasure (1.3.0) and private LLM (1.4.0); queue/providers/audit-export stay out.
	partial := map[string]bool{
		"getDecisionStatus":         true,
		"getDictationCapabilities":  true,
		"createSessionDictation":    true,
		"startErasure":              true,
		"getErasureJob":             true,
		"putPrivateLlmEndpoint":     true,
		"getPrivateLlmEndpoint":     true,
		"deletePrivateLlmEndpoint":  true,
		"putTenantPrivateLlm":       true,
		"getTenantPrivateLlm":       true,
		"deleteTenantPrivateLlm":    true,
		"putPrincipalPrivateLlm":    true,
		"getPrincipalPrivateLlm":    true,
		"deletePrincipalPrivateLlm": true,
		"getTenantAnalyst":          true,
		"putTenantAnalyst":          true,
	}
	have := map[string]bool{}
	for _, op := range sdkgo.Operations() {
		have[op.ID] = true
	}
	for _, o := range s.ops {
		if (o.area == "decision" || o.area == "voice" || o.area == "ops") && have[o.id] != partial[o.id] {
			t.Errorf("%s: the SDK speaks only the partial %s surface (have=%v)", o.id, o.area, have[o.id])
		}
		if implemented[o.area] && !have[o.id] {
			t.Errorf("%s (%s %s, area %s) is in the document and the SDK does not speak it", o.id, o.method, o.path, o.area)
		}
	}
	// Areas the SDK does not implement stay out on purpose: the TypeScript SDK does not speak them either
	// (files, settings and history are not part of the SDK surface of M3-09 and M3-10).
	for _, o := range s.ops {
		if !implemented[o.area] && o.area != "decision" && o.area != "voice" && o.area != "ops" && have[o.id] {
			t.Errorf("%s is spoken by the SDK but belongs to the unimplemented area %s", o.id, o.area)
		}
	}
}

// The validator must really enforce the document, or every conformance check below would pass vacuously.
func TestSpecValidatorIsNotVacuous(t *testing.T) {
	s := loadSpec(t)
	session := `{"session_id":"ses-1","status":"active","turn_count":0,"created_at":"2026-10-02T00:00:00Z","updated_at":"2026-10-02T00:00:00Z","pending":null}`
	ok := s.responseSchema("getSession", 200, t)
	if err := validateJSON(ok, []byte(session)); err != nil {
		t.Fatalf("a valid session is refused: %v", err)
	}
	for name, bad := range map[string]string{
		"unknown status":  strings.Replace(session, `"active"`, `"paused"`, 1),
		"negative count":  strings.Replace(session, `"turn_count":0`, `"turn_count":-1`, 1),
		"extra member":    strings.Replace(session, `"pending":null`, `"pending":null,"extra":1`, 1),
		"missing members": `{"session_id":"ses-1"}`,
	} {
		if validateJSON(ok, []byte(bad)) == nil {
			t.Errorf("%s: an invalid session is accepted", name)
		}
	}
	errSchema := s.responseSchema("getSession", 403, t)
	if err := validateJSON(errSchema, []byte(`{"code":"scope_violation","message":"no","retryable":false}`)); err != nil {
		t.Errorf("a valid error is refused: %v", err)
	}
	if validateJSON(errSchema, []byte(`{"code":"made_up","message":"no","retryable":false}`)) == nil {
		t.Error("an error with an unknown code is accepted: the shared schemas are not resolved")
	}
	if validateJSON(s.responseSchema("getAction", 200, t), []byte(`{"preparation_id":"x"}`)) == nil {
		t.Error("a prepared action of one member is accepted")
	}
	statuses := fmt.Sprint(s.declaredStatuses("confirmAction"))
	if statuses != "[200 400 401 403 409 500 503]" {
		t.Errorf("confirmAction declares %s", statuses)
	}
}

// The TurnRequest of the document carries the attachments the SDK sends, and refuses what the SDK refuses.
func TestTurnRequestSchemaCarriesAttachments(t *testing.T) {
	s := loadSpec(t)
	schema := s.requestSchema("sendTurn", t)
	id := `"fil_0123456789ABCDEFGHJKMNPQRS"`
	for name, ok := range map[string]string{
		"one file":   `{"text":"hi","attachments":[` + id + `]}`,
		"four files": `{"text":"hi","language":"it","attachments":[` + strings.Join([]string{id, id, id, id}, ",") + `]}`,
	} {
		if err := validateJSON(schema, []byte(ok)); err != nil {
			t.Errorf("%s: a valid turn is refused: %v", name, err)
		}
	}
	for name, bad := range map[string]string{
		"five files":       `{"text":"hi","attachments":[` + strings.Join([]string{id, id, id, id, id}, ",") + `]}`,
		"a malformed id":   `{"text":"hi","attachments":["../../etc/passwd"]}`,
		"with a choice":    `{"choice_id":"c1","attachments":[` + id + `]}`,
		"with a rejection": `{"reject":{"preparation_id":"prep_1"},"attachments":[` + id + `]}`,
		"without any text": `{"attachments":[` + id + `]}`,
		"another member":   `{"text":"hi","files":[` + id + `]}`,
	} {
		if validateJSON(schema, []byte(bad)) == nil {
			t.Errorf("%s: an invalid turn is accepted", name)
		}
	}
	// What the SDK builds is what the document accepts.
	body, _ := json.Marshal(sdkgo.TextTurn("hi").WithAttachments(strings.Trim(id, `"`)))
	if err := validateJSON(schema, body); err != nil {
		t.Errorf("the SDK's own body is refused: %v", err)
	}
}

// conformance is a RoundTripper that checks every exchange against the OpenAPI document: the operation
// exists, a bearer token travels exactly with authenticated operations, a JSON request body validates, the
// status is declared and every JSON body (and every event of an event stream) validates.
type conformance struct {
	spec  *spec
	t     *testing.T
	inner http.RoundTripper

	mu         sync.Mutex
	violations []string
	validated  int
	statuses   map[string]map[int]bool
}

func (c *conformance) violate(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.violations = append(c.violations, fmt.Sprintf(format, args...))
}

var placeholder = regexp.MustCompile(`{[a-z_]+}`)

func templateRegexp(path string) *regexp.Regexp {
	parts := placeholder.Split(path, -1)
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile("^" + strings.Join(parts, "[^/]+") + "$")
}

func matchOperation(method, path string) (sdkgo.Operation, bool) {
	for _, op := range sdkgo.Operations() {
		if op.Method == method && templateRegexp(op.Path).MatchString(path) {
			return op, true
		}
	}
	return sdkgo.Operation{}, false
}

func (c *conformance) RoundTrip(req *http.Request) (*http.Response, error) {
	op, ok := matchOperation(req.Method, req.URL.Path)
	if !ok {
		c.violate("%s %s: no operation of the SDK matches", req.Method, req.URL.Path)
		return c.inner.RoundTrip(req)
	}
	if bearer := strings.HasPrefix(req.Header.Get("Authorization"), "Bearer "); bearer != op.Authenticated {
		c.violate("%s: bearer token sent=%v, operation authenticated=%v", op.ID, bearer, op.Authenticated)
	}
	if req.Body != nil && strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		if sch := c.spec.requestSchema(op.ID, c.t); sch == nil {
			c.violate("%s: the SDK sends a JSON body the document does not declare", op.ID)
		} else if err := validateJSON(sch, body); err != nil {
			c.violate("%s: request body %s: %v", op.ID, body, err)
		}
	}
	resp, err := c.inner.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	c.mu.Lock()
	if c.statuses[op.ID] == nil {
		c.statuses[op.ID] = map[int]bool{}
	}
	c.statuses[op.ID][resp.StatusCode] = true
	c.mu.Unlock()
	declared := false
	for _, s := range c.spec.declaredStatuses(op.ID) {
		declared = declared || s == resp.StatusCode
	}
	if !declared {
		c.violate("%s: the fake answered %d, which the document does not declare", op.ID, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return resp, nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	media := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(media, "application/json"):
		sch := c.spec.responseSchema(op.ID, resp.StatusCode, c.t)
		if sch == nil {
			c.violate("%s %d: no JSON schema declared", op.ID, resp.StatusCode)
		} else if err := validateJSON(sch, body); err != nil {
			c.violate("%s %d: %v\n%s", op.ID, resp.StatusCode, err, body)
		} else {
			c.mu.Lock()
			c.validated++
			c.mu.Unlock()
		}
		if resp.StatusCode/100 == 2 {
			c.checkGoType(op.ID, body)
		}
	case strings.HasPrefix(media, "text/event-stream"):
		if op.ID != "sendTurn" && op.ID != "streamTurnEvents" {
			c.violate("%s: an event stream from an operation that has none", op.ID)
		}
		c.checkStream(op.ID, body)
	}
	return resp, nil
}

// checkStream validates the frames of an event stream: the `data` is a TurnEvent and `id` and `event` are its
// seq and type.
func (c *conformance) checkStream(opID string, body []byte) {
	for _, block := range strings.Split(string(body), "\n\n") {
		var id, event, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" {
			continue
		}
		if err := validateJSON(c.spec.turnEventSchema(c.t), []byte(data)); err != nil {
			c.violate("%s: event %s: %v\n%s", opID, id, err, data)
			continue
		}
		var head struct {
			Type string `json:"type"`
			Seq  int    `json:"seq"`
		}
		if err := json.Unmarshal([]byte(data), &head); err != nil || head.Type != event || strconv.Itoa(head.Seq) != id {
			c.violate("%s: frame id=%q event=%q does not match its data %s", opID, id, event, data)
		}
		c.mu.Lock()
		c.validated++
		c.mu.Unlock()
	}
}

const (
	scopeBook    = "demo.book"
	articleWords = "Appointments can be cancelled free of charge up to 24 hours before the start."
)

// TestFakeAnswersConformToOpenAPI drives every operation the SDK speaks through the fake, success and failure
// paths, and checks every answer of the fake against the OpenAPI document.
func TestFakeAnswersConformToOpenAPI(t *testing.T) {
	spec := loadSpec(t)
	fake := sdktest.New()
	defer fake.Close()
	fake.DeclareSource("clinic-policies", "Clinic cancellation policy", articleWords)

	rec := &conformance{spec: spec, t: t, inner: http.DefaultTransport, statuses: map[string]map[int]bool{}}
	claims := sdktest.Claims{CapabilityScopes: []string{"demo.lookup", scopeBook, "demo.close_account", "applications.register", "exports.create"}}
	client, err := fake.Client(claims, func(o *sdkgo.ClientOptions) { o.HTTPClient = &http.Client{Transport: rec} })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	failsWith := func(what string, err error, code contracts.ErrCode) {
		t.Helper()
		if !contracts.HasCode(err, code) {
			t.Fatalf("%s: want %s, got %v", what, code, err)
		}
	}

	fake.OnTurn(func(turn sdktest.Turn) sdktest.TurnPlan {
		text := turn.Input.Text
		switch {
		case text == "fail":
			return sdktest.FailTurn(contracts.ErrUnavailable, "the model is down")
		case text == "toomany":
			return sdktest.RejectTurn(http.StatusTooManyRequests, contracts.ErrRateLimited, "slow down")
		case text == "denied":
			return sdktest.PrepareAction(sdktest.PrepareInput{CapabilityID: "demo.other", Summary: "x"})
		case strings.HasPrefix(text, "commit:"):
			return sdktest.PrepareAction(sdktest.PrepareInput{
				CapabilityID: scopeBook, Summary: "Book the 10:00 slot", Commit: sdktest.CommitPlan{Status: strings.TrimPrefix(text, "commit:")},
			})
		case text == "book":
			return sdktest.PrepareAction(sdktest.PrepareInput{CapabilityID: scopeBook, Summary: "Book the 10:00 slot"})
		}
		return sdktest.Reply(strings.Repeat("hello ", 30))
	})

	// probes, session, discovery
	_, err = client.GetLiveness(ctx)
	must("liveness", err)
	_, err = client.GetReadiness(ctx)
	must("readiness", err)
	_, err = client.GetHealth(ctx)
	must("health", err)
	_, err = client.CreateSession(ctx)
	must("create session", err)
	_, err = client.GetSession(ctx, "ses-1")
	must("get session", err)
	pending, err := client.GetPendingState(ctx, "ses-1")
	must("pending", err)
	if pending != nil {
		t.Fatalf("a fresh session has no pending state, got %+v", pending)
	}
	_, err = client.ListCapabilities(ctx)
	must("capabilities", err)
	_, err = client.GetApplication(ctx, "fake-app")
	must("application", err)
	archive := []byte{0x1f, 0x8b, 8, 0, 9}
	_, err = client.RegisterApplication(ctx, archive)
	must("register", err)
	_, err = client.RegisterApplication(ctx, archive)
	must("register again", err)

	// turns: JSON and stream, then every commit outcome
	_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("hello"))
	must("json turn", err)
	stream, err := client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("hello"))
	must("stream turn", err)
	first, err := stream.Result()
	must("stream result", err)
	for _, status := range []string{"committed", "queued", "rejected", "partial", "uncertain"} {
		text := "commit:" + status
		turn, err := client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn(text))
		must(text, err)
		grant := turn.Confirmation
		if grant == nil {
			t.Fatalf("%s: the turn prepared an action and returned no grant", text)
		}
		_, err = client.GetAction(ctx, grant.PreparationID)
		must("get action", err)
		if status == "committed" {
			pending, err := client.GetPendingState(ctx, "ses-1")
			must("pending", err)
			if pending == nil || pending.Kind != "confirmation" {
				t.Fatalf("a prepared action is the pending confirmation, got %+v", pending)
			}
			_, err = client.GetActionReceipt(ctx, grant.PreparationID)
			failsWith("receipt before the outcome", err, contracts.ErrConflict)
		}
		outcome, err := client.ConfirmAction(ctx, grant.PreparationID, grant.Token)
		if status == "committed" || status == "queued" {
			must("confirm", err)
			if outcome.Receipt == nil || string(outcome.Receipt.Status) != status {
				t.Fatalf("%s: receipt %+v", status, outcome.Receipt)
			}
		} else {
			var api *sdkgo.APIError
			if !errors.As(err, &api) || api.Receipt == nil || string(api.Receipt.Status) != status {
				t.Fatalf("%s: want an APIError with the receipt, got %v", status, err)
			}
		}
		_, err = client.GetActionReceipt(ctx, grant.PreparationID)
		must("receipt", err)
		_, err = client.ConfirmAction(ctx, grant.PreparationID, grant.Token)
		failsWith("a second confirmation", err, contracts.ErrReplayed)
	}
	declined, err := client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("book"))
	must("book", err)
	_, err = client.RejectAction(ctx, declined.Confirmation.PreparationID)
	must("reject", err)
	for _, input := range []func(*sdkgo.ConfirmationGrant) sdkgo.TurnInput{
		func(g *sdkgo.ConfirmationGrant) sdkgo.TurnInput { return sdkgo.ConfirmTurn(g.PreparationID, g.Token) },
		func(g *sdkgo.ConfirmationGrant) sdkgo.TurnInput { return sdkgo.RejectTurn(g.PreparationID) },
	} {
		turn, err := client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("book"))
		must("book", err)
		_, err = client.SendTurnJSON(ctx, "ses-1", input(turn.Confirmation))
		must("confirmation as a turn", err)
	}
	_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("fail"))
	must("a failed turn is still a turn", err)
	_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("denied"))
	must("a denied capability is an error event", err)
	_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("toomany"))
	failsWith("rate limited turn", err, contracts.ErrRateLimited)
	_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.ChoiceTurn("c1"))
	failsWith("no clarification pending", err, contracts.ErrValidationFailed)

	// turn attachments: the ids travel in the request (validated against TurnRequest by the transport above); a deleted,
	// an expired and an unknown file are the one scope_violation
	attached := fake.AddFile(claims)
	_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("hello").WithAttachments(attached))
	must("a turn with a file", err)
	attachedStream, err := client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("hello").WithAttachments(attached, attached))
	must("a streamed turn with a file", err)
	_, err = attachedStream.Result()
	must("a streamed turn with a file, result", err)
	gone, lapsed := fake.AddFile(claims), fake.AddFile(claims)
	fake.DeleteFile(gone)
	fake.ExpireFile(lapsed)
	for _, id := range []string{gone, lapsed, "fil_0123456789ABCDEFGHJKMNPQRS"} {
		_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("hello").WithAttachments(attached, id))
		failsWith("a file that does not resolve", err, contracts.ErrScopeViolation)
	}

	// resume: SSE and WebSocket, the end of a finished turn, an expired cursor
	turnID := first.TurnID
	last := int64(len(first.Events) - 1)
	for _, opt := range [][]sdkgo.CallOption{{sdkgo.WithCursor(1)}, {sdkgo.WithCursor(1), sdkgo.WithWebSocket()}, {}} {
		resumed, err := client.StreamTurnEvents(ctx, "ses-1", turnID, opt...)
		must("resume", err)
		_, err = resumed.Result()
		must("resume result", err)
	}
	ended, err := client.StreamTurnEvents(ctx, "ses-1", turnID, sdkgo.WithCursor(last))
	must("cursor at the end", err)
	if len(ended.Events()) != 0 {
		t.Fatal("a cursor at the end of a finished turn has no events")
	}
	_, err = client.StreamTurnEvents(ctx, "ses-1", turnID, sdkgo.WithCursor(last), sdkgo.WithWebSocket())
	must("cursor at the end over a WebSocket", err)
	fake.RetainEvents(2)
	_, err = client.StreamTurnEvents(ctx, "ses-1", turnID, sdkgo.WithCursor(0))
	failsWith("cursor behind the window", err, contracts.ErrExpired)
	fake.RetainEvents(0)
	_, err = client.StreamTurnEvents(ctx, "ses-1", turnID, sdkgo.WithCursor(last+5))
	failsWith("cursor ahead of the stream", err, contracts.ErrValidationFailed)
	other, err := fake.Client(sdktest.Claims{PrincipalID: "someone-else"}, func(o *sdkgo.ClientOptions) { o.HTTPClient = &http.Client{Transport: rec} })
	must("second client", err)
	_, err = other.StreamTurnEvents(ctx, "ses-1", turnID)
	failsWith("another principal's turn", err, contracts.ErrScopeViolation)
	_, err = other.GetAction(ctx, declined.Confirmation.PreparationID)
	failsWith("another principal's preparation", err, contracts.ErrScopeViolation)

	// rag
	r := client.RAG()
	_, err = r.RegisterSource(ctx, "unknown")
	failsWith("an undeclared source", err, contracts.ErrValidationFailed)
	source, err := r.RegisterSource(ctx, "clinic-policies")
	must("register source", err)
	id := string(source.Status.SourceID)
	_, err = r.RegisterSource(ctx, "clinic-policies")
	must("register again", err)
	list, err := r.ListSources(ctx)
	must("list", err)
	if len(list) != 1 {
		t.Fatalf("one source is registered, got %d", len(list))
	}
	_, err = r.GetSource(ctx, id)
	must("get source", err)
	_, err = r.GetSource(ctx, "src_00000000000000000000000000")
	failsWith("an unknown source", err, contracts.ErrValidationFailed)
	_, err = r.SyncSource(ctx, id)
	must("sync", err)
	_, err = r.RebuildSource(ctx, id)
	must("rebuild", err)
	generations, err := r.ListGenerations(ctx, id)
	must("generations", err)
	_, err = r.Search(ctx, rag.SearchRequest{Query: "cancelled free of charge"})
	must("search", err)
	answer, err := r.Answer(ctx, rag.AnswerRequest{Question: "can an appointment be cancelled free of charge"})
	must("answer", err)
	grounded, ok := sdkgo.Answered(*answer)
	if !ok || len(grounded.Citations) == 0 {
		t.Fatalf("a grounded answer has citations, got %+v", answer)
	}
	ref, isFile := grounded.Citations[0].OpenRef.Value.(conversation.CitationOpenRefFile)
	if !isFile {
		t.Fatalf("a citation opens through a file reference, got %T", grounded.Citations[0].OpenRef.Value)
	}
	fileID := string(ref.FileID)
	_, err = r.ResolveCitation(ctx, fileID)
	must("resolve", err)
	nothing, err := r.Answer(ctx, rag.AnswerRequest{Question: "what is the airspeed of a swallow"})
	must("no evidence", err)
	if _, ok := sdkgo.NoEvidence(*nothing); !ok {
		t.Fatalf("an unrelated question has no supported evidence, got %+v", nothing)
	}
	_, err = r.ReauditSources(ctx)
	must("reaudit", err)
	_, err = r.InvalidateGeneration(ctx, id, string(generations[0].GenerationID))
	must("invalidate", err)
	must("remove", r.RemoveSource(ctx, id))

	// the decision status (advisory): needs its capability, and a server without the service answers unavailable
	statusClaims := sdktest.Claims{CapabilityScopes: []string{sdktest.DecisionStatusCapability}}
	decisionClient, err := fake.Client(statusClaims, func(o *sdkgo.ClientOptions) { o.HTTPClient = &http.Client{Transport: rec} })
	must("decision client", err)
	fake.SetDecisionStatus(sdktest.DecisionStatusFixture(decision.DecisionStatusModeShadow, 3, map[decision.FallbackReason]int64{decision.FallbackReasonLowConfidence: 2, decision.FallbackReasonTimeout: 1}))
	_, err = decisionClient.Decision().Status(ctx)
	must("decision status", err)
	_, err = client.Decision().Status(ctx)
	failsWith("a token without the status capability", err, contracts.ErrPermissionDenied)
	fake.SetDecisionUnavailable(true)
	_, err = decisionClient.Decision().Status(ctx)
	failsWith("no decision service", err, contracts.ErrUnavailable)
	fake.SetDecisionUnavailable(false)

	// Dictation (record-to-text, 1.1.0): capabilities and one-shot create; not the voice WebSocket.
	_, err = client.Dictations().Capabilities(ctx)
	must("dictation capabilities", err)
	fake.SetDictationText("book Tuesday")
	_, err = client.Dictations().Create(ctx, "ses-1", wavClip(0.5), &sdkgo.DictationCreateOptions{Language: "en"})
	must("dictation create", err)
	fake.SetDictationSilent(true)
	_, err = client.Dictations().Create(ctx, "ses-1", wavClip(0.2), nil)
	failsWith("a silent clip", err, contracts.ErrValidationFailed)
	fake.SetDictationSilent(false)
	fake.SetDictationUnavailable(true)
	_, err = client.Dictations().Capabilities(ctx)
	failsWith("no transcription service", err, contracts.ErrUnavailable)
	fake.SetDictationUnavailable(false)

	// External Provider Mode: the providers and OAuth areas, every operation, with a recorded client (checked against the
	// document) and a plain one (the budget create, which the document's request schema cannot describe)
	exerciseProvidersConformance(t, fake,
		func(c sdktest.Claims) *sdkgo.Client {
			cl, err := fake.Client(c, func(o *sdkgo.ClientOptions) { o.HTTPClient = &http.Client{Transport: rec} })
			must("providers client", err)
			return cl
		},
		func(c sdktest.Claims) *sdkgo.Client {
			cl, err := fake.Client(c)
			must("plain providers client", err)
			return cl
		})

	// Per-principal LLM (ADR 0041) and long-term memory (ADR 0042).
	_, err = client.Me().Catalog(ctx)
	must("provider catalog", err)
	_, err = client.Me().LLM(ctx)
	must("principal llm", err)
	_, err = client.Me().PutRole(ctx, "conversational_controller", pv.PrincipalLlmBindingUpdate{ProfileID: "pprf_01J0000000000000000000000A"})
	must("put principal llm role", err)
	_, err = client.Me().DeleteRole(ctx, "conversational_controller")
	must("delete principal llm role", err)
	_, err = client.Me().SubmitCredential(ctx, "openai", "sk-test")
	must("principal credential", err)
	_, err = client.Me().DeleteCredential(ctx, "openai")
	must("delete principal credential", err)
	_, err = client.Me().AcknowledgeBillingNotice(ctx, "openai", sdkgo.PrincipalBillingAck{
		NoticeVersion: "2026-01", ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true,
	})
	must("principal billing ack", err)
	_, err = client.Me().StartOAuth(ctx, "openai", sdkgo.PrincipalOAuthStart{Capabilities: []string{"chat"}, RedirectURI: "http://127.0.0.1:5555/cb"})
	must("principal oauth start", err)
	_, err = client.Me().CompleteOAuth(ctx, sdkgo.PrincipalOAuthCallback{RedirectURI: "https://host.example/cb", Query: "code=c&state=s"})
	must("principal oauth callback", err)
	_, err = client.Me().OAuthFlowStatus(ctx, "oaf_01J0000000000000000000000A")
	must("principal oauth flow status", err)
	must("principal oauth flow cancel", client.Me().CancelOAuthFlow(ctx, "oaf_01J0000000000000000000000A"))
	_, err = client.Ops().StartErasure(ctx, sdkgo.ErasureRequest{Level: "principal", PrincipalID: "usr_alice", Reason: "gdpr"})
	must("ops erasure start", err)
	_, err = client.Ops().ErasureStatus(ctx, "erj_01J0000000000000000000000A")
	must("ops erasure status", err)
	_, err = client.Ops().PutPrivateLlmEndpoint(ctx, "ple_01J0000000000000000000000A", map[string]any{
		"origin": "http://127.0.0.1:8080", "api_dialect": "openai-compatible",
		"model_ids": []string{"local-model"}, "roles": []string{"conversational_controller"}, "owner_kind": "shared",
	})
	must("ops put private llm endpoint", err)
	_, err = client.Ops().GetPrivateLlmEndpoint(ctx, "ple_01J0000000000000000000000A")
	must("ops get private llm endpoint", err)
	must("ops delete private llm endpoint", client.Ops().DeletePrivateLlmEndpoint(ctx, "ple_01J0000000000000000000000A"))
	_, err = client.Ops().PutTenantPrivateLlm(ctx, "ten-1", map[string]any{"endpoint_id": "ple_01J0000000000000000000000A"})
	must("ops put tenant private llm", err)
	_, err = client.Ops().GetTenantPrivateLlm(ctx, "ten-1")
	must("ops get tenant private llm", err)
	must("ops delete tenant private llm", client.Ops().DeleteTenantPrivateLlm(ctx, "ten-1"))
	_, err = client.Ops().PutPrincipalPrivateLlm(ctx, "usr_alice", map[string]any{
		"endpoint_id": "ple_01J0000000000000000000000A", "roles": []string{"conversational_controller"},
	})
	must("ops put principal private llm", err)
	_, err = client.Ops().GetPrincipalPrivateLlm(ctx, "usr_alice")
	must("ops get principal private llm", err)
	must("ops delete principal private llm", client.Ops().DeletePrincipalPrivateLlm(ctx, "usr_alice"))
	_, err = client.Me().Usage(ctx, sdkgo.MeUsageQuery{})
	must("me usage", err)
	_, err = client.Me().Budget(ctx)
	must("me budget", err)
	_, err = client.Me().Analyst(ctx)
	must("me analyst", err)
	_, err = client.Ops().GetTenantAnalyst(ctx, "ten-1")
	must("ops get tenant analyst", err)
	_, err = client.Ops().PutTenantAnalyst(ctx, "ten-1", an.TenantSettings{})
	must("ops put tenant analyst", err)
	_, err = client.Memory().List(ctx, nil)
	must("memory list", err)
	created, err := client.Memory().Create(ctx, memgen.MemoryCreate{Text: "prefer dark mode"})
	must("memory create", err)
	patched := memgen.MemoryText("prefer light mode")
	_, err = client.Memory().Patch(ctx, string(created.ID), memgen.MemoryPatch{Text: &patched})
	must("memory patch", err)
	_, err = client.Memory().Confirm(ctx, string(created.ID))
	must("memory confirm", err)
	_, err = client.Memory().Reject(ctx, string(created.ID))
	must("memory reject", err)
	must("memory delete", client.Memory().Delete(ctx, string(created.ID)))
	_, err = client.Exports().Create(ctx, "ses-1", conversation.ExportRequest{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", Format: conversation.ExportRequestFormatHtml,
	})
	must("export create", err)
	_, err = client.Exports().Create(ctx, "ses-1", conversation.ExportRequest{
		TurnID: "trn_01ARZ3NDEKTSV4RRFFQ69G5FA0", Format: conversation.ExportRequestFormatCsv,
	})
	failsWith("export missing turn", err, contracts.ErrValidationFailed)

	// failure paths: scope, authentication, availability, readiness, quarantine, ended session
	_, err = client.GetSession(ctx, "ses-9")
	failsWith("another session", err, contracts.ErrScopeViolation)
	_, err = client.GetApplication(ctx, "someone-else")
	failsWith("another application", err, contracts.ErrScopeViolation)
	fake.FailNext("listCapabilities", http.StatusServiceUnavailable, contracts.ErrUnavailable, "down")
	_, err = client.ListCapabilities(ctx)
	failsWith("a queued failure", err, contracts.ErrUnavailable)
	fixed := fake.IssueToken(claims)
	reuse, err := sdkgo.NewClient(sdkgo.ClientOptions{
		BaseURL: fake.URL(), HTTPClient: &http.Client{Transport: rec},
		GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) { return fixed, nil },
	})
	must("reuse client", err)
	_, err = reuse.ListCapabilities(ctx)
	must("the first presentation", err)
	_, err = reuse.ListCapabilities(ctx)
	failsWith("a token presented twice", err, contracts.ErrReplayed)
	stranger, err := sdkgo.NewClient(sdkgo.ClientOptions{
		BaseURL: fake.URL(), HTTPClient: &http.Client{Transport: rec},
		GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) { return "not-a-token", nil },
	})
	must("stranger client", err)
	_, err = stranger.ListCapabilities(ctx)
	failsWith("an unknown token", err, contracts.ErrUnauthenticated)
	fake.SetReady(false)
	health, err := client.GetHealth(ctx)
	must("a service that is not ready reports it", err)
	if health.Ready {
		t.Fatal("the report says ready")
	}
	notReady, err := client.GetReadiness(ctx)
	must("readiness not ready", err)
	if notReady.Status != "unavailable" {
		t.Fatalf("readiness status %q", notReady.Status)
	}
	fake.SetReady(true)
	fake.SetApplicationStatus("quarantined")
	_, err = client.CreateSession(ctx)
	failsWith("a quarantined application", err, contracts.ErrQuarantined)
	_, err = client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("hello"))
	failsWith("a turn of a quarantined application", err, contracts.ErrQuarantined)
	_, err = client.ListCapabilities(ctx)
	failsWith("capabilities of a quarantined application", err, contracts.ErrQuarantined)
	fake.SetApplicationStatus("active")
	_, err = client.EndSession(ctx, "ses-1")
	must("end", err)
	_, err = client.EndSession(ctx, "ses-1")
	must("ending twice is the same as once", err)
	_, err = client.GetSession(ctx, "ses-1")
	failsWith("an ended session", err, contracts.ErrExpired)

	// the verdict
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.violations) > 0 {
		t.Errorf("%d exchange(s) do not conform to the OpenAPI document:\n%s", len(rec.violations), strings.Join(rec.violations, "\n"))
	}
	if rec.validated < 60 {
		t.Errorf("only %d bodies were validated against the document", rec.validated)
	}
	for _, op := range sdkgo.Operations() {
		if len(rec.statuses[op.ID]) == 0 {
			t.Errorf("the scenario never called %s", op.ID)
		}
	}
	reached := map[int]bool{}
	for _, set := range rec.statuses {
		for status := range set {
			reached[status] = true
		}
	}
	for _, status := range []int{200, 201, 202, 204, 400, 401, 403, 404, 409, 410, 429, 500, 503, 101} {
		if !reached[status] {
			t.Errorf("status %d was never exercised", status)
		}
	}
}

// goTypes is the Go type each operation's success answer decodes into. The decoding here refuses unknown
// members, so a member the document has and the Go type lacks (a mistyped json tag, a field the schema gained)
// fails: the hand-written wire types stay in step with the document, and the generated ones with the schemas.
var goTypes = map[string]func() any{
	"getLiveness":   func() any { return new(sdkgo.Liveness) },
	"getReadiness":  func() any { return new(sdkgo.ReadyReport) },
	"getHealth":     func() any { return new(sdkgo.Health) },
	"createSession": func() any { return new(sdkgo.Session) },
	"getSession":    func() any { return new(sdkgo.Session) },
	"endSession":    func() any { return new(sdkgo.SessionEnded) },
	"getPendingState": func() any {
		return new(struct {
			Pending *sdkgo.PendingState `json:"pending"`
		})
	},
	"sendTurn":            func() any { return new(sdkgo.TurnResult) },
	"getAction":           func() any { return new(actions.PreparedAction) },
	"confirmAction":       func() any { return new(sdkgo.ActionOutcome) },
	"rejectAction":        func() any { return new(actions.PreparedAction) },
	"getActionReceipt":    func() any { return new(actions.ActionReceipt) },
	"registerApplication": func() any { return new(sdkgo.Registration) },
	"getApplication":      func() any { return new(sdkgo.Application) },
	"listCapabilities":    func() any { return new(sdkgo.CapabilityList) },
	"registerRagSource":   func() any { return new(sdkgo.Source) },
	"listRagSources": func() any {
		return new(struct {
			Sources []sdkgo.Source `json:"sources"`
		})
	},
	"reauditRagSources": func() any { return new(sdkgo.ReauditReport) },
	"getRagSource":      func() any { return new(sdkgo.Source) },
	"syncRagSource":     func() any { return new(sdkgo.SyncAccepted) },
	"rebuildRagSource":  func() any { return new(sdkgo.SyncAccepted) },
	"listRagGenerations": func() any {
		return new(struct {
			Generations []rag.IndexGeneration `json:"generations"`
		})
	},
	"invalidateRagGeneration":           func() any { return new(rag.IndexGeneration) },
	"searchRag":                         func() any { return new(rag.SearchResponse) },
	"answerRag":                         func() any { return new(rag.AnswerResult) },
	"resolveRagCitation":                func() any { return new(sdkgo.CitationResolution) },
	"getDecisionStatus":                 func() any { return new(decision.DecisionStatus) },
	"getDictationCapabilities":          func() any { return new(voice.TranscriptionCapabilities) },
	"createSessionDictation":            func() any { return new(voice.DictationTranscript) },
	"listProviders":                     func() any { return new(sdkgo.ProviderList) },
	"putProviderCredential":             func() any { return new(pv.ConnectionStatus) },
	"deleteProviderConnection":          func() any { return new(pv.ConnectionStatus) },
	"ackProviderBillingNotice":          func() any { return new(pv.BillingNoticeAck) },
	"getProviderDisclosure":             func() any { return new(pv.EgressDisclosure) },
	"getProviderMode":                   func() any { return new(sdkgo.ProviderMode) },
	"putProviderMode":                   func() any { return new(sdkgo.ProviderMode) },
	"getProviderEgressPolicy":           func() any { return new(sdkgo.EgressPolicyView) },
	"putProviderEgressPolicy":           func() any { return new(pv.EgressPolicy) },
	"getProviderBudgets":                func() any { return new(sdkgo.ProviderBudgets) },
	"putProviderBudget":                 func() any { return new(pv.Budget) },
	"exportProviderUsage":               func() any { return new(sdkgo.UsageExport) },
	"getProviderKillSwitch":             func() any { return new(sdkgo.KillSwitch) },
	"putProviderKillSwitch":             func() any { return new(sdkgo.KillSwitch) },
	"startProviderOAuth":                func() any { return new(sdkgo.OAuthStarted) },
	"completeProviderOAuth":             func() any { return new(pv.ConnectionStatus) },
	"getProviderOAuthFlow":              func() any { return new(pv.OauthFlowStatus) },
	"cancelProviderOAuthFlow":           func() any { return nil }, // 204
	"getProviderCatalog":                func() any { return new(pv.ProviderCatalog) },
	"getPrincipalLlm":                   func() any { return new(pv.PrincipalLlm) },
	"putPrincipalLlmRole":               func() any { return new(pv.PrincipalLlm) },
	"deletePrincipalLlmRole":            func() any { return new(pv.PrincipalLlm) },
	"putPrincipalProviderCredential":    func() any { return new(sdkgo.PrincipalCredentialStatus) },
	"deletePrincipalProviderCredential": func() any { return new(sdkgo.PrincipalCredentialStatus) },
	"ackPrincipalProviderBillingNotice": func() any { return new(sdkgo.PrincipalBillingAckResult) },
	"startPrincipalOAuth":               func() any { return new(sdkgo.PrincipalOAuthStarted) },
	"completePrincipalOAuth":            func() any { return new(sdkgo.PrincipalCredentialStatus) },
	"getPrincipalOAuthFlow":             func() any { return new(sdkgo.PrincipalOAuthFlowStatus) },
	"cancelPrincipalOAuthFlow":          func() any { return nil }, // 204
	"startErasure":                      func() any { return new(sdkgo.ErasureJobAccepted) },
	"getErasureJob":                     func() any { return new(sdkgo.ErasureJobStatus) },
	"putPrivateLlmEndpoint":             func() any { return new(pv.PrivateLlmEndpoint) },
	"getPrivateLlmEndpoint":             func() any { return new(pv.PrivateLlmEndpoint) },
	"deletePrivateLlmEndpoint":          func() any { return nil }, // 204
	"putTenantPrivateLlm":               func() any { return new(pv.PrivateLlmTenantPin) },
	"getTenantPrivateLlm":               func() any { return new(pv.PrivateLlmTenantPin) },
	"deleteTenantPrivateLlm":            func() any { return nil }, // 204
	"putPrincipalPrivateLlm":            func() any { return new(pv.PrivateLlmPrincipalBinding) },
	"getPrincipalPrivateLlm":            func() any { return new(pv.PrivateLlmPrincipalBinding) },
	"deletePrincipalPrivateLlm":         func() any { return nil }, // 204
	"getMeUsage":                        func() any { return new(pv.MeUsage) },
	"getMeBudget":                       func() any { return new(pv.MeBudget) },
	"getPrincipalAnalyst":               func() any { return new(an.Abilities) },
	"getTenantAnalyst":                  func() any { return new(an.TenantSettings) },
	"putTenantAnalyst":                  func() any { return new(an.TenantSettings) },
	"listMemory":                        func() any { return new(memgen.MemoryList) },
	"createMemory":                      func() any { return new(memgen.MemoryItem) },
	"patchMemory":                       func() any { return new(memgen.MemoryItem) },
	"deleteMemory":                      func() any { return nil }, // 204
	"confirmMemory":                     func() any { return new(memgen.MemoryItem) },
	"rejectMemory":                      func() any { return new(memgen.MemoryItem) },
	"createExport":                      func() any { return new(conversation.FileRef) },
}

func (c *conformance) checkGoType(opID string, body []byte) {
	if opID == "removeRagSource" || opID == "streamTurnEvents" || opID == "cancelProviderOAuthFlow" || opID == "cancelPrincipalOAuthFlow" || opID == "deleteMemory" ||
		opID == "deletePrivateLlmEndpoint" || opID == "deleteTenantPrivateLlm" || opID == "deletePrincipalPrivateLlm" {
		return
	}
	newValue, ok := goTypes[opID]
	if !ok {
		c.violate("%s: no Go type is registered for its answer (add it to goTypes)", opID)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(newValue()); err != nil {
		c.violate("%s: the answer does not decode into its Go type: %v\n%s", opID, err, body)
	}
}
