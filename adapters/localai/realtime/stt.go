package realtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Transcribe recognises one complete buffer through a Realtime session. The
// audio is sent in chunks, the stream is closed for sending and drained, so the
// answer has the same completeness guarantee as a stream's.
func (c *Client) Transcribe(ctx context.Context, scope contracts.Scope, req pv.TranscribeRequest) (pv.Transcript, error) {
	if err := scope.Validate(); err != nil {
		return pv.Transcript{}, err
	}
	if err := req.Validate(c.limits); err != nil {
		return pv.Transcript{}, err
	}
	s, err := c.TranscribeStream(ctx, scope, pv.STTStreamRequest{Format: req.Format, Language: req.Language, Profile: req.Profile})
	if err != nil {
		return pv.Transcript{}, err
	}
	for off := 0; off < len(req.Audio); {
		n := min(c.limits.MaxChunkBytes-c.limits.MaxChunkBytes%req.Format.FrameBytes(), len(req.Audio)-off)
		if err := s.Send(req.Audio[off : off+n]); err != nil {
			_ = s.Close()
			return pv.Transcript{}, err
		}
		off += n
	}
	if err := s.CloseSend(); err != nil {
		_ = s.Close()
		return pv.Transcript{}, err
	}
	return pv.CollectTranscript(s)
}

// TranscribeStream opens a Realtime session configured as a transcription-only
// session (see sessionUpdate), waits for the runtime to confirm that
// configuration, and returns. A failure to connect, to upgrade or to configure
// the session happens here, before any audio is sent, and is unavailable when
// the runtime is absent or cannot serve the session: that is the one outcome
// the fallback to the separate STT adapter acts on.
func (c *Client) TranscribeStream(ctx context.Context, scope contracts.Scope, req pv.STTStreamRequest) (pv.STTStream, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.Format.Channels != 1 {
		return nil, contracts.New(contracts.ErrValidationFailed, "localai realtime: the realtime pipeline takes mono audio only")
	}
	if err := c.limits.Validate(); err != nil {
		return nil, err
	}
	model, err := c.model(req.Profile)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, providers.ContextError(err)
	}
	lang := primaryLanguage(req.Language)
	update, err := sessionUpdate(model, lang, req.Format, c.cfg.ServerVAD, c.cfg.silenceMs())
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "localai realtime: cannot build the session configuration")
	}
	conn, err := c.dial(ctx, model)
	if err != nil {
		return nil, err
	}
	if err := c.configure(ctx, conn, update); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &sttStream{
		c:          c,
		ctx:        sctx,
		parent:     ctx,
		cancel:     cancel,
		conn:       conn,
		format:     req.Format,
		language:   lang,
		model:      model,
		began:      c.clk.Now(),
		events:     make(chan readResult, 16),
		acc:        map[string]string{},
		sendClosed: make(chan struct{}),
		closedCh:   make(chan struct{}),
	}
	s.sendClosedC = s.sendClosed
	go s.readLoop()
	return s, nil
}

// configure sends the session configuration and waits until the runtime
// confirms a transcription session. A confirmation of any other session type
// is refused: audio is never sent to a session that could answer.
func (c *Client) configure(ctx context.Context, conn *websocket.Conn, update []byte) error {
	hctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	fail := func(err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return providers.ContextError(cerr)
		}
		return interrupted("the runtime closed the connection while the session was being configured")
	}
	if err := conn.Write(hctx, websocket.MessageText, update); err != nil {
		return fail(err)
	}
	for {
		_, data, err := conn.Read(hctx)
		if err != nil {
			return fail(err)
		}
		ev, err := parseServerEvent(data)
		if err != nil {
			return err
		}
		switch {
		case isSideDoor(ev):
			return sideDoor(ev.Type)
		case ev.Type == evError:
			e := errorEvent{}
			if ev.Error != nil {
				e = *ev.Error
			}
			return mapEventError(e)
		case ev.Type == evSessionUpdated:
			if ev.Session == nil || ev.Session.Type != "transcription" {
				return contracts.New(contracts.ErrInternal, "localai realtime: the runtime did not confirm a transcription-only session; session closed").
					WithDetails("reason", reasonNotTransOnly)
			}
			return nil
		}
	}
}

// readResult is one message (or the end) of the connection.
type readResult struct {
	data []byte
	err  error
}

