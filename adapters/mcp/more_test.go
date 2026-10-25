package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// adapters/mcp is tier 4 of docs/architecture/module-map.md: it may import the
// base packages, internal/adapters and internal/egress. It never imports an
// application package, another concrete adapter or LocalAI.
func TestImportsStayWithinTheModuleMap(t *testing.T) {
	const modulePath = "github.com/buxee/alix-ai/"
	allowed := map[string]bool{
		"pkg/contracts": true, "internal/clock": true, "internal/ids": true, "internal/config": true, "internal/log": true,
		"internal/version": true, "internal/egress": true, "internal/adapters": true,
	}
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		isTest := strings.HasSuffix(path, "_test.go") || strings.Contains(filepath.ToSlash(path), "testdata/")
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, modulePath) {
				continue
			}
			rel := strings.TrimPrefix(p, modulePath)
			if strings.HasPrefix(rel, "pkg/contracts/gen/") || rel == "adapters/mcp" || strings.HasPrefix(rel, "adapters/mcp/testdata/") {
				continue
			}
			if isTest && (strings.HasPrefix(rel, "internal/adapters/") || strings.HasPrefix(rel, "tests/conformance/")) {
				continue // the fake application and the shared suite
			}
			if !allowed[rel] {
				t.Errorf("%s imports %s, which module-map.md does not allow for adapters/mcp", path, rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 10 {
		t.Fatalf("scanned only %d files", files)
	}
}

func TestParseAdapterConfig(t *testing.T) {
	ok, err := ParseAdapterConfig([]byte("# comment\nserver: clinic-mcp\ntools:\n  allow:\n    - b_tool\n    - a_tool\n"), "clinic-mcp")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"a_tool", "b_tool"}, ok.Tools.Allow); diff != "" {
		t.Errorf("allow (-want +got):\n%s", diff)
	}
	empty, err := ParseAdapterConfig([]byte("server: clinic-mcp\n"), "clinic-mcp")
	if err != nil || empty.Tools.Allow == nil || len(empty.Tools.Allow) != 0 {
		t.Errorf("no tools: %+v, %v: want a non-nil empty list, so nothing is approved", empty, err)
	}
	if c, err := ParseAdapterConfig([]byte("tools:\n  allow: [x]\n"), "any-endpoint"); err != nil || len(c.Tools.Allow) != 1 {
		t.Errorf("a configuration that does not name its server: %+v, %v", c, err)
	}
	for _, bad := range []string{
		"server: other-mcp\ntools:\n  allow: [x]\n",
		"tools:\n  allow: [x]\nunknown: 1\n",
		"tools:\n  allow: [x]\n  deny: [y]\n",
		"tools:\n  allow: [x, x]\n",
		"tools:\n  allow: ['a b']\n",
		"tools:\n  allow: [\"x\\ny\"]\n",
		"tools: [unterminated",
		"tools:\n  allow: [x]\n---\ntools:\n  allow: [y]\n",
	} {
		if _, err := ParseAdapterConfig([]byte(bad), "clinic-mcp"); err == nil || contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%q was accepted: %v", bad, err)
		}
	}

	// The reference application's own configuration is in the format.
	raw, err := os.ReadFile("../../examples/reference-apps/clinic-booking/package/schemas/adapters/clinic-mcp.yaml")
	if err != nil {
		t.Fatal(err)
	}
	clinic, err := ParseAdapterConfig(raw, "clinic-mcp")
	if err != nil {
		t.Fatalf("the clinic adapter configuration: %v", err)
	}
	for _, want := range []string{"resolve_patient", "appointments_book_commit", "patients_find"} {
		if !slices.Contains(clinic.Tools.Allow, want) {
			t.Errorf("the clinic configuration lacks %q", want)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	good := func() Config {
		return Config{
			ApplicationID: testApp,
			Endpoints:     []Endpoint{{ID: "ep", URL: "http://127.0.0.1:1/mcp"}},
			Capabilities: []Capability{{ID: "cap.one", Operations: map[adapters.Operation]Binding{
				adapters.OpRead: {Endpoint: "ep", Tool: "tool_one"}}}},
		}
	}
	if _, err := New(good()); err != nil {
		t.Fatalf("the base configuration: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"no application", func(c *Config) { c.ApplicationID = "" }},
		{"no endpoint", func(c *Config) { c.Endpoints = nil }},
		{"an endpoint ID that is malformed", func(c *Config) { c.Endpoints[0].ID = "Bad ID" }},
		{"an endpoint declared twice", func(c *Config) { c.Endpoints = append(c.Endpoints, c.Endpoints[0]) }},
		{"neither a URL nor a command", func(c *Config) { c.Endpoints[0].URL = "" }},
		{"both a URL and a command", func(c *Config) { c.Endpoints[0].Command = &Command{Path: os.Args[0]} }},
		{"a relative command", func(c *Config) { c.Endpoints[0].URL, c.Endpoints[0].Command = "", &Command{Path: "server"} }},
		{"a URL with user information", func(c *Config) { c.Endpoints[0].URL = "http://user:pass@127.0.0.1:1/mcp" }},
		{"a URL that is not http", func(c *Config) { c.Endpoints[0].URL = "ftp://127.0.0.1/mcp" }},
		{"a URL with no host", func(c *Config) { c.Endpoints[0].URL = "http:///mcp" }},
		{"a bad contract hash", func(c *Config) { c.Endpoints[0].ContractSHA256 = "ABC" }},
		{"a mapped tool of an unknown endpoint", func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: "nope", Tool: "tool_one"}
		}},
		{"a tool name with a space", func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: "ep", Tool: "tool one"}
		}},
		{"an operation that returns no result", func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpHealth] = Binding{Endpoint: "ep", Tool: "h"}
		}},
		{"a negative timeout", func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: "ep", Tool: "t", Timeout: -1}
		}},
		{"two fields renamed to one argument", func(c *Config) {
			c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: "ep", Tool: "t", Arguments: map[string]string{"a": "x", "b": "x"}}
		}},
		{"a capability declared twice", func(c *Config) { c.Capabilities = append(c.Capabilities, c.Capabilities[0]) }},
		{"a wildcard capability", func(c *Config) { c.Capabilities[0].ID = "*" }},
		{"a pin for an unknown endpoint", func(c *Config) { c.Pins = map[string]string{"nope/tool": strings.Repeat("a", 64)} }},
		{"a malformed pin", func(c *Config) { c.Pins = map[string]string{"ep/tool": "xyz"} }},
		{"an allow list that lacks a mapped tool", func(c *Config) { c.Endpoints[0].Allow = []string{"another"} }},
		{"a malformed management tool", func(c *Config) { c.DescribeTool = "no good" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mutate(&c)
			a, err := New(c)
			if err == nil {
				_ = a.Close()
				t.Fatal("the configuration was accepted")
			}
			if contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("error = %v, want validation_failed", err)
			}
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Errorf("the cause is not a *ConfigError: %v", err)
			}
		})
	}
}

