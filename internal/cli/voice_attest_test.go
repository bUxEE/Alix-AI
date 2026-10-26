//go:build !noexternal

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/verify"
	vsession "github.com/buxee/alix-ai/internal/voice/session"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ---- the hook the serve engine builder installs ----

// attestCapturedHook runs the wrapped engine builder over a stub and returns the hook it installed.
func attestCapturedHook(t *testing.T, preset dialogue.ReplyHook) dialogue.ReplyHook {
	t.Helper()
	var got dialogue.ReplyHook
	build := attestEngineWithHook(func(d dialogue.Deps) (*dialogue.Engine, error) {
		got = d.OnReply
		return nil, nil
	})
	if _, err := build(dialogue.Deps{OnReply: preset}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAttestEngineBuilderInstallsTheHookOnlyWhereNoneIsSet(t *testing.T) {
	if attestCapturedHook(t, nil) == nil {
		t.Fatal("the serve engine builder installs no reply hook")
	}
	called := false
	own := func(context.Context, contracts.Scope, verify.Outcome) { called = true }
	attestCapturedHook(t, own)(context.Background(), contracts.Scope{}, verify.Outcome{})
	if !called {
		t.Fatal("a Deps that already has a hook lost it")
	}
	// serveProfileNewEngine is wrapped by this file's init: a Deps built through it carries the hook.
	if serveProfileNewEngine == nil {
		t.Fatal("no engine builder")
	}
}

func TestAttestOnReplyAttestsOnlyVerifiedRepliesOfTheirOwnSession(t *testing.T) {
	const reply = "Your visit is booked for Tuesday."
	r := extFinalNewVoiceRig(t, modes.ModeHybrid)
	hook := attestCapturedHook(t, nil)
	ctx := context.Background()

	hook(ctx, r.scope, verify.Outcome{Text: reply, Source: verify.SourceModel, Reason: verify.ReasonVerified})
	if !r.v.svc.Verified(r.scope, reply) {
		t.Fatal("a verified reply was not attested")
	}
	other := r.scope
	other.SessionID = "ses_other"
	if r.v.svc.Verified(other, reply) {
		t.Fatal("an attestation reached another session")
	}

	// An outcome with no provenance, or one that says it was not verified, is never attested.
	for name, out := range map[string]verify.Outcome{
		"zero":                   {},
		"model text, degraded":   {Text: "I can book that for free.", Source: verify.SourceModel, Reason: verify.ReasonDegraded},
		"model text, unverified": {Text: "I can book that for free.", Source: verify.SourceModel, Reason: verify.ReasonVerificationFailed},
		"template as verified":   {Text: "I can book that for free.", Source: verify.SourceTemplate, Reason: verify.ReasonVerified},
		"unknown source":         {Text: "I can book that for free.", Source: "tool", Reason: verify.ReasonVerified},
	} {
		hook(ctx, r.scope, out)
		if out.Text != "" && r.v.svc.Verified(r.scope, out.Text) {
			t.Fatalf("%s: became speakable", name)
		}
	}

	// With the voice stack not composed (external mode off) the hook does nothing and does not panic.
	extFinalVoiceHolder.Store(nil)
	hook(ctx, r.scope, verify.Outcome{Text: reply, Source: verify.SourceModel, Reason: verify.ReasonVerified})
}

// ---- the voice session over the external speech gates ----

type attestTransport struct {
	in, out chan attestFrame
}

type attestFrame struct {
	binary bool
	data   []byte
}

func (c *attestTransport) Read(ctx context.Context) (bool, []byte, error) {
	select {
	case f := <-c.in:
		return f.binary, f.data, nil
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
}

func (c *attestTransport) Write(ctx context.Context, binary bool, data []byte) error {
	select {
	case c.out <- attestFrame{binary: binary, data: append([]byte(nil), data...)}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attestTurns stands in for the dialogue engine: it builds the result of a turn and, like the engine, announces the
// reply of an acknowledged turn to the hook the serve engine builder installed.
type attestTurns struct {
	hook  dialogue.ReplyHook
	reply func() dialogue.Result
	// after, when set, changes the result after the hook has seen it: a defect injected after the engine.
	after func(*dialogue.Result)
}

func (a *attestTurns) RunTextTurn(ctx context.Context, scope contracts.Scope, _, _ string) (dialogue.Result, error) {
	res := a.reply()
	if a.hook != nil {
		a.hook(ctx, scope, res.Response)
	}
	if a.after != nil {
		a.after(&res)
	}
	return res, nil
}

// attestResult is a turn that gave an answer: a directive that carries no fact, and the outcome of verify.Realise.
func attestResult(t *testing.T, text string) dialogue.Result {
	t.Helper()
	turn := ids.New("trn")
	d := conversation.DialogueDirective{
		TurnID: actgen.TurnID(turn), Act: conversation.DialogueDirectiveActOfferHelp,
		Facts: []actgen.Fact{}, MustState: []common.FieldName{}, MissingFields: []common.FieldName{}, InvalidFields: []actgen.Issue{},
	}
	out, err := verify.Realise(text, &d, nil, verify.Options{Language: verify.LangEN})
	if err != nil {
		t.Fatal(err)
	}
	return dialogue.Result{TurnID: turn, Language: verify.LangEN, Directive: d, Response: out}
}

// attestSpeak runs one utterance through a voice session whose synthesis is the external speech gate, with the
// result the engine would return, and reports the response event and whether audio reached the client.
func attestSpeak(t *testing.T, r *extFinalVoiceRig, reply func() dialogue.Result, after ...func(*dialogue.Result)) (resp map[string]any, audio bool) {
	t.Helper()
	reg := providers.NewRegistry(nil)
	if err := voice.Register(reg, providers.RoleSTT, r.locSTT); err != nil {
		t.Fatal(err)
	}
	if err := voice.Register(reg, providers.RoleVAD, vfake.New(vfake.WithName("fake-vad"))); err != nil {
		t.Fatal(err)
	}
	if err := voice.Register(reg, providers.RoleTTS, extFinalTTS{local: r.locTTS}); err != nil {
		t.Fatal(err)
	}
	svc, err := vsession.New(vsession.Deps{Providers: reg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	turns := &attestTurns{hook: attestCapturedHook(t, nil), reply: reply}
	if len(after) > 0 {
		turns.after = after[0]
	}
	sess, err := svc.Open(context.Background(), r.scope, vsession.Options{Turns: turns})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tr := &attestTransport{in: make(chan attestFrame, 16), out: make(chan attestFrame, 1024)}
	done := make(chan struct{})
	go func() { defer close(done); sess.Serve(context.Background(), tr) }()
	t.Cleanup(func() { sess.Close(vsession.ReasonClientClosed); <-done })

	f := voice.PCM16Mono(16000)
	pcm := vfake.Tone(f, 200*time.Millisecond, 6000)
	r.locSTT.Script("book me a visit")
	raw, err := json.Marshal(map[string]any{
		"type":   "audio.submit",
		"format": map[string]any{"encoding": string(f.Encoding), "sample_rate": f.SampleRate, "channels": f.Channels},
		"audio":  base64.StdEncoding.EncodeToString(pcm),
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.in <- attestFrame{data: raw}

	// Read until the speech is over: a skipped or refused one settles after a short quiet time, a queued one
	// ends with playback.done or an error.
	deadline := time.After(5 * time.Second)
	for {
		wait := 5 * time.Second
		if resp != nil && resp["speech"] != "queued" {
			// Nothing was queued: a short quiet time shows that nothing is spoken after all.
			wait = 400 * time.Millisecond
		}
		select {
		case fr := <-tr.out:
			if fr.binary {
				audio = true
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(fr.data, &m); err != nil {
				t.Fatalf("invalid event: %v", err)
			}
			switch m["type"] {
			case "response":
				resp = m
			case "playback.done", "playback.cancelled":
				return resp, audio
			case "error":
				if resp == nil {
					resp = map[string]any{"error": m["error"]}
				}
				return resp, audio
			}
		case <-time.After(wait):
			return resp, audio
		case <-deadline:
			return resp, audio
		}
	}
}

// A text that was not attested is displayed and never spoken: not when the engine let a defect through after
// the reply was attested, not when the outcome has no provenance, not when the model's text fails the verifier. The
// session declines most of these first; the external speech gate refuses the rest, so unattested text never reaches
// the provider and no other provider speaks it instead.
func TestUnattestedTextIsNeverSpoken(t *testing.T) {
	const reply = "I can help you book an appointment."

	t.Run("an attested, verified reply is spoken by the external provider (control)", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeHybrid)
		resp, audio := attestSpeak(t, r, func() dialogue.Result { return attestResult(t, reply) })
		if resp == nil || resp["speech"] != "queued" || resp["text"] != reply {
			t.Fatalf("response = %v", resp)
		}
		if c := r.extTTS.Calls(); len(c) != 1 || c[0].Text != reply || r.locTTS.CallCount() != 0 || !audio {
			t.Fatalf("external %+v, local %d, audio %v", c, r.locTTS.CallCount(), audio)
		}
	})

	t.Run("a defect injected after the engine: the displayed text is not the attested one, and it is not spoken", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeExternal)
		const defect = "I can book that for free."
		resp, audio := attestSpeak(t, r, func() dialogue.Result { return attestResult(t, reply) }, func(res *dialogue.Result) {
			// The hook saw the verified reply; the Result the voice session receives was changed afterwards.
			res.Response.Text = defect
		})
		if resp == nil || resp["text"] != defect {
			t.Fatalf("the text is still displayed: %v", resp)
		}
		if r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 0 || audio {
			t.Fatalf("a text that was not attested was spoken: external %d, local %d, audio %v", r.extTTS.CallCount(), r.locTTS.CallCount(), audio)
		}
		if r.v.svc.Verified(r.scope, defect) {
			t.Fatal("the defect became speakable")
		}
	})

	t.Run("a response with no provenance is displayed or refused and never spoken", func(t *testing.T) {
		for name, out := range map[string]verify.Outcome{
			"model text that says it was degraded": {Text: "I can book that for free.", Source: verify.SourceModel, Reason: verify.ReasonDegraded},
			"template that says it was verified":   {Text: "I can book that for free.", Source: verify.SourceTemplate, Reason: verify.ReasonVerified},
			"no source at all":                     {Text: "I can book that for free."},
		} {
			r := extFinalNewVoiceRig(t, modes.ModeExternal)
			_, audio := attestSpeak(t, r, func() dialogue.Result {
				res := attestResult(t, reply)
				res.Response = out
				return res
			})
			if r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 0 || audio {
				t.Fatalf("%s: spoken (external %d, local %d, audio %v)", name, r.extTTS.CallCount(), r.locTTS.CallCount(), audio)
			}
			if r.v.svc.Verified(r.scope, out.Text) {
				t.Fatalf("%s: became speakable", name)
			}
		}
	})

	t.Run("a model text that fails the verifier shows and is not spoken", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeExternal)
		resp, audio := attestSpeak(t, r, func() dialogue.Result {
			res := attestResult(t, reply)
			res.Directive.Act = conversation.DialogueDirectiveActPresentResult
			res.Directive.Facts = append(res.Directive.Facts, actgen.Fact{Field: "slot_start", Value: actgen.CanonicalValue{Value: actgen.TimeValue{Value: "10:30"}}})
			res.Directive.MustState = append(res.Directive.MustState, "slot_start")
			res.Response = verify.Outcome{Text: "Dr. Rossi is free at 11:45.", Source: verify.SourceModel, Reason: verify.ReasonVerified}
			return res
		})
		if resp == nil || resp["speech"] != "skipped" || !strings.Contains(resp["text"].(string), "11:45") {
			t.Fatalf("response = %v", resp)
		}
		if r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 0 || audio {
			t.Fatalf("spoken: external %d, local %d, audio %v", r.extTTS.CallCount(), r.locTTS.CallCount(), audio)
		}
	})

	t.Run("a reply of an engine whose hook was never installed is not spoken externally", func(t *testing.T) {
		r := extFinalNewVoiceRig(t, modes.ModeExternal)
		if err := r.speak(reply); extmodal.ReasonOf(err) == "" {
			t.Fatalf("an unattested text was not refused: %v", err)
		}
		if r.extTTS.CallCount() != 0 || r.locTTS.CallCount() != 0 {
			t.Fatalf("external %d, local %d", r.extTTS.CallCount(), r.locTTS.CallCount())
		}
	})
}
