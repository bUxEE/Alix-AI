package googletts

import (
	"context"
	"encoding/base64"
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

const fixtureTok = "ya29.fixture-token-0001" // TEST-ONLY

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

func wavPCM(samples int) []byte {
	data := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(data[2*i:], 300)
	}
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], NativeSampleRate)
	binary.LittleEndian.PutUint32(h[28:], NativeSampleRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	return append(h, data...)
}

func TestSynthesizeHappyPath(t *testing.T) {
	var auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/text:synthesize" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		enc := base64.StdEncoding.EncodeToString(wavPCM(80))
		_, _ = io.WriteString(w, `{"audioContent":"`+enc+`"}`)
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_x"}, reg.Dialer(ProviderName), keys{k: fixtureTok})
	if err != nil {
		t.Fatal(err)
	}
	st, err := cl.SynthesizeStream(context.Background(), scope(), voice.SynthesizeRequest{Text: "Hello google."})
	if err != nil {
		t.Fatal(err)
	}
	a, err := voice.CollectAudio(st, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a.Format.SampleRate != NativeSampleRate || len(a.Data) != 160 {
		t.Fatalf("fmt=%+v len=%d", a.Format, len(a.Data))
	}
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("auth=%q", auth)
	}
	ac, _ := body["audioConfig"].(map[string]any)
	if ac["audioEncoding"] != "LINEAR16" {
		t.Fatalf("body=%#v", body)
	}
}

func TestSynthesizeMaps401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"API key not valid"}}`)
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_x"}, reg.Dialer(ProviderName), keys{k: fixtureTok})
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
	if strings.Contains(err.Error(), "API key") || strings.Contains(err.Error(), fixtureTok) {
		t.Fatalf("leaked: %v", err)
	}
}

func TestStripWAV(t *testing.T) {
	pcm, err := stripWAV(wavPCM(10))
	if err != nil || len(pcm) != 20 {
		t.Fatalf("pcm len=%d err=%v", len(pcm), err)
	}
}
