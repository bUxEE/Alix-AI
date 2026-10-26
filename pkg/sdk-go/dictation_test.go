package sdkgo_test

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

func wavClip(seconds float64) []byte {
	rate := 16000
	n := int(float64(rate) * seconds)
	pcm := make([]byte, n*2)
	out := make([]byte, 44+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVE")
	copy(out[12:], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], 1)
	binary.LittleEndian.PutUint32(out[24:], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}

func TestDictationCapabilitiesAndCreate(t *testing.T) {
	fake := sdktest.New()
	t.Cleanup(fake.Close)
	client, err := fake.Client(sdktest.Claims{SessionID: "ses-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caps, err := client.Dictations().Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Available || caps.SampleRateHz != 16000 || caps.Format != "audio/wav" {
		t.Fatalf("unexpected caps: %+v", caps)
	}
	fake.SetDictationText("hello from go")
	got, err := client.Dictations().Create(ctx, "ses-1", wavClip(0.5), &sdkgo.DictationCreateOptions{Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello from go" || got.SegmentKind != "user_recorded" {
		t.Fatalf("unexpected transcript: %+v", got)
	}
}

func TestDictationSilentAndUnavailable(t *testing.T) {
	fake := sdktest.New()
	t.Cleanup(fake.Close)
	client, err := fake.Client(sdktest.Claims{SessionID: "ses-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	fake.SetDictationSilent(true)
	_, err = client.Dictations().Create(ctx, "ses-1", wavClip(0.2), nil)
	if !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("want validation_failed, got %v", err)
	}
	fake.SetDictationSilent(false)
	fake.SetDictationDisabled(true)
	caps, err := client.Dictations().Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.Available {
		t.Fatal("expected unavailable capabilities")
	}
	fake.SetDictationUnavailable(true)
	_, err = client.Dictations().Capabilities(ctx)
	if !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestDictationRefusesShortBody(t *testing.T) {
	fake := sdktest.New()
	t.Cleanup(fake.Close)
	client, err := fake.Client(sdktest.Claims{SessionID: "ses-1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Dictations().Create(context.Background(), "ses-1", []byte("nope"), nil)
	if !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Fatalf("want validation_failed, got %v", err)
	}
	if len(fake.RequestsOf("createSessionDictation")) != 0 {
		t.Fatal("request must not leave the client")
	}
}
