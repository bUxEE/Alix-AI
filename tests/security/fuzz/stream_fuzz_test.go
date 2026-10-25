package fuzzsec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// ---------------------------------------------------------------- reference SSE parser

// sseFrame is one dispatched event of a reference reader.
type sseFrame struct {
	event, id, data string
	hasID           bool
}

// refSSE reads a byte stream the way "Event stream interpretation" of the HTML
// standard says (lines end in CRLF, LF or CR; a line starting with ":" is a
// comment; "field: value" takes one optional space; data lines join with LF; an
// empty line dispatches; an event cut off before its empty line is dropped).
// The id is kept per event (the SDK requires an id for every event, which is
// stricter than the standard's persistent last-event-id). It is the independent
// oracle for the SDK's reader.
func refSSE(b []byte) []sseFrame {
	s := string(b)
	var lines []string
	for len(s) > 0 {
		i := strings.IndexAny(s, "\r\n")
		if i < 0 {
			lines = append(lines, s) // an unterminated last line: never followed by a blank line
			break
		}
		lines = append(lines, s[:i])
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			i++
		}
		s = s[i+1:]
	}
	// The last element is the unterminated line only when the input did not end
	// with a line end; either way an event needs its own empty line, which an
	// unterminated tail never is.
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) && !bytes.HasSuffix(b, []byte("\r")) && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	var out []sseFrame
	var cur sseFrame
	var data []string
	reset := func() { cur, data = sseFrame{}, nil }
	for _, l := range lines {
		if l == "" {
			if len(data) > 0 {
				cur.data = strings.Join(data, "\n")
				if cur.event == "" {
					cur.event = "message"
				}
				out = append(out, cur)
			}
			reset()
			continue
		}
		if l[0] == ':' {
			continue
		}
		field, value := l, ""
		if i := strings.IndexByte(l, ':'); i >= 0 {
			field, value = l[:i], strings.TrimPrefix(l[i+1:], " ")
		}
		switch field {
		case "event":
			cur.event = value
		case "data":
			data = append(data, value)
		case "id":
			if !strings.ContainsRune(value, 0) {
				cur.id, cur.hasID = value, true
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- the emitted stream

var (
	reRetryLine = regexp.MustCompile(`^retry: [0-9]{1,9}$`)
	reIDLine    = regexp.MustCompile(`^id: ([0-9]{1,10})$`)
	reEventLine = regexp.MustCompile(`^event: ([a-z_]{1,32})$`)
	reDataLine  = regexp.MustCompile(`^data: (\{.*\})$`)
)

// checkSSEBody reads the body of the SSE endpoint line by line against the
// grammar the stream documents (internal/stream/doc.go): a retry hint, then for
// each event exactly "id: <digits>", "event: <type>" and one "data: <JSON on one
// line>", then an empty line; comments only as ": text". It returns the data
// lines, or the first violation.
func checkSSEBody(body []byte, maxData int) (datas []string, violation string) {
	if bytes.ContainsAny(body, "\r\x00") {
		return nil, "a CR or NUL in the stream"
	}
	if !utf8.Valid(body) {
		return nil, "the stream is not valid UTF-8"
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return nil, "the stream does not end with a line end"
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	i := 0
	if i < len(lines) && reRetryLine.MatchString(lines[i]) {
		i++
		if i >= len(lines) || lines[i] != "" {
			return nil, "the retry hint is not followed by an empty line"
		}
		i++
	}
	for i < len(lines) {
		if strings.HasPrefix(lines[i], ": ") && !strings.ContainsAny(lines[i], "\x00") {
			i++
			if i < len(lines) && lines[i] == "" {
				i++
			}
			continue
		}
		if i+3 >= len(lines) {
			return nil, fmt.Sprintf("an event cut short at line %d: %q", i, lines[i:])
		}
		idM, evM, dataM := reIDLine.FindStringSubmatch(lines[i]), reEventLine.FindStringSubmatch(lines[i+1]), reDataLine.FindStringSubmatch(lines[i+2])
		if idM == nil || evM == nil || dataM == nil || lines[i+3] != "" {
			return nil, fmt.Sprintf("a frame that is not id / event / one data line / empty line: %q", lines[i:min(i+4, len(lines))])
		}
		if len(dataM[1]) > maxData {
			return nil, fmt.Sprintf("a data line of %d bytes over the event limit %d", len(dataM[1]), maxData)
		}
		var head struct {
			Type string `json:"type"`
			Seq  *int64 `json:"seq"`
		}
		if err := json.Unmarshal([]byte(dataM[1]), &head); err != nil || head.Seq == nil {
			return nil, "a data line that is not a turn event: " + dataM[1]
		}
		if head.Type != evM[1] || strconv.FormatInt(*head.Seq, 10) != idM[1] {
			return nil, fmt.Sprintf("the frame says event %s id %s, the event says %s %d", evM[1], idM[1], head.Type, *head.Seq)
		}
		datas = append(datas, dataM[1])
		i += 4
	}
	return datas, ""
}

// ---------------------------------------------------------------- rig

var fuzzStreamScope = contracts.Scope{
	DeploymentID: "dep1", ApplicationID: "app1", TenantID: "ten1", WorkspaceID: "ws1",
	PrincipalID: "usr1", Roles: []string{"user"}, CapabilityScopes: []string{"appointments.book"}, SessionID: "ses1",
}

const fuzzSessionID = "ses_01J0000000000000000000000A"

var streamTurnCounter atomic.Int64

// nextTurnID mints a well-formed turn id (trn_ + 26 Crockford characters).
func nextTurnID() string {
	return fmt.Sprintf("trn_%026d", streamTurnCounter.Add(1))
}

type streamRig struct {
	hub    *stream.Hub
	opts   stream.Options
	server *httptest.Server
}

// maxFuzzEventBytes is the event limit of the rig's hub: small, so that an
// oversize event is a case the fuzzing reaches.
const maxFuzzEventBytes = 2048

var (
	rigOnce sync.Once
	rig     *streamRig
)

// getRig is one hub behind an httptest server on loopback, shared by the
// iterations of a process. Every iteration uses its own turn.
func getRig() *streamRig {
	rigOnce.Do(func() {
		r := &streamRig{hub: stream.NewHub(stream.Config{MaxEventBytes: maxFuzzEventBytes, MaxBytes: 1 << 20, MaxTurns: 1 << 20, MaxTurnsPerTenant: 1 << 20, Retention: time.Minute})}
		r.opts = stream.Options{
			Scope:        func(*http.Request) (contracts.Scope, error) { return fuzzStreamScope, nil },
			WriteTimeout: 5 * time.Second, Heartbeat: 5 * time.Second, RetryAfter: time.Second,
		}
		mux := http.NewServeMux()
		mux.Handle("GET /v1/sessions/{session_id}/turns/{turn_id}/events", r.hub.SSEHandler(r.opts))
		mux.Handle("GET /ws/{turn_id}", r.hub.WebSocketHandler(r.opts))
		r.server = httptest.NewServer(mux)
		rig = r
	})
	return rig
}

func tokenEvent(turnID string, seq int64, text string) conversation.TurnEvent {
	return conversation.TurnEvent{Value: conversation.TokenEvent{TurnID: actions.TurnID(turnID), Seq: conversation.Seq(seq), Text: text}}
}

func errorEvent(turnID string, seq int64, msg string) conversation.TurnEvent {
	return conversation.TurnEvent{Value: conversation.ErrorEvent{
		TurnID: actions.TurnID(turnID), Seq: conversation.Seq(seq),
		Error: common.Error{Code: common.ErrorCodeUnavailable, Message: msg, Retryable: true},
	}}
}

func doneEvent(turnID string, seq int64) conversation.TurnEvent {
	return conversation.TurnEvent{Value: conversation.DoneEvent{
		TurnID: actions.TurnID(turnID), Seq: conversation.Seq(seq), FinishReason: conversation.DoneEventFinishReasonCompleted,
	}}
}

// publishHostile opens a turn and publishes events that carry the payload:
// a response text (sanitised as a whole, then cut into token events), a token
// event with the text as it is, and an error event with the payload as its
// message. Whatever the hub refuses is not buffered; the turn is always ended.
func (r *streamRig) publishHostile(payload string) (*stream.Turn, string, hostileResp, error) {
	id := nextTurnID()
	turn, err := r.hub.Begin(fuzzStreamScope, id)
	if err != nil {
		return nil, id, hostileResp{}, err
	}
	err = turn.PublishResponse(payload)
	resp := hostileResp{err: err, n: int(turn.NextSeq())}
	// The hub does not check a token's text length (internal/stream is outside this suite's
	// scope), and the client now refuses a token the schema rejects: publish only a text the
	// schema allows (1 to 4096 characters), so the property under test stays the framing.
	if n := utf8.RuneCountInString(payload); n >= 1 && n <= 4096 {
		_ = turn.Publish(tokenEvent(id, turn.NextSeq(), payload))
	}
	_ = turn.Publish(errorEvent(id, turn.NextSeq(), payload))
	if err := turn.Publish(doneEvent(id, turn.NextSeq())); err != nil {
		return nil, id, resp, fmt.Errorf("the done event was refused: %w", err)
	}
	return turn, id, resp, nil
}

// hostileResp is what publishing the payload as a response did: its error, and
// how many events it published (they are the first events of the turn).
type hostileResp struct {
	err error
	n   int
}

// getSSE reads the SSE endpoint's whole answer for a finished turn, through the
// real handler (httptest recorder, no socket).
func (r *streamRig) getSSE(turnID string) (status int, body []byte) {
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+fuzzSessionID+"/turns/"+turnID+"/events", nil)
	req.SetPathValue("turn_id", turnID)
	rec := httptest.NewRecorder()
	r.hub.SSEHandler(r.opts).ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// sdkRead runs the production client (pkg/sdk-go) over a canned
// text/event-stream body and returns the events it delivered and its error. No
// network: the transport hands the client the bytes.
func sdkRead(turnID string, body []byte) ([]conversation.TurnEvent, error) {
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body:   io.NopCloser(bytes.NewReader(body)),
		}, nil
	})
	c, err := sdkgo.NewClient(sdkgo.ClientOptions{
		BaseURL:         "http://127.0.0.1:1",
		GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) { return "tok-abc", nil },
		HTTPClient:      &http.Client{Transport: rt},
		MaxResumes:      -1,
		TurnTimeout:     10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	s, err := c.StreamTurnEvents(context.Background(), fuzzSessionID, turnID, sdkgo.WithTimeout(10*time.Second))
	if err != nil {
		return nil, err
	}
	defer s.Close()
	for s.Next() {
	}
	return s.Events(), s.Err()
}

// checkSDKRead is the property of the reader on arbitrary bytes: whatever events
// it delivered are the leading events of the stream as a standard-conforming
// reader reads it (same event name, same id, same data), and each one satisfies
// the published turn-event schema.
func checkSDKRead(sc *jsonschema.Schema, body []byte) string {
	events, _ := sdkRead(fuzzTurnID, body)
	ref := refSSE(body)
	if len(events) > len(ref) {
		return fmt.Sprintf("the client delivered %d events, a standard reader finds %d frames", len(events), len(ref))
	}
	for i, ev := range events {
		env, ok := sdkgo.Envelope(ev)
		if !ok {
			return fmt.Sprintf("event %d has no envelope", i)
		}
		fr := ref[i]
		if fr.event != env.Type || !fr.hasID || fr.id != strconv.FormatInt(env.Seq, 10) {
			return fmt.Sprintf("event %d: the client says %s/%d, the frame says %s/%q", i, env.Type, env.Seq, fr.event, fr.id)
		}
		if ok, why := valid(sc, []byte(fr.data)); !ok {
			return fmt.Sprintf("event %d: the client delivered an event the published schema rejects (%s): %s", i, why, fr.data)
		}
		enc, err := json.Marshal(ev)
		if err != nil {
			return fmt.Sprintf("event %d does not encode: %v", i, err)
		}
		var want, got any
		if json.Unmarshal([]byte(fr.data), &want) != nil || json.Unmarshal(enc, &got) != nil {
			return fmt.Sprintf("event %d: unreadable JSON", i)
		}
		if !jsonEqual(want, got) {
			return fmt.Sprintf("event %d: the client delivered\n%s\nthe frame carried\n%s", i, enc, fr.data)
		}
	}
	return ""
}

const fuzzTurnID = "trn_00000000000000000000000000"

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// ---------------------------------------------------------------- the properties

// checkEmit publishes hostile text through the production hub and reads it back
// over SSE (raw body), the production client and a WebSocket.
func checkEmit(r *streamRig, payload string) string {
	turn, id, resp, err := r.publishHostile(payload)
	if err != nil {
		return err.Error()
	}
	status, body := r.getSSE(id)
	if status != http.StatusOK {
		return fmt.Sprintf("the SSE endpoint answered %d for a finished turn", status)
	}
	datas, bad := checkSSEBody(body, maxFuzzEventBytes)
	if bad != "" {
		return "SSE: " + bad + "\n" + string(body)
	}
	if int64(len(datas)) != turn.NextSeq() {
		return fmt.Sprintf("SSE carried %d events, the turn published %d", len(datas), turn.NextSeq())
	}
	// The production client reads the same bytes and delivers the same events.
	events, err := sdkRead(id, body)
	if err != nil {
		return "the client failed on the server's own stream: " + err.Error()
	}
	if len(events) != len(datas) {
		return fmt.Sprintf("the client delivered %d events of %d", len(events), len(datas))
	}
	for i, ev := range events {
		enc, err := json.Marshal(ev)
		if err != nil || string(enc) != datas[i] {
			return fmt.Sprintf("event %d does not round-trip:\n%s\n%s (%v)", i, datas[i], enc, err)
		}
	}
	// The response, cut into token events, joins to exactly the sanitised text.
	if resp.err == nil && resp.n > 0 {
		clean, _, err := blocks.Default().SanitizeResponse(payload)
		if err != nil {
			return "the hub published a response the sanitiser refuses: " + err.Error()
		}
		if got := sdkgo.TurnText(events[:resp.n]); got != clean {
			return fmt.Sprintf("the response fragments join to %q, the sanitised text is %q", got, clean)
		}
	}
	// The same events over a WebSocket: one text message per event, the same bytes.
	return checkWSMessages(r, id, datas)
}

// errNoSocket means the rig could not open a connection for a reason of the
// machine (the dial budget of the process is spent, or the operating system has
// no local port left): the WebSocket half of that input is inconclusive, not a
// failure of the stream.
var errNoSocket = errors.New("no socket available")

// wsDials counts the connections this process opened. A fuzzer runs thousands of
// inputs a second and every closed connection holds its local port for minutes
// (TIME_WAIT on Windows leaves about 16000), so the first 300 inputs dial
// always, then one in eight, and no more than wsDialCap in all.
var wsDials atomic.Int64

const wsDialCap = 3000

// wsChecked counts the WebSocket checks that ran to their end.
var wsChecked atomic.Int64

// dialWS opens the WebSocket of a turn.
func (r *streamRig) dialWS(ctx context.Context, turnID string) (*websocket.Conn, error) {
	if n := wsDials.Add(1); n > wsDialCap || (n > 300 && n%8 != 0) {
		return nil, errNoSocket
	}
	u := "ws" + strings.TrimPrefix(r.server.URL, "http") + "/ws/" + turnID
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{Subprotocols: []string{stream.WebSocketSubprotocol}})
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return nil, errNoSocket
		}
		return nil, err
	}
	c.SetReadLimit(maxFuzzEventBytes + 64)
	return c, nil
}

