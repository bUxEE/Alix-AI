package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/quotas"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// quotaWireFakeFiles is a files service that keeps sizes, so the wiring has something to measure and to delete.
type quotaWireFakeFiles struct {
	mu      sync.Mutex
	sizes   map[string]int64
	deleted []string
}

func (f *quotaWireFakeFiles) add(id string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sizes == nil {
		f.sizes = map[string]int64{}
	}
	f.sizes[id] = size
}

func (f *quotaWireFakeFiles) Put(context.Context, contracts.Scope, files.PutInput) (conversation.FileRef, error) {
	return conversation.FileRef{}, contracts.New(contracts.ErrInternal, "not used")
}

func (f *quotaWireFakeFiles) Stat(_ context.Context, _ contracts.Scope, id string) (conversation.FileRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.sizes[id]
	if !ok {
		return conversation.FileRef{}, contracts.New(contracts.ErrScopeViolation, "no such file")
	}
	return conversation.FileRef{SizeBytes: n}, nil
}

func (f *quotaWireFakeFiles) Open(context.Context, contracts.Scope, string, files.Permission) (*files.Content, error) {
	return nil, contracts.New(contracts.ErrInternal, "not used")
}

func (f *quotaWireFakeFiles) Preview(context.Context, contracts.Scope, string) (*files.Preview, error) {
	return nil, contracts.New(contracts.ErrInternal, "not used")
}

func (f *quotaWireFakeFiles) Delete(_ context.Context, _ contracts.Scope, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sizes[id]; !ok {
		return contracts.New(contracts.ErrScopeViolation, "no such file")
	}
	delete(f.sizes, id)
	f.deleted = append(f.deleted, id)
	return nil
}

// Usage makes the fake measurable, as *files.Service is.
func (f *quotaWireFakeFiles) Usage(context.Context, contracts.Scope) (files.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var u files.Usage
	for _, n := range f.sizes {
		u.Files++
		u.Bytes += n
	}
	return u, nil
}

// quotaWireBlind hides Usage: a files service the wiring cannot measure.
type quotaWireBlind struct{ gateway.FilesService }

// quotaWireRig is the wiring in front of a mux with stand-ins for the routes it knows.
type quotaWireRig struct {
	t       *testing.T
	clk     *clock.Fake
	w       *quotaWire
	files   *quotaWireFakeFiles
	srv     *httptest.Server
	reached atomic.Int64 // handlers that ran
	nextID  atomic.Int64
}

var quotaWireT0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func quotaWireTinyLimits() quotas.Limits {
	return quotas.Limits{RequestsPerMinute: 60, Burst: 6, ConcurrentSessions: 2, IngestBytesPerDay: 100, StorageBytes: 1000}
}

