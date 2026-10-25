package fake_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// bound is how long a test waits for something that must happen. It is only
// reached when the test is about to fail.
const bound = 5 * time.Second

// quiet is how long a test watches for something that must NOT happen. A
// negative assertion can only be a bounded wait; a flake here is a false pass,
// never a false failure.
const quiet = 60 * time.Millisecond

var pcm16k = voice.PCM16Mono(16000)

func scope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_1",
		ApplicationID:    "app_clinic",
		TenantID:         "ten_1",
		WorkspaceID:      "ws_main",
		PrincipalID:      "usr_1",
		Roles:            []string{"receptionist"},
		CapabilityScopes: []string{"booking.create"},
		SessionID:        "ses_1",
	}
}

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s, want %s (%v)", got, code, err)
	}
}

type recvResult[T any] struct {
	v   T
	err error
}

// recvAsync starts one Recv in its own goroutine.
func recvAsync[T any](recv func() (T, error)) <-chan recvResult[T] {
	ch := make(chan recvResult[T], 1)
	go func() {
		v, err := recv()
		ch <- recvResult[T]{v, err}
	}()
	return ch
}

func mustBlock[T any](t *testing.T, ch <-chan recvResult[T], what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s: Recv returned (%+v, %v) but must still be waiting", what, r.v, r.err)
	case <-time.After(quiet):
	}
}

func mustReturn[T any](t *testing.T, ch <-chan recvResult[T], what string) recvResult[T] {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(bound):
		t.Fatalf("%s: Recv did not return within %v", what, bound)
		panic("unreachable")
	}
}

