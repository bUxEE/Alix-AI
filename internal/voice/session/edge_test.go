package session

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// A capture that ends having heard nothing, a second capture, audio with no
// capture and an oversize chunk are each refused or cancelled with a typed
// answer, and none of them runs a turn.
func TestCaptureEdges(t *testing.T) {
	h := newHarness(t)

	h.stt.Script("book a table")
	h.captureStart()
	h.expect("capture.started")
	h.send(map[string]any{"type": "capture.end"}) // no audio was sent
	if ev := h.expect("capture.cancelled"); ev.str("reason") != "no_speech" {
		t.Errorf("capture.cancelled = %s", ev.Raw)
	}

	h.sendAudio(tone(50 * time.Millisecond)) // no capture is active
	wantCode(t, h.expect("error"), "validation_failed")

	h.captureStart()
	h.expect("capture.started")
	h.captureStart() // already capturing
	wantCode(t, h.expect("error"), "conflict")
	h.submit(tone(50 * time.Millisecond)) // submission while capturing
	wantCode(t, h.expect("error"), "conflict")
	h.sendAudio(make([]byte, h.svc.d.Limits.MaxChunkBytes+2)) // one chunk over the limit
	ev, _ := h.expectWith("error")
	wantCode(t, ev, "validation_failed")
	if c := h.expect("capture.cancelled"); c.str("reason") != "invalid_audio" {
		t.Errorf("capture.cancelled = %s", c.Raw)
	}
	h.send(map[string]any{"type": "capture.cancel"}) // nothing is capturing any more
	wantCode(t, h.expect("error"), "validation_failed")

	// A submission of audio that is not a whole number of frames, or not base64.
	h.send(map[string]any{"type": "audio.submit", "format": wireFormat(pcm16k), "audio": "AAAA"}) // 3 bytes
	wantCode(t, h.expect("error"), "validation_failed")
	h.send(map[string]any{"type": "audio.submit", "format": wireFormat(pcm16k), "audio": "@@@@@@@@"})
	wantCode(t, h.expect("error"), "validation_failed")

	if n := len(h.turns.Calls()); n != 0 {
		t.Errorf("%d turns ran", n)
	}
	waitUntil(t, "the recognition streams to be released", func() bool { return h.stt.OpenStreams() == 0 })

	// Silence recognises as nothing: no transcript, no turn.
	h.stt.Script(" ")
	h.submit(tone(50 * time.Millisecond))
	if ev := h.expect("capture.cancelled"); ev.str("reason") != "no_speech" {
		t.Errorf("capture.cancelled = %s", ev.Raw)
	}
}