// quotaWireNewRig builds the rig. blind hides the files service's Usage.
func quotaWireNewRig(t *testing.T, blind bool, mutate ...func(*quotas.Config)) *quotaWireRig {
	t.Helper()
	cfg := quotas.DefaultConfig()
	cfg.Defaults = quotaWireTinyLimits()
	for _, m := range mutate {
		m(&cfg)
	}
	rig := &quotaWireRig{t: t, clk: clock.NewFake(quotaWireT0), files: &quotaWireFakeFiles{}}
	var svc gateway.FilesService = rig.files
	if blind {
		svc = quotaWireBlind{rig.files}
	}
	w, err := quotaWireNew(quotaWireInput{
		DB: storetest.Open(t, nil), Clock: rig.clk, Config: cfg, Profile: "lite",
		Files: func() (gateway.FilesService, bool) { return svc, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.w = w

	// The stand-in for identity.Middleware: the scope comes from headers the test sets.
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			app, tenant, session := r.Header.Get("X-App"), r.Header.Get("X-Tenant"), r.Header.Get("X-Session")
			if app == "" {
				http.Error(rw, "unauthenticated", http.StatusUnauthorized)
				return
			}
			sc := storetest.Scope(app, tenant, "w1")
			sc.SessionID = session
			ctx, err := identity.WithScope(r.Context(), sc)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
	wrapped := quotaWireAfter(auth, w)
	mux := http.NewServeMux()
	route := func(pattern string, h func(http.ResponseWriter, *http.Request)) {
		mux.Handle(pattern, wrapped(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			rig.reached.Add(1)
			if r.Header.Get("X-Fail") != "" {
				http.Error(rw, "the handler refuses", http.StatusForbidden)
				return
			}
			h(rw, r)
		})))
	}
	ok := func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{}`))
	}
	route("GET /v1/health", ok)
	route("POST /v1/sessions", ok)
	route("GET /v1/sessions/{session_id}", ok)
	route("POST /v1/sessions/{session_id}/turns", ok)
	route("DELETE /v1/sessions/{session_id}", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusNoContent) })
	route("POST /v1/agui", ok)
	route("POST /v1/actions/{preparation_id}/confirm", ok)
	route("POST /v1/actions/{preparation_id}/reject", ok)
	route("GET /v1/actions/{preparation_id}/receipt", ok)
	route("POST /v1/files", func(rw http.ResponseWriter, r *http.Request) {
		// The body is the file plus 10 bytes of framing, as a multipart upload is.
		n, _ := io.Copy(io.Discard, r.Body)
		size := max(n-10, 1)
		id := "fil_" + strconv.FormatInt(rig.nextID.Add(1), 10)
		rig.files.add(id, size)
		body := fmt.Sprintf(`{"file_id":%q,"size_bytes":%d,"treatment":%q}`, id, size, r.URL.Query().Get("treatment"))
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		_, _ = rw.Write([]byte(body))
	})
	route("DELETE /v1/files/{file_id}", func(rw http.ResponseWriter, r *http.Request) {
		if err := rig.files.Delete(r.Context(), contracts.Scope{}, r.PathValue("file_id")); err != nil {
			http.Error(rw, "no such file", http.StatusNotFound)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})
	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)
	return rig
}

// quotaWireCaller identifies the tenant a request is made as.
type quotaWireCaller struct{ app, tenant, session string }

func (c quotaWireCaller) scope() contracts.Scope {
	sc := storetest.Scope(c.app, c.tenant, "w1")
	sc.SessionID = c.session
	return sc
}

type quotaWireAnswer struct {
	code int
	hdr  http.Header
	err  contracts.Error
	raw  string
}

func (a quotaWireAnswer) detail(key string) any { return a.err.Details[key] }

func (rig *quotaWireRig) do(c quotaWireCaller, method, path, body string, hdr ...string) quotaWireAnswer {
	rig.t.Helper()
	req, err := http.NewRequest(method, rig.srv.URL+path, strings.NewReader(body))
	if err != nil {
		rig.t.Fatal(err)
	}
	req.Header.Set("X-App", c.app)
	req.Header.Set("X-Tenant", c.tenant)
	req.Header.Set("X-Session", c.session)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		rig.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := quotaWireAnswer{code: resp.StatusCode, hdr: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &a.err)
	return a
}

func (rig *quotaWireRig) used(c quotaWireCaller, dim quotas.Dimension) int64 {
	rig.t.Helper()
	us, err := rig.w.q.Usage(context.Background(), c.scope())
	if err != nil {
		rig.t.Fatal(err)
	}
	for _, u := range us {
		if u.Dimension == dim {
			return u.Used
		}
	}
	rig.t.Fatalf("no usage for %s", dim)
	return 0
}

// drain spends every token of the caller's bucket on a plain request and returns how many it spent.
func (rig *quotaWireRig) drain(c quotaWireCaller) int {
	rig.t.Helper()
	n := 0
	for ; n < 1000; n++ {
		if a := rig.do(c, http.MethodGet, "/v1/health", ""); a.code != http.StatusOK {
			if a.err.Code != contracts.ErrRateLimited {
				rig.t.Fatalf("a plain request was refused with %d %q, want rate_limited", a.code, a.err.Code)
			}
			return n
		}
	}
	rig.t.Fatal("the bucket never ran dry")
	return n
}

func (rig *quotaWireRig) refill() { rig.clk.Advance(time.Minute) }

var (
	quotaWireAlice = quotaWireCaller{"app-1", "alice", "ses_a"}
	quotaWireBob   = quotaWireCaller{"app-1", "bob", "ses_b"}
)

func (c quotaWireCaller) as(session string) quotaWireCaller { c.session = session; return c }

// The request path is refused at the tenant's own rate, with the typed body and a Retry-After in whole
// seconds, and another tenant keeps its whole bucket.
func TestQuotaWireRateLimitsOneTenantAndTellsWhenToRetry(t *testing.T) {
	rig := quotaWireNewRig(t, false)
	if got := rig.drain(quotaWireAlice); got != 6 {
		t.Fatalf("a tenant was admitted %d plain requests, want its burst 6", got)
	}
	a := rig.do(quotaWireAlice, http.MethodGet, "/v1/health", "")
	if a.code != http.StatusTooManyRequests || a.err.Code != contracts.ErrRateLimited || !a.err.Retryable {
		t.Fatalf("refusal = %d %+v, want 429 rate_limited, retryable", a.code, a.err)
	}
	ms, _ := a.detail("retry_after_ms").(float64)
	if ms <= 0 || ms > 1000 {
		t.Errorf("retry_after_ms = %v, want a wait of up to a second at 60/min", a.detail("retry_after_ms"))
	}
	if got := a.hdr.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	reached := rig.reached.Load()
	rig.do(quotaWireAlice, http.MethodGet, "/v1/health", "")
	if rig.reached.Load() != reached {
		t.Error("a refused request reached its handler")
	}
	if got := rig.drain(quotaWireBob); got != 6 {
		t.Errorf("another tenant was admitted %d requests, want its whole burst 6", got)
	}
	// The same tenant id under another application is another tenant.
	if got := rig.drain(quotaWireCaller{"app-2", "alice", "ses_a"}); got != 6 {
		t.Errorf("the same tenant id in another application was admitted %d requests, want 6", got)
	}
	// A request with no verified scope is never admitted.
	if a := rig.do(quotaWireCaller{}, http.MethodGet, "/v1/health", ""); a.code != http.StatusUnauthorized {
		t.Errorf("a request with no scope = %d, want 401", a.code)
	}
}

// A session holds one slot from its opening to its end, however often it is opened again; a failed opening
// holds nothing; an expired lease is reclaimed; and the slots are the tenant's own.
func TestQuotaWireSessionsHoldOneSlotEach(t *testing.T) {
	rig := quotaWireNewRig(t, false, func(c *quotas.Config) { c.Defaults.Burst = 100; c.Defaults.RequestsPerMinute = 6000 })
	open := func(c quotaWireCaller, hdr ...string) quotaWireAnswer {
		return rig.do(c, http.MethodPost, "/v1/sessions", "", hdr...)
	}
	a1, a2, a3 := quotaWireAlice.as("ses_1"), quotaWireAlice.as("ses_2"), quotaWireAlice.as("ses_3")

	if r := open(a1); r.code != http.StatusOK {
		t.Fatalf("first open = %d", r.code)
	}
	for i := 0; i < 5; i++ { // opening is idempotent: five more opens of the same session take nothing
		if r := open(a1); r.code != http.StatusOK {
			t.Fatalf("re-open %d = %d", i, r.code)
		}
	}
	if got := rig.used(a1, quotas.DimConcurrentSessions); got != 1 {
		t.Fatalf("one session holds %d slots", got)
	}
	if r := open(a2); r.code != http.StatusOK {
		t.Fatalf("second session = %d", r.code)
	}
	r := open(a3)
	if r.err.Code != contracts.ErrResourceExhausted || r.detail("dimension") != "concurrent_sessions" || r.detail("limit") != float64(2) {
		t.Fatalf("third session = %d %+v, want resource_exhausted for concurrent_sessions, limit 2", r.code, r.err)
	}
	reached := rig.reached.Load()
	open(a3)
	if rig.reached.Load() != reached {
		t.Error("a refused opening reached its handler")
	}
	// A session that is already open is never turned away by its own reads or by its turns.
	if r := rig.do(a1, http.MethodGet, "/v1/sessions/ses_1", ""); r.code != http.StatusOK {
		t.Errorf("reading an open session = %d", r.code)
	}
	if r := rig.do(a2, http.MethodPost, "/v1/sessions/ses_2/turns", "{}"); r.code != http.StatusOK {
		t.Errorf("a turn of an open session = %d", r.code)
	}
	// Another tenant has its own slots.
	if r := open(quotaWireBob.as("ses_1")); r.code != http.StatusOK {
		t.Errorf("another tenant's first session = %d", r.code)
	}
	// The end of a session gives its slot back; a failed end does not.
	if r := rig.do(a1, http.MethodDelete, "/v1/sessions/ses_1", "", "X-Fail", "1"); r.code != http.StatusForbidden {
		t.Fatalf("failing delete = %d", r.code)
	}
	if r := open(a3); r.err.Code != contracts.ErrResourceExhausted {
		t.Errorf("a failed end freed a slot: %d", r.code)
	}
	if r := rig.do(a1, http.MethodDelete, "/v1/sessions/ses_1", ""); r.code != http.StatusNoContent {
		t.Fatalf("delete = %d", r.code)
	}
	if r := open(a3); r.code != http.StatusOK {
		t.Fatalf("after a delete, a new session = %d %+v", r.code, r.err)
	}
	// An opening the handler refuses (a quarantined application) holds nothing.
	if r := rig.do(a3, http.MethodDelete, "/v1/sessions/ses_3", ""); r.code != http.StatusNoContent {
		t.Fatalf("delete ses_3 = %d", r.code)
	}
	if r := open(a1, "X-Fail", "1"); r.code != http.StatusForbidden {
		t.Fatalf("failing open = %d", r.code)
	}
	if got := rig.used(a1, quotas.DimConcurrentSessions); got != 1 {
		t.Errorf("slots held after a failed open = %d, want only ses_2's 1", got)
	}
	// A lease that was not renewed expires: the slot is free, and the session can take one again.
	rig.clk.Advance(2 * time.Hour)
	if got := rig.used(a1, quotas.DimConcurrentSessions); got != 0 {
		t.Errorf("slots held after the lease lifetime = %d", got)
	}
	if r := open(a2); r.code != http.StatusOK {
		t.Errorf("re-opening after expiry = %d %+v", r.code, r.err)
	}
	if got := rig.used(a1, quotas.DimConcurrentSessions); got != 1 {
		t.Errorf("slots held after re-opening = %d, want 1", got)
	}
}

// A confirmed commit is never refused for quota: with every dimension of the tenant used up, a confirmation, a
// rejection, a status read and an AG-UI resume all reach their handlers, and the same tenant's ordinary request,
// a new session, an upload and a new run are refused at that same moment.
func TestQuotaNeverRefusesAConfirmedCommit(t *testing.T) {
	rig := quotaWireNewRig(t, false)
	c := quotaWireAlice

	// Use up the four dimensions: two sessions, 100 ingest bytes (a RAG source), 1000 stored bytes, the bucket.
	for _, s := range []string{"ses_1", "ses_2"} {
		if r := rig.do(c.as(s), http.MethodPost, "/v1/sessions", ""); r.code != http.StatusOK {
			t.Fatalf("open %s = %d", s, r.code)
		}
	}
	if r := rig.do(c, http.MethodPost, "/v1/files?treatment=rag_source", strings.Repeat("x", 110)); r.code != http.StatusCreated {
		t.Fatalf("ingesting upload = %d %s", r.code, r.raw)
	}
	if r := rig.do(c, http.MethodPost, "/v1/files?treatment=ephemeral_context", strings.Repeat("x", 890)); r.code != http.StatusCreated {
		t.Fatalf("filling upload = %d %s", r.code, r.raw)
	}
	rig.refill()
	// The control: at this very moment the tenant is refused on every path that starts work.
	if r := rig.do(c.as("ses_3"), http.MethodPost, "/v1/sessions", ""); r.err.Code != contracts.ErrResourceExhausted {
		t.Fatalf("control: a third session = %d %s", r.code, r.raw)
	}
	r := rig.do(c, http.MethodPost, "/v1/files?treatment=ephemeral_context", strings.Repeat("x", 60))
	if r.code != http.StatusInsufficientStorage || r.err.Code != contracts.ErrResourceExhausted || r.detail("dimension") != "storage_bytes" {
		t.Fatalf("control: an upload = %d %s", r.code, r.raw)
	}
	if got := rig.used(c, quotas.DimStorageBytes); got != 980 {
		t.Errorf("the refused upload left the gauge at %d, want 980", got)
	}
	agui := func(body string) quotaWireAnswer { return rig.do(c, http.MethodPost, "/v1/agui", body) }
	if n := rig.drain(c); n < 1 {
		t.Fatal("the bucket had nothing in it")
	}
	if r := agui(`{"messages":[{"role":"user","content":"hello"}]}`); r.err.Code != contracts.ErrRateLimited {
		t.Fatalf("control: a new run = %d %s", r.code, r.raw)
	}
	if r := rig.do(c, http.MethodGet, "/v1/health", ""); r.err.Code != contracts.ErrRateLimited {
		t.Fatalf("control: an ordinary request = %d %s", r.code, r.raw)
	}

	// The commit path, with the bucket empty and every volume full.
	reached := rig.reached.Load()
	want := int64(0)
	pass := func(what string, a quotaWireAnswer) {
		t.Helper()
		if a.code != http.StatusOK {
			t.Errorf("%s = %d %s, want it to reach its handler", what, a.code, a.raw)
		}
		want++
	}
	for i := 0; i < 8; i++ { // more than the whole burst: a drained bucket does not matter
		pass("confirm", rig.do(c, http.MethodPost, "/v1/actions/prep_1/confirm", `{"token":"x"}`))
	}
	pass("reject", rig.do(c, http.MethodPost, "/v1/actions/prep_2/reject", "{}"))
	pass("receipt", rig.do(c, http.MethodGet, "/v1/actions/prep_1/receipt", ""))
	pass("an AG-UI resume", agui(`{"threadId":"t","runId":"r","resume":[{"interruptId":"i","status":"resolved"}]}`))
	pass("an AG-UI approval as a tool result", agui(`{"threadId":"t","runId":"r","messages":[{"role":"user","content":"x"},{"role":"tool","toolCallId":"call_1","content":"{}"}]}`))
	if got := rig.reached.Load() - reached; got != want {
		t.Errorf("%d of %d commit-path requests reached a handler", got, want)
	}
	// A client that names a resume in a body that is not one is still held to the rate afterwards: a UI tool's
	// result is data, not an approval.
	if r := agui(`{"threadId":"t","runId":"r","messages":[{"role":"tool","toolCallId":"alixai_ui_1","content":"{}"}]}`); r.err.Code != contracts.ErrRateLimited {
		t.Errorf("a UI tool result was let through the rate: %d %s", r.code, r.raw)
	}
	// The same requests, as another tenant's ordinary traffic, are admitted: nothing leaked across tenants.
	if r := rig.do(quotaWireBob, http.MethodGet, "/v1/health", ""); r.code != http.StatusOK {
		t.Errorf("another tenant's request = %d", r.code)
	}
}

// Storage is reserved before an upload is read and settled to the measured value after it: a refused or failed
// upload leaves nothing reserved, a delete gives the bytes back, and a tenant over its day of ingestion has the
// file withdrawn again.
func TestQuotaWireStorageAndIngestion(t *testing.T) {
	for _, blind := range []bool{false, true} {
		t.Run(fmt.Sprintf("blind=%v", blind), func(t *testing.T) {
			rig := quotaWireNewRig(t, blind, func(c *quotas.Config) { c.Defaults.Burst = 100; c.Defaults.RequestsPerMinute = 6000 })
			c := quotaWireAlice
			up := func(n int, query string, hdr ...string) quotaWireAnswer {
				return rig.do(c, http.MethodPost, "/v1/files"+query, strings.Repeat("x", n), hdr...)
			}
			// 400 bytes of file + 10 of framing: the gauge ends at the file's size, not the reservation.
			a := up(410, "?treatment=ephemeral_context")
			if a.code != http.StatusCreated {
				t.Fatalf("upload = %d %s", a.code, a.raw)
			}
			if got := rig.used(c, quotas.DimStorageBytes); got != 400 && !(blind && got == 410) {
				t.Errorf("the gauge after a 400-byte upload = %d", got)
			}
			var ref struct {
				FileID string `json:"file_id"`
			}
			_ = json.Unmarshal([]byte(a.raw), &ref)
			// A handler that fails gives the whole reservation back.
			before := rig.used(c, quotas.DimStorageBytes)
			if a := up(300, "?treatment=ephemeral_context", "X-Fail", "1"); a.code != http.StatusForbidden {
				t.Fatalf("failing upload = %d", a.code)
			}
			if got := rig.used(c, quotas.DimStorageBytes); got != before {
				t.Errorf("a failed upload moved the gauge from %d to %d", before, got)
			}
			// Over the tenant's storage: typed, 507, before the handler.
			reached := rig.reached.Load()
			a = up(700, "?treatment=ephemeral_context")
			if a.code != http.StatusInsufficientStorage || a.err.Code != contracts.ErrResourceExhausted ||
				a.detail("dimension") != "storage_bytes" || a.detail("limit") != float64(1000) {
				t.Fatalf("over-quota upload = %d %s", a.code, a.raw)
			}
			if rig.reached.Load() != reached {
				t.Error("an upload over the storage quota reached its handler")
			}
			// A delete frees the bytes; the same upload is then admitted.
			if r := rig.do(c, http.MethodDelete, "/v1/files/"+ref.FileID, ""); r.code != http.StatusNoContent {
				t.Fatalf("delete = %d", r.code)
			}
			if a := up(700, "?treatment=ephemeral_context"); a.code != http.StatusCreated {
				t.Fatalf("upload after the delete = %d %s", a.code, a.raw)
			}
			// A tenant with another tenant's bytes is not affected by them.
			if got := rig.used(quotaWireBob, quotas.DimStorageBytes); got != 0 {
				t.Errorf("another tenant's storage = %d", got)
			}

			// Ingestion: a RAG source is charged to the day; the next one that does not fit is withdrawn and refused.
			c = quotaWireBob
			if a := up(61, "?treatment=rag_source"); a.code != http.StatusCreated {
				t.Fatalf("ingesting upload = %d %s", a.code, a.raw)
			}
			if got := rig.used(c, quotas.DimIngestBytesPerDay); got != 51 {
				t.Errorf("ingest counter = %d, want 51", got)
			}
			a = up(61, "?treatment=rag_source")
			if a.err.Code != contracts.ErrResourceExhausted || a.detail("dimension") != "ingest_bytes_per_day" ||
				a.detail("limit") != float64(100) || a.detail("resets_at") != "2026-10-09T00:00:00Z" {
				t.Fatalf("over-ingestion = %d %s", a.code, a.raw)
			}
			if got := len(rig.files.deleted); got < 1 {
				t.Errorf("the file over the ingestion quota was not withdrawn (%d deleted)", got)
			}
			if got := rig.used(c, quotas.DimIngestBytesPerDay); got != 51 {
				t.Errorf("a refused ingestion charged the counter: %d", got)
			}
			// A plain upload is not ingestion.
			if a := up(61, "?treatment=ephemeral_context"); a.code != http.StatusCreated {
				t.Errorf("a non-source upload = %d %s", a.code, a.raw)
			}
		})
	}
}

func TestQuotaWireConfigFollowsTheServeProfile(t *testing.T) {
	cfg, err := quotaWireConfig(quotas.DefaultConfig(), quotas.ProfileBalanced)
	if err != nil || cfg.Profile != quotas.ProfileBalanced {
		t.Fatalf("the section's default follows the serve profile: %+v, %v", cfg, err)
	}
	pinned := quotas.DefaultConfig()
	pinned.Profile = quotas.ProfilePerformance
	if cfg, err = quotaWireConfig(pinned, quotas.ProfileBalanced); err != nil || cfg.Profile != quotas.ProfilePerformance {
		t.Fatalf("a profile named in the section wins: %+v, %v", cfg, err)
	}
	if _, err = quotaWireConfig(quotas.DefaultConfig(), "no-such-profile"); err == nil {
		t.Fatal("a profile with no quota table started")
	}
	if cfg, err = quotaWireConfig(quotas.Config{}, ""); err != nil || cfg.Profile != quotas.ProfileLite {
		t.Fatalf("no profile anywhere is lite: %+v, %v", cfg, err)
	}
}

func TestQuotaWireClassifiesTheRoutes(t *testing.T) {
	for pattern, want := range map[string]quotaWireRoute{
		"POST /v1/sessions":                                quotaWireOpen,
		"POST /v1/sessions/{session_id}/turns":             quotaWireTurn,
		"DELETE /v1/sessions/{session_id}":                 quotaWireEnd,
		"GET /v1/sessions/{session_id}/pending":            quotaWireSessionRead,
		"POST /v1/agui":                                    quotaWireAguiRun,
		"GET /v1/agui/capabilities":                        quotaWireSessionRead,
		"POST /v1/actions/{preparation_id}/confirm":        quotaWireCommit,
		"POST /v1/actions/{preparation_id}/reject":         quotaWireCommit,
		"GET /v1/actions/{preparation_id}/receipt":         quotaWireCommit,
		"POST /v1/files":                                   quotaWireUpload,
		"DELETE /v1/files/{file_id}":                       quotaWireFileDelete,
		"GET /v1/files/{file_id}":                          quotaWirePlain,
		"POST /v1/rag/search":                              quotaWirePlain,
		"POST /v1/applications":                            quotaWirePlain,
		"":                                                 quotaWirePlain,
		"POST /v1/actionsx/{preparation_id}/confirm-ish":   quotaWirePlain,
		"POST /v1/sessions/{session_id}/turns/extra/parts": quotaWireSessionRead,
	} {
		if got := quotaWireClassify(&http.Request{Pattern: pattern}); got != want {
			t.Errorf("%q classified as %d, want %d", pattern, got, want)
		}
	}
}

// quotaWireServeYAML is a quotas section tight enough to reach every ceiling in a few calls.
const quotaWireServeYAML = `quotas:
  defaults:
    requests_per_minute: 60
    burst: 12
    concurrent_sessions: 2
`

// The wiring on the real path: `alixai serve` over a live application, real tokens through the identity
// middleware, and the quotas read from the configuration file. Sessions hold slots and give them back, a
// flood is refused with the typed body at the tenant's own rate and leaves another tenant its whole allowance,
// and the commit path still answers with the bucket empty.
func TestServeEnforcesQuotasOnTheRealPaths(t *testing.T) {
	auditChecksProbesBuiltinChecksOnly(t)
	transportsSwapDefault(t)
	f := newFixture(t)
	t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
	iss := newHostIssuer(t)
	pub, priv, err := trust.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg, dataDir := serveConfig(t, f, identityYAML+quotaWireServeYAML)
	provisionIssuer(t, cfg, iss)
	reauditTestSeed(t, dataDir, pub, trust.SigningKey{KeyID: "quota-signer", Key: priv})
	srv := startServe(t, cfg)

	call := func(tenant, session, method, path, body string) (int, http.Header, contracts.Error) {
		t.Helper()
		token := iss.Token(func(d *identitytest.Draft) {
			d.Set(identity.ClaimTenant, tenant)
			d.Set(identity.ClaimSession, session)
		})
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, srv.base+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var e contracts.Error
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, resp.Header, e
	}

	// Concurrent sessions: two slots, idempotent opening, a typed refusal, a slot back on delete.
	const tenant = "ten_sessions"
	for _, s := range []string{"ses_one", "ses_one", "ses_two"} {
		if code, _, e := call(tenant, s, http.MethodPost, "/v1/sessions", ""); code != http.StatusOK {
			t.Fatalf("open %s = %d %+v", s, code, e)
		}
	}
	code, _, e := call(tenant, "ses_three", http.MethodPost, "/v1/sessions", "")
	if code != http.StatusServiceUnavailable || e.Code != contracts.ErrResourceExhausted || e.Details["dimension"] != "concurrent_sessions" || e.Details["limit"] != float64(2) {
		t.Fatalf("a third session = %d %+v, want resource_exhausted for concurrent_sessions with limit 2", code, e)
	}
	if code, _, e := call("ten_victim", "ses_three", http.MethodPost, "/v1/sessions", ""); code != http.StatusOK {
		t.Fatalf("another tenant's session was refused: %d %+v", code, e)
	}
	if code, _, e := call(tenant, "ses_one", http.MethodDelete, "/v1/sessions/ses_one", ""); code >= 300 {
		t.Fatalf("delete = %d %+v", code, e)
	}
	if code, _, e := call(tenant, "ses_three", http.MethodPost, "/v1/sessions", ""); code != http.StatusOK {
		t.Fatalf("a session after a delete = %d %+v", code, e)
	}

	// Requests per minute: the burst, then rate_limited with retry_after_ms; the other tenant is untouched.
	const flooder = "ten_flood"
	admitted := 0
	var refused contracts.Error
	var hdr http.Header
	for i := 0; i < 40; i++ {
		c, h, e := call(flooder, "ses_f", http.MethodGet, "/v1/health", "")
		if c == http.StatusOK {
			admitted++
			continue
		}
		if c != http.StatusTooManyRequests {
			t.Fatalf("a flood request = %d %+v, want 200 or 429", c, e)
		}
		refused, hdr = e, h
		break
	}
	// The burst of 12, plus at most a token the clock refilled (60 a minute is 1 a second) while this ran.
	if admitted < 12 || admitted > 14 {
		t.Errorf("the flooder was admitted %d requests, want its burst of 12", admitted)
	}
	if refused.Code != contracts.ErrRateLimited || !refused.Retryable {
		t.Fatalf("the flood was never refused with rate_limited: %+v", refused)
	}
	if ms, _ := refused.Details["retry_after_ms"].(float64); ms <= 0 || ms > 1000 {
		t.Errorf("retry_after_ms = %v", refused.Details["retry_after_ms"])
	}
	if hdr.Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q", hdr.Get("Retry-After"))
	}
	for i := 0; i < 5; i++ {
		if c, _, e := call("ten_victim", "ses_v", http.MethodGet, "/v1/health", ""); c != http.StatusOK {
			t.Fatalf("the victim's request %d = %d %+v while another tenant flooded", i, c, e)
		}
	}
	// The commit path answers with the bucket empty: what it answers is the action service's, never a quota's.
	for i := 0; i < 3; i++ {
		c, _, e := call(flooder, "ses_f", http.MethodPost, "/v1/actions/prep_01J0000000000000000000A001/confirm", `{"token":"nope"}`)
		if c == http.StatusTooManyRequests || e.Code == contracts.ErrRateLimited || e.Code == contracts.ErrResourceExhausted {
			t.Fatalf("a confirmation was refused for quota: %d %+v", c, e)
		}
	}
	// The control: the same bucket, empty at this moment, still refuses an ordinary request.
	if c, _, e := call(flooder, "ses_f", http.MethodGet, "/v1/health", ""); c != http.StatusTooManyRequests {
		t.Fatalf("control: an ordinary request with the bucket empty = %d %+v", c, e)
	}
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srv.stderr.String(), "tenant quotas") {
		t.Errorf("the log does not say the quotas are on:\n%s", srv.stderr.String())
	}
}

// ---- M9-01: the charge is made in the data's own transaction ---------------------------------------------

// quotaTxAcceptRAG is a RAG ingestion that takes every file, so the ingestion charge of an accepted source can be
// seen (the registry of this build takes none: attachRAGSources refuses).
type quotaTxAcceptRAG struct{}

func (quotaTxAcceptRAG) RegisterFile(context.Context, contracts.Scope, files.RAGFile) error {
	return nil
}
func (quotaTxAcceptRAG) RemoveFile(context.Context, files.Location, string) error { return nil }

// quotaTxRig is the quota wiring in front of the real files service, charging through the hook the way serve
// does: provideFiles when prod is true (the RAG seam of this build, which refuses a file source), a files
// service of the test's own with an accepting ingestion otherwise.
type quotaTxRig struct {
	t   *testing.T
	clk *clock.Fake
	db  *store.DB
	w   *quotaWire
	svc gateway.FilesService
	srv *httptest.Server
	// reached counts the handlers that ran.
	reached atomic.Int64
}

func quotaTxNewRig(t *testing.T, prod bool) *quotaTxRig {
	t.Helper()
	oldFiles := *files.ConfigSection()
	t.Cleanup(func() { *files.ConfigSection() = oldFiles })
	cfg := files.DefaultConfig()
	cfg.RAGSource.Enabled = true
	*files.ConfigSection() = cfg

	rig := &quotaTxRig{t: t, clk: clock.NewFake(quotaWireT0), db: storetest.Open(t, nil)}
	qcfg := quotas.DefaultConfig()
	qcfg.Defaults = quotaWireTinyLimits()
	qcfg.Defaults.Burst, qcfg.Defaults.RequestsPerMinute = 100, 6000
	w, err := quotaWireNew(quotaWireInput{
		DB: rig.db, Clock: rig.clk, Config: qcfg, Profile: "lite",
		Files: func() (gateway.FilesService, bool) { return rig.svc, rig.svc != nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.w = w
	deps := serveDeps{
		Log: slog.New(slog.DiscardHandler), Clock: rig.clk, DB: rig.db, DataDir: t.TempDir(), Services: gateway.NewServices(),
		AddService: func(func(context.Context) func()) {},
	}
	if prod {
		if err := provideFiles(context.Background(), deps); err != nil {
			t.Fatal(err)
		}
		var ok bool
		if rig.svc, ok = gateway.Lookup[gateway.FilesService](deps.Services); !ok {
			t.Fatal("the files service was not provided")
		}
	} else {
		fs, err := files.New(files.Options{
			DB: rig.db, Clock: rig.clk, DataDir: t.TempDir(), RAG: quotaTxAcceptRAG{}, Charge: quotaTxChargerFor(rig.db),
		})
		if err != nil {
			t.Fatal(err)
		}
		rig.svc = fs
	}
	if !quotaTxCharges(rig.svc) {
		t.Fatal("the files service does not charge in its transaction: serve would fall back to the reservation")
	}

	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			sc := storetest.Scope(r.Header.Get("X-App"), r.Header.Get("X-Tenant"), "w1")
			sc.SessionID = r.Header.Get("X-Session")
			ctx, err := identity.WithScope(r.Context(), sc)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
	wrapped := quotaWireAfter(auth, w)
	mux := http.NewServeMux()
	mux.Handle("POST /v1/files", wrapped(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rig.reached.Add(1)
		scope, _ := identity.ScopeFromContext(r.Context())
		ref, err := rig.svc.Put(r.Context(), scope, files.PutInput{
			Name: "f.txt", DeclaredMIME: "text/plain", Treatment: files.Treatment(r.URL.Query().Get("treatment")), Content: r.Body,
		})
		if err != nil {
			// The status the files routes declare for a tenant over its quota (gateway filesError).
			if contracts.CodeOf(err) == contracts.ErrResourceExhausted {
				gateway.WriteErrorStatus(rw, r, http.StatusInsufficientStorage, err)
				return
			}
			gateway.WriteError(rw, r, err)
			return
		}
		treatment := ""
		if ref.Treatment != nil {
			treatment = string(*ref.Treatment)
		}
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(rw, `{"file_id":%q,"size_bytes":%d,"treatment":%q}`, ref.FileID, ref.SizeBytes, treatment)
	})))
	mux.Handle("DELETE /v1/files/{file_id}", wrapped(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		scope, _ := identity.ScopeFromContext(r.Context())
		if err := rig.svc.Delete(r.Context(), scope, r.PathValue("file_id")); err != nil {
			gateway.WriteError(rw, r, err)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	})))
	mux.Handle("POST /v1/actions/{preparation_id}/confirm", wrapped(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rig.reached.Add(1)
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(`{}`))
	})))
	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)
	return rig
}

func (rig *quotaTxRig) do(c quotaWireCaller, method, path, body string) quotaWireAnswer {
	rig.t.Helper()
	req, err := http.NewRequest(method, rig.srv.URL+path, strings.NewReader(body))
	if err != nil {
		rig.t.Fatal(err)
	}
	req.Header.Set("X-App", c.app)
	req.Header.Set("X-Tenant", c.tenant)
	req.Header.Set("X-Session", c.session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		rig.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := quotaWireAnswer{code: resp.StatusCode, hdr: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &a.err)
	return a
}

func (rig *quotaTxRig) used(c quotaWireCaller, dim quotas.Dimension) int64 {
	rig.t.Helper()
	us, err := rig.w.q.Usage(context.Background(), c.scope())
	if err != nil {
		rig.t.Fatal(err)
	}
	for _, u := range us {
		if u.Dimension == dim {
			return u.Used
		}
	}
	rig.t.Fatalf("no usage for %s", dim)
	return 0
}

// live is how many files the tenant holds, as the files module counts them.
func (rig *quotaTxRig) live(c quotaWireCaller) int {
	rig.t.Helper()
	u, err := rig.svc.(quotaWireUsage).Usage(context.Background(), c.scope())
	if err != nil {
		rig.t.Fatal(err)
	}
	return u.Files
}

// M9-01: serve charges storage and ingestion in the transaction that stores the data. The boundary only checks
// early. The tests ask each claim where a mutant would show: the refusal comes from the charge, not from the
// boundary (a rag_source upload is refused by the ingestion quota with the storage quota not even close); the
// refused upload leaves no file and no charge; an ingestion that refuses the file gives its charge back; and a
// confirmed commit still reaches its handler with every volume full.
func TestServeChargesStorageAndIngestThroughTheDataTransaction(t *testing.T) {
	c := quotaWireAlice
	up := func(rig *quotaTxRig, n int, treatment string) quotaWireAnswer {
		return rig.do(c, http.MethodPost, "/v1/files?treatment="+treatment, strings.Repeat("x", n))
	}
	idOf := func(a quotaWireAnswer) string {
		var ref struct {
			FileID string `json:"file_id"`
		}
		_ = json.Unmarshal([]byte(a.raw), &ref)
		return ref.FileID
	}

	t.Run("accepted sources", func(t *testing.T) {
		rig := quotaTxNewRig(t, false)

		// Charged by the write: the ingestion counter and the gauge move with the stored file.
		first := up(rig, 60, "rag_source")
		if first.code != http.StatusCreated {
			t.Fatalf("a 60-byte source = %d %s", first.code, first.raw)
		}
		if i, s := rig.used(c, quotas.DimIngestBytesPerDay), rig.used(c, quotas.DimStorageBytes); i != 60 || s != 60 {
			t.Fatalf("a stored 60-byte source charged ingestion %d and storage %d, want 60 and 60", i, s)
		}

		// Over the day (60 + 50 > 100) while the storage quota (1000) is far away: the refusal is the
		// ingestion charge's, in the transaction. Nothing is stored, nothing is charged, nothing needs withdrawing.
		files0 := rig.live(c)
		a := up(rig, 50, "rag_source")
		if a.code != http.StatusInsufficientStorage || a.err.Code != contracts.ErrResourceExhausted ||
			a.detail("dimension") != "ingest_bytes_per_day" || a.detail("limit") != float64(100) || a.detail("resets_at") != "2026-10-09T00:00:00Z" {
			t.Fatalf("an upload over the day's ingestion = %d %s", a.code, a.raw)
		}
		if i, s := rig.used(c, quotas.DimIngestBytesPerDay), rig.used(c, quotas.DimStorageBytes); i != 60 || s != 60 {
			t.Errorf("the refused source left ingestion %d and storage %d, want 60 and 60", i, s)
		}
		if got := rig.live(c); got != files0 {
			t.Errorf("the refused source left %d files, had %d", got, files0)
		}

		// An ordinary attachment is not ingestion, and the files service refuses by the storage charge when the
		// boundary cannot (a caller that is not the HTTP route: the core storing a generated file).
		if a := up(rig, 600, "ephemeral_context"); a.code != http.StatusCreated {
			t.Fatalf("an attachment = %d %s", a.code, a.raw)
		}
		if got := rig.used(c, quotas.DimIngestBytesPerDay); got != 60 {
			t.Errorf("an attachment was charged to ingestion: %d", got)
		}
		_, err := rig.svc.Put(context.Background(), c.scope(), files.PutInput{
			Name: "g.txt", DeclaredMIME: "text/plain", Origin: files.OriginGenerated, Content: strings.NewReader(strings.Repeat("g", 400)),
		})
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrResourceExhausted || ce.Details["dimension"] != "storage_bytes" {
			t.Fatalf("a direct Put over the storage quota = %v, want resource_exhausted for storage_bytes", err)
		}
		if got := rig.used(c, quotas.DimStorageBytes); got != 660 {
			t.Errorf("the refused Put left the storage gauge at %d, want 660", got)
		}

		// At the boundary a declared length that cannot fit is turned away before the handler runs.
		reached := rig.reached.Load()
		if a := up(rig, 500, "ephemeral_context"); a.code != http.StatusInsufficientStorage || a.detail("dimension") != "storage_bytes" {
			t.Fatalf("an upload that cannot fit = %d %s", a.code, a.raw)
		}
		if rig.reached.Load() != reached {
			t.Error("an upload over the storage quota reached its handler")
		}
		if got := rig.used(c, quotas.DimStorageBytes); got != 660 {
			t.Errorf("a boundary refusal moved the gauge to %d", got)
		}

		// Another tenant is untouched by all of it.
		if got := rig.used(quotaWireBob, quotas.DimStorageBytes) + rig.used(quotaWireBob, quotas.DimIngestBytesPerDay); got != 0 {
			t.Errorf("another tenant was charged %d", got)
		}

		// A confirmed commit is never refused for quota, with the volumes this full and the day's ingestion spent.
		reached = rig.reached.Load()
		if a := rig.do(c, http.MethodPost, "/v1/actions/prep_1/confirm", `{}`); a.code != http.StatusOK || rig.reached.Load() != reached+1 {
			t.Errorf("a confirmation = %d %s", a.code, a.raw)
		}

		// A delete gives the storage back (the gauge is set to what the service holds); the day stays spent.
		if r := rig.do(c, http.MethodDelete, "/v1/files/"+idOf(first), ""); r.code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", r.code, r.raw)
		}
		if i, s := rig.used(c, quotas.DimIngestBytesPerDay), rig.used(c, quotas.DimStorageBytes); i != 60 || s != 600 {
			t.Errorf("after a delete ingestion is %d and storage %d, want 60 and 600", i, s)
		}
	})

	t.Run("the build's own RAG seam", func(t *testing.T) {
		rig := quotaTxNewRig(t, true)
		// attachRAGSources refuses a file source: the upload is refused after the transaction committed, the file
		// is withdrawn and the charges made for it are given back. Nothing is left reserved.
		a := up(rig, 80, "rag_source")
		if a.code == http.StatusCreated || a.err.Code != contracts.ErrUnavailable {
			t.Fatalf("a source this build cannot register = %d %s", a.code, a.raw)
		}
		if i, s := rig.used(c, quotas.DimIngestBytesPerDay), rig.used(c, quotas.DimStorageBytes); i != 0 || s != 0 {
			t.Errorf("a refused source left ingestion %d and storage %d charged, want 0 and 0", i, s)
		}
		if got := rig.live(c); got != 0 {
			t.Errorf("a refused source left %d live files", got)
		}
		// The tenant's day was not spent by it: the same bytes are not refused for quota afterwards.
		if a := up(rig, 90, "ephemeral_context"); a.code != http.StatusCreated {
			t.Fatalf("an attachment after the refused source = %d %s", a.code, a.raw)
		}
		id := idOf(up(rig, 100, "ephemeral_context"))
		if got := rig.used(c, quotas.DimStorageBytes); got != 190 {
			t.Fatalf("storage after two attachments = %d", got)
		}
		if r := rig.do(c, http.MethodDelete, "/v1/files/"+id, ""); r.code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", r.code, r.raw)
		}
		if got := rig.used(c, quotas.DimStorageBytes); got != 90 {
			t.Errorf("storage after a delete = %d, want 90", got)
		}
	})

	t.Run("the adapter", func(t *testing.T) {
		rig := quotaTxNewRig(t, false)
		ch := quotaTxChargerFor(rig.db)
		if ch == nil {
			t.Fatal("the quotas are not the charger of the store")
		}
		ic := ingest.ChargerOf(rig.db)
		if ic == nil {
			t.Fatal("the ingestion is not given the quotas as its charger")
		}
		ctx := context.Background()
		// A charge is a write of the transaction it is made in: it goes when the transaction does.
		boom := errors.New("the data write failed")
		err := rig.db.Tx(ctx, func(tx *store.Tx) error {
			if err := ic.ChargeIngestTx(ctx, tx, c.scope(), 70); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("tx = %v", err)
		}
		if got := rig.used(c, quotas.DimIngestBytesPerDay); got != 0 {
			t.Fatalf("a charge outlived its transaction: %d", got)
		}
		// And it is a charge: committed, it counts; over the day, the refusal names the window.
		if err := rig.db.Tx(ctx, func(tx *store.Tx) error { return ic.ChargeIngestTx(ctx, tx, c.scope(), 70) }); err != nil {
			t.Fatal(err)
		}
		if got := rig.used(c, quotas.DimIngestBytesPerDay); got != 70 {
			t.Fatalf("a committed charge counts %d, want 70", got)
		}
		err = rig.db.Tx(ctx, func(tx *store.Tx) error { return ic.ChargeIngestTx(ctx, tx, c.scope(), 31) })
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Code != contracts.ErrResourceExhausted || ce.Details["resets_at"] != "2026-10-09T00:00:00Z" {
			t.Fatalf("a charge over the day = %v", err)
		}
		err = rig.db.Tx(ctx, func(tx *store.Tx) error { return ch.ChargeTx(ctx, tx, c.scope(), files.ChargeKind("bogus"), 1) })
		if contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("an unknown kind = %v, want validation_failed", err)
		}
	})
}
