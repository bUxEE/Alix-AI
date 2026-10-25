//go:build live

package spike

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/models/registry"
)

// liveEnv is what the live tests read from the environment. Without a runtime URL and key every live test skips.
//
//	ALIXAI_LOCALAI_URL        the LocalAI endpoint, plain HTTP on loopback (Avast intercepts loopback TLS on the dev box)
//	ALIXAI_LOCALAI_KEY        its API key
//	ALIXAI_SPIKE_BIN          the built alixai binary (go build -o bin/alixai ./cmd/alixai); serve tests skip without it
//	ALIXAI_SPIKE_MODELS_DIR   the models directory (default deploy/container/localai/data/models)
//	ALIXAI_SPIKE_MODEL        registry id of the controller under test (default: the registry's lite default)
//	ALIXAI_SPIKE_LOCALAI_MODEL  the name LocalAI knows it by (default: the registry id)
//	ALIXAI_SPIKE_CONTAINER    container whose memory is sampled with docker stats (optional)
//	ALIXAI_SPIKE_DOCKER       absolute path of the docker CLI (Go cannot search PATH on this machine)
//	ALIXAI_SPIKE_EVIDENCE_DIR where evidence files are written (default docs/evidence/m0-spike)
type liveEnv struct {
	runtimeURL string
	key        string
	bin        string
	modelsDir  string
	container  string
	docker     string
	evidence   string
	root       string
}

func needEnv(t *testing.T) liveEnv {
	t.Helper()
	e := liveEnv{
		runtimeURL: strings.TrimRight(os.Getenv("ALIXAI_LOCALAI_URL"), "/"),
		key:        os.Getenv("ALIXAI_LOCALAI_KEY"),
		bin:        os.Getenv("ALIXAI_SPIKE_BIN"),
		container:  os.Getenv("ALIXAI_SPIKE_CONTAINER"),
		docker:     os.Getenv("ALIXAI_SPIKE_DOCKER"),
		root:       repoRoot(t),
	}
	if e.runtimeURL == "" || e.key == "" {
		t.Skip("ALIXAI_LOCALAI_URL and ALIXAI_LOCALAI_KEY are not set")
	}
	e.modelsDir = os.Getenv("ALIXAI_SPIKE_MODELS_DIR")
	if e.modelsDir == "" {
		e.modelsDir = filepath.Join(e.root, "deploy", "container", "localai", "data", "models")
	}
	e.evidence = os.Getenv("ALIXAI_SPIKE_EVIDENCE_DIR")
	if e.evidence == "" {
		e.evidence = filepath.Join(e.root, "docs", "evidence", "m0-spike")
	}
	if e.docker == "" {
		e.docker = "docker"
	}
	return e
}

// needServe additionally requires the alixai binary.
func (e liveEnv) needServe(t *testing.T) {
	t.Helper()
	if e.bin == "" {
		t.Skip("ALIXAI_SPIKE_BIN is not set (go build -o bin/alixai ./cmd/alixai)")
	}
	if st, err := os.Stat(e.bin); err != nil || st.IsDir() {
		t.Skipf("ALIXAI_SPIKE_BIN %q is not a file", e.bin)
	}
}

// registryVariant returns what to record about the registry the alixai binary under test carries: the status the entry has
// in the shipped registry (models/registry, read here from the same embedded copy the shipped binary carries) and, when the
// binary was built by localai/build-spike-binary.sh, the variant sidecar it wrote next to the binary. A binary built without
// the sidecar is recorded as the shipped registry; if that entry is rejected the binary refuses inference and the test fails
// on its own, so the evidence cannot claim an activation the registry does not allow.
func registryVariant(t *testing.T, env liveEnv, registryID string) (variant any, shippedStatus string) {
	t.Helper()
	reg, err := models.LoadFS(registry.FS)
	if err != nil {
		t.Fatalf("the shipped registry does not load: %v", err)
	}
	e, ok := reg.Get(registryID)
	if !ok {
		t.Fatalf("registry id %q is not in the shipped registry", registryID)
	}
	shippedStatus = string(e.Status)
	raw, err := os.ReadFile(env.bin + ".variant.json")
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"used": false, "note": "the binary carries the shipped registry unchanged"}, shippedStatus
	}
	if err != nil {
		t.Fatalf("cannot read the variant sidecar: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("the variant sidecar is not JSON: %v", err)
	}
	v["used"] = true
	return v, shippedStatus
}

func repoRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := wd; ; d = filepath.Dir(d) {
		if st, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !st.IsDir() {
			return d
		}
		if filepath.Dir(d) == d {
			t.Fatal("no go.mod above the working directory")
		}
	}
}

// controllerUnderTest resolves the registry id and the LocalAI model name of the controller.
func controllerUnderTest(t *testing.T) (registryID, localaiName string) {
	t.Helper()
	registryID = os.Getenv("ALIXAI_SPIKE_MODEL")
	if registryID == "" {
		reg, err := models.LoadFS(registry.FS)
		if err != nil {
			t.Fatal(err)
		}
		d, ok := reg.DefaultFor(models.RoleConversationalController, models.ProfileLite)
		if !ok {
			t.Skip("the registry has no lite default for the conversational controller; set ALIXAI_SPIKE_MODEL")
		}
		registryID = d.ID
	}
	localaiName = os.Getenv("ALIXAI_SPIKE_LOCALAI_MODEL")
	if localaiName == "" {
		localaiName = registryID
	}
	return registryID, localaiName
}

// ---------------------------------------------------------------------------------------------------------------------
// A recording reverse proxy between `alixai serve` and LocalAI. The gateway answers with the validated proposal only, so
// the raw model output (what the runtime actually generated) is read here, on the wire. The Authorization header is
// forwarded and never recorded.

type recordedCall struct {
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	Status     int             `json:"status"`
	DurationMS int64           `json:"duration_ms"`
	Request    json.RawMessage `json:"request,omitempty"`
	Response   json.RawMessage `json:"response,omitempty"`
}

type recorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (r *recorder) add(c recordedCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) snapshot() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedCall(nil), r.calls...)
}

func (r *recorder) paths() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range r.snapshot() {
		if !seen[c.Path] {
			seen[c.Path] = true
			out = append(out, c.Path)
		}
	}
	return out
}

// lastChat returns the last recorded chat completion call.
func (r *recorder) lastChat() (recordedCall, bool) {
	calls := r.snapshot()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Path == "/v1/chat/completions" {
			return calls[i], true
		}
	}
	return recordedCall{}, false
}

// rawJSON keeps a body as JSON when it is JSON and as a JSON string otherwise.
func rawJSON(b []byte) json.RawMessage {
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return append(json.RawMessage(nil), b...)
	}
	s, _ := json.Marshal(string(b))
	return s
}

func newRecordingProxy(t *testing.T, target string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "proxy: cannot read the request", http.StatusBadGateway)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, "proxy: bad request", http.StatusBadGateway)
			return
		}
		for _, h := range []string{"Authorization", "Content-Type", "Accept"} {
			if v := r.Header.Get(h); v != "" {
				out.Header.Set(h, v)
			}
		}
		resp, err := client.Do(out)
		if err != nil {
			rec.add(recordedCall{Method: r.Method, Path: r.URL.Path, Status: 0, DurationMS: time.Since(start).Milliseconds(), Request: rawJSON(body)})
			http.Error(w, "proxy: runtime unreachable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		for _, h := range []string{"Content-Type", "Retry-After"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		rec.add(recordedCall{Method: r.Method, Path: r.URL.Path, Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Request: rawJSON(body), Response: rawJSON(respBody)})
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// ---------------------------------------------------------------------------------------------------------------------
// Running `alixai serve`.

type serveOptions struct {
	runtimeEndpoint string // what localai.endpoint points at
	registryModel   string // inference.models
	localaiModel    string // localai.models.controller
	modelsDir       string
	extraEnv        []string
}

type serveProc struct {
	base   string
	cmd    *exec.Cmd
	stderr *syncBuf
	done   chan error
	cancel context.CancelFunc
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var listeningRE = regexp.MustCompile(`listening on (http://\S+)`)

func yamlString(s string) string { return strconv.Quote(filepath.ToSlash(s)) }

// startAlixAI writes a configuration and starts `alixai serve` on an ephemeral loopback port with the spike endpoint on.
// The API key travels in the child's environment (api_key_ref: env:...), never in the file or on the command line.
func startAlixAI(t *testing.T, env liveEnv, o serveOptions) *serveProc {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`data_dir: %s
server:
  listen: 127.0.0.1:0
log:
  level: info
localai:
  endpoint: %s
  api_key_ref: env:ALIXAI_LOCALAI_KEY
  request_timeout: 3m
  models:
    controller: %s
inference:
  models_dir: %s
  models: [%s]
dev:
  spike_endpoint: true
`, yamlString(filepath.Join(dir, "data")), o.runtimeEndpoint, o.localaiModel, yamlString(o.modelsDir), o.registryModel)
	cfgPath := filepath.Join(dir, "alixai.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, env.bin, "serve", "-config", cfgPath)
	cmd.Dir = dir
	// Every ALIXAI_* variable is a possible configuration override: pass on only the key reference's variable.
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ALIXAI_") && !strings.HasPrefix(kv, "ALIXAI_LOCALAI_KEY=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, o.extraEnv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	p := &serveProc{cmd: cmd, stderr: &syncBuf{}, done: make(chan error, 1), cancel: cancel}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("cannot start %s: %v", env.bin, err)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
		}
	})

	deadline := time.After(60 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatalf("alixai serve ended before it listened:\n%s", p.stderr.String())
			}
			if m := listeningRE.FindStringSubmatch(l); m != nil {
				p.base = m[1]
				return p
			}
		case err := <-p.done:
			t.Fatalf("alixai serve stopped before it listened: %v\n%s", err, p.stderr.String())
		case <-deadline:
			t.Fatalf("alixai serve did not listen within 60 s:\n%s", p.stderr.String())
		}
	}
}

