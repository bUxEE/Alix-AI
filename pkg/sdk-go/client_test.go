package sdkgo_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

// harness is a client of the fake whose token provider remembers what it was asked for.
type harness struct {
	t      *testing.T
	fake   *sdktest.Fake
	client *sdkgo.Client
	claims sdktest.Claims

	mu     sync.Mutex
	tokens []sdkgo.TokenRequest
}

func newHarness(t *testing.T, mutate ...func(*sdkgo.ClientOptions)) *harness {
	t.Helper()
	h := &harness{t: t, fake: sdktest.New()}
	t.Cleanup(h.fake.Close)
	h.claims = sdktest.Claims{CapabilityScopes: []string{"demo.lookup", "demo.book", "demo.close_account"}}
	opts := append([]func(*sdkgo.ClientOptions){func(o *sdkgo.ClientOptions) {
		o.GetContextToken = func(_ context.Context, req sdkgo.TokenRequest) (string, error) {
			h.mu.Lock()
			h.tokens = append(h.tokens, req)
			h.mu.Unlock()
			return h.fake.IssueToken(h.claims), nil
		}
	}}, mutate...)
	client, err := h.fake.Client(h.claims, opts...)
	if err != nil {
		t.Fatal(err)
	}
	h.client = client
	return h
}

func (h *harness) tokenRequests() []sdkgo.TokenRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sdkgo.TokenRequest(nil), h.tokens...)
}

func must[T any](t *testing.T, what string) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return v
	}
}

func hasCode(t *testing.T, what string, err error, code contracts.ErrCode) *sdkgo.APIError {
	t.Helper()
	if !contracts.HasCode(err, code) {
		t.Fatalf("%s: want code %s, got %v", what, code, err)
	}
	var api *sdkgo.APIError
	errors.As(err, &api)
	return api
}

// seqs are the seq numbers of events, in order.
func seqs(t *testing.T, events []conversation.TurnEvent) []int64 {
	t.Helper()
	out := make([]int64, len(events))
	for i, ev := range events {
		env, ok := sdkgo.Envelope(ev)
		if !ok {
			t.Fatalf("event %d has no variant", i)
		}
		out[i] = env.Seq
	}
	return out
}

func assertContiguous(t *testing.T, events []conversation.TurnEvent) {
	t.Helper()
	for i, seq := range seqs(t, events) {
		if seq != int64(i) {
			t.Fatalf("events must run from seq 0 without a gap or a repeat, got %v", seqs(t, events))
		}
	}
}

const twoHundred = "abcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghij" +
	"abcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghijabcdefghij"

func TestSessionLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	session := must[*sdkgo.Session](t, "create")(h.client.CreateSession(ctx))
	if session.SessionID != "ses-1" || session.Status != "active" || session.TurnCount != 0 || session.Pending != nil {
		t.Fatalf("a new session is the token's, active, empty: %+v", session)
	}
	again := must[*sdkgo.Session](t, "create again")(h.client.CreateSession(ctx))
	if again.SessionID != session.SessionID || !again.CreatedAt.Equal(session.CreatedAt) {
		t.Fatal("opening an existing session answers with the same session")
	}
	got := must[*sdkgo.Session](t, "get")(h.client.GetSession(ctx, "ses-1"))
	if got.SessionID != "ses-1" {
		t.Fatalf("get: %+v", got)
	}
	if pending := must[*sdkgo.PendingState](t, "pending")(h.client.GetPendingState(ctx, "ses-1")); pending != nil {
		t.Fatalf("no goal, no pending state, got %+v", pending)
	}
	turn := must[*sdkgo.TurnResult](t, "turn")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("hello")))
	if text := sdkgo.TurnText(turn.Events); text == "" {
		t.Fatal("the turn carries text")
	}
	if got := must[*sdkgo.Session](t, "get after a turn")(h.client.GetSession(ctx, "ses-1")); got.TurnCount != 1 {
		t.Fatalf("the session counts its turns, got %d", got.TurnCount)
	}
	ended := must[*sdkgo.SessionEnded](t, "end")(h.client.EndSession(ctx, "ses-1"))
	if ended.Status != "ended" || ended.SessionID != "ses-1" {
		t.Fatalf("end: %+v", ended)
	}
	must[*sdkgo.SessionEnded](t, "ending twice is the same as once")(h.client.EndSession(ctx, "ses-1"))
	_, err := h.client.GetSession(ctx, "ses-1")
	api := hasCode(t, "an ended session", err, contracts.ErrExpired)
	if api.Status != http.StatusGone {
		t.Fatalf("an ended session is 410, got %d", api.Status)
	}
	_, err = h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("hello"))
	hasCode(t, "a turn on an ended session is an error, never a stream", err, contracts.ErrExpired)

	// The requests are the OpenAPI ones: the session ends with DELETE on the session, not a custom route.
	var seen []string
	for _, r := range h.fake.Requests() {
		seen = append(seen, r.Method+" "+r.Path)
	}
	want := []string{
		"POST /v1/sessions", "POST /v1/sessions", "GET /v1/sessions/ses-1", "GET /v1/sessions/ses-1/pending",
		"POST /v1/sessions/ses-1/turns", "GET /v1/sessions/ses-1", "DELETE /v1/sessions/ses-1", "DELETE /v1/sessions/ses-1",
		"GET /v1/sessions/ses-1", "POST /v1/sessions/ses-1/turns",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
	// One fresh token per request, told what it is for; the fake would answer `replayed` to a reused one.
	asked := h.tokenRequests()
	if len(asked) != len(want) || asked[0].Operation != "createSession" || asked[6].Operation != "endSession" || asked[6].SessionID != "ses-1" {
		t.Fatalf("the provider is called once per request, with the operation: %+v", asked)
	}

	// Probes need no token; a malformed argument never costs one.
	before := len(h.tokenRequests())
	must[*sdkgo.Liveness](t, "liveness")(h.client.GetLiveness(ctx))
	must[*sdkgo.ReadyReport](t, "readiness")(h.client.GetReadiness(ctx))
	for _, bad := range []string{"", "..", "a/b", strings.Repeat("a", 129), "with space"} {
		if _, err := h.client.GetSession(ctx, bad); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Fatalf("session id %q must be refused locally, got %v", bad, err)
		}
	}
	if _, err := h.client.SendTurn(ctx, "ses-1", sdkgo.TurnInput{}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("an empty turn input is refused locally, got %v", err)
	}
	both := sdkgo.TextTurn("hi")
	both.ChoiceID = "c1"
	if _, err := h.client.SendTurnJSON(ctx, "ses-1", both); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("a turn with two inputs is refused locally, got %v", err)
	}
	if _, err := h.client.SendTurn(ctx, "ses-1", sdkgo.ConfirmTurn("prep_x", "cfm_short")); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("a malformed confirmation is refused locally, got %v", err)
	}
	if _, err := h.client.RegisterApplication(ctx, []byte("not gzip")); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("a package that is not gzip is refused locally, got %v", err)
	}
	if n := len(h.tokenRequests()); n != before {
		t.Fatalf("probes and malformed arguments must not request a token: %d requested", n-before)
	}
}

