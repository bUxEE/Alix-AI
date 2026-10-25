package openapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// httpMethods are the keys of a path item that hold an operation.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace", "query"}

// ContractSHA256 is the definition of contract_sha256 for an openapi endpoint
// (the manifest's pin of the approved operation subset). It is the lower-case
// hex SHA-256 of the canonical JSON array, sorted by operationId, of
//
//	{"operation_id": ..., "method": ..., "path": ..., "path_parameters": [...], "operation": {...}}
//
// for each of the given operationIds, where the operation and the parameters
// its path item shares are the document's own, with every local $ref replaced by
// what it points to (a reference that closes a cycle stays a reference) and with
// the text meant for people left out: description, summary, title, example,
// examples, externalDocs and every x- extension. Keys are sorted. What the
// operation takes, what it answers and how it is secured are in it; the rest of
// the document is not, so adding an unrelated operation or documenting an
// existing one does not move the hash, and changing a parameter, a schema, a
// status or the path of an approved operation does.
//
// The operationIds are the ones the package maps on the endpoint: the
// operations of its capabilities and the management operations its adapter
// configuration names.
func ContractSHA256(spec []byte, operationIDs []string) (string, error) {
	var root any
	if err := yaml.Unmarshal(spec, &root); err != nil {
		return "", errors.New("the document does not parse")
	}
	rootMap, ok := stringKeys(root).(map[string]any)
	if !ok {
		return "", errors.New("the document is not an object")
	}
	ids := append([]string(nil), operationIDs...)
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return "", errors.New("an operationId is listed twice")
		}
	}
	paths, _ := rootMap["paths"].(map[string]any)
	entries := make([]any, 0, len(ids))
	left := maxInlineNodes
	for _, id := range ids {
		entry, err := subsetEntry(rootMap, paths, id, &left)
		if err != nil {
			return "", err
		}
		entries = append(entries, entry)
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return "", errors.New("the subset cannot be encoded")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// stringKeys converts the maps YAML produces into maps keyed by strings, so the
// document can be hashed whatever its syntax.
func stringKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = stringKeys(c)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[fmt.Sprint(k)] = stringKeys(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = stringKeys(c)
		}
		return out
	}
	return v
}

// subsetEntry finds one operation by its operationId and returns its entry.
func subsetEntry(root, paths map[string]any, id string, left *int) (map[string]any, error) {
	var found map[string]any
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, path := range names {
		item, _ := paths[path].(map[string]any)
		for _, m := range httpMethods {
			op, _ := item[m].(map[string]any)
			if op == nil || op["operationId"] != id {
				continue
			}
			if found != nil {
				return nil, errors.New("an operationId is defined twice")
			}
			opv, err := inlineRefs(root, op, false, 0, map[string]bool{}, left)
			if err != nil {
				return nil, err
			}
			pp, err := inlineRefs(root, item["parameters"], false, 0, map[string]bool{}, left)
			if err != nil {
				return nil, err
			}
			found = map[string]any{"operation_id": id, "method": strings.ToUpper(m), "path": path, "path_parameters": pp, "operation": opv}
		}
	}
	if found == nil {
		return nil, errors.New("the document defines no such operationId")
	}
	return found, nil
}

// nameMaps are the keys whose value is a map from names to definitions: inside
// one, the keys are names chosen by the author and are never text for people.
var nameMaps = map[string]bool{
	"properties": true, "patternProperties": true, "dependentSchemas": true, "$defs": true, "definitions": true,
	"content": true, "responses": true, "headers": true, "links": true, "callbacks": true, "encoding": true,
	"mapping": true, "variables": true, "schemas": true, "securitySchemes": true,
}

// dataKeys hold instance data, which is kept as it is.
var dataKeys = map[string]bool{"enum": true, "const": true, "default": true}