// waitReady polls /readyz until it answers 200 and returns its body.
func (p *serveProc) waitReady(t *testing.T, within time.Duration) (json.RawMessage, error) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		code, body, err := httpDo(http.MethodGet, p.base+"/readyz", nil, nil)
		if err == nil {
			last = fmt.Sprintf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
			if code == http.StatusOK {
				return rawJSON(body), nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("/readyz is not 200 after %s (last: %s)\nserve log:\n%s", within, last, p.stderr.String())
}

func httpDo(method, url string, body []byte, header map[string]string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, err
}

// runtimeVersion asks the runtime directly (GET /version needs the key).
func (e liveEnv) runtimeVersion() (string, error) {
	code, body, err := httpDo(http.MethodGet, e.runtimeURL+"/version", nil, map[string]string{"Authorization": "Bearer " + e.key})
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("GET /version = %d", code)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return strings.TrimSpace(string(body)), nil
	}
	return v.Version, nil
}

// ---------------------------------------------------------------------------------------------------------------------
// Container memory (docker stats: cgroup usage minus inactive file cache, a proxy for RSS and not RSS itself).

type memReport struct {
	Sampled   bool    `json:"sampled"`
	Source    string  `json:"source"`
	Container string  `json:"container,omitempty"`
	BeforeMiB float64 `json:"before_mib"`
	PeakMiB   float64 `json:"peak_mib"`
	AfterMiB  float64 `json:"after_mib"`
	LimitMiB  float64 `json:"limit_mib"`
	Samples   int     `json:"samples"`
	// Process resident set of the LocalAI processes (the figure docker stats cannot give): the sum of VmRSS over local-ai and
	// the backend it runs, with the anonymous part split out (file-backed pages are the memory-mapped model weights).
	RSSSource    string  `json:"rss_source,omitempty"`
	RSSBeforeMiB float64 `json:"rss_before_mib"`
	RSSPeakMiB   float64 `json:"rss_peak_mib"`
	RSSAfterMiB  float64 `json:"rss_after_mib"`
	RSSAnonPeak  float64 `json:"rss_anon_peak_mib"`
	IntervalNote string  `json:"interval_note,omitempty"`
}

var memRE = regexp.MustCompile(`^\s*([0-9.]+)\s*([KMGT]?i?B)\s*/\s*([0-9.]+)\s*([KMGT]?i?B)`)

func toMiB(v float64, unit string) float64 {
	switch strings.ToUpper(unit) {
	case "B":
		return v / (1 << 20)
	case "KIB", "KB":
		return v / 1024
	case "MIB", "MB":
		return v
	case "GIB", "GB":
		return v * 1024
	case "TIB", "TB":
		return v * 1024 * 1024
	}
	return v
}

func (e liveEnv) memNow(ctx context.Context) (used, limit float64, err error) {
	if e.container == "" {
		return 0, 0, errors.New("no container configured")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, e.docker, "stats", "--no-stream", "--format", "{{.MemUsage}}", e.container).Output()
	if err != nil {
		return 0, 0, err
	}
	m := memRE.FindStringSubmatch(string(out))
	if m == nil {
		return 0, 0, fmt.Errorf("unexpected docker stats output %q", strings.TrimSpace(string(out)))
	}
	u, _ := strconv.ParseFloat(m[1], 64)
	l, _ := strconv.ParseFloat(m[3], 64)
	return toMiB(u, m[2]), toMiB(l, m[4]), nil
}

