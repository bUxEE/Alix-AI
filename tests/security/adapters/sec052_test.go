package adapterssec

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	mcpfixture "github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"

	mcptransport "github.com/buxee/alix-ai/adapters/mcp"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const (
	libraryEndpoint = "library-mcp"
	adapterConfig   = "schemas/adapters/library-mcp.yaml"
	capFind         = "catalog.find_title"
	capReserve      = "reservations.create"
)

// mcpConfigFromPackage is the mapping the composition root builds from a loaded
// package (internal/cli transport_mcp.go): the operations of every capability become
// the tools they name, and the adapter configuration's tools.allow is the approved
// list. The endpoint is retargeted at the test server.
func mcpConfigFromPackage(t testing.TB, pkg *apppkg.Package, files pkgFiles, serverURL string) mcptransport.Config {
	t.Helper()
	ac, err := mcptransport.ParseAdapterConfig(files[adapterConfig], libraryEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mcptransport.Config{
		ApplicationID:    libraryApp,
		Endpoints:        []mcptransport.Endpoint{{ID: libraryEndpoint, URL: serverURL, Allow: ac.Tools.Allow}},
		Dialer:           loopbackDialer(t, serverURL),
		MaxResponseBytes: 64 << 10,
		SurfaceRecheck:   -1,
	}
	ids := make([]string, 0, len(pkg.Capabilities))
	for id := range pkg.Capabilities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	bind := func(b *ap.CapabilityOperationBinding) mcptransport.Binding {
		out := mcptransport.Binding{Endpoint: string(b.Endpoint), Tool: string(*b.MCPTool)}
		if len(b.Arguments) > 0 {
			out.Arguments = map[string]string{}
			for from, to := range b.Arguments {
				out.Arguments[from] = string(to)
			}
		}
		return out
	}
	for _, id := range ids {
		c := pkg.Capabilities[id]
		mc := mcptransport.Capability{ID: id, Operations: map[adapters.Operation]mcptransport.Binding{}, Resolve: map[string]mcptransport.Binding{}}
		for op, b := range map[adapters.Operation]*ap.CapabilityOperationBinding{
			adapters.OpRead: c.Operations.Read, adapters.OpValidate: c.Operations.Validate, adapters.OpPrepare: c.Operations.Prepare,
			adapters.OpCommit: c.Operations.Commit, adapters.OpStatus: c.Operations.Status,
		} {
			if b != nil {
				mc.Operations[op] = bind(b)
			}
		}
		for typ, et := range c.EntityTypes {
			if et.Resolve != nil {
				mc.Resolve[typ] = bind(et.Resolve)
			}
		}
		cfg.Capabilities = append(cfg.Capabilities, mc)
	}
	return cfg
}

// okResult is a complete, valid ok result: what an honest tool answers.
var okResult = []byte(`{"status":"ok","identifiers":[],"values":[],"errors":[],"allowed_next_actions":[],` +
	`"freshness":{"as_of":"2026-10-09T08:00:00Z","version":"v1"}}`)

// libraryServer is a hostile MCP server that offers the tools a package maps (each
// answers an honest ok result) and whatever else the test adds to it.
func libraryServer(t testing.TB, tools ...string) *mcpfixture.Server {
	t.Helper()
	srv := mcpfixture.New(fake.NewApp())
	for _, n := range srv.Names() {
		srv.Remove(n)
	}
	for _, n := range tools {
		addTool(srv, n, "the tool of the package")
	}
	return srv
}

func addTool(srv *mcpfixture.Server, name, desc string) {
	srv.Add(
		mcpfixture.Tool{Name: name, Description: desc, Input: map[string]any{"type": "object"}},
		func(context.Context, json.RawMessage) mcpfixture.Result { return mcpfixture.FromBody(okResult, "") },
	)
}

func libraryScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: libraryApp, TenantID: tenantA, WorkspaceID: ws1, PrincipalID: "user-1",
		Roles: []string{"member"}, CapabilityScopes: []string{capFind, capReserve}, SessionID: "ses-1",
	}
}

// activeLibrary is a loaded package presented as the active one, which is the state the
// integration audit gives it; the loader reports a fresh package as quarantined.
type activeLibrary struct{ capabilities.Package }

func (activeLibrary) Status() policy.PackageStatus { return policy.StatusActive }

