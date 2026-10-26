package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/internal/agui"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	vsession "github.com/buxee/alix-ai/internal/voice/session"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of the voice area run the real stack of newAPIEnv (real session
// store, engine, coordinator and stream hub) with the fake speech runtime in
// the voice roles: no audio model runs, and the model behind the engine is
// scripted as in every other test of this package.

// voiceRig is the fake speech runtime behind the voice service of one test
// server.
type voiceRig struct {
	stt, tts *vfake.Provider
	svc      *vsession.Service
}

var (
	voiceRigs sync.Map // *apiEnv -> *voiceRig
	// voiceNoService leaves the voice service out of the next test server: a
	// deployment with no voice profile.
	voiceNoService bool
)

func init() {
	registerTestServices("v1_voice", func(t testing.TB, e *apiEnv, s *Services) {
		rig := &voiceRig{stt: vfake.New(vfake.WithName("fake-stt")), tts: vfake.New(vfake.WithName("fake-tts"))}
		reg := providers.NewRegistry(e.clk)
		if err := voice.Register(reg, providers.RoleSTT, rig.stt); err != nil {
			t.Fatal(err)
		}
		if err := voice.Register(reg, providers.RoleTTS, rig.tts); err != nil {
			t.Fatal(err)
		}
		if err := voice.Register(reg, providers.RoleVAD, vfake.New(vfake.WithName("fake-vad"))); err != nil {
			t.Fatal(err)
		}
		svc, err := vsession.New(vsession.Deps{Providers: reg, Clock: e.clk})
		if err != nil {
			t.Fatal(err)
		}
		rig.svc = svc
		voiceRigs.Store(e, rig)
		if !voiceNoService {
			if err := Provide[VoiceService](s, svc); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func voiceRigOf(t testing.TB, e *apiEnv) *voiceRig {
	t.Helper()
	v, ok := voiceRigs.Load(e)
	if !ok {
		t.Fatal("the test server has no voice rig")
	}
	return v.(*voiceRig)
}

func voicePath(session string) string { return sessionsPath(session) + "/voice" }

// voiceConn is the client side of one voice socket.
type voiceConn struct {
	t   *testing.T
	c   *websocket.Conn
	ctx context.Context
}

func voiceDial(t *testing.T, e *apiEnv, hs *httptest.Server, query string, mods ...claims) *voiceConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + voicePath(apiSess)
	if query != "" {
		u += "?" + query
	}
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + e.token(mods...)}}})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &voiceConn{t: t, c: c, ctx: ctx}
}

// next reads the next message; a binary message comes back as type "audio".
func (v *voiceConn) next() (string, map[string]any) {
	v.t.Helper()
	typ, data, err := v.c.Read(v.ctx)
	if err != nil {
		v.t.Fatalf("read: %v", err)
	}
	if typ == websocket.MessageBinary {
		return "audio", nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		v.t.Fatalf("not JSON: %s", data)
	}
	kind, _ := m["type"].(string)
	return kind, m
}

// expect reads until an event of type kind.
func (v *voiceConn) expect(kind string) map[string]any {
	v.t.Helper()
	var seen []string
	for {
		got, m := v.next()
		if got == kind {
			return m
		}
		seen = append(seen, got)
		if len(seen) > 64 {
			v.t.Fatalf("no %s after %v", kind, seen)
		}
	}
}

func (v *voiceConn) send(m map[string]any) {
	v.t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		v.t.Fatal(err)
	}
	if err := v.c.Write(v.ctx, websocket.MessageText, raw); err != nil {
		v.t.Fatalf("write: %v", err)
	}
}

func (v *voiceConn) submit(d time.Duration) {
	v.t.Helper()
	f := voice.PCM16Mono(16000)
	v.send(map[string]any{
		"type":   "audio.submit",
		"format": map[string]any{"encoding": string(f.Encoding), "sample_rate": f.SampleRate, "channels": f.Channels},
		"audio":  base64.StdEncoding.EncodeToString(vfake.Tone(f, d, 6000)),
	})
}

