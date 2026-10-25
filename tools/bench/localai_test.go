package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// stubLocalAI is a LocalAI v4.10.0 look-alike for the paths the adapter uses:
// readiness, version, model list and streamed chat. It answers every case with
// the proposal the case expects, found through the user text, and records what
// it was sent.
type stubLocalAI struct {
	t      *testing.T
	corpus *Corpus
	model  string

	mu        sync.Mutex
	paths     []string
	chatBody  []map[string]any
	auth      []string
	failCase  string // the case whose chat request is answered with HTTP 429
	failAll   bool   // every chat request is answered with HTTP 429
	notReady  bool
	noModel   bool
	srv       *httptest.Server
	usageIn   int
	usageOut  int
	casesSeen map[string]bool
}

func newStubLocalAI(t *testing.T, corpus *Corpus, model string) *stubLocalAI {
	s := &stubLocalAI{t: t, corpus: corpus, model: model, usageIn: 31, usageOut: 7, casesSeen: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubLocalAI) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	s.mu.Unlock()
	switch r.Method + " " + r.URL.Path {
	case "GET /readyz":
		if s.notReady {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	case "GET /version":
		fmt.Fprint(w, `{"version":"v4.10.0"}`)
	case "GET /v1/models":
		if s.noModel {
			fmt.Fprint(w, `{"data":[{"id":"some-other-model"}]}`)
			return
		}
		fmt.Fprintf(w, `{"data":[{"id":%q}]}`, s.model)
	case "POST /v1/chat/completions":
		s.chat(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *stubLocalAI) chat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", 400)
		return
	}
	s.mu.Lock()
	s.chatBody = append(s.chatBody, body)
	s.mu.Unlock()
	msgs, _ := body["messages"].([]any)
	user, _ := msgs[len(msgs)-1].(map[string]any)["content"].(string)
	var found *Case
	for i := range s.corpus.Cases {
		if RenderUser(s.corpus.Cases[i]) == user {
			found = &s.corpus.Cases[i]
			break
		}
	}
	if found == nil {
		http.Error(w, "unknown case", 400)
		return
	}
	s.mu.Lock()
	s.casesSeen[found.ID] = true
	s.mu.Unlock()
	if found.ID == s.failCase || s.failAll {
		http.Error(w, `{"error":{"message":"busy"}}`, http.StatusTooManyRequests)
		return
	}
	reply := oracleReply(*found)
	if stream, _ := body["stream"].(bool); !stream {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(reply)
		fmt.Fprintf(w, `{"object":"chat.completion","id":"chatcmpl-1","model":%q,"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%s}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			s.model, b, s.usageIn, s.usageOut, s.usageIn+s.usageOut)
		return
	}
	half := len(reply) / 2
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta, finish string) string {
		d, f := "{}", "null"
		if delta != "" {
			b, _ := json.Marshal(delta)
			d = `{"content":` + string(b) + `}`
		}
		if finish != "" {
			f = fmt.Sprintf("%q", finish)
		}
		return fmt.Sprintf(`{"object":"chat.completion.chunk","id":"chatcmpl-1","model":%q,"choices":[{"index":0,"finish_reason":%s,"delta":%s}]}`, s.model, f, d)
	}
	for _, e := range []string{
		chunk("", ""), chunk(reply[:half], ""), chunk(reply[half:], ""), chunk("", "stop"),
		fmt.Sprintf(`{"object":"chat.completion.chunk","id":"chatcmpl-1","model":%q,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			s.model, s.usageIn, s.usageOut, s.usageIn+s.usageOut),
		"[DONE]",
	} {
		fmt.Fprintf(w, "data: %s\n\n", e)
		w.(http.Flusher).Flush()
	}
}

func (s *stubLocalAI) url() string { return s.srv.URL }

func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "test-" + hex.EncodeToString(b)
}

// TestLocalAIProviderEndToEnd runs the real adapters/localai client against a
// stub runtime on loopback: health, version, the streamed request shape, the
// usage counters, the time to first token and the error mapping all go through
// the production code path. Live runs against a real LocalAI are task M0-13.
func TestLocalAIProviderEndToEnd(t *testing.T) {
	corpus := realCorpus(t)
	entry := anyEntry(t) // whatever the registry holds today
	registryID := entry.ID
	stub := newStubLocalAI(t, corpus, "stub-model")
	key := testKey(t)
	t.Setenv("ALIXAI_LOCALAI_KEY", key)
	t.Setenv("ALIXAI_LOCALAI_URL", stub.url())

	dir := t.TempDir()
	args := []string{"run", "--provider", "localai", "--model", registryID, "--localai-model", "stub-model", "--profile", "4g",
		"--corpus", repoPath(t, corpusRel), "--out", dir, "--limit", "8", "--warmup", "1", "--context-size", "2048"}
	code, out, errs := runCLI(t, args...)
	if code != 0 {
		t.Fatalf("run exited %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	path := onlyResultFile(t, dir)
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), key) || strings.Contains(out+errs, key) {
		t.Error("the API key leaked into the result or the terminal output")
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}

	t.Run("the record names the real runtime and model", func(t *testing.T) {
		if res.Provider.Name != "localai" || res.Synthetic {
			t.Errorf("provider %q synthetic %v", res.Provider.Name, res.Synthetic)
		}
		if res.Profile != "experimental-4g" || !res.Emulated || res.ContainerLimits.MemMB != 4096 {
			t.Errorf("profile %q emulated %v limits %+v", res.Profile, res.Emulated, res.ContainerLimits)
		}
		if res.Backend.RuntimeVersion == nil || *res.Backend.RuntimeVersion != "v4.10.0" || *res.Backend.Runtime != entry.Backend.Runtime || *res.Backend.Name != entry.Backend.Name {
			t.Errorf("backend = %+v", res.Backend)
		}
		if res.Model.RegistryID != registryID || res.Model.RuntimeModelName == nil || *res.Model.RuntimeModelName != "stub-model" || !res.Model.InRegistry {
			t.Errorf("model = %+v", res.Model)
		}
		if res.Context.ConfiguredToks == nil || *res.Context.ConfiguredToks != 2048 || !sameIntPtr(res.Context.NativeMaxToks, entry.Context.NativeMaxTokens) {
			t.Errorf("context = %+v", res.Context)
		}
		if res.Config.WarmupRequests != 1 || res.Latency.WarmupFirstRequestMS == nil {
			t.Errorf("warm-up not recorded: %+v", res.Config)
		}
	})

	t.Run("every case was answered and scored", func(t *testing.T) {
		if res.Errors.Count != 0 || res.Metrics.Cases != 8 || res.Metrics.Pass.Passed != 8 || res.Metrics.SchemaValid.Passed != 8 {
			t.Errorf("metrics = %+v errors = %+v", res.Metrics, res.Errors)
		}
	})

	t.Run("streaming measured time to first token and the runtime's token counts", func(t *testing.T) {
		if res.Latency.TTFTMS.N != 8 || res.Latency.TotalMS.N != 8 {
			t.Errorf("ttft n=%d total n=%d, want 8", res.Latency.TTFTMS.N, res.Latency.TotalMS.N)
		}
		if res.Latency.ToksIn != 8*31 || res.Latency.ToksOut != 8*7 {
			t.Errorf("tokens in/out = %d/%d, want %d/%d from the runtime's usage trailer", res.Latency.ToksIn, res.Latency.ToksOut, 8*31, 8*7)
		}
		for _, c := range res.Cases {
			if c.ToksEstimated {
				t.Errorf("%s: tokens are estimated although the runtime reported usage", c.ID)
			}
		}
	})

	t.Run("what was sent to the runtime", func(t *testing.T) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if len(stub.chatBody) != 9 { // warm-up + 8
			t.Fatalf("%d chat requests, want 9", len(stub.chatBody))
		}
		b := stub.chatBody[1]
		if b["model"] != "stub-model" || b["stream"] != true || b["temperature"] != float64(0) || b["max_tokens"] != float64(256) {
			t.Errorf("request = model %v stream %v temperature %v max_tokens %v", b["model"], b["stream"], b["temperature"], b["max_tokens"])
		}
		rf, _ := b["response_format"].(map[string]any)
		if rf["type"] != "json_schema" {
			t.Errorf("response_format = %v, want a JSON Schema constraint", b["response_format"])
		}
		if _, has := b["grammar"]; has {
			t.Error("a schema-constrained request must not also carry a grammar")
		}
		msgs := b["messages"].([]any)
		if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "user" {
			t.Errorf("messages = %v", msgs)
		}
		for i, a := range stub.auth {
			if stub.paths[i] == "GET /readyz" {
				continue // the readiness probe needs no key and is sent without one
			}
			if a != "Bearer "+key {
				t.Errorf("%s: Authorization = %q, want the key from ALIXAI_LOCALAI_KEY", stub.paths[i], a)
				break
			}
		}
		allowed := map[string]bool{"GET /readyz": true, "GET /version": true, "GET /v1/models": true, "POST /v1/chat/completions": true}
		for _, p := range stub.paths {
			if !allowed[p] {
				t.Errorf("the tool called %s, which is outside the inference/health/model-list allowlist", p)
			}
		}
	})

	t.Run("check accepts it", func(t *testing.T) {
		if code, out, _ := runCLI(t, "check", dir); code != 0 {
			t.Errorf("exit %d\n%s", code, out)
		}
		if code, out, _ := runCLI(t, "check", "-reject-synthetic", dir); code != 0 {
			t.Errorf("a LocalAI result is not synthetic, but check refused it: exit %d\n%s", code, out)
		}
	})
}

