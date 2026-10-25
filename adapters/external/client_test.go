package external

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// extTestKey is a TEST-ONLY value shaped like an API key. It is not a credential.
const extTestKey = "sk-TEST-ONLY-0123456789abcdef"

// extTestRef is a TEST-ONLY credential reference id.
const extTestRef = "cred_TEST-ONLY"

type extFakeKeys struct {
	key credentials.Secret
	err error
}

func (f extFakeKeys) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return f.key, f.err
}

// extTestServer is a plain-HTTP loopback mock that counts the requests it sees.
func extTestServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func extTestGuarded(t *testing.T, base string, routes ...ExtRoute) *http.Client {
	t.Helper()
	origin, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		CheckRedirect: ExtRefuseRedirects,
		Transport: &ExtRouteGuard{
			Next: &http.Transport{Proxy: nil}, Origin: origin, Routes: routes, Err: ExtForbiddenRoute("acme"),
		},
	}
}

func extDetail(err error, k string) any {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return ce.Details[k]
	}
	return nil
}

func extNoSecret(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, extTestKey) || strings.Contains(s, "TEST-ONLY") {
		t.Errorf("%s leaks a secret: %q", what, s)
	}
}

// extTestCaller builds a caller whose credential goes in the Authorization header.
func extTestCaller(hc *http.Client, base string, keys ExtKeySource, log *slog.Logger, sleeps *[]time.Duration) ExtCaller {
	return ExtCaller{
		Provider: "acme", BaseURL: base, HTTP: hc, Clock: clock.Real{}, Log: log,
		Retry:  RetryPolicy{MaxRetries: 2, BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxRetryAfter: 10 * time.Second},
		Jitter: func() float64 { return 0 },
		Sleep: func(_ context.Context, d time.Duration) error {
			*sleeps = append(*sleeps, d)
			return nil
		},
		Headers: func(ctx context.Context, scope contracts.Scope, r ExtRoute, _ bool) (http.Header, error) {
			h := http.Header{}
			if r.Auth {
				k, err := ExtAPIKey(ctx, "acme", keys, scope, extTestRef)
				if err != nil {
					return nil, err
				}
				h.Set("Authorization", "Bearer "+k)
			}
			return h, nil
		},
		MapHTTP: func(status int, h http.Header, _ []byte, _ *providers.RateLimitInfo, now time.Time) (error, bool) {
			// The body is read for classification only; the message is fixed.
			if status == http.StatusTooManyRequests {
				rl := providers.RateLimitInfo{Scope: "requests"}
				if ra, ok := providers.ParseRetryAfter(h.Get("Retry-After"), now); ok {
					rl.RetryAfter = ra
				}
				return ExtRateLimited(rl, pv.ProviderErrorKindRateLimited), true
			}
			if status == http.StatusUnauthorized {
				return ExtAPIError("acme", contracts.ErrPermissionDenied, pv.ProviderErrorKindAuthenticationFailed, true, "the provider rejected the API key"), false
			}
			return ExtAPIError("acme", contracts.ErrUnavailable, pv.ProviderErrorKindUnavailable, true, "the provider is unavailable"), false
		},
	}
}