// annotation reports whether a key of a definition is text for people.
func annotation(k string) bool {
	switch k {
	case "description", "summary", "title", "example", "examples", "externalDocs":
		return true
	}
	return strings.HasPrefix(k, "x-")
}

const maxInlineDepth = 64

// maxInlineNodes bounds what the approved operations, all of them together,
// expand to once their references are resolved, in nodes (a string or a key
// counts one more for every 32 bytes): a schema reachable by several paths is
// copied on each, so a small document could otherwise expand without end.
const maxInlineNodes = 1 << 20

// spend takes a node holding text bytes of strings from the budget left.
func spend(left *int, text int) error {
	if *left -= 1 + text/32; *left < 0 {
		return errors.New("the approved operations expand to more than the adapter reads")
	}
	return nil
}

// spendData takes a value that is kept as it is (instance data) from the budget
// left: the copy repeats it all the same.
func spendData(left *int, v any) error {
	s, _ := v.(string)
	if err := spend(left, len(s)); err != nil {
		return err
	}
	switch x := v.(type) {
	case []any:
		for _, c := range x {
			if err := spendData(left, c); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, c := range x {
			*left -= len(k) / 32
			if err := spendData(left, c); err != nil {
				return err
			}
		}
	}
	return nil
}

// inlineRefs copies a node with its local references resolved and its annotations
// dropped. nameMap says that the node's keys are names, not keywords. left is
// what the copy may still take (see maxInlineNodes).
func inlineRefs(root map[string]any, v any, nameMap bool, depth int, active map[string]bool, left *int) (any, error) {
	if depth > maxInlineDepth {
		return nil, errors.New("the document nests deeper than the adapter reads")
	}
	s, _ := v.(string)
	if err := spend(left, len(s)); err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			r, err := inlineRefs(root, c, false, depth+1, active, left)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok && !nameMap {
			return inlineRef(root, x, ref, depth, active, left)
		}
		out := make(map[string]any, len(x))
		for k, c := range x {
			if !nameMap && annotation(k) {
				continue
			}
			*left -= len(k) / 32
			if !nameMap && dataKeys[k] {
				if err := spendData(left, c); err != nil {
					return nil, err
				}
				out[k] = c
				continue
			}
			r, err := inlineRefs(root, c, !nameMap && nameMaps[k], depth+1, active, left)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	}
	return v, nil
}

func inlineRef(root, node map[string]any, ref string, depth int, active map[string]bool, left *int) (any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, errors.New("the document holds a reference that is not local")
	}
	if active[ref] {
		if err := spend(left, len(ref)); err != nil {
			return nil, err
		}
		return map[string]any{"$ref": ref}, nil // a cycle: the reference itself stands for it
	}
	target, err := pointer(root, ref[2:])
	if err != nil {
		return nil, err
	}
	active[ref] = true
	defer delete(active, ref)
	resolved, err := inlineRefs(root, target, false, depth+1, active, left)
	if err != nil {
		return nil, err
	}
	m, isMap := resolved.(map[string]any)
	if !isMap {
		return resolved, nil
	}
	for k, c := range node { // siblings of $ref (OpenAPI 3.1)
		if k == "$ref" || annotation(k) {
			continue
		}
		*left -= len(k) / 32
		r, err := inlineRefs(root, c, false, depth+1, active, left)
		if err != nil {
			return nil, err
		}
		m[k] = r
	}
	return m, nil
}

// pointer follows a JSON pointer (without its leading "#/") in the document.
func pointer(root map[string]any, p string) (any, error) {
	var cur any = root
	for _, tok := range strings.Split(p, "/") {
		t, err := url.PathUnescape(tok)
		if err != nil {
			return nil, errors.New("a reference is malformed")
		}
		t = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
		switch x := cur.(type) {
		case map[string]any:
			next, ok := x[t]
			if !ok {
				return nil, errors.New("a reference points at nothing")
			}
			cur = next
		default:
			return nil, errors.New("a reference points at nothing")
		}
	}
	return cur, nil
}
