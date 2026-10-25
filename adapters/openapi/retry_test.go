package openapi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The adapter never retries, and neither does the HTTP stack under it on its
// behalf: net/http repeats a request on a fresh connection when a reused one
// turns out to be dead, and treats a request with an Idempotency-Key header as
// safe to repeat. Every request therefore asks for its connection to be closed,
// so no connection is ever reused and nothing is repeated behind the adapter's
// back (a repeat of a Commit is the coordinator's decision, taken after Status).
func TestNoHiddenRetry(t *testing.T) {
	ctx := context.Background()
	spec := miniSpec("      type: object\n      required: [status]\n      properties:\n        status: {type: string}\n")

	t.Run("a connection that dies after the request was read", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		var accepts, requests atomic.Int64
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				accepts.Add(1)
				go func() {
					defer c.Close()
					br := bufio.NewReader(c)
					for {
						line, err := br.ReadString('\n')
						if err != nil {
							return
						}
						if strings.TrimSpace(line) == "" {
							requests.Add(1)
							return // the request was read; no reply
						}
					}
				}()
			}
		}()
		api := &testAPI{URL: "http://" + ln.Addr().String(), Addr: ln.Addr().String()}
		a := newAdapter(t, miniConfig(t, api, spec, miniCapabilities()))

		_, err = read(a, fake.ReadCapability, map[string]any{"item_id": "x"})
		if contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("a read on a connection that died: %v, want unavailable", err)
		}
		p := preparedFor(fake.ActionCapability, newKey())
		_, err = a.Commit(ctx, testScope(), adapters.CommitRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: p.IdempotencyKey, Prepared: p})
		if contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
			t.Errorf("a commit on a connection that died: %v, want uncertain_outcome", err)
		}
		time.Sleep(50 * time.Millisecond)
		if accepts.Load() != 2 || requests.Load() != 2 {
			t.Errorf("%d connections and %d requests for 2 calls: something repeated a request", accepts.Load(), requests.Load())
		}
	})

	t.Run("a reused connection that died is not tried again behind the adapter's back", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		var requests atomic.Int64
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					br := bufio.NewReader(c)
					for n := 0; ; n++ {
						for {
							line, err := br.ReadString('\n')
							if err != nil {
								return
							}
							if strings.TrimSpace(line) == "" {
								break
							}
						}
						requests.Add(1)
						if n > 0 {
							return // a second request on a connection: it dies without a reply
						}
						reply := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(okResult)) + "\r\n\r\n" + okResult
						_, _ = io.WriteString(c, reply)
					}
				}()
			}
		}()
		api := &testAPI{URL: "http://" + ln.Addr().String(), Addr: ln.Addr().String()}
		a := newAdapter(t, miniConfig(t, api, spec, miniCapabilities()))
		for i := 0; i < 3; i++ {
			if _, err := read(a, fake.ReadCapability, map[string]any{"item_id": "x"}); err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
		}
		if n := requests.Load(); n != 3 {
			t.Errorf("the application received %d requests for 3 calls: a request was repeated", n)
		}
	})

	t.Run("a connection is used for one request", func(t *testing.T) {
		api := startAPI(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeBody(w, 200, "application/json", okResult)
		})
		a := newAdapter(t, miniConfig(t, api, spec, miniCapabilities()))
		for i := 0; i < 3; i++ {
			if _, err := read(a, fake.ReadCapability, map[string]any{"item_id": "x"}); err != nil {
				t.Fatal(err)
			}
		}
		if api.Accepts() != 3 {
			t.Errorf("%d connections for 3 calls: a connection was reused", api.Accepts())
		}
		for _, r := range api.Requests() {
			if !r.Close {
				t.Errorf("a request did not ask for its connection to be closed: %+v", r)
			}
		}
	})
}

type recordingTripper struct {
	got []*http.Request
}

func (r *recordingTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = append(r.got, req)
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
}

type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

// The credential goes to requests for the endpoint's own origin and to no other,
// and a credential that cannot be read or sent stops the request, whose body is
// then released.
func TestCredentialIsForItsOwnOriginOnly(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:8080/api")
	mk := func(secrets SecretProvider) (*credTripper, *recordingTripper) {
		next := &recordingTripper{}
		return &credTripper{
			next: next, origin: originOf(base), ref: testRef, secrets: secrets, header: "Authorization", scheme: "Bearer",
			log: slog.New(slog.NewTextHandler(io.Discard, nil)), app: testApp, ep: testEP,
		}, next
	}
	do := func(tr *credTripper, rawURL string, body *closeTracker) error {
		req, _ := http.NewRequest("POST", rawURL, nil)
		if body != nil {
			req.Body = body
		}
		_, err := tr.RoundTrip(req)
		return err
	}

	tr, next := mk(StaticSecrets{testRef: testToken})
	for _, u := range []string{"http://127.0.0.1:8080/x", "http://127.0.0.1:8081/x", "https://127.0.0.1:8080/x", "http://localhost:8080/x", "http://127.0.0.1.evil.test:8080/x", "http://127.0.0.1:80/x"} {
		if err := do(tr, u, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []string{"Bearer " + testToken, "", "", "", "", ""} {
		if got := next.got[i].Header.Get("Authorization"); got != want {
			t.Errorf("request to %s carries %q, want %q", next.got[i].URL, got, want)
		}
	}

	for name, secrets := range map[string]SecretProvider{
		"an unknown reference": StaticSecrets{},
		"a header injection":   StaticSecrets{testRef: "x\r\nX-Evil: 1"},
		"an empty value":       StaticSecrets{testRef: ""},
		"a failing provider":   secretFunc(func(context.Context, string) (Secret, error) { return "", io.ErrUnexpectedEOF }),
	} {
		tr, next := mk(secrets)
		body := &closeTracker{Reader: strings.NewReader("{}")}
		err := do(tr, "http://127.0.0.1:8080/x", body)
		var ce *credentialError
		if !asCredentialError(err, &ce) || len(next.got) != 0 || !body.closed {
			t.Errorf("%s: error %v, %d requests passed on, body closed %v", name, err, len(next.got), body.closed)
		}
	}
}

func asCredentialError(err error, target **credentialError) bool {
	ce, ok := err.(*credentialError)
	if ok {
		*target = ce
	}
	return ok
}

// The adapter is safe for concurrent use: many calls at once, each on its own
// connection, all answered, and the application sees each exactly once.
func TestConcurrentCalls(t *testing.T) {
	app := fake.NewApp()
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	h := fixture.Start(t, app)
	h.RequireBearer(testToken)
	ad := wrapped(t, newAdapter(t, fixtureConfig(t, h)))

	const workers, rounds = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds*2)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if res, err := readItem(context.Background(), ad, "item_1"); err != nil || res.Status != act.DeterministicResultStatusOk {
					errs <- fmt.Errorf("read: %+v, %v", res.Status, err)
				}
				po, err := ad.Prepare(context.Background(), testScope(), adapters.PrepareRequest{CapabilityID: fake.ActionCapability, Fields: map[string]any{"name": "Widget"}})
				if err != nil || po.Preparation == nil {
					errs <- fmt.Errorf("prepare: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := len(h.Requests()); n != workers*rounds*2 {
		t.Errorf("the application received %d requests, want %d", n, workers*rounds*2)
	}
}