// TestSharedClientRedactsKeysAndCapsResponses: a key never reaches an error, a
// detail or a log record whatever the provider echoes back or the transport
// reports, and a response over the cap is a malformed response, never a
// truncated success.
func TestSharedClientRedactsKeysAndCapsResponses(t *testing.T) {
	post := ExtRoute{Method: http.MethodPost, Path: "/v1/chat", Auth: true}
	srv, _ := extTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// An error body that echoes the Authorization header and the prompt.
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":{"message":"bad key %s","echo":%q}}`, extTestKey, r.Header.Get("Authorization"))
	})
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var sleeps []time.Duration
	c := extTestCaller(extTestGuarded(t, srv.URL, post), srv.URL, extFakeKeys{key: extTestKey}, log, &sleeps)

	_, err := c.Do(context.Background(), contracts.Scope{}, post, map[string]string{"q": "hello"}, "application/json")
	if err == nil || contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Fatalf("got %v", err)
	}
	extNoSecret(t, "the mapped error", err.Error())
	extNoSecret(t, "the error details", fmt.Sprint(err.(*contracts.Error).Details))
	if extDetail(err, "sent") != true || extDetail(err, "provider_error_kind") != string(pv.ProviderErrorKindAuthenticationFailed) {
		t.Errorf("details %v", err.(*contracts.Error).Details)
	}
	if !strings.Contains(logBuf.String(), "acme request") || !strings.Contains(logBuf.String(), "status=401") {
		t.Errorf("no debug record: %q", logBuf.String())
	}
	extNoSecret(t, "the log", logBuf.String())

	// A transport error that carries a URL with a key in its query: the URL is stripped.
	leak := &url.Error{Op: "Post", URL: "http://x/v1/chat?key=" + extTestKey, Err: io.ErrUnexpectedEOF}
	mapped := ExtMapTransportError(context.Background(), "acme", leak)
	extNoSecret(t, "the transport error", mapped.Error())
	extNoSecret(t, "the transport error details", fmt.Sprint(mapped.(*contracts.Error).Details))
	if extDetail(mapped, "provider_error_kind") != string(pv.ProviderErrorKindConnectionLost) || extDetail(mapped, "sent") != true {
		t.Errorf("details %v", mapped.(*contracts.Error).Details)
	}

	// A credential source that fails: nothing is sent, the reason survives, no value leaks.
	cause := contracts.New(contracts.ErrPermissionDenied, "credential revoked")
	srv2, hits2 := extTestServer(t, func(http.ResponseWriter, *http.Request) {})
	c2 := extTestCaller(extTestGuarded(t, srv2.URL, post), srv2.URL, extFakeKeys{err: cause}, nil, &sleeps)
	_, err = c2.Do(context.Background(), contracts.Scope{}, post, nil, "application/json")
	if contracts.CodeOf(err) != contracts.ErrPermissionDenied || hits2.Load() != 0 || extDetail(err, "sent") != false {
		t.Fatalf("got %v after %d hits", err, hits2.Load())
	}
	if !errors.Is(err, cause) {
		t.Error("the reason of the credential failure was lost")
	}

	// A key that cannot travel in a header is refused before anything is sent.
	for _, bad := range []credentials.Secret{"", "has space", "line\nbreak", credentials.Secret(strings.Repeat("k", 513))} {
		_, err := ExtAPIKey(context.Background(), "acme", extFakeKeys{key: bad}, contracts.Scope{}, extTestRef)
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied || extDetail(err, "sent") != false {
			t.Errorf("key %q: %v", bad, err)
		}
		if bad != "" {
			extNoSecret(t, "the unusable-key error", err.Error())
			if strings.Contains(err.Error(), string(bad)) {
				t.Errorf("the error repeats the key: %q", err)
			}
		}
	}

	// The response cap: exactly the limit is read, one byte more is malformed.
	const limit = 1024
	got, err := ExtReadBody(context.Background(), "acme", strings.NewReader(strings.Repeat("x", limit)), limit)
	if err != nil || len(got) != limit {
		t.Fatalf("at the limit: %d bytes, %v", len(got), err)
	}
	_, err = ExtReadBody(context.Background(), "acme", strings.NewReader(strings.Repeat("x", limit+1)), limit)
	if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || extDetail(err, "provider_error_kind") != string(pv.ProviderErrorKindMalformedResponse) {
		t.Fatalf("over the limit: %v", err)
	}
	if !strings.Contains(err.Error(), "acme: ") {
		t.Errorf("message %q", err)
	}
}

