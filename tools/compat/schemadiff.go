package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Documents are decoded with json.Number so large integers survive.
type object = map[string]any

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after the JSON document")
	}
	return v, nil
}

// canon is the canonical JSON text of a value (object keys sorted by encoding/json).
func canon(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

// docKeywords are annotations: changing them never changes what is valid.
var docKeywords = map[string]bool{
	"title": true, "description": true, "$comment": true, "examples": true, "example": true,
	"deprecated": true, "readOnly": true, "writeOnly": true, "externalDocs": true, "summary": true,
}

// lowerBounds tighten when they grow; upperBounds tighten when they shrink.
var lowerBounds = map[string]bool{
	"minimum": true, "exclusiveMinimum": true, "minLength": true, "minItems": true, "minProperties": true, "minContains": true,
}
var upperBounds = map[string]bool{
	"maximum": true, "exclusiveMaximum": true, "maxLength": true, "maxItems": true, "maxProperties": true, "maxContains": true,
}

// subschemaKeywords hold one subschema; an absent one accepts everything.
var subschemaKeywords = map[string]bool{
	"items": true, "additionalProperties": true, "unevaluatedProperties": true, "unevaluatedItems": true,
	"additionalItems": true, "propertyNames": true, "then": true, "else": true, "contentSchema": true,
}

// diffSchema compares two JSON Schema values (2020-12). The rule: a document valid against the old schema must stay
// valid against the new one, and a guarantee the old schema gave (a required member, a field) must not disappear.
func (d differ) diffSchema(ptr string, o, n any) {
	if reflect.DeepEqual(o, n) {
		return
	}
	if b, ok := o.(bool); ok {
		if !b {
			d.add(Additive, ptr, "schema_loosened", "a schema that accepted nothing now accepts values")
			return
		}
		o = object{}
	}
	if b, ok := n.(bool); ok {
		if !b {
			d.add(Breaking, ptr, "schema_tightened", "the schema now accepts nothing")
			return
		}
		n = object{}
	}
	om, ok1 := o.(object)
	nm, ok2 := n.(object)
	if !ok1 || !ok2 {
		d.add(Breaking, ptr, "schema_kind_changed", "a schema changed from %T to %T", o, n)
		return
	}
	d.diffObject(ptr, om, nm)
}

func unionKeys(a, b object) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range []object{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func (d differ) diffObject(ptr string, o, n object) {
	newlyRequired := setDiff(stringSet(n["required"]), stringSet(o["required"]))
	for _, k := range unionKeys(o, n) {
		ov, oHas := o[k]
		nv, nHas := n[k]
		if oHas && nHas && reflect.DeepEqual(ov, nv) {
			continue
		}
		kp := ptr + "/" + esc(k)
		switch {
		case docKeywords[k]:
			d.add(DocsOnly, kp, "docs_changed", "annotation %q changed", k)
		case strings.HasPrefix(k, "x-alixai-"):
			d.add(Breaking, kp, "authority_changed", "%q changed (%s -> %s)", k, show(ov, oHas), show(nv, nHas))
		case strings.HasPrefix(k, "x-"):
			d.add(DocsOnly, kp, "annotation_changed", "extension %q changed", k)
		case k == "$schema" || k == "$id" || k == "$anchor" || k == "$dynamicRef" || k == "$dynamicAnchor" || k == "$vocabulary":
			d.add(Breaking, kp, "identity_changed", "%q changed (%s -> %s)", k, show(ov, oHas), show(nv, nHas))
		case k == "$ref":
			d.add(Breaking, kp, "ref_changed", "$ref changed (%s -> %s)", show(ov, oHas), show(nv, nHas))
		case k == "type":
			d.diffType(kp, ov, oHas, nv, nHas)
		case k == "enum":
			d.diffEnum(kp, ov, oHas, nv, nHas)
		case k == "const":
			switch {
			case !oHas:
				d.add(Breaking, kp, "const_added", "const %s now required", canon(nv))
			case !nHas:
				d.add(Additive, kp, "const_removed", "const %s no longer required", canon(ov))
			default:
				d.add(Breaking, kp, "const_changed", "const changed (%s -> %s)", canon(ov), canon(nv))
			}
		case lowerBounds[k]:
			d.diffBound(kp, k, ov, oHas, nv, nHas, true)
		case upperBounds[k]:
			d.diffBound(kp, k, ov, oHas, nv, nHas, false)
		case k == "multipleOf" || k == "pattern" || k == "format":
			switch {
			case !nHas:
				d.add(Additive, kp, "constraint_removed", "%q constraint removed", k)
			case !oHas:
				d.add(Breaking, kp, "constraint_added", "%q constraint added (%s)", k, canon(nv))
			default:
				d.add(Breaking, kp, "constraint_changed", "%q changed (%s -> %s)", k, canon(ov), canon(nv))
			}
		case k == "uniqueItems":
			if truthy(nv) && !truthy(ov) {
				d.add(Breaking, kp, "constraint_added", "uniqueItems now required")
			} else {
				d.add(Additive, kp, "constraint_removed", "uniqueItems no longer required")
			}
		case k == "default" || k == "contentEncoding" || k == "contentMediaType":
			d.add(Breaking, kp, "keyword_changed", "%q changed (%s -> %s)", k, show(ov, oHas), show(nv, nHas))
		case k == "required":
			d.diffRequired(kp, o, n)
		case k == "dependentRequired":
			d.diffDependentRequired(kp, ov, nv)
		case k == "properties":
			d.diffProperties(kp, asObject(ov), asObject(nv), newlyRequired)
		case k == "patternProperties":
			d.diffPatternProperties(kp, asObject(ov), asObject(nv), o)
		case k == "$defs" || k == "definitions":
			d.diffDefs(kp, asObject(ov), asObject(nv))
		case k == "dependentSchemas":
			d.diffDependentSchemas(kp, asObject(ov), asObject(nv))
		case subschemaKeywords[k]:
			d.diffSchema(kp, schemaOrTrue(ov, oHas), schemaOrTrue(nv, nHas))
		case k == "contains":
			switch {
			case !oHas:
				d.add(Breaking, kp, "constraint_added", "contains constraint added")
			case !nHas:
				d.add(Additive, kp, "constraint_removed", "contains constraint removed")
			default:
				d.diffSchema(kp, ov, nv)
			}
		case k == "not" || k == "if":
			d.diffOpaqueSubschema(kp, k, ov, oHas, nv, nHas)
		case k == "allOf" || k == "anyOf" || k == "oneOf":
			d.diffBranches(kp, k, asList(ov), asList(nv))
		case k == "prefixItems":
			d.diffPrefixItems(kp, asList(ov), asList(nv))
		default:
			d.add(Breaking, kp, "keyword_changed", "unknown keyword %q changed (%s -> %s)", k, show(ov, oHas), show(nv, nHas))
		}
	}
}

func show(v any, has bool) string {
	if !has {
		return "absent"
	}
	s := canon(v)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func truthy(v any) bool { b, ok := v.(bool); return ok && b }

func schemaOrTrue(v any, has bool) any {
	if !has {
		return true
	}
	return v
}

func asObject(v any) object {
	m, _ := v.(object)
	if m == nil {
		return object{}
	}
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	for _, e := range asList(v) {
		if s, ok := e.(string); ok {
			out[s] = true
		}
	}
	return out
}

// setDiff is a minus b.
func setDiff(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func typeSet(v any) map[string]bool {
	out := map[string]bool{}
	switch t := v.(type) {
	case string:
		out[t] = true
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func (d differ) diffType(ptr string, ov any, oHas bool, nv any, nHas bool) {
	if !oHas {
		d.add(Breaking, ptr, "type_constraint_added", "type now restricted to %s", canon(nv))
		return
	}
	if !nHas {
		d.add(Additive, ptr, "type_constraint_removed", "type no longer restricted (was %s)", canon(ov))
		return
	}
	ot, nt := typeSet(ov), typeSet(nv)
	for _, t := range sortedKeys(ot) {
		if !nt[t] && !(t == "integer" && nt["number"]) {
			d.add(Breaking, ptr, "type_narrowed", "type %q is no longer accepted", t)
		}
	}
	for _, t := range sortedKeys(nt) {
		if !ot[t] && !(t == "integer" && ot["number"]) {
			d.add(Additive, ptr, "type_widened", "type %q is now also accepted", t)
		}
	}
}

func (d differ) diffEnum(ptr string, ov any, oHas bool, nv any, nHas bool) {
	switch {
	case !oHas:
		d.add(Breaking, ptr, "enum_added", "values are now restricted to an enum")
		return
	case !nHas:
		d.add(Additive, ptr, "enum_removed", "the enum restriction was lifted")
		return
	}
	oset, nset := map[string]bool{}, map[string]bool{}
	for _, e := range asList(ov) {
		oset[canon(e)] = true
	}
	for _, e := range asList(nv) {
		nset[canon(e)] = true
	}
	for _, v := range sortedKeys(setDiff(oset, nset)) {
		d.add(Breaking, ptr, "enum_value_removed", "enum value %s was removed", v)
	}
	for _, v := range sortedKeys(setDiff(nset, oset)) {
		d.add(Additive, ptr, "enum_value_added", "enum value %s was added", v)
	}
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return f, err == nil
	case float64:
		return t, true
	}
	return 0, false
}

func (d differ) diffBound(ptr, key string, ov any, oHas bool, nv any, nHas bool, lower bool) {
	switch {
	case !nHas:
		d.add(Additive, ptr, "bound_removed", "%q bound removed", key)
	case !oHas:
		d.add(Breaking, ptr, "bound_added", "%q bound added (%s)", key, canon(nv))
	default:
		of, ok1 := toFloat(ov)
		nf, ok2 := toFloat(nv)
		if !ok1 || !ok2 {
			d.add(Breaking, ptr, "bound_changed", "%q changed (%s -> %s)", key, canon(ov), canon(nv))
			return
		}
		tighter := nf > of
		if !lower {
			tighter = nf < of
		}
		if tighter {
			d.add(Breaking, ptr, "bound_tightened", "%q tightened (%s -> %s)", key, canon(ov), canon(nv))
		} else {
			d.add(Additive, ptr, "bound_loosened", "%q loosened (%s -> %s)", key, canon(ov), canon(nv))
		}
	}
}

func (d differ) diffRequired(ptr string, o, n object) {
	oreq, nreq := stringSet(o["required"]), stringSet(n["required"])
	oprops := asObject(o["properties"])
	for _, f := range sortedKeys(setDiff(nreq, oreq)) {
		if _, had := oprops[f]; had {
			d.add(Breaking, ptr, "optional_became_required", "field %q is now required", f)
		} else {
			d.add(Breaking, ptr, "new_required_field", "field %q is new and required", f)
		}
	}
	for _, f := range sortedKeys(setDiff(oreq, nreq)) {
		d.add(Breaking, ptr, "required_removed", "field %q is no longer guaranteed (required was dropped)", f)
	}
}

func (d differ) diffDependentRequired(ptr string, ov, nv any) {
	om, nm := asObject(ov), asObject(nv)
	for _, k := range unionKeys(om, nm) {
		kp := ptr + "/" + esc(k)
		_, oHas := om[k]
		_, nHas := nm[k]
		switch {
		case !oHas:
			d.add(Breaking, kp, "constraint_added", "dependentRequired added for %q", k)
		case !nHas:
			d.add(Additive, kp, "constraint_removed", "dependentRequired removed for %q", k)
		default:
			oset, nset := stringSet(om[k]), stringSet(nm[k])
			if len(setDiff(nset, oset)) > 0 {
				d.add(Breaking, kp, "constraint_added", "dependentRequired for %q grew", k)
			}
			if len(setDiff(oset, nset)) > 0 {
				d.add(Additive, kp, "constraint_removed", "dependentRequired for %q shrank", k)
			}
		}
	}
}

func (d differ) diffProperties(ptr string, o, n object, newlyRequired map[string]bool) {
	for _, k := range unionKeys(o, n) {
		kp := ptr + "/" + esc(k)
		ov, oHas := o[k]
		nv, nHas := n[k]
		switch {
		case oHas && !nHas:
			d.add(Breaking, kp, "removed_field", "field %q was removed", k)
		case !oHas && nHas:
			if !newlyRequired[k] { // a new required field is reported once, as new_required_field
				d.add(Additive, kp, "new_optional_field", "optional field %q was added", k)
			}
		default:
			d.diffSchema(kp, ov, nv)
		}
	}
}

func (d differ) diffPatternProperties(ptr string, o, n, oldSchema object) {
	closed := false
	if b, ok := oldSchema["additionalProperties"].(bool); ok && !b {
		closed = true
	}
	for _, k := range unionKeys(o, n) {
		kp := ptr + "/" + esc(k)
		ov, oHas := o[k]
		nv, nHas := n[k]
		switch {
		case oHas && !nHas:
			d.add(Breaking, kp, "removed_field", "property pattern %q was removed", k)
		case !oHas && nHas:
			if closed {
				d.add(Additive, kp, "new_optional_field", "property pattern %q was added", k)
			} else {
				d.add(Breaking, kp, "constraint_added", "property pattern %q now constrains members that were free", k)
			}
		default:
			d.diffSchema(kp, ov, nv)
		}
	}
}

func (d differ) diffDefs(ptr string, o, n object) {
	for _, k := range unionKeys(o, n) {
		kp := ptr + "/" + esc(k)
		ov, oHas := o[k]
		nv, nHas := n[k]
		switch {
		case oHas && !nHas:
			d.add(Breaking, kp, "removed_definition", "definition %q was removed", k)
		case !oHas && nHas:
			d.add(Additive, kp, "new_definition", "definition %q was added", k)
		default:
			d.diffSchema(kp, ov, nv)
		}
	}
}

func (d differ) diffDependentSchemas(ptr string, o, n object) {
	for _, k := range unionKeys(o, n) {
		kp := ptr + "/" + esc(k)
		ov, oHas := o[k]
		nv, nHas := n[k]
		switch {
		case oHas && !nHas:
			d.add(Additive, kp, "constraint_removed", "dependent schema for %q removed", k)
		case !oHas && nHas:
			d.add(Breaking, kp, "constraint_added", "dependent schema for %q added", k)
		default:
			d.diffSchema(kp, ov, nv)
		}
	}
}

// diffOpaqueSubschema handles not / if: their effect is not monotone, so any change that is not documentation is breaking.
func (d differ) diffOpaqueSubschema(ptr, key string, ov any, oHas bool, nv any, nHas bool) {
	switch {
	case !oHas:
		d.add(Breaking, ptr, "constraint_added", "%q condition added", key)
	case !nHas:
		d.add(Breaking, ptr, "constraint_changed", "%q condition removed", key)
	default:
		sd, list := d.scratch()
		sd.diffSchema(ptr, ov, nv)
		var first *Change
		for i, c := range *list {
			if c.Severity == DocsOnly {
				*d.out = append(*d.out, c)
			} else if first == nil {
				first = &(*list)[i]
			}
		}
		if first != nil {
			d.add(Breaking, ptr, "constraint_changed", "%q condition changed (%s)", key, first.Message)
		}
	}
}

func (d differ) diffPrefixItems(ptr string, o, n []any) {
	for i := 0; i < len(o) || i < len(n); i++ {
		ip := fmt.Sprintf("%s/%d", ptr, i)
		switch {
		case i >= len(n):
			d.add(Additive, ip, "constraint_removed", "positional item %d constraint removed", i)
		case i >= len(o):
			d.add(Breaking, ip, "constraint_added", "positional item %d constraint added", i)
		default:
			d.diffSchema(ip, o[i], n[i])
		}
	}
}

// branchKey identifies a oneOf/anyOf/allOf branch across versions without relying on its index.
func branchKey(v any) string {
	m, ok := v.(object)
	if !ok {
		return ""
	}
	if r, ok := m["$ref"].(string); ok {
		return "ref:" + r
	}
	props := asObject(m["properties"])
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if c, ok := asObject(props[k])["const"]; ok {
			return "const:" + k + "=" + canon(c)
		}
	}
	if t, ok := m["title"].(string); ok {
		return "title:" + t
	}
	return ""
}

func (d differ) diffBranches(ptr, key string, o, n []any) {
	// A new anyOf/oneOf is a constraint where there was none: values matching no branch were valid and are not.
	if key != "allOf" && len(o) == 0 && len(n) > 0 {
		d.add(Breaking, ptr, "constraint_added", "%s constraint added (%d branches)", key, len(n))
		return
	}
	usedO, usedN := make([]bool, len(o)), make([]bool, len(n))
	type pair struct{ i, j int }
	var pairs []pair
	// 1. identical branches, 2. branches with the same identity, 3. the rest in order.
	for i := range o {
		for j := range n {
			if !usedO[i] && !usedN[j] && canon(o[i]) == canon(n[j]) {
				usedO[i], usedN[j] = true, true
				break
			}
		}
	}
	for i := range o {
		if usedO[i] || branchKey(o[i]) == "" {
			continue
		}
		cnt := 0
		for k := range o {
			if !usedO[k] && branchKey(o[k]) == branchKey(o[i]) {
				cnt++
			}
		}
		match, ncnt := -1, 0
		for j := range n {
			if !usedN[j] && branchKey(n[j]) == branchKey(o[i]) {
				match = j
				ncnt++
			}
		}
		if cnt == 1 && ncnt == 1 {
			usedO[i], usedN[match] = true, true
			pairs = append(pairs, pair{i, match})
		}
	}
	var restO, restN []int
	for i := range o {
		if !usedO[i] {
			restO = append(restO, i)
		}
	}
	for j := range n {
		if !usedN[j] {
			restN = append(restN, j)
		}
	}
	m := len(restO)
	if len(restN) < m {
		m = len(restN)
	}
	for k := 0; k < m; k++ {
		pairs = append(pairs, pair{restO[k], restN[k]})
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].j < pairs[b].j })
	for _, p := range pairs {
		d.diffSchema(fmt.Sprintf("%s/%d", ptr, p.j), o[p.i], n[p.j])
	}
	for _, i := range restO[m:] {
		ip := fmt.Sprintf("%s/%d", ptr, i)
		if key == "allOf" {
			d.add(Additive, ip, "branch_removed", "allOf member removed (constraint relaxed)")
		} else {
			d.add(Breaking, ip, "branch_removed", "%s branch %s was removed", key, branchLabel(o[i]))
		}
	}
	for _, j := range restN[m:] {
		jp := fmt.Sprintf("%s/%d", ptr, j)
		if key == "allOf" {
			d.add(Breaking, jp, "branch_added", "allOf member added (constraint tightened)")
		} else {
			d.add(Additive, jp, "branch_added", "%s branch %s was added", key, branchLabel(n[j]))
		}
	}
}

func branchLabel(v any) string {
	if k := branchKey(v); k != "" {
		return k
	}
	s := canon(v)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// diffSchemaDocs classifies the change between two schema documents of one subject.
func diffSchemaDocs(subject string, o, n any) []Change {
	var out []Change
	differ{subject: subject, out: &out}.diffSchema("", o, n)
	return out
}
