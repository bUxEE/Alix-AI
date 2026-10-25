package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

// The stack probes: how the harness sees what the stack says about itself.
//
// Two ways, because the stack has two doors. The container stack publishes one
// port (the API, 127.0.0.1:7470); the operator's /metrics door is loopback
// inside the AlixAI network namespace and is not reachable from the host
// unless the operator forwards it. So:
//
//   - readyz + ops (the default): the model roles from /readyz (unauthenticated)
//     and the queue from /v1/ops/queue (a context token with ops.queue.read).
//     Neither has the governor's counters, so a model reload cannot be seen
//     directly; the signal is a protected role leaving ok.
//   - metrics (--metrics-url): the operator's Prometheus text, which has the
//     roles, the queue and the governor's unload and reload counters.
//
// LocalAI's own LRU eviction at LOCALAI_MAX_ACTIVE_BACKENDS is invisible to the
// governor's counters (they count reloads after the governor's own unloads);
// what shows it is a role going not-ok, or the latency of the turn after it.

// maxProbeBody bounds what a probe reads.
const maxProbeBody = 4 << 20

// checkTargetURL validates a URL the harness will request: http or https, no
// credentials in it, and plain http only to a loopback host (a bearer token
// must not cross a network in clear). loopbackOnly demands loopback for https
// too (the operator's metrics door).
func checkTargetURL(raw string, loopbackOnly bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL", raw)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%q must be scheme://host[:port][/path] with no credentials, query or fragment", raw)
	}
	loop := isLoopbackHost(u.Hostname())
	switch u.Scheme {
	case "http":
		if !loop {
			return nil, fmt.Errorf("%q: plain http is allowed only to a loopback host", raw)
		}
	case "https":
		if loopbackOnly && !loop {
			return nil, fmt.Errorf("%q must be a loopback address", raw)
		}
	default:
		return nil, fmt.Errorf("%q: the scheme must be http or https", raw)
	}
	return u, nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// probeClient is the HTTP client of the probes: no proxy from the environment,
// no redirects (a redirect would carry the bearer token elsewhere).
func probeClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func httpGet(ctx context.Context, hc *http.Client, rawURL, bearer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxProbeBody {
		return nil, errors.New("the response is larger than expected")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", req.URL.Path, resp.StatusCode)
	}
	return body, nil
}

// ---------------------------------------------------------------- readyz

// readyzProbe reads the model roles from /readyz through the SDK.
type readyzProbe struct{ c *sdkgo.Client }

func (p readyzProbe) Kind() string { return "readyz" }

func (p readyzProbe) Stats(ctx context.Context) (StackStats, error) {
	rep, err := p.c.GetReadiness(ctx)
	if err != nil {
		return StackStats{}, err
	}
	roles := map[string]RoleState{}
	for _, r := range rep.Roles {
		roles[r.Role] = RoleState{Configured: r.Configured, Status: r.Status}
	}
	return StackStats{Roles: roles}, nil
}

// ---------------------------------------------------------------- ops queue

// opsQueueProbe reads the queue from /v1/ops/queue. token mints a fresh
// single-use context token with the ops.queue.read capability.
type opsQueueProbe struct {
	url   string
	hc    *http.Client
	token func(ctx context.Context) (string, error)
}

func newOpsQueueProbe(base string, token func(ctx context.Context) (string, error)) (*opsQueueProbe, error) {
	u, err := checkTargetURL(base, false)
	if err != nil {
		return nil, err
	}
	return &opsQueueProbe{url: strings.TrimRight(u.String(), "/") + "/v1/ops/queue", hc: probeClient(), token: token}, nil
}

func (p *opsQueueProbe) Kind() string { return "ops-queue" }

type queueReport struct {
	Classes []struct {
		Class   string `json:"class"`
		Pending int    `json:"pending"`
		Running int    `json:"running"`
	} `json:"classes"`
}

func (p *opsQueueProbe) Stats(ctx context.Context) (StackStats, error) {
	tok, err := p.token(ctx)
	if err != nil {
		return StackStats{}, err
	}
	body, err := httpGet(ctx, p.hc, p.url, tok)
	if err != nil {
		return StackStats{}, err
	}
	return parseQueueReport(body)
}

// parseQueueReport reads the QueueReport of /v1/ops/queue. An empty class list
// is an error: the report always has the six classes.
func parseQueueReport(body []byte) (StackStats, error) {
	var rep queueReport
	if err := json.Unmarshal(body, &rep); err != nil {
		return StackStats{}, fmt.Errorf("the queue report is not JSON: %w", err)
	}
	if len(rep.Classes) == 0 {
		return StackStats{}, errors.New("the queue report has no class")
	}
	st := StackStats{QueueKnown: true}
	for _, c := range rep.Classes {
		st.QueuePending += c.Pending
		st.QueueRunning += c.Running
		if c.Class == "ingest" {
			st.QueueIngestPending += c.Pending
		}
	}
	return st, nil
}

