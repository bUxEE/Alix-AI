package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The tests of the turn-events area (M3-19) run the real stack of newAPIEnv with
// a real stream hub behind the real handlers: the POST turn publishes into the
// hub and the GET route reads from it. Only the model is scripted.

// ---- the hub of the test server ----

// turneventsHubConfig is the configuration the hub of the next test server is
// built with; a test that needs small limits sets it (and restores it) before
// newAPIEnv. The fake clock is the test environment's, so retention can be
// stepped past.
var turneventsHubConfig stream.Config

func init() {
	registerTestServices("v1_turnevents", func(t testing.TB, e *apiEnv, s *Services) {
		cfg := turneventsHubConfig
		cfg.Clock = e.clk
		if err := Provide(s, stream.NewHub(cfg)); err != nil {
			t.Fatal(err)
		}
	})
}

// turneventsWithHub builds a test server whose hub has cfg's limits.
func turneventsWithHub(t *testing.T, cfg stream.Config) *apiEnv {
	t.Helper()
	prev := turneventsHubConfig
	turneventsHubConfig = cfg
	t.Cleanup(func() { turneventsHubConfig = prev })
	return newAPIEnv(t)
}

func turneventsHub(t testing.TB, e *apiEnv) *stream.Hub {
	t.Helper()
	hub, ok := Lookup[*stream.Hub](e.svc)
	if !ok {
		t.Fatal("the test server has no stream hub")
	}
	return hub
}

// ---- requests ----

// turneventsFrame is one server-sent event.
type turneventsFrame struct{ ID, Event, Data string }

// turneventsStream is what an event-stream body holds: its events, whether it
// began with a retry hint and how many keep-alive comments it carried.
type turneventsStream struct {
	frames   []turneventsFrame
	retry    bool
	comments int
}

// turneventsParse reads an event-stream body. Unlike parseSSE it accepts the
// retry hint and the comments a GET stream carries.
func turneventsParse(t testing.TB, body string) turneventsStream {
	t.Helper()
	var out turneventsStream
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f turneventsFrame
		isFrame := false
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ":"):
				out.comments++
			case strings.HasPrefix(line, "retry: "):
				out.retry = true
			case strings.HasPrefix(line, "id: "):
				f.ID, isFrame = strings.TrimPrefix(line, "id: "), true
			case strings.HasPrefix(line, "event: "):
				f.Event, isFrame = strings.TrimPrefix(line, "event: "), true
			case strings.HasPrefix(line, "data: "):
				if f.Data != "" {
					t.Fatalf("a frame with two data lines: %q", block)
				}
				f.Data, isFrame = strings.TrimPrefix(line, "data: "), true
			default:
				t.Fatalf("unexpected line %q in %q", line, block)
			}
		}
		if isFrame {
			out.frames = append(out.frames, f)
		}
	}
	return out
}

func turneventsPath(turnID string) string {
	return sessionsPath(apiSess) + "/turns/" + turnID + "/events"
}

// turneventsGet reads the events of a turn with a fresh token. query is the
// raw query string without the "?".
func turneventsGet(e *apiEnv, turnID, query string, headers []string, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	path := turneventsPath(turnID)
	if query != "" {
		path += "?" + query
	}
	return e.callAs(e.token(mods...), http.MethodGet, path, nil, headers...)
}

// turneventsPostStream sends a turn asking for server-sent events.
func turneventsPostStream(e *apiEnv, in TurnRequest, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.callAs(e.token(mods...), http.MethodPost, sessionsPath(apiSess)+"/turns", in, "Accept", "text/event-stream")
}

// turneventsPrepare scripts and sends the turn that prepares items.create, as
// an event stream, and returns what it sent.
func turneventsPrepare(t *testing.T, e *apiEnv, response string) (*httptest.ResponseRecorder, turneventsStream, string) {
	t.Helper()
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse(response)
	rec := turneventsPostStream(e, TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	sent := turneventsParse(t, rec.Body.String())
	if len(sent.frames) < 8 || sent.retry {
		t.Fatalf("the turn stream = %d frames, retry hint %v", len(sent.frames), sent.retry)
	}
	return rec, sent, turneventsTurnID(t, sent.frames[0])
}

func turneventsTurnID(t testing.TB, f turneventsFrame) string {
	t.Helper()
	var head struct {
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal([]byte(f.Data), &head); err != nil || head.TurnID == "" {
		t.Fatalf("a frame without a turn id: %q (%v)", f.Data, err)
	}
	return head.TurnID
}

func turneventsSameFrames(t testing.TB, what string, got, want []turneventsFrame) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d frames, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: frame %d = %+v, want %+v", what, i, got[i], want[i])
		}
	}
}

