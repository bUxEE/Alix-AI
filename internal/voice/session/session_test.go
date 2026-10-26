package session

import (
	"bytes"
	"context"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Master plan 13, requirement 1: voice and text share one session and its
// structured goal state. The voice path runs the finalised transcript through
// the same turn runner as a text message, under the very scope the verified
// context token gave the connection: no second session is created, so what a
// spoken turn started a typed one continues.
func TestVoiceAndTextShareSession(t *testing.T) {
	h := newHarness(t)
	scope := testScope("ses-voice-1")
	h.turns.reply = func(n int, text string) (dialogue.Result, error) {
		// What the engine does with its session: the second message finds the
		// goal the first one started.
		if n == 2 {
			return verifiedResult("Your request has been updated."), nil
		}
		return verifiedResult("I can book that."), nil
	}

	h.utterance("book a table for four")
	// The text path: the same runner, called the way POST /turns calls the engine.
	if _, err := h.turns.RunTextTurn(t.Context(), scope, "make it for two", ""); err != nil {
		t.Fatal(err)
	}

	calls := h.turns.Calls()
	if len(calls) != 2 {
		t.Fatalf("the runner saw %d turns, want 2 (one spoken, one typed)", len(calls))
	}
	if calls[0].Text != "book a table for four" {
		t.Errorf("the spoken turn ran %q: the final transcript is the message", calls[0].Text)
	}
	if diff := cmp.Diff(scope, calls[0].Scope); diff != "" {
		t.Errorf("the spoken turn did not run under the token's scope (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(calls[1].Scope, calls[0].Scope); diff != "" {
		t.Errorf("voice and text ran under different scopes:\n%s", diff)
	}
	got := h.turns.Session(scope.SessionID)
	if diff := cmp.Diff([]string{"book a table for four", "make it for two"}, got); diff != "" {
		t.Errorf("the session's state is not one place for both (-want +got):\n%s", diff)
	}
	if n := len(h.turns.goal); n != 1 {
		t.Errorf("the turns landed in %d sessions, want one: %v", n, h.turns.goal)
	}

	// A scope the caller keeps changing afterwards does not reach the session.
	scope.Roles[0] = "admin"
	scope.TenantID = "tnt-other"
	h.utterance("and a window seat")
	if c := h.turns.Calls()[2].Scope; c.TenantID != "tnt-acme-7781" || c.Roles[0] != "receptionist" {
		t.Errorf("the session runs under a scope the caller can change: %+v", c)
	}
}

// Requirement 2: the finalised transcript is visible to the host. Interim
// hypotheses stream while the user speaks, then exactly one final transcript
// precedes the turn that runs it.
func TestFinalTranscriptVisibleToHost(t *testing.T) {
	h := newHarness(t)
	h.stt.Script("book a table for four")
	h.captureStart()
	started := h.expect("capture.started")
	utt := started.str("utterance_id")
	for i := 0; i < 2; i++ {
		h.sendAudio(tone(100 * time.Millisecond))
	}
	h.send(map[string]any{"type": "capture.end"})

	final, before := h.expectWith("transcript.final")
	if final.str("text") != "book a table for four" || final.str("utterance_id") != utt {
		t.Fatalf("final transcript = %s", final.Raw)
	}
	var interim []string
	for _, ev := range before {
		if ev.Type != "transcript.interim" || ev.str("utterance_id") != utt {
			t.Errorf("before the final transcript: %s", ev.Raw)
			continue
		}
		interim = append(interim, ev.str("text"))
	}
	if diff := cmp.Diff([]string{"book", "book a"}, interim); diff != "" {
		t.Errorf("interim hypotheses (-want +got):\n%s", diff)
	}
	// The transcript is shown before the turn it starts has any outcome.
	if len(h.turns.Calls()) > 1 {
		t.Fatalf("the turn ran %d times", len(h.turns.Calls()))
	}
	_, afterFinal := h.expectWith("turn.completed")
	for _, ev := range afterFinal {
		if ev.Type == "transcript.final" {
			t.Errorf("a second final transcript for one utterance")
		}
	}
	if got := h.turns.Calls()[0].Text; got != "book a table for four" {
		t.Errorf("the turn ran %q", got)
	}
}

// Requirement 4: speech starts only after material facts are verified. The
// turn returns before any synthesis starts, speech is made from the verified
// outcome's text exactly, and text that does not verify is shown, never spoken.
// SEC-094
func TestTTSOnlyAfterVerification(t *testing.T) {
	t.Run("synthesis starts after the turn returns", func(t *testing.T) {
		h := newHarness(t)
		gate := make(chan struct{})
		h.turns.gate = gate
		h.stt.Script("when is the doctor free")
		h.submit(tone(200 * time.Millisecond))
		<-h.turns.entered
		time.Sleep(100 * time.Millisecond)
		if n := h.tts.CallCount(); n != 0 {
			t.Fatalf("speech was synthesised (%d calls) while the turn was still running", n)
		}
		close(gate)
		resp := h.expect("response")
		if resp.str("speech") != "queued" || resp.str("source") != "model" {
			t.Fatalf("response = %s", resp.Raw)
		}
		h.expect("playback.done")
		calls := h.tts.Calls()
		if len(calls) != 1 || calls[0].Text != resp.str("text") {
			t.Fatalf("synthesis calls = %+v, want exactly the verified response text %q", calls, resp.str("text"))
		}
		if at := h.turns.Calls()[0].TTSCallsAtEntry; at != 0 {
			t.Errorf("%d syntheses had started when the turn began", at)
		}
	})

	t.Run("text that fails the verifier is not spoken", func(t *testing.T) {
		h := newHarness(t)
		// An engine that let a wrong fact through: the directive carries 10:30,
		// the "verified" text says 11:45. The session does not rest on the engine.
		h.turns.reply = func(int, string) (dialogue.Result, error) {
			r := verifiedResult("placeholder")
			r.Directive.Act = "present_result"
			r.Directive.Facts = append(r.Directive.Facts, timeFact("slot_start", "10:30"))
			r.Directive.MustState = append(r.Directive.MustState, "slot_start")
			r.Response = verify.Outcome{Text: "Dr. Rossi is free at 11:45.", Source: verify.SourceModel, Reason: verify.ReasonVerified}
			return r, nil
		}
		h.stt.Script("when is the doctor free")
		h.submit(tone(200 * time.Millisecond))
		resp := h.expect("response")
		if resp.str("speech") != "skipped" || resp.str("speech_reason") != "verification_failed" {
			t.Fatalf("response = %s: text that does not verify must not be spoken", resp.Raw)
		}
		if resp.str("text") == "" {
			t.Error("the text is still delivered")
		}
		time.Sleep(50 * time.Millisecond)
		if n := h.tts.CallCount(); n != 0 {
			t.Errorf("%d syntheses for text that failed verification", n)
		}
	})

	t.Run("an outcome with no provenance is not spoken", func(t *testing.T) {
		for name, out := range map[string]verify.Outcome{
			"zero":                 {},
			"model but not proven": {Text: "I can book that.", Source: verify.SourceModel, Reason: verify.ReasonDegraded},
			"template as verified": {Text: "I can book that.", Source: verify.SourceTemplate, Reason: verify.ReasonVerified},
			"unknown source":       {Text: "I can book that.", Source: "tool", Reason: verify.ReasonVerified},
		} {
			r := verifiedResult("I can book that.")
			r.Response = out
			if speakable(r) {
				t.Errorf("%s: speakable", name)
			}
		}
		if r := verifiedResult("I can book that."); !speakable(r) {
			t.Error("a verified outcome is not speakable")
		}
	})

	t.Run("a turn that failed is not spoken", func(t *testing.T) {
		h := newHarness(t)
		h.turns.reply = func(int, string) (dialogue.Result, error) {
			r := verifiedResult("I can book that.")
			return r, contracts.New(contracts.ErrUnavailable, "the audit trail is unavailable")
		}
		h.stt.Script("book it")
		h.submit(tone(200 * time.Millisecond))
		ev, before := h.expectWith("error")
		wantCode(t, ev, "unavailable")
		for _, b := range before {
			if b.Type == "response" || b.Type == "turn.completed" {
				t.Errorf("%s was sent for a turn that failed", b.Type)
			}
		}
		time.Sleep(50 * time.Millisecond)
		if n := h.tts.CallCount(); n != 0 {
			t.Errorf("%d syntheses for a turn that failed", n)
		}
	})
}

// Requirement 3: the user may correct the transcript before any consequential
// confirmation. The corrected text runs through the same pipeline as a new
// message on the same session; the preparation built on the old transcript is
// reported superseded; the confirmation secret of either never leaves in
// speech or in a log; and speech has no way at all to confirm.
func TestTranscriptCorrectionBeforeConfirm(t *testing.T) {
	h := newHarness(t)
	var prep []dialogue.Result
	var mu sync.Mutex
	h.turns.reply = func(n int, text string) (dialogue.Result, error) {
		r := preparedResult("Please confirm the booking for " + map[int]string{1: "four", 2: "two"}[n] + " people.")
		mu.Lock()
		prep = append(prep, r)
		mu.Unlock()
		return r, nil
	}

	h.stt.Script("book a table for four")
	h.submit(tone(200 * time.Millisecond))
	started := h.expect("capture.started")
	utt := started.str("utterance_id")
	_ = utt
	final, before := h.expectWith("transcript.final")
	_ = before
	utt = final.str("utterance_id")
	done, afterFinal := h.expectWith("turn.completed")
	for _, ev := range afterFinal {
		if ev.Type == "turn.completed" {
			t.Fatal("two turns for one utterance")
		}
	}
	first := prep[0]
	if done.str("preparation_id") != string(first.Prepared.PreparationID) {
		t.Fatalf("turn.completed = %s", done.Raw)
	}
	conf, _ := done.M["confirmation"].(map[string]any)
	if conf["token"] != first.Confirmation.Reveal() || conf["preparation_id"] != string(first.Prepared.PreparationID) {
		t.Fatalf("the interface did not get the confirmation of the preparation: %s", done.Raw)
	}
	h.expect("response")
	h.expect("playback.done")

	// The user sees "four" and corrects it, before confirming anything.
	h.send(map[string]any{"type": "transcript.correct", "utterance_id": utt, "text": "book a table for two"})
	corrected, _ := h.expectWith("transcript.corrected")
	if corrected.str("text") != "book a table for two" || corrected.str("previous_text") != "book a table for four" || corrected.str("utterance_id") != utt {
		t.Fatalf("transcript.corrected = %s", corrected.Raw)
	}
	second, _ := h.expectWith("turn.completed")
	if second.str("corrects_turn_id") != first.TurnID {
		t.Errorf("the correction does not name the turn it replaces: %s", second.Raw)
	}
	if second.str("superseded_preparation_id") != string(first.Prepared.PreparationID) {
		t.Errorf("the old preparation is not reported superseded: %s", second.Raw)
	}
	newPrep := string(prep[1].Prepared.PreparationID)
	if second.str("preparation_id") == "" || second.str("preparation_id") != newPrep {
		t.Errorf("the corrected turn's preparation = %q, want %q", second.str("preparation_id"), newPrep)
	}

	calls := h.turns.Calls()
	if len(calls) != 2 || calls[1].Text != "book a table for two" {
		t.Fatalf("the corrected text did not run as a new message: %+v", calls)
	}
	if diff := cmp.Diff(calls[0].Scope, calls[1].Scope); diff != "" {
		t.Errorf("the correction ran under another scope:\n%s", diff)
	}

	t.Run("corrections the session refuses", func(t *testing.T) {
		h.drain(100 * time.Millisecond)
		h.send(map[string]any{"type": "transcript.correct", "utterance_id": utt, "text": "book a table for two"})
		wantCode(t, h.expect("error"), "validation_failed") // unchanged
		h.send(map[string]any{"type": "transcript.correct", "utterance_id": ids.New("utt"), "text": "something"})
		wantCode(t, h.expect("error"), "validation_failed") // unknown utterance
		// A newer utterance exists: only the latest transcript can be corrected.
		h.stt.Script("and a window seat")
		h.submit(tone(200 * time.Millisecond))
		h.expect("turn.completed")
		h.send(map[string]any{"type": "transcript.correct", "utterance_id": utt, "text": "book a table for three"})
		wantCode(t, h.expect("error"), "stale")
		if n := len(h.turns.Calls()); n != 3 {
			t.Errorf("a refused correction ran a turn (%d turns)", n)
		}
	})

	t.Run("the secret is never spoken or logged", func(t *testing.T) {
		h.drain(150 * time.Millisecond)
		for _, c := range h.tts.Calls() {
			for _, r := range prep {
				if strings.Contains(c.Text, r.Confirmation.Reveal()) {
					t.Errorf("a confirmation secret was sent to synthesis: %q", c.Text)
				}
			}
		}
		for _, r := range prep {
			if strings.Contains(h.logs.String(), r.Confirmation.Reveal()) {
				t.Error("a confirmation secret is in the log")
			}
		}
	})

	t.Run("speech cannot confirm", func(t *testing.T) {
		// The runner takes text and nothing else: the voice path has no way to
		// carry a confirmation, a rejection or a choice into the engine.
		rt := reflect.TypeOf((*TurnRunner)(nil)).Elem()
		if rt.NumMethod() != 1 {
			t.Fatalf("TurnRunner has %d methods", rt.NumMethod())
		}
		m := rt.Method(0)
		var params []string
		for i := 0; i < m.Type.NumIn(); i++ {
			params = append(params, m.Type.In(i).String())
		}
		want := []string{"context.Context", "contracts.Scope", "string", "string"}
		if diff := cmp.Diff(want, params); diff != "" {
			t.Errorf("TurnRunner.%s takes (-want +got):\n%s", m.Name, diff)
		}
		// And no message of the client names a confirmation: it is not a voice message.
		h.send(map[string]any{"type": "confirm", "preparation_id": string(first.Prepared.PreparationID), "token": first.Confirmation.Reveal()})
		wantCode(t, h.expect("error"), "validation_failed")
		if n := len(h.turns.Calls()); n != 3 {
			t.Errorf("a confirm message ran a turn (%d turns)", n)
		}
	})
}

// Requirement 5: playback can be cancelled immediately, by the client and by a
// new user utterance (barge-in), and nothing more of it is sent afterwards.
func TestCancelPlaybackImmediate(t *testing.T) {
	long := strings.Repeat("I can book appointments for you and check the schedule. ", 12)
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.turns.reply = func(int, string) (dialogue.Result, error) { return verifiedResult(long), nil }
		// A runtime that hangs after its first chunk: only a cancellation ends it.
		h.tts.StallAfter(1)
		h.stt.Script("what can you do")
		h.submit(tone(200 * time.Millisecond))
		h.expect("playback.started")
		if ev, ok := h.next(2 * time.Second); !ok || ev.Type != "audio" {
			t.Fatalf("no audio followed playback.started: %+v", ev)
		}
		return h
	}
	afterCancel := func(t *testing.T, h *harness, d time.Duration) {
		t.Helper()
		for _, ev := range h.drain(d) {
			if ev.Type == "audio" || ev.Type == "playback.done" {
				t.Errorf("%s after playback.cancelled", ev.Type)
			}
		}
		waitUntil(t, "the synthesis stream to be released", func() bool { return h.tts.OpenStreams() == 0 })
	}

	t.Run("client cancel", func(t *testing.T) {
		h := setup(t)
		start := time.Now()
		h.send(map[string]any{"type": "playback.cancel"})
		ev := h.expect("playback.cancelled")
		if ev.str("reason") != "client" {
			t.Errorf("reason = %s", ev.Raw)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("cancel took %v against a hung synthesizer", d)
		}
		afterCancel(t, h, 150*time.Millisecond)
		// Cancelling with nothing playing is not an error.
		h.send(map[string]any{"type": "playback.cancel"})
		for _, e := range h.drain(100 * time.Millisecond) {
			if e.Type == "error" {
				t.Errorf("a second cancel is an error: %s", e.Raw)
			}
		}
	})

	t.Run("barge-in", func(t *testing.T) {
		h := setup(t)
		h.stt.Script("never mind")
		h.captureStart()
		ev, before := h.expectWith("capture.started")
		// The speech stops before the new utterance is acknowledged.
		var cancelled *event
		for i := range before {
			if before[i].Type == "playback.cancelled" {
				cancelled = &before[i]
			}
		}
		if cancelled == nil || cancelled.str("reason") != "barge_in" {
			t.Fatalf("playback was not cancelled by the new utterance (events before capture.started: %s) %s", types(before), ev.Raw)
		}
		afterCancel(t, h, 150*time.Millisecond)
	})

	t.Run("a response that is not yet speaking is dropped when the user speaks again", func(t *testing.T) {
		h := newHarness(t)
		gate := make(chan struct{})
		h.turns.gate = gate
		h.stt.Script("first thing")
		h.submit(tone(200 * time.Millisecond))
		<-h.turns.entered
		// The user speaks again before the first answer is ready.
		h.stt.Script("second thing")
		h.submit(tone(200 * time.Millisecond))
		h.expect("capture.started")
		h.expect("capture.started") // the second utterance is acknowledged: the barge-in clock has moved
		close(gate)
		first, _ := h.expectWith("response")
		if first.str("speech") != "skipped" || first.str("speech_reason") != "superseded" {
			t.Errorf("the stale response = %s", first.Raw)
		}
	})
}

// Requirement 6: raw audio is not retained by default. Nothing keeps it, the
// session holds none once an utterance is done, no log or event carries it, and
// only a tenant whose policy says so has it handed to the retainer.
// SEC-094
func TestRawAudioNotRetained(t *testing.T) {
	type retained struct {
		scope contracts.Scope
		a     RetainedAudio
	}
	var mu sync.Mutex
	var got []retained
	retainer := retainerFunc(func(scope contracts.Scope, a RetainedAudio) error {
		mu.Lock()
		defer mu.Unlock()
		a.PCM = append([]byte(nil), a.PCM...)
		got = append(got, retained{scope: scope, a: a})
		return nil
	})
	policy := AudioPolicyFunc(func(_ context.Context, scope contracts.Scope) (bool, error) {
		return scope.TenantID == "tnt-retains", nil
	})

	run := func(t *testing.T, h *harness) (streamed, submitted []byte) {
		t.Helper()
		streamed = fake.Tone(pcm16k, 300*time.Millisecond, 12345)
		h.stt.Script("book a table")
		h.captureStart()
		h.expect("capture.started")
		h.sendAudio(streamed[:len(streamed)/2])
		h.sendAudio(streamed[len(streamed)/2:])
		h.send(map[string]any{"type": "capture.end"})
		h.expect("response")

		submitted = fake.Tone(pcm16k, 300*time.Millisecond, 23456)
		h.stt.Script("and a window seat")
		h.submit(submitted)
		h.expect("response")
		h.drain(100 * time.Millisecond)
		return streamed, submitted
	}

	t.Run("default: nothing is kept", func(t *testing.T) {
		h := newHarness(t, withDeps(func(d *Deps) { d.Policy, d.Retainer = nil, retainer }))
		mid := make(chan int, 1)
		h.stt.Script("book a table")
		h.captureStart()
		h.expect("capture.started")
		h.sendAudio(tone(100 * time.Millisecond))
		time.Sleep(50 * time.Millisecond)
		mid <- h.sess.BufferedAudioBytes()
		if n := <-mid; n != 0 {
			t.Errorf("the session buffered %d bytes of audio mid-capture for a tenant that does not retain it", n)
		}
		h.send(map[string]any{"type": "capture.cancel"})
		h.expect("capture.cancelled")

		streamed, submitted := run(t, h)
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n != 0 {
			t.Errorf("the retainer was called %d times for a tenant that does not retain audio", n)
		}
		if n := h.sess.BufferedAudioBytes(); n != 0 {
			t.Errorf("the session still holds %d bytes of audio", n)
		}
		logs := h.logs.String()
		for name, pcm := range map[string][]byte{"streamed": streamed, "submitted": submitted} {
			if strings.Contains(logs, base64Of(pcm)[:64]) || containsBytes(logs, pcm[:64]) {
				t.Errorf("%s audio is in the log", name)
			}
		}
		if strings.Contains(logs, "book a table") {
			t.Error("a transcript is in the log")
		}
		if got := h.stt.AudioBytesReceived(); got == 0 {
			t.Error("the recogniser received no audio")
		}
	})

	t.Run("a tenant that retains gets its audio kept", func(t *testing.T) {
		mu.Lock()
		got = nil
		mu.Unlock()
		scope := testScope("ses-retain")
		scope.TenantID = "tnt-retains"
		h := newHarness(t, withDeps(func(d *Deps) { d.Policy, d.Retainer = policy, retainer }))
		h.open(scope, Options{Turns: h.turns})

		streamed := fake.Tone(pcm16k, 300*time.Millisecond, 12345)
		h.stt.Script("book a table")
		h.captureStart()
		h.expect("capture.started")
		h.sendAudio(streamed)
		waitUntil(t, "the capture's audio to be buffered", func() bool { return h.sess.BufferedAudioBytes() == len(streamed) })
		h.send(map[string]any{"type": "capture.end"})
		h.expect("response")
		submitted := fake.Tone(pcm16k, 300*time.Millisecond, 23456)
		h.stt.Script("and a window seat")
		h.submit(submitted)
		h.expect("response")

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 2 {
			t.Fatalf("retained %d utterances, want 2", len(got))
		}
		if !bytes.Equal(got[0].a.PCM, streamed) || !bytes.Equal(got[1].a.PCM, submitted) {
			t.Error("the retainer was not handed the audio of each utterance, whole and in order")
		}
		if got[0].scope.TenantID != "tnt-retains" || got[0].a.Format != pcm16k {
			t.Errorf("retained under %+v", got[0])
		}
		waitUntil(t, "the buffer to be released", func() bool { return h.sess.BufferedAudioBytes() == 0 })
	})

	t.Run("fail closed", func(t *testing.T) {
		// A tenant that retains and a deployment with nowhere to put it: no session.
		h := newHarness(t)
		svc, err := New(Deps{Providers: h.reg, Policy: policy, Clock: h.clk})
		if err != nil {
			t.Fatal(err)
		}
		scope := testScope("ses-x")
		scope.TenantID = "tnt-retains"
		_, err = svc.Open(t.Context(), scope, Options{Turns: h.turns})
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("Open for a retaining tenant with no retainer = %v", err)
		}
		// A policy that cannot answer refuses too.
		svc, _ = New(Deps{Providers: h.reg, Clock: h.clk, Policy: AudioPolicyFunc(func(_ context.Context, _ contracts.Scope) (bool, error) {
			return false, contracts.New(contracts.ErrInternal, "settings store down")
		})})
		if _, err = svc.Open(t.Context(), testScope("ses-x"), Options{Turns: h.turns}); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("Open with an unreadable policy = %v", err)
		}
	})

	t.Run("a retention failure drops the utterance", func(t *testing.T) {
		scope := testScope("ses-retain-2")
		scope.TenantID = "tnt-retains"
		failing := retainerFunc(func(contracts.Scope, RetainedAudio) error {
			return contracts.New(contracts.ErrUnavailable, "disk full")
		})
		h := newHarness(t, withDeps(func(d *Deps) { d.Policy, d.Retainer = policy, failing }))
		h.open(scope, Options{Turns: h.turns})
		h.stt.Script("book a table")
		h.submit(tone(200 * time.Millisecond))
		wantCode(t, h.expect("error"), "unavailable")
		if ev := h.expect("capture.cancelled"); ev.str("reason") != "retention_failed" {
			t.Errorf("capture.cancelled = %s", ev.Raw)
		}
		if n := len(h.turns.Calls()); n != 0 {
			t.Errorf("an utterance that could not be retained ran a turn")
		}
	})

	t.Run("the configuration decides per tenant", func(t *testing.T) {
		c := Config{RetainRawAudio: true, DiscardTenants: []string{"tnt-private"}}
		if c.retains("tnt-private") || !c.retains("tnt-other") {
			t.Error("a discard tenant retains, or the default does not apply")
		}
		c = Config{RetainTenants: []string{"tnt-a"}}
		if !c.retains("tnt-a") || c.retains("tnt-b") {
			t.Error("a retain tenant does not retain, or another does")
		}
		if (Config{}).retains("tnt-a") {
			t.Error("the zero configuration retains audio")
		}
		if CurrentConfig().retains("tnt-a") {
			t.Error("the default configuration retains audio")
		}
	})

	t.Run("the session caps are counts", func(t *testing.T) {
		for _, c := range []Config{{MaxSessions: -1}, {MaxSessionsPerTenant: -1}, {MaxSessionsPerPrincipal: -1}} {
			if c.Validate() == nil {
				t.Errorf("%+v was accepted", c)
			}
		}
		if err := (Config{MaxSessions: 8, MaxSessionsPerTenant: 4, MaxSessionsPerPrincipal: 2}).Validate(); err != nil {
			t.Errorf("valid caps: %v", err)
		}
		// A share larger than the whole is held to the whole.
		svc, err := New(Deps{Providers: providers.NewRegistry(clock.NewFake(testNow)), MaxSessions: 3, MaxSessionsPerTenant: 9, MaxSessionsPerPrincipal: 9})
		if err != nil {
			t.Fatal(err)
		}
		if svc.d.MaxSessionsPerTenant != 3 || svc.d.MaxSessionsPerPrincipal != 3 {
			t.Errorf("caps = %d per tenant, %d per principal, want 3 and 3", svc.d.MaxSessionsPerTenant, svc.d.MaxSessionsPerPrincipal)
		}
		if svc, _ := New(Deps{Providers: providers.NewRegistry(clock.NewFake(testNow))}); svc.d.MaxSessionsPerTenant != 2 || svc.d.MaxSessionsPerPrincipal != 1 {
			t.Errorf("default caps = %d per tenant, %d per principal, want 2 and 1", svc.d.MaxSessionsPerTenant, svc.d.MaxSessionsPerPrincipal)
		}
	})
}