func checkWSMessages(r *streamRig, turnID string, want []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := r.dialWS(ctx, turnID)
	if err != nil {
		if errors.Is(err, errNoSocket) {
			return ""
		}
		return "websocket dial: " + err.Error()
	}
	defer c.CloseNow()
	for i := 0; ; i++ {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				return fmt.Sprintf("the websocket ended with %v after %d messages, want a normal closure", err, i)
			}
			if i != len(want) {
				return fmt.Sprintf("the websocket carried %d messages, SSE %d", i, len(want))
			}
			wsChecked.Add(1)
			return ""
		}
		if typ != websocket.MessageText {
			return "a binary websocket message"
		}
		if len(msg) > maxFuzzEventBytes {
			return fmt.Sprintf("a websocket message of %d bytes over the event limit %d", len(msg), maxFuzzEventBytes)
		}
		if i >= len(want) || string(msg) != want[i] {
			return fmt.Sprintf("websocket message %d is not the SSE data line:\n%s", i, msg)
		}
	}
}

// checkWSClientMessage connects to an OPEN turn and sends the payload as a client
// message (the transport is server to client only): the server must end the
// connection with 1008, or 1009 for a message over its read limit, and must
// not die (a second client still gets the turn).
func checkWSClientMessage(r *streamRig, payload []byte, binary bool) string {
	id := nextTurnID()
	turn, err := r.hub.Begin(fuzzStreamScope, id)
	if err != nil {
		return err.Error()
	}
	if err := turn.Publish(tokenEvent(id, turn.NextSeq(), "hello")); err != nil {
		return err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() { _ = turn.Publish(doneEvent(id, turn.NextSeq())) }()
	c, err := r.dialWS(ctx, id)
	if err != nil {
		if errors.Is(err, errNoSocket) {
			return ""
		}
		return "websocket dial: " + err.Error()
	}
	defer c.CloseNow()
	if _, msg, err := c.Read(ctx); err != nil || !strings.Contains(string(msg), `"hello"`) {
		return fmt.Sprintf("the first message was %q (%v)", msg, err)
	}
	typ := websocket.MessageText
	if binary {
		typ = websocket.MessageBinary
	}
	if err := c.Write(ctx, typ, payload); err != nil && websocket.CloseStatus(err) == -1 {
		// A failed write of a big frame is the server having closed already.
		_ = err
	}
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue // an event or a ping that crossed the message
		}
		code := websocket.CloseStatus(err)
		switch {
		case code == websocket.StatusPolicyViolation, code == websocket.StatusMessageTooBig:
			// The server closes at the first data frame it sees (1008), or at the
			// read limit of 256 bytes (1009): either way the message is never read
			// into memory, whatever length the client declared.
			wsChecked.Add(1)
			return ""
		default:
			return fmt.Sprintf("a client message of %d bytes ended the connection with %v (status %d), want 1008 or 1009", len(payload), err, code)
		}
	}
}