// exactTools are the tools valid-minimal maps, plus the tools of its disposable-tenant
// provisioning, which tools.allow approves and no capability maps.
var exactTools = []string{
	"find_title", "resolve_title", "validate_reservation", "prepare_reservation", "commit_reservation", "reservation_status",
	"test_create_tenant", "test_seed", "test_cleanup",
}

// SEC-052: duplicate, look-alike or renamed tool names are rejected, or resolved only by
// the exact mapped identifier, across the real package loader, the capability registry
// and policy gateway, and the MCP adapter built from the package's mapping.
//
// SEC-052
func TestSecToolNameCollisionAndLookalike(t *testing.T) {
	v := verifier(t, libraryApp)
	base := validMinimal(t)
	// valid-minimal renames the argument pickup_date to pickupDate, which the package grammar
	// accepts and the MCP adapter refuses (argument names are lower snake_case); the
	// copy this test signs uses a name both accept.
	base.edit(t, "capabilities/reservations-create.yaml", "pickup_date: pickupDate", "pickup_date: pickup_day")

	t.Run("package loader", func(t *testing.T) {
		const capFile = "capabilities/reservations-create.yaml"
		control := load(t, v, base.clone().signed(t, libraryApp, "1.0.0"))
		if !control.OK() {
			t.Fatalf("control: the unmodified package does not load: %v", criticalIDs(control))
		}

		// A mapped name that is not a valid tool name at all: whitespace, a zero-width
		// space, a Cyrillic homoglyph, a path.
		for name, tool := range map[string]string{
			"trailing space":        "validate_reservation ",
			"zero-width space":      "validate​_reservation",
			"Cyrillic homoglyph":    "valіdate_reservation",
			"embedded space":        "validate reservation",
			"parent path":           "../validate_reservation",
			"trailing slash":        "validate_reservation/",
			"leading dot":           ".validate_reservation",
			"newline":               "validate_reservation\n",
			"line separator":        "validate_reservation ",
			"right-to-left mark":    "validate_reservation‏",
			"full-width underscore": "validate＿reservation",
		} {
			t.Run(name, func(t *testing.T) {
				p := base.clone().edit(t, capFile, "mcp_tool: validate_reservation", "mcp_tool: "+yamlQuote(tool))
				res := load(t, v, p.signed(t, libraryApp, "1.0.0"))
				if res.Package != nil || !hasID(criticalIDs(res), "schema.capability") {
					t.Fatalf("a package that maps %q loaded, or without the schema finding: ok=%v critical=%v", tool, res.OK(), criticalIDs(res))
				}
			})
		}

		// Two different names on one endpoint that differ only by case or by '_', '-' and
		// '.': one could be mistaken for the other.
		for name, tool := range map[string]string{
			"case and hyphen of the commit tool": "Commit-Reservation",
			"dots instead of underscores":        "commit.reservation",
			"upper case":                         "COMMIT_RESERVATION",
			"no separators":                      "commitreservation",
		} {
			t.Run("look-alike of another mapped tool: "+name, func(t *testing.T) {
				p := base.clone().edit(t, capFile, "mcp_tool: reservation_status", "mcp_tool: "+tool).
					edit(t, adapterConfig, "    - reservation_status", "    - "+tool)
				res := load(t, v, p.signed(t, libraryApp, "1.0.0"))
				if res.Package != nil || !hasID(criticalIDs(res), "adapter.lookalike_operation") {
					t.Fatalf("a package that maps %q next to commit_reservation loaded, or without the finding: critical=%v", tool, criticalIDs(res))
				}
			})
		}

		// Duplicate names within a package: a key written twice (the last one wins in a lenient
		// parser), the same capability under two manifest keys, the same tool listed twice by
		// the adapter configuration.
		t.Run("duplicates inside one package", func(t *testing.T) {
			p := base.clone().edit(t, capFile, "mcp_tool: validate_reservation", "mcp_tool: validate_reservation\n    mcp_tool: commit_reservation")
			if res := load(t, v, p.signed(t, libraryApp, "1.0.0")); res.Package != nil || !hasID(criticalIDs(res), "yaml.invalid") {
				t.Errorf("a capability file with the key mcp_tool twice loaded, or without the finding: critical=%v", criticalIDs(res))
			}
			q := base.clone()
			q["capabilities/reservations-create2.yaml"] = append([]byte(nil), q[capFile]...)
			q.edit(t, "alixai.yaml", "  reservations.create: capabilities/reservations-create.yaml", "  reservations.create: capabilities/reservations-create.yaml\n  reservations.create2: capabilities/reservations-create2.yaml")
			if res := load(t, v, q.signed(t, libraryApp, "1.0.0")); res.Package != nil || !hasID(criticalIDs(res), "manifest.capability_id_mismatch") {
				t.Errorf("the same capability under two manifest keys loaded, or without the finding: critical=%v", criticalIDs(res))
			}
			// The adapter configuration is the adapter's to read: a tool listed twice, or a key
			// written twice, is refused when the adapter is configured. (Control: the file as it is.)
			if _, err := mcptransport.ParseAdapterConfig(base[adapterConfig], libraryEndpoint); err != nil {
				t.Fatalf("control: the adapter configuration is refused: %v", err)
			}
			for name, raw := range map[string][]byte{
				"a tool twice":              base.clone().edit(t, adapterConfig, "    - find_title", "    - find_title\n    - find_title")[adapterConfig],
				"a key twice":               base.clone().edit(t, adapterConfig, "server: library-mcp", "server: library-mcp\nserver: library-mcp")[adapterConfig],
				"a look-alike with a space": base.clone().edit(t, adapterConfig, "    - find_title", "    - find_title \n    - find title")[adapterConfig],
			} {
				if _, err := mcptransport.ParseAdapterConfig(raw, libraryEndpoint); contracts.CodeOf(err) != contracts.ErrValidationFailed {
					t.Errorf("%s: ParseAdapterConfig = %v, want validation_failed", name, err)
				}
			}
		})

		// A capability whose operation points at a call the transport cannot make.
		t.Run("a capability without an exact adapter mapping", func(t *testing.T) {
			for name, edit := range map[string]func(pkgFiles) pkgFiles{
				"an endpoint the manifest does not declare": func(p pkgFiles) pkgFiles {
					return p.edit(t, capFile, "endpoint: library-mcp\n    mcp_tool: validate_reservation", "endpoint: library-mcp2\n    mcp_tool: validate_reservation")
				},
				"another transport's key": func(p pkgFiles) pkgFiles {
					return p.edit(t, capFile, "mcp_tool: validate_reservation", "openapi_operation_id: validate_reservation")
				},
				"two call keys": func(p pkgFiles) pkgFiles {
					return p.edit(t, capFile, "mcp_tool: validate_reservation", "mcp_tool: validate_reservation\n    openapi_operation_id: validate_reservation")
				},
			} {
				res := load(t, v, edit(base.clone()).signed(t, libraryApp, "1.0.0"))
				if res.Package != nil || !res.Critical() {
					t.Errorf("%s: the package loaded", name)
				}
			}
		})
	})

	t.Run("capability registry and policy gateway", func(t *testing.T) {
		res := load(t, v, base.clone().signed(t, libraryApp, "1.0.0"))
		if !res.OK() {
			t.Fatalf("the package does not load: %v", criticalIDs(res))
		}
		pkg := activeLibrary{capabilities.FromApppkg(res.Package)}
		clk := clock.NewFake(loadNow)
		reg := capabilities.NewRegistry(nil, capabilities.WithClock(clk))
		if err := reg.Activate(pkg); err != nil {
			t.Fatal(err)
		}
		info, err := reg.Info(ctxb(t), libraryApp)
		if err != nil || strings.Join(info.Capabilities, ",") != capFind+","+capReserve {
			t.Fatalf("registered capabilities = %v (%v), want exactly %s and %s", info.Capabilities, err, capFind, capReserve)
		}

		gw := policy.NewGateway(clk)
		sc := libraryScope()
		decide := func(id string) policy.Decision {
			return gw.Decide(ctxb(t), policy.Request{
				Scope: sc, Package: pkg, CapabilityID: id, Binding: policy.BindingOf(sc), Phase: policy.PhasePrepare,
				Fields: map[string]any{"title": "x", "pickup_date": "2026-11-13"},
			})
		}
		// Control: the exact id is allowed.
		if d := decide(capReserve); !d.Allowed {
			t.Fatalf("control: the exact capability id is denied: %v", d.Err())
		}
		for name, id := range map[string]string{
			"upper case":              "RESERVATIONS.CREATE",
			"mixed case":              "Reservations.Create",
			"trailing space":          "reservations.create ",
			"leading space":           " reservations.create",
			"trailing dot":            "reservations.create.",
			"zero-width space":        "reservations.create​",
			"Cyrillic homoglyph":      "reservаtions.create",
			"hyphen for underscore":   "reservations-create",
			"path":                    "reservations/create",
			"path-normalised":         "catalog.find_title/../reservations.create",
			"newline":                 "reservations.create\n",
			"another capability's id": "reservations.create2",
		} {
			d := decide(id)
			if d.Allowed {
				t.Errorf("%s: the look-alike capability id %q was allowed", name, id)
			}
			if d.Code == "" {
				t.Errorf("%s: the denial has no typed code", name)
			}
			if _, ok := pkg.Capability(id); ok {
				t.Errorf("%s: the package resolves %q", name, id)
			}
			if sc.Permits(id) {
				t.Errorf("%s: the token permits %q", name, id)
			}
		}
		// A token that names a look-alike does not grant the real capability, and one that
		// names only the look-alike cannot use it.
		other := sc
		other.CapabilityScopes = []string{"RESERVATIONS.CREATE", capFind}
		if d := gw.Decide(ctxb(t), policy.Request{Scope: other, Package: pkg, CapabilityID: capReserve, Binding: policy.BindingOf(other),
			Phase: policy.PhasePrepare, Fields: map[string]any{"title": "x", "pickup_date": "2026-11-13"}}); d.Allowed {
			t.Error("a token that names only a look-alike was allowed to use the real capability")
		}
	})

	t.Run("MCP adapter built from the package", func(t *testing.T) {
		res := load(t, v, base.clone().signed(t, libraryApp, "1.0.0"))
		if !res.OK() {
			t.Fatalf("the package does not load: %v", criticalIDs(res))
		}
		build := func(t *testing.T, srv *mcpfixture.Server, mutate ...func(*mcptransport.Config)) (*mcptransport.Adapter, adapters.AppAdapter) {
			t.Helper()
			h := mcpfixture.StartHTTP(t, srv)
			cfg := mcpConfigFromPackage(t, res.Package, base, h.URL)
			for _, m := range mutate {
				m(&cfg)
			}
			a, err := mcptransport.New(cfg)
			if err != nil {
				t.Fatalf("mcp.New: %v", err)
			}
			t.Cleanup(func() { _ = a.Close() })
			w, err := adapters.Wrap(a, adapters.Options{ApplicationID: libraryApp})
			if err != nil {
				t.Fatal(err)
			}
			return a, w
		}
		read := func(w adapters.AppAdapter, capID string) error {
			_, err := w.Read(ctxb(t), libraryScope(), adapters.ReadRequest{CapabilityID: capID, Fields: map[string]any{"title": "x"}})
			return err
		}
		validate := func(w adapters.AppAdapter) error {
			_, err := w.Validate(ctxb(t), libraryScope(), adapters.ValidateRequest{CapabilityID: capReserve, Fields: map[string]any{"title": "x", "pickup_date": "2026-11-13"}})
			return err
		}
		reason := func(err error) string {
			var ce *contracts.Error
			if e, ok := err.(*contracts.Error); ok {
				ce = e
			}
			if ce == nil {
				return ""
			}
			r, _ := ce.Details["reason"].(string)
			return r
		}

		// Look-alikes of every mapped tool are listed next to it.
		var lookalikes []string
		for _, n := range []string{"find_title", "validate_reservation", "commit_reservation"} {
			lookalikes = append(lookalikes,
				strings.ToUpper(n), strings.ReplaceAll(n, "_", "-"), strings.ReplaceAll(n, "_", "."), n+".", n+"_", "_"+n,
				n+"​", n+" ", strings.Replace(n, "e", "е", 1) /* Cyrillic e */, "../"+n, n+"/")
		}
		t.Run("look-alikes next to the mapped tools are never called", func(t *testing.T) {
			srv := libraryServer(t, exactTools...)
			for _, n := range lookalikes {
				addTool(srv, n, "a tool that looks like a mapped one")
			}
			a, w := build(t, srv)
			// Control: the exact mapped names resolve and execute.
			if err := read(w, capFind); err != nil {
				t.Fatalf("control: the exact mapped tool find_title does not execute: %v", err)
			}
			if err := validate(w); err != nil {
				t.Fatalf("control: the exact mapped tool validate_reservation does not execute: %v", err)
			}
			if srv.ToolCalls("find_title") != 1 || srv.ToolCalls("validate_reservation") != 1 {
				t.Fatalf("the mapped tools were called %d and %d times, want 1 and 1", srv.ToolCalls("find_title"), srv.ToolCalls("validate_reservation"))
			}
			// Nothing else was ever called, whichever name it carries.
			for _, n := range lookalikes {
				if srv.ToolCalls(n) != 0 {
					t.Errorf("the look-alike tool %q was called", n)
				}
			}
			if srv.ToolCalls("") != 2 {
				t.Errorf("the server received %d tool calls, want 2", srv.ToolCalls(""))
			}
			// The adapter reports the look-alikes as unmapped for the integration audit,
			// by name when the name is printable and by length and hash otherwise, and
			// still maps every operation to its exact tool.
			d, err := w.Describe(ctxb(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Unmapped) < len(lookalikes)/2 {
				t.Fatalf("only %d of %d look-alikes are reported as unmapped: %v", len(d.Unmapped), len(lookalikes), d.Unmapped)
			}
			for _, u := range d.Unmapped {
				for _, exact := range exactTools {
					if u == exact {
						t.Errorf("the exact tool %q is reported as unmapped", u)
					}
				}
			}
			ops := map[string]int{}
			for _, c := range d.Capabilities {
				ops[c.CapabilityID] = len(c.Operations)
			}
			if ops[capFind] != 1 || ops[capReserve] != 4 {
				t.Errorf("the described operations = %v, want 1 for %s and 4 for %s", ops, capFind, capReserve)
			}
			if a.ReauditRequired() {
				t.Error("look-alike tools are unmapped, not drift")
			}
		})

		t.Run("a look-alike capability id reaches no tool", func(t *testing.T) {
			srv := libraryServer(t, exactTools...)
			a, w := build(t, srv)
			for _, id := range []string{"CATALOG.FIND_TITLE", "catalog.find_title ", "catalog.find_title​", "catаlog.find_title", "find_title", "../catalog.find_title", "catalog.find-title", "catalog.find_title."} {
				err := read(w, id)
				if err == nil {
					t.Errorf("the look-alike capability id %q was served", id)
				}
				// The token check of adapters.Wrap refuses it first.
				if c := contracts.CodeOf(err); c != contracts.ErrScopeViolation && c != contracts.ErrValidationFailed {
					t.Errorf("capability id %q: error code %s, want scope_violation or validation_failed", id, c)
				}
				// And the adapter itself, reached without the wrapper, maps nothing for it.
				if _, rerr := a.Read(ctxb(t), libraryScope(), adapters.ReadRequest{CapabilityID: id, Fields: map[string]any{"title": "x"}}); rerr == nil || (contracts.CodeOf(rerr) != contracts.ErrUnknownCapability && contracts.CodeOf(rerr) != contracts.ErrValidationFailed) {
					t.Errorf("capability id %q: the raw adapter answered %v, want unknown_capability", id, rerr)
				}
			}
			if srv.ToolCalls("") != 0 {
				t.Fatalf("a look-alike capability id caused %d tool calls", srv.ToolCalls(""))
			}
			// Control: the exact id on the same adapter is served.
			if err := read(w, capFind); err != nil || srv.ToolCalls("find_title") != 1 {
				t.Fatalf("control: the exact capability id is not served: %v", err)
			}
		})

		t.Run("a server that renames a mapped tool", func(t *testing.T) {
			srv := libraryServer(t, exactTools...)
			a, w := build(t, srv)
			if err := validate(w); err != nil {
				t.Fatalf("control: before the rename: %v", err)
			}
			srv.Remove("validate_reservation")
			addTool(srv, "Validate_Reservation", "renamed")
			addTool(srv, "validate_reservation.", "renamed")
			err := validate(w)
			if c := contracts.CodeOf(err); c != contracts.ErrQuarantined || reason(err) != mcptransport.ReasonToolSchemaDrift {
				t.Fatalf("validate after the rename = %v, want quarantined (%s)", err, mcptransport.ReasonToolSchemaDrift)
			}
			if srv.ToolCalls("Validate_Reservation") != 0 || srv.ToolCalls("validate_reservation.") != 0 || srv.ToolCalls("validate_reservation") != 1 {
				t.Fatal("a renamed tool received the call")
			}
			d, err := w.Describe(ctxb(t))
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range d.Capabilities {
				for _, op := range c.Operations {
					if c.CapabilityID == capReserve && op == adapters.OpValidate {
						t.Error("the described operations still offer validate")
					}
				}
			}
			_ = a
		})

		t.Run("a server that lists a mapped name twice", func(t *testing.T) {
			srv := libraryServer(t, exactTools...)
			a, w := build(t, srv)
			if err := validate(w); err != nil {
				t.Fatalf("control: before the duplicate: %v", err)
			}
			calls := srv.ToolCalls("validate_reservation")
			srv.AddDuplicate(mcpfixture.Tool{Name: "validate_reservation", Description: "the other one", Input: map[string]any{"type": "object", "properties": map[string]any{"debug": map[string]any{"type": "boolean"}}}})
			err := validate(w)
			if c := contracts.CodeOf(err); c != contracts.ErrQuarantined || reason(err) != mcptransport.ReasonToolSchemaDrift {
				t.Fatalf("validate with a duplicated tool = %v, want quarantined (%s)", err, mcptransport.ReasonToolSchemaDrift)
			}
			if srv.ToolCalls("validate_reservation") != calls {
				t.Fatal("a duplicated tool received a call")
			}
			if !a.ReauditRequired() {
				t.Error("the duplicate did not raise the re-audit flag")
			}
		})

		t.Run("a mapped name that the adapter configuration does not approve", func(t *testing.T) {
			// The loader accepts these names (they are well formed and distinct); the adapter
			// configuration lists only the exact one, and the adapter refuses to be built.
			for _, tool := range []string{"Validate_Reservation", "validate_reservation.", "validate-reservation"} {
				p := base.clone().edit(t, "capabilities/reservations-create.yaml", "mcp_tool: validate_reservation", "mcp_tool: "+tool)
				lr := load(t, v, p.signed(t, libraryApp, "1.0.0"))
				if !lr.OK() {
					// A loader that refuses these names fails closed too; the adapter is not reached.
					t.Logf("%q: refused by the loader: %v", tool, criticalIDs(lr))
					continue
				}
				cfg := mcpConfigFromPackage(t, lr.Package, p, "http://127.0.0.1:9/mcp")
				_, err := mcptransport.New(cfg)
				if contracts.CodeOf(err) != contracts.ErrValidationFailed {
					t.Errorf("%q: mcp.New = %v, want validation_failed (the tool is not approved)", tool, err)
				}
				// Control: with the exact name approved as well, the adapter is built.
				p2 := p.clone().edit(t, adapterConfig, "    - validate_reservation", "    - validate_reservation\n    - "+tool)
				cfg2 := mcpConfigFromPackage(t, lr.Package, p2, "http://127.0.0.1:9/mcp")
				a, err := mcptransport.New(cfg2)
				if err != nil {
					t.Errorf("%q: control: the adapter is refused although the tool is approved: %v", tool, err)
					continue
				}
				_ = a.Close()
			}
		})

		t.Run("an approved look-alike beside the exact tool resolves to the mapped one only", func(t *testing.T) {
			// The package maps Validate_Reservation and approves it; the server also lists
			// validate_reservation, which is approved but mapped by nobody.
			p := base.clone().edit(t, "capabilities/reservations-create.yaml", "mcp_tool: validate_reservation", "mcp_tool: Validate_Reservation").
				edit(t, adapterConfig, "    - validate_reservation", "    - validate_reservation\n    - Validate_Reservation")
			lr := load(t, v, p.signed(t, libraryApp, "1.0.0"))
			if !lr.OK() {
				t.Fatalf("the package does not load: %v", criticalIDs(lr))
			}
			srv := libraryServer(t, append(append([]string(nil), exactTools...), "Validate_Reservation")...)
			h := mcpfixture.StartHTTP(t, srv)
			a, err := mcptransport.New(mcpConfigFromPackage(t, lr.Package, p, h.URL))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.Close() })
			w, _ := adapters.Wrap(a, adapters.Options{ApplicationID: libraryApp})
			if err := validate(w); err != nil {
				t.Fatalf("the mapped tool does not execute: %v", err)
			}
			if srv.ToolCalls("Validate_Reservation") != 1 || srv.ToolCalls("validate_reservation") != 0 {
				t.Fatalf("calls: mapped %d, look-alike %d; want 1 and 0", srv.ToolCalls("Validate_Reservation"), srv.ToolCalls("validate_reservation"))
			}
		})
	})
}

// yamlQuote writes s as a double-quoted YAML scalar with the escapes JSON uses, which
// YAML reads the same way.
func yamlQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