// TestSharedClientRouteGuardRefusesUnlistedRoutes: a request that is not exactly
// a listed route, or that goes to another origin, is refused before any I/O.
func TestSharedClientRouteGuardRefusesUnlistedRoutes(t *testing.T) {
	post := ExtRoute{Method: http.MethodPost, Path: "/v1/chat", Auth: true}
	stream := ExtRoute{Method: http.MethodPost, Path: "/v1/stream", Query: "alt=sse", Auth: true}
	list := ExtRoute{Method: http.MethodGet, Path: "/v1/models"}
	srv, hits := extTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	other, otherHits := extTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	hc := extTestGuarded(t, srv.URL, post, stream, list)

	send := func(method, target string) error {
		req, err := http.NewRequest(method, target, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			// What the adapters do with every transport failure: net/http wraps it
			// in a *url.Error that prints the full URL, and the mapping strips it.
			return ExtMapTransportError(context.Background(), "acme", err)
		}
		_ = resp.Body.Close()
		return nil
	}

	for _, ok := range []struct{ method, path string }{
		{http.MethodPost, "/v1/chat"}, {http.MethodPost, "/v1/stream?alt=sse"}, {http.MethodGet, "/v1/models"},
	} {
		if err := send(ok.method, srv.URL+ok.path); err != nil {
			t.Errorf("%s %s refused: %v", ok.method, ok.path, err)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("%d listed requests reached the server, want 3", hits.Load())
	}

	refused := []struct{ name, method, target string }{
		{"unlisted path", http.MethodPost, srv.URL + "/v1/files"},
		{"wrong method", http.MethodGet, srv.URL + "/v1/chat"},
		{"sub-path", http.MethodPost, srv.URL + "/v1/chat/extra"},
		{"query on a route without one", http.MethodPost, srv.URL + "/v1/chat?key=" + extTestKey},
		{"missing constant query", http.MethodPost, srv.URL + "/v1/stream"},
		{"another query on the stream route", http.MethodPost, srv.URL + "/v1/stream?alt=json"},
		{"extra query on the stream route", http.MethodPost, srv.URL + "/v1/stream?alt=sse&key=x"},
		{"empty query", http.MethodPost, srv.URL + "/v1/chat?"},
		{"fragment", http.MethodPost, srv.URL + "/v1/chat#frag"},
		{"escaped path", http.MethodPost, srv.URL + "/v1/%63hat"},
		{"userinfo", http.MethodPost, strings.Replace(srv.URL, "http://", "http://user:pw@", 1) + "/v1/chat"},
		{"another origin", http.MethodPost, other.URL + "/v1/chat"},
	}
	for _, r := range refused {
		err := send(r.method, r.target)
		if err == nil {
			t.Errorf("%s: not refused", r.name)
			continue
		}
		if contracts.CodeOf(err) != contracts.ErrPermissionDenied || extDetail(err, "sent") != false || extDetail(err, "component") != "acme" {
			t.Errorf("%s: %v (%v)", r.name, err, extDetail(err, "sent"))
		}
		if strings.Contains(err.Error(), "pw@") || strings.Contains(err.Error(), extTestKey) {
			t.Errorf("%s: the error repeats the URL: %v", r.name, err)
		}
	}
	if hits.Load() != 3 || otherHits.Load() != 0 {
		t.Errorf("a refused request reached a server: %d / %d", hits.Load(), otherHits.Load())
	}

	// A redirect is returned to the caller, never followed.
	redir, redirHits := extTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/chat", http.StatusFound)
	})
	hc = extTestGuarded(t, redir.URL, post)
	req, _ := http.NewRequest(http.MethodPost, redir.URL+"/v1/chat", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || redirHits.Load() != 1 || otherHits.Load() != 0 {
		t.Errorf("redirect: status %d, hits %d / %d", resp.StatusCode, redirHits.Load(), otherHits.Load())
	}
}

