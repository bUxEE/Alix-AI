package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fixtureFS holds the recorded wire fixtures: whole response and error bodies of the four
// adapters (copies of adapters/external/{anthropic,gemini,meta}/testdata and of
// tools/providerconf/fixtures/openai; TestFixturesMatchTheirSources keeps the copies honest),
// as templates with {{PLACEHOLDER}} members. They were written from each vendor's documented
// shapes; none is the recording of a call to a live service and none holds a key.
//
//go:embed fixtures/*/*.json
var fixtureFS embed.FS

func fixture(provider, name string) (string, error) {
	b, err := fixtureFS.ReadFile("fixtures/" + provider + "/" + name)
	if err != nil {
		return "", fmt.Errorf("missing fixture %s/%s", provider, name)
	}
	return string(b), nil
}

// upstreamMarker is planted in every error fixture body; no error the adapter returns may show it.
const upstreamMarker = "UPSTREAM-BODY-MARKER-6f1c"

// kind is the scenario the stub plays for the next request.
type kind string

const (
	kText       kind = "text"
	kToolCall   kind = "tool_call"
	kHostedTool kind = "hosted_tool"
	kActive     kind = "active_media"
	kMalformed  kind = "malformed"
	kServer     kind = "server_error"
	kAuth       kind = "auth_error"
	kQuota      kind = "quota"
	kRateLimit  kind = "rate_limit"
	kTruncated  kind = "truncated"
)

// scenario is one scripted answer.
type scenario struct {
	Kind       kind
	Text       string
	CallID     string
	CallName   string
	CallArgs   string
	ToksIn     int
	ToksOut    int
	RetryAfter time.Duration
	Times      int
}

// seenRequest is one request as the stub received it.
type seenRequest struct {
	Path   string
	Bytes  int
	Body   string
	HasKey bool
}

// latencyModel is the injected network latency. It is an INPUT of the emulation, never a
// result: the stub waits Base plus a deterministic jitter in [0, Jitter) before it writes a
// reply, so the latency a caller measures is this wait plus the adapter's own work.
type latencyModel struct {
	Base   time.Duration
	Jitter time.Duration
	Seed   int64
}

// wire describes how one provider's API looks to the stub.
type wire struct {
	provider string
	// match says whether a request is the generate call of this API.
	match func(r *http.Request) bool
	// vars builds the template members of a scenario.
	vars func(sc scenario, n int) map[string]string
	// authStatus is the HTTP status of an invalid-key answer (Gemini answers 400).
	authStatus int
	// rateHeaders adds the provider's own rate-limit headers.
	rateHeaders func(h http.Header, retry time.Duration)
}

func jstr(s string) string { b, _ := json.Marshal(s); return string(b) }

func wireFor(provider string) (wire, error) {
	switch provider {
	case "openai":
		return wire{
			provider:   provider,
			match:      func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/responses" },
			authStatus: http.StatusUnauthorized,
			vars: func(sc scenario, _ int) map[string]string {
				return map[string]string{"CALL_ID": jstr(sc.CallID), "NAME": jstr(sc.CallName), "ARGS": jstr(orEmptyObject(sc.CallArgs))}
			},
		}, nil
	case "anthropic":
		return wire{
			provider:   provider,
			match:      func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/messages" },
			authStatus: http.StatusUnauthorized,
			vars: func(sc scenario, _ int) map[string]string {
				return map[string]string{"CALL_ID": jstr(sc.CallID), "NAME": jstr(sc.CallName), "ARGS_RAW": orEmptyObject(sc.CallArgs)}
			},
			rateHeaders: func(h http.Header, d time.Duration) {
				h.Set("Anthropic-Ratelimit-Requests-Remaining", "0")
				h.Set("Anthropic-Ratelimit-Requests-Reset", time.Now().Add(d).UTC().Format(time.RFC3339))
			},
		}, nil
	case "gemini":
		return wire{
			provider: provider,
			match: func(r *http.Request) bool {
				return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1beta/models/") &&
					strings.HasSuffix(r.URL.Path, ":generateContent") && r.URL.RawQuery == ""
			},
			authStatus: http.StatusBadRequest, // an invalid key is a 400 with reason API_KEY_INVALID
			vars: func(sc scenario, n int) map[string]string {
				delay := ""
				if sc.RetryAfter > 0 {
					delay = strconv.FormatFloat(sc.RetryAfter.Seconds(), 'f', -1, 64) + "s"
				}
				return map[string]string{
					"CALL_ID": jstr(sc.CallID), "NAME": jstr(sc.CallName), "ARGS_RAW": orEmptyObject(sc.CallArgs),
					"TOTAL": strconv.Itoa(sc.ToksIn + sc.ToksOut), "RID": "FixtureResp" + strconv.Itoa(n), "RETRY_DELAY": delay, "FINISH": "STOP",
				}
			},
		}, nil
	case "meta":
		return wire{
			provider:   provider,
			match:      func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" },
			authStatus: http.StatusUnauthorized,
			vars: func(sc scenario, _ int) map[string]string {
				return map[string]string{"CALL_ID": jstr(sc.CallID), "NAME": jstr(sc.CallName), "ARGS_STR": jstr(orEmptyObject(sc.CallArgs))}
			},
			rateHeaders: func(h http.Header, d time.Duration) {
				h.Set("X-Ratelimit-Remaining-Requests", "0")
				h.Set("X-Ratelimit-Reset-Requests", d.String())
			},
		}, nil
	}
	return wire{}, fmt.Errorf("unknown provider %q", provider)
}

