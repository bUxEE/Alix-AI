package polly

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

const fixtureCred = "AKIAIOSFODNN7EXAMPLE:wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" // TEST-ONLY AWS docs example

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
		binary.LittleEndian.PutUint16(b[2*i:], 400)
	}
	return b
}

func TestSynthesizeHappyPath(t *testing.T) {
	var auth, amzDate string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		amzDate = r.Header.Get("X-Amz-Date")
		if r.URL.Path != "/v1/speech" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(pcm(120))
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, Region: "us-east-1", CredentialRef: "cred_x"}, reg.Dialer(ProviderName), keys{k: fixtureCred})
	if err != nil {
		t.Fatal(err)
	}
	st, err := cl.SynthesizeStream(context.Background(), scope(), voice.SynthesizeRequest{Text: "Hello polly."})
	if err != nil {
		t.Fatal(err)
	}
	a, err := voice.CollectAudio(st, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if a.Format.SampleRate != NativeSampleRate || len(a.Data) != 240 {
		t.Fatalf("fmt=%+v len=%d", a.Format, len(a.Data))
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") || amzDate == "" {
		t.Fatalf("auth=%q date=%q", auth, amzDate)
	}
	if strings.Contains(auth, "wJalrXUtnFEMI") {
		t.Fatal("secret leaked into Authorization")
	}
	if body["OutputFormat"] != "pcm" || body["SampleRate"] != "16000" || body["Engine"] != "neural" || body["VoiceId"] != "Joanna" {
		t.Fatalf("body=%#v", body)
	}
}

func TestSynthesizeMaps403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"The security token included in the request is invalid."}`)
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, Region: "eu-west-1", CredentialRef: "cred_x"}, reg.Dialer(ProviderName), keys{k: fixtureCred})
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
	if strings.Contains(err.Error(), "security token") || strings.Contains(err.Error(), "wJalr") {
		t.Fatalf("leaked: %v", err)
	}
}

func TestBadCredentialFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("must not call provider")
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	secretish := "AKIAFAKE:super-secret-value-do-not-leak"
	cl, err := New(Config{BaseURL: srv.URL, Region: "us-east-1", CredentialRef: "cred_x"}, reg.Dialer(ProviderName), keys{k: "not-aws-format"})
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
	if !strings.Contains(err.Error(), "malformed credential") {
		t.Fatalf("want malformed credential, got %v", err)
	}
	if strings.Contains(err.Error(), "not-aws-format") || strings.Contains(err.Error(), secretish) {
		t.Fatalf("error leaked credential material: %v", err)
	}
}