// The pins are canonical: key order and whitespace do not matter, a changed schema
// does, and the description does not.
func TestToolHashAndContractSHA256(t *testing.T) {
	a := map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}, "a": map[string]any{"type": "integer"}}}
	var b any
	if err := json.Unmarshal([]byte(`{"properties":{"a":{"type":"integer"},"b":{"type":"string"}},   "type":"object"}`), &b); err != nil {
		t.Fatal(err)
	}
	h1, _ := toolHash("t", a, nil)
	h2, _ := toolHash("t", b, nil)
	if h1 != h2 {
		t.Error("the hash depends on the order of the members of a schema")
	}
	if h3, _ := toolHash("u", a, nil); h3 == h1 {
		t.Error("the hash ignores the tool's name")
	}
	if h4, _ := toolHash("t", a, map[string]any{"type": "object"}); h4 == h1 {
		t.Error("the hash ignores the output schema")
	}
	if h5, _ := toolHash("t", map[string]any{"type": "object"}, nil); h5 == h1 {
		t.Error("the hash ignores the input schema")
	}

	// The manifest pin is the SHA-256 of this exact document: sorted tools, each
	// with its name and input schema, members in sorted order, no whitespace.
	got, err := ContractSHA256([]ContractTool{
		{Name: "b_tool", InputSchema: map[string]any{"type": "object", "required": []any{"x"}}},
		{Name: "a_tool", InputSchema: map[string]any{"type": "object"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`[{"input_schema":{"type":"object"},"name":"a_tool"},{"input_schema":{"required":["x"],"type":"object"},"name":"b_tool"}]`))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("ContractSHA256 = %s, want %s", got, want)
	}
	// HTML is not escaped, as in RFC 8785.
	h, _ := canonicalJSON(map[string]any{"a": "<&>"})
	if string(h) != `{"a":"<&>"}` {
		t.Errorf("canonical JSON = %s", h)
	}
}

