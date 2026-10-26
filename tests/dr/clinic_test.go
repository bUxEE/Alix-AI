package dr

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// clinicApp is the running clinic reference application in test support mode: a subprocess this
// test started, whose process the test holds and the only one it ever stops.
type clinicApp struct {
	baseURL string
	mcpURL  string
	port    int
	// bearer is CLINIC_MCP_BEARER; every request to the app except /healthz must present it.
	bearer string

	cmd    *exec.Cmd
	done   chan struct{}
	pid    int
	mu     sync.Mutex
	stderr bytes.Buffer
}

var clinicHTTP = &http.Client{Timeout: 30 * time.Second}

// startClinic starts the real clinic application on a loopback port it picks (CLINIC_PORT=0) and
// returns once it has said where it listens. It fails, with what to do about it, when node or the
// application's dependencies are missing: a drill that cannot run the application proves nothing,
// so it does not skip.
func startClinic(moduleRoot, mcpBearer string) (*clinicApp, error) {
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, errors.New("node is not installed: the drill runs the clinic reference application as a subprocess (node 22.18 or newer)")
	}
	appDir := filepath.Join(moduleRoot, "examples", "reference-apps", "clinic-booking")
	if _, err := os.Stat(filepath.Join(appDir, "node_modules")); err != nil {
		return nil, fmt.Errorf("the clinic application is not installed: run `pnpm install --ignore-workspace --offline --frozen-lockfile` in %s", appDir)
	}
	cmd := exec.Command(node, "src/main.ts")
	cmd.Dir = appDir
	for _, kv := range cleanEnv() {
		if !strings.HasPrefix(kv, "CLINIC_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "CLINIC_PORT=0", "CLINIC_TEST_SUPPORT=1", "CLINIC_MCP_BEARER="+mcpBearer)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &clinicApp{cmd: cmd, done: make(chan struct{}), bearer: mcpBearer}
	cmd.Stderr = &lockedWriter{mu: &c.mu, b: &c.stderr}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the clinic application: %w", err)
	}
	c.pid = cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		close(c.done)
	}()

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
			c.stop()
			return nil, fmt.Errorf("the clinic application did not report where it listens: %q\nstderr:\n%s", line, c.stderrText())
		}
		c.baseURL, c.mcpURL, c.port = ev.URL, ev.MCP, ev.Port
	case <-time.After(60 * time.Second):
		c.stop()
		return nil, fmt.Errorf("the clinic application did not start in time\nstderr:\n%s", c.stderrText())
	}
	return c, nil
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (c *clinicApp) stderrText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stderr.String()
}

// stop stops the process this test started, and nothing else. It is idempotent.
func (c *clinicApp) stop() {
	select {
	case <-c.done:
		return
	default:
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
	}
}

func (c *clinicApp) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// httpJSON is one request to the application's own HTTP surface.
func (c *clinicApp) httpJSON(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := clinicHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s = %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// clinicStats is GET /test/stats: how often each tool was called.
type clinicStats struct {
	Calls  map[string]int `json:"calls"`
	Totals map[string]int `json:"totals"`
}

func (c *clinicApp) stats(t *testing.T) clinicStats {
	t.Helper()
	var s clinicStats
	if err := c.httpJSON(http.MethodGet, "/test/stats", nil, &s); err != nil {
		t.Fatalf("the application's counters: %v", err)
	}
	if s.Calls == nil {
		s.Calls = map[string]int{}
	}
	return s
}

// commits is how many times the application was asked to commit the given capability's tool.
func (s clinicStats) commitsOf(tool string) int { return s.Calls[tool+"_commit"] }

// callTool calls one MCP tool of the application directly (its server is stateless: a tools/call
// needs no handshake) and returns the structured content. This is the test's way to provision and
// to read what the application holds; it is not how AlixAI reaches the application.
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
	switch {
	case env.Error != nil:
		return nil, fmt.Errorf("%s: %s", tool, env.Error.Message)
	case env.Result.IsError:
		return nil, fmt.Errorf("%s: the tool reported an error: %s", tool, raw)
	}
	return env.Result.StructuredContent, nil
}

