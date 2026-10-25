package session

import (
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	voicewire "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// correction says what a corrected transcript replaces.
type correction struct {
	turnID string
	prepID string
}

// runTurn runs a final transcript through the normal turn pipeline, on the
// worker. It is the only way voice reaches the engine, and it goes through
// TurnRunner, which carries text and nothing else: a transcript can start a
// goal, answer a question or correct a value, but never confirm, reject or
// choose. What comes back is reported to the client, and the response is spoken
// only when it passes the checks of speakable.
func (s *Session) runTurn(u *utterance, text string, gen uint64, corr *correction) {
	res, err := s.opts.Turns.RunTextTurn(s.ctx, s.scope.Clone(), text, s.turnLanguage(u.lang))
	if err != nil {
		// A turn that could not run, or could not be reported whole: a typed
		// error, and nothing is spoken (the response of a turn that failed to
		// be delivered is not something to say aloud).
		if s.closing() {
			return
		}
		s.emitError(err, u.id, res.TurnID)
		return
	}
	prep := ""
	if res.Prepared != nil {
		prep = string(res.Prepared.PreparationID)
	}
	s.mu.Lock()
	u.turnID, u.prepID = res.TurnID, prep
	s.mu.Unlock()

	tc := voicewire.TurnCompleted{
		UtteranceID: voicewire.UtteranceID(u.id),
		TurnID:      actions.TurnID(res.TurnID),
		Act:         string(res.Directive.Act),
		Failed:      res.Failed(),
	}
	if c := common.ErrorCode(res.Code); res.Code != "" && c.Valid() {
		tc.Code = &c
	}
	if res.Prepared != nil {
		pid := res.Prepared.PreparationID
		tc.PreparationID = &pid
		// The single-use secret goes to the client's own confirm control, in this
		// event, and nowhere else: not into speech, not into a log.
		if !res.Confirmation.IsZero() {
			tc.Confirmation = &voicewire.TurnCompletedConfirmation{PreparationID: pid, Token: res.Confirmation.Reveal()}
		}
	}
	if corr != nil {
		prev := actions.TurnID(corr.turnID)
		tc.CorrectsTurnID = &prev
		if corr.prepID != "" && corr.prepID != prep {
			old := actions.PreparationID(corr.prepID)
			tc.SupersededPreparationID = &old
		}
	}
	s.emit(tc)

	text = strings.TrimSpace(res.Response.Text)
	if text == "" {
		s.emitError(contracts.New(contracts.ErrInternal, "the turn produced no response text"), u.id, res.TurnID)
		return
	}
	resp := voicewire.Response{
		UtteranceID: voicewire.UtteranceID(u.id),
		TurnID:      actions.TurnID(res.TurnID),
		Text:        clipRunes(text, 4096),
		Source:      voicewire.ResponseSource(res.Response.Source),
		Speech:      voicewire.ResponseSpeechSkipped,
	}
	if !resp.Source.Valid() {
		s.emitError(contracts.New(contracts.ErrInternal, "the turn's response has no verified source"), u.id, res.TurnID)
		return
	}
	skip := func(r voicewire.ResponseSpeechReason) { resp.SpeechReason = &r }
	var pb *playback
	switch {
	case s.opts.NoSpeech:
		skip(voicewire.ResponseSpeechReasonSpeechDisabled)
	case s.tts == nil:
		skip(voicewire.ResponseSpeechReasonTTSUnavailable)
	case !speakable(res):
		skip(voicewire.ResponseSpeechReasonVerificationFailed)
	default:
		var ok bool
		if pb, ok = s.reservePlayback(res.TurnID, gen); ok {
			resp.Speech = voicewire.ResponseSpeechQueued
		} else {
			// A newer utterance began while the turn ran: its speech is stale.
			skip(voicewire.ResponseSpeechReasonSuperseded)
		}
	}
	s.emit(resp)
	if pb != nil {
		s.wg.Add(1)
		go s.runPlayback(pb, text, string(res.Language))
	}
}

// turnLanguage is the language to ask the turn for: the client's tag when the
// core supports it, otherwise "" (the core chooses). The language the
// recogniser reported is never used: a tag the pipeline does not support would
// fail the turn.
func (s *Session) turnLanguage(hint string) string {
	if hint == "" {
		hint = s.opts.Language
	}
	if hint == "" {
		return ""
	}
	if _, err := verify.ParseLanguage(hint); err != nil {
		return ""
	}
	return hint
}

// speakable reports whether the response of a turn may be spoken (master plan
// 13: "TTS begins only after material facts are verified"). Speech starts from
// a verify.Outcome and from nothing else, and only from one whose provenance is
// coherent: the model's text that passed the verifier, or the deterministic
// template. The model's text is checked once more here against the directive
// and the receipt the turn produced, so speech does not rest on the engine
// alone. (The check has no field labels of the application: where the engine
// accepted text that uses one, speech is declined rather than risked, and the
// text is still shown.)
func speakable(res dialogue.Result) bool {
	out := res.Response
	if strings.TrimSpace(out.Text) == "" {
		return false
	}
	switch {
	case out.Source == verify.SourceModel && out.Reason == verify.ReasonVerified:
		v, err := verify.Verify(out.Text, &res.Directive, res.Receipt, verify.Options{Language: res.Language})
		return err == nil && v.OK
	case out.Source == verify.SourceTemplate && (out.Reason == verify.ReasonVerificationFailed || out.Reason == verify.ReasonDegraded):
		return true
	}
	return false
}

// onTranscriptCorrect replaces the transcript of the latest utterance with the
// user's corrected text and runs it as a new message on the same session.
// Nothing about the earlier transcript is amended in place: the pipeline's own
// correction rules void a preparation that was built on the old values, so the
// action the user would have confirmed is no longer the one on offer. A
// correction cannot touch what is already committed, because a commit is a
// receipt and the corrected text is only ever a new request.
func (s *Session) onTranscriptCorrect(m voicewire.TranscriptCorrect) {
	text := strings.TrimSpace(m.Text)
	id := string(m.UtteranceID)
	switch {
	case !ids.Valid(id, prefixUtterance):
		s.violation(contracts.New(contracts.ErrValidationFailed, "the utterance id is malformed"))
		return
	case text == "" || utf8.RuneCountInString(text) > maxTranscriptRunes:
		s.violation(contracts.Newf(contracts.ErrValidationFailed, "a corrected transcript is 1 to %d characters", maxTranscriptRunes))
		return
	}
	s.mu.Lock()
	var u *utterance
	for _, x := range s.utts {
		if x.id == id {
			u = x
		}
	}
	latest := len(s.utts) > 0 && s.utts[len(s.utts)-1] == u
	s.mu.Unlock()
	switch {
	case u == nil:
		s.violation(contracts.New(contracts.ErrValidationFailed, "the utterance is unknown to this session").WithDetails("reason", "unknown_utterance"))
		return
	case !latest:
		s.violation(contracts.New(contracts.ErrStale, "a newer utterance exists; only the latest transcript can be corrected").WithDetails("reason", "not_latest"))
		return
	}

	gen := s.beginUtterance() // the correction is the user speaking up: speech in progress stops
	if !s.enqueue(func() { s.applyCorrection(u, text, gen) }) {
		s.violation(contracts.New(contracts.ErrRateLimited, "voice: too many utterances are waiting for the pipeline"))
	}
}

// applyCorrection runs on the worker, behind the turn of the utterance it
// corrects, so what that turn prepared is known.
func (s *Session) applyCorrection(u *utterance, text string, gen uint64) {
	s.mu.Lock()
	prev, corr := u.text, correction{turnID: u.turnID, prepID: u.prepID}
	s.mu.Unlock()
	if corr.turnID == "" {
		// The utterance's own turn did not produce a result (it failed): there is
		// nothing to supersede, and the corrected text starts afresh.
		corr = correction{}
	}
	if text == prev {
		s.emitError(contracts.New(contracts.ErrValidationFailed, "the correction does not change the transcript"), u.id, "")
		return
	}
	s.mu.Lock()
	u.text = text
	s.mu.Unlock()
	s.emit(voicewire.TranscriptCorrected{UtteranceID: voicewire.UtteranceID(u.id), Text: text, PreviousText: prev})
	var cp *correction
	if corr.turnID != "" {
		cp = &corr
	}
	s.runTurn(u, text, gen, cp)
}
