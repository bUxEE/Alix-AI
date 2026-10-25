package sdkgo_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
)

func itoa(n int) string { return strconv.Itoa(n) }

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// rawClient is a client of a server the test controls, with a token provider that counts its calls.
func rawClient(t *testing.T, url string, calls *atomic.Int32, mutate ...func(*sdkgo.ClientOptions)) *sdkgo.Client {
	t.Helper()
	o := sdkgo.ClientOptions{
		BaseURL: url,
		GetContextToken: func(context.Context, sdkgo.TokenRequest) (string, error) {
			if calls != nil {
				calls.Add(1)
			}
			return "tok", nil
		},
	}
	for _, m := range mutate {
		m(&o)
	}
	client, err := sdkgo.NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestTypedErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("an error document of the server is typed by its code", func(t *testing.T) {
		h := newHarness(t)
		_, err := h.client.GetSession(ctx, "ses-9")
		api := hasCode(t, "another session", err, contracts.ErrScopeViolation)
		if api.Status != http.StatusForbidden || api.Err.Retryable || api.RequestID == "" || api.Code() != contracts.ErrScopeViolation {
			t.Fatalf("status, retryable flag and request id of the document: %+v", api)
		}
		if !errors.Is(err, contracts.New(contracts.ErrScopeViolation, "")) || contracts.CodeOf(err) != contracts.ErrScopeViolation {
			t.Fatal("errors.Is and contracts.CodeOf see through the SDK error")
		}
		if errors.Is(err, contracts.New(contracts.ErrUnavailable, "")) {
			t.Fatal("another code does not match")
		}

		h.fake.FailNext("listCapabilities", http.StatusServiceUnavailable, contracts.ErrUnavailable, "the model runtime is down")
		_, err = h.client.ListCapabilities(ctx)
		api = hasCode(t, "unavailable", err, contracts.ErrUnavailable)
		if !sdkgo.IsRetryable(err) || api.Status != 503 || api.RetryAfter != time.Second {
			t.Fatalf("503 is retryable and carries Retry-After: %+v", api)
		}
		h.fake.FailNext("listCapabilities", http.StatusTooManyRequests, contracts.ErrRateLimited, "slow down")
		_, err = h.client.ListCapabilities(ctx)
		hasCode(t, "rate limited", err, contracts.ErrRateLimited)
		if !sdkgo.IsRetryable(err) {
			t.Fatal("rate_limited is retryable")
		}
		h.fake.SetApplicationStatus("quarantined")
		_, err = h.client.CreateSession(ctx)
		hasCode(t, "quarantined", err, contracts.ErrQuarantined)
		if sdkgo.IsRetryable(err) {
			t.Fatal("quarantined is not retryable")
		}
	})

	t.Run("a token is single use", func(t *testing.T) {
		h := newHarness(t)
		fixed := h.fake.IssueToken(h.claims)
		client := rawClient(t, h.fake.URL(), nil, func(o *sdkgo.ClientOptions) {
			o.GetContextToken = func(context.Context, sdkgo.TokenRequest) (string, error) { return fixed, nil }
		})
		if _, err := client.ListCapabilities(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := client.ListCapabilities(ctx)
		hasCode(t, "a second presentation", err, contracts.ErrReplayed)
		unknown := rawClient(t, h.fake.URL(), nil)
		_, err = unknown.ListCapabilities(ctx)
		hasCode(t, "an unknown token", err, contracts.ErrUnauthenticated)
	})

	t.Run("anything that is not an AlixAI error document falls back by status", func(t *testing.T) {
		cases := []struct {
			name        string
			status      int
			contentType string
			body        string
			code        contracts.ErrCode
			retryable   bool
		}{
			{"a proxy page", 502, "text/html", "<html>bad gateway</html>", contracts.ErrUnavailable, true},
			{"an unknown code is not trusted", 500, "application/json", `{"code":"made_up","message":"x","retryable":true}`, contracts.ErrInternal, false},
			{"a document without the retryable flag", 409, "application/json", `{"code":"stale","message":"x"}`, contracts.ErrConflict, false},
			{"an empty body", 403, "", "", contracts.ErrPermissionDenied, false},
			{"a gateway timeout page", 504, "text/plain", "timeout", contracts.ErrTimeout, true},
			{"a conforming document", 410, "application/json", `{"code":"expired","message":"gone","retryable":false,"details":{"reason":"x"}}`, contracts.ErrExpired, false},
			{"an unsupported media type", 415, "application/json", `[]`, contracts.ErrValidationFailed, false},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					if c.contentType != "" {
						w.Header().Set("Content-Type", c.contentType)
					}
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(c.body))
				})
				_, err := rawClient(t, srv.URL, nil).GetSession(ctx, "ses-1")
				api := hasCode(t, c.name, err, c.code)
				if api.Status != c.status || sdkgo.IsRetryable(err) != c.retryable {
					t.Fatalf("status %d retryable %v: %+v", api.Status, sdkgo.IsRetryable(err), api)
				}
			})
		}
	})

	t.Run("a success that is not what the document says is never a zero value", func(t *testing.T) {
		for name, body := range map[string]string{
			"an empty object":   `{}`,
			"an unknown status": `{"session_id":"s","status":"paused","turn_count":0,"created_at":"2026-10-02T00:00:00Z","updated_at":"2026-10-02T00:00:00Z","pending":null}`,
			"not JSON":          `<html>ok</html>`,
			"a bad timestamp":   `{"session_id":"s","status":"active","turn_count":0,"created_at":"yesterday","updated_at":"2026-10-02T00:00:00Z","pending":null}`,
			"null":              `null`,
		} {
			t.Run(name, func(t *testing.T) {
				srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				})
				session, err := rawClient(t, srv.URL, nil).GetSession(ctx, "ses-1")
				api := hasCode(t, name, err, contracts.ErrInternal)
				_ = api
				if session != nil || !errors.Is(err, contracts.New(contracts.ErrInternal, "")) {
					t.Fatalf("no result with an unexpected shape: %+v", session)
				}
				var ce *contracts.Error
				if !errors.As(err, &ce) || ce.Details["reason"] != "unexpected_shape" {
					t.Fatalf("reason unexpected_shape: %+v", err)
				}
			})
		}
	})

	t.Run("a transport failure: a read is unavailable, a write has an unknown outcome", func(t *testing.T) {
		srv := newTestServer(t, func(http.ResponseWriter, *http.Request) {})
		client := rawClient(t, srv.URL, nil)
		srv.Close()
		_, err := client.GetSession(ctx, "ses-1")
		hasCode(t, "a read on a dead server", err, contracts.ErrUnavailable)
		if !sdkgo.IsRetryable(err) {
			t.Fatal("a read that never reached the server can be repeated")
		}
		for name, call := range map[string]func() error{
			"create session": func() error { _, err := client.CreateSession(ctx); return err },
			"end session":    func() error { _, err := client.EndSession(ctx, "ses-1"); return err },
			"confirm": func() error {
				_, err := client.ConfirmAction(ctx, "prep_x", "cfm_"+strings.Repeat("A", 43))
				return err
			},
		} {
			err := call()
			hasCode(t, name, err, contracts.ErrUncertainOutcome)
			if sdkgo.IsRetryable(err) {
				t.Fatalf("%s: a write that may have been carried out is never retried blindly", name)
			}
		}
	})

	t.Run("a deadline: a read times out, a write has an unknown outcome", func(t *testing.T) {
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		})
		client := rawClient(t, srv.URL, nil, func(o *sdkgo.ClientOptions) { o.Timeout = 30 * time.Millisecond })
		_, err := client.GetSession(ctx, "ses-1")
		hasCode(t, "a slow read", err, contracts.ErrTimeout)
		if !sdkgo.IsRetryable(err) {
			t.Fatal("timeout is retryable")
		}
		_, err = client.CreateSession(ctx)
		hasCode(t, "a slow write", err, contracts.ErrUncertainOutcome)
		_, err = client.CreateSession(ctx, sdkgo.WithTimeout(20*time.Millisecond))
		hasCode(t, "a per-call deadline", err, contracts.ErrUncertainOutcome)
	})

	t.Run("the caller's context is reported as it is, and spends no token", func(t *testing.T) {
		var calls atomic.Int32
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { t.Error("a cancelled call must not be sent") })
		client := rawClient(t, srv.URL, &calls)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := client.GetSession(cancelled, "ses-1"); !errors.Is(err, context.Canceled) {
			t.Fatalf("context.Canceled, got %v", err)
		}
		if calls.Load() != 0 {
			t.Fatal("no token for a call that is already cancelled")
		}
		expired, stop := context.WithTimeout(ctx, time.Nanosecond)
		defer stop()
		time.Sleep(time.Millisecond)
		if _, err := client.CreateSession(expired); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the caller's own deadline is reported as it is, got %v", err)
		}
	})

	t.Run("a token provider that fails or misbehaves stops the call before it is sent", func(t *testing.T) {
		var hits atomic.Int32
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
		boom := errors.New("vault unreachable")
		failing := rawClient(t, srv.URL, nil, func(o *sdkgo.ClientOptions) {
			o.GetContextToken = func(context.Context, sdkgo.TokenRequest) (string, error) { return "", boom }
		})
		_, err := failing.GetSession(ctx, "ses-1")
		api := hasCode(t, "a failing provider", err, contracts.ErrUnauthenticated)
		_ = api
		if !errors.Is(err, boom) {
			t.Fatal("the provider's error is the cause")
		}
		for _, bad := range []string{"", "Bearer abc", "two words", "line\nbreak"} {
			client := rawClient(t, srv.URL, nil, func(o *sdkgo.ClientOptions) {
				o.GetContextToken = func(context.Context, sdkgo.TokenRequest) (string, error) { return bad, nil }
			})
			_, err := client.GetSession(ctx, "ses-1")
			hasCode(t, "the token "+strconv.Quote(bad), err, contracts.ErrUnauthenticated)
		}
		if hits.Load() != 0 {
			t.Fatalf("nothing was sent, got %d requests", hits.Load())
		}
	})

	t.Run("a redirect is an error, never followed with the credential", func(t *testing.T) {
		var elsewhere atomic.Int32
		target := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) })
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) })
		_, err := rawClient(t, srv.URL, nil).GetSession(ctx, "ses-1")
		if err == nil {
			t.Fatal("a redirect answer is not a session")
		}
		if elsewhere.Load() != 0 {
			t.Fatal("the redirect target was contacted")
		}
	})

	t.Run("a client is not built from options that would leak or never work", func(t *testing.T) {
		provider := func(context.Context, sdkgo.TokenRequest) (string, error) { return "tok", nil }
		for name, o := range map[string]sdkgo.ClientOptions{
			"no base URL":              {GetContextToken: provider},
			"no provider":              {BaseURL: "http://127.0.0.1:1"},
			"plain http off loopback":  {BaseURL: "http://alixai.example.com", GetContextToken: provider},
			"a name that looks local":  {BaseURL: "http://127.foo.example.com", GetContextToken: provider},
			"a name with digits":       {BaseURL: "http://127.0.0.example:8080", GetContextToken: provider},
			"credentials in the URL":   {BaseURL: "https://user:pw@alixai.example.com", GetContextToken: provider},
			"a query in the URL":       {BaseURL: "https://alixai.example.com/?a=b", GetContextToken: provider},
			"not an absolute URL":      {BaseURL: "alixai.example.com", GetContextToken: provider},
			"a scheme that is not web": {BaseURL: "ftp://alixai.example.com", GetContextToken: provider},
		} {
			if _, err := sdkgo.NewClient(o); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("%s: want validation_failed, got %v", name, err)
			}
		}
		for _, ok := range []sdkgo.ClientOptions{
			{BaseURL: "https://alixai.example.com/alixai/", GetContextToken: provider},
			{BaseURL: "http://localhost:8080", GetContextToken: provider},
			{BaseURL: "http://[::1]:8080", GetContextToken: provider},
			{BaseURL: "http://127.0.0.1:8080", GetContextToken: provider},
			{BaseURL: "http://127.1.2.3", GetContextToken: provider},
			{BaseURL: "http://alixai.internal", GetContextToken: provider, AllowInsecureHTTP: true},
		} {
			if _, err := sdkgo.NewClient(ok); err != nil {
				t.Errorf("%s: %v", ok.BaseURL, err)
			}
		}
	})

	t.Run("a path prefix of the base URL is kept", func(t *testing.T) {
		var path atomic.Value
		srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			path.Store(r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		if _, err := rawClient(t, srv.URL+"/alixai/", nil).GetLiveness(ctx); err != nil {
			t.Fatal(err)
		}
		if path.Load() != "/alixai/healthz" {
			t.Fatalf("got %v", path.Load())
		}
	})
}
