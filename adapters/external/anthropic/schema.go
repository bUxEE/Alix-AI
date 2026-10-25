package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
)

// Strict tool use and structured outputs accept a SUBSET of JSON Schema (see
// doc.go for the sources). AlixAI's own schemas are written for local
// validation, so a schema is CONVERTED before it is sent. Unlike the OpenAI
// strict mode, the Anthropic subset allows optional properties and does not
// force them nullable, so the conversion never changes the SHAPE of an answer:
// an answer produced under the converted schema is still a valid answer to the
// original shape and nothing has to be restored. That is why a request with a
// converted schema can still be streamed.
//
// What ConvertSchema does:
//
//   - every object gets additionalProperties:false (the API requires it); an
//     object that already allows other members (true, or a schema: an open map)
//     is refused, never silently closed into something else;
//   - oneOf becomes anyOf; internal $ref/$defs/definitions are kept;
//     recursive and external references are refused;
//   - every keyword outside the kept list is removed and reported in
//     Stripped (bounds, lengths, pattern, not, if/then/else, ...). A removed
//     keyword costs only a local rejection of a bad answer by the caller, who
//     still validates against the schema it owns; a keyword the provider does
//     not support is a 400 that only a live call would show;
//   - the documented complexity limits (optional properties and unions per
//     request) are counted so that an over-complex request is refused here.
//
// The weakening is one-way: the model may be constrained more than the original
// schema says, never less, and the caller's own validation is unchanged.

const (
	// maxOptionalProps and maxUnions are the documented per-request limits of
	// strict tools and structured outputs ("Schema is too complex").
	maxOptionalProps = 24
	maxUnions        = 16
	maxSchemaDepth   = 32
)

// formats listed as supported.
var okFormats = map[string]bool{
	"date-time": true, "time": true, "date": true, "duration": true, "email": true, "hostname": true,
	"uri": true, "ipv4": true, "ipv6": true, "uuid": true,
}

// SchemaReport is what a conversion did.
type SchemaReport struct {
	// Stripped lists the keywords removed (sorted, unique).
	Stripped []string
	// OptionalProps and Unions are the complexity counts of the schema.
	OptionalProps int
	Unions        int
}

type schemaConv struct {
	root     map[string]any
	stripped map[string]bool
	optional int
	unions   int
}

func schemaFail(path, msg string) error {
	if path == "" {
		path = "$"
	}
	return providers.UnsupportedError("schema: " + path + ": " + msg)
}

// ConvertSchema converts schema (a JSON Schema object whose root is an object)
// to the subset the provider accepts.
func ConvertSchema(schema json.RawMessage) (json.RawMessage, SchemaReport, error) {
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, SchemaReport{}, schemaFail("", "not valid JSON")
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, SchemaReport{}, schemaFail("", "the root must be an object schema")
	}
	if !hasType(root, "object") {
		return nil, SchemaReport{}, schemaFail("", "the root must have type object")
	}
	c := &schemaConv{root: root, stripped: map[string]bool{}}
	if err := c.checkRefs(); err != nil {
		return nil, SchemaReport{}, err
	}
	out, err := c.node(root, "", 0)
	if err != nil {
		return nil, SchemaReport{}, err
	}
	rep := SchemaReport{OptionalProps: c.optional, Unions: c.unions}
	for k := range c.stripped {
		rep.Stripped = append(rep.Stripped, k)
	}
	sort.Strings(rep.Stripped)
	b, err := json.Marshal(out)
	if err != nil {
		return nil, SchemaReport{}, schemaFail("", "cannot be encoded")
	}
	return b, rep, nil
}

func hasType(m map[string]any, t string) bool {
	switch v := m["type"].(type) {
	case string:
		return v == t
	case []any:
		for _, x := range v {
			if s, _ := x.(string); s == t {
				return true
			}
		}
	}
	return false
}

// refTarget resolves a reference of the form #/$defs/name or
// #/definitions/name against the root.
func (c *schemaConv) refTarget(ref string) (string, map[string]any, bool) {
	for _, pfx := range []string{"#/$defs/", "#/definitions/"} {
		if name, ok := strings.CutPrefix(ref, pfx); ok && name != "" && !strings.Contains(name, "/") {
			holder := strings.TrimSuffix(strings.TrimPrefix(pfx, "#/"), "/")
			defs, _ := c.root[holder].(map[string]any)
			t, ok := defs[name].(map[string]any)
			return pfx + name, t, ok
		}
	}
	return "", nil, false
}

// refsIn collects the $ref values below v.
func refsIn(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		if r, ok := x["$ref"].(string); ok {
			*out = append(*out, r)
		}
		for _, e := range x {
			refsIn(e, out)
		}
	case []any:
		for _, e := range x {
			refsIn(e, out)
		}
	}
}

