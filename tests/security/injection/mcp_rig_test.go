package injectionsec

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"testing"

	mcpadapter "github.com/buxee/alix-ai/adapters/mcp"
	"github.com/buxee/alix-ai/adapters/mcp/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/tests/conversation"
)

// mcpEndpoint is the endpoint ID of the clinic's MCP server in the package.
const mcpEndpoint = "clinic-mcp"

// clinicMCP is the clinic application behind a real MCP server over loopback HTTP, reached by the
// real MCP adapter: the scriptable fixture server of adapters/mcp, with the clinic's read tools
// served from the same stand-in host the other tests use. The server is also an adversary, and the
// test decides what it says.
type clinicMCP struct {
	srv     *fixture.HTTPServer
	adapter *mcpadapter.Adapter
}

// mapped is what the package maps for the two capabilities the MCP world serves: the read tool of
// each.
var mapped = []struct{ tool, capability string }{
	{"availability_list", "availability.list"},
	{"patients_find", "patients.find"},
}

// loopbackDialer allows exactly the host and port of an http URL.
func loopbackDialer(t testing.TB, rawURL string) *egress.Dialer {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portText)
	al, err := egress.NewAllowlist(egress.Destination{Host: host, Ports: []int{port}, Scheme: egress.SchemeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	return egress.NewDialer(al)
}

// objectSchema is the input schema of a capability tool: the minimum identity and the fields.
func objectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"identity": map[string]any{"type": "object"}, "fields": map[string]any{"type": "object"},
	}, "required": []string{"identity", "fields"}}
}

// clinicTool answers a read tool from the stand-in host, as the clinic application's own MCP server
// does: the host members of a DeterministicResult as structured content, mirrored as text.
func clinicTool(host *conversation.ClinicHost, capability string) fixture.Handler {
	ad := host.Adapter()
	return func(ctx context.Context, args json.RawMessage) fixture.Result {
		var in struct {
			Identity adapters.Identity `json:"identity"`
			Fields   map[string]any    `json:"fields"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return fixture.Result{IsError: true, Text: "bad arguments"}
		}
		sc := contracts.Scope{
			DeploymentID: "dep-mcp", ApplicationID: in.Identity.ApplicationID, TenantID: in.Identity.TenantID, WorkspaceID: in.Identity.WorkspaceID,
			PrincipalID: in.Identity.PrincipalID, Roles: in.Identity.Roles, SessionID: in.Identity.SessionID, CapabilityScopes: []string{capability},
		}
		res, err := ad.Read(ctx, sc, adapters.ReadRequest{CapabilityID: capability, Fields: in.Fields})
		if err != nil {
			return fixture.Result{IsError: true, Text: "the application failed"}
		}
		b, err := json.Marshal(res)
		if err != nil {
			return fixture.Result{IsError: true, Text: "the reply cannot be encoded"}
		}
		return fixture.FromBody(b, "")
	}
}

// newClinicMCP starts the server over the host and builds the adapter from the mapping the package
// declares (read tools only: the rest of the clinic's tools are not served here).
func newClinicMCP(t testing.TB, host *conversation.ClinicHost) *clinicMCP {
	t.Helper()
	srv := fixture.New(fake.NewApp())
	out := map[string]any{"type": "object", "properties": map[string]any{"status": map[string]any{"type": "string"}}, "required": []string{"status"}}
	var allow []string
	var caps []mcpadapter.Capability
	for _, m := range mapped {
		srv.Add(fixture.Tool{Name: m.tool, Description: "Reads the clinic's " + m.capability + ".", Input: objectSchema(), Output: out}, clinicTool(host, m.capability))
		allow = append(allow, m.tool)
		caps = append(caps, mcpadapter.Capability{ID: m.capability, Operations: map[adapters.Operation]mcpadapter.Binding{
			adapters.OpRead: {Endpoint: mcpEndpoint, Tool: m.tool},
		}})
	}
	h := fixture.StartHTTP(t, srv)
	a, err := mcpadapter.New(mcpadapter.Config{
		ApplicationID: clinicApp,
		Endpoints:     []mcpadapter.Endpoint{{ID: mcpEndpoint, URL: h.URL, Allow: allow}},
		Capabilities:  caps,
		Dialer:        loopbackDialer(t, h.URL),
		// The tools are listed again before every call, so a change the server makes is seen at once.
		SurfaceRecheck: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return &clinicMCP{srv: h, adapter: a}
}