// rssNow reads the resident set of the LocalAI processes inside the container: VmRSS and RssAnon of every process named
// local-ai, ld.so or llama*, in MiB (the same readout as localai/rss-sampler.sh).
func (e liveEnv) rssNow(ctx context.Context) (total, anon float64, err error) {
	if e.container == "" {
		return 0, 0, errors.New("no container configured")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	script := `for p in /proc/[0-9]*; do n=$(cat "$p/comm" 2>/dev/null) || continue; case "$n" in local-ai|llama*|ld.so) awk '/^VmRSS:/{r=$2} /^RssAnon:/{a=$2} END{print r, a}' "$p/status";; esac; done`
	out, err := exec.CommandContext(cctx, e.docker, "exec", e.container, "sh", "-c", script).Output()
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		r, _ := strconv.ParseFloat(f[0], 64)
		a, _ := strconv.ParseFloat(f[1], 64)
		total += r / 1024
		anon += a / 1024
	}
	return total, anon, nil
}

// trackMem samples the container until the returned function is called, which stops the sampling and reports it.
func (e liveEnv) trackMem() func() memReport {
	rep := memReport{Source: "docker stats --no-stream (cgroup usage minus inactive file cache); emulated: this is the lite profile's cap, not a certified machine"}
	if e.container == "" {
		return func() memReport { rep.Source = "not sampled (no container configured)"; return rep }
	}
	rep.Container = e.container
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	sample := func() {
		u, l, err := e.memNow(ctx)
		if err != nil {
			return
		}
		rss, anon, rerr := e.rssNow(ctx)
		mu.Lock()
		defer mu.Unlock()
		if rerr == nil {
			rep.RSSSource = "docker exec: VmRSS summed over the local-ai, ld.so and llama* processes (file-backed pages of the memory-mapped weights included)"
			if rep.RSSBeforeMiB == 0 {
				rep.RSSBeforeMiB = rss
			}
			rep.RSSAfterMiB = rss
			rep.RSSPeakMiB = max(rep.RSSPeakMiB, rss)
			rep.RSSAnonPeak = max(rep.RSSAnonPeak, anon)
		}
		rep.Sampled = true
		rep.Samples++
		rep.LimitMiB = l
		if rep.Samples == 1 {
			rep.BeforeMiB = u
		}
		rep.AfterMiB = u
		rep.PeakMiB = max(rep.PeakMiB, u)
	}
	sample()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			sample()
		}
	}()
	return func() memReport {
		cancel()
		wg.Wait()
		// One last sample after the load has ended.
		ctx2 := context.Background()
		if u, _, err := e.memNow(ctx2); err == nil {
			mu.Lock()
			rep.AfterMiB = u
			rep.PeakMiB = max(rep.PeakMiB, u)
			mu.Unlock()
		}
		mu.Lock()
		defer mu.Unlock()
		rep.IntervalNote = "back-to-back docker stats calls (each takes about a second); a spike shorter than that can be missed"
		return rep
	}
}

// ---------------------------------------------------------------------------------------------------------------------
// Independent schema validation: the published TaskProposal schema, compiled here from schemas/, not from the gateway's
// embedded copy.

var schemaURNRE = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9]+(?:-[a-z0-9]+)*):([a-z0-9]+(?:-[a-z0-9]+)*):v[1-9][0-9]*$`)

type urnLoader struct{ dir string }

func (l urnLoader) Load(url string) (any, error) {
	m := schemaURNRE.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("only urn:alixai:schema references can be resolved, got %q", url)
	}
	f, err := os.Open(filepath.Join(l.dir, m[1], m[2]+".schema.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jsonschema.UnmarshalJSON(f)
}

func compileTaskProposal(t testing.TB, root string) *jsonschema.Schema {
	t.Helper()
	dir := filepath.Join(root, "schemas")
	f, err := os.Open(filepath.Join(dir, "conversation", "task-proposal.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	const id = "urn:alixai:schema:conversation:task-proposal:v1"
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(urnLoader{dir: dir})
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(id)
	if err != nil {
		t.Fatalf("the TaskProposal schema does not compile: %v", err)
	}
	return s
}

// writeEvidence writes v as indented JSON into the evidence directory, refusing to write the API key.
func writeEvidence(t testing.TB, e liveEnv, name string, v any) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if e.key != "" && bytes.Contains(data, []byte(e.key)) {
		t.Fatalf("refusing to write %s: it contains the API key", name)
	}
	if err := os.MkdirAll(e.evidence, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(e.evidence, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