// Section 18: a failed speech component degrades to text, never to nothing. (The
// text path here is the scripted runner; the real engine's text turn on the same
// server is exercised in TestVoiceUnavailableWithoutTheService in internal/gateway.)
func TestSTTDownTextStillWorks(t *testing.T) {
	t.Run("streaming capture", func(t *testing.T) {
		h := newHarness(t)
		h.stt.FailNext(fake.FailUnavailable)
		h.captureStart()
		ev := h.expect("error")
		wantCode(t, ev, "unavailable")
		if ev.str("effect") != "text_input_only" {
			t.Errorf("effect = %q, want text_input_only: %s", ev.str("effect"), ev.Raw)
		}
		// Text on the same session still works, and so does voice once the
		// recogniser is back.
		if _, err := h.turns.RunTextTurn(t.Context(), testScope("ses-voice-1"), "book a table for two", ""); err != nil {
			t.Fatalf("a text turn after the recogniser failed: %v", err)
		}
		h.utterance("book a table for four")
		if got := h.turns.Session("ses-voice-1"); len(got) != 2 {
			t.Errorf("session state = %v", got)
		}
	})

	t.Run("a recognition that fails mid-stream yields no transcript", func(t *testing.T) {
		h := newHarness(t)
		h.stt.FailNext(fake.FailPartialStream)
		h.stt.Script("book a table for four")
		h.captureStart()
		h.expect("capture.started")
		h.sendAudio(tone(100 * time.Millisecond))
		h.sendAudio(tone(100 * time.Millisecond))
		h.send(map[string]any{"type": "capture.end"})
		ev, _ := h.expectWith("error")
		if ev.str("effect") != "text_input_only" {
			t.Errorf("error = %s", ev.Raw)
		}
		if c := h.expect("capture.cancelled"); c.str("reason") != "stt_failed" {
			t.Errorf("capture.cancelled = %s", c.Raw)
		}
		if n := len(h.turns.Calls()); n != 0 {
			t.Errorf("a partial transcript ran a turn")
		}
		waitUntil(t, "the recognition stream to be released", func() bool { return h.stt.OpenStreams() == 0 })
	})

	t.Run("submitted audio", func(t *testing.T) {
		h := newHarness(t)
		h.stt.FailNext(fake.FailTimeout)
		h.submit(tone(200 * time.Millisecond))
		ev, _ := h.expectWith("error")
		wantCode(t, ev, "timeout")
		if ev.str("effect") != "text_input_only" {
			t.Errorf("effect = %q", ev.str("effect"))
		}
		if c := h.expect("capture.cancelled"); c.str("reason") != "stt_failed" {
			t.Errorf("capture.cancelled = %s", c.Raw)
		}
		h.utterance("book a table for four") // the session carries on
	})

	t.Run("no recogniser at all", func(t *testing.T) {
		h := newHarness(t)
		empty := providers.NewRegistry(h.clk)
		svc, err := New(Deps{Providers: empty, Clock: h.clk})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.Open(t.Context(), testScope("ses-voice-1"), Options{Turns: h.turns})
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("Open without STT = %v", err)
		}
		var ce *contracts.Error
		if !asContractsError(err, &ce) || ce.Details["effect"] != providers.EffectTextInputOnly {
			t.Errorf("the refusal does not carry the text_input_only effect: %v", err)
		}
		// Text is a different path and does not notice.
		if _, err := h.turns.RunTextTurn(t.Context(), testScope("ses-voice-1"), "book a table", ""); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("synthesis down: the text is still delivered", func(t *testing.T) {
		h := newHarness(t)
		h.tts.FailNext(fake.FailUnavailable)
		h.stt.Script("when is the doctor free")
		h.submit(tone(200 * time.Millisecond))
		resp := h.expect("response")
		if resp.str("speech") != "queued" || resp.str("text") == "" {
			t.Fatalf("response = %s", resp.Raw)
		}
		ev := h.expect("error")
		if ev.str("effect") != "verified_text_only" || ev.str("turn_id") != resp.str("turn_id") {
			t.Errorf("error = %s", ev.Raw)
		}
		if c := h.expect("playback.cancelled"); c.str("reason") != "error" {
			t.Errorf("playback.cancelled = %s", c.Raw)
		}
	})

	t.Run("no synthesis role: speech is off and the session says so", func(t *testing.T) {
		h := newHarness(t, noTTS())
		h.stt.Script("when is the doctor free")
		h.submit(tone(200 * time.Millisecond))
		resp := h.expect("response")
		if resp.str("speech") != "skipped" || resp.str("speech_reason") != "tts_unavailable" {
			t.Errorf("response = %s", resp.Raw)
		}
	})
}

// Requirement 9: speaker recognition may improve convenience and nothing more.
// Whatever a speaker hint claims, the utterance runs under the verified token's
// scope, and the hint is reported as a hint.
// SEC-094
func TestSpeakerRecognitionNeverAuthorizes(t *testing.T) {
	claims := []struct {
		name string
		hint SpeakerHint
	}{
		{"another principal", SpeakerHint{Label: "usr-admin-0001", Confidence: 0.99}},
		{"the session's own principal", SpeakerHint{Label: "usr-jane-4410", Confidence: 1}},
		{"an administrator", SpeakerHint{Label: "admin", Confidence: 1}},
	}
	for _, c := range claims {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withDeps(func(d *Deps) {
				d.Speaker = speakerFunc(func(voice.AudioFormat, []byte) (SpeakerHint, error) { return c.hint, nil })
			}))
			evs := h.utterance("delete every appointment")
			var final event
			for _, ev := range evs {
				if ev.Type == "transcript.final" {
					final = ev
				}
			}
			hint, _ := final.M["speaker_hint"].(map[string]any)
			if hint["label"] != c.hint.Label {
				t.Errorf("the hint is not reported beside the transcript: %s", final.Raw)
			}
			calls := h.turns.Calls()
			if len(calls) != 1 {
				t.Fatalf("%d turns", len(calls))
			}
			if diff := cmp.Diff(testScope("ses-voice-1"), calls[0].Scope); diff != "" {
				t.Errorf("a speaker hint changed the scope of the turn (-want +got):\n%s", diff)
			}
			if calls[0].Text != "delete every appointment" {
				t.Errorf("the turn ran %q: the hint is not part of the message", calls[0].Text)
			}
		})
	}

	t.Run("a hint that is out of range, or a recogniser that fails, is no hint", func(t *testing.T) {
		for name, rec := range map[string]SpeakerRecognizer{
			"confidence over one": speakerFunc(func(voice.AudioFormat, []byte) (SpeakerHint, error) {
				return SpeakerHint{Label: "x", Confidence: 7}, nil
			}),
			"empty label": speakerFunc(func(voice.AudioFormat, []byte) (SpeakerHint, error) { return SpeakerHint{Confidence: 0.5}, nil }),
			"failure": speakerFunc(func(voice.AudioFormat, []byte) (SpeakerHint, error) {
				return SpeakerHint{}, contracts.New(contracts.ErrUnavailable, "down")
			}),
		} {
			h := newHarness(t, withDeps(func(d *Deps) { d.Speaker = rec }))
			evs := h.utterance("book a table")
			for _, ev := range evs {
				if _, has := ev.M["speaker_hint"]; has && ev.Type == "transcript.final" {
					t.Errorf("%s: a hint was reported: %s", name, ev.Raw)
				}
			}
			if len(h.turns.Calls()) != 1 {
				t.Errorf("%s: the turn did not run", name)
			}
		}
	})

	t.Run("no message of the client carries an identity", func(t *testing.T) {
		h := newHarness(t)
		h.send(map[string]any{"type": "capture.start", "format": wireFormat(pcm16k), "principal_id": "usr-admin-0001"})
		wantCode(t, h.expect("error"), "validation_failed")
		h.send(map[string]any{"type": "capture.start", "format": wireFormat(pcm16k), "speaker": "usr-admin-0001"})
		wantCode(t, h.expect("error"), "validation_failed")
		if len(h.turns.Calls()) != 0 {
			t.Error("a turn ran")
		}
	})
}

