package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Strict structured outputs accept only a subset of JSON Schema (see doc.go for
// the sources). AlixAI's own schemas are written for local validation, so a
// schema is CONVERTED before it is sent, and the model's answer is converted
// BACK before anyone sees it. The conversion only ever narrows what the model
// may write; the caller still validates the restored answer against the schema
// it owns, so nothing here weakens a local check.
//
// What ConvertStrict does:
//
//   - every object gets additionalProperties:false and lists every property in
//     required; a property that was optional becomes nullable, and a null for it
//     is dropped again by Restore;
//   - an object used as an open map (additionalProperties is a schema) becomes
//     an array of {key, value} entries, and a free-form object becomes a string
//     that holds JSON; Restore reverses both;
//   - internal $ref/$defs are inlined (recursion and external references are
//     refused), oneOf becomes anyOf, enum/const get a type;
//   - every keyword outside the kept list below is removed and reported in
//     Stripped (pattern, format, length, numeric and size bounds, allOf, if/then,
//     not, propertyNames and so on). The two readings of the vendor page
//     disagreed about the bounds, so the conservative reading is used; a
//     removed keyword costs only a local rejection of a bad answer, whereas a
//     keyword the provider does not support is a 400 that only a live call
//     would show.
//
// A schema that cannot be converted without ambiguity is refused, never sent
// with strict switched off: the capability record promises strict_schema.

const (
	strictMaxDepth      = 10
	strictMaxProperties = 5000
	strictMaxEnum       = 1000
	strictMaxNameChars  = 120000
)

// strictKept are the keywords a converted schema may contain.
var strictKept = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "enum": true, "anyOf": true, "description": true, "title": true,
}

type snodeKind int

const (
	skScalar snodeKind = iota
	skObject
	skArray
	skEntries
	skJSONString
	skUnion
)

type snode struct {
	kind     snodeKind
	props    map[string]*snode
	optional map[string]bool
	elem     *snode
	// nullOK: the ORIGINAL schema accepts null here, so a null is data, not
	// the marker of an absent optional property.
	nullOK bool
	// changed: Restore has work to do somewhere in this subtree.
	changed bool
}

// StrictPlan is the record of one conversion, used to restore an answer.
type StrictPlan struct {
	root *snode
	// Stripped lists the keywords that were removed, sorted.
	Stripped []string
}

// Identity reports whether the converted schema describes the same shape as
// the original (only strictness keywords were added), so an answer needs no
// restoration. Streamed output is allowed only in that case.
func (p *StrictPlan) Identity() bool { return p == nil || p.root == nil || !p.root.changed }

type strictConv struct {
	root     map[string]any
	stripped map[string]bool
	stack    []string
	props    int
	enums    int
	chars    int
}

func strictFail(path, msg string) error {
	return contracts.New(contracts.ErrValidationFailed, "openai: schema cannot be sent as a strict structured output: "+msg).
		WithDetails("unsupported", "strict_schema").WithDetails("schema_path", path)
}

// ConvertStrict converts schema (a JSON Schema object) to the strict subset and
// returns the plan that restores an answer.
func ConvertStrict(schema json.RawMessage) (json.RawMessage, *StrictPlan, error) {
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return nil, nil, strictFail("", "the schema is not a JSON object")
	}
	c := &strictConv{root: root, stripped: map[string]bool{}}
	out, n, err := c.conv(root, "", 1)
	if err != nil {
		return nil, nil, err
	}
	if n.kind != skObject {
		return nil, nil, strictFail("", "the root must be an object")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, nil, contracts.Wrap(contracts.ErrInternal, err, "openai: cannot encode the converted schema")
	}
	if v := CheckStrict(raw); len(v) > 0 {
		return nil, nil, strictFail("", "conversion result is not strict: "+v[0])
	}
	plan := &StrictPlan{root: n}
	for k := range c.stripped {
		plan.Stripped = append(plan.Stripped, k)
	}
	sort.Strings(plan.Stripped)
	return raw, plan, nil
}

func (c *strictConv) strip(m map[string]any) {
	for k := range m {
		if !strictKept[k] && k != "$ref" && k != "oneOf" && k != "const" && k != "$defs" && k != "definitions" &&
			k != "$schema" && k != "$id" && k != "$comment" && !strings.HasPrefix(k, "x-") {
			c.stripped[k] = true
		}
	}
}