// A booking started by voice is confirmed by text, in one session: the spoken
// transcript runs through the real engine like a typed message, the confirmation
// secret reaches the client over the voice socket (and is never spoken), the
// turn is in the stream hub like any other, and the confirmation is the
// structured input of the turns route.
func TestVoiceStartedByVoiceConfirmedByText(t *testing.T) {
	e := newAPIEnv(t)
	rig := voiceRigOf(t, e)
	hs := httptest.NewServer(e.srv.Handler()) // plain-HTTP loopback
	defer hs.Close()

	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget.")
	rig.stt.Script(sayCreate)
	vc := voiceDial(t, e, hs, "language=en")

	started := vc.expect("session.started")
	if started["session_id"] != apiSess || started["speech"] != true || started["retain_raw_audio"] != false {
		t.Fatalf("session.started = %v", started)
	}
	vc.submit(200 * time.Millisecond)
	final := vc.expect("transcript.final")
	if final["text"] != sayCreate {
		t.Fatalf("transcript.final = %v", final)
	}
	done := vc.expect("turn.completed")
	conf, _ := done["confirmation"].(map[string]any)
	prepID, _ := done["preparation_id"].(string)
	token, _ := conf["token"].(string)
	if done["act"] != "request_confirmation" || prepID == "" || conf["preparation_id"] != prepID || !confirmTokenRE.MatchString(token) {
		t.Fatalf("turn.completed = %v: the voice turn did not prepare a confirmable action", done)
	}
	turnID, _ := done["turn_id"].(string)
	resp := vc.expect("response")
	if resp["source"] == "" || resp["text"] == "" {
		t.Fatalf("response = %v", resp)
	}
	t.Logf("response: speech=%v reason=%v source=%v", resp["speech"], resp["speech_reason"], resp["source"])
	if resp["speech"] == "queued" {
		vc.expect("playback.done")
		for _, c := range rig.tts.Calls() {
			if strings.Contains(c.Text, token) {
				t.Error("the confirmation secret was sent to synthesis")
			}
		}
	}

	// One session: the voice turn is the session's first.
	rec := e.call(http.MethodGet, sessionsPath(apiSess), nil)
	wantStatus(t, rec, http.StatusOK)
	got := decodeBody[SessionBody](t, rec)
	if got.TurnCount != 1 || got.Pending == nil || got.Pending.Kind != "confirmation" || string(got.Pending.Preparation.PreparationID) != prepID {
		t.Fatalf("session after the spoken turn = %+v pending = %+v", got, got.Pending)
	}
	// The turn is in the hub, like any other: its events resume.
	evs := e.callAs(e.token(), http.MethodGet, turneventsPath(turnID), nil)
	wantStatus(t, evs, http.StatusOK)
	if !strings.Contains(evs.Body.String(), "event: prepared_action") || strings.Contains(evs.Body.String(), token) {
		t.Errorf("the events of the voice turn: %s", evs.Body.String())
	}

	// Voice cannot confirm. A spoken "yes" is only text.
	e.model.Reset()
	e.onResponse("Please confirm: create an item named widget.")
	rig.stt.Script("yes, confirm it")
	vc.submit(200 * time.Millisecond)
	vc.expect("turn.completed")
	if n := e.host.Effects(); n != 0 {
		t.Fatalf("a spoken confirmation committed %d effects", n)
	}

	// The structured confirmation, by text, on the same session, commits it.
	e.model.Reset()
	e.onResponse("Done. The item widget was created.")
	rec = e.turn(TurnRequest{Confirmation: &TurnConfirmation{PreparationID: prepID, Token: token}})
	wantStatus(t, rec, http.StatusOK)
	if n := e.host.Effects(); n != 1 {
		t.Fatalf("after the confirmation by text the host has %d effects, want 1", n)
	}
	rec = e.call(http.MethodGet, sessionsPath(apiSess), nil)
	if n := decodeBody[SessionBody](t, rec).TurnCount; n != 3 {
		t.Errorf("turn count = %d, want 3 (spoken, spoken, typed) on one session", n)
	}
}

// Everything that can be refused is refused with an HTTP status before the
// upgrade, and the voice route is no way around the identity or the quarantine
// of the session.
func TestVoiceRouteRefusesBeforeTheUpgrade(t *testing.T) {
	e := newAPIEnv(t)
	path := voicePath(apiSess)
	upgrade := []string{"Upgrade", "websocket", "Connection", "Upgrade", "Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}

	// Not an upgrade request.
	rec := e.call(http.MethodGet, path, nil)
	wantError(t, rec, http.StatusUpgradeRequired, contracts.ErrValidationFailed)
	if rec.Header().Get("Upgrade") != "websocket" {
		t.Errorf("a 426 without the Upgrade header: %v", rec.Header())
	}
	// No token.
	wantError(t, e.callAs("", http.MethodGet, path, nil, upgrade...), http.StatusUnauthorized, contracts.ErrUnauthenticated)
	// A session that is not the token's.
	wantError(t, e.callAs(e.token(), http.MethodGet, voicePath("ses_other"), nil, upgrade...), http.StatusForbidden, contracts.ErrScopeViolation)
	// A malformed language and speak.
	wantError(t, e.callAs(e.token(), http.MethodGet, path+"?language=english", nil, upgrade...), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.callAs(e.token(), http.MethodGet, path+"?speak=maybe", nil, upgrade...), http.StatusBadRequest, contracts.ErrValidationFailed)
	// A browser on another origin.
	hs := httptest.NewServer(e.srv.Handler())
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(hs.URL, "http") + path
	_, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + e.token()}, "Origin": {"https://evil.example.test"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Origin = %v, %v; want a 403", resp, err)
	}
	// Every refusal above spent no turn and opened no voice session.
	if n := e.sessionRows(); n != 0 {
		t.Errorf("%d session rows after refusals", n)
	}
}