// ADR 0039: an STT-only pack serves dictation, not the voice-conversation WebSocket.
func TestOpenRefusesSTTOnlyPack(t *testing.T) {
	h := newHarness(t)
	sttOnly := profile.Pack{ID: "en-stt", Language: "en", Roles: []providers.Role{providers.RoleSTT}}
	packs, err := profile.NewPacks(profile.NewMemoryCatalog(sttOnly), profile.NopLoader{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := packs.Activate(t.Context(), "en-stt"); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Deps{Providers: h.reg, Packs: packs, Clock: h.clk})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Open(t.Context(), testScope("ses-1"), Options{Turns: h.turns})
	if contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("Open STT-only pack = %v, want unavailable", err)
	}
}

// Requirement 7 and 8: a language pack is activated explicitly, and voice is
// absent from a text-only deployment. A pack taken away cancels the streams
// that are live, which the profile's gate (checked at open) does not.
func TestLanguagePackGatesAndCancelsStreams(t *testing.T) {
	t.Run("no session without an active pack", func(t *testing.T) {
		h := newHarness(t)
		packs := newTestPacks(t)
		svc, err := New(Deps{Providers: h.reg, Packs: packs, Clock: h.clk})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.Open(t.Context(), testScope("ses-1"), Options{Turns: h.turns})
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("Open with no active pack = %v", err)
		}
		if _, err := svc.Packs().Activate(t.Context(), "en"); err != nil {
			t.Fatal(err)
		}
		sess, err := svc.Open(t.Context(), testScope("ses-1"), Options{Turns: h.turns})
		if err != nil {
			t.Fatalf("Open with an active pack: %v", err)
		}
		sess.Close(ReasonClientClosed)
	})

	// A new utterance cancels the speech that is playing, so a capture and a
	// playback are never live together: each is cancelled on its own.
	deactivate := func(t *testing.T) *harness {
		packs := newTestPacks(t)
		if _, err := packs.Activate(t.Context(), "en"); err != nil {
			t.Fatal(err)
		}
		return newHarness(t, withDeps(func(d *Deps) { d.Packs = packs }))
	}
	stays := func(t *testing.T, h *harness) {
		t.Helper()
		waitUntil(t, "the streams to be released", func() bool { return h.tts.OpenStreams() == 0 && h.stt.OpenStreams() == 0 })
		// The session itself stays: text keeps working.
		if _, err := h.turns.RunTextTurn(t.Context(), testScope("ses-voice-1"), "book a table", ""); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-h.done:
			t.Errorf("the session closed (%s) when its pack was deactivated", r)
		default:
		}
	}

	t.Run("deactivating the pack cancels a live playback", func(t *testing.T) {
		h := deactivate(t)
		long := strings.Repeat("I can book appointments for you and check the schedule. ", 12)
		h.turns.reply = func(int, string) (dialogue.Result, error) { return verifiedResult(long), nil }
		h.tts.StallAfter(1) // a runtime that never ends the stream by itself
		h.stt.Script("what can you do")
		h.submit(tone(200 * time.Millisecond))
		h.expect("playback.started")

		if err := h.svc.Packs().Deactivate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if ev := h.expect("playback.cancelled"); ev.str("reason") != "pack_changed" {
			t.Errorf("playback.cancelled = %s", ev.Raw)
		}
		stays(t, h)
	})

	t.Run("deactivating the pack cancels a live capture", func(t *testing.T) {
		h := deactivate(t)
		h.stt.Script("book a table")
		h.captureStart()
		h.expect("capture.started")
		h.sendAudio(tone(100 * time.Millisecond))

		if err := h.svc.Packs().Deactivate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if ev := h.expect("capture.cancelled"); ev.str("reason") != "pack_changed" {
			t.Errorf("capture.cancelled = %s", ev.Raw)
		}
		stays(t, h)
	})

	t.Run("a pack removed behind the service's back is noticed at the next chunk", func(t *testing.T) {
		packs := newTestPacks(t)
		if _, err := packs.Activate(t.Context(), "en"); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, withDeps(func(d *Deps) { d.Packs = packs }))
		h.stt.Script("book a table")
		h.captureStart()
		h.expect("capture.started")
		h.sendAudio(tone(100 * time.Millisecond))
		if err := packs.Deactivate(t.Context()); err != nil { // not through Service.Packs
			t.Fatal(err)
		}
		h.sendAudio(tone(100 * time.Millisecond))
		if ev := h.expect("capture.cancelled"); ev.str("reason") != "pack_changed" {
			t.Errorf("capture.cancelled = %s", ev.Raw)
		}
		waitUntil(t, "the recognition stream to be released", func() bool { return h.stt.OpenStreams() == 0 })
	})
}