func TestLocalAIGrammarAndErrors(t *testing.T) {
	corpus := realCorpus(t)
	base := func(stub *stubLocalAI, dir string, extra ...string) []string {
		return append([]string{"run", "--provider", "localai", "--model", anyEntry(t).ID, "--localai-model", "stub-model",
			"--profile", "lite", "--endpoint", stub.url(), "--corpus", repoPath(t, corpusRel), "--out", dir, "--limit", "4", "--warmup", "0"}, extra...)
	}

	t.Run("a grammar constraint, no streaming, and one rejected request", func(t *testing.T) {
		stub := newStubLocalAI(t, corpus, "stub-model")
		stub.failCase = "rt-en-02"
		dir := t.TempDir()
		if code, out, errs := runCLI(t, base(stub, dir, "--constraint", "grammar", "--stream=false")...); code != 0 {
			t.Fatalf("exit %d, stdout %s, stderr %s", code, out, errs)
		}
		var res Result
		raw, _ := os.ReadFile(onlyResultFile(t, dir))
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		if res.Config.Constraint != "grammar" || res.Config.Stream {
			t.Errorf("config = %+v", res.Config)
		}
		if res.Errors.Count != 1 || res.Errors.ByCode["resource_exhausted"] != 1 {
			t.Errorf("errors = %+v, want the HTTP 429 mapped to resource_exhausted", res.Errors)
		}
		if res.Metrics.Cases != 4 || res.Metrics.SchemaValid.Passed != 3 || res.Metrics.Pass.Passed != 3 {
			t.Errorf("metrics = %+v", res.Metrics)
		}
		if res.Latency.TTFTMS.N != 0 || res.Latency.TotalMS.N != 3 {
			t.Errorf("latency = %+v: without streaming there is no time to first token", res.Latency)
		}
		stub.mu.Lock()
		last := stub.chatBody[len(stub.chatBody)-1]
		stub.mu.Unlock()
		if g, _ := last["grammar"].(string); !strings.HasPrefix(g, "root ::=") {
			t.Errorf("a grammar-constrained request carried grammar %q", last["grammar"])
		}
		if _, has := last["response_format"]; has {
			t.Error("a grammar-constrained request must not also carry a response format")
		}
		if last["stream"] == true {
			t.Error("--stream=false must send a plain request")
		}
		if code, out, _ := runCLI(t, "check", dir); code != 0 {
			t.Errorf("check refused the result: %s", out)
		}
	})

	t.Run("a run in which nothing completes writes nothing", func(t *testing.T) {
		stub := newStubLocalAI(t, corpus, "stub-model")
		stub.failAll = true
		dir := t.TempDir()
		code, _, errs := runCLI(t, base(stub, dir)...)
		if code != 1 || !strings.Contains(errs, "every case ended in an error") {
			t.Fatalf("exit %d, stderr %s", code, errs)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%d file(s) left behind", len(entries))
		}
	})
}