func openSTT(t *testing.T, f *fake.Provider, ctx context.Context) voice.STTStream {
	t.Helper()
	st, err := f.TranscribeStream(ctx, scope(), voice.STTStreamRequest{Format: pcm16k, Language: "en"})
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFakeSTTStreaming(t *testing.T) {
	ctx := context.Background()
	chunk := fake.Tone(pcm16k, 100*time.Millisecond, 3000)

	t.Run("interim events before CloseSend, finals only after", func(t *testing.T) {
		f := fake.New(fake.WithInterimWords(2))
		f.Script("book a table", "for two")
		st := openSTT(t, f, ctx)

		var interim []voice.TranscriptEvent
		for i := 0; i < 2; i++ {
			if err := st.Send(chunk); err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
			ev, err := st.Recv()
			if err != nil {
				t.Fatalf("Recv interim %d: %v", i, err)
			}
			interim = append(interim, ev)
		}
		wantInterim := []voice.TranscriptEvent{
			{Seq: 1, Text: "book a", Language: "en", Offset: 100 * time.Millisecond},
			{Seq: 2, Text: "book a table for", Language: "en", Offset: 200 * time.Millisecond},
		}
		if diff := cmp.Diff(wantInterim, interim); diff != "" {
			t.Fatalf("interim events (-want +got):\n%s", diff)
		}

		// Nothing final exists yet: the next Recv has to wait for CloseSend.
		next := recvAsync(st.Recv)
		mustBlock(t, next, "before CloseSend")
		if err := st.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		first := mustReturn(t, next, "after CloseSend")
		if first.err != nil || !first.v.Final || first.v.Text != "book a table" || first.v.Seq != 3 {
			t.Fatalf("first event after CloseSend = %+v, %v; want final \"book a table\" seq 3", first.v, first.err)
		}
		second, err := st.Recv()
		if err != nil || !second.Final || second.Text != "for two" {
			t.Fatalf("second final = %+v, %v", second, err)
		}
		done, err := st.Recv()
		if err != nil || !done.Done || done.Text != "" || done.Model != "fake-voice-model" || done.TraceID == "" {
			t.Fatalf("done event = %+v, %v", done, err)
		}
		if _, err := st.Recv(); err != io.EOF {
			t.Fatalf("Recv after Done = %v, want io.EOF", err)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d after the stream completed", n)
		}
		if got, want := f.AudioBytesReceived(), 2*len(chunk); got != want {
			t.Fatalf("AudioBytesReceived = %d, want %d", got, want)
		}
	})

	t.Run("CollectTranscript joins the finals and ignores interims", func(t *testing.T) {
		f := fake.New()
		f.Script("book a table", "for two")
		st := openSTT(t, f, ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // the sender, concurrent with the collector
			defer wg.Done()
			for i := 0; i < 3; i++ {
				if err := st.Send(chunk); err != nil {
					t.Errorf("Send: %v", err)
					return
				}
			}
			if err := st.CloseSend(); err != nil {
				t.Errorf("CloseSend: %v", err)
			}
		}()
		tr, err := voice.CollectTranscript(st)
		wg.Wait()
		if err != nil {
			t.Fatalf("CollectTranscript: %v", err)
		}
		if tr.Text != "book a table for two" || tr.Language != "en" || tr.Duration != 300*time.Millisecond {
			t.Fatalf("transcript = %+v", tr)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d after CollectTranscript", n)
		}
	})

	t.Run("a recognition that heard nothing is an empty success", func(t *testing.T) {
		f := fake.New()
		f.Script("")
		st := openSTT(t, f, ctx)
		if err := st.CloseSend(); err != nil {
			t.Fatal(err)
		}
		tr, err := voice.CollectTranscript(st)
		if err != nil || tr.Text != "" {
			t.Fatalf("silence transcript = %+v, %v", tr, err)
		}
	})

	t.Run("a stream cut part way is an error, never a transcript", func(t *testing.T) {
		f := fake.New()
		f.Script("book a table", "for two")
		f.FailNext(fake.FailPartialStream)
		st := openSTT(t, f, ctx)
		if err := st.Send(chunk); err != nil {
			t.Fatal(err)
		}
		if err := st.CloseSend(); err != nil {
			t.Fatal(err)
		}
		tr, err := voice.CollectTranscript(st)
		wantCode(t, err, contracts.ErrUnavailable)
		if tr.Text != "" {
			t.Fatalf("a failed stream leaked text %q", tr.Text)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d", n)
		}
	})

	t.Run("cancelling the context mid-stream ends Recv and Send", func(t *testing.T) {
		f := fake.New()
		f.Script("book a table for two")
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		st := openSTT(t, f, cctx)
		if err := st.Send(chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Recv(); err != nil {
			t.Fatal(err)
		}
		waiting := recvAsync(st.Recv)
		mustBlock(t, waiting, "waiting for audio")
		cancel()
		r := mustReturn(t, waiting, "after cancel")
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Recv after cancel = %v, want context.Canceled", r.err)
		}
		if err := st.Send(chunk); !errors.Is(err, context.Canceled) {
			t.Fatalf("Send after cancel = %v, want context.Canceled", err)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d after cancel", n)
		}
	})

	t.Run("Close unblocks a waiting Recv", func(t *testing.T) {
		f := fake.New()
		f.Script("hello")
		st := openSTT(t, f, ctx)
		waiting := recvAsync(st.Recv)
		mustBlock(t, waiting, "waiting for audio")
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		r := mustReturn(t, waiting, "after Close")
		if !errors.Is(r.err, providers.ErrStreamClosed) {
			t.Fatalf("Recv after Close = %v, want ErrStreamClosed", r.err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("second Close = %v, want nil (idempotent)", err)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d after Close", n)
		}
	})

	t.Run("audio is validated and bounded", func(t *testing.T) {
		f := fake.New(fake.WithLimits(voice.Limits{MaxAudioBytes: 6400, MaxChunkBytes: 3200, MaxTextRunes: 100}))
		f.Script("hello")
		st := openSTT(t, f, ctx)
		wantCode(t, st.Send(nil), contracts.ErrValidationFailed)
		wantCode(t, st.Send(make([]byte, 3)), contracts.ErrValidationFailed)    // not whole frames
		wantCode(t, st.Send(make([]byte, 3202)), contracts.ErrValidationFailed) // over the chunk cap
		if err := st.Send(make([]byte, 3200)); err != nil {
			t.Fatal(err)
		}
		if err := st.Send(make([]byte, 3200)); err != nil {
			t.Fatal(err)
		}
		wantCode(t, st.Send(make([]byte, 2)), contracts.ErrValidationFailed) // over the stream cap
		if err := st.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if err := st.CloseSend(); err != nil {
			t.Fatalf("second CloseSend = %v, want nil", err)
		}
		wantCode(t, st.Send(make([]byte, 2)), contracts.ErrValidationFailed) // after CloseSend
	})

	t.Run("requests fail closed", func(t *testing.T) {
		f := fake.New()
		f.Script("hello")
		_, err := f.TranscribeStream(ctx, contracts.Scope{}, voice.STTStreamRequest{Format: pcm16k})
		wantCode(t, err, contracts.ErrScopeMissing)
		_, err = f.Transcribe(ctx, contracts.Scope{}, voice.TranscribeRequest{Audio: chunk, Format: pcm16k})
		wantCode(t, err, contracts.ErrScopeMissing)
		_, err = f.TranscribeStream(ctx, scope(), voice.STTStreamRequest{Format: voice.AudioFormat{Encoding: "mp3", SampleRate: 16000, Channels: 1}})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = f.TranscribeStream(ctx, scope(), voice.STTStreamRequest{Format: pcm16k, Language: "not a tag"})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = f.Transcribe(ctx, scope(), voice.TranscribeRequest{Format: pcm16k}) // no audio
		wantCode(t, err, contracts.ErrValidationFailed)
		if n := f.CallCount(); n != 0 {
			t.Fatalf("rejected calls were recorded as sent: %d", n)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d after rejected calls", n)
		}
		g := fake.New() // nothing scripted
		_, err = g.TranscribeStream(ctx, scope(), voice.STTStreamRequest{Format: pcm16k})
		wantCode(t, err, contracts.ErrInternal)
		if g.OpenStreams() != 0 {
			t.Fatalf("an unscripted open leaked a stream")
		}
	})

	t.Run("batch transcription", func(t *testing.T) {
		f := fake.New()
		f.Script("first")
		f.Script("second", "part")
		tr, err := f.Transcribe(ctx, scope(), voice.TranscribeRequest{Audio: chunk, Format: pcm16k, Language: "it"})
		if err != nil || tr.Text != "first" || tr.Language != "it" || tr.Duration != 100*time.Millisecond {
			t.Fatalf("first = %+v, %v", tr, err)
		}
		tr, err = f.Transcribe(ctx, scope(), voice.TranscribeRequest{Audio: chunk, Format: pcm16k})
		if err != nil || tr.Text != "second part" || tr.Language != "en" {
			t.Fatalf("second = %+v, %v", tr, err)
		}
		tr, _ = f.Transcribe(ctx, scope(), voice.TranscribeRequest{Audio: chunk, Format: pcm16k})
		if tr.Text != "second part" {
			t.Fatalf("the last script must repeat, got %q", tr.Text)
		}
		calls := f.Calls()
		if len(calls) != 3 || calls[0].Op != fake.OpTranscribe || calls[0].AudioBytes != len(chunk) || calls[0].Scope.TenantID != "ten_1" {
			t.Fatalf("recorded calls = %+v", calls)
		}

		for kind, code := range map[fake.FailKind]contracts.ErrCode{
			fake.FailTimeout:           contracts.ErrTimeout,
			fake.FailUnavailable:       contracts.ErrUnavailable,
			fake.FailResourceExhausted: contracts.ErrResourceExhausted,
			fake.FailMalformed:         contracts.ErrSchemaInvalid,
		} {
			f.FailNext(kind)
			_, err := f.Transcribe(ctx, scope(), voice.TranscribeRequest{Audio: chunk, Format: pcm16k})
			wantCode(t, err, code)
		}

		f.StallAfter(0) // a hung runtime ends with the caller's deadline
		dctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		_, err = f.Transcribe(dctx, scope(), voice.TranscribeRequest{Audio: chunk, Format: pcm16k})
		wantCode(t, err, contracts.ErrTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout must wrap context.DeadlineExceeded: %v", err)
		}
	})
}

func openTTS(t *testing.T, f *fake.Provider, ctx context.Context, text string) voice.AudioStream {
	t.Helper()
	st, err := f.SynthesizeStream(ctx, scope(), voice.SynthesizeRequest{Text: text})
	if err != nil {
		t.Fatalf("SynthesizeStream: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// speech is 160 runes: ten chunks at the default 16 runes per chunk.
var speech = strings.Repeat("0123456789abcdef", 10)

func TestFakeTTSCancellable(t *testing.T) {
	ctx := context.Background()

	t.Run("cancelling the context stops the audio at once", func(t *testing.T) {
		f := fake.New()
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		st := openTTS(t, f, cctx, speech)
		for i := 1; i <= 3; i++ {
			c, err := st.Recv()
			if err != nil || c.Seq != i || c.Done || len(c.Data) == 0 {
				t.Fatalf("chunk %d = %+v, %v", i, c, err)
			}
		}
		cancel()
		delivered := f.ChunksDelivered()
		for i := 0; i < 3; i++ {
			if _, err := st.Recv(); !errors.Is(err, context.Canceled) {
				t.Fatalf("Recv after cancel = %v, want context.Canceled", err)
			}
		}
		if got := f.ChunksDelivered(); got != delivered {
			t.Fatalf("chunks kept flowing after cancel: %d -> %d", delivered, got)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d after cancel", n)
		}
	})

	t.Run("Close cancels playback and unblocks a waiting Recv", func(t *testing.T) {
		f := fake.New()
		f.StallAfter(1) // the runtime hangs after the first chunk
		st := openTTS(t, f, ctx, speech)
		if c, err := st.Recv(); err != nil || c.Seq != 1 {
			t.Fatalf("first chunk = %+v, %v", c, err)
		}
		waiting := recvAsync(st.Recv)
		mustBlock(t, waiting, "stalled runtime")
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r := mustReturn(t, waiting, "after Close")
		if !errors.Is(r.err, providers.ErrStreamClosed) {
			t.Fatalf("Recv after Close = %v, want ErrStreamClosed", r.err)
		}
		if _, err := st.Recv(); !errors.Is(err, providers.ErrStreamClosed) {
			t.Fatalf("later Recv = %v, want ErrStreamClosed", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("second Close = %v, want nil (idempotent)", err)
		}
		if got := f.ChunksDelivered(); got != 1 {
			t.Fatalf("ChunksDelivered = %d, want 1: audio flowed after the cancel", got)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d after Close", n)
		}
	})

	t.Run("cancelling the context unblocks a stalled Recv", func(t *testing.T) {
		f := fake.New()
		f.StallAfter(0)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		st := openTTS(t, f, cctx, speech)
		waiting := recvAsync(st.Recv)
		mustBlock(t, waiting, "stalled runtime")
		cancel()
		r := mustReturn(t, waiting, "after cancel")
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Recv after cancel = %v, want context.Canceled", r.err)
		}
		if n := f.OpenStreams(); n != 0 || f.ChunksDelivered() != 0 {
			t.Fatalf("OpenStreams = %d, ChunksDelivered = %d", n, f.ChunksDelivered())
		}
	})

	t.Run("a deadline is a timeout", func(t *testing.T) {
		f := fake.New()
		f.StallAfter(0)
		dctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		st := openTTS(t, f, dctx, speech)
		_, err := st.Recv()
		wantCode(t, err, contracts.ErrTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout must wrap context.DeadlineExceeded: %v", err)
		}
	})

	t.Run("a complete synthesis is deterministic", func(t *testing.T) {
		f := fake.New()
		a, err := voice.CollectAudio(openTTS(t, f, ctx, speech), 1<<20)
		if err != nil {
			t.Fatalf("CollectAudio: %v", err)
		}
		b, err := voice.CollectAudio(openTTS(t, f, ctx, speech), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		other, err := voice.CollectAudio(openTTS(t, f, ctx, speech+"!"), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if a.Format != voice.PCM16Mono(24000) {
			t.Fatalf("default format = %+v", a.Format)
		}
		if want := 10 * a.Format.BytesFor(20*time.Millisecond); len(a.Data) != want {
			t.Fatalf("audio is %d bytes, want %d (ten 20 ms chunks)", len(a.Data), want)
		}
		if string(a.Data) != string(b.Data) {
			t.Fatal("the same text must give the same audio")
		}
		if string(a.Data) == string(other.Data) {
			t.Fatal("different text must give different audio")
		}
		if a.Model != "fake-voice-model" || a.TraceID == "" || a.Timings.Total != 200*time.Millisecond {
			t.Fatalf("final chunk metadata = %+v", a)
		}
		if n := f.OpenStreams(); n != 0 {
			t.Fatalf("OpenStreams = %d", n)
		}
	})

	t.Run("a stream cut part way is an error, never audio", func(t *testing.T) {
		f := fake.New()
		f.FailNext(fake.FailPartialStream)
		st := openTTS(t, f, ctx, speech)
		au, err := voice.CollectAudio(st, 1<<20)
		wantCode(t, err, contracts.ErrUnavailable)
		if len(au.Data) != 0 {
			t.Fatalf("a failed stream leaked %d bytes of audio", len(au.Data))
		}
		if got := f.ChunksDelivered(); got == 0 || got >= 10 {
			t.Fatalf("ChunksDelivered = %d, want a partial stream", got)
		}
		// One chunk of speech cannot be "half delivered": it fails outright.
		f.FailNext(fake.FailPartialStream)
		_, err = voice.CollectAudio(openTTS(t, f, ctx, "hi"), 1<<20)
		wantCode(t, err, contracts.ErrUnavailable)
	})

	t.Run("the requested format and chunk size are honoured", func(t *testing.T) {
		f := fake.New(fake.WithSynthChunks(4, 10*time.Millisecond))
		stereo := voice.AudioFormat{Encoding: voice.EncodingPCM16, SampleRate: 48000, Channels: 2}
		st, err := f.SynthesizeStream(ctx, scope(), voice.SynthesizeRequest{Text: "twelve runes", Format: stereo, Voice: "alloy", Language: "en-GB"})
		if err != nil {
			t.Fatal(err)
		}
		au, err := voice.CollectAudio(st, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if au.Format != stereo || len(au.Data) != 3*stereo.BytesFor(10*time.Millisecond) {
			t.Fatalf("format %+v, %d bytes", au.Format, len(au.Data))
		}
		call := f.Calls()[0]
		if call.Text != "twelve runes" || call.Voice != "alloy" || call.Language != "en-GB" || call.Op != fake.OpSynthesize {
			t.Fatalf("recorded call = %+v", call)
		}
	})

	t.Run("requests fail closed", func(t *testing.T) {
		f := fake.New(fake.WithLimits(voice.Limits{MaxAudioBytes: 1 << 20, MaxChunkBytes: 1 << 10, MaxTextRunes: 20}))
		cases := map[string]struct {
			scope contracts.Scope
			req   voice.SynthesizeRequest
			code  contracts.ErrCode
		}{
			"no scope":       {contracts.Scope{}, voice.SynthesizeRequest{Text: "hi"}, contracts.ErrScopeMissing},
			"empty text":     {scope(), voice.SynthesizeRequest{Text: "  "}, contracts.ErrValidationFailed},
			"too long":       {scope(), voice.SynthesizeRequest{Text: strings.Repeat("a", 21)}, contracts.ErrValidationFailed},
			"bad format":     {scope(), voice.SynthesizeRequest{Text: "hi", Format: voice.PCM16Mono(100)}, contracts.ErrValidationFailed},
			"path-like name": {scope(), voice.SynthesizeRequest{Text: "hi", Voice: "../etc/passwd"}, contracts.ErrValidationFailed},
			"bad language":   {scope(), voice.SynthesizeRequest{Text: "hi", Language: "EN_us"}, contracts.ErrValidationFailed},
		}
		for name, tc := range cases {
			_, err := f.SynthesizeStream(ctx, tc.scope, tc.req)
			if err == nil || contracts.CodeOf(err) != tc.code {
				t.Errorf("%s: err = %v, want code %s", name, err, tc.code)
			}
		}
		if f.CallCount() != 0 || f.OpenStreams() != 0 {
			t.Fatalf("rejected calls reached the runtime: calls %d, open %d", f.CallCount(), f.OpenStreams())
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := f.SynthesizeStream(cctx, scope(), voice.SynthesizeRequest{Text: "hi"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("an already cancelled context = %v", err)
		}
		f.FailNext(fake.FailUnavailable)
		_, err = f.SynthesizeStream(ctx, scope(), voice.SynthesizeRequest{Text: "hi"})
		wantCode(t, err, contracts.ErrUnavailable)
		if f.OpenStreams() != 0 {
			t.Fatal("a failed open leaked a stream")
		}
	})
}

func TestFakeVADSegments(t *testing.T) {
	ctx := context.Background()
	f := fake.New()
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	audio := fake.Speech(
		fake.Silence(pcm16k, ms(200)),
		fake.Tone(pcm16k, ms(300), 4000),
		fake.Silence(pcm16k, ms(50)), // a pause between words: bridged
		fake.Tone(pcm16k, ms(200), 4000),
		fake.Silence(pcm16k, ms(500)),
		fake.Tone(pcm16k, ms(100), 4000),
	)
	res, err := f.DetectSpeech(ctx, scope(), voice.DetectRequest{Audio: audio, Format: pcm16k})
	if err != nil {
		t.Fatalf("DetectSpeech: %v", err)
	}
	want := []voice.SpeechSegment{{Start: ms(200), End: ms(750)}, {Start: ms(1250), End: ms(1350)}}
	if diff := cmp.Diff(want, res.Segments); diff != "" {
		t.Fatalf("segments (-want +got):\n%s", diff)
	}

	noise := fake.Speech(fake.Silence(pcm16k, ms(300)), fake.Tone(pcm16k, ms(300), 100)) // under the threshold
	res, err = f.DetectSpeech(ctx, scope(), voice.DetectRequest{Audio: noise, Format: pcm16k})
	if err != nil || len(res.Segments) != 0 {
		t.Fatalf("silence and noise = %+v, %v; want no segments", res.Segments, err)
	}

	_, err = f.DetectSpeech(ctx, contracts.Scope{}, voice.DetectRequest{Audio: audio, Format: pcm16k})
	wantCode(t, err, contracts.ErrScopeMissing)
	f.FailNext(fake.FailUnavailable)
	_, err = f.DetectSpeech(ctx, scope(), voice.DetectRequest{Audio: audio, Format: pcm16k})
	wantCode(t, err, contracts.ErrUnavailable)

	hi := fake.New(fake.WithVADThreshold(5000))
	res, _ = hi.DetectSpeech(ctx, scope(), voice.DetectRequest{Audio: audio, Format: pcm16k})
	if len(res.Segments) != 0 {
		t.Fatalf("a higher threshold must reject the same tone, got %+v", res.Segments)
	}
}

func TestFakeRegistersForEveryVoiceRole(t *testing.T) {
	f := fake.New()
	reg := providers.NewRegistry(nil)
	for _, role := range voice.Roles() {
		if err := voice.Register(reg, role, f); err != nil {
			t.Fatalf("Register(%s): %v", role, err)
		}
	}
	reg.Refresh(context.Background())
	for _, role := range voice.Roles() {
		if st := reg.Status(role); st.Status != providers.HealthOK || st.Provider != "fake-voice" {
			t.Fatalf("%s status = %+v", role, st)
		}
	}
	f.SetHealth(providers.HealthUnavailable, "model not loaded")
	reg.Refresh(context.Background())
	if reg.Usable(providers.RoleTTS) {
		t.Fatal("an unhealthy fake must make the role unusable")
	}
	if _, err := voice.LookupSTT(reg); err != nil {
		t.Fatalf("LookupSTT: %v", err)
	}
}

func TestFakeConcurrentUse(t *testing.T) {
	f := fake.New()
	f.Script("one two three")
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				if _, err := voice.CollectAudio(openTTSNoT(f, ctx, speech), 1<<20); err != nil {
					t.Errorf("synthesize: %v", err)
				}
			case 1:
				if _, err := f.Transcribe(ctx, scope(), voice.TranscribeRequest{Audio: fake.Tone(pcm16k, 50*time.Millisecond, 1000), Format: pcm16k}); err != nil {
					t.Errorf("transcribe: %v", err)
				}
			default:
				if _, err := f.DetectSpeech(ctx, scope(), voice.DetectRequest{Audio: fake.Tone(pcm16k, 50*time.Millisecond, 1000), Format: pcm16k}); err != nil {
					t.Errorf("detect: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	if f.OpenStreams() != 0 || f.CallCount() != 16 {
		t.Fatalf("open streams %d, calls %d", f.OpenStreams(), f.CallCount())
	}
}

func openTTSNoT(f *fake.Provider, ctx context.Context, text string) voice.AudioStream {
	st, err := f.SynthesizeStream(ctx, scope(), voice.SynthesizeRequest{Text: text})
	if err != nil {
		panic(err)
	}
	return st
}
