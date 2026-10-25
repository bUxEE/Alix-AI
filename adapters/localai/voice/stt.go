package voice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxSSELine bounds one server-sent-events line.
const maxSSELine = 1 << 20

// buildTranscribeBody builds the multipart/form-data body of a transcription
// request in memory. The audio goes in as a WAV file named audio.wav: the file
// name is fixed, never derived from the caller, because the runtime joins it to
// a temporary directory path. Nothing is written to disk by the adapter.
func buildTranscribeBody(model, language string, stream bool, f pv.AudioFormat, pcm []byte) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	buf.Grow(len(pcm) + 1024)
	w := multipart.NewWriter(&buf)
	fields := [][2]string{{"model", model}}
	if stream {
		fields = append(fields, [2]string{"stream", "true"})
	} else {
		fields = append(fields, [2]string{"response_format", "verbose_json"})
	}
	// Temperature 0 makes the recognition greedy, so the same audio gives the
	// same text instead of whatever the model file defaults to.
	fields = append(fields, [2]string{"temperature", "0"})
	if language != "" {
		fields = append(fields, [2]string{"language", language})
	}
	for _, kv := range fields {
		if err := w.WriteField(kv[0], kv[1]); err != nil {
			return nil, "", contracts.Wrap(contracts.ErrInternal, err, "localai voice: cannot encode the request")
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", contracts.Wrap(contracts.ErrInternal, err, "localai voice: cannot encode the request")
	}
	_, _ = part.Write(wavHeader(f, len(pcm)))
	_, _ = part.Write(pcm)
	if err := w.Close(); err != nil {
		return nil, "", contracts.Wrap(contracts.ErrInternal, err, "localai voice: cannot encode the request")
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// transcriptWire is the JSON answer of a non-streaming transcription. Segment
// and word timings are ignored: nothing downstream uses them, and the duration
// reported to callers is the one of the audio that was sent.
type transcriptWire struct {
	Text     *string `json:"text"`
	Language string  `json:"language"`
}

// decodeBody reads a capped body and decodes it as one JSON document.
func (c *Client) decodeBody(ctx context.Context, body io.Reader, out any) error {
	raw, err := io.ReadAll(newCapReader(body, c.maxResp))
	if err != nil {
		return mapTransportError(ctx, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return schemaInvalid("response is not the expected JSON document")
	}
	return nil
}

// Transcribe implements voice.STT: one POST of the whole buffer.
func (c *Client) Transcribe(ctx context.Context, scope contracts.Scope, req pv.TranscribeRequest) (pv.Transcript, error) {
	if err := scope.Validate(); err != nil {
		return pv.Transcript{}, err
	}
	if err := req.Validate(c.limits); err != nil {
		return pv.Transcript{}, err
	}
	model, err := c.model(providers.RoleSTT, req.Profile)
	if err != nil {
		return pv.Transcript{}, err
	}
	body, ct, err := buildTranscribeBody(model, primaryLanguage(req.Language), false, req.Format, req.Audio)
	if err != nil {
		return pv.Transcript{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	started := c.clk.Now()
	resp, err := c.post(ctx, pathTranscribe, ct, "application/json", body)
	if err != nil {
		return pv.Transcript{}, err
	}
	defer resp.Body.Close()
	var out transcriptWire
	if err := c.decodeBody(ctx, resp.Body, &out); err != nil {
		return pv.Transcript{}, err
	}
	if out.Text == nil {
		return pv.Transcript{}, schemaInvalid("transcription answer has no text field")
	}
	return pv.Transcript{
		Text:     strings.TrimSpace(*out.Text),
		Language: reportedLanguage(out.Language),
		Duration: req.Format.DurationOf(len(req.Audio)),
		Timings:  providers.Timings{Total: c.clk.Now().Sub(started)},
		TraceID:  traceID(resp.Header),
		Model:    model,
	}, nil
}

// TranscribeStream implements voice.STT.
//
// LocalAI's transcription route takes a complete file, so the live input side
// cannot be streamed to it (live audio in is what the Realtime adapter is
// for). The stream therefore holds the audio in memory, bounded by the limits,
// until CloseSend; then the first Recv sends it with "stream": true and relays
// the runtime's incremental output as interim events, followed by the final
// transcript and Done. No call touches the network before CloseSend, and no
// goroutine is started: Recv does all of the waiting.
func (c *Client) TranscribeStream(ctx context.Context, scope contracts.Scope, req pv.STTStreamRequest) (pv.STTStream, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if err := c.limits.Validate(); err != nil {
		return nil, err
	}
	model, err := c.model(providers.RoleSTT, req.Profile)
	if err != nil {
		return nil, err
	}
	return &sttStream{
		c:          c,
		ctx:        ctx,
		format:     req.Format,
		language:   primaryLanguage(req.Language),
		model:      model,
		sendClosed: make(chan struct{}),
		closedCh:   make(chan struct{}),
	}, nil
}

// sttEvent is one data event of a streaming transcription (LocalAI v4.10.0,
// core/http/endpoints/openai/transcription.go).
type sttEvent struct {
	Type     string    `json:"type"`
	Delta    string    `json:"delta"`
	Text     *string   `json:"text"`
	Language string    `json:"language"`
	Error    *apiError `json:"error"`
}

// sttStream implements voice.STTStream. Send and CloseSend are for one sender
// and Recv for one receiver; Close may be called at any time.
type sttStream struct {
	c        *Client
	ctx      context.Context
	format   pv.AudioFormat
	language string
	model    string

	mu       sync.Mutex // guards buf, sendDone, cancel and body
	buf      []byte
	sendDone bool
	cancel   context.CancelFunc
	body     io.ReadCloser

	sendClosed chan struct{} // closed by CloseSend
	closedCh   chan struct{} // closed by Close
	closed     atomic.Bool
	closeOnce  sync.Once

	// Receiver state.
	started    bool
	began      time.Time
	rctx       context.Context
	sc         *bufio.Scanner
	queue      []pv.TranscriptEvent
	seq        int
	acc        strings.Builder
	firstDelta time.Time
	sawDone    bool
	doneText   string
	doneLang   string
	trace      string
	audioDur   time.Duration
	finished   bool
	err        error
}

// Send implements voice.STTStream.
func (s *sttStream) Send(audio []byte) error {
	if s.closed.Load() {
		return providers.ErrStreamClosed
	}
	if err := s.ctx.Err(); err != nil {
		return providers.ContextError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendDone {
		return contracts.New(contracts.ErrValidationFailed, "localai voice: Send after CloseSend")
	}
	if err := s.c.limits.CheckChunk(s.format, audio, len(s.buf)); err != nil {
		return err
	}
	s.buf = append(s.buf, audio...)
	return nil
}

// CloseSend implements voice.STTStream.
func (s *sttStream) CloseSend() error {
	if s.closed.Load() {
		return providers.ErrStreamClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sendDone {
		s.sendDone = true
		close(s.sendClosed)
	}
	return nil
}

// Close implements voice.STTStream. It is idempotent, drops any buffered audio
// and unblocks a pending Recv.
func (s *sttStream) Close() error {
	s.closed.Store(true)
	s.closeOnce.Do(func() { close(s.closedCh) })
	s.free()
	return nil
}

func (s *sttStream) free() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = nil
	if s.cancel != nil {
		s.cancel()
	}
	if s.body != nil {
		_ = s.body.Close()
	}
}

// fail ends the stream with err. Later Recv calls return the same error.
func (s *sttStream) fail(err error) (pv.TranscriptEvent, error) {
	if s.closed.Load() {
		return pv.TranscriptEvent{}, providers.ErrStreamClosed
	}
	s.err = err
	s.free()
	return pv.TranscriptEvent{}, err
}

// push queues an event with the next sequence number.
func (s *sttStream) push(ev pv.TranscriptEvent) {
	s.seq++
	ev.Seq = s.seq
	s.queue = append(s.queue, ev)
}

// open sends the buffered audio. With no audio at all there is nothing to
// recognise: the stream completes at once with an empty transcript (what
// silence recognises as) without calling the runtime.
func (s *sttStream) open() error {
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return providers.ErrStreamClosed
	}
	audio := s.buf
	s.buf = nil
	s.started = true
	s.began = s.c.clk.Now()
	if len(audio) == 0 {
		s.mu.Unlock()
		s.finish()
		return nil
	}
	s.audioDur = s.format.DurationOf(len(audio))
	rctx, cancel := context.WithTimeout(s.ctx, s.c.timeout)
	s.cancel = cancel
	s.rctx = rctx
	s.mu.Unlock()

	body, ct, err := buildTranscribeBody(s.model, s.language, true, s.format, audio)
	if err != nil {
		return err
	}
	resp, err := s.c.post(rctx, pathTranscribe, ct, "text/event-stream, application/json", body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		_ = resp.Body.Close()
		return providers.ErrStreamClosed
	}
	s.body = resp.Body
	s.mu.Unlock()
	s.trace = traceID(resp.Header)

	switch mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt {
	case "text/event-stream":
		s.sc = bufio.NewScanner(newCapReader(resp.Body, s.c.maxResp))
		s.sc.Buffer(make([]byte, 0, 4096), maxSSELine)
	case "application/json":
		// A runtime that ignored "stream": the whole answer is one final event.
		var out transcriptWire
		if err := s.c.decodeBody(rctx, resp.Body, &out); err != nil {
			return err
		}
		if out.Text == nil {
			return schemaInvalid("transcription answer has no text field")
		}
		s.sawDone, s.doneText, s.doneLang = true, *out.Text, out.Language
		s.finish()
	default:
		return schemaInvalid("runtime answered with neither an event stream nor JSON")
	}
	return nil
}

// finish queues the final transcript (when there is text) and the Done event.
func (s *sttStream) finish() {
	if text := strings.TrimSpace(s.doneText); text != "" {
		s.push(pv.TranscriptEvent{Text: text, Final: true, Offset: s.audioDur})
	}
	t := providers.Timings{Total: s.c.clk.Now().Sub(s.began)}
	if !s.firstDelta.IsZero() {
		t.TimeToFirstToken = s.firstDelta.Sub(s.began)
	}
	s.push(pv.TranscriptEvent{
		Done:     true,
		Language: reportedLanguage(s.doneLang),
		Offset:   s.audioDur,
		Timings:  t,
		TraceID:  s.trace,
		Model:    s.model,
	})
}

// nextPayload returns the data of the next server-sent event: the "data" lines
// of one event joined by newlines. Comments, other fields and empty events are
// skipped, and an event cut off by the end of the body is dropped.
func (s *sttStream) nextPayload() (string, error) {
	var data []string
	for s.sc.Scan() {
		line := strings.TrimSuffix(s.sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
		case line[0] == ':':
		default:
			field, value, _ := strings.Cut(line, ":")
			if field == "data" {
				data = append(data, strings.TrimPrefix(value, " "))
			}
		}
	}
	if err := s.sc.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

// Recv implements voice.STTStream. The transcript is complete only when its
// Done event was delivered: the runtime must have sent both the final
// "transcript.text.done" event and the [DONE] marker.
func (s *sttStream) Recv() (pv.TranscriptEvent, error) {
	if s.closed.Load() {
		return pv.TranscriptEvent{}, providers.ErrStreamClosed
	}
	if s.err != nil {
		return pv.TranscriptEvent{}, s.err
	}
	if s.finished {
		return pv.TranscriptEvent{}, io.EOF
	}
	if !s.started {
		select {
		case <-s.sendClosed:
		case <-s.closedCh:
			return pv.TranscriptEvent{}, providers.ErrStreamClosed
		case <-s.ctx.Done():
			return s.fail(providers.ContextError(s.ctx.Err()))
		}
		if err := s.open(); err != nil {
			return s.fail(err)
		}
	}
	for len(s.queue) == 0 {
		if err := s.readEvent(); err != nil {
			return s.fail(err)
		}
	}
	ev := s.queue[0]
	s.queue = s.queue[1:]
	if ev.Done {
		s.finished = true
		s.free()
	}
	return ev, nil
}

// readEvent consumes one server-sent event and queues what it yields.
func (s *sttStream) readEvent() error {
	payload, err := s.nextPayload()
	switch {
	case errors.Is(err, io.EOF):
		return interrupted("transcript stream ended before completion")
	case err != nil:
		return mapTransportError(s.rctx, err)
	}
	if payload == "[DONE]" {
		if !s.sawDone {
			return schemaInvalid("stream ended without a final transcript")
		}
		s.finish()
		return nil
	}
	var ev sttEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return schemaInvalid("stream event is not valid JSON")
	}
	switch {
	case ev.Error != nil:
		return mapEventError(*ev.Error)
	case ev.Type == "error":
		return mapEventError(apiError{})
	case s.sawDone && (ev.Type == "transcript.text.delta" || ev.Type == "transcript.text.done"):
		return schemaInvalid("stream carried an event after its final transcript")
	case ev.Type == "transcript.text.delta":
		if ev.Delta == "" {
			return nil
		}
		if s.firstDelta.IsZero() {
			s.firstDelta = s.c.clk.Now()
		}
		// Deltas are increments; an interim event carries the hypothesis so
		// far, which replaces the previous one.
		s.acc.WriteString(ev.Delta)
		s.push(pv.TranscriptEvent{Text: s.acc.String()})
	case ev.Type == "transcript.text.done":
		if ev.Text == nil {
			return schemaInvalid("final transcript event has no text")
		}
		s.sawDone, s.doneText, s.doneLang = true, *ev.Text, ev.Language
	}
	return nil
}