// turneventsOpenTurn opens a turn of the default scope in the hub directly, for
// the tests of the transport that need a turn that is still being produced.
func turneventsOpenTurn(t testing.TB, e *apiEnv, n int) (*stream.Turn, string) {
	t.Helper()
	id := fmt.Sprintf("trn_01J0000000000000000000%04d", n)
	turn, err := turneventsHub(t, e).Begin(e.scopeOf(), id)
	if err != nil {
		t.Fatal(err)
	}
	return turn, id
}

func turneventsPublishToken(t testing.TB, turn *stream.Turn, text string) {
	t.Helper()
	ev := conversation.TokenEvent{TurnID: act.TurnID(turn.ID()), Seq: conversation.Seq(turn.NextSeq()), Text: text}
	if err := turn.Publish(conversation.TurnEvent{Value: ev}); err != nil {
		t.Fatalf("publish a token event: %v", err)
	}
}

func turneventsPublishDone(t testing.TB, turn *stream.Turn) {
	t.Helper()
	ev := conversation.DoneEvent{TurnID: act.TurnID(turn.ID()), Seq: conversation.Seq(turn.NextSeq()), FinishReason: conversation.DoneEventFinishReasonCompleted}
	if err := turn.Publish(conversation.TurnEvent{Value: ev}); err != nil {
		t.Fatalf("publish a done event: %v", err)
	}
}

// ---- resume ----

// A client that lost its connection resumes from the id of the last event it
// received, and receives exactly the rest of the turn, in order, byte for byte
// what the POST sent; the turn is not run again, and the confirmation secret is
// nowhere in a stream that is read again.
func TestTurnEventsResumeFromLastEventID(t *testing.T) {
	e := newAPIEnv(t)
	post, sent, turnID := turneventsPrepare(t, e, "Please confirm: create an item named widget.")
	n := len(sent.frames)
	if secret := post.Header().Get(HeaderConfirmationToken); !confirmTokenRE.MatchString(secret) {
		t.Fatalf("the turn did not hand out its confirmation secret: %q", secret)
	}
	if last := sent.frames[n-1]; last.Event != "done" || last.ID != strconv.Itoa(n-1) {
		t.Fatalf("the last frame = %+v", last)
	}
	modelCalls, hostCalls := e.model.Calls(), len(e.host.Calls())

	read := func(what, query string, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		rec := turneventsGet(e, turnID, query, headers)
		if strings.Contains(rec.Body.String(), "cfm_") || rec.Header().Get(HeaderConfirmationToken) != "" {
			t.Fatalf("%s: a stream that is read again carries the confirmation secret", what)
		}
		return rec
	}
	frames := func(what string, rec *httptest.ResponseRecorder) turneventsStream {
		t.Helper()
		wantStatus(t, rec, http.StatusOK)
		if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("%s: Content-Type = %q", what, ct)
		}
		got := turneventsParse(t, rec.Body.String())
		if !got.retry {
			t.Errorf("%s: no retry hint", what)
		}
		return got
	}

	// Without a cursor: the whole turn, the very frames the POST sent.
	turneventsSameFrames(t, "no cursor", frames("no cursor", read("no cursor", "", nil...)).frames, sent.frames)

	// From every event: Last-Event-ID, which is what an EventSource sends.
	for k := 0; k < n-1; k++ {
		what := "Last-Event-ID " + strconv.Itoa(k)
		got := frames(what, read(what, "", "Last-Event-ID", strconv.Itoa(k)))
		turneventsSameFrames(t, what, got.frames, sent.frames[k+1:])
	}
	// The cursor query parameter, for a reader that cannot set the header.
	got := frames("cursor", read("cursor", "cursor=3"))
	turneventsSameFrames(t, "cursor=3", got.frames, sent.frames[4:])
	// With both, the later of the two wins.
	got = frames("both", read("both", "cursor=4", "Last-Event-ID", "2"))
	turneventsSameFrames(t, "cursor=4 and Last-Event-ID 2", got.frames, sent.frames[5:])
	got = frames("both", read("both", "cursor=1", "Last-Event-ID", "5"))
	turneventsSameFrames(t, "cursor=1 and Last-Event-ID 5", got.frames, sent.frames[6:])

	// At the end of a finished turn there is nothing more: 204, the signal to
	// stop reconnecting, and no body.
	rec := read("end", "", "Last-Event-ID", strconv.Itoa(n-1))
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("a cursor at the end of the turn = %d %q, want 204 and no body", rec.Code, rec.Body.String())
	}

	// A cursor the stream has not reached, or that is not a sequence number, is
	// a 400 with a stable reason; a request never gets an empty stream that
	// looks like success.
	for name, c := range map[string]struct{ query, reason string }{
		"ahead":         {"cursor=" + strconv.Itoa(n), "cursor_ahead"},
		"far ahead":     {"cursor=4294967295", "cursor_ahead"},
		"text":          {"cursor=abc", "cursor_malformed"},
		"negative":      {"cursor=-1", "cursor_malformed"},
		"exponent":      {"cursor=1e3", "cursor_malformed"},
		"too many":      {"cursor=99999999999", "cursor_malformed"},
		"given twice":   {"cursor=1&cursor=2", "cursor_malformed"},
		"beyond the id": {"cursor=4294967296", "cursor_malformed"},
	} {
		env := wantError(t, read(name, c.query), http.StatusBadRequest, contracts.ErrValidationFailed)
		if env.Details["reason"] != c.reason {
			t.Errorf("%s: reason = %v, want %s", name, env.Details["reason"], c.reason)
		}
	}
	env := wantError(t, read("header", "", "Last-Event-ID", "x1"), http.StatusBadRequest, contracts.ErrValidationFailed)
	if env.Details["reason"] != "cursor_malformed" {
		t.Errorf("a malformed Last-Event-ID: %v", env.Details)
	}

	// Reading is only reading: the turn ran once.
	if e.model.Calls() != modelCalls || len(e.host.Calls()) != hostCalls {
		t.Errorf("a resume ran the turn again: model calls %d -> %d, host calls %d -> %d", modelCalls, e.model.Calls(), hostCalls, len(e.host.Calls()))
	}
	var turns int
	if err := e.db.QueryRowContext(context.Background(), `SELECT turn_count FROM sessions`).Scan(&turns); err != nil || turns != 1 {
		t.Errorf("the session has %d turns (%v), want the one that was sent", turns, err)
	}
}

