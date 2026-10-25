package voice_test

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func wantCode(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s, want %s (%v)", got, code, err)
	}
}

func TestAudioFormatValidate(t *testing.T) {
	cases := []struct {
		name string
		f    voice.AudioFormat
		ok   bool
	}{
		{"16k mono", voice.PCM16Mono(16000), true},
		{"48k stereo", voice.AudioFormat{Encoding: voice.EncodingPCM16, SampleRate: 48000, Channels: 2}, true},
		{"zero value", voice.AudioFormat{}, false},
		{"unknown encoding", voice.AudioFormat{Encoding: "opus", SampleRate: 16000, Channels: 1}, false},
		{"rate too low", voice.PCM16Mono(7999), false},
		{"rate too high", voice.PCM16Mono(48001), false},
		{"no channels", voice.AudioFormat{Encoding: voice.EncodingPCM16, SampleRate: 16000}, false},
		{"three channels", voice.AudioFormat{Encoding: voice.EncodingPCM16, SampleRate: 16000, Channels: 3}, false},
	}
	for _, tc := range cases {
		err := tc.f.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil {
			wantCode(t, err, contracts.ErrValidationFailed)
		}
	}
	f := voice.AudioFormat{Encoding: voice.EncodingPCM16, SampleRate: 16000, Channels: 2}
	if f.FrameBytes() != 4 || f.BytesPerSecond() != 64000 {
		t.Fatalf("frame %d, rate %d", f.FrameBytes(), f.BytesPerSecond())
	}
	if d := f.DurationOf(32000); d != 500*time.Millisecond {
		t.Fatalf("DurationOf = %v", d)
	}
	if n := f.BytesFor(10*time.Millisecond + 1); n != 640 {
		t.Fatalf("BytesFor = %d, want 640 (whole frames)", n)
	}
}

func TestLimitsAreMandatoryAndFailValidation(t *testing.T) {
	if err := (voice.Limits{}).Validate(); err == nil {
		t.Fatal("a zero Limits must be rejected, not mean unlimited")
	}
	if err := voice.DefaultLimits().Validate(); err != nil {
		t.Fatalf("DefaultLimits invalid: %v", err)
	}
	l := voice.Limits{MaxAudioBytes: 100, MaxChunkBytes: 40, MaxTextRunes: 5}
	f := voice.PCM16Mono(16000)
	if err := l.CheckAudio(f, make([]byte, 100)); err != nil {
		t.Fatalf("audio at the cap: %v", err)
	}
	err := l.CheckAudio(f, make([]byte, 102))
	wantCode(t, err, contracts.ErrValidationFailed) // never the retryable resource_exhausted
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Retryable || ce.Details["limit"] != 100 || ce.Details["requested"] != 102 {
		t.Fatalf("oversize error = %+v", ce)
	}
	wantCode(t, l.CheckAudio(f, nil), contracts.ErrValidationFailed)
	wantCode(t, l.CheckAudio(f, make([]byte, 99)), contracts.ErrValidationFailed)
	wantCode(t, l.CheckAudio(voice.AudioFormat{}, make([]byte, 100)), contracts.ErrValidationFailed)
	wantCode(t, l.CheckChunk(f, make([]byte, 42), 0), contracts.ErrValidationFailed)
	wantCode(t, l.CheckChunk(f, make([]byte, 40), 80), contracts.ErrValidationFailed)
	if err := l.CheckChunk(f, make([]byte, 40), 60); err != nil {
		t.Fatalf("chunk filling the stream exactly: %v", err)
	}
}

