package elevenlabs

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const fixtureKey = "el-fixture-key-0001" // TEST-ONLY
const fixtureVoice = "voiceidABCDEF1234"

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

func pcm(n int) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], 200)
	}
	return b
}

func TestSynthesizeHappyPath(t *testing.T) {
	var keyHdr, path, query string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyHdr = r.Header.Get("xi-api-key")
		path, query = r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write(pcm(100))
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_x", VoiceID: fixtureVoice}, reg.Dialer(ProviderName), keys{k: fixtureKey})
	if err != nil {
		t.Fatal(err)
	}
	st, err := cl.SynthesizeStream(context.Background(), scope(), voice.SynthesizeRequest{Text: "Hello eleven."})
	if err != nil {
		t.Fatal(err)
	}
	a, err := voice.CollectAudio(st, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a.Format.SampleRate != NativeSampleRate || keyHdr != fixtureKey {
		t.Fatalf("fmt=%+v key=%q", a.Format, keyHdr)
	}
	if path != "/v1/text-to-speech/"+fixtureVoice || query != "output_format=pcm_24000" {
		t.Fatalf("path=%s query=%s", path, query)
	}
	if body["model_id"] != DefaultModel || body["text"] != "Hello eleven." {
		t.Fatalf("body=%#v", body)
	}
}

func TestSynthesizeMaps401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":{"status":"invalid_api_key","message":"secret"}}`)
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_x", VoiceID: fixtureVoice}, reg.Dialer(ProviderName), keys{k: fixtureKey})
	if err != nil {
		t.Fatal(err)
	}
	st, err := cl.SynthesizeStream(context.Background(), scope(), voice.SynthesizeRequest{Text: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Recv()
	_ = st.Close()
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), "invalid_api_key") || strings.Contains(err.Error(), fixtureKey) {
		t.Fatalf("leaked: %v", err)
	}
}

func TestConfigRequiresVoice(t *testing.T) {
	if err := (Config{CredentialRef: "c"}).Validate(); err == nil {
		t.Fatal("want VoiceID error")
	}
}
