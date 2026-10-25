package session

import (
	"context"
	"encoding/base64"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	voicewire "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// Why an utterance or a playback was cancelled (the reason members of
// schemas/voice/server-event).
const (
	reasonClient        = "client"
	reasonBargeIn       = "barge_in"
	reasonNoSpeech      = "no_speech"
	reasonSTTFailed     = "stt_failed"
	reasonInvalidAudio  = "invalid_audio"
	reasonPackChanged   = "pack_changed"
	reasonSessionClosed = "session_closed"
	reasonRetention     = "retention_failed"
	reasonOverloaded    = "overloaded"
	reasonError         = "error"
)

// capture is one streaming utterance: audio in, transcript events out.
type capture struct {
	utt    *utterance
	gen    uint64
	format voice.AudioFormat
	lang   string
	pack   string
	stream voice.STTStream
	cancel context.CancelFunc

	cancelled atomic.Bool

	mu    sync.Mutex // sent, buf and ended: the read loop writes them, the capture goroutine reads buf
	sent  int
	ended bool
	keep  bool   // the audio is wanted after the recognition: retention or speaker recognition
	buf   []byte // raw audio, only when keep
}

// utterance is what the session remembers of one spoken or submitted message,
// so that it can be corrected.
type utterance struct {
	id     string
	text   string // the final transcript, as corrected so far
	lang   string // the recognition language hint the capture named, if any
	turnID string // the turn the transcript ran, once it has run
	prepID string // the preparation that turn left awaiting confirmation, if any
}

// remember records u as the newest utterance.
func (s *Session) remember(u *utterance) {
	s.mu.Lock()
	s.utts = append(s.utts, u)
	if len(s.utts) > keepUtterances {
		s.utts = append([]*utterance(nil), s.utts[len(s.utts)-keepUtterances:]...)
	}
	s.mu.Unlock()
}

// beginUtterance starts a new utterance: it advances the barge-in clock and
// cancels the speech that is playing, at once and before anything else is said
// (the user has begun to speak over it).
func (s *Session) beginUtterance() uint64 {
	s.mu.Lock()
	s.gen++
	gen := s.gen
	s.mu.Unlock()
	s.cancelPlayback(reasonBargeIn)
	return gen
}

func (s *Session) packChanged(pack string) bool {
	if s.svc.d.Packs == nil || pack == "" {
		return false
	}
	p, ok := s.svc.d.Packs.Active()
	return !ok || p.ID != pack
}

func (s *Session) activePack() string {
	if s.svc.d.Packs == nil {
		return ""
	}
	p, _ := s.svc.d.Packs.Active()
	return p.ID
}

// wantsAudio reports whether the audio of an utterance is needed after it was
// recognised.
func (s *Session) wantsAudio() bool { return s.retain || s.svc.d.Speaker != nil }

func (s *Session) onCaptureStart(m voicewire.CaptureStart) {
	format := voice.AudioFormat{Encoding: voice.Encoding(m.Format.Encoding), SampleRate: int(m.Format.SampleRate), Channels: int(m.Format.Channels)}
	if err := format.Validate(); err != nil {
		s.violation(err)
		return
	}
	lang := s.opts.Language
	if m.Language != nil {
		lang = string(*m.Language)
	}
	if lang != "" && !voice.ValidLanguage(lang) {
		s.violation(contracts.Newf(contracts.ErrValidationFailed, "voice: language %q is not a language tag", lang))
		return
	}
	s.mu.Lock()
	busy := s.capture != nil
	s.mu.Unlock()
	if busy {
		s.violation(contracts.New(contracts.ErrConflict, "a capture is already active; end or cancel it first"))
		return
	}

	gen := s.beginUtterance()
	u := s.newUtterance()
	u.lang = lang
	pack := s.activePack()
	ctx, cancel := context.WithCancel(s.ctx)
	stream, err := s.stt.TranscribeStream(ctx, s.scope.Clone(), voice.STTStreamRequest{Format: format, Language: lang})
	if err != nil {
		cancel()
		if s.ctx.Err() == nil {
			s.emitError(withEffect(err, providers.RoleSTT), "", "")
		}
		return
	}
	c := &capture{utt: u, gen: gen, format: format, lang: lang, pack: pack, stream: stream, cancel: cancel, keep: s.wantsAudio()}
	s.mu.Lock()
	s.capture = c
	s.mu.Unlock()
	s.emit(voicewire.CaptureStarted{UtteranceID: voicewire.UtteranceID(u.id)})
	s.wg.Add(1)
	go s.runCapture(c)
}

// onAudio is one binary message: a chunk of the active capture.
func (s *Session) onAudio(data []byte) {
	s.mu.Lock()
	c := s.capture
	s.mu.Unlock()
	if c == nil {
		s.violation(contracts.New(contracts.ErrValidationFailed, "audio arrived with no capture active; send capture.start first"))
		return
	}
	c.mu.Lock()
	ended, sent := c.ended, c.sent
	c.mu.Unlock()
	if ended {
		s.violation(contracts.New(contracts.ErrValidationFailed, "audio arrived after capture.end"))
		return
	}
	// The gate of the profile is checked when a stream opens; a pack that was
	// taken away since is noticed here, before the next chunk is sent on.
	if s.packChanged(c.pack) {
		s.cancelCapture(reasonPackChanged)
		return
	}
	if err := s.svc.d.Limits.CheckChunk(c.format, data, sent); err != nil {
		s.emitError(err, c.utt.id, "")
		s.cancelCaptureOf(c, reasonInvalidAudio)
		return
	}
	if err := c.stream.Send(data); err != nil {
		s.captureFailed(c, err, reasonSTTFailed)
		return
	}
	c.mu.Lock()
	c.sent += len(data)
	if c.keep {
		c.buf = append(c.buf, data...)
		s.buffered.Add(int64(len(data)))
	}
	c.mu.Unlock()
}

func (s *Session) onCaptureEnd() {
	s.mu.Lock()
	c := s.capture
	s.mu.Unlock()
	if c == nil {
		s.violation(contracts.New(contracts.ErrValidationFailed, "no capture is active"))
		return
	}
	c.mu.Lock()
	already, sent := c.ended, c.sent
	c.ended = true
	c.mu.Unlock()
	switch {
	case already:
		return
	case sent == 0:
		// Nothing was said: no utterance, no turn.
		s.cancelCapture(reasonNoSpeech)
		return
	}
	if err := c.stream.CloseSend(); err != nil {
		s.captureFailed(c, err, reasonSTTFailed)
	}
}

// cancelCapture cancels the streaming capture, if there is one: the recogniser
// is closed, the audio is dropped and the client is told. Nothing from the
// capture reaches the turn pipeline.
func (s *Session) cancelCapture(reason string) bool {
	s.mu.Lock()
	c := s.capture
	s.mu.Unlock()
	return c != nil && s.cancelCaptureOf(c, reason)
}

// cancelCaptureOf is cancelCapture for one capture.
func (s *Session) cancelCaptureOf(c *capture, reason string) bool {
	s.mu.Lock()
	if s.capture == c {
		s.capture = nil
	}
	s.mu.Unlock()
	if c.cancelled.Swap(true) {
		return false
	}
	c.cancel()
	_ = c.stream.Close()
	s.releaseBuffer(c)
	s.emit(voicewire.CaptureCancelled{UtteranceID: voicewire.UtteranceID(c.utt.id), Reason: voicewire.CaptureCancelledReason(reason)})
	return true
}

// captureFailed ends a capture whose recogniser failed: the client gets the
// typed error with the text_input_only effect, then the capture's terminal event.
func (s *Session) captureFailed(c *capture, err error, reason string) {
	if c.cancelled.Load() || s.closing() {
		return
	}
	s.emitError(withEffect(err, providers.RoleSTT), c.utt.id, "")
	s.cancelCaptureOf(c, reason)
}

// cancelStreams cancels the capture and the playback (a pack change, a closing
// session).
func (s *Session) cancelStreams(reason string) {
	s.cancelCapture(reason)
	s.cancelPlayback(reason)
}

// takeBuffer hands the audio of the capture to the caller, which owns it (and
// its place in the buffered count) from then on.
func (s *Session) takeBuffer(c *capture) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf
	c.buf = nil
	return b
}

