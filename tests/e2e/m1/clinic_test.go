package m1

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// clinicDir is the clinic-booking reference application (TypeScript, MCP over streamable HTTP).
const clinicDir = "../../../examples/reference-apps/clinic-booking"

// clinicApp is a running clinic application in test support mode: a subprocess this test
// started, whose process the test holds and the only one it ever stops.
type clinicApp struct {
	t       tb
	baseURL string
	mcpURL  string
	port    int
	bearer  string

	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	err  error
}

// startClinic starts the real clinic application on a loopback port it picks (CLINIC_PORT=0)
// and returns once it has said where it listens. It fails, with what to do about it, when node
// or the application's dependencies are missing: a gate that cannot run the application proves
// nothing, so it does not skip.
func startClinic(t tb) *clinicApp {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is not installed: the end-to-end gate runs the clinic reference application as a subprocess (node 22.18 or newer)")
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
	secret := ensureClinicMCPSecret()
	cmd.Env = append(cmd.Env, "CLINIC_PORT=0", "CLINIC_TEST_SUPPORT=1", "CLINIC_MCP_BEARER="+secret)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the clinic application: %v", err)
	}
	c := &clinicApp{t: t, cmd: cmd, done: make(chan struct{}), bearer: secret}
	go func() {
		err := cmd.Wait()
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		close(c.done)
	}()
	t.Cleanup(c.stop)

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
		c.baseURL, c.mcpURL, c.port = ev.URL, ev.MCP, ev.Port
	case <-time.After(60 * time.Second):
		t.Fatalf("the clinic application did not start in time\nstderr:\n%s", stderr.String())
	}
	return c
}

// stop stops the process this test started, and nothing else. It is idempotent.
func (c *clinicApp) stop() {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
		c.t.Errorf("the clinic application (pid %d) did not exit", c.cmd.Process.Pid)
	}
}

// alive reports whether the subprocess is still running: a test that finds it gone has not
// proved what it thinks it has.
func (c *clinicApp) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

var clinicHTTP = &http.Client{Timeout: 30 * time.Second}

func (c *clinicApp) httpJSON(method, path string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := clinicHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %w: %s", method, path, err, raw)
		}
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s = %d: %s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, nil
}

// ---- the application's own account of what it was asked ---------------------------------

// clinicStats is GET /test/stats: how often each tool was called and how many rows of each
// kind the application holds.
type clinicStats struct {
	Calls  map[string]int `json:"calls"`
	Totals map[string]int `json:"totals"`
	// ClockOffsetSeconds is how far the application's clock has been moved forward.
	ClockOffsetSeconds int `json:"clock_offset_seconds"`
}

func (c *clinicApp) stats(t tb) clinicStats {
	t.Helper()
	var s clinicStats
	if _, err := c.httpJSON(http.MethodGet, "/test/stats", nil, &s); err != nil {
		t.Fatalf("the application's counters: %v", err)
	}
	if s.Calls == nil {
		s.Calls = map[string]int{}
	}
	return s
}

// commits is how many times the application was asked to commit anything: the sum of every
// tool that ends in _commit. This is the number the fail-closed matrix holds at zero.
func (s clinicStats) commits() int {
	n := 0
	for tool, k := range s.Calls {
		if strings.HasSuffix(tool, "_commit") {
			n += k
		}
	}
	return n
}

func (s clinicStats) commitsOf(tool string) int { return s.Calls[tool+"_commit"] }

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

// ---- MCP, for the test's own provisioning and inspection --------------------------------

// toolReply is the structured content of a tool call.
type toolReply struct {
	Status string `json:"status"`
	Errors []struct {
		Code string `json:"code"`
	} `json:"errors"`
	Raw json.RawMessage `json:"-"`
}

// callTool calls one MCP tool of the application directly (the clinic's server is stateless:
// a tools/call needs no handshake) and returns its structured content. This is the test's way
// to provision and to read what the application holds; it is not how AlixAI reaches the
// application, which is the MCP adapter built from the package.
func (c *clinicApp) callTool(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := clinicHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: the reply is not JSON-RPC: %w: %s", tool, err, raw)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("%s: %s", tool, env.Error.Message)
	}
	if env.Result.IsError {
		return nil, fmt.Errorf("%s: the tool reported an error: %s", tool, raw)
	}
	return env.Result.StructuredContent, nil
}

// mustTool calls a tool and fails the test unless it answers status ok.
func (c *clinicApp) mustTool(t tb, tool string, args map[string]any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := c.callTool(ctx, tool, args)
	if err != nil {
		t.Fatalf("%v", err)
	}
	var r toolReply
	if err := json.Unmarshal(raw, &r); err != nil || r.Status != "ok" {
		t.Fatalf("%s did not answer ok: %s", tool, raw)
	}
	return raw
}

// ---- host tokens --------------------------------------------------------------------------

// hostToken asks the application, standing in for the host's login, for a context token for a
// principal of a tenant and workspace. Every token is single use, so a request asks for its own.
// lifetime is in seconds (the profile allows 1 to 900); sessionID is the conversation the token
// belongs to.
func (c *clinicApp) hostToken(t tb, tenant, workspace, principal, sessionID string, lifetime int) string {
	t.Helper()
	return c.hostTokenScoped(t, tenant, workspace, principal, sessionID, lifetime, nil)
}

// hostTokenScoped is hostToken with the capability scopes the host grants the token: by default the
// scopes of the principal's role, here whatever the host decides to grant (more or fewer).
func (c *clinicApp) hostTokenScoped(t tb, tenant, workspace, principal, sessionID string, lifetime int, scopes []string) string {
	t.Helper()
	body := map[string]any{
		"tenant_id": tenant, "workspace_id": workspace, "principal_id": principal,
		"session_id": sessionID, "lifetime_seconds": lifetime,
	}
	if scopes != nil {
		body["capability_scopes"] = scopes
	}
	var out struct {
		Token string `json:"token"`
	}
	if _, err := c.httpJSON(http.MethodPost, "/host/context-token", body, &out); err != nil || out.Token == "" {
		t.Fatalf("the host would not mint a context token for %s/%s/%s: %v", tenant, workspace, principal, err)
	}
	return out.Token
}

// advanceClock moves the application's clock forward (it can only move forward).
func (c *clinicApp) advanceClock(t tb, d time.Duration) {
	t.Helper()
	if _, err := c.httpJSON(http.MethodPost, "/test/clock", map[string]any{"advance_seconds": int(d / time.Second)}, nil); err != nil {
		t.Fatalf("advance the application's clock: %v", err)
	}
}
