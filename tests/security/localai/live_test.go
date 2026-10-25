//go:build live

package localaisec

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// The live half of the suite probes a running LocalAI stack from outside. It never runs in verify: it needs the tag
// `live` and ALIXAI_SEC_LOCALAI_URL, the base URL of the stack's LocalAI endpoint (for example http://127.0.0.1:8080).
//
//	ALIXAI_SEC_LOCALAI_URL      base URL of the LocalAI port; every live test skips without it
//	ALIXAI_SEC_LOCALAI_KEY      optional: the API key, sent only to that URL and only when it is a loopback or private address
//	ALIXAI_SEC_LOCALAI_LAN_URL  optional: the same port on a LAN address of the host; it must refuse every connection
//
// Install attempts carry harmless bodies that point at a closed loopback port: nothing is ever fetched from outside.

const (
	envLiveURL = "ALIXAI_SEC_LOCALAI_URL"
	envLiveKey = "ALIXAI_SEC_LOCALAI_KEY"
	envLiveLAN = "ALIXAI_SEC_LOCALAI_LAN_URL"

	liveRequestTimeout = 5 * time.Second
	liveTestDeadline   = 30 * time.Second
)

// exemptRoutes are the routes the checklist (HC-11, measured in M0-13) lets answer without a key.
var exemptRoutes = map[string]bool{
	"/readyz": true, "/healthz": true, "/swagger/index.html": true, "/api/instructions": true,
	"/.well-known/localai.json": true, "/api/auth/status": true,
}

type liveProbe struct{ method, path, body string }

const closedLoopback = "http://127.0.0.1:1/"

// noKeyProbes are the surfaces that must refuse a caller without the key: the model list, install and apply, the
// backends, settings, agent, MCP and CORS-proxy paths, the gallery, inference and the web UI.
var noKeyProbes = []liveProbe{
	{"GET", "/v1/models", ""},
	{"GET", "/models", ""},
	{"GET", "/models/available", ""},
	{"GET", "/models/galleries", ""},
	{"POST", "/models/apply", `{"url":"` + closedLoopback + `m.yaml"}`},
	{"POST", "/models/import-uri", `{"uri":"` + closedLoopback + `m.gguf"}`},
	{"POST", "/models/delete/probe", ""},
	{"GET", "/backends", ""},
	{"GET", "/backends/available", ""},
	{"POST", "/backends/apply", `{"id":"probe","url":"` + closedLoopback + `b.yaml"}`},
	{"POST", "/backends/delete/probe", ""},
	{"GET", "/api/settings", ""},
	{"POST", "/api/settings", `{}`},
	{"GET", "/api/agents", ""},
	{"POST", "/api/agents", `{}`},
	{"GET", "/api/agent/tasks", ""},
	{"GET", "/api/cors-proxy?url=" + url.QueryEscape(closedLoopback), ""},
	{"POST", "/v1/mcp/chat/completions", `{}`},
	{"GET", "/api/mcp", ""},
	{"GET", "/api/nodes", ""},
	{"GET", "/api/p2p", ""},
	{"GET", "/api/traces", ""},
	{"GET", "/metrics", ""},
	{"POST", "/v1/chat/completions", `{"model":"probe","messages":[{"role":"user","content":"x"}]}`},
	{"POST", "/v1/embeddings", `{"model":"probe","input":"x"}`},
	{"POST", "/v1/audio/transcriptions", ``},
	{"GET", "/", ""},
	{"GET", "/app", ""},
	{"GET", "/browse", ""},
}

// disabledWithKey are the surfaces that must not serve content even with the key (checklist section 5, item 4).
var disabledWithKey = []liveProbe{
	{"POST", "/models/apply", `{"url":"` + closedLoopback + `m.yaml"}`},
	{"POST", "/models/import-uri", `{"uri":"` + closedLoopback + `m.gguf"}`},
	{"GET", "/models/available", ""},
	{"GET", "/models/galleries", ""},
	{"GET", "/backends", ""},
	{"GET", "/backends/available", ""},
	{"POST", "/backends/apply", `{"id":"probe","url":"` + closedLoopback + `b.yaml"}`},
	{"GET", "/api/settings", ""},
	{"GET", "/api/agents", ""},
	{"GET", "/api/agent/tasks", ""},
	{"GET", "/api/cors-proxy?url=" + url.QueryEscape(closedLoopback), ""},
	{"GET", "/api/nodes", ""},
	{"GET", "/", ""},
	{"GET", "/app", ""},
	{"GET", "/browse", ""},
}