// releaseBuffer drops (and zeroes) the audio of a capture.
func (s *Session) releaseBuffer(c *capture) {
	b := s.takeBuffer(c)
	s.buffered.Add(-int64(len(b)))
	clear(b)
}

// runCapture reads the recogniser's events until the stream completes: interim
// hypotheses go to the client as they come, and the final segments, once the
// recogniser says it is done, become the transcript of the utterance. A stream
// that ends any other way yields no transcript at all: partial output is never
// acted on.
func (s *Session) runCapture(c *capture) {
	defer s.wg.Done()
	var (
		finals []string
		last   int
		done   bool
		lang   string
	)
	fail := func(err error) { s.captureFailed(c, err, reasonSTTFailed) }
	for {
		ev, err := c.stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(err)
			return
		}
		if done || ev.Seq <= last {
			fail(contracts.New(contracts.ErrInternal, "voice: the recogniser's events are out of order"))
			return
		}
		last = ev.Seq
		switch {
		case ev.Done:
			done, lang = true, ev.Language
		case ev.Final:
			if t := strings.TrimSpace(ev.Text); t != "" {
				finals = append(finals, t)
			}
		default:
			s.emit(voicewire.TranscriptInterim{UtteranceID: voicewire.UtteranceID(c.utt.id), Seq: int64(ev.Seq), Text: clipRunes(ev.Text, 4096)})
		}
	}
	if !done {
		fail(contracts.New(contracts.ErrUnavailable, "voice: the transcript stream ended before completion"))
		return
	}
	if c.cancelled.Load() {
		return
	}
	s.mu.Lock()
	if s.capture == c {
		s.capture = nil
	}
	s.mu.Unlock()
	_ = c.stream.Close()
	c.cancel()
	text := strings.Join(finals, " ")
	audio := s.takeBuffer(c)
	if !s.enqueue(func() { s.finalize(c.utt, text, lang, c.format, audio, c.gen) }) {
		s.buffered.Add(-int64(len(audio)))
		clear(audio)
		s.emitError(contracts.New(contracts.ErrRateLimited, "voice: too many utterances are waiting for the pipeline"), c.utt.id, "")
		s.emit(voicewire.CaptureCancelled{UtteranceID: voicewire.UtteranceID(c.utt.id), Reason: voicewire.CaptureCancelledReason(reasonOverloaded)})
	}
}