// checkRefs refuses external references and cycles among internal ones.
func (c *schemaConv) checkRefs() error {
	graph := map[string][]string{}
	var all []string
	refsIn(c.root, &all)
	for _, r := range all {
		key, target, ok := c.refTarget(r)
		if !ok {
			return schemaFail("", "a reference is external or unresolved")
		}
		if _, done := graph[key]; done {
			continue
		}
		var next []string
		refsIn(target, &next)
		graph[key] = nil
		for _, n := range next {
			k, _, ok := c.refTarget(n)
			if !ok {
				return schemaFail("", "a reference is external or unresolved")
			}
			graph[key] = append(graph[key], k)
		}
	}
	// the graph above only lists definitions reachable from a reference made
	// anywhere in the document; look for a cycle among them
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(k string) bool
	visit = func(k string) bool {
		switch state[k] {
		case visiting:
			return true
		case done:
			return false
		}
		state[k] = visiting
		for _, n := range graph[k] {
			if visit(n) {
				return true
			}
		}
		state[k] = done
		return false
	}
	for k := range graph {
		if visit(k) {
			return schemaFail("", "recursive schemas are not supported")
		}
	}
	return nil
}

// node converts one schema node. depth counts nesting levels.
func (c *schemaConv) node(v any, path string, depth int) (map[string]any, error) {
	if depth > maxSchemaDepth {
		return nil, schemaFail(path, "nested too deeply")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaFail(path, "a boolean or non-object subschema")
	}
	out := map[string]any{}
	for k, val := range m {
		switch k {
		case "type", "description", "title", "enum", "const", "$ref":
			out[k] = val
		case "format":
			if s, _ := val.(string); okFormats[s] {
				out[k] = val
			} else {
				c.stripped[k] = true
			}
		case "minItems":
			if n, ok := val.(json.Number); ok && (n.String() == "0" || n.String() == "1") {
				out[k] = val
			} else {
				c.stripped[k] = true
			}
		case "required":
			out[k] = val
		case "properties":
			props, ok := val.(map[string]any)
			if !ok {
				return nil, schemaFail(path, "properties is not an object")
			}
			conv := make(map[string]any, len(props))
			for name, sub := range props {
				s, err := c.node(sub, path+"/properties/"+name, depth+1)
				if err != nil {
					return nil, err
				}
				conv[name] = s
			}
			out[k] = conv
		case "items":
			s, err := c.node(val, path+"/items", depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = s
		case "anyOf", "oneOf", "allOf":
			alts, ok := val.([]any)
			if !ok || len(alts) == 0 {
				return nil, schemaFail(path, k+" is not a non-empty list")
			}
			conv := make([]any, len(alts))
			for i, a := range alts {
				s, err := c.node(a, fmt.Sprintf("%s/%s/%d", path, k, i), depth+1)
				if err != nil {
					return nil, err
				}
				conv[i] = s
			}
			name := k
			if k == "oneOf" {
				name = "anyOf" // exclusivity is not expressible; local validation keeps it
				c.stripped["oneOf(exclusive)"] = true
			}
			if name == "anyOf" {
				c.unions++
			}
			out[name] = conv
		case "$defs", "definitions":
			defs, ok := val.(map[string]any)
			if !ok {
				return nil, schemaFail(path, k+" is not an object")
			}
			conv := make(map[string]any, len(defs))
			for name, sub := range defs {
				s, err := c.node(sub, path+"/"+k+"/"+name, depth+1)
				if err != nil {
					return nil, err
				}
				conv[name] = s
			}
			out[k] = conv
		case "additionalProperties":
			// handled below, once the type is known
		default:
			c.stripped[k] = true
		}
	}
	if ts, ok := m["type"].([]any); ok && len(ts) > 1 {
		c.unions++
	}
	if hasType(m, "object") || m["properties"] != nil {
		switch ap := m["additionalProperties"].(type) {
		case nil:
		case bool:
			if ap {
				return nil, schemaFail(path, "an object that allows other members cannot be expressed")
			}
		default:
			return nil, schemaFail(path, "an open map (additionalProperties is a schema) cannot be expressed")
		}
		out["additionalProperties"] = false
		props, _ := m["properties"].(map[string]any)
		req := map[string]bool{}
		if rl, ok := m["required"].([]any); ok {
			for _, r := range rl {
				if s, ok := r.(string); ok {
					req[s] = true
				}
			}
		}
		for name := range props {
			if !req[name] {
				c.optional++
			}
		}
	} else if m["additionalProperties"] != nil {
		return nil, schemaFail(path, "additionalProperties on a non-object")
	}
	return out, nil
}

// checkComplexity refuses a request whose schemas together exceed the
// documented limits.
func checkComplexity(reps []SchemaReport) error {
	opt, uni := 0, 0
	for _, r := range reps {
		opt += r.OptionalProps
		uni += r.Unions
	}
	if opt > maxOptionalProps {
		return providers.UnsupportedError("schema: more than 24 optional properties in one request")
	}
	if uni > maxUnions {
		return providers.UnsupportedError("schema: more than 16 union types in one request")
	}
	return nil
}
