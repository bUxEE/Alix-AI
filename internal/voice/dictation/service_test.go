package dictation_test

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers/voice"
	vfake "github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/internal/voice/dictation"
	"github.com/buxee/alix-ai/pkg/contracts"
	voicegen "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_t", ApplicationID: "app_t", TenantID: "ten_t",
		WorkspaceID: "wsp_t", PrincipalID: "usr_t", Roles: []string{"member"},
		CapabilityScopes: []string{"cap.read"}, SessionID: "ses_t",
	}
}

func pcmTone(ms int, amp int16) []byte {
	n := SampleRateHz * ms / 1000
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		// simple square-ish energy
		v := amp
		if (i/40)%2 == 0 {
			v = -amp
		}
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

const SampleRateHz = 16000

func wavOf(pcm []byte) []byte {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], SampleRateHz)
	binary.LittleEndian.PutUint32(h[28:], SampleRateHz*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

func TestParseAndRefuseBadClips(t *testing.T) {
	stt := vfake.New(vfake.WithName("fake-stt"))
	stt.Script("hello")
	svc := dictation.New(dictation.Deps{STT: stt, Locality: dictation.LocalityLocal, Languages: []voicegen.Language{"en"}, MaxSeconds: 2})
	ctx := context.Background()
	scope := testScope()

	if _, err := svc.Transcribe(ctx, scope, []byte("not wav"), ""); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("bad format: %v", err)
	}
	short := wavOf(pcmTone(100, 1000))
	if _, err := svc.Transcribe(ctx, scope, short, ""); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("too short: %v", err)
	}
	silent := wavOf(make([]byte, SampleRateHz*2)) // 1s of zeros
	if err := mustReason(svc.Transcribe(ctx, scope, silent, "")); err != "silent" {
		t.Fatalf("silent reason = %q", err)
	}
	longPCM := pcmTone(2500, 2000) // 2.5s > max 2
	if _, err := svc.Transcribe(ctx, scope, wavOf(longPCM), ""); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("too long: %v", err)
	}
	if stt.CallCount() != 0 {
		t.Fatalf("STT called on refused clips: %d", stt.CallCount())
	}
}

func mustReason(_ voicegen.DictationTranscript, err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(*contracts.Error); ok {
		if r, ok := e.Details["reason"].(string); ok {
			return r
		}
	}
	return string(contracts.CodeOf(err))
}

func TestTranscribeHappyPathZerosAudio(t *testing.T) {
	stt := vfake.New(vfake.WithName("fake-stt"))
	stt.Script("hello world")
	var events []dictation.Event
	svc := dictation.New(dictation.Deps{
		STT: stt, Locality: dictation.LocalityLocal, Languages: []voicegen.Language{"en"},
		Observe: func(e dictation.Event) { events = append(events, e) },
	})
	pcm := pcmTone(500, 2000)
	raw := wavOf(pcm)
	got, err := svc.Transcribe(context.Background(), testScope(), raw, "en")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello world" || got.SegmentKind != "user_recorded" || got.Locality != voicegen.DictationTranscriptLocalityLocal {
		t.Fatalf("got %+v", got)
	}
	if got.DurationMS < 400 || got.DurationMS > 600 {
		t.Fatalf("duration_ms = %d", got.DurationMS)
	}
	for _, b := range raw {
		if b != 0 {
			t.Fatal("audio body was not zeroed")
		}
	}
	if len(events) != 1 || events[0].Outcome != "ok" || events[0].Locality != "local" {
		t.Fatalf("events = %+v", events)
	}
	// ensure fake received non-zero audio length before zeroing of caller's buffer
	if stt.AudioBytesReceived() == 0 {
		t.Fatal("STT received no audio")
	}
}

func TestCapabilitiesUnavailableWithoutSTT(t *testing.T) {
	svc := dictation.New(dictation.Deps{})
	caps, err := svc.Capabilities(context.Background(), testScope())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Available || caps.Locality != voicegen.TranscriptionCapabilitiesLocalityNone {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestExternalDictationLocalityAndProvider(t *testing.T) {
	stt := vfake.New(vfake.WithName("fake-ext-stt"))
	stt.Script("external hello")
	svc := dictation.New(dictation.Deps{
		STT: stt, Locality: dictation.LocalityExternal, Provider: "openai", External: true,
		Languages: []voicegen.Language{"en"},
	})
	got, err := svc.Transcribe(context.Background(), testScope(), wavOf(pcmTone(500, 2000)), "en")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "external hello" || got.Locality != voicegen.DictationTranscriptLocalityExternal {
		t.Fatalf("got %+v", got)
	}
	if got.Provider == nil || *got.Provider != "openai" {
		t.Fatalf("provider = %v", got.Provider)
	}
}

func TestFormatRequires16kMono(t *testing.T) {
	// 8 kHz mono PCM WAV
	pcm := make([]byte, 8000)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], 8000)
	binary.LittleEndian.PutUint32(h[28:], 16000)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	_, err := dictation.ParseWAV(append(h, pcm...))
	if contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("8 kHz: %v", err)
	}
	_ = voice.PCM16Mono(16000)
	_ = time.Second
}