// TestSharedClientRetriesOnlyRefusalsWithinThePolicy: a refusal made before
// processing is retried within the bound and the provider's wait is honoured; a
// refreshable status is repeated once, with a forced refresh and no retry slot.
func TestSharedClientRetriesOnlyRefusalsWithinThePolicy(t *testing.T) {
	post := ExtRoute{Method: http.MethodPost, Path: "/v1/chat", Auth: true}

	srv, hits := extTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "6")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var sleeps []time.Duration
	c := extTestCaller(extTestGuarded(t, srv.URL, post), srv.URL, extFakeKeys{key: extTestKey}, nil, &sleeps)
	_, err := c.Do(context.Background(), contracts.Scope{}, post, nil, "application/json")
	if contracts.CodeOf(err) != contracts.ErrRateLimited || hits.Load() != 3 || len(sleeps) != 2 {
		t.Fatalf("got %v after %d requests and %v", err, hits.Load(), sleeps)
	}
	for _, d := range sleeps {
		if d < 6*time.Second {
			t.Errorf("waited %v, the provider asked for 6s", d)
		}
	}

	// A wait longer than the policy allows is not waited for.
	srv2, hits2 := extTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	sleeps = nil
	c = extTestCaller(extTestGuarded(t, srv2.URL, post), srv2.URL, extFakeKeys{key: extTestKey}, nil, &sleeps)
	if _, err = c.Do(context.Background(), contracts.Scope{}, post, nil, "application/json"); contracts.CodeOf(err) != contracts.ErrRateLimited || hits2.Load() != 1 || len(sleeps) != 0 {
		t.Fatalf("got %v after %d requests and %v", err, hits2.Load(), sleeps)
	}

	// A status that is not a refusal-before-processing is never retried.
	srv3, hits3 := extTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	sleeps = nil
	c = extTestCaller(extTestGuarded(t, srv3.URL, post), srv3.URL, extFakeKeys{key: extTestKey}, nil, &sleeps)
	if _, err = c.Do(context.Background(), contracts.Scope{}, post, nil, "application/json"); contracts.CodeOf(err) != contracts.ErrUnavailable || hits3.Load() != 1 {
		t.Fatalf("got %v after %d requests", err, hits3.Load())
	}

	// RefreshOn: the 401 is repeated once with forceRefresh, then the answer stands.
	srv4, hits4 := extTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer fresh" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	var forced []bool
	c = extTestCaller(extTestGuarded(t, srv4.URL, post), srv4.URL, extFakeKeys{}, nil, &sleeps)
	c.RefreshOn = func(status int) bool { return status == http.StatusUnauthorized }
	c.Headers = func(_ context.Context, _ contracts.Scope, _ ExtRoute, force bool) (http.Header, error) {
		forced = append(forced, force)
		h := http.Header{}
		if force {
			h.Set("Authorization", "Bearer fresh")
		} else {
			h.Set("Authorization", "Bearer stale")
		}
		return h, nil
	}
	rep, err := c.Do(context.Background(), contracts.Scope{}, post, nil, "application/json")
	if err != nil || rep.Resp.StatusCode != http.StatusOK || hits4.Load() != 2 || len(forced) != 2 || forced[0] || !forced[1] {
		t.Fatalf("got %v after %d requests, forced %v", err, hits4.Load(), forced)
	}
	_ = rep.Resp.Body.Close()
	// Still 401 after the refresh: not retried a second time.
	srv5, hits5 := extTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	c.BaseURL, c.HTTP = srv5.URL, extTestGuarded(t, srv5.URL, post)
	if _, err = c.Do(context.Background(), contracts.Scope{}, post, nil, "application/json"); contracts.CodeOf(err) != contracts.ErrPermissionDenied || hits5.Load() != 2 {
		t.Fatalf("got %v after %d requests", err, hits5.Load())
	}
}

// TestSharedClientRetryWaitCutShortKeepsTheRefusal: when the deadline or a
// cancellation cuts the wait before a retry, the answer is the refusal the
// provider made, never a timeout or a lost connection after a send (which the
// router would settle as an uncertain, billed call).
func TestSharedClientRetryWaitCutShortKeepsTheRefusal(t *testing.T) {
	post := ExtRoute{Method: http.MethodPost, Path: "/v1/chat", Auth: true}
	srv, hits := extTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	for _, cut := range []error{context.DeadlineExceeded, context.Canceled} {
		hits.Store(0)
		var sleeps []time.Duration
		c := extTestCaller(extTestGuarded(t, srv.URL, post), srv.URL, extFakeKeys{key: extTestKey}, nil, &sleeps)
		c.Sleep = func(context.Context, time.Duration) error { return cut }
		_, err := c.Do(context.Background(), contracts.Scope{}, post, nil, "application/json")
		if contracts.CodeOf(err) != contracts.ErrRateLimited || extDetail(err, "provider_error_kind") != string(pv.ProviderErrorKindRateLimited) || hits.Load() != 1 {
			t.Errorf("%v: got %v (%v) after %d requests, want the rate-limit refusal", cut, err, extDetail(err, "provider_error_kind"), hits.Load())
		}
	}
}