// A connection that sends garbage is told so, and closed after too much of it.
func TestProtocolViolations(t *testing.T) {
	h := newHarness(t)
	for _, raw := range []string{`not json`, `{}`, `{"type":"nope"}`, `[]`, `{"type":"capture.end","extra":1}`} {
		h.tr.in <- frame{data: []byte(raw)}
		wantCode(t, h.expect("error"), "validation_failed")
	}
	if len(h.turns.Calls()) != 0 {
		t.Error("a malformed message ran a turn")
	}
	for i := 0; i < maxViolations; i++ {
		h.tr.in <- frame{data: []byte(`not json`)}
	}
	select {
	case r := <-h.done:
		h.finished = true
		if r != ReasonProtocol {
			t.Errorf("closed for %q, want %q", r, ReasonProtocol)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection was not closed")
	}
}

// A connection never outlives the credential it was opened with, a second one
// for the same conversation session takes over the first, and the number of
// connections is bounded.
func TestSessionLifetimeAndLimits(t *testing.T) {
	t.Run("the credential's expiry ends the connection", func(t *testing.T) {
		h := newHarness(t, withOptions(func(o *Options) { o.Deadline = testNow.Add(150 * time.Millisecond) }))
		select {
		case r := <-h.done:
			if r != ReasonExpired {
				t.Errorf("closed for %q, want %q", r, ReasonExpired)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the connection outlived its credential")
		}
		h.finished = true
	})

	t.Run("an expired credential opens nothing", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.svc.Open(t.Context(), testScope("ses-late"), Options{Turns: h.turns, Deadline: testNow.Add(-time.Second)})
		if contracts.CodeOf(err) != contracts.ErrExpired {
			t.Errorf("Open with an expired deadline = %v", err)
		}
	})

	t.Run("a new connection takes over", func(t *testing.T) {
		h := newHarness(t)
		first, firstDone := h.sess, h.done
		_ = first
		second, err := h.svc.Open(t.Context(), testScope("ses-voice-1"), Options{Turns: h.turns})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-firstDone:
			if r != ReasonReplaced {
				t.Errorf("the first connection closed for %q, want %q", r, ReasonReplaced)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the first connection was not replaced")
		}
		h.finished = true
		second.Close(ReasonClientClosed)
		waitUntil(t, "no live sessions", func() bool { return h.svc.Live() == 0 })
	})

	t.Run("at most MaxSessions connections", func(t *testing.T) {
		h := newHarness(t, withDeps(func(d *Deps) { d.MaxSessions, d.MaxSessionsPerTenant, d.MaxSessionsPerPrincipal = 2, 2, 2 }))
		a, err := h.svc.Open(t.Context(), testScope("ses-a"), Options{Turns: h.turns})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close(ReasonClientClosed)
		_, err = h.svc.Open(t.Context(), testScope("ses-b"), Options{Turns: h.turns})
		if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
			t.Errorf("a third connection = %v, want resource_exhausted", err)
		}
		// A reconnect of a session that is open is not a new connection.
		b, err := h.svc.Open(t.Context(), testScope("ses-a"), Options{Turns: h.turns})
		if err != nil {
			t.Errorf("a reconnect at the limit: %v", err)
		} else {
			b.Close(ReasonClientClosed)
		}
	})

	// One tenant, or one user, cannot take every slot: the deployment's four are shared out two per tenant and
	// one per user by default, and a reconnect of a user's open session is still not a new connection.
	t.Run("a tenant and a user have their share", func(t *testing.T) {
		h := newHarness(t) // Jane of tnt-acme-7781 holds ses-voice-1
		as := func(tenant, principal, session string) contracts.Scope {
			sc := testScope(session)
			sc.TenantID, sc.PrincipalID = tenant, principal
			return sc
		}
		limit := func(err error) string {
			var ce *contracts.Error
			if !asContractsError(err, &ce) || ce.Code != contracts.ErrResourceExhausted {
				return ""
			}
			s, _ := ce.Details["limit"].(string)
			return s
		}
		jane := testScope("ses-jane-2")
		if _, err := h.svc.Open(t.Context(), jane, Options{Turns: h.turns}); limit(err) != "principal" {
			t.Errorf("a second session of one user = %v, want resource_exhausted (principal)", err)
		}
		// The same user of another application is another principal.
		otherApp := jane
		otherApp.ApplicationID = "other-app"
		o, err := h.svc.Open(t.Context(), otherApp, Options{Turns: h.turns})
		if err != nil {
			t.Fatalf("a user of another application: %v", err)
		}
		defer o.Close(ReasonClientClosed)
		sam, err := h.svc.Open(t.Context(), as("tnt-acme-7781", "usr-sam-5520", "ses-sam-1"), Options{Turns: h.turns})
		if err != nil {
			t.Fatalf("a second user of the tenant: %v", err)
		}
		defer sam.Close(ReasonClientClosed)
		if _, err := h.svc.Open(t.Context(), as("tnt-acme-7781", "usr-ann-6630", "ses-ann-1"), Options{Turns: h.turns}); limit(err) != "tenant" {
			t.Errorf("a third session of one tenant = %v, want resource_exhausted (tenant)", err)
		}
		// Another tenant still gets a slot: the fourth of the deployment.
		bob, err := h.svc.Open(t.Context(), as("tnt-beta-1200", "usr-bob-7740", "ses-bob-1"), Options{Turns: h.turns})
		if err != nil {
			t.Fatalf("another tenant was refused: %v", err)
		}
		defer bob.Close(ReasonClientClosed)
		if _, err := h.svc.Open(t.Context(), as("tnt-gamma-1300", "usr-eve-8850", "ses-eve-1"), Options{Turns: h.turns}); limit(err) != "deployment" {
			t.Errorf("a fifth session = %v, want resource_exhausted (deployment)", err)
		}
		// Sam reconnects his session at every cap: the old one is replaced, not counted.
		again, err := h.svc.Open(t.Context(), as("tnt-acme-7781", "usr-sam-5520", "ses-sam-1"), Options{Turns: h.turns})
		if err != nil {
			t.Fatalf("a reconnect at the user's cap: %v", err)
		}
		again.Close(ReasonClientClosed)
	})

	t.Run("no scope, no session", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.svc.Open(t.Context(), contracts.Scope{}, Options{Turns: h.turns}); contracts.CodeOf(err) != contracts.ErrScopeMissing {
			t.Errorf("Open with no scope = %v", err)
		}
		if _, err := h.svc.Open(t.Context(), testScope("ses-x"), Options{}); err == nil {
			t.Error("Open without a turn runner")
		}
		if _, err := h.svc.Open(t.Context(), testScope("ses-x"), Options{Turns: h.turns, Language: "english"}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("Open with a malformed language = %v", err)
		}
		reg := providers.NewRegistry(clock.NewFake(testNow))
		if _, err := New(Deps{Providers: reg, Limits: voice.Limits{MaxAudioBytes: -1, MaxChunkBytes: 1, MaxTextRunes: 1}}); err == nil {
			t.Error("New accepted invalid limits")
		}
		if _, err := New(Deps{}); err == nil {
			t.Error("New without a provider registry")
		}
	})
}

