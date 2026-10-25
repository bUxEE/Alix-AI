package dr

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// deployment is one scenario's own copy of the golden deployment: a data directory, a
// configuration, and the real commands and service run against them.
type deployment struct {
	t       *testing.T
	e       *env
	root    string
	dataDir string
	cfgPath string
	listen  string
	// extra is YAML appended to the configuration at every start (another section).
	extra string
}

// newDeployment copies the golden data directory into a temporary directory of the test. Nothing a
// scenario does reaches the golden copy or another scenario.
func (e *env) newDeployment(t *testing.T) *deployment {
	t.Helper()
	root := t.TempDir()
	d := &deployment{t: t, e: e, root: root, dataDir: filepath.Join(root, "data"), cfgPath: filepath.Join(root, "alixai.yaml")}
	if err := copyTree(filepath.Join(e.goldenRoot, "data"), d.dataDir); err != nil {
		t.Fatalf("copy the golden data directory: %v", err)
	}
	d.relisten()
	return d
}

// sibling is another data directory and configuration next to this deployment's, for a restore
// rehearsal: the same binary, the same signer, an empty directory.
func (d *deployment) sibling(name string) *deployment {
	d.t.Helper()
	root := filepath.Join(d.root, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		d.t.Fatal(err)
	}
	c := &deployment{t: d.t, e: d.e, root: root, dataDir: filepath.Join(root, "data"), cfgPath: filepath.Join(root, "alixai.yaml")}
	c.relisten()
	return c
}

// relisten picks a fresh loopback address for the next service start and writes the configuration.
func (d *deployment) relisten() {
	d.t.Helper()
	addr, err := freeAddr()
	if err != nil {
		d.t.Fatal(err)
	}
	d.listen = addr
	if err := d.e.writeConfig(d.cfgPath, d.dataDir, addr, d.extra); err != nil {
		d.t.Fatal(err)
	}
}

func (d *deployment) dbPath() string { return filepath.Join(d.dataDir, "alixai.db") }

// run executes one command of the built binary against this deployment.
func (d *deployment) run(args ...string) result { return d.e.run(d.cfgPath, args...) }

// must runs a command that has to succeed.
func (d *deployment) must(args ...string) result {
	d.t.Helper()
	r := d.run(args...)
	if r.Code != 0 {
		d.t.Fatalf("alixai %s: %s", strings.Join(args, " "), r)
	}
	return r
}

// ---- the service ------------------------------------------------------------------------------

// serveProc is a running `alixai serve`: a subprocess the test started and whose PID it holds.
type serveProc struct {
	d      *deployment
	cmd    *exec.Cmd
	pid    int
	done   chan struct{}
	base   string
	mu     sync.Mutex
	stdout bytes.Buffer
	stderr bytes.Buffer
	// ready is how long the service took from start to accepting connections, which includes
	// resolving interrupted commits.
	ready time.Duration
}

var serveHTTP = &http.Client{Timeout: 3 * time.Minute}

// startServe starts the service on a fresh address and waits until it says it is listening. The
// service is killed when the test ends if the test has not done it already.
func (d *deployment) startServe() *serveProc {
	d.t.Helper()
	d.relisten()
	cmd := exec.Command(d.e.bin, "serve", "-config", d.cfgPath)
	cmd.Dir = d.e.moduleRoot
	cmd.Env = cleanEnv()
	p := &serveProc{d: d, cmd: cmd, done: make(chan struct{})}
	out, err := cmd.StdoutPipe()
	if err != nil {
		d.t.Fatal(err)
	}
	cmd.Stderr = &lockedWriter{mu: &p.mu, b: &p.stderr}
	begin := time.Now()
	if err := cmd.Start(); err != nil {
		d.t.Fatalf("start the service: %v", err)
	}
	p.pid = cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	d.t.Cleanup(p.kill)

	listening := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			line := sc.Text()
			p.mu.Lock()
			p.stdout.WriteString(line + "\n")
			p.mu.Unlock()
			if rest, ok := strings.CutPrefix(line, "alixai: listening on "); ok {
				select {
				case listening <- strings.TrimSpace(rest):
				default:
				}
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case base := <-listening:
		p.base = base
		p.ready = time.Since(begin)
	case <-p.done:
		d.t.Fatalf("the service exited before it listened:\n%s", p.logs())
	case <-time.After(90 * time.Second):
		p.kill()
		d.t.Fatalf("the service did not start in time:\n%s", p.logs())
	}
	return p
}

// startServeExpectingFailure starts the service and reports whether it exited by itself within the
// limit, with its exit code and what it said. A service that stays up is killed.
func (d *deployment) startServeExpectingFailure(limit time.Duration) (exited bool, code int, logs string) {
	d.t.Helper()
	d.relisten()
	cmd := exec.Command(d.e.bin, "serve", "-config", d.cfgPath)
	cmd.Dir = d.e.moduleRoot
	cmd.Env = cleanEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		d.t.Fatalf("start the service: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code = 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return true, code, out.String() + errb.String()
	case <-time.After(limit):
		_ = cmd.Process.Kill()
		<-done
		return false, 0, out.String() + errb.String()
	}
}

func (p *serveProc) logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return "stdout:\n" + p.stdout.String() + "stderr:\n" + p.stderr.String()
}

func (p *serveProc) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

// kill stops the service the hard way: TerminateProcess on Windows, SIGKILL elsewhere. No handler
// runs, no checkpoint is written, SQLite's -wal and -shm files stay behind. Only the PID this test
// started is touched. It is idempotent.
func (p *serveProc) kill() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		p.d.t.Errorf("the service (pid %d) did not exit after a kill", p.pid)
	}
}

// call is one request to the service with a context token.
func (p *serveProc) call(method, path, token string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := serveHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// preparationState reads the stored state of a preparation through the service.
func (p *serveProc) preparationState(t *testing.T, w *world, sessionID, prepID string) string {
	t.Helper()
	status, raw, err := p.call(http.MethodGet, "/v1/actions/"+prepID, w.hostToken(t, sessionID), nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /v1/actions/%s = %d, %v: %s", prepID, status, err, raw)
	}
	var pa struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &pa); err != nil {
		t.Fatalf("the preparation is not JSON: %v: %s", err, raw)
	}
	return pa.State
}

// receipt reads the receipt of a preparation: the status code, and the body when there is one.
func (p *serveProc) receipt(t *testing.T, w *world, sessionID, prepID string) (int, []byte) {
	t.Helper()
	status, raw, err := p.call(http.MethodGet, "/v1/actions/"+prepID+"/receipt", w.hostToken(t, sessionID), nil)
	if err != nil {
		t.Fatalf("GET /v1/actions/%s/receipt: %v", prepID, err)
	}
	return status, raw
}

// confirm presents the confirmation secret. Confirming and committing are one request: the
// response carries the receipt.
func (p *serveProc) confirm(t *testing.T, w *world, sessionID, prepID, secret string) (int, []byte, error) {
	t.Helper()
	return p.call(http.MethodPost, "/v1/actions/"+prepID+"/confirm", w.hostToken(t, sessionID), map[string]any{"token": secret})
}

// ---- files on disk -----------------------------------------------------------------------------

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("not the JSON that was expected: %v: %s", err, raw)
	}
}

func fail(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatalf(format, args...)
}

var _ = fmt.Sprintf