func TestRequestValidation(t *testing.T) {
	l := voice.DefaultLimits()
	f := voice.PCM16Mono(16000)
	audio := make([]byte, 320)
	cases := []struct {
		name string
		err  error
		ok   bool
	}{
		{"transcribe ok", voice.TranscribeRequest{Audio: audio, Format: f, Language: "it-IT", Profile: "lite"}.Validate(l), true},
		{"transcribe no language", voice.TranscribeRequest{Audio: audio, Format: f}.Validate(l), true},
		{"transcribe bad language", voice.TranscribeRequest{Audio: audio, Format: f, Language: "english"}.Validate(l), false},
		{"transcribe bad profile", voice.TranscribeRequest{Audio: audio, Format: f, Profile: "a b"}.Validate(l), false},
		{"transcribe empty", voice.TranscribeRequest{Format: f}.Validate(l), false},
		{"stream ok", voice.STTStreamRequest{Format: f}.Validate(), true},
		{"stream no format", voice.STTStreamRequest{}.Validate(), false},
		{"detect ok", voice.DetectRequest{Audio: audio, Format: f}.Validate(l), true},
		{"detect empty", voice.DetectRequest{Format: f}.Validate(l), false},
		{"synthesize ok", voice.SynthesizeRequest{Text: "Ciao, come stai?", Voice: "it_f1", Language: "it"}.Validate(l), true},
		{"synthesize zero format means default", voice.SynthesizeRequest{Text: "x"}.Validate(l), true},
		{"synthesize bad format", voice.SynthesizeRequest{Text: "x", Format: voice.PCM16Mono(1)}.Validate(l), false},
		{"synthesize blank", voice.SynthesizeRequest{Text: " \n"}.Validate(l), false},
		{"synthesize invalid utf8", voice.SynthesizeRequest{Text: "a\xffb"}.Validate(l), false},
		{"synthesize too long", voice.SynthesizeRequest{Text: strings.Repeat("è", l.MaxTextRunes+1)}.Validate(l), false},
		{"synthesize multibyte at the cap", voice.SynthesizeRequest{Text: strings.Repeat("è", l.MaxTextRunes)}.Validate(l), true},
		{"synthesize zero limits", voice.SynthesizeRequest{Text: "x"}.Validate(voice.Limits{}), false},
	}
	for _, tc := range cases {
		if (tc.err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, tc.err, tc.ok)
		} else if tc.err != nil {
			wantCode(t, tc.err, contracts.ErrValidationFailed)
		}
	}
}

func TestAudioIsNeverSerialised(t *testing.T) {
	secret := []byte("RAW-AUDIO-MUST-NOT-LEAK")
	for name, v := range map[string]any{
		"transcribe": voice.TranscribeRequest{Audio: secret, Format: voice.PCM16Mono(16000)},
		"detect":     voice.DetectRequest{Audio: secret, Format: voice.PCM16Mono(16000)},
		"chunk":      voice.AudioChunk{Seq: 1, Data: secret},
		"audio":      voice.Audio{Data: secret},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "RAW-AUDIO") || strings.Contains(string(b), "UkFX") { // the text and its base64
			t.Errorf("%s serialises audio: %s", name, b)
		}
	}
}