// readyzOpsProbe is the default probe: roles from readyz, queue from ops.
type readyzOpsProbe struct {
	roles readyzProbe
	queue *opsQueueProbe
}

func (p readyzOpsProbe) Kind() string { return "readyz+ops" }

func (p readyzOpsProbe) Stats(ctx context.Context) (StackStats, error) {
	st, err := p.roles.Stats(ctx)
	if err != nil {
		return StackStats{}, err
	}
	q, err := p.queue.Stats(ctx)
	if err != nil {
		return StackStats{}, err
	}
	st.QueueKnown, st.QueuePending, st.QueueRunning, st.QueueIngestPending = q.QueueKnown, q.QueuePending, q.QueueRunning, q.QueueIngestPending
	return st, nil
}

// ---------------------------------------------------------------- metrics

// metricsProbe reads the operator's Prometheus text (internal/diag).
type metricsProbe struct {
	url string
	hc  *http.Client
}

func newMetricsProbe(rawURL string) (*metricsProbe, error) {
	u, err := checkTargetURL(rawURL, true)
	if err != nil {
		return nil, err
	}
	return &metricsProbe{url: u.String(), hc: probeClient()}, nil
}

func (p *metricsProbe) Kind() string { return "metrics" }

func (p *metricsProbe) Stats(ctx context.Context) (StackStats, error) {
	body, err := httpGet(ctx, p.hc, p.url, "")
	if err != nil {
		return StackStats{}, err
	}
	return parseMetrics(string(body))
}

var labelPattern = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// metricLine is one sample line of the Prometheus text format.
type metricLine struct {
	name   string
	labels map[string]string
	value  float64
}

func parseMetricLine(line string) (metricLine, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return metricLine{}, false
	}
	ml := metricLine{labels: map[string]string{}}
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return metricLine{}, false
		}
		ml.name = line[:i]
		for _, m := range labelPattern.FindAllStringSubmatch(line[i+1:j], -1) {
			ml.labels[m[1]] = m[2]
		}
		rest = line[j+1:]
	} else {
		name, after, ok := strings.Cut(line, " ")
		if !ok {
			return metricLine{}, false
		}
		ml.name, rest = name, after
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return metricLine{}, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return metricLine{}, false
	}
	ml.value = v
	return ml, true
}

// parseMetrics reads what the harness needs from the operator's /metrics: the
// model roles, the queue and the governor's unload and reload counters.
// A group that is absent stays unknown rather than zero.
func parseMetrics(text string) (StackStats, error) {
	var st StackStats
	configured := map[string]bool{}
	status := map[string]string{}
	var haveProvider, haveQueue, haveReloads, haveUnloads bool
	for _, line := range strings.Split(text, "\n") {
		ml, ok := parseMetricLine(line)
		if !ok {
			continue
		}
		switch ml.name {
		case "alixai_provider_configured":
			haveProvider = true
			configured[ml.labels["role"]] = ml.value == 1
		case "alixai_provider_status":
			if ml.value == 1 {
				status[ml.labels["role"]] = ml.labels["status"]
			}
		case "alixai_queue_jobs":
			haveQueue = true
			switch ml.labels["state"] {
			case "pending":
				st.QueuePending += int(ml.value)
				if ml.labels["class"] == "ingest" {
					st.QueueIngestPending += int(ml.value)
				}
			case "running":
				st.QueueRunning += int(ml.value)
			}
		case "alixai_resource_model_reloads_total":
			haveReloads = true
			st.Reloads = uint64(ml.value)
		case "alixai_resource_model_unloads_total":
			haveUnloads = true
			st.Unloads = uint64(ml.value)
		}
	}
	if !haveProvider && !haveQueue && !haveReloads {
		return StackStats{}, errors.New("the metrics text has none of the alixai_provider, alixai_queue or alixai_resource families")
	}
	if haveProvider {
		st.Roles = map[string]RoleState{}
		for role, c := range configured {
			st.Roles[role] = RoleState{Configured: c, Status: statusOrUnavailable(status[role])}
		}
	}
	st.QueueKnown = haveQueue
	st.ReloadsKnown = haveReloads && haveUnloads
	return st, nil
}

// statusOrUnavailable: a role with no series at 1 is not ok (fail closed).
func statusOrUnavailable(s string) string {
	if s == "" {
		return "unavailable"
	}
	return s
}