// The turn is read back from the hub the way it is kept: a cursor behind the
// retained window is 410, never a stream with a gap, and the code `expired`
// that the gateway would map to 401 is a 410 here.
func TestTurnEventsCursorBehindTheWindowIsGone(t *testing.T) {
	e := turneventsWithHub(t, stream.Config{MaxEvents: 6})
	turn, id := turneventsOpenTurn(t, e, 1)
	for i := 0; i < 9; i++ {
		turneventsPublishToken(t, turn, "word"+strconv.Itoa(i)+" ")
	}
	turneventsPublishDone(t, turn)
	if st := turn.Stats(); st.Retained > 6 || st.FirstSeq == 0 {
		t.Fatalf("the ring did not forget anything: %+v", st)
	}
	first := int(turn.Stats().FirstSeq)

	env := wantError(t, turneventsGet(e, id, "cursor=0", nil), http.StatusGone, contracts.ErrExpired)
	if env.Details["reason"] != "cursor_expired" {
		t.Errorf("details = %v", env.Details)
	}
	wantError(t, turneventsGet(e, id, "", nil), http.StatusGone, contracts.ErrExpired) // from the start is behind it too
	// From the edge of the window it still resumes, exactly the rest.
	rec := turneventsGet(e, id, "cursor="+strconv.Itoa(first-1), nil)
	wantStatus(t, rec, http.StatusOK)
	got := turneventsParse(t, rec.Body.String())
	if len(got.frames) != 10-first || got.frames[0].ID != strconv.Itoa(first) || got.frames[len(got.frames)-1].Event != "done" {
		t.Fatalf("a resume from the edge of the window = %d frames from %q", len(got.frames), got.frames[0].ID)
	}
}

// ---- what the POST puts in the hub ----

