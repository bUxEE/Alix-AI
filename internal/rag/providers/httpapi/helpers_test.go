package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const (
	testApp    = "demo-library"
	testTenant = "tenant-1"
	testWS     = "ws-1"
)

var (
	clockStart = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	oldTime    = time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
)

// syncBuf is a goroutine-safe buffer for the logger under test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeItem is one document of the fake application.
type fakeItem struct {
	body        []byte
	version     string
	etag        string // "" means the version; "-" sends no ETag header
	mod         time.Time
	hideSize    bool
	hideMod     bool
	deleted     bool // listed as a deletion marker; the item endpoint answers 404
	contentType string
}

// rec is one request the fake application saw.
type rec struct {
	method string
	path   string
	raw    string // the request URI as it arrived
	query  url.Values
	header http.Header
}

// fakeAPI is an application that follows the API contract of the package
// comment, on a loopback port.
type fakeAPI struct {
	t    *testing.T
	srv  *httptest.Server
	root string

	mu      sync.Mutex
	items   map[string]*fakeItem
	reqs    []rec
	seq     int
	perPage int

	// token, when set, is the bearer credential the application requires.
	token string
	// listHook and itemHook can take over a response. They are called without
	// the lock held; page is the 1-based number of the list page asked for.
	listHook func(w http.ResponseWriter, r *http.Request, page int) bool
	itemHook func(w http.ResponseWriter, r *http.Request, key string) bool
	// curEnc and curDec replace the cursor format.
	curEnc func(offset int) string
	curDec func(cursor string) (int, bool)
}

func newFakeAPI(t *testing.T, root string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, root: root, items: map[string]*fakeItem{}, perPage: 2}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) rootURL() string { return f.srv.URL + f.root }

func (f *fakeAPI) port() int {
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fakeAPI) put(key, body string) *fakeItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	it := &fakeItem{body: []byte(body), version: fmt.Sprintf("v%d", f.seq), mod: oldTime}
	f.items[key] = it
	return it
}

// update changes an item's content and bumps its version and time.
func (f *fakeAPI) update(key, body string, mod time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	it := f.items[key]
	it.body, it.version, it.mod = []byte(body), fmt.Sprintf("v%d", f.seq), mod
}

func (f *fakeAPI) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, key)
}

func (f *fakeAPI) mark(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[key].deleted = true
}

func (f *fakeAPI) requests() []rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rec(nil), f.reqs...)
}

func (f *fakeAPI) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

// count is the number of requests with the method whose path is below the root
// (items) or is the root itself (list) according to items.
func (f *fakeAPI) count(method string, items bool) int {
	n := 0
	for _, r := range f.requests() {
		isItem := strings.HasPrefix(r.path, f.root+"/") && r.path != f.root+"/"
		if r.method == method && isItem == items {
			n++
		}
	}
	return n
}

type wireOut struct {
	Key        string `json:"key"`
	Version    string `json:"version"`
	Size       *int64 `json:"size,omitempty"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Deleted    bool   `json:"deleted,omitempty"`
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, rec{method: r.Method, path: r.URL.Path, raw: r.URL.RequestURI(), query: r.URL.Query(), header: r.Header.Clone()})
	token := f.token
	f.mu.Unlock()
	if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == f.root || (f.root == "" && r.URL.Path == "/"):
		f.list(w, r)
	case strings.HasPrefix(r.URL.Path, f.root+"/"):
		f.item(w, r, strings.TrimPrefix(r.URL.Path, f.root+"/"))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) list(w http.ResponseWriter, r *http.Request) {
	offset := 0
	if c := first(r.URL.Query(), "cursor", "after"); c != "" {
		n, ok := f.decodeCursor(c)
		if !ok {
			http.Error(w, "bad cursor", http.StatusBadRequest)
			return
		}
		offset = n
	}
	if f.listHook != nil && f.listHook(w, r, offset/max(f.perPage, 1)+1) {
		return
	}
	f.mu.Lock()
	keys := make([]string, 0, len(f.items))
	for k := range f.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	per := f.perPage
	if l, err := strconv.Atoi(first(r.URL.Query(), "limit", "page_size")); err == nil && l > 0 && l < per {
		per = l
	}
	end := min(offset+per, len(keys))
	var out struct {
		Items      []wireOut `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}
	out.Items = []wireOut{}
	for _, k := range keys[min(offset, len(keys)):end] {
		it := f.items[k]
		e := wireOut{Key: k, Version: it.version, Deleted: it.deleted}
		if !it.hideSize {
			n := int64(len(it.body))
			e.Size = &n
		}
		if !it.hideMod {
			e.ModifiedAt = it.mod.UTC().Format(time.RFC3339)
		}
		out.Items = append(out.Items, e)
	}
	if end < len(keys) {
		c := f.encodeCursor(end)
		out.NextCursor = &c
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeAPI) item(w http.ResponseWriter, r *http.Request, key string) {
	if f.itemHook != nil && f.itemHook(w, r, key) {
		return
	}
	f.mu.Lock()
	it, ok := f.items[key]
	var cp fakeItem
	if ok {
		cp = *it
	}
	f.mu.Unlock()
	if !ok || cp.deleted {
		http.NotFound(w, r)
		return
	}
	switch cp.etag {
	case "":
		w.Header().Set("ETag", `"`+cp.version+`"`)
	case "-":
	default:
		w.Header().Set("ETag", cp.etag)
	}
	w.Header().Set("Last-Modified", cp.mod.UTC().Format(http.TimeFormat))
	if cp.contentType != "" {
		w.Header().Set("Content-Type", cp.contentType)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(cp.body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(cp.body)
}

// allowlistFor allows exactly the fake API's loopback host and port over plain
// http: the egress entry a loopback endpoint needs.
func allowlistFor(t *testing.T, ports ...int) *egress.Allowlist {
	t.Helper()
	al, err := egress.NewAllowlist(egress.Destination{Host: "127.0.0.1", Ports: ports, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	return al
}

func baseDecl(rootURL string) ap.RAGSource {
	u := ap.HTTPURL(rootURL)
	return ap.RAGSource{
		ID:    "api-docs",
		Type:  ap.RAGSourceTypeApplicationDocumentAPI,
		Owner: ap.RAGSourceOwner{Name: "Test owner"},
		Scope: ap.RAGSourceScope{
			ApplicationID: testApp,
			Tenant:        ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
			Workspace:     ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
		},
		Root:     ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Kind: "api", URL: &u}},
		Include:  []ap.GlobPattern{"**"},
		Exclude:  []ap.GlobPattern{},
		Formats:  []ap.MimeType{source.MIMEText, source.MIMEMarkdown, source.MIMEJSON},
		Limits:   ap.RAGSourceLimits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 30, MaxFiles: 10000},
		Sync:     ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodManual},
		Deletion: ap.RAGSourceDeletion{PurgeAfterHours: 24, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedPurge},
		Egress:   ap.RAGSourceEgress{Entries: []ap.Slug{"app-api"}},
	}
}