type liveResp struct {
	status int
	body   string
	err    error
}

func liveClient() *http.Client {
	return &http.Client{
		Timeout:       liveRequestTimeout,
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// keyHostOK reports whether the key may be sent to base: a loopback or private address, never a public name.
func keyHostOK(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func liveDo(ctx context.Context, base string, p liveProbe, key string) liveResp {
	var body io.Reader
	if p.body != "" {
		body = strings.NewReader(p.body)
	}
	req, err := http.NewRequestWithContext(ctx, p.method, strings.TrimRight(base, "/")+p.path, body)
	if err != nil {
		return liveResp{err: err}
	}
	if p.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := liveClient().Do(req)
	if err != nil {
		return liveResp{err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return liveResp{status: resp.StatusCode, body: string(b)}
}

func liveSetup(t *testing.T) (ctx context.Context, base, key string) {
	t.Helper()
	base = os.Getenv(envLiveURL)
	if base == "" {
		t.Skipf("%s is not set: no running LocalAI stack to probe", envLiveURL)
	}
	key = os.Getenv(envLiveKey)
	if key != "" && !keyHostOK(base) {
		t.Fatalf("%s is set but %s is not a loopback or private address: the key is never sent there", envLiveKey, envLiveURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveTestDeadline)
	t.Cleanup(cancel)
	return ctx, base, key
}

// refused is true for the three answers the checklist accepts from a protected route.
func refused(status int) bool { return status == 401 || status == 403 || status == 404 }

// liveReachable is the control of every live test: an exempt route answers 200, so the URL does point at a live
// LocalAI and a refusal below means something.
func liveReachable(ctx context.Context, t *testing.T, base string) {
	t.Helper()
	ok := false
	for _, p := range []string{"/readyz", "/healthz"} {
		if r := liveDo(ctx, base, liveProbe{"GET", p, ""}, ""); r.err == nil && r.status == 200 {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("neither /readyz nor /healthz of %s answers 200: the stack is not up, and a refusal would prove nothing", base)
	}
}

// SEC-066
func TestSecLocalAIUnreachableWithoutKey(t *testing.T) {
	ctx, base, key := liveSetup(t)
	liveReachable(ctx, t, base)

	t.Run("no_key_no_service", func(t *testing.T) {
		for _, p := range noKeyProbes {
			r := liveDo(ctx, base, p, "")
			path := p.path
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			switch {
			case r.err != nil:
				t.Errorf("%s %s: %v", p.method, p.path, r.err)
			case exemptRoutes[path]:
			case !refused(r.status):
				t.Errorf("%s %s without the key answered %d, want 401, 403 or 404", p.method, p.path, r.status)
			}
		}
	})

	t.Run("a_wrong_key_is_refused", func(t *testing.T) {
		r := liveDo(ctx, base, liveProbe{"GET", "/v1/models", ""}, "not-the-key-"+time.Now().Format("150405"))
		if r.err != nil || !refused(r.status) {
			t.Errorf("GET /v1/models with a wrong key: %d %v, want 401, 403 or 404", r.status, r.err)
		}
	})

	t.Run("the_refusal_is_the_key_and_not_a_missing_route", func(t *testing.T) {
		if key == "" {
			t.Skipf("%s is not set: the control that the key opens /v1/models needs it", envLiveKey)
		}
		if r := liveDo(ctx, base, liveProbe{"GET", "/v1/models", ""}, key); r.err != nil || r.status != 200 {
			t.Errorf("GET /v1/models with the key: %d %v, want 200", r.status, r.err)
		}
	})

	t.Run("lan_address_refuses_connections", func(t *testing.T) {
		lan := os.Getenv(envLiveLAN)
		if lan == "" {
			t.Skipf("%s is not set: the LAN probe needs the host's LAN address", envLiveLAN)
		}
		if u, err := url.Parse(lan); err != nil || net.ParseIP(u.Hostname()) == nil || net.ParseIP(u.Hostname()).IsLoopback() {
			t.Fatalf("%s must be a non-loopback IP address URL, got %q", envLiveLAN, lan)
		}
		for _, path := range []string{"/readyz", "/v1/models"} {
			r := liveDo(ctx, lan, liveProbe{"GET", path, ""}, "")
			if r.err == nil {
				t.Errorf("GET %s%s answered %d: LocalAI is reachable from the LAN address", lan, path, r.status)
			}
		}
	})
}

// secLocalAILiveInstallProbe is the live half of SEC-069: install and apply attempts are refused without the key and,
// when the key is known, with it, and the set of installed models does not change.
func secLocalAILiveInstallProbe(t *testing.T) {
	t.Helper()
	t.Run("live_install_attempts_are_refused", func(t *testing.T) {
		ctx, base, key := liveSetup(t)
		liveReachable(ctx, t, base)
		var installs []liveProbe
		for _, p := range disabledWithKey {
			if p.method == "POST" || strings.Contains(p.path, "galleries") || strings.Contains(p.path, "available") || p.path == "/backends" {
				installs = append(installs, p)
			}
		}
		installs = append(installs,
			liveProbe{"POST", "/api/models/install/probe", `{}`},
			liveProbe{"POST", "/api/models/apply", `{"url":"` + closedLoopback + `m.yaml"}`},
			liveProbe{"POST", "/models/galleries", `{"name":"x","url":"` + closedLoopback + `g.yaml"}`},
			liveProbe{"POST", "/backends/install", `{"id":"probe"}`},
		)
		for _, p := range installs {
			if r := liveDo(ctx, base, p, ""); r.err != nil || !refused(r.status) {
				t.Errorf("%s %s without the key: %d %v, want 401, 403 or 404", p.method, p.path, r.status, r.err)
			}
		}
		if key == "" {
			t.Logf("%s is not set: the attempts with the key were not made", envLiveKey)
			return
		}
		before := liveModelIDs(ctx, t, base, key)
		for _, p := range installs {
			r := liveDo(ctx, base, p, key)
			if r.err != nil || !refused(r.status) {
				t.Errorf("%s %s with the key: %d %v, want 401, 403 or 404 (the install surface is disabled)", p.method, p.path, r.status, r.err)
			}
			if strings.Contains(strings.ToLower(r.body), `"uuid"`) || strings.Contains(strings.ToLower(r.body), `"job`) {
				t.Errorf("%s %s with the key started a job: %s", p.method, p.path, r.body)
			}
		}
		after := liveModelIDs(ctx, t, base, key)
		if strings.Join(before, ",") != strings.Join(after, ",") {
			t.Errorf("the installed models changed during the attempts: %v -> %v", before, after)
		}
	})
}

// liveModelIDs lists the model ids the runtime reports, sorted.
func liveModelIDs(ctx context.Context, t *testing.T, base, key string) []string {
	t.Helper()
	r := liveDo(ctx, base, liveProbe{"GET", "/v1/models", ""}, key)
	if r.err != nil || r.status != 200 {
		t.Fatalf("GET /v1/models with the key: %d %v", r.status, r.err)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.body), &doc); err != nil {
		t.Fatalf("GET /v1/models is not the model list: %v", err)
	}
	var ids []string
	for _, d := range doc.Data {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return ids
}

// secLocalAILiveAgentsProbe is the live half of SEC-070: the agent pool, the MCP paths and the CORS proxy do not serve
// content, without the key or with it.
func secLocalAILiveAgentsProbe(t *testing.T) {
	t.Helper()
	t.Run("live_agents_mcp_and_cors_proxy_are_gone", func(t *testing.T) {
		ctx, base, key := liveSetup(t)
		liveReachable(ctx, t, base)
		probes := []liveProbe{
			{"GET", "/api/agents", ""},
			{"POST", "/api/agents", `{}`},
			{"GET", "/api/agent/tasks", ""},
			{"GET", "/api/cors-proxy?url=" + url.QueryEscape(closedLoopback), ""},
			{"POST", "/v1/mcp/chat/completions", `{}`},
			{"GET", "/api/mcp", ""},
		}
		for _, p := range probes {
			if r := liveDo(ctx, base, p, ""); r.err != nil || !refused(r.status) {
				t.Errorf("%s %s without the key: %d %v, want 401, 403 or 404", p.method, p.path, r.status, r.err)
			}
		}
		if key == "" {
			t.Logf("%s is not set: the probes with the key were not made", envLiveKey)
			return
		}
		for _, p := range probes {
			r := liveDo(ctx, base, p, key)
			if r.err != nil || r.status != 404 {
				t.Errorf("%s %s with the key: %d %v, want 404 (HC-15, HC-16: the surface is not served even to the key holder)", p.method, p.path, r.status, r.err)
			}
		}
	})
}