// sttStream implements voice.STTStream over one Realtime connection. Send and
// CloseSend are for one sender and Recv for one receiver; Close may be called
// at any time and unblocks a waiting Recv.
type sttStream struct {
	c        *Client
	parent   context.Context // the caller's context, to tell its end from ours
	ctx      context.Context // stream context: ends on Close, drop or the caller's end
	cancel   context.CancelFunc
	conn     *websocket.Conn
	format   pv.AudioFormat
	language string
	model    string
	began    time.Time

	events chan readResult // filled by readLoop, closed when it exits

	wmu      sync.Mutex // guards writes, sent, sendDone, noAudio
	sent     int
	sendDone bool
	noAudio  bool

	sendClosed chan struct{} // closed by CloseSend after its writes
	closedCh   chan struct{} // closed by Close
	closed     atomic.Bool
	closeOnce  sync.Once
	dropOnce   sync.Once

	// Receiver state (one receiver).
	sendClosedC <-chan struct{} // sendClosed until it has been seen, then nil
	deadline    *time.Timer
	settle      *time.Timer
	queue       []pv.TranscriptEvent
	seq         int
	acc         map[string]string
	firstDelta  time.Time
	completed   int
	started     int
	done        bool
	err         error
}

// readLoop is the only reader of the connection. It stops when the connection
// ends or the stream context does.
func (s *sttStream) readLoop() {
	defer close(s.events)
	for {
		typ, data, err := s.conn.Read(s.ctx)
		var r readResult
		switch {
		case err != nil:
			r = readResult{err: err}
		case typ != websocket.MessageText:
			r = readResult{err: schemaInvalid("the runtime sent a binary message")}
		default:
			r = readResult{data: data}
		}
		select {
		case s.events <- r:
		case <-s.ctx.Done():
			return
		}
		if r.err != nil {
			return
		}
	}
}

// writeMsg sends one text message under the request timeout.
func (s *sttStream) writeMsg(data []byte) error {
	wctx, cancel := context.WithTimeout(s.ctx, s.c.timeout)
	defer cancel()
	if err := s.conn.Write(wctx, websocket.MessageText, data); err != nil {
		return s.mapWriteError(err)
	}
	return nil
}

func (s *sttStream) mapWriteError(error) error {
	switch {
	case s.closed.Load():
		return providers.ErrStreamClosed
	case s.parent.Err() != nil:
		return providers.ContextError(s.parent.Err())
	}
	return interrupted("the connection to the runtime was lost while sending audio")
}

// audioState returns the audio sent so far and whether CloseSend found none.
func (s *sttStream) audioState() (sent int, noAudio bool) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.sent, s.noAudio
}

// Send implements voice.STTStream.
func (s *sttStream) Send(audio []byte) error {
	if s.closed.Load() {
		return providers.ErrStreamClosed
	}
	if err := s.parent.Err(); err != nil {
		return providers.ContextError(err)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.sendDone {
		return contracts.New(contracts.ErrValidationFailed, "localai realtime: Send after CloseSend")
	}
	if err := s.c.limits.CheckChunk(s.format, audio, s.sent); err != nil {
		return err
	}
	msg, err := appendEvent(audio)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "localai realtime: cannot build the audio event")
	}
	if err := s.writeMsg(msg); err != nil {
		return err
	}
	s.sent += len(audio)
	return nil
}

// CloseSend implements voice.STTStream. In manual mode it commits the audio as
// one utterance (or clears the empty buffer, so silence is an empty
// transcript). In server-VAD mode it appends enough silence for the runtime's
// turn detection to close the last utterance.
func (s *sttStream) CloseSend() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.sendDone {
		return nil
	}
	if s.closed.Load() {
		return providers.ErrStreamClosed
	}
	s.sendDone = true
	s.noAudio = s.sent == 0
	defer close(s.sendClosed) // wakes Recv after the writes below, success or not
	switch {
	case s.noAudio:
		return s.writeMsg(bareEvent(evAudioClear))
	case s.c.cfg.ServerVAD:
		tail := s.format.BytesFor(time.Duration(s.c.cfg.silenceMs()+vadTickMs+200) * time.Millisecond)
		for tail > 0 {
			n := min(tail, s.c.limits.MaxChunkBytes-s.c.limits.MaxChunkBytes%s.format.FrameBytes())
			msg, err := appendEvent(make([]byte, n))
			if err != nil {
				return contracts.Wrap(contracts.ErrInternal, err, "localai realtime: cannot build the audio event")
			}
			if err := s.writeMsg(msg); err != nil {
				return err
			}
			tail -= n
		}
		return nil
	}
	return s.writeMsg(bareEvent(evAudioCommit))
}

// Close implements voice.STTStream: it cancels the stream, drops the
// connection (which stops the runtime's work at once) and unblocks a waiting
// Recv. It is idempotent.
func (s *sttStream) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.closedCh)
		s.drop()
	})
	return nil
}

// drop releases the connection and the reader without marking the stream
// closed (a completed or failed stream still answers Recv).
func (s *sttStream) drop() {
	s.dropOnce.Do(func() {
		s.cancel()
		_ = s.conn.CloseNow()
		if s.deadline != nil {
			s.deadline.Stop()
		}
		if s.settle != nil {
			s.settle.Stop()
		}
	})
}

func (s *sttStream) fail(err error) (pv.TranscriptEvent, error) {
	s.err = err
	s.drop()
	return pv.TranscriptEvent{}, err
}