// The POST turn opens the turn in the hub and publishes into it: there is
// something to resume. What the client got, as a document or as a stream, is
// what the hub holds, event for event.
func TestPostTurnPublishesIntoTheHub(t *testing.T) {
	e := newAPIEnv(t)
	hub := turneventsHub(t, e)
	if st := hub.Stats(); st.Turns != 0 {
		t.Fatalf("a new hub holds %d turns", st.Turns)
	}

	// A JSON turn.
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget.")
	rec := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, rec, http.StatusOK)
	body := decodeBody[TurnBody](t, rec)
	turn, err := hub.Lookup(e.scopeOf(), body.TurnID)
	if err != nil {
		t.Fatalf("the turn the POST answered for is not in the hub: %v", err)
	}
	st := turn.Stats()
	if !turn.Finished() || st.NextSeq != int64(len(body.Events)) || st.Retained != len(body.Events) || st.FirstSeq != 0 {
		t.Fatalf("hub turn = %+v, the response has %d events", st, len(body.Events))
	}
	sub, err := turn.Subscribe(stream.FromStart)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	raws := rawEvents(t, body)
	for i, want := range raws {
		f, err := sub.Next(context.Background())
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		var a, b bytes.Buffer
		if json.Compact(&a, want) != nil || json.Compact(&b, f.Data) != nil || a.String() != b.String() || f.Seq != int64(i) {
			t.Fatalf("event %d: the hub holds %s (seq %d), the response says %s", i, f.Data, f.Seq, want)
		}
	}
	if _, err := sub.Next(context.Background()); err != io.EOF {
		t.Fatalf("the hub holds more than the response: %v", err)
	}
	if hub.Stats().Turns != 1 {
		t.Errorf("hub turns = %d", hub.Stats().Turns)
	}

	// The turn is visible to its own scope and to no other: another principal,
	// tenant or session of the same deployment finds nothing.
	for name, mut := range map[string]func(*contracts.Scope){
		"another principal": func(s *contracts.Scope) { s.PrincipalID = "usr_mallory" },
		"another tenant":    func(s *contracts.Scope) { s.TenantID = apiOther },
		"another workspace": func(s *contracts.Scope) { s.WorkspaceID = apiWsp2 },
		"another session":   func(s *contracts.Scope) { s.SessionID = "ses_other_01" },
	} {
		sc := e.scopeOf()
		mut(&sc)
		if _, err := hub.Lookup(sc, body.TurnID); stream.ReasonOf(err) != stream.ReasonNotVisible {
			t.Errorf("%s: Lookup = %v, want turn_not_visible", name, err)
		}
	}

	// An event-stream turn is read back from the hub too, and a second turn is
	// a second hub turn.
	_, sent, id2 := turneventsPrepare(t, e, "Please confirm: create an item named widget.")
	if id2 == body.TurnID {
		t.Fatal("two turns have one id")
	}
	turn2, err := hub.Lookup(e.scopeOf(), id2)
	if err != nil || turn2.Stats().NextSeq != int64(len(sent.frames)) {
		t.Fatalf("the second turn in the hub = %+v, %v; the stream had %d frames", turn2, err, len(sent.frames))
	}
	if hub.Stats().Turns != 2 {
		t.Errorf("hub turns = %d, want 2", hub.Stats().Turns)
	}

	// A turn that could not be run opens nothing: there is nothing to resume.
	wantError(t, e.turn(TurnRequest{Text: "hello", Language: "xx-invalid"}), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.turn(TurnRequest{}), http.StatusBadRequest, contracts.ErrValidationFailed)
	if hub.Stats().Turns != 2 {
		t.Errorf("a turn that did not run opened a stream: hub turns = %d", hub.Stats().Turns)
	}
}

// ---- sanitised text ----

// The response text reaches the client through Turn.PublishResponse: sanitised
// as a whole before it is cut into token events, in the document, in the stream
// and in a resume alike. The verifier passes this text (it carries no fact it
// cannot account for), so the markup below is the stream's to remove.
func TestResponseMarkdownIsSanitisedInTokenEvents(t *testing.T) {
	e := newAPIEnv(t)
	raw := "Please confirm: create an item named widget. <b>Bold</b> and [docs](#anchor) and &lt;script&gt; and **strong**."
	want, report, err := blocks.Default().SanitizeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want == raw || strings.Contains(want, "<b>") || strings.Contains(want, "](") {
		t.Fatalf("the test text is not changed by the sanitiser: %q (%+v)", want, report)
	}

	tokens := func(what string, frames []turneventsFrame) string {
		t.Helper()
		var sb strings.Builder
		for _, f := range frames {
			if f.Event != "token" {
				continue
			}
			var ev struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
				t.Fatal(err)
			}
			sb.WriteString(ev.Text)
		}
		if sb.Len() == 0 {
			t.Fatalf("%s: no token event", what)
		}
		return sb.String()
	}

	// The stream of the POST.
	_, sent, id := turneventsPrepare(t, e, raw)
	got := tokens("POST stream", sent.frames)
	if strings.Contains(got, "Shall I go ahead") {
		t.Fatalf("the verifier replaced the model text with the template, so nothing was sanitised: %q", got)
	}
	if got != want {
		t.Fatalf("the token text = %q, want the sanitised %q", got, want)
	}
	// The same text, resumed.
	rec := turneventsGet(e, id, "", nil)
	wantStatus(t, rec, http.StatusOK)
	if resumed := tokens("resume", turneventsParse(t, rec.Body.String()).frames); resumed != want {
		t.Errorf("a resume carries %q, want %q", resumed, want)
	}
	// And as a JSON document.
	e.model.Reset()
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse(raw)
	doc := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
	wantStatus(t, doc, http.StatusOK)
	var joined strings.Builder
	for _, ev := range checkTurnEvents(t, rawEvents(t, decodeBody[TurnBody](t, doc))) {
		if ev["type"] == "token" {
			joined.WriteString(ev["text"].(string))
		}
	}
	if joined.String() != want {
		t.Errorf("the JSON document carries %q, want %q", joined.String(), want)
	}
	// Nowhere is there a tag or a link target as the model wrote it.
	for what, body := range map[string]string{"stream": rec.Body.String(), "document": doc.Body.String()} {
		for _, bad := range []string{"<b>", "</b>", "(#anchor)", "&lt;script"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s still carries %q", what, bad)
			}
		}
	}
}

