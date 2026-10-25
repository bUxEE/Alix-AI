package voice

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// suiteLog records every request any test server in this package received, so
// TestMain can assert that the whole suite stayed on the voice allowlist
// (SEC-067). Every test server is created through newStub.
var suiteLog struct {
	mu   sync.Mutex
	seen map[string]int
}

func recordSuiteRequest(r *http.Request) {
	suiteLog.mu.Lock()
	defer suiteLog.mu.Unlock()
	if suiteLog.seen == nil {
		suiteLog.seen = map[string]int{}
	}
	suiteLog.seen[r.Method+" "+r.URL.Path]++
}

func suiteViolations() (bad []string, total int) {
	suiteLog.mu.Lock()
	defer suiteLog.mu.Unlock()
	allowed := map[string]bool{}
	for _, e := range AllowedEndpoints() {
		allowed[e] = true
	}
	for k, n := range suiteLog.seen {
		total += n
		if !allowed[k] {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad, total
}

func TestMain(m *testing.M) {
	code := m.Run()
	if bad, _ := suiteViolations(); code == 0 && len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: the suite sent requests to paths outside the voice allowlist: %v\n", bad)
		code = 1
	}
	os.Exit(code)
}

// newTestKey returns a fresh random API key for one test: a TEST-ONLY value,
// generated per run, never a real credential.
func newTestKey(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "TESTONLY-" + hex.EncodeToString(b)
}

type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// stub is a recording stand-in for LocalAI on loopback TCP.
type stub struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recordedRequest
}

func newStub(t testing.TB, h http.HandlerFunc) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recordSuiteRequest(r)
		s.mu.Lock()
		s.reqs = append(s.reqs, recordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.reqs...)
}

func (s *stub) only(t testing.TB) recordedRequest {
	t.Helper()
	reqs := s.requests()
	if len(reqs) != 1 {
		t.Fatalf("server received %d requests, want 1", len(reqs))
	}
	return reqs[0]
}

func testVoiceConfig() Config {
	return Config{
		Models:   ModelSet{STT: "stt-model", TTS: "tts-model", VAD: "vad-model"},
		Profiles: map[string]ModelSet{"it-fast": {STT: "stt-it"}},
	}
}

// client builds a voice client against the stub with a fresh key.
func (s *stub) client(t testing.TB, mutate ...func(*localai.Config, *Config)) (*Client, string) {
	t.Helper()
	rt, cfg := localai.Config{Endpoint: s.URL}, testVoiceConfig()
	for _, m := range mutate {
		m(&rt, &cfg)
	}
	key := newTestKey(t)
	c, err := New(rt, cfg, WithAPIKey(key))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, key
}

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_test",
		ApplicationID:    "app_test",
		TenantID:         "ten_test",
		WorkspaceID:      "wsp_test",
		PrincipalID:      "usr_test",
		Roles:            []string{"member"},
		CapabilityScopes: []string{"cap.read"},
		SessionID:        "ses_test",
	}
}

var bg = context.Background()

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want code %s", code)
	}
	if !contracts.HasCode(err, code) {
		t.Fatalf("error code = %s (%v), want %s", contracts.CodeOf(err), err, code)
	}
}

// pcm16k is n milliseconds of non-silent 16 kHz mono PCM.
func tonePCM(f pv.AudioFormat, d time.Duration) []byte {
	out := make([]byte, f.BytesFor(d))
	for i := 0; i+2 <= len(out); i += 2 {
		binary.LittleEndian.PutUint16(out[i:], uint16(int16((i/2)%2000-1000)))
	}
	return out
}

// streamedWAVHeader is what LocalAI sends first on a streamed synthesis: a
// header whose size fields are 0xFFFFFFFF.
func streamedWAVHeader(rate int) []byte {
	h := wavHeader(pv.PCM16Mono(rate), 0)
	binary.LittleEndian.PutUint32(h[4:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(h[40:], 0xFFFFFFFF)
	return h
}

// formField is what a multipart upload carried.
type multipartBody struct {
	fields   map[string]string
	file     []byte
	fileName string
	fileType string
}

func parseMultipart(t testing.TB, r recordedRequest) multipartBody {
	t.Helper()
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Fatalf("Content-Type = %q, want multipart/form-data", r.Header.Get("Content-Type"))
	}
	// multipart.Reader keeps everything in memory; ParseMultipartForm could
	// spill an upload to a temporary file.
	mr := multipart.NewReader(bytes.NewReader(r.Body), params["boundary"])
	out := multipartBody{fields: map[string]string{}}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		if p.FormName() == "file" {
			out.file, out.fileName, out.fileType = data, p.FileName(), p.Header.Get("Content-Type")
			continue
		}
		if _, dup := out.fields[p.FormName()]; dup {
			t.Fatalf("form field %q sent twice", p.FormName())
		}
		out.fields[p.FormName()] = string(data)
	}
}

// sse writes server-sent events and flushes after each.
func sse(w http.ResponseWriter, payloads ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, p := range payloads {
		fmt.Fprintf(w, "data: %s\n\n", p)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// validTranscript is a stub handler answering any transcription with text.
func validTranscript(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, `{"text":" hello ","language":"en"}`)
}

// recvAll drains a TTS stream and returns its chunks, stopping at the first
// error (io.EOF included).
func recvAll(s pv.AudioStream) (chunks []pv.AudioChunk, err error) {
	for {
		c, err := s.Recv()
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
	}
}
