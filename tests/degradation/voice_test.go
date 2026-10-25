package degradation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/voice/session"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The voice side: the real voice session service over the real dialogue engine (the same session as the text API),
// with the fake speech runtime of internal/providers/voice/fake failing where section 18 says it fails. The client
// is the Transport: every event the session sends is validated against schemas/voice/server-event.

var pcm16k = voice.PCM16Mono(16000)

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

func serverEventSchema(t testing.TB) *jsonschema.Schema {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(urnLoader{root: filepath.Join("..", "..")})
		serverEv, schemaErr = c.Compile("urn:alixai:schema:voice:server-event:v1")
	})
	if schemaErr != nil {
		t.Fatalf("the server-event schema does not compile: %v", schemaErr)
	}
	return serverEv
}

type frame struct {
	binary bool
	data   []byte
}

type chanTransport struct{ in, out chan frame }

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

type event struct {
	Type string
	M    map[string]any
	Raw  []byte
}

func (e event) str(k string) string { s, _ := e.M[k].(string); return s }

type voiceRig struct {
	t    *testing.T
	w    *world
	stt  *vfake.Provider
	tts  *vfake.Provider
	svc  *session.Service
	sess *session.Session
	tr   *chanTransport
	done chan session.CloseReason
}

const askItem = "show me item item_1"

// newVoice builds the world, seeds an item, scripts the model for the read, and opens a served voice session on the
// session of the text API.
func newVoice(t *testing.T, withTTS bool) *voiceRig {
	t.Helper()
	w := newWorld(t)
	w.app.Seed(tenantA, workspace, "item_1", "Alpha widget")
	w.model.On(pfakeProposal()).Reply(`{"capability_id":"items.get","fields":{"item_id":"item_1"},"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9}}`)
	w.model.On(pfake.Constrained(providers.ConstraintNone)).Reply("Here is what I found. Name: Alpha widget.")
	r := &voiceRig{t: t, w: w, stt: vfake.New(vfake.WithName("fake-stt")), tts: vfake.New(vfake.WithName("fake-tts"))}
	reg := providers.NewRegistry(w.clk)
	if err := voice.Register(reg, providers.RoleSTT, r.stt); err != nil {
		t.Fatal(err)
	}
	if withTTS {
		if err := voice.Register(reg, providers.RoleTTS, r.tts); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := session.New(session.Deps{Providers: reg, Clock: w.clk})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	r.open()
	return r
}

func (r *voiceRig) open() {
	r.t.Helper()
	sess, err := r.svc.Open(ctxb(), memberScope(), session.Options{Turns: session.EngineTurns(r.w.eng)})
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	r.sess, r.tr, r.done = sess, &chanTransport{in: make(chan frame, 64), out: make(chan frame, 4096)}, make(chan session.CloseReason, 1)
	go func() { r.done <- sess.Serve(context.Background(), r.tr) }()
	r.expect("session.started")
	r.t.Cleanup(func() {
		r.sess.Close(session.ReasonClientClosed)
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			r.t.Error("Serve did not return after Close")
		}
	})
}

func (r *voiceRig) send(v any) {
	r.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		r.t.Fatal(err)
	}
	r.tr.in <- frame{data: raw}
}

func wire(f voice.AudioFormat) map[string]any {
	return map[string]any{"encoding": string(f.Encoding), "sample_rate": f.SampleRate, "channels": f.Channels}
}

func (r *voiceRig) captureStart() {
	r.send(map[string]any{"type": "capture.start", "format": wire(pcm16k)})
}

func (r *voiceRig) submit() {
	pcm := vfake.Tone(pcm16k, 200*time.Millisecond, 6000)
	r.send(map[string]any{"type": "audio.submit", "format": wire(pcm16k), "audio": base64.StdEncoding.EncodeToString(pcm)})
}

func (r *voiceRig) next(timeout time.Duration) (event, bool) {
	r.t.Helper()
	select {
	case f := <-r.tr.out:
		if f.binary {
			return event{Type: "audio", Raw: f.data}, true
		}
		var m map[string]any
		if err := json.Unmarshal(f.data, &m); err != nil {
			r.t.Fatalf("the session sent invalid JSON: %v: %s", err, f.data)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(f.data))
		if err != nil {
			r.t.Fatal(err)
		}
		if err := serverEventSchema(r.t).Validate(doc); err != nil {
			r.t.Fatalf("the event does not validate against schemas/voice/server-event: %v\n%s", err, f.data)
		}
		typ, _ := m["type"].(string)
		return event{Type: typ, M: m, Raw: f.data}, true
	case <-time.After(timeout):
		return event{}, false
	}
}