func TestStreamReconnect(t *testing.T) {
	ctx := context.Background()
	long := func(h *harness) {
		h.fake.OnTurn(func(sdktest.Turn) sdktest.TurnPlan { return sdktest.Reply(twoHundred) }) // 5 tokens and done: seq 0 to 5
	}

	t.Run("a dropped stream resumes after the last event, once and in order", func(t *testing.T) {
		h := newHarness(t)
		long(h)
		h.fake.DropNext(2)
		stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go")))
		result := must[*sdkgo.TurnResult](t, "result")(stream.Result())
		if len(result.Events) != 6 {
			t.Fatalf("5 token events and done, got %v", seqs(t, result.Events))
		}
		assertContiguous(t, result.Events)
		if got := sdkgo.TurnText(result.Events); got != twoHundred {
			t.Fatalf("the text is whole after a resume: %q", got)
		}
		resumes := h.fake.RequestsOf("streamTurnEvents")
		if len(resumes) != 1 || resumes[0].LastEventID != "1" || resumes[0].WebSocket {
			t.Fatalf("one resume over SSE, after seq 1: %+v", resumes)
		}
		if n := len(h.fake.RequestsOf("sendTurn")); n != 1 {
			t.Fatalf("the turn is never sent again, got %d sends", n)
		}
		if turns := must[*sdkgo.Session](t, "session")(h.client.GetSession(ctx, "ses-1")).TurnCount; turns != 1 {
			t.Fatalf("the turn ran once, got %d", turns)
		}
		// Every open needed its own token, told what it was for; the fake answers `replayed` to a reused one.
		var ops []string
		for _, r := range h.tokenRequests() {
			ops = append(ops, r.Operation)
		}
		if got := strings.Join(ops[:2], ","); got != "sendTurn,streamTurnEvents" {
			t.Fatalf("the send and the resume each ask for a token: %v", ops)
		}
		if asked := h.tokenRequests(); asked[1].SessionID != "ses-1" {
			t.Fatalf("a resume names the session: %+v", asked[1])
		}
	})

	t.Run("the confirmation secret survives a resume", func(t *testing.T) {
		h := newHarness(t)
		h.fake.OnTurn(func(sdktest.Turn) sdktest.TurnPlan {
			return sdktest.PrepareAction(sdktest.PrepareInput{CapabilityID: "demo.book", Summary: "Book the 10:00 slot"})
		})
		h.fake.DropNext(1)
		result := must[*sdkgo.TurnResult](t, "result")(must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("book"))).Result())
		assertContiguous(t, result.Events)
		if result.Confirmation == nil || !strings.HasPrefix(result.Confirmation.Token, "cfm_") || result.Confirmation.PreparationID == "" {
			t.Fatalf("the grant is the header's secret with the preparation of the event: %+v", result.Confirmation)
		}
		done, ok := sdkgo.DoneOf(result.Events)
		if !ok || done.PreparationID == nil || string(*done.PreparationID) != result.Confirmation.PreparationID {
			t.Fatalf("done names the preparation: %+v", done)
		}
		must[*sdkgo.ActionOutcome](t, "confirm with the grant of a resumed stream")(h.client.ConfirmAction(ctx, result.Confirmation.PreparationID, result.Confirmation.Token))
	})

	t.Run("a WebSocket resume, and a dropped WebSocket resumes again", func(t *testing.T) {
		h := newHarness(t)
		long(h)
		h.fake.DropNext(1) // the POST stream, after seq 0
		h.fake.DropNext(1) // the WebSocket resume, after seq 1: closed with 1013
		stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go"), sdkgo.WithWebSocket()))
		result := must[*sdkgo.TurnResult](t, "result")(stream.Result())
		assertContiguous(t, result.Events)
		if len(result.Events) != 6 || sdkgo.TurnText(result.Events) != twoHundred {
			t.Fatalf("whole turn expected, got %v", seqs(t, result.Events))
		}
		resumes := h.fake.RequestsOf("streamTurnEvents")
		if len(resumes) != 2 || !resumes[0].WebSocket || !resumes[1].WebSocket ||
			!strings.Contains(resumes[0].RawQuery, "cursor=0") || !strings.Contains(resumes[1].RawQuery, "cursor=1") {
			t.Fatalf("two WebSocket resumes, after seq 0 and after seq 1: %+v", resumes)
		}
		if resumes[0].Status != http.StatusSwitchingProtocols {
			t.Fatalf("the resume upgraded the connection, got %d", resumes[0].Status)
		}
	})

	t.Run("StreamTurnEvents reads a turn again from a cursor, over SSE and over a WebSocket", func(t *testing.T) {
		h := newHarness(t)
		long(h)
		sent := must[*sdkgo.TurnResult](t, "turn")(must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go"))).Result())
		for name, opts := range map[string][]sdkgo.CallOption{
			"sse": {sdkgo.WithCursor(2)}, "websocket": {sdkgo.WithCursor(2), sdkgo.WithWebSocket()},
		} {
			rest := must[*sdkgo.TurnResult](t, name)(must[*sdkgo.TurnStream](t, name)(h.client.StreamTurnEvents(ctx, "ses-1", sent.TurnID, opts...)).Result())
			if got := seqs(t, rest.Events); len(got) != 3 || got[0] != 3 || got[2] != 5 {
				t.Fatalf("%s: the events after seq 2 are 3 to 5, got %v", name, got)
			}
			if rest.Confirmation != nil {
				t.Fatalf("%s: the confirmation secret is never sent by the resume route", name)
			}
		}
		all := must[*sdkgo.TurnResult](t, "no cursor")(must[*sdkgo.TurnStream](t, "no cursor")(h.client.StreamTurnEvents(ctx, "ses-1", sent.TurnID)).Result())
		if len(all.Events) != 6 {
			t.Fatalf("no cursor is the start of the turn, got %v", seqs(t, all.Events))
		}
		atEnd := must[*sdkgo.TurnStream](t, "cursor at the end")(h.client.StreamTurnEvents(ctx, "ses-1", sent.TurnID, sdkgo.WithCursor(5)))
		if atEnd.Next() || atEnd.Err() != nil {
			t.Fatalf("a cursor at the end of a finished turn is a stream with nothing left: %v", atEnd.Err())
		}
	})

	t.Run("a stream that cannot be resumed is an uncertain outcome, typed", func(t *testing.T) {
		h := newHarness(t, func(o *sdkgo.ClientOptions) { o.MaxResumes = 1 })
		long(h)
		h.fake.DropNext(1)
		h.fake.DropNext(1)
		stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go")))
		_, err := stream.Result()
		api := hasCode(t, "the second drop exhausts the resume budget", err, contracts.ErrUncertainOutcome)
		if sdkgo.IsRetryable(err) || api != nil && api.Err.Retryable {
			t.Fatal("an uncertain outcome is never retryable")
		}
		if got := seqs(t, stream.Events()); len(got) != 2 {
			t.Fatalf("the events received before the failure stay readable, got %v", got)
		}
		if _, err := stream.Confirmation(); !contracts.HasCode(err, contracts.ErrUncertainOutcome) {
			t.Fatalf("no grant from a failed stream, got %v", err)
		}

		// A drop before the first event leaves the turn id unknown: there is nothing to resume.
		h2 := newHarness(t)
		h2.fake.DropNext(0)
		stream2 := must[*sdkgo.TurnStream](t, "send")(h2.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go")))
		_, err = stream2.Result()
		hasCode(t, "a drop before the first event", err, contracts.ErrUncertainOutcome)
		if n := len(h2.fake.RequestsOf("streamTurnEvents")); n != 0 {
			t.Fatalf("no resume without a turn id, got %d", n)
		}
	})

	t.Run("a cursor that expired is not resumed with a gap", func(t *testing.T) {
		h := newHarness(t)
		long(h)
		h.fake.RetainEvents(2)
		h.fake.DropNext(1)
		stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go")))
		_, err := stream.Result()
		api := hasCode(t, "a cursor behind the window", err, contracts.ErrExpired)
		if api.Status != http.StatusGone || api.Err.Details["reason"] != "cursor_expired" {
			t.Fatalf("410 with details.reason cursor_expired, got %+v", api)
		}
		if n := len(h.fake.RequestsOf("streamTurnEvents")); n != 1 {
			t.Fatalf("an expired cursor is not retried, got %d resumes", n)
		}
	})

	t.Run("a turn of another principal is not visible", func(t *testing.T) {
		h := newHarness(t)
		turn := must[*sdkgo.TurnResult](t, "turn")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("hello")))
		other, err := h.fake.Client(sdktest.Claims{PrincipalID: "someone-else"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = other.StreamTurnEvents(ctx, "ses-1", turn.TurnID)
		api := hasCode(t, "another principal", err, contracts.ErrScopeViolation)
		if api.Err.Details["reason"] != "turn_not_visible" {
			t.Fatalf("a turn id is no oracle: %+v", api.Err.Details)
		}
		_, err = h.client.StreamTurnEvents(ctx, "ses-1", "trn_unknown")
		hasCode(t, "an unknown turn is the same refusal", err, contracts.ErrScopeViolation)
	})

	t.Run("iteration, All and Close", func(t *testing.T) {
		h := newHarness(t)
		long(h)
		stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("go")))
		var n int
		for ev, err := range stream.All() {
			if err != nil {
				t.Fatal(err)
			}
			if env, _ := sdkgo.Envelope(ev); env.Seq != int64(n) {
				t.Fatalf("seq %d at position %d", env.Seq, n)
			}
			n++
			if n == 3 {
				break // leaving the loop releases the connection
			}
		}
		if stream.Err() != nil || stream.Next() {
			t.Fatalf("a stream closed early ends without an error and without more events: %v", stream.Err())
		}
		if _, err := stream.Confirmation(); !contracts.HasCode(err, contracts.ErrUncertainOutcome) {
			t.Fatalf("closed before done: the outcome is unknown, got %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		stream = must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(cancelled, "ses-1", sdkgo.TextTurn("go")))
		cancel()
		if _, err := stream.Result(); !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled context ends the stream with its own error, got %v", err)
		}
	})
}