func typeList(m map[string]any) []string {
	switch t := m["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, v := range t {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func hasType(ts []string, t string) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func withoutNull(ts []string) (rest []string, hadNull bool) {
	for _, t := range ts {
		if t == "null" {
			hadNull = true
		} else {
			rest = append(rest, t)
		}
	}
	return rest, hadNull
}

func typeValue(ts []string) any {
	if len(ts) == 1 {
		return ts[0]
	}
	out := make([]any, len(ts))
	for i, t := range ts {
		out[i] = t
	}
	return out
}

func (c *strictConv) resolveRef(ref, path string) (map[string]any, error) {
	var base string
	switch {
	case strings.HasPrefix(ref, "#/$defs/"):
		base = "$defs"
	case strings.HasPrefix(ref, "#/definitions/"):
		base = "definitions"
	default:
		return nil, strictFail(path, "an external or non-local $ref cannot be resolved")
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	defs, _ := c.root[base].(map[string]any)
	d, ok := defs[name].(map[string]any)
	if !ok || strings.Contains(strings.TrimPrefix(ref, "#/"+base+"/"), "/") {
		return nil, strictFail(path, "a $ref names no definition")
	}
	for _, s := range c.stack {
		if s == ref {
			return nil, strictFail(path, "a recursive $ref cannot be inlined")
		}
	}
	return d, nil
}

// conv converts one schema node. depth counts object/array nesting levels.
func (c *strictConv) conv(s any, path string, depth int) (any, *snode, error) {
	m, ok := s.(map[string]any)
	if !ok {
		return nil, nil, strictFail(path, "a boolean or non-object subschema")
	}
	if depth > strictMaxDepth {
		return nil, nil, strictFail(path, "nesting is deeper than the provider allows")
	}
	if ref, ok := m["$ref"].(string); ok {
		d, err := c.resolveRef(ref, path)
		if err != nil {
			return nil, nil, err
		}
		c.stack = append(c.stack, ref)
		defer func() { c.stack = c.stack[:len(c.stack)-1] }()
		merged := map[string]any{}
		for k, v := range d {
			merged[k] = v
		}
		if desc, ok := m["description"]; ok {
			merged["description"] = desc
		}
		m = merged
	}
	c.strip(m)

	alts, _ := m["anyOf"].([]any)
	if one, ok := m["oneOf"].([]any); ok && len(alts) == 0 {
		alts = one
	}
	if len(alts) > 0 {
		return c.union(m, alts, path, depth)
	}
	if cv, ok := m["const"]; ok {
		m = copyMap(m)
		m["enum"] = []any{cv}
	}
	ts := typeList(m)
	enum, _ := m["enum"].([]any)
	if len(ts) == 0 {
		switch {
		case len(enum) > 0:
			ts = enumTypes(enum)
		case m["properties"] != nil:
			ts = []string{"object"}
		case m["items"] != nil:
			ts = []string{"array"}
		default:
			return nil, nil, strictFail(path, "a subschema with no type")
		}
	}
	core, hadNull := withoutNull(ts)
	if len(core) > 1 {
		for _, t := range core {
			if t == "object" || t == "array" {
				return nil, nil, strictFail(path, "a type list that mixes an object or array with another type")
			}
		}
	}
	out := map[string]any{}
	for _, k := range []string{"description", "title"} {
		if v, ok := m[k].(string); ok {
			out[k] = v
		}
	}
	n := &snode{nullOK: hadNull}
	switch {
	case len(core) == 1 && core[0] == "object":
		o, on, err := c.object(m, path, depth)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range o {
			out[k] = v
		}
		n = on
		n.nullOK = hadNull
		if n.kind == skJSONString {
			out["type"] = typeValue(appendNull([]string{"string"}, hadNull))
		} else if n.kind == skEntries {
			out["type"] = typeValue(appendNull([]string{"array"}, hadNull))
		} else {
			out["type"] = typeValue(appendNull([]string{"object"}, hadNull))
		}
	case len(core) == 1 && core[0] == "array":
		items, ok := m["items"]
		if !ok {
			return nil, nil, strictFail(path, "an array with no items schema")
		}
		ie, in, err := c.conv(items, path+"/items", depth+1)
		if err != nil {
			return nil, nil, err
		}
		out["type"] = typeValue(appendNull([]string{"array"}, hadNull))
		out["items"] = ie
		n.kind, n.elem, n.changed = skArray, in, in.changed
	default:
		for _, t := range core {
			switch t {
			case "string", "number", "integer", "boolean":
			default:
				return nil, nil, strictFail(path, "unsupported type "+fmt.Sprintf("%q", t))
			}
		}
		if len(core) == 0 && !hadNull {
			return nil, nil, strictFail(path, "a subschema with no type")
		}
		out["type"] = typeValue(ts)
		if len(enum) > 0 {
			c.enums += len(enum)
			if c.enums > strictMaxEnum {
				return nil, nil, strictFail(path, "more enum values than the provider allows")
			}
			for _, e := range enum {
				if s, ok := e.(string); ok {
					c.chars += len(s)
				}
			}
			out["enum"] = enum
		}
		n.kind = skScalar
	}
	if c.chars > strictMaxNameChars {
		return nil, nil, strictFail(path, "names and enum values exceed the provider's size limit")
	}
	return out, n, nil
}

func appendNull(ts []string, hadNull bool) []string {
	if hadNull && !hasType(ts, "null") {
		return append(ts, "null")
	}
	return ts
}

func copyMap(m map[string]any) map[string]any {
	o := make(map[string]any, len(m))
	for k, v := range m {
		o[k] = v
	}
	return o
}

func enumTypes(enum []any) []string {
	seen := map[string]bool{}
	var ts []string
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			ts = append(ts, t)
		}
	}
	for _, e := range enum {
		switch v := e.(type) {
		case string:
			add("string")
		case bool:
			add("boolean")
		case nil:
			add("null")
		case json.Number:
			if strings.ContainsAny(v.String(), ".eE") {
				add("number")
			} else {
				add("integer")
			}
		default:
			add("object")
		}
	}
	sort.Strings(ts)
	return ts
}