// ---- sequence numbers ----

// A refused event does not use up its sequence number, and the events after it
// are numbered when they are published: the numbers never skip and the events
// never arrive out of order. Where an event cannot be sent the turn is failed
// (an error event and a failed done), never silently shorter.
func TestRefusedEventDoesNotBreakSequenceOrder(t *testing.T) {
	t.Run("a draft is numbered when it is published", func(t *testing.T) {
		e := turneventsWithHub(t, stream.Config{MaxEventBytes: 800})
		turn, _ := turneventsOpenTurn(t, e, 2)
		id := act.TurnID(turn.ID())
		progress := func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.ProgressEvent{TurnID: id, Seq: seq, Stage: conversation.ProgressEventStageInterpreting, State: conversation.ProgressEventStateCompleted}
		}
		tooBig := func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.TokenEvent{TurnID: id, Seq: seq, Text: strings.Repeat("a", 1200)}
		}
		done := func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.DoneEvent{TurnID: id, Seq: seq, FinishReason: conversation.DoneEventFinishReasonCompleted}
		}
		for i, step := range []struct {
			draft   turnEventsDraft
			refused bool
		}{{progress, false}, {tooBig, true}, {progress, false}, {tooBig, true}, {done, false}} {
			err := turnEventsPublishDraft(turn, step.draft, false)
			if (err != nil) != step.refused {
				t.Fatalf("step %d: err = %v, want refused = %v", i, err, step.refused)
			}
			if err != nil && stream.ReasonOf(err) != stream.ReasonEventRefused {
				t.Fatalf("step %d: reason = %q", i, stream.ReasonOf(err))
			}
		}
		// Three events were accepted, and they are 0, 1 and 2.
		sub, err := turn.Subscribe(stream.FromStart)
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		for want := int64(0); want < 3; want++ {
			f, err := sub.Next(context.Background())
			if err != nil || f.Seq != want {
				t.Fatalf("event %d = seq %d, %v: a refused event left a hole", want, f.Seq, err)
			}
		}
		if _, err := sub.Next(context.Background()); err != io.EOF {
			t.Fatalf("after the done event: %v", err)
		}
	})

	t.Run("a turn whose event is refused is failed at the next number", func(t *testing.T) {
		// The prepared action (about 1 KB) does not fit; the events before it
		// (the progress events and the directive) do.
		e := turneventsWithHub(t, stream.Config{MaxEventBytes: 800})
		e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		e.onResponse("Please confirm: create an item named widget.")
		rec := turneventsPostStream(e, TurnRequest{Text: sayCreate, Language: "en"})
		wantStatus(t, rec, http.StatusOK)
		sent := turneventsParse(t, rec.Body.String())
		raws := make([]json.RawMessage, len(sent.frames))
		for i, f := range sent.frames {
			raws[i] = json.RawMessage(f.Data)
		}
		// checkTurnEvents asserts seq runs from 0 without a gap, that one turn
		// id is throughout, that done is last and that a failed done follows an
		// error event.
		evs := checkTurnEvents(t, raws)
		types := eventTypes(evs)
		wantTypes := []string{"progress", "progress", "progress", "progress", "progress", "directive", "error", "done"}
		if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
			t.Fatalf("events = %v, want %v", types, wantTypes)
		}
		if findEvent(evs, "prepared_action") != nil || findEvent(evs, "done")["finish_reason"] != "failed" {
			t.Fatalf("a refused prepared action is in the stream or the turn did not fail: %v", types)
		}
		// The error event takes the number the refused event would have had.
		if seq := evs[6]["seq"]; seq != float64(6) {
			t.Errorf("the error event has seq %v, want 6", seq)
		}
		// The user would be confirming something the stream did not show: the
		// secret is not handed out, and the action stays unconfirmed.
		if tok := rec.Header().Get(HeaderConfirmationToken); tok != "" {
			t.Errorf("the confirmation secret left with a failed turn: %q", tok)
		}
		// The same turn as a document.
		e.model.Reset()
		e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		e.onResponse("Please confirm: create an item named widget.")
		doc := e.turn(TurnRequest{Text: sayCreate, Language: "en"})
		wantStatus(t, doc, http.StatusOK)
		body := decodeBody[TurnBody](t, doc)
		if body.Confirmation != nil || strings.Contains(doc.Body.String(), "cfm_") {
			t.Errorf("the confirmation secret left with a failed turn: %s", doc.Body.String())
		}
		if got := eventTypes(checkTurnEvents(t, rawEvents(t, body))); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
			t.Errorf("the document's events = %v, want %v", got, wantTypes)
		}
		// A resume says the same.
		got := turneventsParse(t, turneventsGet(e, body.TurnID, "", nil).Body.String())
		if len(got.frames) != len(wantTypes) || got.frames[len(got.frames)-1].Event != "done" {
			t.Errorf("a resume has %d frames", len(got.frames))
		}
		if e.host.Effects() != 0 || e.hostCalls(adapters.OpCommit) != 0 {
			t.Error("a refused turn committed something")
		}
	})
}