// Entity resolution is a Read whose fields carry entity_type; the adapter sends it
// to the resolver the capability declares, with the fields untouched, and to no
// other tool.
func TestEntityResolutionIsRoutedToTheDeclaredResolver(t *testing.T) {
	app := fake.NewApp()
	srv := fixture.New(app)
	h := fixture.StartHTTP(t, srv)
	var mu sync.Mutex
	var resolverArgs []json.RawMessage
	resolve := func(name string) {
		srv.Add(fixture.Tool{Name: name, Input: objectSchema("identity", "fields"), Output: objectSchema("status")}, func(_ context.Context, args json.RawMessage) fixture.Result {
			mu.Lock()
			resolverArgs = append(resolverArgs, args)
			mu.Unlock()
			return fixture.FromBody([]byte(`{"status":"ok","identifiers":[{"entity_type":"patient","entity_id":"pat_1"},{"entity_type":"patient","entity_id":"pat_2"}],`+
				`"values":[{"field":"label","value":{"kind":"text","value":"Maria Rossi"}},{"field":"label","value":{"kind":"text","value":"Maria Rosso"}}],`+
				`"errors":[],"allowed_next_actions":[],"freshness":{"as_of":"2026-10-09T08:00:00Z","version":"v7"}}`), "")
		})
	}
	resolve("resolve_patient")
	resolve("resolve_slot")

	cfg := httpConfig(t, h)
	cfg.Capabilities = append(cfg.Capabilities, Capability{
		ID: "bookings.book",
		Operations: map[adapters.Operation]Binding{
			adapters.OpValidate: {Endpoint: testEP, Tool: fixture.ToolValidate},
		},
		Resolve: map[string]Binding{
			"patient": {Endpoint: testEP, Tool: "resolve_patient"},
			"slot":    {Endpoint: testEP, Tool: "resolve_slot"},
		},
	}, Capability{
		// A capability that is both read and resolver.
		ID:         "bookings.list",
		Operations: map[adapters.Operation]Binding{adapters.OpRead: {Endpoint: testEP, Tool: fixture.ToolGet}},
		Resolve:    map[string]Binding{"patient": {Endpoint: testEP, Tool: "resolve_patient"}},
	})
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx := testContext(t)
	scope := testScope()
	scope.CapabilityScopes = append(scope.CapabilityScopes, "bookings.book", "bookings.list")

	fields := map[string]any{"entity_type": "patient", "mention": "Maria", "candidate_id": "pat_9"}
	res, err := a.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "bookings.book", Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != actions.DeterministicResultStatusOk || len(res.Identifiers) != 2 || res.Freshness == nil || res.Freshness.Version != "v7" {
		t.Errorf("the candidates did not come back as the host answered: %+v", res)
	}
	if res.Operation != actions.DeterministicResultOperationRead || res.CapabilityID != "bookings.book" {
		t.Errorf("the result is for %s %s", res.Operation, res.CapabilityID)
	}
	mu.Lock()
	if len(resolverArgs) != 1 {
		t.Fatalf("the resolver was called %d times", len(resolverArgs))
	}
	var sent struct {
		Identity map[string]any `json:"identity"`
		Fields   map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(resolverArgs[0], &sent); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	if diff := cmp.Diff(fields, sent.Fields); diff != "" {
		t.Errorf("the reserved arguments did not travel in fields untouched (-want +got):\n%s", diff)
	}
	if sent.Identity["principal_id"] != "usr_alice" {
		t.Errorf("identity = %v", sent.Identity)
	}

	// The other entity type goes to its own resolver.
	if _, err := a.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "bookings.book", Fields: map[string]any{"entity_type": "slot", "mention": "next available"}}); err != nil {
		t.Fatal(err)
	}
	if srv.ToolCalls("resolve_slot") != 1 || srv.ToolCalls("resolve_patient") != 1 {
		t.Errorf("resolver calls: slot %d, patient %d", srv.ToolCalls("resolve_slot"), srv.ToolCalls("resolve_patient"))
	}

	// With entity_type, the resolver; without it, the read tool.
	if _, err := a.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "bookings.list", Fields: map[string]any{"item_id": "item_1"}}); err != nil {
		t.Fatal(err)
	}
	if srv.ToolCalls(fixture.ToolGet) != 1 {
		t.Error("a Read without entity_type did not reach the read tool")
	}
	if _, err := a.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "bookings.list", Fields: map[string]any{"entity_type": "patient", "mention": "x"}}); err != nil {
		t.Fatal(err)
	}
	if srv.ToolCalls(fixture.ToolGet) != 1 || srv.ToolCalls("resolve_patient") != 2 {
		t.Error("a Read with entity_type reached the read tool")
	}

	// A resolver-only capability has no read, and does not claim one.
	if _, err := a.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "bookings.book", Fields: map[string]any{"item_id": "x"}}); contracts.CodeOf(err) != contracts.ErrUnknownCapability {
		t.Errorf("a plain Read of a capability that only resolves: %v, want unknown_capability", err)
	}
	d, err := a.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Supports("bookings.book", adapters.OpRead) {
		t.Error("Describe lists read for a capability that maps none")
	}
	if !d.Supports("bookings.book", adapters.OpValidate) || !d.Supports("bookings.list", adapters.OpRead) {
		t.Errorf("Describe = %+v", d.Capabilities)
	}
	// An entity type that is not declared, and a malformed one, are refused before anything is sent.
	before := srv.ToolCalls("")
	for _, f := range []map[string]any{
		{"entity_type": "doctor", "mention": "x"},
		{"entity_type": "", "mention": "x"},
		{"entity_type": 7, "mention": "x"},
		{"entity_type": true},
	} {
		if _, err := a.Read(ctx, scope, adapters.ReadRequest{CapabilityID: "bookings.book", Fields: f}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("entity_type %v: %v, want validation_failed", f["entity_type"], err)
		}
	}
	if srv.ToolCalls("") != before {
		t.Error("a malformed resolution request reached the server")
	}
}