// Long responses are spoken whole, in order, in pieces the synthesizer accepts.
func TestSpeechPieces(t *testing.T) {
	long := strings.Repeat("Dr Rossi can see you on the first free morning. ", 20)
	for name, tc := range map[string]struct {
		text string
		max  int
	}{
		"fits":            {"Hello there.", 100},
		"sentences":       {long, 120},
		"one long run":    {strings.Repeat("word ", 100), 40},
		"no spaces":       {strings.Repeat("x", 250), 100},
		"multibyte":       {strings.Repeat("è ", 100), 30},
		"newline breaks":  {"First line.\nSecond line.\nThird line.", 15},
		"exactly the max": {strings.Repeat("a", 50), 50},
	} {
		pieces := speechPieces(tc.text, tc.max)
		if len(pieces) == 0 {
			t.Errorf("%s: no pieces", name)
			continue
		}
		var joined []string
		for _, p := range pieces {
			if n := utf8.RuneCountInString(p); n == 0 || n > tc.max {
				t.Errorf("%s: a piece of %d runes (max %d): %q", name, n, tc.max, p)
			}
			joined = append(joined, p)
		}
		// Nothing is lost, added or reordered: the pieces are the words of the text.
		if diff := cmp.Diff(compact(tc.text), compact(strings.Join(joined, " "))); diff != "" && name != "no spaces" && name != "multibyte" {
			t.Errorf("%s: the pieces are not the text (-want +got):\n%s", name, diff)
		}
		if name == "no spaces" || name == "multibyte" {
			if compact(tc.text) != compact(strings.Join(joined, "")) && compact(tc.text) != compact(strings.Join(joined, " ")) {
				t.Errorf("%s: the pieces are not the text", name)
			}
		}
	}
	if len(speechPieces("  ", 10)) != 0 || len(speechPieces("x", 0)) != 0 {
		t.Error("an empty text or a zero limit yields pieces")
	}
}

func compact(s string) string { return strings.Join(strings.Fields(s), "") }