// rawTurnServer answers every turn request with a fixed event stream, for streams a conforming server
// would never send.
func rawTurnServer(t *testing.T, body string) *sdkgo.Client {
	t.Helper()
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	})
	client, err := sdkgo.NewClient(sdkgo.ClientOptions{
		BaseURL: srv.URL, MaxResumes: -1,
		GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) { return "tok", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestStreamRefusesABrokenSequence(t *testing.T) {
	const trn = "trn_01J0000000000000000000000A"
	tok := func(seq int, turn string) string {
		return `{"type":"token","turn_id":"` + turn + `","seq":` + itoa(seq) + `,"text":"hi"}`
	}
	done := func(seq int) string {
		return `{"type":"done","turn_id":"` + trn + `","seq":` + itoa(seq) + `,"finish_reason":"completed"}`
	}
	frame := func(id int, event, data string) string {
		return "id: " + itoa(id) + "\nevent: " + event + "\ndata: " + data + "\n\n"
	}
	cases := map[string]struct {
		body string
		code contracts.ErrCode
	}{
		"a gap":                      {frame(0, "token", tok(0, trn)) + frame(2, "token", tok(2, trn)), contracts.ErrInternal},
		"another turn id":            {frame(0, "token", tok(0, trn)) + frame(1, "token", tok(1, "trn_01J0000000000000000000000B")), contracts.ErrInternal},
		"an unknown event type":      {frame(0, "mystery", `{"type":"mystery","turn_id":"`+trn+`","seq":0}`), contracts.ErrInternal},
		"an event name that lies":    {frame(0, "done", tok(0, trn)), contracts.ErrInternal},
		"an id that lies":            {frame(7, "token", tok(0, trn)), contracts.ErrInternal},
		"a cut-off frame":            {frame(0, "token", tok(0, trn)) + "id: 1\nevent: done\ndata: " + done(1), contracts.ErrUncertainOutcome},
		"an end before done":         {frame(0, "token", tok(0, trn)), contracts.ErrUncertainOutcome},
		"done without a preparation": {frame(0, "done", `{"type":"done","turn_id":"`+trn+`","seq":0,"finish_reason":"awaiting_confirmation"}`), contracts.ErrInternal},
		"not JSON":                   {"id: 0\nevent: token\ndata: {nope\n\n", contracts.ErrInternal},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client := rawTurnServer(t, c.body)
			stream, err := client.SendTurn(context.Background(), "ses-1", sdkgo.TextTurn("hi"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Result(); !contracts.HasCode(err, c.code) {
				t.Fatalf("want %s, got %v", c.code, err)
			}
		})
	}
	// The stream ends at done: whatever follows it is never read.
	trailing := frame(0, "token", tok(0, trn)) + frame(1, "done", done(1)) + "id: 2\nevent: mystery\ndata: {nope\n\n"
	stream, err := rawTurnServer(t, trailing).SendTurn(context.Background(), "ses-1", sdkgo.TextTurn("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := stream.Result(); err != nil || len(result.Events) != 2 {
		t.Fatalf("a turn is over at its done event: %v %+v", err, result)
	}
	// CRLF and CR line ends, comments and a split frame are the same events.
	ok := ": comment\r\nid: 0\r\nevent: token\r\ndata: " + tok(0, trn) + "\r\n\r\nid: 1\revent: done\rdata: " + done(1) + "\r\r"
	stream, err = rawTurnServer(t, ok).SendTurn(context.Background(), "ses-1", sdkgo.TextTurn("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if result, err := stream.Result(); err != nil || len(result.Events) != 2 {
		t.Fatalf("a valid stream with CRLF and CR line ends: %v %+v", err, result)
	}
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResumeRetriesATransportFailureOfTheResume(t *testing.T) {
	const trn = "trn_01J0000000000000000000000A"
	sse := func(r *http.Request, body string) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: r}
	}
	var mu sync.Mutex
	var resumes []string
	client, err := sdkgo.NewClient(sdkgo.ClientOptions{
		BaseURL: "http://127.0.0.1:9", ResumeBackoff: time.Millisecond,
		GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) { return "tok", nil },
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost { // seq 0, then the connection ends before done
				return sse(r, "id: 0\nevent: token\ndata: {\"type\":\"token\",\"turn_id\":\""+trn+"\",\"seq\":0,\"text\":\"hi\"}\n\n"), nil
			}
			last := r.Header.Get("Last-Event-ID")
			if v := r.Header["Last-Event-ID"]; len(v) > 0 { // set by its exact name, as it goes on the wire
				last = v[0]
			}
			mu.Lock()
			resumes = append(resumes, last)
			first := len(resumes) == 1
			mu.Unlock()
			if first { // the service is restarting: the resume cannot connect
				return nil, errors.New("connection refused")
			}
			return sse(r, "id: 1\nevent: done\ndata: {\"type\":\"done\",\"turn_id\":\""+trn+"\",\"seq\":1,\"finish_reason\":\"completed\"}\n\n"), nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.SendTurn(context.Background(), "ses-1", sdkgo.TextTurn("hi"))
	if err != nil {
		t.Fatal(err)
	}
	// The resume is a read of the turn's events: its transport failure is retryable, not the uncertain outcome of the send.
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("a resume that could not connect is tried again within MaxResumes, got %v", err)
	}
	if len(result.Events) != 2 || len(resumes) != 2 || resumes[0] != "0" || resumes[1] != "0" {
		t.Fatalf("the whole turn after two resumes from seq 0, got %d events and resumes %v", len(result.Events), resumes)
	}
}

func TestConfirmFlow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	book := func(commit sdktest.CommitPlan) *sdkgo.TurnResult {
		h.fake.OnTurn(func(sdktest.Turn) sdktest.TurnPlan {
			return sdktest.PrepareAction(sdktest.PrepareInput{CapabilityID: "demo.book", Summary: "Book the 10:00 slot", Commit: commit})
		})
		stream := must[*sdkgo.TurnStream](t, "send")(h.client.SendTurn(ctx, "ses-1", sdkgo.TextTurn("book me in")))
		result := must[*sdkgo.TurnResult](t, "result")(stream.Result())
		if result.Confirmation == nil {
			t.Fatal("the turn prepared an action that needs the user: it carries a confirmation grant")
		}
		return result
	}

	// A turn prepares an action; the secret is not a TurnEvent.
	turn := book(sdktest.CommitPlan{})
	grant := turn.Confirmation
	done, _ := sdkgo.DoneOf(turn.Events)
	if done.FinishReason != conversation.DoneEventFinishReasonAwaitingConfirmation {
		t.Fatalf("the turn awaits confirmation, got %s", done.FinishReason)
	}
	for _, ev := range turn.Events {
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), grant.Token) {
			t.Fatal("the confirmation secret is never part of an event")
		}
	}
	prepared := must[*sdkgo.PendingState](t, "pending")(h.client.GetPendingState(ctx, "ses-1"))
	if prepared == nil || prepared.Kind != "confirmation" || prepared.Preparation == nil || string(prepared.Preparation.PreparationID) != grant.PreparationID {
		t.Fatalf("the pending state is the confirmation of that preparation: %+v", prepared)
	}
	if raw, _ := json.Marshal(prepared); strings.Contains(string(raw), grant.Token) {
		t.Fatal("the pending state never carries the confirmation secret")
	}

	// Text can never confirm anything.
	h.fake.OnTurn(nil)
	must[*sdkgo.TurnResult](t, "yes")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("yes, confirm it")))
	if still := must[*sdkgo.PendingState](t, "pending")(h.client.GetPendingState(ctx, "ses-1")); still == nil {
		t.Fatal("a text turn must not confirm the pending action")
	}
	inspected := must[*sdkgo.Session](t, "session")(h.client.GetSession(ctx, "ses-1"))
	if inspected.Pending == nil {
		t.Fatal("the session carries its pending state")
	}

	// The wrong secret is refused and does not use the right one up.
	wrong := "cfm_" + strings.Repeat("A", 43)
	_, err := h.client.ConfirmAction(ctx, grant.PreparationID, wrong)
	hasCode(t, "a secret of another preparation", err, contracts.ErrPermissionDenied)
	// The right one commits, once.
	outcome := must[*sdkgo.ActionOutcome](t, "confirm")(h.client.ConfirmAction(ctx, grant.PreparationID, grant.Token))
	if outcome.Receipt.Status != "committed" || string(outcome.Receipt.PreparationID) != grant.PreparationID || len(outcome.Receipt.Errors) != 0 {
		t.Fatalf("committed receipt of that preparation: %+v", outcome.Receipt)
	}
	if outcome.Preparation == nil || outcome.Preparation.State != "committed" {
		t.Fatalf("the preparation is committed: %+v", outcome.Preparation)
	}
	_, err = h.client.ConfirmAction(ctx, grant.PreparationID, grant.Token)
	api := hasCode(t, "a second confirmation", err, contracts.ErrReplayed)
	if api.Status != http.StatusConflict || api.Receipt != nil {
		t.Fatalf("replayed is a bare 409: %+v", api)
	}
	stored := must[*sdkgo.PendingState](t, "pending after")(h.client.GetPendingState(ctx, "ses-1"))
	if stored != nil {
		t.Fatalf("nothing pends once the action is committed: %+v", stored)
	}
	read, err := h.client.GetActionReceipt(ctx, grant.PreparationID)
	if err != nil || read.ReceiptID != outcome.Receipt.ReceiptID {
		t.Fatalf("the receipt is durable: %v %+v", err, read)
	}

	// Declining sends nothing to the application and leaves no receipt.
	declined := book(sdktest.CommitPlan{}).Confirmation
	rejected := must[*actions.PreparedAction](t, "reject")(h.client.RejectAction(ctx, declined.PreparationID))
	if rejected.State != "rejected" {
		t.Fatalf("a rejected preparation: %+v", rejected)
	}
	_, err = h.client.ConfirmAction(ctx, declined.PreparationID, declined.Token)
	hasCode(t, "confirming a rejected preparation", err, contracts.ErrConflict)
	_, err = h.client.GetActionReceipt(ctx, declined.PreparationID)
	hasCode(t, "a declined action has no outcome", err, contracts.ErrConflict)

	// The conversation form: a structured confirmation input.
	viaTurn := book(sdktest.CommitPlan{}).Confirmation
	h.fake.OnTurn(nil)
	confirmed := must[*sdkgo.TurnResult](t, "confirm turn")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.ConfirmTurn(viaTurn.PreparationID, viaTurn.Token)))
	var sawReceipt bool
	for _, ev := range confirmed.Events {
		if r, ok := ev.Value.(conversation.ReceiptEvent); ok {
			sawReceipt = r.Receipt.Status == "committed"
		}
	}
	if !sawReceipt {
		t.Fatalf("a confirmation turn ends with the receipt: %v", confirmed.Events)
	}

	// A commit the host refused or could not finish is an error that still carries the receipt.
	for status, want := range map[string]struct {
		code   contracts.ErrCode
		http   int
		stored string
	}{
		"rejected":  {contracts.ErrConflict, http.StatusConflict, "rejected"},
		"partial":   {contracts.ErrPartialFailure, http.StatusInternalServerError, "partial"},
		"uncertain": {contracts.ErrUncertainOutcome, http.StatusInternalServerError, "uncertain"},
	} {
		g := book(sdktest.CommitPlan{Status: status}).Confirmation
		_, err := h.client.ConfirmAction(ctx, g.PreparationID, g.Token)
		api := hasCode(t, status, err, want.code)
		if api.Status != want.http || api.Receipt == nil || string(api.Receipt.Status) != want.stored || len(api.Receipt.Errors) == 0 {
			t.Fatalf("%s: the failure carries the receipt: %+v", status, api)
		}
		if api.Preparation == nil {
			t.Fatalf("%s: and the preparation as it stood", status)
		}
		if sdkgo.IsRetryable(err) {
			t.Fatalf("%s: a commit that did not succeed is never retried blindly", status)
		}
		stored, err := h.client.GetActionReceipt(ctx, g.PreparationID)
		if err != nil || stored.ReceiptID != api.Receipt.ReceiptID {
			t.Fatalf("%s: the receipt can be read later: %v", status, err)
		}
	}

	// A queued commit succeeds and says it is queued.
	queued := book(sdktest.CommitPlan{Status: "queued"}).Confirmation
	q := must[*sdkgo.ActionOutcome](t, "queued")(h.client.ConfirmAction(ctx, queued.PreparationID, queued.Token))
	if q.Receipt.Status != "queued" || q.Receipt.FollowUp == nil {
		t.Fatalf("queued receipt with its follow-up: %+v", q.Receipt)
	}

	// A preparation expires; the secret of an expired one is refused.
	now := time.Now()
	h.fake.SetClock(func() time.Time { return now })
	late := book(sdktest.CommitPlan{}).Confirmation
	h.fake.SetClock(func() time.Time { return now.Add(11 * time.Minute) })
	_, err = h.client.ConfirmAction(ctx, late.PreparationID, late.Token)
	hasCode(t, "an expired preparation", err, contracts.ErrExpired)
	// Another principal's preparation is the same refusal as one that does not exist.
	h.fake.SetClock(time.Now)
	other, err := h.fake.Client(sdktest.Claims{PrincipalID: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.ConfirmAction(ctx, grant.PreparationID, grant.Token)
	hasCode(t, "another principal's preparation", err, contracts.ErrScopeViolation)
	_, err = other.GetAction(ctx, "prep_01J0000000000000000000000Z")
	hasCode(t, "a preparation that does not exist", err, contracts.ErrScopeViolation)

	// A capability outside the token's scopes cannot be prepared.
	h.claims.CapabilityScopes = []string{"demo.lookup"}
	h.fake.OnTurn(func(sdktest.Turn) sdktest.TurnPlan {
		return sdktest.PrepareAction(sdktest.PrepareInput{CapabilityID: "demo.book", Summary: "Book"})
	})
	denied := must[*sdkgo.TurnResult](t, "denied")(h.client.SendTurnJSON(ctx, "ses-1", sdkgo.TextTurn("book")))
	if denied.Confirmation != nil {
		t.Fatal("a denied capability yields no confirmation secret")
	}
	last, _ := sdkgo.DoneOf(denied.Events)
	if last.FinishReason != conversation.DoneEventFinishReasonFailed {
		t.Fatalf("a denied turn ends failed: %s", last.FinishReason)
	}
}

func TestRAGAnswerWithCitations(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const passage = "Appointments can be cancelled free of charge up to 24 hours before the start."
	h.fake.DeclareSource("clinic-policies", "Clinic cancellation policy", passage)
	r := h.client.RAG()

	// A source is registered by the name the package declares, and by nothing else.
	_, err := r.RegisterSource(ctx, "made-up")
	api := hasCode(t, "an undeclared source", err, contracts.ErrValidationFailed)
	if api.Err.Details["reason"] != "not_declared" {
		t.Fatalf("details.reason not_declared, got %+v", api.Err.Details)
	}
	for _, bad := range []string{"", " padded ", strings.Repeat("a", 129), "tab\there"} {
		if _, err := r.RegisterSource(ctx, bad); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Fatalf("declaration %q must be refused locally, got %v", bad, err)
		}
	}
	source := must[*sdkgo.Source](t, "register")(r.RegisterSource(ctx, "clinic-policies"))
	if source.Declaration != "clinic-policies" || !source.Serving || source.Status.State != "ready" {
		t.Fatalf("a registered, serving source: %+v", source)
	}
	sourceID := string(source.Status.SourceID)
	if again := must[*sdkgo.Source](t, "again")(r.RegisterSource(ctx, "clinic-policies")); string(again.Status.SourceID) != sourceID {
		t.Fatal("registering the same declaration again is the same source")
	}

	// A grounded answer has a citation for every claim.
	answer := must[*rag.AnswerResult](t, "answer")(r.Answer(ctx, rag.AnswerRequest{Question: "Can I cancel an appointment free of charge?"}))
	grounded, ok := sdkgo.Answered(*answer)
	if !ok {
		t.Fatalf("the question is supported by the source: %+v", answer.Value)
	}
	if len(grounded.Claims) == 0 || len(grounded.Citations) == 0 {
		t.Fatalf("an answer has claims and citations: %+v", grounded)
	}
	for _, claim := range grounded.Claims {
		if len(claim.CitationIndexes) == 0 {
			t.Fatalf("every claim cites a source: %+v", claim)
		}
		for _, i := range claim.CitationIndexes {
			if int(i) >= len(grounded.Citations) {
				t.Fatalf("a citation index %d outside the %d citations", i, len(grounded.Citations))
			}
		}
	}
	citation := grounded.Citations[0]
	if string(citation.SourceID) != sourceID || citation.DisplayName != "Clinic cancellation policy" || len(citation.Claims) == 0 {
		t.Fatalf("the citation names the original source: %+v", citation)
	}
	file, isFile := citation.OpenRef.Value.(conversation.CitationOpenRefFile)
	if !isFile {
		t.Fatalf("a citation opens through a permission-checked reference, got %T", citation.OpenRef.Value)
	}

	// The reference resolves to the source as it stands now, and expires.
	resolved := must[*sdkgo.CitationResolution](t, "resolve")(r.ResolveCitation(ctx, string(file.FileID)))
	if string(resolved.SourceID) != sourceID || resolved.Preview != passage || resolved.Availability != conversation.CitationAvailabilityAvailable {
		t.Fatalf("the resolution: %+v", resolved)
	}
	other, err := h.fake.Client(sdktest.Claims{PrincipalID: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.RAG().ResolveCitation(ctx, string(file.FileID))
	hasCode(t, "another principal's reference", err, contracts.ErrPermissionDenied)
	_, err = r.ResolveCitation(ctx, "fil_01J0000000000000000000000Z")
	hasCode(t, "a reference that does not exist is the same refusal", err, contracts.ErrPermissionDenied)
	h.fake.SetClock(func() time.Time { return time.Now().Add(16 * time.Minute) })
	_, err = r.ResolveCitation(ctx, string(file.FileID))
	hasCode(t, "an expired reference", err, contracts.ErrExpired)
	h.fake.SetClock(time.Now)

	// Nothing supported is a typed result, not an error and never an uncited answer.
	nothing := must[*rag.AnswerResult](t, "no evidence")(r.Answer(ctx, rag.AnswerRequest{Question: "What is the airspeed of a swallow?"}))
	absent, ok := sdkgo.NoEvidence(*nothing)
	if !ok || absent.Reason != rag.NoSupportedEvidenceReasonNoMatch {
		t.Fatalf("no_supported_evidence with its reason: %+v", nothing.Value)
	}
	if _, isAnswer := sdkgo.Answered(*nothing); isAnswer {
		t.Fatal("an unsupported question has no answer")
	}
	// An answer that could not run is a typed error, never no_supported_evidence.
	h.fake.FailNext("answerRag", http.StatusServiceUnavailable, contracts.ErrUnavailable, "the model is down")
	_, err = r.Answer(ctx, rag.AnswerRequest{Question: "Can I cancel?"})
	hasCode(t, "an unavailable model", err, contracts.ErrUnavailable)
	if !sdkgo.IsRetryable(err) {
		t.Fatal("unavailable is retryable")
	}

	// Search and the sources API.
	hits := must[*rag.SearchResponse](t, "search")(r.Search(ctx, rag.SearchRequest{Query: "cancelled free of charge"}))
	if len(hits.Results) != 1 || string(hits.Results[0].SourceID) != sourceID || hits.Results[0].Excerpt != passage {
		t.Fatalf("one hit on the source: %+v", hits.Results)
	}
	empty := must[*rag.SearchResponse](t, "search nothing")(r.Search(ctx, rag.SearchRequest{Query: "swallow"}))
	if empty.Results == nil || len(empty.Results) != 0 {
		t.Fatalf("a search that matched nothing is an empty result, not an error: %+v", empty)
	}
	limit := int64(0)
	for _, bad := range []rag.SearchRequest{
		{Query: "  "}, {Query: strings.Repeat("a", 2001)}, {Query: "x\x00"}, {Query: "ok", Limit: &limit},
		{Query: "ok", SourceIDs: []actions.SourceID{"nope"}}, {Query: "ok", SourceIDs: []actions.SourceID{}},
	} {
		if _, err := r.Search(ctx, bad); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Fatalf("search %+v must be refused locally, got %v", bad, err)
		}
	}
	if _, err := r.Answer(ctx, rag.AnswerRequest{Question: ""}); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("an empty question is refused locally, got %v", err)
	}
	if list := must[[]sdkgo.Source](t, "list")(r.ListSources(ctx)); len(list) != 1 {
		t.Fatalf("one source: %+v", list)
	}
	accepted := must[*sdkgo.SyncAccepted](t, "sync")(r.SyncSource(ctx, sourceID))
	if string(accepted.SourceID) != sourceID || !strings.HasPrefix(accepted.JobID, "job_") {
		t.Fatalf("a queued job: %+v", accepted)
	}
	must[*sdkgo.SyncAccepted](t, "rebuild")(r.RebuildSource(ctx, sourceID))
	generations := must[[]rag.IndexGeneration](t, "generations")(r.ListGenerations(ctx, sourceID))
	if len(generations) != 1 || generations[0].State != rag.IndexGenerationStateActive {
		t.Fatalf("one active generation: %+v", generations)
	}
	invalid := must[*rag.IndexGeneration](t, "invalidate")(r.InvalidateGeneration(ctx, sourceID, string(generations[0].GenerationID)))
	if invalid.State != rag.IndexGenerationStateInvalidated {
		t.Fatalf("the generation is withdrawn: %+v", invalid)
	}
	if after := must[*rag.SearchResponse](t, "search")(r.Search(ctx, rag.SearchRequest{Query: "cancelled free of charge"})); len(after.Results) != 0 {
		t.Fatal("an invalidated generation is never retrievable again")
	}
	report := must[*sdkgo.ReauditReport](t, "reaudit")(r.ReauditSources(ctx))
	if report.Checked != 1 {
		t.Fatalf("one registration checked: %+v", report)
	}
	if err := r.RemoveSource(ctx, sourceID); err != nil {
		t.Fatal(err)
	}
	_, err = r.GetSource(ctx, sourceID)
	api = hasCode(t, "a removed source", err, contracts.ErrValidationFailed)
	if api.Status != http.StatusNotFound || api.Err.Details["reason"] != "not_found" {
		t.Fatalf("not_found: %+v", api)
	}
	for _, bad := range []string{"", "src_short", "SRC_01J0000000000000000000000A"} {
		if _, err := r.GetSource(ctx, bad); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Fatalf("source id %q is refused locally, got %v", bad, err)
		}
	}
}
