package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
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

// clinicDir is the clinic-booking reference application (TypeScript, MCP over streamable
// HTTP) and clinicPackage its signed Application Integration Package.
const (
	clinicDir     = "../../examples/reference-apps/clinic-booking"
	clinicPackage = clinicDir + "/package"
)

// clinicApp is a running clinic application in test support mode: a subprocess this test
// started, whose process the test holds and the only one it ever stops.
type clinicApp struct {
	baseURL string
	mcpURL  string

	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	err  error
}

// startClinic starts the real clinic application on a loopback port it picks (CLINIC_PORT=0)
// and returns once it has said where it listens. It fails, with what to do about it, when node
// or the application's dependencies are missing: a conformance run that cannot reach the
// application proves nothing, so it does not skip.
func startClinic(t *testing.T) *clinicApp {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is not installed: the conformance tests run the clinic reference application as a subprocess (node 22.18 or newer)")
	}
	appDir, err := filepath.Abs(clinicDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "node_modules")); err != nil {
		t.Fatalf("the clinic application is not installed: run `pnpm install --ignore-workspace --offline --frozen-lockfile` in %s", appDir)
	}
	cmd := exec.Command(node, "src/main.ts")
	cmd.Dir = appDir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLINIC_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "CLINIC_PORT=0", "CLINIC_TEST_SUPPORT=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the clinic application: %v", err)
	}
	c := &clinicApp{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	}()
	t.Cleanup(func() { c.stop(t) })

	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		if sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case line, ok := <-lines:
		var ev struct {
			Event, URL, MCP string
			Port            int
		}
		if !ok || json.Unmarshal([]byte(line), &ev) != nil || ev.Event != "listening" || ev.MCP == "" || ev.URL == "" || ev.Port == 0 {
			t.Fatalf("the clinic application did not report where it listens: %q\nstderr:\n%s", line, stderr.String())
		}
		c.baseURL, c.mcpURL = ev.URL, ev.MCP
	case <-time.After(60 * time.Second):
		t.Fatalf("the clinic application did not start in time\nstderr:\n%s", stderr.String())
	}
	return c
}

// stop stops the process this test started, and nothing else. It is idempotent.
func (c *clinicApp) stop(t *testing.T) {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
		t.Errorf("the clinic application (pid %d) did not exit", c.cmd.Process.Pid)
	}
}

// clinicStats is GET /test/stats: how often each tool was called and how many rows of each
// kind the application holds.
type clinicStats struct {
	Calls  map[string]int `json:"calls"`
	Totals map[string]int `json:"totals"`
}

// prepares is how many times the application was asked to prepare anything.
func (s clinicStats) prepares() int {
	n := 0
	for tool, k := range s.Calls {
		if strings.HasSuffix(tool, "_prepare") {
			n += k
		}
	}
	return n
}

func (c *clinicApp) stats(t *testing.T) clinicStats {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/test/stats", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the application's counters: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the application's counters: %d %v %s", resp.StatusCode, err, raw)
	}
	var s clinicStats
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("the application's counters: %v: %s", err, raw)
	}
	if s.Calls == nil {
		s.Calls = map[string]int{}
	}
	return s
}

// conform runs the tool in-process as its command line and returns what it did.
type conformRun struct {
	code   int
	stdout string
	stderr string
	report Report
	junit  junitSuites
}

// runConformance runs the command line with a JSON and a JUnit file added, and reads both
// back. The command's own stdout is whatever the arguments left on it.
func runConformance(t *testing.T, args ...string) conformRun {
	t.Helper()
	dir := t.TempDir()
	jsonPath, junitPath := filepath.Join(dir, "report.json"), filepath.Join(dir, "report.xml")
	full := append([]string{"-json", jsonPath, "-junit", junitPath}, args...)
	var out, errb bytes.Buffer
	res := conformRun{}
	res.code = run(context.Background(), full, &out, &errb)
	res.stdout, res.stderr = out.String(), errb.String()
	if raw, err := os.ReadFile(jsonPath); err == nil {
		if err := json.Unmarshal(raw, &res.report); err != nil {
			t.Fatalf("the JSON report is not JSON: %v\n%s", err, raw)
		}
	}
	if raw, err := os.ReadFile(junitPath); err == nil {
		if err := xml.Unmarshal(raw, &res.junit); err != nil {
			t.Fatalf("the JUnit report is not XML: %v\n%s", err, raw)
		}
	}
	return res
}

func (r conformRun) stage(t *testing.T, name string) Stage {
	t.Helper()
	for _, s := range r.report.Stages {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("the report has no %s stage: %s", name, r.stderr)
	return Stage{}
}

func (s Stage) item(id string) (Item, bool) {
	for _, it := range s.Items {
		if it.ID == id {
			return it, true
		}
	}
	return Item{}, false
}

func (s Stage) count(status Status) int {
	n := 0
	for _, it := range s.Items {
		if it.Status == status {
			n++
		}
	}
	return n
}

func describe(r conformRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit %d\n%s", r.code, r.stderr)
	return b.String()
}
