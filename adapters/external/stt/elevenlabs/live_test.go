//go:build live

package elevenlabs

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

type liveKeys struct{ key string }

func (l liveKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return credentials.Secret(l.key), nil
}

func liveScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_live", ApplicationID: "app_live", TenantID: "ten_live", WorkspaceID: "wsp_live",
		PrincipalID: "usr_live", Roles: []string{"member"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses_live",
	}
}

// TestLiveTranscribe makes one small real call. Run with
//
//	ALIXAI_LIVE_ELEVENLABS_KEY=... go test -tags live -run TestLive ./adapters/external/stt/elevenlabs/
//
// ALIXAI_LIVE_ELEVENLABS_MODEL overrides the model (default scribe_v2).
func TestLiveTranscribe(t *testing.T) {
	key := os.Getenv("ALIXAI_LIVE_ELEVENLABS_KEY")
	if key == "" {
		t.Skip("ALIXAI_LIVE_ELEVENLABS_KEY is not set")
	}
	model := os.Getenv("ALIXAI_LIVE_ELEVENLABS_MODEL")
	if model == "" {
		model = DefaultModel
	}
	dest, err := egress.DestinationFromURL(DefaultBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{CredentialRef: "cred_live", Model: model}, reg.Dialer(ProviderName), liveKeys{key})
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 16000)
	for i := 0; i < 8000; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], 500)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tr, err := cl.Transcribe(ctx, liveScope(), voice.TranscribeRequest{
		Audio: pcm, Format: voice.PCM16Mono(16000), Language: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("text_len=%d model=%s", len(tr.Text), tr.Model)
}