// Requirement 8: a text-only deployment carries no voice cost, and a deployment
// with voice pays nothing until a session opens.
func TestTextOnlyHasNoVoiceWorkers(t *testing.T) {
	clk := clock.NewFake(testNow)
	g0 := runtime.NumGoroutine()
	// A text-only deployment: no voice role is registered, so there is no
	// session to open and nothing that ever started.
	svc, err := New(Deps{Providers: providers.NewRegistry(clk), Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Open(t.Context(), testScope("ses-1"), Options{Turns: newFakeTurns(t, nil)})
	var ce *contracts.Error
	if !asContractsError(err, &ce) || ce.Code != contracts.ErrUnavailable || ce.Details["effect"] != providers.EffectTextInputOnly {
		t.Fatalf("Open on a text-only deployment = %v", err)
	}
	if svc.Live() != 0 || runtime.NumGoroutine() > g0 {
		t.Errorf("a text-only deployment holds %d sessions and %d extra goroutines", svc.Live(), runtime.NumGoroutine()-g0)
	}

	// With voice: opening registers but starts nothing, serving starts the one
	// worker, and closing ends everything.
	h := newHarness(t)
	g1 := runtime.NumGoroutine()
	other := testScope("ses-open-only")
	other.PrincipalID = "usr-sam-5520" // one session per user: the harness's own is Jane's
	sess, err := h.svc.Open(t.Context(), other, Options{Turns: h.turns})
	if err != nil {
		t.Fatal(err)
	}
	if g := runtime.NumGoroutine(); g > g1 {
		t.Errorf("Open started %d goroutines", g-g1)
	}
	sess.Close(ReasonClientClosed)
	if h.svc.Live() != 1 {
		t.Errorf("%d live sessions after an unserved one was closed, want the harness's own", h.svc.Live())
	}
	h.close()
	waitUntil(t, "no live sessions", func() bool { return h.svc.Live() == 0 })
	waitUntil(t, "the session goroutines to end", func() bool { return runtime.NumGoroutine() <= g0 })
}