func (c *strictConv) union(m map[string]any, alts []any, path string, depth int) (any, *snode, error) {
	out := map[string]any{}
	if d, ok := m["description"].(string); ok {
		out["description"] = d
	}
	n := &snode{kind: skUnion}
	conv := make([]any, 0, len(alts))
	for i, a := range alts {
		ae, an, err := c.conv(a, fmt.Sprintf("%s/anyOf/%d", path, i), depth)
		if err != nil {
			return nil, nil, err
		}
		if an.changed || an.kind == skEntries || an.kind == skJSONString {
			return nil, nil, strictFail(path, "a union whose alternative needs a shape conversion cannot be restored unambiguously")
		}
		if an.nullOK {
			n.nullOK = true
		}
		conv = append(conv, ae)
	}
	out["anyOf"] = conv
	return out, n, nil
}

func (c *strictConv) object(m map[string]any, path string, depth int) (map[string]any, *snode, error) {
	props, _ := m["properties"].(map[string]any)
	ap := m["additionalProperties"]
	apSchema, apIsSchema := ap.(map[string]any)
	if len(props) == 0 {
		switch {
		case apIsSchema && len(apSchema) > 0:
			ve, vn, err := c.conv(apSchema, path+"/additionalProperties", depth+2)
			if err != nil {
				return nil, nil, err
			}
			entry := map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"key", "value"},
				"properties":           map[string]any{"key": map[string]any{"type": "string"}, "value": ve},
			}
			c.props += 2
			return map[string]any{"items": entry}, &snode{kind: skEntries, elem: vn, changed: true}, nil
		case ap == nil || ap == true || (apIsSchema && len(apSchema) == 0):
			return map[string]any{"description": "A JSON object, encoded as a string."}, &snode{kind: skJSONString, changed: true}, nil
		}
		// additionalProperties:false and no properties: the empty object.
		return map[string]any{"properties": map[string]any{}, "required": []any{}, "additionalProperties": false}, &snode{kind: skObject}, nil
	}
	reqSet := map[string]bool{}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				reqSet[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	outProps := make(map[string]any, len(props))
	required := make([]any, 0, len(props))
	n := &snode{kind: skObject, props: map[string]*snode{}, optional: map[string]bool{}}
	for _, name := range names {
		c.props++
		c.chars += len(name)
		if c.props > strictMaxProperties {
			return nil, nil, strictFail(path, "more properties than the provider allows")
		}
		pe, pn, err := c.conv(props[name], path+"/properties/"+name, depth+1)
		if err != nil {
			return nil, nil, err
		}
		if !reqSet[name] {
			n.optional[name] = true
			n.changed = true
			if !pn.nullOK {
				pe = makeNullable(pe.(map[string]any))
			}
		}
		if pn.changed {
			n.changed = true
		}
		n.props[name] = pn
		outProps[name] = pe
		required = append(required, name)
	}
	return map[string]any{"properties": outProps, "required": required, "additionalProperties": false}, n, nil
}

// makeNullable lets a property that was optional carry null instead of being
// absent.
func makeNullable(p map[string]any) map[string]any {
	if alts, ok := p["anyOf"].([]any); ok {
		p["anyOf"] = append(alts, map[string]any{"type": "null"})
		return p
	}
	ts := typeList(p)
	if !hasType(ts, "null") {
		p["type"] = typeValue(append(ts, "null"))
	}
	if enum, ok := p["enum"].([]any); ok {
		has := false
		for _, e := range enum {
			if e == nil {
				has = true
			}
		}
		if !has {
			p["enum"] = append(append([]any(nil), enum...), nil)
		}
	}
	return p
}