// onAudioSubmit is one finalised utterance in a single message.
func (s *Session) onAudioSubmit(m voicewire.AudioSubmit) {
	format := voice.AudioFormat{Encoding: voice.Encoding(m.Format.Encoding), SampleRate: int(m.Format.SampleRate), Channels: int(m.Format.Channels)}
	lang := s.opts.Language
	if m.Language != nil {
		lang = string(*m.Language)
	}
	if lang != "" && !voice.ValidLanguage(lang) {
		s.violation(contracts.Newf(contracts.ErrValidationFailed, "voice: language %q is not a language tag", lang))
		return
	}
	audio, err := base64.StdEncoding.DecodeString(m.Audio)
	if err != nil {
		s.violation(contracts.New(contracts.ErrValidationFailed, "voice: audio.submit carries audio that is not base64"))
		return
	}
	if err := s.svc.d.Limits.CheckAudio(format, audio); err != nil {
		clear(audio)
		s.violation(err)
		return
	}
	s.mu.Lock()
	busy := s.capture != nil
	s.mu.Unlock()
	if busy {
		clear(audio)
		s.violation(contracts.New(contracts.ErrConflict, "a capture is active; end or cancel it before submitting audio"))
		return
	}

	gen := s.beginUtterance()
	u := s.newUtterance()
	u.lang = lang
	s.emit(voicewire.CaptureStarted{UtteranceID: voicewire.UtteranceID(u.id)})
	s.buffered.Add(int64(len(audio)))
	ok := s.enqueue(func() {
		s.transcribeBatch(u, gen, format, lang, audio)
	})
	if !ok {
		s.buffered.Add(-int64(len(audio)))
		clear(audio)
		s.emitError(contracts.New(contracts.ErrRateLimited, "voice: too many utterances are waiting for the pipeline"), u.id, "")
		s.emit(voicewire.CaptureCancelled{UtteranceID: voicewire.UtteranceID(u.id), Reason: voicewire.CaptureCancelledReason(reasonOverloaded)})
	}
}

