package session

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	voicewire "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// playback is the speech of one verified response on its way to the client.
type playback struct {
	id     string
	turnID string
	pack   string
	ctx    context.Context
	cancel context.CancelFunc

	// cancelled is set first by whoever ends the playback early: from then on no
	// further chunk is written (emitAudio checks it under the write lock), so
	// after playback.cancelled has been sent nothing more of this playback is.
	cancelled atomic.Bool
	// ended makes the terminal event (done or cancelled) go out exactly once.
	ended atomic.Bool

	mu     sync.Mutex
	stream voice.AudioStream
}

func (pb *playback) claimEnd() bool { return pb.ended.CompareAndSwap(false, true) }

// setStream records the open synthesis stream. If the playback was cancelled
// meanwhile the stream is closed at once and false is returned.
func (pb *playback) setStream(st voice.AudioStream) bool {
	pb.mu.Lock()
	pb.stream = st
	pb.mu.Unlock()
	if pb.cancelled.Load() {
		_ = st.Close()
		return false
	}
	return true
}

func (pb *playback) closeStream() {
	pb.mu.Lock()
	st := pb.stream
	pb.mu.Unlock()
	if st != nil {
		_ = st.Close()
	}
}

// reservePlayback registers the playback of the response of turnID, unless a
// newer utterance has begun since the turn started (gen moved on): that speech
// is stale and is not started. A playback still running for an earlier
// response is cancelled first. Registering happens before the response event
// is sent, so a barge-in that follows it finds the playback and cancels it.
func (s *Session) reservePlayback(turnID string, gen uint64) (*playback, bool) {
	ctx, cancel := context.WithCancel(s.ctx)
	pb := &playback{id: s.svc.ids.New(prefixPlayback), turnID: turnID, pack: s.activePack(), ctx: ctx, cancel: cancel}
	s.mu.Lock()
	if s.closed || s.gen != gen {
		s.mu.Unlock()
		cancel()
		return nil, false
	}
	old := s.pb
	s.pb = pb
	s.mu.Unlock()
	if old != nil {
		s.endPlayback(old, reasonBargeIn)
	}
	return pb, true
}

// cancelPlayback cancels the speech that is playing, if any. Synthesis is
// stopped before anything else happens (the stream is closed, which is how a
// provider is told to stop), and the client is told once the last chunk it will
// get has been written.
func (s *Session) cancelPlayback(reason string) bool {
	s.mu.Lock()
	pb := s.pb
	s.pb = nil
	s.mu.Unlock()
	if pb == nil {
		return false
	}
	return s.endPlayback(pb, reason)
}

func (s *Session) endPlayback(pb *playback, reason string) bool {
	if pb.cancelled.Swap(true) {
		return false
	}
	pb.cancel()
	pb.closeStream()
	if pb.claimEnd() {
		s.emit(voicewire.PlaybackCancelled{
			PlaybackID: voicewire.PlaybackID(pb.id), TurnID: actions.TurnID(pb.turnID),
			Reason: voicewire.PlaybackCancelledReason(reason),
		})
	}
	return true
}

// emitAudio writes one chunk of speech, unless the playback was cancelled: the
// check is made under the write lock, so a chunk can never follow the
// playback.cancelled event.
func (s *Session) emitAudio(pb *playback, data []byte) bool {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if pb.cancelled.Load() {
		return false
	}
	return s.write(true, data)
}

