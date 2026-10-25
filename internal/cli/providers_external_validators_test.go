//go:build !noexternal

package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/external/meta"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// extGapKeyServer is a loopback provider that answers the key probe: it records the path and the key header, and
// answers with the scripted status and body.
type extGapKeyServer struct {
	*httptest.Server
	mu     sync.Mutex
	path   string
	header http.Header
	query  string
	hits   int
	status int
	body   string
}

func extGapNewKeyServer(t *testing.T, status int, body string) *extGapKeyServer {
	t.Helper()
	s := &extGapKeyServer{status: status, body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.path, s.header, s.query = r.URL.Path, r.Header.Clone(), r.URL.RawQuery
		status, body := s.status, s.body
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *extGapKeyServer) set(status int, body string) {
	s.mu.Lock()
	s.status, s.body = status, body
	s.mu.Unlock()
}

// A manual key is validated against its own provider, with the minimal authenticated request, the key in the
// provider's own header and never in the URL: an accepted key, a rejected key, a provider that cannot be reached
// and a provider with no validator are told apart; and a key submitted through the operator service is stored only
// after its provider accepted it.
func TestManualKeysAreValidatedPerProvider(t *testing.T) {
	const candidate = "fixture-candidate-key-0001" // TEST-ONLY value, not a key
	ctx := context.Background()

	anthropicSrv := extGapNewKeyServer(t, 200, `{"id":"claude-haiku-4-5","type":"model"}`)
	geminiSrv := extGapNewKeyServer(t, 200, `{"name":"models/gemini-2.5-flash-lite"}`)
	metaSrv := extGapNewKeyServer(t, 200, `{"data":[{"id":"llama-4-maverick"}]}`)
	sec := externalConfig{
		AnthropicBaseURL: anthropicSrv.URL, GeminiBaseURL: geminiSrv.URL, MetaBaseURL: metaSrv.URL, MetaDialect: string(meta.DialectOpenAICompat),
	}
	v := externalKeyValidator{base: "http://127.0.0.1:1", deployment: externalTestDeployment, sec: sec}

	type tc struct {
		provider  string
		srv       *extGapKeyServer
		path      string
		headerKey string
		headerVal string
		rejected  [2]any // status, body
		notFound  [2]any
	}
	for _, c := range []tc{
		{ExternalProviderAnthropic, anthropicSrv, "/v1/models/claude-haiku-4-5", "X-Api-Key", candidate,
			[2]any{401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`},
			[2]any{404, `{"type":"error","error":{"type":"not_found_error","message":"model"}}`}},
		{ExternalProviderGemini, geminiSrv, "/v1beta/models/gemini-2.5-flash-lite", "X-Goog-Api-Key", candidate,
			[2]any{400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"API key not valid.","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID"}]}}`},
			[2]any{404, `{"error":{"code":404,"status":"NOT_FOUND","message":"model"}}`}},
		{ExternalProviderMeta, metaSrv, "/v1/models", "Authorization", "Bearer " + candidate,
			[2]any{401, `{"error":{"message":"bad key"}}`}, [2]any{}},
	} {
		t.Run(c.provider, func(t *testing.T) {
			req := credentials.ValidationRequest{ProviderID: pv.ProviderID(c.provider), Key: candidate}
			res, err := v.ValidateKey(ctx, req)
			if err != nil || !res.Accepted {
				t.Fatalf("accepted key: %+v, %v", res, err)
			}
			c.srv.mu.Lock()
			path, got, query, hits := c.srv.path, c.srv.header.Get(c.headerKey), c.srv.query, c.srv.hits
			c.srv.mu.Unlock()
			if path != c.path || got != c.headerVal || hits != 1 {
				t.Errorf("request: path %q, %s=%q, hits=%d", path, c.headerKey, got, hits)
			}
			if strings.Contains(query, candidate) || strings.Contains(path, candidate) {
				t.Error("the key is in the URL")
			}

			c.srv.set(c.rejected[0].(int), c.rejected[1].(string))
			if res, err = v.ValidateKey(ctx, req); err != nil || res.Accepted {
				t.Errorf("rejected key: %+v, %v", res, err)
			}
			if c.notFound[0] != nil {
				// The key authenticated and only the probe's model is unknown to it: the key is accepted.
				c.srv.set(c.notFound[0].(int), c.notFound[1].(string))
				if res, err = v.ValidateKey(ctx, req); err != nil || !res.Accepted {
					t.Errorf("a model the key cannot see: %+v, %v", res, err)
				}
			}
			c.srv.set(503, `{}`)
			if res, err = v.ValidateKey(ctx, req); err == nil || res.Accepted {
				t.Errorf("provider down: %+v, %v", res, err)
			}
			c.srv.Close()
			if res, err = v.ValidateKey(ctx, req); err == nil || res.Accepted {
				t.Errorf("unreachable provider: %+v, %v", res, err)
			}
		})
	}
	if _, err := v.ValidateKey(ctx, credentials.ValidationRequest{ProviderID: "other", Key: candidate}); err == nil {
		t.Error("an unknown provider was validated")
	}

	// Through the operator service: the key is stored when its provider accepts it and refused, not stored, when it does not.
	t.Run("operator service", func(t *testing.T) {
		srv := extGapNewKeyServer(t, 200, `{"id":"claude-haiku-4-5","type":"model"}`)
		db := storetest.Open(t, nil)
		log, err := audit.New(db, audit.Options{})
		if err != nil {
			t.Fatal(err)
		}
		a, err := newExternalAdmin(externalAdminDeps{
			DB: db, Audit: log, DeploymentID: externalTestDeployment, Dir: filepath.Join(t.TempDir(), "external"),
			Section: externalConfig{Enabled: true, DeploymentOperators: []string{"app_demo/usr_ops"}, AnthropicBaseURL: srv.URL},
		})
		if err != nil {
			t.Fatal(err)
		}
		sc := externalTestScope("ten_acme", "usr_admin")
		draft, err := extWireDraft(ExternalProviderAnthropic, "", "app_demo", nil)
		if err != nil {
			t.Fatal(err)
		}
		day := time.Now().UTC().Format("2006-01-02")
		draft.LastPolicyReview, draft.Terms.RecordedOn = day, day
		if _, err := a.reg.Register(ctx, externalTestScope("ten_acme", "usr_ops"), draft); err != nil {
			t.Fatal(err)
		}
		var in gateway.ProvidersCredentialInput
		if err := json.Unmarshal([]byte(`{"key":"`+candidate+`"}`), &in); err != nil {
			t.Fatal(err)
		}
		srv.set(401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		if _, err := a.SubmitCredential(ctx, sc, ExternalProviderAnthropic, in); err == nil {
			t.Fatal("a key the provider rejected was stored")
		}
		if refs, _ := a.creds.List(ctx, sc, ExternalProviderAnthropic); len(refs) != 0 {
			t.Fatalf("%d references were stored for a rejected key", len(refs))
		}
		srv.set(200, `{"id":"claude-haiku-4-5","type":"model"}`)
		st, err := a.SubmitCredential(ctx, sc, ExternalProviderAnthropic, in)
		if err != nil || st.State != pv.ConnectionStatusStateConnected {
			t.Fatalf("an accepted key: %+v, %v", st, err)
		}
		if code := contracts.CodeOf(err); code != "" {
			t.Fatalf("code %q", code)
		}
	})
}

// The key probe goes through a registry with the platform deny-list, like every other: a provider base URL that is
// one of the process's own listeners (the gateway, the admin interface, LocalAI) is refused before it is contacted,
// so a key submission cannot be turned into a request to the core itself.
func TestKeyProbeRefusesThePlatformsOwnListeners(t *testing.T) {
	const candidate = "fixture-candidate-key-0002" // TEST-ONLY value, not a key
	srv := extGapNewKeyServer(t, 200, `{"id":"claude-haiku-4-5","type":"model"}`)
	v := externalKeyValidator{base: "http://127.0.0.1:1", deployment: externalTestDeployment, sec: externalConfig{AnthropicBaseURL: srv.URL}}
	req := credentials.ValidationRequest{ProviderID: pv.ProviderID(ExternalProviderAnthropic), Key: candidate}
	if res, err := v.ValidateKey(context.Background(), req); err != nil || !res.Accepted {
		t.Fatalf("positive control: %+v, %v", res, err)
	}
	addr := strings.TrimPrefix(srv.URL, "http://")
	platformDenyAddListener(addr)
	t.Cleanup(func() { platformDenyRemoveListener(addr) })
	srv.mu.Lock()
	before := srv.hits
	srv.mu.Unlock()
	// Refused before it left, the key was not judged: an error that stores nothing, not a rejected key.
	if res, err := v.ValidateKey(context.Background(), req); err == nil || res.Accepted {
		t.Errorf("a probe to a platform listener = %+v, %v", res, err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.hits != before {
		t.Errorf("the platform listener was contacted %d times", srv.hits-before)
	}
}