// transcribeBatch recognises one submitted utterance (on the worker).
func (s *Session) transcribeBatch(u *utterance, gen uint64, format voice.AudioFormat, lang string, audio []byte) {
	tr, err := s.stt.Transcribe(s.ctx, s.scope.Clone(), voice.TranscribeRequest{Audio: audio, Format: format, Language: lang})
	if err != nil {
		s.buffered.Add(-int64(len(audio)))
		clear(audio)
		if s.closing() {
			return
		}
		s.emitError(withEffect(err, providers.RoleSTT), u.id, "")
		s.emit(voicewire.CaptureCancelled{UtteranceID: voicewire.UtteranceID(u.id), Reason: voicewire.CaptureCancelledReason(reasonSTTFailed)})
		return
	}
	s.finalize(u, tr.Text, tr.Language, format, audio, gen)
}

// finalize is what happens to a recognised utterance (on the worker): the
// audio is retained or dropped, the transcript is shown to the host, and the
// turn pipeline runs it. audio is owned by finalize and zeroed before it
// returns.
func (s *Session) finalize(u *utterance, text, lang string, format voice.AudioFormat, audio []byte, gen uint64) {
	defer func() {
		s.buffered.Add(-int64(len(audio)))
		clear(audio)
	}()
	text = strings.TrimSpace(text)
	if text == "" {
		s.emit(voicewire.CaptureCancelled{UtteranceID: voicewire.UtteranceID(u.id), Reason: voicewire.CaptureCancelledReason(reasonNoSpeech)})
		return
	}
	if s.retain {
		// Only for a tenant whose policy says so, and fail closed: audio the
		// tenant requires to be kept and could not be kept is not processed.
		err := s.svc.d.Retainer.Retain(s.ctx, s.scope.Clone(), RetainedAudio{UtteranceID: u.id, Format: format, PCM: audio})
		if err != nil {
			if s.closing() {
				return
			}
			s.emitError(contracts.Wrap(contracts.ErrUnavailable, err, "voice: the audio could not be retained as this tenant's policy requires"), u.id, "")
			s.emit(voicewire.CaptureCancelled{UtteranceID: voicewire.UtteranceID(u.id), Reason: voicewire.CaptureCancelledReason(reasonRetention)})
			return
		}
	}
	ev := voicewire.TranscriptFinal{UtteranceID: voicewire.UtteranceID(u.id), Text: clipRunes(text, 4096)}
	if voice.ValidLanguage(lang) {
		l := voicewire.Language(lang)
		ev.Language = &l
	}
	if h, ok := s.speakerHint(format, audio); ok {
		ev.SpeakerHint = &h
	}
	u.text = ev.Text
	s.remember(u)
	s.emit(ev)
	s.runTurn(u, ev.Text, gen, nil)
}

// speakerHint asks the optional speaker-recognition component who spoke. Its
// answer is reported beside the transcript and used for nothing else.
func (s *Session) speakerHint(format voice.AudioFormat, audio []byte) (voicewire.SpeakerHint, bool) {
	rec := s.svc.d.Speaker
	if rec == nil || len(audio) == 0 {
		return voicewire.SpeakerHint{}, false
	}
	h, err := rec.Recognize(s.ctx, s.scope.Clone(), format, audio)
	label := strings.TrimSpace(h.Label)
	if err != nil || label == "" || h.Confidence < 0 || h.Confidence > 1 {
		return voicewire.SpeakerHint{}, false
	}
	return voicewire.SpeakerHint{Label: clipRunes(label, 64), Confidence: h.Confidence}, true
}

func clipRunes(s string, n int) string {
	if len(s) <= n { // bytes bound runes: nothing to clip
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
