package elevenlabs

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

type keys struct{ k string }

func (k keys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return credentials.Secret(k.k), nil
}

func scope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_t", ApplicationID: "app_t", TenantID: "ten_t", WorkspaceID: "wsp_t",
		PrincipalID: "usr_t", Roles: []string{"m"}, CapabilityScopes: []string{"c"}, SessionID: "ses_t",
	}
}

func TestSpeechToTextHappyPath(t *testing.T) {
	var keyHdr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyHdr = r.Header.Get("xi-api-key")
		if r.URL.Path != "/v1/speech-to-text" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"text":"hello eleven","language_code":"en"}`)
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_x"}, reg.Dialer(ProviderName), keys{k: "el-key"})
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 3200)
	for i := 0; i < 1600; i++ {
		binary.LittleEndian.PutUint16(pcm[2*i:], 500)
	}
	tr, err := cl.Transcribe(context.Background(), scope(), voice.TranscribeRequest{Audio: pcm, Format: voice.PCM16Mono(16000)})
	if err != nil || tr.Text != "hello eleven" || keyHdr != "el-key" {
		t.Fatalf("tr=%+v key=%q err=%v", tr, keyHdr, err)
	}
}
