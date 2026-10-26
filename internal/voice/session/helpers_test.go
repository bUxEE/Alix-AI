package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The tests drive a Session exactly as the gateway does, through its Transport
// and the JSON messages of schemas/voice, with the fake speech runtime of
// internal/providers/voice/fake and a scripted TurnRunner. No audio model runs.

var testNow = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

func testScope(session string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-prod-3301", ApplicationID: "demo-app", TenantID: "tnt-acme-7781", WorkspaceID: "wsp-north-2290",
		PrincipalID: "usr-jane-4410", Roles: []string{"receptionist"}, CapabilityScopes: []string{"appointments.book", "appointments.read"},
		SessionID: session,
	}
}

var pcm16k = voice.PCM16Mono(16000)

func wireFormat(f voice.AudioFormat) map[string]any {
	return map[string]any{"encoding": string(f.Encoding), "sample_rate": f.SampleRate, "channels": f.Channels}
}

// ---- the schema every emitted event is checked against ----

type urnLoader struct{ root string }

var urnRE = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9-]+):([a-z0-9-]+):v[0-9]+$`)

func (l urnLoader) Load(url string) (any, error) {
	m := urnRE.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("cannot resolve %q", url)
	}
	raw, err := os.ReadFile(filepath.Join(l.root, "schemas", m[1], m[2]+".schema.json"))
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

var (
	schemaOnce sync.Once
	serverEv   *jsonschema.Schema
	schemaErr  error
)

// serverEventSchema is schemas/voice/server-event, compiled once.
func serverEventSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(urnLoader{root: filepath.Join("..", "..", "..")})
		serverEv, schemaErr = c.Compile("urn:alixai:schema:voice:server-event:v1")
	})
	if schemaErr != nil {
		t.Fatalf("the server-event schema does not compile: %v", schemaErr)
	}
	return serverEv
}

// ---- the transport ----

type frame struct {
	binary bool
	data   []byte
}

// chanTransport is the connection of a test: the client side writes into in and
// reads from out.
type chanTransport struct {
	in  chan frame
	out chan frame
}

func newChanTransport() *chanTransport {
	return &chanTransport{in: make(chan frame, 64), out: make(chan frame, 4096)}
}

func (c *chanTransport) Read(ctx context.Context) (bool, []byte, error) {
	select {
	case f := <-c.in:
		return f.binary, f.data, nil
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}

func (c *chanTransport) Write(ctx context.Context, binary bool, data []byte) error {
	select {
	case c.out <- frame{binary: binary, data: append([]byte(nil), data...)}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// event is a decoded server event with its raw JSON.
type event struct {
	Type string
	M    map[string]any
	Raw  []byte
}

func (e event) str(k string) string { s, _ := e.M[k].(string); return s }

// ---- the scripted turn runner ----

type turnCall struct {
	Scope    contracts.Scope
	Text     string
	Language string
	// TTSCallsAtEntry is how many speech syntheses had been started when the
	// turn began running: speech never starts before the turn returns.
	TTSCallsAtEntry int
}

// fakeTurns is a TurnRunner standing in for the dialogue engine. It keeps
// state per conversation session, the way the engine's session store does, so
// that a test can see that voice and text land in one place.
type fakeTurns struct {
	t   testing.TB
	tts *fake.Provider

	mu    sync.Mutex
	calls []turnCall
	// reply builds the result of call n (1-based) for text.
	reply func(n int, text string) (dialogue.Result, error)
	// gate, when set, holds every turn until it is closed; entered is signalled
	// (non-blocking) when a turn starts.
	gate    chan struct{}
	entered chan struct{}
	// goal is the per-session structured state: the user messages the session
	// has seen, voice and text alike.
	goal map[string][]string
}

func newFakeTurns(t testing.TB, tts *fake.Provider) *fakeTurns {
	return &fakeTurns{t: t, tts: tts, goal: map[string][]string{}, entered: make(chan struct{}, 16)}
}

func (f *fakeTurns) RunTextTurn(ctx context.Context, scope contracts.Scope, text, language string) (dialogue.Result, error) {
	f.mu.Lock()
	call := turnCall{Scope: scope.Clone(), Text: text, Language: language}
	if f.tts != nil {
		call.TTSCallsAtEntry = f.tts.CallCount()
	}
	f.calls = append(f.calls, call)
	n := len(f.calls)
	f.goal[scope.SessionID] = append(f.goal[scope.SessionID], text)
	gate, reply := f.gate, f.reply
	f.mu.Unlock()
	select {
	case f.entered <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return dialogue.Result{}, ctx.Err()
		}
	}
	if reply == nil {
		return verifiedResult("I can help you book an appointment."), nil
	}
	return reply(n, text)
}

func (f *fakeTurns) Calls() []turnCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]turnCall(nil), f.calls...)
}

func (f *fakeTurns) Session(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.goal[id]...)
}

// verifiedResult is what the engine returns for a turn that gave an answer: a
// directive that carries no fact, and the model's text, verified.
func verifiedResult(text string) dialogue.Result {
	turn := ids.New("trn")
	d := conversation.DialogueDirective{
		TurnID: act.TurnID(turn), Act: conversation.DialogueDirectiveActOfferHelp,
		Facts: []act.Fact{}, MustState: []common.FieldName{}, MissingFields: []common.FieldName{}, InvalidFields: []act.Issue{},
	}
	out, err := verify.Realise(text, &d, nil, verify.Options{Language: verify.LangEN})
	if err != nil {
		panic(err)
	}
	return dialogue.Result{TurnID: turn, Language: verify.LangEN, Directive: d, Response: out}
}

// preparedResult is a turn that prepared an action and awaits confirmation.
func preparedResult(text string) dialogue.Result {
	r := verifiedResult(text)
	r.Directive.Act = conversation.DialogueDirectiveActRequestConfirmation
	r.Prepared = &contracts.PreparedAction{PreparationID: act.PreparationID(ids.New("prep"))}
	tok, err := actions.ParseConfirmationToken("cfm_" + ids.New("tok")[4:] + strings.Repeat("a", 17))
	if err != nil {
		panic(err)
	}
	r.Confirmation = tok
	return r
}

// ---- the harness ----

type harness struct {
	t     *testing.T
	svc   *Service
	reg   *providers.Registry
	stt   *fake.Provider
	tts   *fake.Provider
	turns *fakeTurns
	sess  *Session
	tr    *chanTransport
	done  chan CloseReason
	logs  *syncBuf
	clk   *clock.Fake
	deps  Deps
	// finished is set once the current session has been closed and has returned.
	finished bool
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harnessOpt func(*harness, *Deps, *Options)

func withDeps(f func(*Deps)) harnessOpt       { return func(_ *harness, d *Deps, _ *Options) { f(d) } }
func withOptions(f func(*Options)) harnessOpt { return func(_ *harness, _ *Deps, o *Options) { f(o) } }

// noTTS leaves the synthesis role unregistered.
func noTTS() harnessOpt { return func(h *harness, _ *Deps, _ *Options) { h.tts = nil } }

// newHarness builds a service over fake STT and TTS, opens a voice session on
// session ses-voice-1 and serves it in the background.
func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t, clk: clock.NewFake(testNow), logs: &syncBuf{}}
	h.stt, h.tts = fake.New(fake.WithName("fake-stt")), fake.New(fake.WithName("fake-tts"))
	o := Options{}
	d := Deps{Clock: h.clk, Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	for _, f := range opts {
		f(h, &d, &o)
	}
	h.turns = newFakeTurns(t, h.tts)
	h.reg = providers.NewRegistry(h.clk)
	if err := voice.Register(h.reg, providers.RoleSTT, h.stt); err != nil {
		t.Fatal(err)
	}
	if err := voice.Register(h.reg, providers.RoleVAD, fake.New(fake.WithName("fake-vad"))); err != nil {
		t.Fatal(err)
	}
	if h.tts != nil {
		if err := voice.Register(h.reg, providers.RoleTTS, h.tts); err != nil {
			t.Fatal(err)
		}
	}
	d.Providers = h.reg
	h.deps = d
	svc, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	if o.Turns == nil {
		o.Turns = h.turns
	}
	h.open(testScope("ses-voice-1"), o)
	t.Cleanup(h.close)
	return h
}

// open opens and serves a session; it replaces the harness's current one.
func (h *harness) open(scope contracts.Scope, o Options) {
	h.t.Helper()
	if h.sess != nil {
		h.close()
	}
	sess, err := h.svc.Open(context.Background(), scope, o)
	if err != nil {
		h.t.Fatalf("Open: %v", err)
	}
	h.sess, h.tr, h.done, h.finished = sess, newChanTransport(), make(chan CloseReason, 1), false
	go func() { h.done <- sess.Serve(context.Background(), h.tr) }()
	h.expect("session.started")
}

func (h *harness) close() {
	if h.finished {
		return
	}
	h.finished = true
	h.sess.Close(ReasonClientClosed)
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("Serve did not return after Close")
	}
}

// ---- the client side ----

func (h *harness) send(v any) {
	h.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tr.in <- frame{data: raw}
}

func (h *harness) sendAudio(pcm []byte) { h.tr.in <- frame{binary: true, data: pcm} }

func (h *harness) captureStart() {
	h.send(map[string]any{"type": "capture.start", "format": wireFormat(pcm16k)})
}

// submit sends one finalised utterance of dur of audio.
func (h *harness) submit(pcm []byte) {
	h.send(map[string]any{"type": "audio.submit", "format": wireFormat(pcm16k), "audio": base64.StdEncoding.EncodeToString(pcm)})
}

// tone is d of audio the fake recogniser accepts.
func tone(d time.Duration) []byte { return fake.Tone(pcm16k, d, 6000) }

// next reads the next message the session sent; a binary message is returned
// as an event of type "audio". Every JSON event is validated against the
// schema.
func (h *harness) next(timeout time.Duration) (event, bool) {
	h.t.Helper()
	select {
	case f := <-h.tr.out:
		if f.binary {
			return event{Type: "audio", Raw: f.data}, true
		}
		var m map[string]any
		if err := json.Unmarshal(f.data, &m); err != nil {
			h.t.Fatalf("the session sent invalid JSON: %v: %s", err, f.data)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(f.data))
		if err != nil {
			h.t.Fatal(err)
		}
		if err := serverEventSchema(h.t).Validate(doc); err != nil {
			h.t.Fatalf("the event does not validate against schemas/voice/server-event: %v\n%s", err, f.data)
		}
		typ, _ := m["type"].(string)
		return event{Type: typ, M: m, Raw: f.data}, true
	case <-time.After(timeout):
		return event{}, false
	}
}

// expect reads until an event of type typ and returns it, with the messages
// that came before it.
func (h *harness) expectWith(typ string) (event, []event) {
	h.t.Helper()
	var before []event
	for {
		ev, ok := h.next(5 * time.Second)
		if !ok {
			h.t.Fatalf("timed out waiting for %s (saw %s)", typ, types(before))
		}
		if ev.Type == typ {
			return ev, before
		}
		before = append(before, ev)
	}
}

func (h *harness) expect(typ string) event {
	h.t.Helper()
	ev, _ := h.expectWith(typ)
	return ev
}

// drain reads everything that arrives within d.
func (h *harness) drain(d time.Duration) []event {
	h.t.Helper()
	var out []event
	for {
		ev, ok := h.next(d)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

func types(evs []event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return "[" + strings.Join(s, " ") + "]"
}

// utterance runs one submitted utterance (script says what the recogniser
// hears) through to the response event and returns the events it produced, the
// transcript and the response included.
func (h *harness) utterance(script string) (events []event) {
	h.t.Helper()
	h.stt.Script(script)
	h.submit(tone(200 * time.Millisecond))
	_, before := h.expectWith("response")
	events = append(events, before...)
	return events
}

func wantCode(t testing.TB, ev event, code string) {
	t.Helper()
	e, _ := ev.M["error"].(map[string]any)
	if ev.Type != "error" || e["code"] != code {
		t.Fatalf("event = %s, want an error with code %s", ev.Raw, code)
	}
}

func waitUntil(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---- small adapters ----

type retainerFunc func(scope contracts.Scope, a RetainedAudio) error

func (f retainerFunc) Retain(_ context.Context, s contracts.Scope, a RetainedAudio) error {
	return f(s, a)
}

type speakerFunc func(f voice.AudioFormat, pcm []byte) (SpeakerHint, error)

func (f speakerFunc) Recognize(_ context.Context, _ contracts.Scope, fm voice.AudioFormat, pcm []byte) (SpeakerHint, error) {
	return f(fm, pcm)
}

func base64Of(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func containsBytes(s string, b []byte) bool { return strings.Contains(s, string(b)) }

func asContractsError(err error, target **contracts.Error) bool { return errors.As(err, target) }

func goroutines() int { return runtime.NumGoroutine() }

func timeFact(field, v string) act.Fact {
	return act.Fact{Field: common.FieldName(field), Value: act.CanonicalValue{Value: act.TimeValue{Value: v}}}
}

// testCatalog lists one installed pack that provides speech recognition and
// synthesis.
type testCatalog struct{}

func (testCatalog) List(context.Context) ([]profile.Pack, error) {
	return []profile.Pack{{ID: "en", Language: "en", Roles: []providers.Role{providers.RoleSTT, providers.RoleTTS, providers.RoleVAD}}}, nil
}

func newTestPacks(t testing.TB) *profile.Packs {
	t.Helper()
	p, err := profile.NewPacks(testCatalog{}, profile.NopLoader{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
