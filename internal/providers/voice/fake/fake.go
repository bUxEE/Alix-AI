// Package fake is the deterministic speech runtime used by every voice test.
// One *Provider implements voice.STT, voice.TTS, voice.VAD and
// providers.AIRuntimeProvider, so a single value can be registered for all
// three voice roles (or for any subset).
//
//	f := fake.New()
//	f.Script("book a table for two")             // what the next recognition hears
//	st, _ := f.TranscribeStream(ctx, scope, voice.STTStreamRequest{Format: fmt)
//	st.Send(fake.Tone(fmt, 200*time.Millisecond, 3000)) // interim events follow
//	st.CloseSend()                                       // finals and Done follow
//
// It behaves like a real adapter where that matters: it validates the scope,
// the request and the audio (fail closed), enforces voice.Limits, never
// returns partial output as a success, and honours context cancellation and
// Close mid-stream. An STT call with nothing scripted is an error, never a
// default transcript, so a test cannot pass by accident. TTS audio is a pure
// function of the text, and VAD is a small energy detector over the PCM it is
// given, so both are deterministic without scripting.
//
// Streams own no goroutine: all waiting happens in the caller's Recv, so a
// cancelled or closed stream leaves nothing running (OpenStreams reports any
// that were never ended). The fake keeps sizes and text, never audio samples:
// raw audio is not retained.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// FailKind is an injectable failure.
type FailKind string

const (
	// FailTimeout fails at once with timeout (wrapping
	// context.DeadlineExceeded). It does not sleep; use StallAfter to make a
	// call hang until its context ends.
	FailTimeout FailKind = "timeout"
	// FailUnavailable fails at once with unavailable (connection refused).
	FailUnavailable FailKind = "unavailable"
	// FailResourceExhausted fails at once with resource_exhausted (the model
	// could not be loaded).
	FailResourceExhausted FailKind = "resource_exhausted"
	// FailMalformed fails at once with schema_invalid (the runtime answered
	// something unusable).
	FailMalformed FailKind = "malformed"
	// FailPartialStream cuts a stream part way with unavailable: after the
	// first event of a recognition, after about half the chunks of a
	// synthesis. A call that has no stream fails with unavailable straight
	// away.
	FailPartialStream FailKind = "partial_stream"
)

func (k FailKind) valid() bool {
	switch k {
	case FailTimeout, FailUnavailable, FailResourceExhausted, FailMalformed, FailPartialStream:
		return true
	}
	return false
}

func injected(kind FailKind) error {
	switch kind {
	case FailTimeout:
		return contracts.Wrap(contracts.ErrTimeout, context.DeadlineExceeded, "fake: injected timeout")
	case FailResourceExhausted:
		return contracts.New(contracts.ErrResourceExhausted, "fake: injected resource exhaustion")
	case FailMalformed:
		return contracts.New(contracts.ErrSchemaInvalid, "fake: injected malformed runtime response")
	case FailPartialStream:
		return contracts.New(contracts.ErrUnavailable, "fake: injected connection drop mid-stream")
	}
	return contracts.New(contracts.ErrUnavailable, "fake: injected runtime unavailable")
}

// Op is the kind of call a Call records.
type Op string

const (
	OpTranscribe       Op = "transcribe"
	OpTranscribeStream Op = "transcribe_stream"
	OpSynthesize       Op = "synthesize"
	OpDetect           Op = "detect"
)

// Call is one call as it reached the "runtime": the scope it ran under and what
// was asked. Only calls that passed scope and request validation are recorded;
// a rejected call never reached the runtime. Audio itself is not kept, only its
// size.
type Call struct {
	Seq        int
	Op         Op
	Scope      contracts.Scope
	Format     voice.AudioFormat
	Language   string
	Voice      string // OpSynthesize
	Text       string // OpSynthesize: the text to speak
	AudioBytes int    // OpTranscribe, OpDetect: size of the buffer
}

// Option configures New.
type Option func(*Provider)

// WithName sets the name reported through AIRuntimeProvider (default
// "fake-voice").
func WithName(name string) Option { return func(p *Provider) { p.name = name } }

// WithModel sets the model id reported in results (default "fake-voice-model").
func WithModel(model string) Option { return func(p *Provider) { p.model = model } }

// WithLanguage sets the language reported by recognitions that were not given
// one (default "en").
func WithLanguage(tag string) Option { return func(p *Provider) { p.language = tag } }

// WithLimits replaces voice.DefaultLimits. It panics on an invalid Limits: that
// is a bug in the test.
func WithLimits(l voice.Limits) Option {
	if err := l.Validate(); err != nil {
		panic(fmt.Sprintf("fake: WithLimits: %v", err))
	}
	return func(p *Provider) { p.limits = l }
}

// WithInterimWords sets how many more words each Send on a recognition stream
// reveals in the interim hypothesis (default 1).
func WithInterimWords(n int) Option {
	return func(p *Provider) {
		if n > 0 {
			p.interimWords = n
		}
	}
}

// WithSynthChunks sets how many runes of text each synthesized chunk covers and
// how long each chunk plays (defaults 16 runes and 20 ms).
func WithSynthChunks(runes int, play time.Duration) Option {
	return func(p *Provider) {
		if runes > 0 {
			p.runesPerChunk = runes
		}
		if play > 0 {
			p.chunkPlay = play
		}
	}
}