func TestArgumentRenamesAndCancel(t *testing.T) {
	var got json.RawMessage
	a, h, _ := newHTTPAdapter(t, func(c *Config) {
		c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, Tool: fixture.ToolGet, Arguments: map[string]string{"item": "item_id"}}
		// The cancel mapping is the host's: this configuration has none.
		delete(c.Capabilities[1].Operations, adapters.OpCancel)
	})
	h.Handle(fixture.ToolGet, func(_ context.Context, args json.RawMessage) fixture.Result {
		got = args
		return fixture.FromBody([]byte(validResultJSON), "")
	})
	ctx := testContext(t)
	if _, err := a.Read(ctx, testScope(), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item": "item_1"}}); err != nil {
		t.Fatal(err)
	}
	var sent struct{ Fields map[string]any }
	if err := json.Unmarshal(got, &sent); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]any{"item_id": "item_1"}, sent.Fields); diff != "" {
		t.Errorf("the renamed argument (-want +got):\n%s", diff)
	}
	if _, err := a.Read(ctx, testScope(), adapters.ReadRequest{CapabilityID: fake.ReadCapability, Fields: map[string]any{"item": "a", "item_id": "b"}}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("two fields for one argument: %v, want validation_failed", err)
	}

	before := h.Requests()
	_, err := a.Cancel(ctx, testScope(), adapters.CancelRequest{CapabilityID: fake.ActionCapability, IdempotencyKey: "idem_0000000000000000000001"})
	if contracts.CodeOf(err) != contracts.ErrUnknownCapability || detail(err, "reason") != ReasonOperationNotMapped {
		t.Errorf("Cancel with no cancel tool mapped: %v, want unknown_capability", err)
	}
	if h.Requests() != before {
		t.Error("an unmapped Cancel reached the server")
	}
	d, err := a.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Supports(fake.ActionCapability, adapters.OpCancel) {
		t.Error("Describe lists cancel, which is not mapped")
	}
}

