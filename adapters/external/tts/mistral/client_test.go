package mistral

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/buxee/alix-ai/adapters/external/tts/internal/textchunk"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const fixtureKey = "fixture-credential-value-0001" // TEST-ONLY

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

func pcmSilence(nSamples int) []byte {
	b := make([]byte, nSamples*2)
	for i := 0; i < nSamples; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], 100)
	}
	return b
}

func wavPCM(samples int) []byte {
	data := pcmSilence(samples)
	h := make([]byte, 44+len(data))
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
	copy(h[44:], data)
	return h
}

func jsonAudio(wav []byte) []byte {
	b, _ := json.Marshal(map[string]string{"audio_data": base64.StdEncoding.EncodeToString(wav)})
	return b
}

func newTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	dest, err := egress.DestinationFromURL(srvURL)
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
	cl, err := New(Config{BaseURL: srvURL, CredentialRef: "cred_01TESTREFERENCE"}, reg.Dialer(ProviderName), &fakeKeys{key: fixtureKey}) // TEST-ONLY
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

func collect(t *testing.T, st voice.AudioStream) voice.Audio {
	t.Helper()
	a, err := voice.CollectAudio(st, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSynthesizeHappyPath(t *testing.T) {
	var gotAuth, gotCT string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonAudio(wavPCM(160)))
	}))
	t.Cleanup(srv.Close)

	cl := newTestClient(t, srv.URL)
	st, err := cl.SynthesizeStream(context.Background(), testScope(), voice.SynthesizeRequest{Text: "Hello there.", Voice: "fr_marie_neutral"})
	if err != nil {
		t.Fatal(err)
	}
	a := collect(t, st)
	if a.Format.SampleRate != NativeSampleRate || a.Format.Channels != 1 {
		t.Fatalf("format = %+v", a.Format)
	}
	if len(a.Data) != 320 {
		t.Fatalf("pcm len = %d", len(a.Data))
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") || !strings.Contains(gotCT, "application/json") {
		t.Fatalf("auth=%q ct=%q", gotAuth, gotCT)
	}
	if body["response_format"] != "wav" || body["voice_id"] != "fr_marie_neutral" || body["model"] != DefaultModel {
		t.Fatalf("body = %#v", body)
	}
}

func TestSynthesizeMaps401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"bad key sk-secret"}}`)
	}))
	t.Cleanup(srv.Close)
	cl := newTestClient(t, srv.URL)
	st, err := cl.SynthesizeStream(context.Background(), testScope(), voice.SynthesizeRequest{Text: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Recv()
	_ = st.Close()
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "invalid_request") || strings.Contains(err.Error(), "sk-secret") || strings.Contains(err.Error(), fixtureKey) {
		t.Fatalf("error leaked provider text or key: %v", err)
	}
}

func TestSynthesizeMaps429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	cl := newTestClient(t, srv.URL)
	st, err := cl.SynthesizeStream(context.Background(), testScope(), voice.SynthesizeRequest{Text: "Hi"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Recv()
	_ = st.Close()
	if contracts.CodeOf(err) != contracts.ErrRateLimited {
		t.Fatalf("err = %v", err)
	}
}

func TestTextChunkingRespectsLimit(t *testing.T) {
	var inputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		inputs = append(inputs, body["input"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonAudio(wavPCM(80)))
	}))
	t.Cleanup(srv.Close)
	dest, _ := egress.DestinationFromURL(srv.URL)
	al, _ := egress.NewAllowlist(dest)
	reg := egress.NewProviderRegistry()
	_ = reg.Set(ProviderName, al)
	cl, err := New(Config{BaseURL: srv.URL, CredentialRef: "cred_x", MaxInputChars: 20}, reg.Dialer(ProviderName), &fakeKeys{key: fixtureKey})
	if err != nil {
		t.Fatal(err)
	}
	text := "First sentence here. Second sentence follows. Third one ends."
	st, err := cl.SynthesizeStream(context.Background(), testScope(), voice.SynthesizeRequest{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, st)
	want := textchunk.Split(text, 20)
	if len(inputs) != len(want) {
		t.Fatalf("calls=%d want=%d inputs=%#v", len(inputs), len(want), inputs)
	}
	for _, in := range inputs {
		if utf8.RuneCountInString(in) > 20 {
			t.Fatalf("oversized piece %q", in)
		}
	}
}

func TestStripWAV(t *testing.T) {
	pcm, err := stripWAV(wavPCM(10))
	if err != nil || len(pcm) != 20 {
		t.Fatalf("pcm len=%d err=%v", len(pcm), err)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("want error")
	}
}