func orEmptyObject(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// stub is a loopback HTTP server that plays one provider's API from the recorded fixtures,
// one scripted scenario per request, after an injected network delay. It listens on 127.0.0.1
// only and never dials out.
type stub struct {
	w   wire
	lat latencyModel
	srv *http.Server
	URL string

	mu     sync.Mutex
	rng    *rand.Rand
	queue  []scenario
	seen   []seenRequest
	served int
	// repeat, when set, answers every request with the same scenario (the concurrent phase of the
	// footprint run, where the order of requests is not known). dropBodies keeps no request body.
	repeat     *scenario
	dropBodies bool
	// lastDelay is the delay injected into the most recent request (meaningful when requests
	// arrive one at a time).
	lastDelay time.Duration
}

func newStub(provider string, lat latencyModel) (*stub, error) {
	w, err := wireFor(provider)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &stub{w: w, lat: lat, URL: "http://" + ln.Addr().String(), rng: rand.New(rand.NewSource(lat.Seed))}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Close stops the server.
func (s *stub) Close() { _ = s.srv.Close() }

// script replaces the queue.
func (s *stub) script(sc ...scenario) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = nil
	for _, x := range sc {
		n := x.Times
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			s.queue = append(s.queue, x)
		}
	}
	s.seen = nil
}

// lastInjected returns the delay injected into the most recent request.
func (s *stub) lastInjected() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDelay
}

// setRepeat answers every later request with sc (nil restores the queue).
func (s *stub) setRepeat(sc *scenario) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repeat = sc
}

// requests returns the requests seen since the last script call.
func (s *stub) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

func (s *stub) delay() time.Duration {
	if s.lat.Base <= 0 && s.lat.Jitter <= 0 {
		return 0
	}
	d := s.lat.Base
	if s.lat.Jitter > 0 {
		s.mu.Lock()
		d += time.Duration(s.rng.Int63n(int64(s.lat.Jitter)))
		s.mu.Unlock()
	}
	return d
}

func writeBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *stub) drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func (s *stub) handle(w http.ResponseWriter, r *http.Request) {
	if !s.w.match(r) {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	hasKey := r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != ""

	s.mu.Lock()
	s.served++
	n := s.served
	rec := seenRequest{Path: r.URL.Path, Bytes: len(body), HasKey: hasKey}
	if !s.dropBodies {
		rec.Body = string(body)
		s.seen = append(s.seen, rec)
	}
	var sc scenario
	have := len(s.queue) > 0
	if have {
		sc, s.queue = s.queue[0], s.queue[1:]
	}
	if s.repeat != nil {
		sc, have = *s.repeat, true
	}
	s.mu.Unlock()

	d := s.delay()
	s.mu.Lock()
	s.lastDelay = d
	s.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	render := func(name string, extra map[string]string) (string, bool) {
		tpl, err := fixture(s.w.provider, name)
		if err != nil {
			return "", false
		}
		in, out := sc.ToksIn, sc.ToksOut
		if in == 0 && out == 0 {
			in, out = 50, 10
		}
		kv := map[string]string{
			"IN": strconv.Itoa(in), "OUT": strconv.Itoa(out), "TEXT": jstr(sc.Text), "MARKER": upstreamMarker, "KEY": "",
		}
		for k, v := range s.w.vars(withTokens(sc, in, out), n) {
			kv[k] = v
		}
		for k, v := range extra {
			kv[k] = v
		}
		pairs := make([]string, 0, len(kv)*2)
		for k, v := range kv {
			pairs = append(pairs, "{{"+k+"}}", v)
		}
		return strings.NewReplacer(pairs...).Replace(tpl), true
	}
	reply := func(status int, name string) {
		b, ok := render(name, nil)
		if !ok {
			writeBody(w, http.StatusInternalServerError, `{"error":"missing fixture"}`)
			return
		}
		writeBody(w, status, b)
	}
	if !have {
		reply(http.StatusInternalServerError, "error_no_script.json")
		return
	}
	switch sc.Kind {
	case kText:
		reply(http.StatusOK, "text.json")
	case kToolCall:
		reply(http.StatusOK, "tool_call.json")
	case kHostedTool:
		reply(http.StatusOK, "hosted_tool.json")
	case kActive:
		reply(http.StatusOK, "active_media.json")
	case kMalformed:
		reply(http.StatusOK, "malformed.json")
	case kServer:
		reply(http.StatusInternalServerError, "error_server.json")
	case kAuth:
		reply(s.w.authStatus, "error_auth.json")
	case kQuota:
		reply(http.StatusTooManyRequests, "error_quota.json")
	case kRateLimit:
		w.Header().Set("Retry-After", strconv.Itoa(int(sc.RetryAfter/time.Second)))
		if s.w.rateHeaders != nil {
			s.w.rateHeaders(w.Header(), sc.RetryAfter)
		}
		reply(http.StatusTooManyRequests, "error_rate_limit.json")
	case kTruncated:
		s.drop(w)
	default:
		reply(http.StatusInternalServerError, "error_no_script.json")
	}
}

func withTokens(sc scenario, in, out int) scenario {
	sc.ToksIn, sc.ToksOut = in, out
	return sc
}