func scope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: testApp, TenantID: testTenant, WorkspaceID: testWS,
		PrincipalID: "svc-sync", Roles: []string{"sync"}, CapabilityScopes: []string{"rag.sync"}, SessionID: "sess-1",
	}
}

// harness is a provider on a fake clock, aimed at a fake API and allowed to
// reach exactly that API.
type harness struct {
	t    *testing.T
	api  *fakeAPI
	reg  *egress.Registry
	clk  *clock.Fake
	logs *syncBuf
	p    *Provider
	ctx  context.Context
}

func newHarness(t *testing.T, root string, cfg ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, api: newFakeAPI(t, root), clk: clock.NewFake(clockStart), logs: &syncBuf{}, ctx: context.Background()}
	h.reg = egress.NewRegistry()
	if err := h.reg.Set(testApp, allowlistFor(t, h.api.port())); err != nil {
		t.Fatal(err)
	}
	c := Config{
		Clock:  h.clk,
		Egress: h.reg,
		Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, f := range cfg {
		f(&c)
	}
	p, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	return h
}

func (h *harness) def(mutate ...func(*ap.RAGSource)) source.Definition {
	h.t.Helper()
	d := baseDecl(h.api.rootURL())
	for _, m := range mutate {
		m(&d)
	}
	def, err := source.FromDeclaration(d, source.Binding{SourceID: ids.New(source.IDPrefix), TenantID: testTenant, WorkspaceID: testWS})
	if err != nil {
		h.t.Fatal(err)
	}
	return def
}

func (h *harness) discover(def source.Definition, prev []source.Record) source.ChangeSet {
	h.t.Helper()
	cs, err := h.p.Discover(h.ctx, scope(), def, prev)
	if err != nil {
		h.t.Fatalf("Discover: %v", err)
	}
	return cs
}

func keysOf(items []source.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Key)
	}
	sort.Strings(out)
	return out
}

func recordKeys(rs []source.Record) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Key)
	}
	sort.Strings(out)
	return out
}

func tombstoneReasons(ts []source.Tombstone) map[string]source.Reason {
	out := map[string]source.Reason{}
	for _, k := range ts {
		out[k.Key] = k.Reason
	}
	return out
}

func skipReasons(s []source.Skip) map[string]source.Reason {
	out := map[string]source.Reason{}
	for _, k := range s {
		out[k.Key] = k.Reason
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// defBound is def with an explicit registration binding, for a source whose
// tenant or workspace level is shared.
func (h *harness) defBound(b source.Binding, mutate ...func(*ap.RAGSource)) source.Definition {
	h.t.Helper()
	d := baseDecl(h.api.rootURL())
	for _, m := range mutate {
		m(&d)
	}
	b.SourceID = ids.New(source.IDPrefix)
	def, err := source.FromDeclaration(d, b)
	if err != nil {
		h.t.Fatal(err)
	}
	return def
}

// noChange asserts that a ChangeSet carries nothing a caller could apply.
func noChange(t *testing.T, cs source.ChangeSet) {
	t.Helper()
	if len(cs.Added)+len(cs.Modified)+len(cs.Deleted)+len(cs.Next) != 0 {
		t.Errorf("a scan that failed must carry no change, got added=%d modified=%d deleted=%d next=%d",
			len(cs.Added), len(cs.Modified), len(cs.Deleted), len(cs.Next))
	}
}

// encodeCursor and decodeCursor are the fake's opaque cursors: the offset of
// the next page, base64 by default, or whatever a test plugs in.
func (f *fakeAPI) encodeCursor(offset int) string {
	if f.curEnc != nil {
		return f.curEnc(offset)
	}
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

func (f *fakeAPI) decodeCursor(c string) (int, bool) {
	if f.curDec != nil {
		return f.curDec(c)
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(raw), "o:") {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw[2:]))
	return n, err == nil
}

// setVersion replaces an item's listed version (and so its ETag).
func (f *fakeAPI) setVersion(key, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[key].version = version
}

// first returns the first non-empty value among the named parameters.
func first(q url.Values, names ...string) string {
	for _, n := range names {
		if v := q.Get(n); v != "" {
			return v
		}
	}
	return ""
}