func TestValidLanguage(t *testing.T) {
	for _, ok := range []string{"en", "it", "en-US", "zh-Hant-TW", "fil"} {
		if !voice.ValidLanguage(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "e", "english", "EN", "en_US", "en-", "en-U", "en US", "../x", "en-US-"} {
		if voice.ValidLanguage(bad) {
			t.Errorf("%q must be invalid", bad)
		}
	}
}

// scriptedSTT replays events then ends with err (io.EOF for a clean end).
type scriptedSTT struct {
	events []voice.TranscriptEvent
	end    error
	closed int
}

func (s *scriptedSTT) Send([]byte) error { return nil }
func (s *scriptedSTT) CloseSend() error  { return nil }
func (s *scriptedSTT) Close() error      { s.closed++; return nil }
func (s *scriptedSTT) Recv() (voice.TranscriptEvent, error) {
	if len(s.events) == 0 {
		return voice.TranscriptEvent{}, s.end
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func TestCollectTranscript(t *testing.T) {
	interim := func(seq int, text string) voice.TranscriptEvent { return voice.TranscriptEvent{Seq: seq, Text: text} }
	final := func(seq int, text string) voice.TranscriptEvent {
		return voice.TranscriptEvent{Seq: seq, Text: text, Final: true}
	}
	done := func(seq int) voice.TranscriptEvent {
		return voice.TranscriptEvent{Seq: seq, Done: true, Language: "it", Model: "m", TraceID: "tr"}
	}
	cases := []struct {
		name   string
		events []voice.TranscriptEvent
		end    error
		want   string
		code   contracts.ErrCode // "" for success
	}{
		{"finals joined, interims ignored", []voice.TranscriptEvent{interim(1, "ciao"), final(2, " ciao mondo "), final(3, "come stai"), done(4)}, io.EOF, "ciao mondo come stai", ""},
		{"silence is an empty success", []voice.TranscriptEvent{done(1)}, io.EOF, "", ""},
		{"ends before Done", []voice.TranscriptEvent{final(1, "ciao")}, io.EOF, "", contracts.ErrUnavailable},
		{"interrupted", []voice.TranscriptEvent{final(1, "ciao")}, contracts.New(contracts.ErrTimeout, "x"), "", contracts.ErrTimeout},
		{"event after Done", []voice.TranscriptEvent{done(1), final(2, "late")}, io.EOF, "", contracts.ErrInternal},
		{"out of order", []voice.TranscriptEvent{final(2, "a"), final(1, "b"), done(3)}, io.EOF, "", contracts.ErrInternal},
	}
	for _, tc := range cases {
		s := &scriptedSTT{events: tc.events, end: tc.end}
		tr, err := voice.CollectTranscript(s)
		if s.closed != 1 {
			t.Errorf("%s: stream closed %d times, want 1", tc.name, s.closed)
		}
		if tc.code != "" {
			if err == nil || contracts.CodeOf(err) != tc.code {
				t.Errorf("%s: err = %v, want %s", tc.name, err, tc.code)
			}
			if tr.Text != "" {
				t.Errorf("%s: failure leaked text %q", tc.name, tr.Text)
			}
			continue
		}
		if err != nil || tr.Text != tc.want {
			t.Errorf("%s: = %q, %v; want %q", tc.name, tr.Text, err, tc.want)
		}
		if tc.name == "finals joined, interims ignored" && (tr.Language != "it" || tr.Model != "m" || tr.TraceID != "tr") {
			t.Errorf("metadata from Done lost: %+v", tr)
		}
	}
}

type scriptedAudio struct {
	chunks []voice.AudioChunk
	end    error
}

func (s *scriptedAudio) Close() error { return nil }
func (s *scriptedAudio) Recv() (voice.AudioChunk, error) {
	if len(s.chunks) == 0 {
		return voice.AudioChunk{}, s.end
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return c, nil
}

func TestCollectAudio(t *testing.T) {
	f := voice.PCM16Mono(16000)
	chunk := func(seq int, n int, done bool) voice.AudioChunk {
		return voice.AudioChunk{Seq: seq, Data: make([]byte, n), Format: f, Done: done}
	}
	cases := []struct {
		name   string
		chunks []voice.AudioChunk
		end    error
		max    int
		bytes  int
		code   contracts.ErrCode
	}{
		{"complete", []voice.AudioChunk{chunk(1, 4, false), chunk(2, 4, true)}, io.EOF, 100, 8, ""},
		{"no Done", []voice.AudioChunk{chunk(1, 4, false)}, io.EOF, 100, 0, contracts.ErrUnavailable},
		{"interrupted", []voice.AudioChunk{chunk(1, 4, false)}, contracts.New(contracts.ErrUnavailable, "x"), 100, 0, contracts.ErrUnavailable},
		{"over the cap", []voice.AudioChunk{chunk(1, 60, false), chunk(2, 60, true)}, io.EOF, 100, 0, contracts.ErrResourceExhausted},
		{"half a frame", []voice.AudioChunk{chunk(1, 3, true)}, io.EOF, 100, 0, contracts.ErrSchemaInvalid},
		{"chunk after Done", []voice.AudioChunk{chunk(1, 2, true), chunk(2, 2, true)}, io.EOF, 100, 0, contracts.ErrInternal},
		{"no format", []voice.AudioChunk{{Seq: 1, Data: make([]byte, 2), Done: true}}, io.EOF, 100, 0, contracts.ErrSchemaInvalid},
		{"format changes", []voice.AudioChunk{chunk(1, 2, false), {Seq: 2, Data: make([]byte, 2), Format: voice.PCM16Mono(24000), Done: true}}, io.EOF, 100, 0, contracts.ErrSchemaInvalid},
	}
	for _, tc := range cases {
		au, err := voice.CollectAudio(&scriptedAudio{chunks: tc.chunks, end: tc.end}, tc.max)
		if tc.code != "" {
			if err == nil || contracts.CodeOf(err) != tc.code {
				t.Errorf("%s: err = %v, want %s", tc.name, err, tc.code)
			}
			if len(au.Data) != 0 {
				t.Errorf("%s: failure leaked audio", tc.name)
			}
			continue
		}
		if err != nil || len(au.Data) != tc.bytes || au.Format != f {
			t.Errorf("%s: = %d bytes %+v, %v", tc.name, len(au.Data), au.Format, err)
		}
	}
	if _, err := voice.CollectAudio(&scriptedAudio{end: io.EOF}, 0); err == nil {
		t.Fatal("a zero byte cap must be rejected")
	}
}

func TestDetectResultCheck(t *testing.T) {
	f := voice.PCM16Mono(16000)
	req := voice.DetectRequest{Audio: make([]byte, f.BytesFor(time.Second)), Format: f}
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	seg := func(a, b int) voice.SpeechSegment { return voice.SpeechSegment{Start: ms(a), End: ms(b)} }
	cases := []struct {
		name string
		segs []voice.SpeechSegment
		ok   bool
	}{
		{"none", nil, true},
		{"ordered", []voice.SpeechSegment{seg(0, 100), seg(100, 400), seg(900, 1000)}, true},
		{"rounding slack at the end", []voice.SpeechSegment{seg(900, 1040)}, true},
		{"past the end", []voice.SpeechSegment{seg(900, 1200)}, false},
		{"empty span", []voice.SpeechSegment{seg(100, 100)}, false},
		{"backwards span", []voice.SpeechSegment{seg(200, 100)}, false},
		{"negative start", []voice.SpeechSegment{{Start: -1, End: ms(10)}}, false},
		{"overlap", []voice.SpeechSegment{seg(0, 300), seg(200, 400)}, false},
		{"out of order", []voice.SpeechSegment{seg(500, 600), seg(0, 100)}, false},
	}
	for _, tc := range cases {
		err := voice.DetectResult{Segments: tc.segs}.Check(req)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		} else if err != nil {
			wantCode(t, err, contracts.ErrSchemaInvalid)
		}
	}
}

type onlyTTS struct{ voice.TTS }

func TestRegisterChecksTheInterface(t *testing.T) {
	f := fake.New()
	reg := providers.NewRegistry(nil)

	// The registry itself takes anything for a reserved role.
	if err := reg.Register(providers.RoleVAD, struct{}{}); err != nil {
		t.Fatalf("registry accepts reserved roles untyped: %v", err)
	}
	if _, err := voice.LookupVAD(reg); contracts.CodeOf(err) != contracts.ErrInternal {
		t.Fatalf("Lookup of a mistyped provider = %v, want internal", err)
	}

	// voice.Register does not.
	reg = providers.NewRegistry(nil)
	wantCode(t, voice.Register(reg, providers.RoleSTT, struct{}{}), contracts.ErrValidationFailed)
	wantCode(t, voice.Register(reg, providers.RoleSTT, nil), contracts.ErrValidationFailed)
	wantCode(t, voice.Register(reg, providers.RoleSTT, onlyTTS{f}), contracts.ErrValidationFailed) // a TTS is not an STT
	wantCode(t, voice.Register(reg, providers.RoleController, f), contracts.ErrValidationFailed)
	wantCode(t, voice.Register(reg, providers.Role("nope"), f), contracts.ErrValidationFailed)
	for _, role := range voice.Roles() {
		if reg.Has(role) {
			t.Fatalf("a rejected Register left %s registered", role)
		}
	}
	if err := voice.Register(reg, providers.RoleTTS, f); err != nil {
		t.Fatal(err)
	}
	wantCode(t, voice.Register(reg, providers.RoleTTS, f), contracts.ErrConflict)
	if _, err := voice.LookupTTS(reg); err != nil {
		t.Fatalf("LookupTTS: %v", err)
	}
}

func TestMissingVoiceRolesDegradeWithTheirEffect(t *testing.T) {
	reg := providers.NewRegistry(nil)
	for role, effect := range map[providers.Role]string{
		providers.RoleSTT: providers.EffectTextInputOnly,
		providers.RoleTTS: providers.EffectVerifiedTextOnly,
		providers.RoleVAD: providers.EffectManualTurnTaking,
	} {
		var err error
		switch role {
		case providers.RoleSTT:
			_, err = voice.LookupSTT(reg)
		case providers.RoleTTS:
			_, err = voice.LookupTTS(reg)
		default:
			_, err = voice.LookupVAD(reg)
		}
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable || ce.Details["effect"] != effect {
			t.Errorf("%s: err = %v, want unavailable with effect %s", role, err, effect)
		}
	}
}