// emitLive writes an event of the playback unless it was cancelled.
func (s *Session) emitLive(pb *playback, ev voicewire.ServerEventValue) bool {
	raw, ok := s.encode(ev)
	if !ok {
		return false
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if pb.cancelled.Load() {
		return false
	}
	return s.write(false, raw)
}

// playbackFailed ends a playback whose synthesis failed: the client gets the
// typed error with the verified_text_only effect (the text it already has is
// the answer) and the playback's terminal event.
func (s *Session) playbackFailed(pb *playback, err error) {
	if pb.cancelled.Swap(true) {
		return
	}
	pb.cancel()
	pb.closeStream()
	s.mu.Lock()
	if s.pb == pb {
		s.pb = nil
	}
	s.mu.Unlock()
	if s.closing() {
		return
	}
	s.emitError(withEffect(err, providers.RoleTTS), "", pb.turnID)
	if pb.claimEnd() {
		s.emit(voicewire.PlaybackCancelled{
			PlaybackID: voicewire.PlaybackID(pb.id), TurnID: actions.TurnID(pb.turnID),
			Reason: voicewire.PlaybackCancelledReasonError,
		})
	}
}

// runPlayback synthesises text (already verified) and streams it to the client.
func (s *Session) runPlayback(pb *playback, text, language string) {
	defer s.wg.Done()
	started := false
	for _, piece := range speechPieces(text, s.svc.d.Limits.MaxTextRunes) {
		if pb.cancelled.Load() {
			return
		}
		stream, err := s.tts.SynthesizeStream(pb.ctx, s.scope.Clone(), voice.SynthesizeRequest{Text: piece, Language: language})
		if err != nil {
			s.playbackFailed(pb, err)
			return
		}
		if !pb.setStream(stream) {
			return
		}
		var done bool
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				if !pb.cancelled.Load() {
					s.playbackFailed(pb, err)
				}
				return
			}
			if s.packChanged(pb.pack) {
				s.cancelPlayback(reasonPackChanged)
				return
			}
			if len(chunk.Data) > 0 {
				if !started {
					if err := chunk.Format.Validate(); err != nil {
						s.playbackFailed(pb, contracts.Wrap(contracts.ErrSchemaInvalid, err, "voice: the synthesizer reported an unusable audio format"))
						return
					}
					started = true
					if !s.emitLive(pb, voicewire.PlaybackStarted{
						PlaybackID: voicewire.PlaybackID(pb.id), TurnID: actions.TurnID(pb.turnID),
						Format: voicewire.AudioFormat{Encoding: string(chunk.Format.Encoding), SampleRate: int64(chunk.Format.SampleRate), Channels: int64(chunk.Format.Channels)},
					}) {
						return
					}
				}
				if !s.emitAudio(pb, chunk.Data) {
					return
				}
			}
			done = done || chunk.Done
		}
		_ = stream.Close()
		if !done {
			s.playbackFailed(pb, contracts.New(contracts.ErrUnavailable, "voice: the audio stream ended before completion"))
			return
		}
	}
	if !started {
		s.playbackFailed(pb, contracts.New(contracts.ErrUnavailable, "voice: the synthesizer produced no audio"))
		return
	}
	s.mu.Lock()
	if s.pb == pb {
		s.pb = nil
	}
	s.mu.Unlock()
	if pb.cancelled.Load() {
		return
	}
	if pb.claimEnd() {
		s.emit(voicewire.PlaybackDone{PlaybackID: voicewire.PlaybackID(pb.id), TurnID: actions.TurnID(pb.turnID)})
	}
	pb.cancel()
}

// speechPieces cuts text into pieces of at most max runes for synthesis: at
// sentence ends where it can, at spaces where it cannot, and mid-word only for
// a word longer than max. The pieces together are exactly the text, in order,
// so nothing that was verified is left unspoken and nothing else is added.
func speechPieces(text string, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" || max <= 0 {
		return nil
	}
	if utf8.RuneCountInString(text) <= max {
		return []string{text}
	}
	var sentences []string
	runes := []rune(text)
	start := 0
	for i, r := range runes {
		if (r == '.' || r == '!' || r == '?' || r == '\n') && (i+1 == len(runes) || unicode.IsSpace(runes[i+1])) {
			sentences = append(sentences, string(runes[start:i+1]))
			start = i + 1
		}
	}
	if start < len(runes) {
		sentences = append(sentences, string(runes[start:]))
	}
	var out []string
	var cur []rune
	flush := func() {
		if t := strings.TrimSpace(string(cur)); t != "" {
			out = append(out, t)
		}
		cur = cur[:0]
	}
	for _, sent := range sentences {
		sr := []rune(sent)
		for len(sr) > max {
			// A sentence longer than a piece: cut at the last space inside the piece.
			cut := max
			for j := max; j > max/2; j-- {
				if unicode.IsSpace(sr[j-1]) {
					cut = j
					break
				}
			}
			flush()
			out = append(out, strings.TrimSpace(string(sr[:cut])))
			sr = sr[cut:]
		}
		if len(cur)+len(sr) > max {
			flush()
		}
		cur = append(cur, sr...)
	}
	flush()
	return out
}