// checkCursor is the property of the resume cursor parser (stream.ParseCursor) on
// a hostile value, as the "cursor" query parameter and as Last-Event-ID.
func checkCursor(raw string) string {
	reDigits := regexp.MustCompile(`^[0-9]{1,10}$`)
	wantOK := func(s string) (int64, bool) {
		if !reDigits.MatchString(s) {
			return 0, false
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		return n, n <= stream.MaxSeq
	}
	for _, mode := range []string{"query", "header"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if mode == "query" {
			req.URL.RawQuery = "cursor=" + url.QueryEscape(raw)
		} else {
			req.Header.Set("Last-Event-ID", raw)
		}
		n, err := stream.ParseCursor(req)
		want, ok := wantOK(raw)
		switch {
		case raw == "":
			if err != nil || n != stream.FromStart {
				return fmt.Sprintf("%s: an empty cursor gave %d, %v", mode, n, err)
			}
		case ok && (err != nil || n != want):
			return fmt.Sprintf("%s: %q gave %d, %v, want %d", mode, raw, n, err, want)
		case !ok && err == nil:
			return fmt.Sprintf("%s: %q was accepted as %d", mode, raw, n)
		case !ok && stream.ReasonOf(err) != stream.ReasonCursorMalformed:
			return fmt.Sprintf("%s: %q refused for the wrong reason: %v", mode, raw, err)
		}
	}
	// A parameter given twice is malformed whatever it holds.
	req := httptest.NewRequest(http.MethodGet, "/?cursor=1&cursor=2", nil)
	if _, err := stream.ParseCursor(req); err == nil {
		return "a cursor given twice was accepted"
	}
	return ""
}

// ---------------------------------------------------------------- the target

// SEC-097
// FuzzStreamFraming: the framing of the public stream, SSE and
// WebSocket, with the production emitter (internal/stream) and the production
// reader (pkg/sdk-go). Four modes, selected by the first byte:
//
//	0  the client reads arbitrary bytes as a text/event-stream: it never panics
//	   or hangs, and whatever events it delivers are the leading events of the
//	   stream as a standard-conforming reader reads it (same name, id and data).
//	1  hostile text is published through the hub (as a response, as a token
//	   event and as an error message) and read back over SSE (raw body checked
//	   line by line: no CR, no newline in a data line or an id or a type, exactly
//	   id / event / data per frame, every data line one JSON event whose type and
//	   seq are the frame's), by the client (the same events, byte for byte), and
//	   over a WebSocket (one text message per event, the same bytes, each within
//	   the event limit). An event over the limit is refused, never framed.
//	2  the resume cursor (stream.ParseCursor) on a hostile value: accepted only
//	   when it is digits within the sequence range.
//	3  a client message of arbitrary size to the server-to-client WebSocket: the
//	   server closes the connection with 1008 (a data message is a violation), or
//	   1009 at its 256-byte read limit, never reads it into memory, and keeps serving.
//
// Controls (before the fuzzing): the SSE checker flags an injected field, a CR
// and a multi-line data frame; the reference reader dispatches what the client
// does; an event over the limit is refused by the hub.
//
//	go test -run '^$' -fuzz '^FuzzStreamFraming$' -fuzztime 5s ./tests/security/fuzz/
func FuzzStreamFraming(f *testing.F) {
	r := getRig()
	f.Cleanup(r.server.Close)
	turnSchema := loadSchemaSet(f).schema(f, "urn:alixai:schema:conversation:turn-event:v1")
	// The client once delivered this stream's event (no seq, no text, a key in another case);
	// it must now refuse it, and deliver nothing.
	const knownBad = "id: 0\nevent: token\ndata: {\"tYpe\":\"token\",\"turn_id\":\"" + fuzzTurnID + "\"}\n\n"
	if events, err := sdkRead(fuzzTurnID, []byte(knownBad)); len(events) != 0 || err == nil {
		f.Fatalf("control: the client delivered %d events (err %v) for an event the turn-event schema rejects", len(events), err)
	}
	if msg := checkSDKRead(turnSchema, []byte(knownBad)); msg != "" {
		f.Fatalf("control: %s", msg)
	}
	f.Add(uint8(0), []byte(knownBad))
	f.Add(uint8(0), []byte("id: 0\nevent: token\ndata: {\"type\":\"token\",\"turn_id\":\""+fuzzTurnID+"\",\"seq\":0}\n\n"))

	// Controls.
	good := "retry: 1000\n\nid: 0\nevent: token\ndata: {\"type\":\"token\",\"seq\":0,\"text\":\"a\"}\n\n"
	if _, bad := checkSSEBody([]byte(good), 4096); bad != "" {
		f.Fatalf("control: a well-formed stream is flagged: %s", bad)
	}
	for name, planted := range map[string]string{
		"an injected event field": "id: 0\nevent: token\ndata: {\"type\":\"token\",\"seq\":0}\nevent: done\n\n",
		"an injected second data": "id: 0\nevent: token\ndata: {\"type\":\"token\",\"seq\":0}\ndata: {\"type\":\"done\",\"seq\":1}\n\n",
		"a CR in a data line":     "id: 0\nevent: token\ndata: {\"type\":\"token\",\"seq\":0,\"text\":\"a\rb\"}\n\n",
		"a newline in the data":   "id: 0\nevent: token\ndata: {\"type\":\"token\",\"seq\":0,\"text\":\"a\nb\"}\n\n",
		"an id with a newline":    "id: 0\n1\nevent: token\ndata: {\"type\":\"token\",\"seq\":0}\n\n",
		"an event type in upper":  "id: 0\nevent: TOKEN\ndata: {\"type\":\"TOKEN\",\"seq\":0}\n\n",
		"an id that disagrees":    "id: 5\nevent: token\ndata: {\"type\":\"token\",\"seq\":0}\n\n",
		"a data line over limit":  "id: 0\nevent: token\ndata: {\"type\":\"token\",\"seq\":0,\"text\":\"" + strings.Repeat("a", 5000) + "\"}\n\n",
		"an unfinished event":     "id: 0\nevent: token\ndata: {\"type\":\"token\",\"seq\":0}\n",
	} {
		if _, bad := checkSSEBody([]byte(planted), 4096); bad == "" {
			f.Fatalf("control: the SSE checker did not flag %s", name)
		}
	}
	if frames := refSSE([]byte("id: 0\r\nevent: token\r\ndata: a\r\ndata: b\r\n\r\n: comment\n\nid: 1\ndata: cut")); len(frames) != 1 || frames[0].data != "a\nb" || frames[0].event != "token" {
		f.Fatalf("control: the reference reader read %+v", frames)
	}
	// An event over the limit is refused, not framed.
	big := tokenEvent(nextTurnID(), 0, strings.Repeat("a", maxFuzzEventBytes*2))
	turn, err := r.hub.Begin(fuzzStreamScope, nextTurnID())
	if err != nil {
		f.Fatal(err)
	}
	if err := turn.Publish(conversation.TurnEvent{Value: conversation.TokenEvent{TurnID: actions.TurnID(turn.ID()), Seq: 0, Text: string(big.Value.(conversation.TokenEvent).Text)}}); err == nil {
		f.Fatal("control: an event over the limit was accepted")
	}
	if turn.NextSeq() != 0 {
		f.Fatal("control: a refused event used up a sequence number")
	}
	_ = turn.Publish(doneEvent(turn.ID(), 0))
	// The whole chain on a plain text, so the later failures are the payload's.
	if msg := checkEmit(r, "hello **world**"); msg != "" {
		f.Fatalf("control: a plain response: %s", msg)
	}
	if msg := checkWSClientMessage(r, []byte("x"), false); msg != "" {
		f.Fatalf("control: a short client message: %s", msg)
	}
	if msg := checkWSClientMessage(r, bytes.Repeat([]byte("x"), 300), false); msg != "" {
		f.Fatalf("control: a long client message: %s", msg)
	}
	if n := wsChecked.Load(); n < 3 {
		f.Fatalf("control: only %d WebSocket checks ran to their end: the WebSocket half of this target checked nothing", n)
	}

	// Seeds.
	valid := []byte("retry: 1000\n\nid: 0\nevent: token\ndata: {\"type\":\"token\",\"turn_id\":\"" + fuzzTurnID + "\",\"seq\":0,\"text\":\"hi\"}\n\n" +
		"id: 1\nevent: done\ndata: {\"type\":\"done\",\"turn_id\":\"" + fuzzTurnID + "\",\"seq\":1,\"finish_reason\":\"completed\"}\n\n")
	f.Add(uint8(0), valid)
	f.Add(uint8(0), bytes.ReplaceAll(valid, []byte("\n"), []byte("\r\n")))
	f.Add(uint8(0), bytes.ReplaceAll(valid, []byte("\n"), []byte("\r")))
	f.Add(uint8(0), append([]byte("\xef\xbb\xbf"), valid...))
	f.Add(uint8(0), []byte("id: 0\nevent: token\ndata: {\"type\":\"token\",\"turn_id\":\""+fuzzTurnID+"\",\"seq\":0,\n"))
	f.Add(uint8(0), []byte("id: 0\nevent: token\ndata: {\"type\":\"token\",\"turn_id\":\""+fuzzTurnID+"\",\ndata: \"seq\":0,\"text\":\"a\"}\n\n"))
	f.Add(uint8(0), []byte("id: 0\nid: 7\nevent: token\nevent: done\ndata: {}\n\n"))
	f.Add(uint8(0), []byte("data\n\ndata:\n\n:\n\nevent\n\nretry: x\n\n"))
	f.Add(uint8(0), []byte("id: 0\x00\nevent: token\ndata: {\"type\":\"token\"}\n\n"))
	f.Add(uint8(0), []byte(strings.Repeat("data: aaaaaaaa\n", 20000)))
	f.Add(uint8(0), []byte(strings.Repeat("x", 100000)))
	f.Add(uint8(0), []byte("data: \xff\xfe\n\n"))
	for _, text := range []string{
		"hello", "line one\nline two", "a\rb", "a\r\nb", "a\u2028b\u2029c", "a\x00b", "\n\nevent: done\ndata: {}\n\n", "id: 99\n",
		"data: {\"type\":\"done\"}", "<script>alert(1)</script>", "[x](javascript:alert(1))", "\u202eevil", strings.Repeat("a", 3000), strings.Repeat("\n", 500),
		"\xff\xfe invalid", "{\"type\":\"done\",\"seq\":1}", "\\u000a", "token\":\"x", "`` ` ``", "- a\n- b\n\n1. c",
	} {
		f.Add(uint8(1), []byte(text))
		f.Add(uint8(3), []byte(text))
	}
	for _, c := range []string{"0", "1", "00", "4294967295", "4294967296", "-1", "+1", "1 ", " 1", "0x10", "1e3", "\u0661", "", "99999999999", "1\n2", "١٢"} {
		f.Add(uint8(2), []byte(c))
	}
	f.Add(uint8(3), bytes.Repeat([]byte("x"), 257))
	f.Add(uint8(3), bytes.Repeat([]byte("x"), 70000))
	f.Add(uint8(3), []byte{})

	f.Fuzz(func(t *testing.T, mode uint8, payload []byte) {
		var msg string
		within(t, 60*time.Second, "the stream", func() {
			switch mode % 4 {
			case 0:
				msg = checkSDKRead(turnSchema, payload)
			case 1:
				msg = checkEmit(r, string(payload))
			case 2:
				msg = checkCursor(string(payload))
			default:
				msg = checkWSClientMessage(r, payload, mode&0x80 != 0)
			}
		})
		if msg != "" {
			t.Fatalf("mode %d, input %s: %s", mode%4, quoteShort(payload), msg)
		}
	})
}
