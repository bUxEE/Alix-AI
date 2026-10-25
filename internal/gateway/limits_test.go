package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers/fake"
)

// SEC-076: oversized request bodies are rejected at the gateway, before any
// handler logic or model call.
func TestBodyLimit(t *testing.T) {
	const limit = 2048
	h := newHarness(t, withSpike(), withConfig(func(c *Config) { c.MaxBodyBytes = limit }))
	h.fake.On(fake.Any()).ReplyJSON(proposalJSON(nil))

	fits := demoRequest()
	if body := mustJSON(fits); len(body) >= limit {
		t.Fatalf("the sample request is %d bytes, over the test limit", len(body))
	}
	big := demoRequest()
	big.Text = strings.Repeat("a", limit) // a valid-looking request, just too big
	bigBody := mustJSON(big)

	assertTooLarge := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %s", rec.Code, rec.Body.String())
		}
		env := mustEnvelope(t, rec)
		if env.Code != "validation_failed" || env.Message != "request body too large" {
			t.Errorf("envelope = %+v", env)
		}
		if got, _ := env.Details["limit"].(float64); int64(got) != limit {
			t.Errorf("details.limit = %v, want %d", env.Details["limit"], limit)
		}
		if h.fake.Calls() != 0 {
			t.Error("an oversize body reached the model")
		}
	}

	t.Run("declared length over the limit", func(t *testing.T) {
		// A declared Content-Length is refused before a byte is read.
		req := httptest.NewRequest(http.MethodPost, proposePath, &failOnRead{t: t})
		req.ContentLength = int64(len(bigBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		assertTooLarge(t, rec)
	})

	t.Run("declared length that lies", func(t *testing.T) {
		// A body longer than it claims is cut off by the reader.
		req := httptest.NewRequest(http.MethodPost, proposePath, strings.NewReader(bigBody))
		req.ContentLength = 100
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		assertTooLarge(t, rec)
	})

	t.Run("unknown length (chunked)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, proposePath, io.MultiReader(strings.NewReader(bigBody)))
		if req.ContentLength != -1 {
			t.Fatalf("ContentLength = %d, want -1 for an unknown length", req.ContentLength)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		assertTooLarge(t, rec)
	})

	t.Run("limit applies to every route", func(t *testing.T) {
		rec := h.doRaw(http.MethodPost, "/healthz", bigBody, "application/json")
		assertTooLarge(t, rec)
	})

	t.Run("a body within the limit is read", func(t *testing.T) {
		rec := h.do(http.MethodPost, proposePath, fits)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a body of exactly the limit is not too large", func(t *testing.T) {
		req := demoRequest()
		req.Text = strings.Repeat("a", limit)
		body := mustJSON(req)
		// Trim the request to land on the limit exactly: the text pads it.
		for len(body) > limit {
			req.Text = req.Text[:len(req.Text)-1]
			body = mustJSON(req)
		}
		if len(body) != limit {
			t.Fatalf("could not build a body of %d bytes, got %d", limit, len(body))
		}
		rec := h.doRaw(http.MethodPost, proposePath, body, "application/json")
		if rec.Code == http.StatusRequestEntityTooLarge {
			t.Errorf("a body of exactly %d bytes was refused as too large", limit)
		}
	})
}

// failOnRead is a body that fails the test if the gateway reads it.
type failOnRead struct{ t *testing.T }

func (f *failOnRead) Read([]byte) (int, error) {
	f.t.Error("the gateway read a body whose declared length was over the limit")
	return 0, io.EOF
}

func TestDefaultBodyLimitIs64KiB(t *testing.T) {
	h := newHarness(t, withSpike())
	h.fake.On(fake.Any()).ReplyJSON(proposalJSON(nil))
	rec := h.doRaw(http.MethodPost, proposePath, strings.Repeat(" ", DefaultMaxBodyBytes+1), "application/json")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 over the default %d bytes", rec.Code, DefaultMaxBodyBytes)
	}
	if h.fake.Calls() != 0 {
		t.Error("the model was called")
	}
}

// Over a real connection: the server's own limits and the chunked path.
func TestBodyLimitOverTheWire(t *testing.T) {
	h := newHarness(t, withSpike(), withConfig(func(c *Config) { c.MaxBodyBytes = 1024 }))
	h.fake.On(fake.Any()).ReplyJSON(proposalJSON(nil))
	ts := httptest.NewServer(h.srv.Handler()) // plain-HTTP loopback
	defer ts.Close()

	big := strings.NewReader(`{"text":"` + strings.Repeat("a", 4096) + `"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+proposePath, io.NopCloser(big)) // unknown length: chunked
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if h.fake.Calls() != 0 {
		t.Error("the model was called")
	}
}