// ---- who may read a turn ----

// An unknown turn, another principal's turn and a turn past its retention are
// one and the same 403: a turn id is no oracle.
//
// SEC-025: another tenant's, workspace's or principal's turn stream is
// scope_violation, whatever the id.
func TestUnknownOtherPrincipalAndExpiredTurnsAreIdentical403(t *testing.T) {
	e := newAPIEnv(t)
	_, sent, id := turneventsPrepare(t, e, "Please confirm: create an item named widget.")

	// The owner reads it.
	rec := turneventsGet(e, id, "", nil)
	wantStatus(t, rec, http.StatusOK)
	turneventsSameFrames(t, "the owner", turneventsParse(t, rec.Body.String()).frames, sent.frames)

	// What a refusal says, without the request id (the one thing that differs
	// between two requests).
	type refusal struct {
		status       int
		contentType  string
		cacheControl string
		body         string
	}
	seen := func(rec *httptest.ResponseRecorder) refusal {
		t.Helper()
		wantError(t, rec, http.StatusForbidden, contracts.ErrScopeViolation)
		var env map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		details, _ := env["details"].(map[string]any)
		if details["request_id"] == "" || details["request_id"] == nil {
			t.Errorf("the refusal has no request id: %v", env)
		}
		delete(details, "request_id")
		b, _ := json.Marshal(env)
		return refusal{rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"), string(b)}
	}

	unknown := seen(turneventsGet(e, "trn_01J000000000000000000099ZZ", "", nil))
	for name, got := range map[string]refusal{
		"another principal":    seen(turneventsGet(e, id, "", nil, withPrincipal("usr_mallory"))),
		"another tenant":       seen(turneventsGet(e, id, "", nil, withTenant(apiOther))),
		"another workspace":    seen(turneventsGet(e, id, "", nil, withWorkspace(apiWsp2))),
		"a malformed turn id":  seen(turneventsGet(e, "trn%20bad", "", nil)),
		"an overlong turn id":  seen(turneventsGet(e, "trn_"+strings.Repeat("A", 300), "", nil)),
		"a cursor and a guess": seen(turneventsGet(e, "trn_01J000000000000000000098ZZ", "cursor=2", nil)),
	} {
		if got != unknown {
			t.Errorf("%s: %+v differs from an unknown turn: %+v", name, got, unknown)
		}
	}
	if !strings.Contains(unknown.body, `"turn_not_visible"`) {
		t.Errorf("the refusal = %s", unknown.body)
	}

	// Past its retention the turn is forgotten, and refused the same way.
	e.clk.Advance(turneventsHub(t, e).Config().Retention + time.Second)
	if got := seen(turneventsGet(e, id, "", nil)); got != unknown {
		t.Errorf("an expired turn: %+v differs from an unknown turn: %+v", got, unknown)
	}
	if st := turneventsHub(t, e).Stats(); st.Turns != 0 {
		t.Errorf("the hub still holds %d turns past their retention", st.Turns)
	}
}

// The checks that come before the turn: identity, the session of the path and
// the application's state, and the one route for both transports.
func TestTurnEventsRouteChecks(t *testing.T) {
	e := newAPIEnv(t)
	_, _, id := turneventsPrepare(t, e, "Please confirm: create an item named widget.")

	// No token, a malformed one, one that was already used: refused first.
	for name, tok := range map[string]string{"no token": "", "malformed": "x.y.z"} {
		wantUnauthorized(t, e.callAs(tok, http.MethodGet, turneventsPath(id), nil))
		_ = name
	}
	used := e.token()
	wantStatus(t, e.callAs(used, http.MethodGet, turneventsPath(id), nil), http.StatusOK)
	wantError(t, e.callAs(used, http.MethodGet, turneventsPath(id), nil), http.StatusUnauthorized, contracts.ErrReplayed)
	// A token in the query is not a credential.
	wantUnauthorized(t, e.callAs("", http.MethodGet, turneventsPath(id)+"?access_token="+e.token(), nil))

	// The session of the path is the token's.
	env := wantError(t, e.callAs(e.token(), http.MethodGet, sessionsPath("ses_other_01")+"/turns/"+id+"/events", nil), http.StatusForbidden, contracts.ErrScopeViolation)
	if levels, _ := env.Details["levels"].([]any); len(levels) != 1 || levels[0] != "session" {
		t.Errorf("details = %v", env.Details)
	}
	wantError(t, e.callAs(e.token(), http.MethodGet, "/v1/sessions/bad%20id/turns/"+id+"/events", nil), http.StatusBadRequest, contracts.ErrValidationFailed)

	// Only GET reads a stream.
	wantError(t, e.callAs(e.token(), http.MethodPost, turneventsPath(id), "{}"), http.StatusMethodNotAllowed, contracts.ErrValidationFailed)
	wantError(t, e.callAs(e.token(), http.MethodDelete, turneventsPath(id), nil), http.StatusMethodNotAllowed, contracts.ErrValidationFailed)

	// A quarantined application serves no traffic, a stream included.
	e.fapps.quarantine(true)
	wantError(t, turneventsGet(e, id, "", nil), http.StatusForbidden, contracts.ErrQuarantined)
	e.fapps.quarantine(false)
	wantStatus(t, turneventsGet(e, id, "", nil), http.StatusOK)

	// No CORS: nothing a browser could use from another origin.
	rec := turneventsGet(e, id, "", []string{"Origin", "https://evil.example"})
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("the stream sent %s", k)
		}
	}
}