// WithSynthFormat sets the output format used when a request leaves it unset
// (default 24 kHz mono PCM16).
func WithSynthFormat(f voice.AudioFormat) Option {
	return func(p *Provider) { p.synthFormat = f }
}

// WithVADThreshold sets the peak amplitude (0 to 32767) from which a 10 ms
// frame counts as speech (default 500).
func WithVADThreshold(amplitude int) Option {
	return func(p *Provider) {
		if amplitude > 0 {
			p.vadThreshold = amplitude
		}
	}
}

// Provider is the deterministic fake speech runtime. It is safe for concurrent
// use.
type Provider struct {
	name          string
	model         string
	language      string
	limits        voice.Limits
	interimWords  int
	runesPerChunk int
	chunkPlay     time.Duration
	synthFormat   voice.AudioFormat
	vadThreshold  int

	mu          sync.Mutex
	scripts     [][]string // STT utterances, each a list of final segments
	sttServed   int        // STT calls that consumed a script
	fails       []FailKind
	stall       int // -1: never; n: streams (and batch calls) hang after n items
	calls       []Call
	seq         int
	health      providers.HealthReport
	openStreams int
	chunksSent  int
	audioIn     int
}

var (
	_ voice.STT                   = (*Provider)(nil)
	_ voice.TTS                   = (*Provider)(nil)
	_ voice.VAD                   = (*Provider)(nil)
	_ providers.AIRuntimeProvider = (*Provider)(nil)
)

// New returns a fake with nothing scripted.
func New(opts ...Option) *Provider {
	p := &Provider{
		name: "fake-voice", model: "fake-voice-model", language: "en",
		limits: voice.DefaultLimits(), interimWords: 1,
		runesPerChunk: 16, chunkPlay: 20 * time.Millisecond,
		synthFormat:  voice.PCM16Mono(24000),
		vadThreshold: 500, stall: -1,
		health: providers.HealthReport{Status: providers.HealthOK, Version: "fake"},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Script queues what the next recognition hears: one utterance made of one or
// more final segments (a recognition's transcript is its segments joined by a
// space). Successive recognitions, batch or streaming, consume successive
// scripts and the last one repeats. It panics without a segment: that is a bug
// in the test.
func (p *Provider) Script(segments ...string) {
	if len(segments) == 0 {
		panic("fake: Script needs at least one segment")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts = append(p.scripts, append([]string(nil), segments...))
}

// FailNext makes the next call (of any kind) fail with kind. Failures queue:
// two FailNext calls fail the next two calls, in order.
func (p *Provider) FailNext(kind FailKind) { p.FailTimes(kind, 1) }

// FailTimes queues n failures of kind.
func (p *Provider) FailTimes(kind FailKind, n int) {
	if !kind.valid() {
		panic(fmt.Sprintf("fake: unknown FailKind %q", string(kind)))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < n; i++ {
		p.fails = append(p.fails, kind)
	}
}

// StallAfter simulates a hung runtime for calls made from now on: a stream
// delivers n items and then blocks until its context ends or it is closed; a
// batch call blocks until its context ends. A negative n turns it off.
func (p *Provider) StallAfter(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stall = n
}

// Calls returns every call that reached the runtime, oldest first.
func (p *Provider) Calls() []Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Call, len(p.calls))
	for i, c := range p.calls {
		c.Scope = c.Scope.Clone()
		out[i] = c
	}
	return out
}

// CallCount returns the number of calls that reached the runtime.
func (p *Provider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// Reset removes scripts, queued failures, the stall and recorded calls.
func (p *Provider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts, p.fails, p.calls, p.stall, p.sttServed = nil, nil, nil, -1, 0
}

// OpenStreams returns the number of streams started and not yet ended
// (completed, failed, cancelled or closed). A test that leaves it above zero
// leaked a stream.
func (p *Provider) OpenStreams() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.openStreams
}

// ChunksDelivered returns the number of stream items (transcript events and
// audio chunks) handed to callers.
func (p *Provider) ChunksDelivered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chunksSent
}

// AudioBytesReceived returns the audio the runtime was given (batch buffers
// and streamed chunks). The audio itself is not retained.
func (p *Provider) AudioBytesReceived() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.audioIn
}

// Name implements providers.AIRuntimeProvider.
func (p *Provider) Name() string { return p.name }

// Health implements providers.AIRuntimeProvider; it returns what SetHealth
// last set (ok by default).
func (p *Provider) Health(ctx context.Context) (providers.HealthReport, error) {
	if err := ctx.Err(); err != nil {
		return providers.HealthReport{}, providers.ContextError(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.health, nil
}

// SetHealth sets what Health reports.
func (p *Provider) SetHealth(status providers.Health, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.health = providers.HealthReport{Status: status, Reason: reason, Version: "fake"}
}

// begin records an accepted call and takes the next queued failure. The caller
// has already validated scope and request.
func (p *Provider) begin(c Call) (seq int, fail FailKind, stall int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	c.Seq = p.seq
	c.Scope = c.Scope.Clone()
	p.calls = append(p.calls, c)
	if len(p.fails) > 0 {
		fail, p.fails = p.fails[0], p.fails[1:]
	}
	return p.seq, fail, p.stall
}

func (p *Provider) traceID(seq int) string { return fmt.Sprintf("fake-voice-%06d", seq) }

// hang blocks until ctx ends: the simulated hung runtime of a batch call.
func hang(ctx context.Context) error {
	<-ctx.Done()
	return providers.ContextError(ctx.Err())
}