// voiceNoTurns is a turn runner for a session that is only held open.
type voiceNoTurns struct{}

func (voiceNoTurns) RunTextTurn(context.Context, contracts.Scope, string, string) (dialogue.Result, error) {
	return dialogue.Result{}, contracts.New(contracts.ErrUnavailable, "no turn runs here")
}

// One user holds one voice connection by default: a second, on another conversation session, is refused before the
// upgrade with resource_exhausted, and the refusal says which cap it met.
func TestVoiceRouteRefusesAUserOverTheirShare(t *testing.T) {
	e := newAPIEnv(t)
	held := e.scopeOf()
	held.SessionID = "ses_voice_elsewhere"
	sess, err := voiceRigOf(t, e).svc.Open(context.Background(), held, vsession.Options{Turns: voiceNoTurns{}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(vsession.ReasonClientClosed)
	upgrade := []string{"Upgrade", "websocket", "Connection", "Upgrade", "Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}
	env := wantError(t, e.callAs(e.token(), http.MethodGet, voicePath(apiSess), nil, upgrade...), http.StatusServiceUnavailable, contracts.ErrResourceExhausted)
	if env.Details["limit"] != "principal" || !env.Retryable {
		t.Errorf("refusal = %+v, want a retryable one with details.limit principal", env)
	}
}

// A deployment with no voice profile has no voice service: the route answers
// unavailable with the text_input_only effect, and text carries on.
func TestVoiceUnavailableWithoutTheService(t *testing.T) {
	voiceNoService = true
	defer func() { voiceNoService = false }()
	e := newAPIEnv(t)
	upgrade := []string{"Upgrade", "websocket", "Connection", "Upgrade", "Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}
	env := wantError(t, e.callAs(e.token(), http.MethodGet, voicePath(apiSess), nil, upgrade...), http.StatusServiceUnavailable, contracts.ErrUnavailable)
	if env.Details["effect"] != providers.EffectTextInputOnly {
		t.Errorf("details = %v, want the text_input_only effect", env.Details)
	}
	// The text API of the same server is unaffected.
	e.onProposal(apiProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget.")
	wantStatus(t, e.turn(TurnRequest{Text: sayCreate, Language: "en"}), http.StatusOK)
}

// ADR 0035: the AG-UI capabilities answer states voice honestly. Full duplex
// voice is not claimed until a measured end-to-end test proves it, and the
// answer, the documentation and vsession.FullDuplexMeasured change together:
// flipping the constant without the others (or the others without it) fails
// here.
func TestVoiceCapabilitiesNeverClaimFullDuplexUntilMeasured(t *testing.T) {
	e := newAPIEnv(t)
	rec := e.call(http.MethodGet, "/v1/agui/capabilities", nil)
	wantStatus(t, rec, http.StatusOK)
	doc := decodeBody[AGUICapabilitiesBody](t, rec)

	claimed := !slices.Contains(doc.NotClaimed, "voice_full_duplex")
	if claimed != vsession.FullDuplexMeasured {
		t.Fatalf("the capabilities answer claims full duplex voice = %v but a measured end-to-end test exists = %v: update both together (agui.NotClaimed, docs/integration/agui.md and vsession.FullDuplexMeasured)", claimed, vsession.FullDuplexMeasured)
	}
	if !slices.Contains(agui.NotClaimed, "voice_full_duplex") != claimed {
		t.Errorf("agui.NotClaimed and the served document disagree about full duplex voice")
	}
	multimodal, _ := doc.Agent["multimodal"].(map[string]any)
	in, _ := multimodal["input"].(map[string]any)
	out, _ := multimodal["output"].(map[string]any)
	if !vsession.FullDuplexMeasured && (in["audio"] != false || out["audio"] != false) {
		t.Errorf("multimodal audio input %v / output %v is claimed without a measured end-to-end test", in["audio"], out["audio"])
	}
	// The word appears only as the not-claimed identifier.
	if n := strings.Count(strings.ToLower(rec.Body.String()), "duplex"); !vsession.FullDuplexMeasured && n != 1 {
		t.Errorf("the capabilities answer mentions duplex %d times: %s", n, rec.Body.String())
	}

	// The documentation says the same, in its own words.
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "integration", "agui.md"))
	if err != nil {
		t.Fatal(err)
	}
	says := bytes.Contains(docs, []byte("Full duplex voice is not claimed"))
	if says == vsession.FullDuplexMeasured {
		t.Errorf("docs/integration/agui.md says full duplex voice is not claimed = %v, but a measured end-to-end test exists = %v", says, vsession.FullDuplexMeasured)
	}
	// And so does the voice area's own contract: it describes turn taking with
	// barge-in and claims nothing more.
	spec, err := os.ReadFile(filepath.Join(apiSpecRoot, "paths", "voice.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !vsession.FullDuplexMeasured && strings.Contains(strings.ToLower(string(spec)), "duplex") {
		t.Error("schemas/api/paths/voice.yaml claims or mentions full duplex voice before it is measured")
	}
}