func (c *clinicApp) mustTool(t *testing.T, tool string, args map[string]any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := c.callTool(ctx, tool, args)
	if err != nil {
		t.Fatalf("%v", err)
	}
	var r struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Status != "ok" {
		t.Fatalf("%s did not answer ok: %s", tool, raw)
	}
	return raw
}

// ---- a disposable tenant -------------------------------------------------------------------

// world is a disposable tenant of the clinic application: one clinic group with one site, a
// receptionist, one named patient and free slots with a doctor. Every record is synthetic.
type world struct {
	clinic    *clinicApp
	Tenant    string
	Site      string
	Reception string
	PatientID string
	Slots     []string
}

func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// seedWorld creates a disposable tenant through the application's own test tools and removes it
// when the test ends.
func seedWorld(t *testing.T, clinic *clinicApp, label string, slots int) *world {
	t.Helper()
	suffix := randomSuffix()
	w := &world{
		clinic: clinic, Tenant: "tnt_" + label + "_" + suffix, Site: "ws_" + label + "_a_" + suffix,
		Reception: "usr_" + label + "_reception_" + suffix,
	}
	doctor := "usr_" + label + "_doctor_" + suffix
	clinic.mustTool(t, "test_create_tenant", map[string]any{
		"tenant_id": w.Tenant, "name": "Drill Clinic Group " + label,
		"workspaces": []map[string]any{{"workspace_id": w.Site, "name": "Drill Site", "timezone": "Europe/Rome"}},
		"principals": []map[string]any{
			{"principal_id": w.Reception, "workspace_id": w.Site, "role": "receptionist", "display_name": "Drill receptionist"},
			{"principal_id": doctor, "workspace_id": w.Site, "role": "doctor", "display_name": "Drill doctor"},
		},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = clinic.callTool(ctx, "test_cleanup", map[string]any{"tenant_id": w.Tenant})
	})
	records := []map[string]any{{"ref": "alma", "entity_type": "patient", "workspace_id": w.Site, "given_name": "Alma", "family_name": "Gatefield"}}
	for i := 1; i <= slots; i++ {
		records = append(records, map[string]any{"ref": fmt.Sprintf("slot%d", i), "entity_type": "slot", "workspace_id": w.Site, "principal_id": doctor})
	}
	raw := clinic.mustTool(t, "test_seed", map[string]any{"tenant_id": w.Tenant, "records": records})
	var seeded struct {
		Records []struct {
			Ref      string `json:"ref"`
			EntityID string `json:"entity_id"`
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &seeded); err != nil {
		t.Fatalf("test_seed: %v: %s", err, raw)
	}
	byRef := map[string]string{}
	for _, r := range seeded.Records {
		byRef[r.Ref] = r.EntityID
	}
	if w.PatientID = byRef["alma"]; w.PatientID == "" {
		t.Fatalf("test_seed did not return the patient: %s", raw)
	}
	for i := 1; i <= slots; i++ {
		id := byRef[fmt.Sprintf("slot%d", i)]
		if id == "" {
			t.Fatalf("test_seed did not return slot %d: %s", i, raw)
		}
		w.Slots = append(w.Slots, id)
	}
	return w
}

// hostToken asks the application, standing in for the host's login, for a single-use context token
// of the receptionist for a session.
func (w *world) hostToken(t *testing.T, sessionID string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	err := w.clinic.httpJSON(http.MethodPost, "/host/context-token", map[string]any{
		"tenant_id": w.Tenant, "workspace_id": w.Site, "principal_id": w.Reception,
		"session_id": sessionID, "lifetime_seconds": 600,
	}, &out)
	if err != nil || out.Token == "" {
		t.Fatalf("the host would not mint a context token: %v", err)
	}
	return out.Token
}

// tokenClaims decodes the claims of a context token the application minted (the test reads what the
// application asserted; AlixAI verifies the signature itself).
func tokenClaims(token string) (roles, scopes []string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("not a compact JWS")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, err
	}
	var c struct {
		Roles  []string `json:"roles"`
		Scopes []string `json:"capability_scopes"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, nil, err
	}
	return c.Roles, c.Scopes, nil
}
