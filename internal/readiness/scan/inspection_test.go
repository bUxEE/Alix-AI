package scan

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

const scanTestOpenAPIHead = "openapi: 3.0.3\ninfo:\n  title: t\n  version: \"1\"\npaths:\n"

func scanTestOpenAPIPath(p, id string) string {
	return "  " + p + ":\n    get:\n      operationId: " + id + "\n      responses:\n        \"200\":\n          description: ok\n"
}

// A health or inspection surface is recognised where an application really declares one: a versioned or prefixed route or
// OpenAPI path, a management operation in an adapter configuration, a native bridge's inspect or health handler, or an
// operation or tool with that exact name. Prose and look-alike names never count.
func TestScanInspectionSurfaces(t *testing.T) {
	const nativeImport = "package app\n\nimport \"github.com/buxee/alix-ai/pkg/bridge\"\n\n"
	found := []struct {
		name  string
		files map[string]string
		path  string // the file the evidence must cite
	}{
		{"versioned OpenAPI health path", map[string]string{"api/openapi.yaml": scanTestOpenAPIHead + scanTestOpenAPIPath("/v1/health", "getHealth")}, "api/openapi.yaml"},
		{"prefixed OpenAPI inspect path", map[string]string{"api/openapi.yaml": scanTestOpenAPIHead + scanTestOpenAPIPath("/api/v1/inspect", "inspectApplication")}, "api/openapi.yaml"},
		{"OpenAPI JSON readyz path", map[string]string{"api/openapi.json": `{"openapi": "3.0.3", "info": {"title": "t", "version": "1"}, "paths": {"/svc/v2/readyz": {"get": {"operationId": "ready", "responses": {"200": {"description": "ok"}}}}}}`}, "api/openapi.json"},
		{"versioned route in code", map[string]string{"src/app.py": "ROUTES = {\n    \"getHealth\": (\"GET\", \"/v1/health\"),\n}\n"}, "src/app.py"},
		{"prefixed route in code", map[string]string{"src/server.ts": "app.get('/api/v1/inspect', handler);\n"}, "src/server.ts"},
		{"management health in an adapter configuration", map[string]string{"package/schemas/adapters/api.yaml": "server: api\nmanagement:\n  describe: describeApplication\n  health: getHealth\n"}, "package/schemas/adapters/api.yaml"},
		{"management inspect in an adapter configuration", map[string]string{"package/schemas/adapters/api.yaml": "server: api\nmanagement:\n  inspect: inspectApplication\n"}, "package/schemas/adapters/api.yaml"},
		{"native bridge inspect handler", map[string]string{"bridge.go": nativeImport + "func build() {\n\tbridge.New(bridge.Config{\n\t\tLogger: nil, Inspect: l.inspect,\n\t})\n}\n"}, "bridge.go"},
		{"native bridge health handler", map[string]string{"bridge.go": nativeImport + "var cfg = bridge.Config{\n\tHealth: func(context.Context) (bridge.Health, error) { return bridge.Health{}, nil },\n}\n"}, "bridge.go"},
		{"native operation named inspect", map[string]string{"host.go": "package app\n\nfunc reg(srv *Server) { srv.Handle(\"inspect\", inspectHandler) }\n"}, "host.go"},
		{"MCP tool named health", map[string]string{"src/mcp.ts": "server.registerTool(\"health\", { inputSchema: {} }, run);\n"}, "src/mcp.ts"},
	}
	for _, c := range found {
		t.Run(c.name, func(t *testing.T) {
			root := scanTestWrite(t, c.files)
			res := scanTestScan(t, root, ScanLimits{})
			f := scanTestFinding(t, res, "req.deployment.inspection")
			if f.Status != readiness.StatusFound {
				t.Fatalf("status %s, want found", f.Status)
			}
			cited := false
			for _, e := range f.Evidence {
				cited = cited || string(e.Path) == c.path
			}
			if !cited {
				t.Fatalf("the evidence does not cite %s: %+v", c.path, f.Evidence)
			}
			scanVerifyEvidence(t, root, res)
		})
	}

	missing := []struct {
		name  string
		files map[string]string
	}{
		{"prose in documentation", map[string]string{"README.md": "Call GET \"/v1/health\" to check it, or inspect the logs.\n", "docs/ops.md": "The `/api/v1/inspect` route and the health tool.\n"}},
		{"look-alike OpenAPI paths", map[string]string{"api/openapi.yaml": scanTestOpenAPIHead + scanTestOpenAPIPath("/v1/healthcare", "listHealthcare") +
			scanTestOpenAPIPath("/v1/inspections", "listInspections") + scanTestOpenAPIPath("/v1/actions/status/{key}", "getActionStatus") +
			scanTestOpenAPIPath("/v1/workorders/{id}/status", "getWorkOrderStatus")}},
		{"words in a code string", map[string]string{"src/app.ts": "const msg = \"please inspect the health of the system\";\nconst p = \"/v1/healthcare\";\n"}},
		{"Health field without the SDK", map[string]string{"model.go": "package app\n\nvar r = Report{\n\tHealth: status,\n\tInspect: true,\n}\n"}},
		{"commented-out bridge handler", map[string]string{"bridge.go": nativeImport + "var cfg = bridge.Config{\n\t// Inspect: l.inspect,\n}\n"}},
		{"look-alike tool name", map[string]string{"src/mcp.ts": "server.registerTool(\"getHealthRecords\", { inputSchema: {} }, run);\n"}},
		{"other management entries", map[string]string{"package/schemas/adapters/api.yaml": "server: api\nmanagement:\n  describe: describeApplication\n  provisioning: [createTenant]\n"}},
	}
	for _, c := range missing {
		t.Run(c.name, func(t *testing.T) {
			res := scanTestScan(t, scanTestWrite(t, c.files), ScanLimits{})
			if f := scanTestFinding(t, res, "req.deployment.inspection"); f.Status != readiness.StatusMissing {
				t.Fatalf("status %s with evidence %+v, want missing", f.Status, f.Evidence)
			}
		})
	}
}
