package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// canonicalJSON encodes v as compact JSON with object members in sorted order
// and no HTML escaping. For values decoded from JSON this is the canonical form
// of RFC 8785 for everything an MCP schema contains: objects sorted by key,
// strings as UTF-8, numbers in the shortest form of a float64.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// toolHash is the pin of one tool: the SHA-256 of the canonical JSON of its
// name, input schema and, when it has one, output schema. The description, the
// title and the annotations are not part of it: they are display text for a
// person, they never reach a model through this adapter, and an application
// may reword them without changing what the tool accepts or returns.
func toolHash(name string, inputSchema, outputSchema any) (string, error) {
	doc := map[string]any{"name": name, "input_schema": inputSchema}
	if outputSchema != nil {
		doc["output_schema"] = outputSchema
	}
	b, err := canonicalJSON(doc)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// ContractTool is a tool as the manifest's contract_sha256 sees it: its name and
// its input schema, the schema decoded from JSON (a map[string]any).
type ContractTool struct {
	Name        string
	InputSchema any
}

// ContractSHA256 computes the value of the manifest's optional contract_sha256
// for an MCP endpoint: the lower-case hex SHA-256 of the canonical JSON array
// of {"name", "input_schema"} objects, one per tool the capabilities of the
// package map on that endpoint (capability operations and entity resolvers, no
// management tool), sorted by name. A listing of the server that hashes
// differently is drift: the approved list changed, whether by a tool that
// appeared in it, one that went away, or one whose input schema changed. The
// order of the tools and of the members of a schema does not matter.
func ContractSHA256(tools []ContractTool) (string, error) {
	sorted := append([]ContractTool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	doc := make([]map[string]any, len(sorted))
	for i, t := range sorted {
		doc[i] = map[string]any{"name": t.Name, "input_schema": t.InputSchema}
	}
	b, err := canonicalJSON(doc)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// DriftKind says how a tool differs from what was approved.
type DriftKind string

const (
	// DriftSchemaChanged: the tool's input or output schema hashes differently
	// than the pin.
	DriftSchemaChanged DriftKind = "schema_changed"
	// DriftToolMissing: a pinned tool is no longer listed.
	DriftToolMissing DriftKind = "tool_missing"
	// DriftAmbiguousName: the server lists the same name twice with different
	// definitions, so no definition can be the approved one.
	DriftAmbiguousName DriftKind = "ambiguous_name"
	// DriftContractHash: the tools the package maps on an endpoint hash
	// differently than the manifest's contract_sha256.
	DriftContractHash DriftKind = "contract_hash"
)

// Drift is a finding that the server no longer offers what was approved. It is
// sticky: once found, calls to the tool (to every mapped tool of the endpoint,
// for DriftContractHash) are refused with quarantined until the adapter is
// rebuilt after a re-audit. It is the re-audit flag of master plan 4.5 and 16.4
// (a malicious server or a schema change after the audit).
type Drift struct {
	// Endpoint is the endpoint ID.
	Endpoint string
	// Tool is the tool name; empty for DriftContractHash. It is the name the
	// package maps, never text the server chose.
	Tool string
	Kind DriftKind
	// Pinned and Observed are the hashes compared (empty where there is none).
	Pinned   string
	Observed string
	// At is when the drift was found.
	At time.Time
}
