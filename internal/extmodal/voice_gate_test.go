package extmodal

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// NewSTT does not assume that the session cut the audio with the local VAD: raw audio goes out only for a call whose
// context says so, and everything else stays local.
func TestNewSTTSendsRawAudioOnlyWhenTheCallSaysTheVADCutIt(t *testing.T) {
	r := newRig(t)
	r.on(true, ModSTT)
	r.stt.Script("book me a visit")
	req := voice.TranscribeRequest{Audio: pcm(32000), Format: pcm16k}
	gate := NewSTT(r.svc, r.target, nil, r.stt)

	_, err := gate.Transcribe(context.Background(), scopeA, req)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonAudioNotSegmented)
	if r.stt.CallCount() != 0 || r.stt.AudioBytesReceived() != 0 {
		t.Fatalf("unmarked audio reached the provider: %d calls, %d bytes", r.stt.CallCount(), r.stt.AudioBytesReceived())
	}

	tr, err := gate.Transcribe(WithVADSegmented(context.Background()), scopeA, req)
	if err != nil || tr.Text != "book me a visit" || r.stt.CallCount() != 1 {
		t.Fatalf("marked audio: %+v, %v, %d calls", tr, err, r.stt.CallCount())
	}

	// The marker is per call: the plain context of the next call is unmarked again.
	if _, err := gate.Transcribe(context.Background(), scopeA, req); err == nil || r.stt.CallCount() != 1 {
		t.Fatalf("the marker leaked into another call: %v, %d calls", err, r.stt.CallCount())
	}
}

// Dictation sets WithUserSegmented instead of VAD: the host's record/stop is the cut.
func TestNewSTTSendsRawAudioWhenTheCallSaysTheUserRecordedIt(t *testing.T) {
	r := newRig(t)
	r.on(true, ModSTT)
	r.stt.Script("hello from dictation")
	req := voice.TranscribeRequest{Audio: pcm(32000), Format: pcm16k}
	gate := NewSTT(r.svc, r.target, nil, r.stt)

	_, err := gate.Transcribe(context.Background(), scopeA, req)
	wantCode(t, err, contracts.ErrPermissionDenied, ReasonAudioNotSegmented)

	ctx := WithUserSegmented(WithAudioClass(context.Background(), "internal"))
	tr, err := gate.Transcribe(ctx, scopeA, req)
	if err != nil || tr.Text != "hello from dictation" || r.stt.CallCount() != 1 {
		t.Fatalf("user-segmented audio: %+v, %v, %d calls", tr, err, r.stt.CallCount())
	}
	if !UserSegmentedForTest(ctx) || VADSegmentedForTest(ctx) || AudioClassForTest(ctx) != "internal" {
		t.Fatalf("markers: user=%v vad=%v class=%q", UserSegmentedForTest(ctx), VADSegmentedForTest(ctx), AudioClassForTest(ctx))
	}
}

// A piece of one attested reply is speakable; text spliced from two replies, or text the reply does not contain, is not.
func TestAttestedPiecesAreContiguousPiecesOfOneReply(t *testing.T) {
	r := newRig(t)
	if err := r.svc.Attest(scopeA, verifiedOutcome("Your visit is booked. See you on Tuesday.")); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Attest(scopeA, verifiedOutcome("The fee is paid at the desk.")); err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]bool{
		"Your visit is booked.":                 true,
		" See you on Tuesday. ":                 true,
		"The fee is paid at the desk.":          true,
		"Your visit is booked. The fee is paid": false,
		"Your visit is booked for free.":        false,
		"":                                      false,
		"   ":                                   false,
		"See you on Tuesday. Your visit is booked.": false,
	} {
		if got := r.svc.Verified(scopeA, text); got != want {
			t.Errorf("Verified(%q) = %v, want %v", text, got, want)
		}
	}
	other := scopeA
	other.SessionID = "ses_other"
	if r.svc.Verified(other, "Your visit is booked.") {
		t.Error("a reply attested in one session is speakable in another")
	}
}