// ---- the transports over a real connection ----

// The same route is a WebSocket for a client that asks to upgrade: one text
// message per event, the same bytes as the stream, a normal closure after done,
// a resume from the cursor, and an Origin that is not the request's own host is
// refused.
func TestTurnEventsOverWebSocket(t *testing.T) {
	e := newAPIEnv(t)
	_, sent, id := turneventsPrepare(t, e, "Please confirm: create an item named widget.")
	hs := httptest.NewServer(e.srv.Handler()) // plain-HTTP loopback
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dial := func(query string, origin string) (*websocket.Conn, *http.Response, error) {
		u := "ws" + strings.TrimPrefix(hs.URL, "http") + turneventsPath(id)
		if query != "" {
			u += "?" + query
		}
		hdr := http.Header{"Authorization": {"Bearer " + e.token()}}
		if origin != "" {
			hdr.Set("Origin", origin)
		}
		return websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr, Subprotocols: []string{stream.WebSocketSubprotocol}})
	}
	read := func(c *websocket.Conn, from int) {
		t.Helper()
		for i := from; i < len(sent.frames); i++ {
			typ, data, err := c.Read(ctx)
			if err != nil || typ != websocket.MessageText {
				t.Fatalf("message %d: %v (type %v)", i, err, typ)
			}
			if string(data) != sent.frames[i].Data {
				t.Fatalf("message %d = %s, want the frame %s", i, data, sent.frames[i].Data)
			}
		}
		if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Fatalf("after done the connection ends with %v, want a normal closure", err)
		}
	}

	c, _, err := dial("", "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if c.Subprotocol() != stream.WebSocketSubprotocol {
		t.Errorf("subprotocol = %q", c.Subprotocol())
	}
	read(c, 0)
	c.CloseNow()

	c, _, err = dial("cursor=4", "")
	if err != nil {
		t.Fatalf("dial with a cursor: %v", err)
	}
	read(c, 5)
	c.CloseNow()

	// A browser on another origin is refused before the upgrade (the gateway
	// configures no origin besides its own host).
	if _, resp, err := dial("", "https://evil.example.test"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Origin = %v, %v; want a 403", resp, err)
	}
	// And a refusal before the upgrade is an HTTP status, the typed envelope.
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + turneventsPath("trn_01J000000000000000000099ZZ")
	_, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + e.token()}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a WebSocket for an unknown turn = %v, %v; want a 403", resp, err)
	}
}

// A stream that is still being produced reaches the client live and outlives the
// server.s read and write timeouts (sized for one request, not for a stream): the
// hub's handler extends the write deadline of every frame it sends through the
// gateway's response wrapper, and the read timeout does not cut a request that
// was read in full long ago.
func TestTurnEventsLiveStreamOutlivesTheServerTimeouts(t *testing.T) {
	e := newAPIEnv(t)
	turn, id := turneventsOpenTurn(t, e, 3)
	turneventsPublishToken(t, turn, "first ")

	hs := httptest.NewUnstartedServer(e.srv.Handler())
	hs.Config.ReadTimeout = 300 * time.Millisecond
	hs.Config.WriteTimeout = 300 * time.Millisecond
	hs.Start()
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+turneventsPath(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token())
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(resp.Body)
		got <- result{string(b), err}
	}()
	time.Sleep(900 * time.Millisecond) // three timeouts
	turneventsPublishToken(t, turn, "second ")
	turneventsPublishDone(t, turn)
	r := <-got
	if r.err != nil {
		t.Fatalf("the stream was cut: %v", r.err)
	}
	frames := turneventsParse(t, r.body).frames
	if len(frames) != 3 || frames[2].Event != "done" || frames[1].Event != "token" {
		t.Fatalf("the live stream = %q", r.body)
	}
}