// Restore converts an answer produced under the converted schema back to the
// shape of the original schema. An answer that does not fit the plan is
// schema_invalid; the error never repeats the answer.
func (p *StrictPlan) Restore(doc json.RawMessage) (json.RawMessage, error) {
	if p.Identity() {
		return doc, nil
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, schemaInvalid("structured output is not valid JSON")
	}
	r, err := restore(v, p.root)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(r)
	if err != nil {
		return nil, schemaInvalid("structured output cannot be re-encoded")
	}
	return out, nil
}

func schemaInvalid(msg string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "openai: "+msg)
}

func restore(v any, n *snode) (any, error) {
	if v == nil {
		if n.nullOK {
			return nil, nil
		}
		return nil, schemaInvalid("structured output holds a null where the schema allows none")
	}
	switch n.kind {
	case skObject:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, schemaInvalid("structured output does not match the schema shape")
		}
		out := make(map[string]any, len(m))
		for k, x := range m {
			cn, known := n.props[k]
			if !known {
				return nil, schemaInvalid("structured output has a property the schema does not declare")
			}
			if x == nil && n.optional[k] && !cn.nullOK {
				continue
			}
			r, err := restore(x, cn)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case skArray:
		a, ok := v.([]any)
		if !ok {
			return nil, schemaInvalid("structured output does not match the schema shape")
		}
		out := make([]any, len(a))
		for i, x := range a {
			r, err := restore(x, n.elem)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case skEntries:
		a, ok := v.([]any)
		if !ok {
			return nil, schemaInvalid("structured output does not match the schema shape")
		}
		out := make(map[string]any, len(a))
		for _, e := range a {
			em, ok := e.(map[string]any)
			key, kok := em["key"].(string)
			if !ok || !kok || len(em) != 2 {
				return nil, schemaInvalid("structured output holds a malformed map entry")
			}
			if _, dup := out[key]; dup {
				return nil, schemaInvalid("structured output repeats a map key")
			}
			r, err := restore(em["value"], n.elem)
			if err != nil {
				return nil, err
			}
			out[key] = r
		}
		return out, nil
	case skJSONString:
		s, ok := v.(string)
		if !ok {
			return nil, schemaInvalid("structured output does not match the schema shape")
		}
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		var o map[string]any
		if err := dec.Decode(&o); err != nil || o == nil || dec.More() {
			return nil, schemaInvalid("an embedded JSON object is malformed")
		}
		return o, nil
	}
	return v, nil // scalars and unions need no restoration
}

// CheckStrict reports every way schema falls outside the provider's strict
// subset, as short path-prefixed strings; none means it is acceptable. It
// checks the documented rules: an object root; additionalProperties:false and
// every property required on each object; only kept keywords; a type (or
// anyOf) on each subschema; at most 10 levels and 5000 properties.
func CheckStrict(schema json.RawMessage) []string {
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil || root == nil {
		return []string{"(root): not a JSON object"}
	}
	var out []string
	props := 0
	var walk func(s any, path string, depth int)
	walk = func(s any, path string, depth int) {
		m, ok := s.(map[string]any)
		if !ok {
			out = append(out, path+": not an object schema")
			return
		}
		if depth > strictMaxDepth {
			out = append(out, path+": nested deeper than 10 levels")
			return
		}
		for k := range m {
			if !strictKept[k] {
				out = append(out, path+": keyword "+k+" is not in the strict subset")
			}
		}
		if alts, ok := m["anyOf"].([]any); ok {
			for i, a := range alts {
				walk(a, fmt.Sprintf("%s/anyOf/%d", path, i), depth)
			}
			return
		}
		ts := typeList(m)
		if len(ts) == 0 {
			out = append(out, path+": no type")
			return
		}
		if hasType(ts, "object") {
			ps, _ := m["properties"].(map[string]any)
			if m["additionalProperties"] != false {
				out = append(out, path+": additionalProperties is not false")
			}
			req := map[string]bool{}
			if r, ok := m["required"].([]any); ok {
				for _, x := range r {
					if s, ok := x.(string); ok {
						req[s] = true
					}
				}
			}
			for name, ps2 := range ps {
				props++
				if !req[name] {
					out = append(out, path+"/properties/"+name+": not listed in required")
				}
				walk(ps2, path+"/properties/"+name, depth+1)
			}
		}
		if hasType(ts, "array") {
			if it, ok := m["items"]; ok {
				walk(it, path+"/items", depth+1)
			} else {
				out = append(out, path+": array without items")
			}
		}
	}
	if ts := typeList(root); !hasType(ts, "object") {
		out = append(out, "(root): the root must be an object")
	}
	walk(root, "", 1)
	if props > strictMaxProperties {
		out = append(out, "(root): more than 5000 properties")
	}
	sort.Strings(out)
	return out
}
