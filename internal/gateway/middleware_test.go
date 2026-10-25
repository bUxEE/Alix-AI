package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The body limit of a request is gateway.max_body_bytes unless its exact route declared one,
// and a declared one is clamped to the ceiling and ignored when it is not positive.
func TestAttachBodyLimitOfARoute(t *testing.T) {
	rt := &Router{}
	s := &Server{cfg: DefaultConfig(), router: rt}
	req := func(method, path string) *http.Request { return httptest.NewRequest(method, path, nil) }

	// Nothing declared: the gateway-wide limit, even for a server with no router.
	if got := s.attachBodyLimit(req(http.MethodPost, "/v1/files")); got != DefaultMaxBodyBytes {
		t.Fatalf("undeclared route = %d, want %d", got, DefaultMaxBodyBytes)
	}
	if got := (&Server{cfg: DefaultConfig()}).attachBodyLimit(req(http.MethodPost, "/v1/files")); got != DefaultMaxBodyBytes {
		t.Fatalf("no router = %d, want %d", got, DefaultMaxBodyBytes)
	}

	limit := int64(1 << 20)
	attachSetRouteLimit(rt, http.MethodPost, "/v1/files", func() int64 { return limit })
	for _, c := range []struct {
		method, path string
		want         int64
	}{
		{http.MethodPost, "/v1/files", 1 << 20},
		{http.MethodPut, "/v1/files", DefaultMaxBodyBytes},        // another method
		{http.MethodPost, "/v1/files/fil_x", DefaultMaxBodyBytes}, // another route
		{http.MethodPost, "/v1/files/", DefaultMaxBodyBytes},
		{http.MethodPost, "/v1/sessions", DefaultMaxBodyBytes},
	} {
		if got := s.attachBodyLimit(req(c.method, c.path)); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, got, c.want)
		}
	}

	// The declared function is asked for every request, so a changed configuration follows.
	limit = 2 << 20
	if got := s.attachBodyLimit(req(http.MethodPost, "/v1/files")); got != 2<<20 {
		t.Errorf("after a change = %d", got)
	}
	// Never above the ceiling, and a limit that is not positive is no limit declared.
	limit = 1 << 40
	if got := s.attachBodyLimit(req(http.MethodPost, "/v1/files")); got != maxMaxBodyBytes {
		t.Errorf("above the ceiling = %d, want %d", got, maxMaxBodyBytes)
	}
	for _, bad := range []int64{0, -5} {
		limit = bad
		if got := s.attachBodyLimit(req(http.MethodPost, "/v1/files")); got != DefaultMaxBodyBytes {
			t.Errorf("limit %d = %d, want the gateway's %d", bad, got, DefaultMaxBodyBytes)
		}
	}

	// Another router's declarations are not this one's.
	other := &Server{cfg: DefaultConfig(), router: &Router{}}
	limit = 1 << 20
	if got := other.attachBodyLimit(req(http.MethodPost, "/v1/files")); got != DefaultMaxBodyBytes {
		t.Errorf("another router = %d, want %d", got, DefaultMaxBodyBytes)
	}
}
