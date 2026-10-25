package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
)

// responseJsonSchema (structured output) and parametersJsonSchema (function
// declarations) accept a SUBSET of JSON Schema. The vendor's page lists the
// types, properties / required / additionalProperties, items / minItems /
// maxItems, enum, minimum / maximum, title / description, format (date-time,
// date, time), anyOf and the recursive reference "$ref": "#" (see doc.go; read
// 2026-10-08 as summarised, not verified). AlixAI's own schemas are written for
// local validation, so a schema is CONVERTED before it is sent. Like the
// Anthropic conversion, and unlike the OpenAI strict mode, it never changes the
// SHAPE of an answer: optional properties stay optional and nothing has to be
// restored, so a request with a converted schema can still be streamed.
//
// What ConvertSchema does:
//
//   - every object gets additionalProperties:false; an object that already
//     allows other members (true, or a schema: an open map) is refused, never
//     silently closed into something else;
//   - internal $ref / $defs / definitions are INLINED (the documented
//     reference form is the root-recursive "#" only); recursive and external
//     references are refused;
//   - const becomes a one-member enum; oneOf becomes anyOf (exclusivity is not
//     expressible and is reported); allOf and a boolean subschema are refused;
//   - every other keyword outside the kept list is removed and reported in
//     Stripped (pattern, string lengths, exclusive bounds, uniqueItems, not,
//     if/then/else, default, ...). A removed keyword costs only a local
//     rejection of a bad answer by the caller, who still validates against the
//     schema it owns; a keyword the provider does not support is a 400 that
//     only a live call would show.
//
// The weakening is one-way: the model may be constrained more than the original
// schema says, never less, and the caller's own validation is unchanged.

const (
	maxSchemaDepth = 32
	// maxSchemaNodes bounds the size of the converted document after inlining
	// references, so that a reference fan-out cannot blow up the request.
	maxSchemaNodes = 4000
)

// formats listed as supported.
var okFormats = map[string]bool{"date-time": true, "date": true, "time": true}

// SchemaReport is what a conversion did.
type SchemaReport struct {
	// Stripped lists the keywords removed (sorted, unique).
	Stripped []string
	// Inlined is the number of references that were expanded in place.
	Inlined int
}

type schemaConv struct {
	root     map[string]any
	stripped map[string]bool
	inlined  int
	nodes    int
	// active holds the definitions being expanded, to refuse a cycle.
	active map[string]bool
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
	c := &schemaConv{root: root, stripped: map[string]bool{}, active: map[string]bool{}}
	out, err := c.node(root, "", 0)
	if err != nil {
		return nil, SchemaReport{}, err
	}
	rep := SchemaReport{Inlined: c.inlined}
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

func asInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return int(i), err == nil && i >= 0
}

// node converts one schema node. depth counts nesting levels.
func (c *schemaConv) node(v any, path string, depth int) (map[string]any, error) {
	if depth > maxSchemaDepth {
		return nil, schemaFail(path, "nested too deeply")
	}
	if c.nodes++; c.nodes > maxSchemaNodes {
		return nil, schemaFail(path, "too large once references are expanded")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, schemaFail(path, "a boolean or non-object subschema")
	}
	out := map[string]any{}
	if ref, has := m["$ref"]; has {
		s, _ := ref.(string)
		key, target, ok := c.refTarget(s)
		if !ok {
			return nil, schemaFail(path, "a reference is external or unresolved")
		}
		if c.active[key] {
			return nil, schemaFail(path, "recursive schemas are not supported")
		}
		c.active[key] = true
		inl, err := c.node(target, path+"/"+key, depth+1)
		delete(c.active, key)
		if err != nil {
			return nil, err
		}
		c.inlined++
		for k, val := range inl {
			out[k] = val
		}
	}
	for k, val := range m {
		switch k {
		case "$ref", "$defs", "definitions":
			// expanded in place above / only the target of a reference
		case "type", "description", "title", "enum", "minimum", "maximum":
			out[k] = val
		case "const":
			out["enum"] = []any{val}
		case "format":
			if s, _ := val.(string); okFormats[s] {
				out[k] = val
			} else {
				c.stripped[k] = true
			}
		case "minItems", "maxItems":
			if _, ok := asInt(val); ok {
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
		case "allOf":
			return nil, schemaFail(path, "allOf cannot be expressed")
		case "anyOf", "oneOf":
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
			if k == "oneOf" {
				c.stripped["oneOf(exclusive)"] = true // exclusivity is not expressible; local validation keeps it
			}
			out["anyOf"] = conv
		case "additionalProperties":
			// handled below, once the type is known
		default:
			c.stripped[k] = true
		}
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
	} else if m["additionalProperties"] != nil {
		return nil, schemaFail(path, "additionalProperties on a non-object")
	}
	return out, nil
}
