package scaffold

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

// scaffoldNativeWorld is the standard world with the MCP evidence replaced by a verified native adapter in a Go file.
func scaffoldNativeWorld(file, content string) *scaffoldWorld {
	w := scaffoldNewWorld()
	w.files[file] = content
	w.fsys[file] = &fstest.MapFile{Data: []byte(content)}
	w.in.Report.Findings = slices.DeleteFunc(w.in.Report.Findings, func(f readiness.Finding) bool { return f.RequirementID == "req.mcp.tool_list" })
	w.in.Report.Findings = append(w.in.Report.Findings,
		scaffoldFinding("0000000000000009", "req.native.adapter", readiness.StatusFound, readiness.SourceDeterministic, w.files, file))
	w.in.Inventory.MCPTools = nil
	return w
}

// A project whose verified transport is a native bridge gets a native endpoint, in the language of the file the evidence
// cites, with local IPC context exchange: not an MCP placeholder. The draft stays unsigned and the loader refuses it.
func TestScaffoldNativeTransportGetsANativeEndpoint(t *testing.T) {
	for _, c := range []struct{ file, bridge string }{
		{"bridge.go", "go"}, {"src/host.ts", "typescript"}, {"app/host.py", "python"}, {"Host.java", "other"},
	} {
		t.Run(c.bridge, func(t *testing.T) {
			w := scaffoldNativeWorld(c.file, "uses the AlixAI bridge\n")
			out := filepath.Join(t.TempDir(), "draft")
			scaffoldMust(t, out, w.in)
			man := scaffoldReadTree(t, out)["alixai.yaml"]
			if strings.Contains(man, "could not be determined") {
				t.Fatalf("the native transport is reported as undetermined:\n%s", man)
			}
			body := scaffoldUncomment(man)
			for _, want := range []string{"transport: native\n", "bridge: " + c.bridge + "\n", "kind: loopback_tcp\n", "mode: local_ipc\n"} {
				if !strings.Contains(body, want) {
					t.Errorf("the manifest does not say %q:\n%s", strings.TrimSpace(want), man)
				}
			}
			if strings.Contains(body, "transport: mcp") || strings.Contains(body, "mode: context_token") {
				t.Errorf("the native draft carries the MCP template:\n%s", man)
			}
			if !strings.Contains(man, "verified in: "+c.file+" lines 1-1") {
				t.Errorf("the endpoint does not cite the verified native evidence:\n%s", man)
			}
		})
	}

	w := scaffoldNativeWorld("bridge.go", "uses the AlixAI bridge\n")
	out := filepath.Join(t.TempDir(), "draft")
	scaffoldMust(t, out, w.in)
	load := func() map[string]bool {
		res, err := apppkg.Load(context.Background(), apppkg.FromDir(out), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
		if err != nil {
			t.Fatal(err)
		}
		if res.Package != nil || res.OK() {
			t.Fatal("the loader let the native draft through")
		}
		return scaffoldCritical(res)
	}
	if crit := load(); !crit["signature.missing"] {
		t.Fatalf("the loader does not refuse the native draft as unsigned: %v", crit)
	}
	// completed with native method bindings: no schema or manifest error is left, and it is still refused as unsigned
	scaffoldComplete(t, out, 2, "catalog.find_title", true)
	for _, rel := range []string{"capabilities/cap-001.yaml", "capabilities/cap-002.yaml"} {
		p := filepath.Join(out, filepath.FromSlash(rel))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), "mcp_tool: a_tool", "native_method: a_tool", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	crit := load()
	for id := range crit {
		if strings.HasPrefix(id, "schema.") || strings.HasPrefix(id, "yaml.") || strings.HasPrefix(id, "manifest.") {
			t.Errorf("the completed native draft still fails %s", id)
		}
	}
	if !crit["signature.missing"] {
		t.Fatalf("the completed native draft is not refused as unsigned: %v", crit)
	}
}