func TestLocalAIPreflight(t *testing.T) {
	corpus := realCorpus(t)
	run := func(t *testing.T, stub *stubLocalAI, extra ...string) (int, string, string, string) {
		dir := t.TempDir()
		args := append([]string{"run", "--provider", "localai", "--model", anyEntry(t).ID, "--localai-model", "stub-model",
			"--profile", "lite", "--endpoint", stub.url(), "--corpus", repoPath(t, corpusRel), "--out", dir, "--limit", "2", "--warmup", "0"}, extra...)
		code, out, errs := runCLI(t, args...)
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%d file(s) written although the run failed", len(entries))
		}
		return code, out, errs, dir
	}
	t.Run("a runtime that is still starting", func(t *testing.T) {
		stub := newStubLocalAI(t, corpus, "stub-model")
		stub.notReady = true
		code, _, errs, _ := run(t, stub)
		if code != 1 || !strings.Contains(errs, "not ready") {
			t.Errorf("exit %d, stderr %q", code, errs)
		}
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if len(stub.chatBody) != 0 {
			t.Error("cases were sent to a runtime that is not ready")
		}
	})
	t.Run("a runtime without the model", func(t *testing.T) {
		stub := newStubLocalAI(t, corpus, "stub-model")
		stub.noModel = true
		code, _, errs, _ := run(t, stub)
		if code != 1 || !strings.Contains(errs, "not ready") || !strings.Contains(errs, "--localai-model") {
			t.Errorf("exit %d, stderr %q", code, errs)
		}
	})
	t.Run("a runtime that is not there", func(t *testing.T) {
		stub := newStubLocalAI(t, corpus, "stub-model")
		stub.srv.Close()
		if code, _, errs, _ := run(t, stub); code != 1 || !strings.Contains(errs, "not ready") {
			t.Errorf("exit %d, stderr %q", code, errs)
		}
	})
	t.Run("a non-loopback plaintext endpoint is refused by the adapter", func(t *testing.T) {
		stub := newStubLocalAI(t, corpus, "stub-model")
		dir := t.TempDir()
		code, _, errs := runCLI(t, "run", "--provider", "localai", "--model", anyEntry(t).ID, "--profile", "lite",
			"--endpoint", "http://203.0.113.7:8080", "--corpus", repoPath(t, corpusRel), "--out", dir)
		_ = stub
		if code != 1 || errs == "" {
			t.Errorf("exit %d, stderr %q", code, errs)
		}
	})
	t.Run("an API key given as a literal is refused", func(t *testing.T) {
		stub := newStubLocalAI(t, corpus, "stub-model")
		code, _, errs, _ := run(t, stub, "--api-key-ref", "sk-not-a-reference")
		if code != 1 || strings.Contains(errs, "sk-not-a-reference") {
			t.Errorf("exit %d, stderr %q (the literal must be refused and never echoed)", code, errs)
		}
	})
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