func (r *voiceRig) expectWith(typ string) (event, []event) {
	r.t.Helper()
	var before []event
	for {
		ev, ok := r.next(5 * time.Second)
		if !ok {
			var seen []string
			for _, b := range before {
				seen = append(seen, b.Type)
			}
			r.t.Fatalf("timed out waiting for %s (saw %v)", typ, seen)
		}
		if ev.Type == typ {
			return ev, before
		}
		before = append(before, ev)
	}
}

func (r *voiceRig) expect(typ string) event {
	r.t.Helper()
	ev, _ := r.expectWith(typ)
	return ev
}

func (r *voiceRig) drain(d time.Duration) []event {
	var out []event
	for {
		ev, ok := r.next(d)
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

func countType(evs []event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitFor(t, what, cond)
}

func wantErrorEvent(t *testing.T, ev event, code, effect string) {
	t.Helper()
	e, _ := ev.M["error"].(map[string]any)
	if ev.Type != "error" || e["code"] != code || ev.str("effect") != effect {
		t.Fatalf("event %s, want an error with code %s and effect %s", ev.Raw, code, effect)
	}
}

// textTurn runs a turn on the text API of the same session.
func (r *voiceRig) textTurn() {
	r.t.Helper()
	res, err := r.w.eng.Turn(ctxb(), memberScope(), turnInput(askItem))
	if err != nil || res.Code != "" {
		r.t.Fatalf("a text turn on the same session: err %v code %q", err, res.Code)
	}
}

// Section 18, "STT unavailable": text input remains available.
func TestDegradeSTTUnavailable(t *testing.T) {
	if providers.RoleSTT.Effect() != providers.EffectTextInputOnly {
		t.Fatalf("the STT effect is %q", providers.RoleSTT.Effect())
	}

	t.Run("control: speech recognition works, runs the turn and answers", func(t *testing.T) {
		r := newVoice(t, true)
		r.stt.Script(askItem)
		r.submit()
		r.expect("transcript.final")
		resp := r.expect("response")
		if !strings.Contains(resp.str("text"), "Alpha widget") {
			t.Fatalf("response %s", resp.Raw)
		}
	})

	t.Run("recognition is down: a typed error, the session stays open, and text input still works", func(t *testing.T) {
		r := newVoice(t, true)
		r.stt.FailNext(vfake.FailUnavailable)
		r.captureStart()
		wantErrorEvent(t, r.expect("error"), "unavailable", providers.EffectTextInputOnly)
		select {
		case reason := <-r.done:
			t.Fatalf("the session closed (%s) because recognition failed", reason)
		default:
		}
		before := len(r.w.model.Prompts())
		r.textTurn() // text input remains available, on the same session
		if len(r.w.model.Prompts()) == before {
			t.Fatal("the text turn did not run")
		}
		// And voice comes back with the recogniser, in the same session.
		r.stt.Script(askItem)
		r.submit()
		r.expect("transcript.final")
		r.expect("response")
	})

	t.Run("a recognition that fails part way yields no transcript and runs no turn", func(t *testing.T) {
		r := newVoice(t, true)
		r.stt.FailNext(vfake.FailPartialStream)
		r.stt.Script(askItem)
		r.captureStart()
		r.expect("capture.started")
		for i := 0; i < 2; i++ {
			r.tr.in <- frame{binary: true, data: vfake.Tone(pcm16k, 100*time.Millisecond, 6000)}
		}
		r.send(map[string]any{"type": "capture.end"})
		ev, before := r.expectWith("error")
		wantErrorEvent(t, ev, "unavailable", providers.EffectTextInputOnly)
		if countType(before, "transcript.final") != 0 {
			t.Fatal("a partial recognition produced a final transcript")
		}
		if c := r.expect("capture.cancelled"); c.str("reason") != "stt_failed" {
			t.Fatalf("capture.cancelled %s", c.Raw)
		}
		if n := len(r.w.model.Prompts()); n != 0 {
			t.Fatalf("a partial transcript reached the dialogue engine (%d model calls)", n)
		}
		waitUntil(t, "the recognition stream to be released", func() bool { return r.stt.OpenStreams() == 0 })
	})

	t.Run("submitted audio that times out", func(t *testing.T) {
		r := newVoice(t, true)
		r.stt.FailNext(vfake.FailTimeout)
		r.submit()
		wantErrorEvent(t, r.expect("error"), "timeout", providers.EffectTextInputOnly)
		r.textTurn()
	})

	t.Run("no recogniser is registered: no voice session opens, and text is untouched", func(t *testing.T) {
		w := newWorld(t)
		svc, err := session.New(session.Deps{Providers: providers.NewRegistry(w.clk), Clock: w.clk})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.Open(ctxb(), memberScope(), session.Options{Turns: session.EngineTurns(w.eng)})
		wantCode(t, err, contracts.ErrUnavailable)
		var ce *contracts.Error
		if !asContracts(err, &ce) || ce.Details["effect"] != providers.EffectTextInputOnly {
			t.Fatalf("the refusal does not carry the text_input_only effect: %v", err)
		}
		w.app.Seed(tenantA, workspace, "item_1", "Alpha widget")
		w.model.On(pfakeProposal()).Reply(`{"capability_id":"items.get","fields":{"item_id":"item_1"},"entity_refs":[],"intent":"task","uncertainty":{"overall":0.9}}`)
		w.model.On(pfake.Constrained(providers.ConstraintNone)).Reply("Here is what I found. Name: Alpha widget.")
		if res := w.say(askItem); res.Code != "" {
			t.Fatalf("text turn: code %q", res.Code)
		}
	})
}

// Section 18, "TTS unavailable": return verified text.
func TestDegradeTTSUnavailable(t *testing.T) {
	if providers.RoleTTS.Effect() != providers.EffectVerifiedTextOnly {
		t.Fatalf("the TTS effect is %q", providers.RoleTTS.Effect())
	}

	t.Run("control: with synthesis the response is spoken", func(t *testing.T) {
		r := newVoice(t, true)
		r.stt.Script(askItem)
		r.submit()
		resp := r.expect("response")
		if resp.str("speech") != "queued" {
			t.Fatalf("response %s, want speech queued", resp.Raw)
		}
		if countType(r.drain(300*time.Millisecond), "audio") == 0 {
			t.Fatal("control: no audio frames reached the client, so \"no audio\" below proves nothing")
		}
	})

	t.Run("synthesis is down: the verified text is delivered and no audio follows", func(t *testing.T) {
		r := newVoice(t, true)
		r.tts.FailNext(vfake.FailUnavailable)
		r.stt.Script(askItem)
		r.submit()
		resp := r.expect("response")
		text := resp.str("text")
		if text == "" || !strings.Contains(text, "Alpha widget") {
			t.Fatalf("response %s: the verified text must be delivered", resp.Raw)
		}
		ev, before := r.expectWith("error")
		wantErrorEvent(t, ev, "unavailable", providers.EffectVerifiedTextOnly)
		if ev.str("turn_id") != resp.str("turn_id") {
			t.Fatalf("the error is for turn %s, the response for %s", ev.str("turn_id"), resp.str("turn_id"))
		}
		rest := append(before, r.drain(300*time.Millisecond)...)
		if n := countType(rest, "audio"); n != 0 {
			t.Fatalf("%d audio frames followed a failed synthesis", n)
		}
		// The session carries on: the next utterance is answered, spoken again once synthesis is back.
		r.stt.Script(askItem)
		r.submit()
		if again := r.expect("response"); again.str("speech") != "queued" {
			t.Fatalf("after recovery: %s", again.Raw)
		}
	})

	t.Run("no synthesis role: speech is off, the text is still the answer, and the session says so", func(t *testing.T) {
		r := newVoice(t, false)
		r.stt.Script(askItem)
		r.submit()
		resp := r.expect("response")
		if resp.str("speech") != "skipped" || resp.str("speech_reason") != "tts_unavailable" || !strings.Contains(resp.str("text"), "Alpha widget") {
			t.Fatalf("response %s", resp.Raw)
		}
		if n := countType(r.drain(200*time.Millisecond), "audio"); n != 0 {
			t.Fatalf("%d audio frames without a synthesis role", n)
		}
	})
}

func turnInput(text string) dialogue.TurnInput { return dialogue.TurnInput{Text: text, Language: "en"} }

func asContracts(err error, target **contracts.Error) bool { return errors.As(err, target) }