// The application restarts: the adapter opens a new session by itself.
func TestSessionIsOpenedAgainAfterTheServerRestarts(t *testing.T) {
	a, h, app := newHTTPAdapter(t)
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	w := wrapped(t, a)
	ctx := testContext(t)
	if _, err := readItem(ctx, w, "item_1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h.Down()
		a.closeIdleConnections()
		if _, err := readItem(ctx, w, "item_1"); contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Fatalf("read of a server that is down: %v, want unavailable", err)
		}
		if hr, err := w.Health(ctx); err != nil || hr.Status != adapters.HealthUnavailable {
			t.Fatalf("Health of a server that is down: %+v, %v", hr, err)
		}
		h.Up(t)
		if res, err := readItem(ctx, w, "item_1"); err != nil || res.Status != actions.DeterministicResultStatusOk {
			t.Fatalf("read after the restart: %+v, %v", res, err)
		}
	}
}

func TestToolListsArePaged(t *testing.T) {
	a, h, app := newHTTPAdapter(t)
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	h.SetPageSize(2)
	for i := 0; i < 5; i++ {
		name := "extra_" + strconv.Itoa(i)
		h.Add(fixture.Tool{Name: name, Input: objectSchema()}, func(context.Context, json.RawMessage) fixture.Result { return fixture.Result{} })
	}
	ctx := testContext(t)
	d, err := a.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Supports(fake.ReadCapability, adapters.OpRead) || !d.Supports(fake.ActionCapability, adapters.OpCommit) {
		t.Errorf("a tool on a later page was not seen: %+v", d.Capabilities)
	}
	if len(d.Unmapped) != 5 {
		t.Errorf("unmapped = %v, want the 5 extra tools", d.Unmapped)
	}
	if _, err := readItem(ctx, a, "item_1"); err != nil {
		t.Error(err)
	}

	// A server with an unreasonable number of tools is refused, not truncated.
	h.SetPageSize(0)
	for i := 0; i < maxListedTools+1; i++ {
		h.Server.AddDuplicate(fixture.Tool{Name: "bulk_" + strconv.Itoa(i), Input: objectSchema()})
	}
	if _, err := a.Describe(ctx); contracts.CodeOf(err) != contracts.ErrSchemaInvalid && contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("Describe of a server with %d tools: %v, want a refusal", maxListedTools+6, err)
	}
}

func TestBindingTimeout(t *testing.T) {
	a, h, _ := newHTTPAdapter(t, func(c *Config) {
		c.Capabilities[0].Operations[adapters.OpRead] = Binding{Endpoint: testEP, Tool: fixture.ToolGet, Timeout: 150 * time.Millisecond}
	})
	h.Handle(fixture.ToolGet, func(context.Context, json.RawMessage) fixture.Result { return fixture.Result{Hang: true} })
	w := wrapped(t, a)
	start := time.Now()
	_, err := readItem(testContext(t), w, "item_1")
	if contracts.CodeOf(err) != contracts.ErrTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("read = %v, want timeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the operation's timeout took %s", d)
	}
}

