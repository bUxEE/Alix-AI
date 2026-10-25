package clm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/decision/internal/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// clmKeyFake builds an obviously fake 40-character service key at run time (the
// sidecar demands 32 or more); no real key is ever written in this package.
func clmKeyFake(tag string) string { return "fake-" + tag + "-" + strings.Repeat("0", 40-len(tag)-6) }

// clmKeyServer answers like the sidecar of deploy/container/clm: every route but
// /health demands "Authorization: Bearer <key>", else 401 with a body that echoes
// the received header (the worst case for a leak).
type clmKeyServer struct {
	*httptest.Server
	mu   sync.Mutex
	auth map[string][]string // path -> Authorization headers received
}

func (s *clmKeyServer) headers(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth[path]...)
}

func clmKeyNewServer(t *testing.T, want string) *clmKeyServer {
	t.Helper()
	s := &clmKeyServer{auth: map[string][]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got := r.Header.Get("Authorization")
		s.mu.Lock()
		s.auth[r.URL.Path] = append(s.auth[r.URL.Path], got)
		s.mu.Unlock()
		if r.URL.Path == "/health" {
			fmt.Fprint(w, healthOK)
			return
		}
		if got != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"detail":"bad key","received":%q}`, got)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, modelsOK)
		case "/v1/rank":
			fmt.Fprint(w, rankOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCLMAdapterSendsTheServiceKey(t *testing.T) {
	key := clmKeyFake("env")
	srv := clmKeyNewServer(t, key)

	t.Run("env reference", func(t *testing.T) {
		t.Setenv("CLM_TEST_SERVICE_KEY", key)
		cfg := testConfig(srv.URL)
		cfg.APIKeyRef = "env:CLM_TEST_SERVICE_KEY"
		p := newProvider(t, cfg)
		if err := p.Ready(context.Background()); err != nil {
			t.Fatalf("Ready with the key = %v, want 200s", err)
		}
		if res, err := score(t, p, testRequest(3)); err != nil || res.Disposition != "accepted" {
			t.Fatalf("score = %q, %v", res.Disposition, err)
		}
		for _, path := range []string{"/v1/models", "/v1/rank"} {
			hs := srv.headers(path)
			if len(hs) == 0 {
				t.Fatalf("%s was never called", path)
			}
			for _, h := range hs {
				if h != "Bearer "+key {
					t.Errorf("%s sent a wrong Authorization header", path)
				}
			}
		}
		for _, h := range srv.headers("/health") {
			if h != "" {
				t.Error("/health needs no key but one was sent")
			}
		}
	})

	t.Run("file reference", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "clm.key")
		if err := os.WriteFile(f, []byte(key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(srv.URL)
		cfg.APIKeyRef = "file:" + f
		if err := newProvider(t, cfg).Ready(context.Background()); err != nil {
			t.Fatalf("Ready with a file key = %v", err)
		}
	})

	t.Run("direct key option", func(t *testing.T) {
		if err := newProvider(t, testConfig(srv.URL), WithAPIKey(key)).Ready(context.Background()); err != nil {
			t.Fatalf("Ready with WithAPIKey = %v", err)
		}
	})

	t.Run("no key is the 401 of the sidecar, not a success", func(t *testing.T) {
		bare := clmKeyNewServer(t, key)
		res, err := score(t, newProvider(t, testConfig(bare.URL)), testRequest(3))
		if err == nil && res.Disposition == "accepted" {
			t.Fatal("a call without the key was accepted")
		}
		if h := bare.headers("/v1/models"); len(h) == 0 || h[0] != "" {
			t.Error("a provider with no key sent an Authorization header")
		}
	})

	t.Run("bad references fail at construction", func(t *testing.T) {
		for _, ref := range []string{"literal-key-value", "env:", "env:1BAD", "env:CLM_TEST_UNSET_KEY", "file:", "file:/nonexistent/clm.key"} {
			cfg := testConfig(srv.URL)
			cfg.APIKeyRef = ref
			if _, err := New(cfg); err == nil {
				t.Errorf("api_key_ref %q was accepted", ref)
			}
		}
	})
}

func TestCLMAdapterNeverPrintsTheKey(t *testing.T) {
	good := clmKeyFake("good")
	wrong := clmKeyFake("wrong")
	srv := clmKeyNewServer(t, good) // the server wants another key and echoes what it receives
	t.Setenv("CLM_TEST_SERVICE_KEY", wrong)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := testConfig(srv.URL)
	cfg.APIKeyRef = "env:CLM_TEST_SERVICE_KEY"
	p := newProvider(t, cfg, WithLogger(logger))

	var texts []string
	record := func(err error) {
		texts = append(texts, fmt.Sprintf("%v|%+v|%#v", err, err, err))
		var ce *contracts.Error
		if errors.As(err, &ce) {
			texts = append(texts, fmt.Sprintf("%v", ce.Details))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.Ready(ctx)
	if err == nil {
		t.Fatal("a wrong key was accepted")
	}
	record(err)
	res, err := score(t, p, testRequest(3))
	if err == nil && res.Disposition == "accepted" {
		t.Fatal("a wrong key was accepted by the scorer")
	}
	record(err)
	texts = append(texts, mustJSON(res), mustJSON(p.Manifest()), fmt.Sprintf("%v|%+v|%#v|%s", p, p, p, p), fmt.Sprintf("%v|%+v|%#v", cfg, cfg, cfg))

	// every way of printing a key, and a key reaching a record as an attribute
	k := wire.NewKey(wrong)
	texts = append(texts, fmt.Sprintf("%v|%+v|%#v|%s|%q", k, k, k, k, k), mustJSON(k))
	slog.New(slog.NewTextHandler(&logs, nil)).Info("key", "k", k)

	// references that fail: no message repeats the key or the reference
	t.Setenv("CLM_TEST_BAD_KEY", wrong+" with whitespace")
	for _, ref := range []string{"env:CLM_TEST_BAD_KEY", "file:" + wrong, wrong} {
		c := testConfig(srv.URL)
		c.APIKeyRef = ref
		_, err := New(c)
		if err == nil {
			t.Fatalf("api_key_ref %q was accepted", ref)
		}
		record(err)
		if strings.Contains(err.Error(), ref) {
			t.Errorf("a construction error repeats the reference")
		}
	}

	if !strings.Contains(logs.String(), "path=/v1/models") {
		t.Fatalf("the debug log has no request records: the check would pass by seeing nothing:\n%.500s", logs.String())
	}
	texts = append(texts, logs.String())
	for _, s := range texts {
		for _, secret := range []string{good, wrong} {
			if strings.Contains(s, secret) {
				t.Fatalf("a service key leaked: %.300s", strings.ReplaceAll(s, secret, "<KEY>"))
			}
		}
	}
}