// PublishTrusted must follow the CoreMaterialised provenance flag from the
// analyst materialiser — never a model-writable dataset_id member.
func TestCoreMaterialisedFlagNotDatasetID(t *testing.T) {
	e := turneventsWithHub(t, stream.Config{})
	turn, id := turneventsOpenTurn(t, e, 77)
	tid := act.TurnID(id)
	ds := analyst.DatasetID("ds_core_001")
	table := conversation.RichBlock{Value: conversation.TableBlock{
		Type: "table", DatasetID: &ds,
		Columns: []conversation.TableColumn{{Key: "company", Label: "Company"}, {Key: "revenue", Label: "Revenue"}},
		Rows:    [][]conversation.TableCell{{"Acme", float64(12400)}},
	}}
	chart := conversation.RichBlock{Value: conversation.ChartBlock{
		Type: "chart", Description: conversation.PlainText("Revenue."), Mark: conversation.ChartBlockMarkBar,
		DatasetID: &ds,
		Data:      []conversation.ChartRecord{{"company": "Acme", "revenue": float64(12400)}},
		Encoding: conversation.ChartEncoding{
			X: &conversation.ChartChannel{Field: common.FieldName("company"), Type: conversation.ChartChannelTypeNominal},
			Y: &conversation.ChartChannel{Field: common.FieldName("revenue"), Type: conversation.ChartChannelTypeQuantitative},
		},
	}}

	t.Run("dataset_id without flag uses fact-checked Publish", func(t *testing.T) {
		draft := func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.BlockEvent{TurnID: tid, Seq: seq, Block: table}
		}
		if err := turnEventsPublishDraft(turn, draft, false); err == nil {
			t.Fatal("table with dataset_id but no CoreMaterialised flag must be fact-checked and refused")
		}
	})
	t.Run("analyst-materialised table and chart pass PublishTrusted", func(t *testing.T) {
		for name, b := range map[string]conversation.RichBlock{"table": table, "chart": chart} {
			blk := b
			draft := func(seq conversation.Seq) conversation.TurnEventValue {
				return conversation.BlockEvent{TurnID: tid, Seq: seq, Block: blk}
			}
			if err := turnEventsPublishDraft(turn, draft, true); err != nil {
				t.Fatalf("%s core-materialised refused: %v", name, err)
			}
		}
	})
	t.Run("buildTurnEvents trusts only the CoreMaterialised flag", func(t *testing.T) {
		empty := conversation.DialogueDirective{
			TurnID: tid, Act: conversation.DialogueDirectiveActPresentResult,
			Facts: []act.Fact{}, MustState: []common.FieldName{}, MissingFields: []common.FieldName{},
			InvalidFields: []act.Issue{}, AllowedSuggestions: []act.Choice{},
			Confirmation:         act.ConfirmationRequirement{Mode: act.ConfirmationRequirementModeNone},
			PermittedNextActions: []act.PermittedNextAction{}, Sources: []conversation.Citation{},
		}
		plan := buildTurnEvents(dialogue.Result{
			TurnID: id, Directive: empty,
			Response: verify.Outcome{Text: "ok", Blocks: []conversation.RichBlock{table}},
		}, nil)
		var trusted []bool
		for _, item := range plan.leading {
			if _, ok := item.draft(0).(conversation.BlockEvent); ok {
				trusted = append(trusted, item.trusted)
			}
		}
		if len(trusted) != 1 || trusted[0] {
			t.Fatalf("without CoreMaterialised flag, trusted=%v", trusted)
		}
		plan2 := buildTurnEvents(dialogue.Result{
			TurnID: id, Directive: empty,
			Response: verify.Outcome{
				Text: "ok", Blocks: []conversation.RichBlock{table, chart},
				CoreMaterialised: []bool{true, true},
			},
		}, nil)
		trusted = nil
		for _, item := range plan2.leading {
			if _, ok := item.draft(0).(conversation.BlockEvent); ok {
				trusted = append(trusted, item.trusted)
			}
		}
		if len(trusted) != 2 || !trusted[0] || !trusted[1] {
			t.Fatalf("with CoreMaterialised flags, trusted=%v", trusted)
		}
	})
}
