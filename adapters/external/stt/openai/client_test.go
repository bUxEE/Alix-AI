package openai

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const fixtureKey = "fixture-credential-value-0001"

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: "app_test", TenantID: "ten_test", WorkspaceID: "wsp_test",
		PrincipalID: "usr_test", Roles: []string{"member"}, CapabilityScopes: []string{"cap.read"}, SessionID: "ses_test",
	}
}

type fakeKeys struct {
	mu  sync.Mutex
	key string
	err error
}

func (f *fakeKeys) Key(_ context.Context, _ contracts.Scope, _ string) (credentials.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return credentials.Secret(f.key), nil
}

func pcm(n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n/2; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], 1000)
	}
	return b
}

func TestTranscribeHappyPath(t *testing.T) {
	var gotAuth, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseMultipartForm(1 << 20)
		gotLang = r.FormValue("language")
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hello from openai","language":"en"}`)
	}))
	t.Cleanup(srv.Close)

	dest, err := egress.DestinationFromURL(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		t.Fatal(err)
	}
	reg := egress.NewProviderRegistry()
	if err := reg.Set(ProviderName, al); err != nil {
		t.Fatal(err)
	}
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_01TESTREFERENCE", Model: "gpt-transcribe"}, reg.Dialer(ProviderName), &fakeKeys{key: fixtureKey}) // TEST-ONLY
	if err != nil {
		t.Fatal(err)
	}
	tr, err := cl.Transcribe(context.Background(), testScope(), voice.TranscribeRequest{
		Audio: pcm(3200), Format: voice.PCM16Mono(16000), Language: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "hello from openai" || !strings.HasPrefix(gotAuth, "Bearer ") || gotLang != "en" {
		t.Fatalf("tr=%+v auth=%q lang=%q", tr, gotAuth, gotLang)
	}
}

func TestTranscribeMaps401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error"}}`)
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_x"}, reg.Dialer(ProviderName), &fakeKeys{key: fixtureKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = cl.Transcribe(context.Background(), testScope(), voice.TranscribeRequest{Audio: pcm(3200), Format: voice.PCM16Mono(16000)})
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "invalid_request") || strings.Contains(err.Error(), fixtureKey) {
		t.Fatalf("error leaked provider text or key: %v", err)
	}
}

func TestTranscribeStreamUnsupported(t *testing.T) {
	cl := &Client{cfg: Config{}.withDefaults()}
	_, err := cl.TranscribeStream(context.Background(), testScope(), voice.STTStreamRequest{Format: voice.PCM16Mono(16000)})
	if contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("err = %v", err)
	}
	_ = bytes.MinRead
}
