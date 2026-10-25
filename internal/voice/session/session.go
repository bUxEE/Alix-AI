package session

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
	voicewire "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// Transport is the connection under a Session: the WebSocket the gateway
// upgrades, or a pair of channels in a test. A text message is a JSON control
// message (schemas/voice/client-message and server-event); a binary message is
// audio. Read and Write must honour their context, and Write may be called by
// one goroutine at a time (the Session serialises its writes).
type Transport interface {
	// Read blocks for the next message of the client.
	Read(ctx context.Context) (binary bool, data []byte, err error)
	// Write sends one message to the client.
	Write(ctx context.Context, binary bool, data []byte) error
}

// CloseReason says why a Serve ended, for the close frame of the connection.
type CloseReason string

// The reasons a voice connection ends.
const (
	ReasonClientClosed  CloseReason = "client_closed"
	ReasonExpired       CloseReason = "credential_expired"
	ReasonReplaced      CloseReason = "replaced"
	ReasonServerClosing CloseReason = "server_closing"
	ReasonProtocol      CloseReason = "protocol_violation"
	ReasonWriteFailed   CloseReason = "write_failed"
)

const (
	// writeTimeout bounds one write to the client: a client that does not read
	// does not hold the session (or a cancellation) hostage.
	writeTimeout = 10 * time.Second
	// jobQueue is how many finalised utterances may wait for the turn pipeline.
	jobQueue = 8
	// maxViolations is how many malformed messages a connection may send before
	// it is closed.
	maxViolations = 16
	// keepUtterances is how many utterances a session remembers for correction.
	keepUtterances = 8
	// maxTranscriptRunes bounds a corrected transcript (schemas/voice).
	maxTranscriptRunes = 2000
)

// Session is one voice connection on a conversation session. Voice and text
// share that session: every finalised transcript runs through the normal turn
// pipeline under the scope the verified context token gave the connection, and
// nothing a client or a speech component says changes that scope.
//
// Serve drives it. All the client's messages are handled by one goroutine (the
// one in Serve), which never waits on the turn pipeline or on a speech model
// except to open a stream: a cancellation is always handled at once. A capture
// is read by a goroutine of its own, finalised utterances run the pipeline one
// at a time, in order, on one worker, and a playback runs on its own goroutine.
type Session struct {
	svc    *Service
	id     string
	scope  contracts.Scope
	opts   Options
	stt    voice.STT
	tts    voice.TTS // nil: speech is off, responses are text
	retain bool
	packID string
	life   time.Duration
	key    string
	log    *slog.Logger

	// Set by Serve.
	ctx    context.Context
	cancel context.CancelFunc
	tr     Transport
	outMu  sync.Mutex // serialises writes; held across one write, never across a wait on mu
	jobs   chan func()
	wg     sync.WaitGroup

	mu         sync.Mutex
	served     bool
	closed     bool
	reason     CloseReason
	gen        uint64 // bumped by every new utterance: the barge-in clock
	capture    *capture
	pb         *playback
	utts       []*utterance
	violations int

	buffered atomic.Int64 // bytes of raw audio held right now (see BufferedAudioBytes)
}

// ID is the voice session id (vss_...), not the conversation session's.
func (s *Session) ID() string { return s.id }

// ReadLimit is the largest message the transport must accept from the client:
// one finalised utterance of audio.submit, base64 encoded, plus its envelope.
func (s *Session) ReadLimit() int64 {
	return int64(s.svc.d.Limits.MaxAudioBytes)/3*4 + 4*1024 + 4
}

// BufferedAudioBytes is how many bytes of raw audio the session holds right
// now: zero unless an utterance is being captured for a tenant that retains
// audio or for the speaker-recognition component, and zero again the moment
// the utterance is finished.
func (s *Session) BufferedAudioBytes() int { return int(s.buffered.Load()) }

// Close ends the session: Serve returns, live streams are cancelled and the
// connection's registration is released. It is idempotent and safe from any
// goroutine; on a session that was never served it just releases it.
func (s *Session) Close(reason CloseReason) {
	s.mu.Lock()
	if s.reason == "" {
		s.reason = reason
	}
	cancel, served := s.cancel, s.served
	s.closed = s.closed || !served
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !served {
		s.svc.deregister(s)
	}
}

// Serve runs the session over tr until the client goes away, the credential's
// life ends, Close is called or the connection fails, and returns why. It
// cancels everything it started before returning. A session is served once.
func (s *Session) Serve(ctx context.Context, tr Transport) CloseReason {
	s.mu.Lock()
	if s.served || s.closed {
		r := s.reason
		s.mu.Unlock()
		if r == "" {
			r = ReasonServerClosing
		}
		return r
	}
	s.served = true
	s.tr = tr
	s.ctx, s.cancel = context.WithTimeout(ctx, s.life)
	s.jobs = make(chan func(), jobQueue)
	s.wg.Add(1)
	s.mu.Unlock()
	go s.worker()

	s.emit(s.started())
	s.readLoop()
	return s.shutdown()
}

