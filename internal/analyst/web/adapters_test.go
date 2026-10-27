package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type staticSecret string

func (s staticSecret) Resolve(context.Context, string) (string, error) { return string(s), nil }

func fixtureClient(t *testing.T, status int, body []byte) *http.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := r.Header.Get("X-Subscription-Token"); tok != "" && tok != "test-secret" {
			t.Errorf("unexpected brave token")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.Client()
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBraveAdapterFixture(t *testing.T) {
	body := readFixture(t, "brave_search.json")
	hits, err := parseBrave(body)
	if err != nil || len(hits) != 2 {
		t.Fatalf("parseBrave: %v %#v", err, hits)
	}
	b := &Brave{Secrets: staticSecret("test-secret"), CredRef: "cred_01J0000000000000000000000A",
		Client: fixtureClient(t, 200, body)}
	// Override URL by injecting transport that answers regardless of host — use parse path already covered;
	// exercise Search against the fixture server by pointing Client at a rewrite round-tripper.
	b.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Subscription-Token") != "test-secret" {
			t.Fatalf("token missing")
		}
		if !strings.Contains(r.URL.RawQuery, "q=") {
			t.Fatalf("query missing: %s", r.URL)
		}
		rec := httptest.NewRecorder()
		rec.WriteHeader(200)
		_, _ = rec.Write(body)
		return rec.Result(), nil
	})}
	got, err := b.Search(context.Background(), "Italian 2026 company discount XX")
	if err != nil || len(got) != 2 {
		t.Fatalf("Search: %v %#v", err, got)
	}
}

func TestTavilyAdapterFixture(t *testing.T) {
	body := readFixture(t, "tavily_search.json")
	tv := &Tavily{Secrets: staticSecret("test-secret"), CredRef: "cred_01J0000000000000000000000A",
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			rec := httptest.NewRecorder()
			rec.WriteHeader(200)
			_, _ = rec.Write(body)
			return rec.Result(), nil
		})}}
	got, err := tv.Search(context.Background(), "incentive XX")
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %#v", err, got)
	}
}

func TestSearXNGAdapterFixture(t *testing.T) {
	body := readFixture(t, "searxng_search.json")
	sx := &SearXNG{Origin: "https://searx.example",
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(r.URL.Path, "/search") {
				t.Fatalf("path %s", r.URL.Path)
			}
			rec := httptest.NewRecorder()
			rec.WriteHeader(200)
			_, _ = rec.Write(body)
			return rec.Result(), nil
		})}}
	got, err := sx.Search(context.Background(), "incentive XX")
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %#v", err, got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