// Recv implements voice.STTStream.
func (s *sttStream) Recv() (pv.TranscriptEvent, error) {
	var deadlineC, settleC <-chan time.Time
	for {
		if s.closed.Load() {
			return pv.TranscriptEvent{}, providers.ErrStreamClosed
		}
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, nil
		}
		if s.err != nil {
			return pv.TranscriptEvent{}, s.err
		}
		if s.done {
			return pv.TranscriptEvent{}, io.EOF
		}
		if s.deadline != nil {
			deadlineC = s.deadline.C
		}
		if s.settle != nil {
			settleC = s.settle.C
		}
		select {
		case <-s.closedCh:
			return pv.TranscriptEvent{}, providers.ErrStreamClosed
		case <-s.ctx.Done():
			if s.closed.Load() {
				return pv.TranscriptEvent{}, providers.ErrStreamClosed
			}
			if cerr := s.parent.Err(); cerr != nil {
				return s.fail(providers.ContextError(cerr))
			}
			return s.fail(interrupted("the stream was cancelled"))
		case <-s.sendClosedC:
			s.sendClosedC = nil
			s.deadline = time.NewTimer(s.c.timeout)
			if _, noAudio := s.audioState(); s.c.cfg.ServerVAD && !noAudio {
				s.settle = time.NewTimer(s.c.cfg.settle())
			}
		case <-deadlineC:
			return s.fail(contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "localai realtime: the runtime did not finish the transcript in time"))
		case <-settleC:
			s.settle = nil
			settleC = nil
			if s.completed >= s.started {
				s.finish()
			} else {
				s.settle = time.NewTimer(s.c.cfg.settle())
			}
		case r, ok := <-s.events:
			if !ok {
				return s.fail(interrupted("the connection ended before the transcript was complete"))
			}
			if r.err != nil {
				return s.fail(s.mapReadError(r.err))
			}
			if err := s.handle(r.data); err != nil {
				return s.fail(err)
			}
		}
	}
}

func (s *sttStream) mapReadError(err error) error {
	switch {
	case s.closed.Load():
		return providers.ErrStreamClosed
	case s.parent.Err() != nil:
		return providers.ContextError(s.parent.Err())
	case websocket.CloseStatus(err) == websocket.StatusMessageTooBig:
		return schemaInvalid("a runtime event exceeds the size limit")
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return interrupted("the connection ended before the transcript was complete")
}

func (s *sttStream) next(ev pv.TranscriptEvent) pv.TranscriptEvent {
	s.seq++
	ev.Seq = s.seq
	return ev
}

// handle applies one server event.
func (s *sttStream) handle(data []byte) error {
	ev, err := parseServerEvent(data)
	if err != nil {
		return err
	}
	if isSideDoor(ev) {
		return sideDoor(ev.Type)
	}
	switch ev.Type {
	case evError:
		e := errorEvent{}
		if ev.Error != nil {
			e = *ev.Error
		}
		return mapEventError(e)
	case evTransFailed:
		e := errorEvent{Code: "transcription_failed"}
		if ev.Error != nil {
			e = *ev.Error
		}
		return mapEventError(e)
	case evSpeechStarted:
		s.started++
	case evTransDelta:
		if ev.Delta == "" {
			return nil
		}
		if s.firstDelta.IsZero() {
			s.firstDelta = s.c.clk.Now()
		}
		s.acc[ev.ItemID] += ev.Delta
		if t := strings.TrimSpace(s.acc[ev.ItemID]); t != "" {
			s.queue = append(s.queue, s.next(pv.TranscriptEvent{Text: t}))
		}
	case evTransCompleted:
		delete(s.acc, ev.ItemID)
		s.completed++
		if t := strings.TrimSpace(ev.Transcript); t != "" {
			s.queue = append(s.queue, s.next(pv.TranscriptEvent{Text: t, Final: true}))
		}
		if !s.c.cfg.ServerVAD {
			s.finish() // manual mode: the one commit has been answered
		}
	case evCleared:
		if _, noAudio := s.audioState(); noAudio {
			s.finish() // nothing was said: an empty, complete transcript
		}
	}
	return nil
}

// finish queues the Done event and releases the connection. The transcript is
// complete from here: Recv drains the queue, then returns io.EOF.
func (s *sttStream) finish() {
	sent, _ := s.audioState()
	now := s.c.clk.Now()
	t := providers.Timings{Total: now.Sub(s.began)}
	if !s.firstDelta.IsZero() {
		t.TimeToFirstToken = s.firstDelta.Sub(s.began)
	}
	lang := ""
	if pv.ValidLanguage(s.language) {
		lang = s.language
	}
	s.queue = append(s.queue, s.next(pv.TranscriptEvent{
		Done:     true,
		Language: lang,
		Offset:   s.format.DurationOf(sent),
		Timings:  t,
		Model:    s.model,
	}))
	s.done = true
	s.drop()
}