func (s *Session) started() voicewire.SessionStarted {
	ev := voicewire.SessionStarted{
		VoiceSessionID: voicewire.VoiceSessionID(s.id),
		SessionID:      commonIdentifier(s.scope.SessionID),
		Speech:         s.tts != nil,
		RetainRawAudio: s.retain,
		MaxChunkBytes:  int64(s.svc.d.Limits.MaxChunkBytes),
		MaxAudioBytes:  int64(s.svc.d.Limits.MaxAudioBytes),
	}
	if s.opts.Language != "" {
		l := voicewire.Language(s.opts.Language)
		ev.Language = &l
	}
	if s.packID != "" {
		p := s.packID
		ev.PackID = &p
	}
	return ev
}

// shutdown ends everything the session started and says why it ended.
func (s *Session) shutdown() CloseReason {
	s.mu.Lock()
	s.closed = true
	reason := s.reason
	close(s.jobs)
	s.mu.Unlock()

	s.cancelStreams(reasonSessionClosed)
	s.cancel()
	s.wg.Wait()
	s.buffered.Store(0) // whatever a discarded job still held is garbage now
	s.svc.deregister(s)
	if reason == "" {
		reason = ReasonClientClosed
	}
	s.log.Info("voice session ended", "reason", string(reason))
	return reason
}

func (s *Session) readLoop() {
	for {
		binary, data, err := s.tr.Read(s.ctx)
		if err != nil {
			s.mu.Lock()
			if s.reason == "" {
				switch {
				case errors.Is(s.ctx.Err(), context.DeadlineExceeded):
					s.reason = ReasonExpired
				case s.ctx.Err() != nil:
					s.reason = ReasonServerClosing
				default:
					s.reason = ReasonClientClosed
				}
			}
			s.mu.Unlock()
			return
		}
		if binary {
			s.onAudio(data)
		} else {
			s.onControl(data)
		}
		if s.ctx.Err() != nil {
			return
		}
	}
}

// worker runs the finalised utterances, one at a time and in order.
func (s *Session) worker() {
	defer s.wg.Done()
	for job := range s.jobs {
		if s.ctx.Err() != nil {
			continue
		}
		job()
	}
}

// enqueue hands job to the worker; it reports false when the session is
// closing or the queue is full (the client is sending faster than the
// pipeline runs).
func (s *Session) enqueue(job func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.jobs <- job:
		return true
	default:
		return false
	}
}

// violation records a message the session cannot act on: it answers with a
// typed error and, past maxViolations, closes the connection.
func (s *Session) violation(err error) {
	s.emitError(err, "", "")
	s.mu.Lock()
	s.violations++
	over := s.violations > maxViolations
	s.mu.Unlock()
	if over {
		s.Close(ReasonProtocol)
	}
}

func (s *Session) onControl(data []byte) {
	var msg voicewire.ClientMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		s.violation(contracts.New(contracts.ErrValidationFailed, "the message is not a voice client message"))
		return
	}
	switch m := msg.Value.(type) {
	case voicewire.CaptureStart:
		s.onCaptureStart(m)
	case voicewire.CaptureEnd:
		s.onCaptureEnd()
	case voicewire.CaptureCancel:
		if !s.cancelCapture(reasonClient) {
			s.violation(contracts.New(contracts.ErrValidationFailed, "no capture is active"))
		}
	case voicewire.AudioSubmit:
		s.onAudioSubmit(m)
	case voicewire.PlaybackCancel:
		// Not an error when nothing plays: the speech may have just finished.
		s.cancelPlayback(reasonClient)
	case voicewire.TranscriptCorrect:
		s.onTranscriptCorrect(m)
	default:
		s.violation(contracts.New(contracts.ErrValidationFailed, "the message is not a voice client message"))
	}
}

// ---- output ----

// emit sends one event to the client. It reports whether the write succeeded;
// a failed write ends the session (the connection is gone or stuck).
func (s *Session) emit(ev voicewire.ServerEventValue) bool {
	raw, ok := s.encode(ev)
	if !ok {
		return false
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	return s.write(false, raw)
}

// encode is the JSON of an event. An event that does not encode (a variant
// that breaks its own schema) is a bug of ours and is dropped, never sent half.
func (s *Session) encode(ev voicewire.ServerEventValue) ([]byte, bool) {
	raw, err := json.Marshal(voicewire.ServerEvent{Value: ev})
	if err != nil {
		s.log.Error("a voice event could not be encoded", "event", eventName(ev))
		return nil, false
	}
	return raw, true
}

// write sends one message; the caller holds outMu.
func (s *Session) write(binary bool, data []byte) bool {
	ctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
	defer cancel()
	if err := s.tr.Write(ctx, binary, data); err != nil {
		if s.ctx.Err() == nil {
			s.mu.Lock()
			if s.reason == "" {
				s.reason = ReasonWriteFailed
			}
			s.mu.Unlock()
			s.cancel()
		}
		return false
	}
	return true
}

func (s *Session) newUtterance() *utterance {
	return &utterance{id: s.svc.ids.New(prefixUtterance)}
}

// closing reports whether the session is ending: a failure that comes with it
// is the cancellation's doing and says nothing more to the client.
func (s *Session) closing() bool { return s.ctx.Err() != nil }