// A server that has none of the management tools is still usable.
func TestServerWithoutManagementTools(t *testing.T) {
	a, h, app := newHTTPAdapter(t)
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	for _, n := range []string{fixture.ToolDescribe, fixture.ToolHealth, fixture.ToolInspect} {
		h.Remove(n)
	}
	h.SetVersion("2.4.1")
	ctx := testContext(t)
	d, err := a.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.ContractVersions, []int{adapters.ContractVersion}) || d.ApplicationVersion != "2.4.1" {
		t.Errorf("description = %+v", d)
	}
	if len(d.Unmapped) != 0 {
		t.Errorf("unmapped = %v", d.Unmapped)
	}
	hr, err := a.Health(ctx)
	if err != nil || hr.Status != adapters.HealthOK || hr.ApplicationVersion != "2.4.1" {
		t.Errorf("Health = %+v, %v, want ok from a ping", hr, err)
	}
	in, err := a.Inspect(ctx, testScope(), adapters.InspectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Unverified()) != len(adapters.AllAspects()) {
		t.Errorf("a server with no inspect tool reported facts: %+v", in)
	}
	if _, err := readItem(ctx, a, "item_1"); err != nil {
		t.Error(err)
	}
	h.Down()
	a.closeIdleConnections()
	if hr, err := wrapped(t, a).Health(ctx); err != nil || hr.Status != adapters.HealthUnavailable {
		t.Errorf("Health of a server that is down = %+v, %v", hr, err)
	}
}

func TestCloseIsIdempotentAndEndsCalls(t *testing.T) {
	a, h, _ := newHTTPAdapter(t)
	ctx := testContext(t)
	if _, err := a.Describe(ctx); err != nil {
		t.Fatal(err)
	}
	h.Handle(fixture.ToolGet, func(c context.Context, _ json.RawMessage) fixture.Result { return fixture.Result{Hang: true} })
	done := make(chan error, 1)
	go func() {
		_, err := readItem(ctx, a, "item_1")
		done <- err
	}()
	if !fixture.Wait(5*time.Second, func() bool { return h.ToolCalls(fixture.ToolGet) == 1 }) {
		t.Fatal("the call never reached the server")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("a call in flight at Close succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a call in flight at Close did not end")
	}
	if _, err := readItem(ctx, a, "item_1"); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("a call after Close: %v, want unavailable", err)
	}
	if _, err := a.Describe(ctx); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("Describe after Close: %v, want unavailable", err)
	}
}

// Parallel callers share one session and one listing of the tools.
func TestConcurrentCallsShareOneSession(t *testing.T) {
	a, h, app := newHTTPAdapter(t, func(c *Config) { c.SurfaceRecheck = time.Hour })
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	ctx := testContext(t)
	var wg sync.WaitGroup
	var failures atomic.Int64
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := readItem(ctx, a, "item_1"); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d of 24 parallel reads failed", failures.Load())
	}
	count := func(method string) int {
		n := 0
		for _, e := range h.Log() {
			if e.Method == method {
				n++
			}
		}
		return n
	}
	if n := count("initialize"); n != 1 {
		t.Errorf("%d sessions were opened for 24 parallel calls, want 1", n)
	}
	if n := count("tools/list"); n != 1 {
		t.Errorf("the tools were listed %d times, want once", n)
	}
	if n := h.ToolCalls(fixture.ToolGet); n != 24 {
		t.Errorf("%d tool calls, want 24", n)
	}
}

// The text of a JSON-RPC error is the server's, not the adapter's.
func TestServerTextNeverReachesAnError(t *testing.T) {
	a, h, _ := newHTTPAdapter(t)
	w := wrapped(t, a)
	ctx := testContext(t)
	p := prepareItem(ctx, t, w, "Widget")
	for _, tool := range []string{fixture.ToolGet, fixture.ToolCommit} {
		h.Handle(tool, func(context.Context, json.RawMessage) fixture.Result {
			return fixture.Result{RPCError: &fixture.RPCError{Code: -32000, Message: injection}}
		})
	}
	_, err := readItem(ctx, w, "item_1")
	if contracts.CodeOf(err) != contracts.ErrUnavailable || strings.Contains(err.Error(), "SYSTEM OVERRIDE") {
		t.Errorf("read = %v, want unavailable with no server text", err)
	}
	_, err = commitItem(ctx, w, p)
	if contracts.CodeOf(err) != contracts.ErrUncertainOutcome || strings.Contains(err.Error(), "SYSTEM OVERRIDE") {
		t.Errorf("commit = %v, want uncertain_outcome with no server text", err)
	}
}

