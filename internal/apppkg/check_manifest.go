package apppkg

import (
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/trust"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// crossCheck runs the checks that tie the files of the package together
// (docs/integration/application-package.md section 14 items 4 to 16).
func (md *model) crossCheck() {
	md.checkManifest()
	md.checkCapabilities()
	md.checkDocs()
	md.checkContracts()
	md.checkPolicies()
	md.checkTerminology()
	md.checkSources()
	md.checkEgress()
	md.checkTests()
}

// transportOf returns the transport of a declared endpoint.
func (md *model) transportOf(id string) (string, bool) {
	ep, ok := md.m.Endpoints[id]
	if !ok {
		return "", false
	}
	switch ep.Value.(type) {
	case ap.ManifestMCPEndpoint:
		return "mcp", true
	case ap.ManifestOpenAPIEndpoint:
		return "openapi", true
	case ap.ManifestNativeEndpoint:
		return "native", true
	case ap.ManifestEventsEndpoint:
		return "events", true
	}
	return "", false
}

func (md *model) endpointIDs() []string {
	ids := make([]string, 0, len(md.m.Endpoints))
	for id := range md.m.Endpoints {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// callKey says which call key an adapter call uses: "mcp", "openapi" or
// "native"; n is how many keys are present (the schema requires exactly one).
func callKey(c ap.AdapterCall) (key string, n int) {
	if c.MCPTool != nil {
		key, n = "mcp", n+1
	}
	if c.OpenAPIOperationID != nil {
		key, n = "openapi", n+1
	}
	if c.NativeMethod != nil {
		key, n = "native", n+1
	}
	return key, n
}

// operationName returns the name an adapter call maps to.
func operationName(c ap.AdapterCall) string {
	switch {
	case c.MCPTool != nil:
		return string(*c.MCPTool)
	case c.OpenAPIOperationID != nil:
		return string(*c.OpenAPIOperationID)
	case c.NativeMethod != nil:
		return string(*c.NativeMethod)
	}
	return ""
}

func bindingCall(b ap.CapabilityOperationBinding) ap.AdapterCall {
	return ap.AdapterCall{Endpoint: b.Endpoint, MCPTool: b.MCPTool, OpenAPIOperationID: b.OpenAPIOperationID, NativeMethod: b.NativeMethod}
}

// callProblem returns why an adapter call does not fit the manifest, or "".
// It does not report; callers choose the check.
func (md *model) callProblem(c ap.AdapterCall) string {
	tr, ok := md.transportOf(string(c.Endpoint))
	if !ok {
		return "endpoint " + q(string(c.Endpoint)) + " is not declared in the manifest"
	}
	key, n := callKey(c)
	switch {
	case tr == "events":
		return "endpoint " + q(string(c.Endpoint)) + " is an events endpoint, which can never own an operation"
	case n != 1:
		return "the call must name exactly one of mcp_tool, openapi_operation_id or native_method"
	case key != tr:
		return "the call uses the " + key + " key but endpoint " + q(string(c.Endpoint)) + " is " + tr
	}
	return ""
}

// schemaFileExists reports whether a schema reference (a path below schemas/
// with an optional JSON Pointer fragment) names a file of the package.
func (md *model) schemaFileExists(ref string) bool {
	p := ref
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	return md.snap.Has(p)
}

func (md *model) checkManifest() {
	if !md.langs[string(md.m.Languages.Default)] {
		md.c.add(chkLangDefault, "", trust.ManifestFile, "languages.default %s is not in languages.supported", q(string(md.m.Languages.Default)))
	}

	for _, id := range md.endpointIDs() {
		var adapterConfig, spec, rawURL string
		switch v := md.m.Endpoints[id].Value.(type) {
		case ap.ManifestMCPEndpoint:
			adapterConfig, rawURL = string(v.AdapterConfig), string(v.URL)
		case ap.ManifestOpenAPIEndpoint:
			adapterConfig, spec, rawURL = string(v.AdapterConfig), string(v.Spec), string(v.BaseURL)
		case ap.ManifestNativeEndpoint:
			adapterConfig = string(v.AdapterConfig)
		case ap.ManifestEventsEndpoint:
			adapterConfig = string(v.AdapterConfig)
			if v.URL != nil {
				rawURL = string(*v.URL)
			}
		}
		if adapterConfig != "" && !md.schemaFileExists(adapterConfig) {
			md.c.add(chkEndpointFile, "", trust.ManifestFile, "endpoint %s: adapter_config %s does not exist in the package", q(id), q(adapterConfig))
		}
		if spec != "" && !md.schemaFileExists(spec) {
			md.c.add(chkEndpointFile, "", trust.ManifestFile, "endpoint %s: spec %s does not exist in the package", q(id), q(spec))
		}
		if rawURL != "" {
			if !md.egressAllows(rawURL, md.m.Network.Egress, nil) {
				md.c.add(chkEgress, "", trust.ManifestFile, "endpoint %s: the host and port of %s have no matching network.egress entry (exact host, port and scheme are required, loopback hosts included)", q(id), q(rawURL))
			}
		}
	}
}

// egressAllows reports whether rawURL may be reached: it must be covered by an
// entry of network.egress (by exact host, port and scheme). A loopback host is
// no exception (PT-001): the operator sees every loopback port a package asks
// for, and the platform's own ports are refused at the dialer even when listed.
// When only is not nil it limits the entries considered to those IDs.
func (md *model) egressAllows(rawURL string, entries map[string]ap.ManifestEgressEntry, only map[string]bool) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	pn, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	for id, e := range entries {
		if only != nil && !only[id] {
			continue
		}
		if strings.ToLower(string(e.Host)) != host || string(e.Scheme) != scheme {
			continue
		}
		for _, p := range e.Ports {
			if int(p) == pn {
				return true
			}
		}
	}
	return false
}
