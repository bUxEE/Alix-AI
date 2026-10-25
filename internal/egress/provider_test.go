package egress

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestProviderRegistryDenyByDefaultAndSeparateFromApplications(t *testing.T) {
	const host = "https://api.example.com/x"
	var zero *ProviderRegistry
	if err := zero.Dialer("openai").allowlist().CheckURL(mustURL(t, host)); err == nil {
		t.Fatal("a nil provider registry allowed a URL")
	}
	pr := NewProviderRegistry()
	if err := pr.Dialer("openai").allowlist().CheckURL(mustURL(t, host)); err == nil {
		t.Fatal("a provider with no allowlist reached a host")
	}
	d, err := DestinationFromURL("https://api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	al, err := NewAllowlist(d)
	if err != nil {
		t.Fatal(err)
	}
	// An application allowlist grants the provider nothing.
	apps := NewRegistry()
	if err := apps.Set("app-1", al); err != nil {
		t.Fatal(err)
	}
	if err := pr.Dialer("openai").allowlist().CheckURL(mustURL(t, host)); err == nil {
		t.Fatal("an application allowlist granted a provider host")
	}
	if err := pr.Set("openai", al); err != nil {
		t.Fatal(err)
	}
	dl := pr.Dialer("openai")
	if err := dl.allowlist().CheckURL(mustURL(t, host)); err != nil {
		t.Fatalf("listed provider host refused: %v", err)
	}
	if err := dl.allowlist().CheckURL(mustURL(t, "https://other.example.com/x")); err == nil {
		t.Fatal("an unlisted host was allowed")
	}
	if err := pr.Dialer("anthropic").allowlist().CheckURL(mustURL(t, host)); err == nil {
		t.Fatal("another provider reached openai's host")
	}
	pr.Revoke("openai")
	if err := dl.allowlist().CheckURL(mustURL(t, host)); err == nil {
		t.Fatal("revocation did not apply to an existing dialer")
	}
	if err := pr.Set("Bad Id", al); err == nil {
		t.Fatal("malformed provider id accepted")
	}
}

func TestDestinationFromURL(t *testing.T) {
	ok := map[string]Destination{
		"https://api.openai.com":   {Host: "api.openai.com", Ports: []int{443}, Scheme: "https"},
		"https://api.openai.com/":  {Host: "api.openai.com", Ports: []int{443}, Scheme: "https"},
		"https://example.com:8443": {Host: "example.com", Ports: []int{8443}, Scheme: "https"},
		"http://127.0.0.1:9000":    {Host: "127.0.0.1", Ports: []int{9000}, Scheme: "http"},
	}
	for raw, want := range ok {
		got, err := DestinationFromURL(raw)
		if err != nil || got.Host != want.Host || got.Scheme != want.Scheme || len(got.Ports) != 1 || got.Ports[0] != want.Ports[0] {
			t.Errorf("%s: got %+v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "api.openai.com", "ftp://x.com", "https://u:p@x.com", "https://x.com/v1", "https://x.com?q=1", "https://x.com#f"} {
		if _, err := DestinationFromURL(raw); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
	d, err := DestinationFromURL("http://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAllowlist(d); err == nil {
		t.Error("plain http to a public host accepted")
	}
}

func TestProviderDialerReachesListedLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	d, err := DestinationFromURL(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	al, _ := NewAllowlist(d)
	pr := NewProviderRegistry()
	_ = pr.Set("fake", al)
	resp, err := pr.Dialer("fake").HTTPClient(0).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