// A hostile server may ask the client for things; the adapter gives none.
func TestServerRequestsOfTheClientAreRefused(t *testing.T) {
	app := fake.NewApp()
	srv := fixture.New(app)
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	cfg := Config{
		ApplicationID:    testApp,
		Endpoints:        []Endpoint{{ID: testEP, Command: &Command{Path: os.Args[0]}}},
		Capabilities:     fakeMapping(testEP, true),
		MaxResponseBytes: testSize,
		connect:          pipeConnect(srv, nil),
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	hostile := []any{
		map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "sampling/createMessage", "params": map[string]any{"messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": injection}}}, "maxTokens": 100}},
		map[string]any{"jsonrpc": "2.0", "id": 9002, "method": "elicitation/create", "params": map[string]any{"message": injection, "requestedSchema": map[string]any{"type": "object"}}},
		map[string]any{"jsonrpc": "2.0", "id": 9003, "method": "roots/list"},
	}
	srv.Handle(fixture.ToolGet, func(context.Context, json.RawMessage) fixture.Result {
		r := fixture.FromBody([]byte(validResultJSON), "")
		r.Preamble = hostile
		return r
	})
	res, err := readItem(testContext(t), a, "item_1")
	if err != nil || res.Status != actions.DeterministicResultStatusOk {
		t.Fatalf("read = %+v, %v", res, err)
	}
	if !fixture.Wait(5*time.Second, func() bool {
		n := 0
		for _, e := range srv.Log() {
			if e.Method == "<response>" {
				n++
			}
		}
		return n == len(hostile)
	}) {
		t.Fatalf("the client did not answer the server's requests: %+v", srv.Log())
	}
	for _, e := range srv.Log() {
		if e.Method != "<response>" {
			continue
		}
		var r struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct{}       `json:"error"`
		}
		if err := json.Unmarshal(e.Args, &r); err != nil {
			t.Fatal(err)
		}
		if r.ID == 9003 {
			// The SDK answers roots/list on its own, with no root: nothing of the host.
			if string(r.Result) != `{"roots":[]}` {
				t.Errorf("the client answered roots/list with %s", e.Args)
			}
			continue
		}
		if r.Error == nil || len(r.Result) != 0 {
			t.Errorf("the client answered a server request with a result: %s", e.Args)
		}
	}
}

// A structured result that is larger than the bound, though the message is not
// larger than the transport allows, is refused too.
func TestStructuredResultOverTheBound(t *testing.T) {
	a, h, _ := newHTTPAdapter(t)
	w := wrapped(t, a)
	big := strings.Repeat("a", testSize+1024)
	h.Handle(fixture.ToolGet, func(context.Context, json.RawMessage) fixture.Result {
		return fixture.Result{Structured: map[string]any{"status": "ok", "padding": big}}
	})
	_, err := readItem(testContext(t), w, "item_1")
	if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || detail(err, "reason") != adapters.ReasonResponseTooLarge {
		t.Errorf("read = %v, want schema_invalid (response_too_large)", err)
	}
	h.Handle(fixture.ToolGet, func(context.Context, json.RawMessage) fixture.Result {
		return fixture.FromBody([]byte(validResultJSON), "")
	})
	if _, err := readItem(testContext(t), w, "item_1"); err != nil {
		t.Errorf("after the oversize reply: %v", err)
	}
}

// A server that accepts the connection and never speaks cannot hold a call past the
// connect timeout.
func TestConnectTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	url := "http://" + ln.Addr().String() + "/mcp"
	a, err := New(Config{
		ApplicationID: testApp, Endpoints: []Endpoint{{ID: testEP, URL: url}}, Capabilities: fakeMapping(testEP, true),
		Dialer: loopbackDialer(t, url), ConnectTimeout: 300 * time.Millisecond, MaxResponseBytes: testSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	start := time.Now()
	_, err = readItem(testContext(t), wrapped(t, a), "item_1")
	if contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("read = %v, want unavailable", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a server that never answers held the call for %s", d)
	}
}

// Closing the adapter leaves nothing behind: no goroutine of a session, no idle
// connection, no process.
func TestCloseReleasesWhatItHolds(t *testing.T) {
	settle := func() int {
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	base := settle()

	app := fake.NewApp()
	app.Seed("ten_a", "wks_a", "item_1", "Widget")
	srv := fixture.New(app)
	h := fixture.StartHTTP(t, srv)
	a, err := New(httpConfig(t, h))
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	for i := 0; i < 20; i++ {
		if _, err := readItem(ctx, a, "item_1"); err != nil {
			t.Fatal(err)
		}
	}
	_ = a.Close()
	h.Down()
	if !fixture.Wait(10*time.Second, func() bool { return settle() <= base+2 }) {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Errorf("%d goroutines after Close, %d before:\n%s", runtime.NumGoroutine(), base, buf[:n])
	}
}

// Contract of M1-12, held by the fixture and passed through by the adapter:
// Validate on unchanged data reports the version Prepare stored (the snapshot
// version of the records the action relies on), and a change to those records
// moves it. A version that moved with every response would make every commit stale.
func TestValidateReportsThePrepareVersion(t *testing.T) {
	a, _, _ := newHTTPAdapter(t)
	w := wrapped(t, a)
	ctx := testContext(t)
	fields := map[string]any{"name": "Widget"}
	prep, err := w.Prepare(ctx, testScope(), adapters.PrepareRequest{CapabilityID: fake.ActionCapability, Fields: fields})
	if err != nil || prep.Freshness == nil || len(prep.Preparation.DataVersions) == 0 {
		t.Fatalf("Prepare = %+v, %v", prep, err)
	}
	for i := 0; i < 3; i++ {
		val, err := w.Validate(ctx, testScope(), adapters.ValidateRequest{CapabilityID: fake.ActionCapability, Fields: fields})
		if err != nil || val.Freshness == nil {
			t.Fatalf("Validate = %+v, %v", val, err)
		}
		if val.Freshness.Version != prep.Freshness.Version || string(prep.Preparation.DataVersions[0].Version) != string(prep.Freshness.Version) {
			t.Errorf("Validate reports version %q, Prepare stored %q (data version %q)", val.Freshness.Version, prep.Freshness.Version, prep.Preparation.DataVersions[0].Version)
		}
	}
	p := prepareItem(ctx, t, w, "Widget")
	if _, err := commitItem(ctx, w, p); err != nil {
		t.Fatal(err)
	}
	val, err := w.Validate(ctx, testScope(), adapters.ValidateRequest{CapabilityID: fake.ActionCapability, Fields: fields})
	if err != nil || val.Freshness == nil || val.Freshness.Version == prep.Freshness.Version {
		t.Errorf("after a change to the records the version is still %q (%v)", prep.Freshness.Version, err)
	}
}

// The line limit counts a line across reads, and a newline starts the count again.
func TestLineLimitReader(t *testing.T) {
	for _, tc := range []struct {
		in    string
		chunk int
		trip  bool
	}{
		{"abcd\nabcd\n", 3, false},
		{"abcd\nabcde\n", 3, true},
		{"abcde", 10, true},
		{"ab\n\n\nabcd", 1, false},
		{"abcd\nabcd", 64, false},
	} {
		var trips atomic.Int64
		r := &lineLimitReader{rc: io.NopCloser(strings.NewReader(tc.in)), limit: 4, trips: &trips}
		p := make([]byte, tc.chunk)
		var got []byte
		var err error
		for err == nil {
			var n int
			n, err = r.Read(p)
			got = append(got, p[:n]...)
		}
		if tripped := errors.Is(err, errTooLarge); tripped != tc.trip || tripped != (trips.Load() == 1) {
			t.Errorf("%q by %d: error %v, %d trips, want a trip %v", tc.in, tc.chunk, err, trips.Load(), tc.trip)
		}
		if !tc.trip && string(got) != tc.in {
			t.Errorf("%q by %d: read %q", tc.in, tc.chunk, got)
		}
	}
}
