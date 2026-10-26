package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// The authority rule (docs/integration/conversation-objects.md, master plan
// 11.2): the model can propose and verbalise structured objects but cannot
// manufacture, authorise or alter their authoritative fields. The schemas
// encode it as x-alixai-authority on every property:
//
//	model   written by the model (and re-validated)
//	alixai  minted or decided by the core
//	host    reported by the host application
//
// These checks run over the schemas under schemas/actions and
// schemas/conversation. They look only at structural keywords (properties,
// $defs, items, additionalProperties, type-bearing oneOf/anyOf): the members of
// if/then/else, not, contains and overlay-only allOf/oneOf are constraints, not
// properties, and are skipped.

var authorityValues = map[string]bool{"model": true, "alixai": true, "host": true}

// authorityAreas are the areas the rule applies to.
var authorityAreas = []string{"actions", "conversation", "rag", "providers", "decision", "readiness"}

type authChecker struct {
	reg      *Registry
	problems []string
}

func (c *authChecker) problem(f *File, ptr, format string, args ...any) {
	loc := f.Rel
	if ptr != "" {
		loc += "#" + ptr
	}
	c.problems = append(c.problems, loc+": "+fmt.Sprintf(format, args...))
}

// checkAuthority returns every violation of the authority rules in the given
// areas, sorted.
func checkAuthority(reg *Registry, areas []string) []string {
	c := &authChecker{reg: reg}
	for _, area := range areas {
		for _, f := range reg.byArea[area] {
			c.walk(f, "", f.Root)
		}
	}
	sort.Strings(c.problems)
	return c.problems
}

// walk visits a schema and everything structurally below it.
func (c *authChecker) walk(f *File, ptr string, n *Node) {
	if !n.isObject() {
		return
	}
	c.checkShape(f, ptr, n)

	if list := n.Get("allOf"); list != nil {
		if bearing, _ := countTypeBearing(list); bearing > 0 {
			c.problem(f, ptr, "allOf with type-bearing members is not allowed here: it would hide properties from the authority check; flatten the schema")
		}
	}
	if defs := n.Get("$defs"); defs.isObject() {
		for _, k := range defs.Keys {
			c.walk(f, joinPointer(ptr, "$defs", k), defs.Vals[k])
		}
	}
	if props := n.Get("properties"); props.isObject() {
		// Nested fields of an alixai-owned object inherit that authority (the core assembles
		// them); a separate marker on every leaf is not required and must not rewrite a frozen shape.
		inheritAlixai := false
		if a := n.Get("x-alixai-authority"); a != nil && a.Kind == nString && a.Str == "alixai" {
			inheritAlixai = true
		}
		for _, k := range props.Keys {
			pn := props.Vals[k]
			pptr := joinPointer(ptr, "properties", k)
			if !pn.isObject() {
				continue
			}
			if inheritAlixai && pn.Get("x-alixai-authority") == nil {
				c.walk(f, pptr, pn)
				continue
			}
			auth, ok := c.annotation(f, pptr, pn)
			if ok && auth != "alixai" {
				c.checkContainer(f, pptr, auth, pn)
			}
			c.walk(f, pptr, pn)
		}
	}
	if items := n.Get("items"); items.isObject() {
		c.walk(f, joinPointer(ptr, "items"), items)
	}
	if ap := n.Get("additionalProperties"); ap.isObject() {
		c.walk(f, joinPointer(ptr, "additionalProperties"), ap)
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		list := n.Get(kw)
		if bearing, total := countTypeBearing(list); list != nil && bearing == total {
			for i, m := range list.Items {
				c.walk(f, joinPointer(ptr, kw, fmt.Sprint(i)), m)
			}
		}
	}
}

// annotation returns the authority of a property and whether it is valid.
func (c *authChecker) annotation(f *File, ptr string, pn *Node) (string, bool) {
	a := pn.Get("x-alixai-authority")
	switch {
	case a == nil:
		c.problem(f, ptr, "property has no x-alixai-authority")
		return "", false
	case a.Kind != nString || !authorityValues[a.Str]:
		c.problem(f, ptr, "x-alixai-authority must be one of model, alixai, host (got %s)", describe(a))
		return "", false
	}
	return a.Str, true
}

func describe(n *Node) string {
	if n.Kind == nString {
		return fmt.Sprintf("%q", n.Str)
	}
	return fmt.Sprintf("a JSON %v value", n.Kind)
}

// member is a property reached from a container.
type member struct {
	file *File
	ptr  string
	node *Node
}

// checkContainer enforces: a model container holds only model members, a host
// container only host members. An alixai container is unrestricted: the core
// assembles it from members that keep the authority they came with.
func (c *authChecker) checkContainer(f *File, ptr, auth string, pn *Node) {
	var members []member
	c.members(f, ptr, pn, map[string]bool{}, &members)
	for _, m := range members {
		got := "none"
		if a := m.node.Get("x-alixai-authority"); a != nil && a.Kind == nString {
			got = a.Str
		}
		if got != auth {
			loc := m.file.Rel + "#" + m.ptr
			c.problem(f, ptr, "%s container holds %s with authority %s: members of a %s container must all be %s", auth, loc, got, auth, auth)
		}
	}
}

// members collects the properties directly inside the value schema of a
// container, following $refs across files and looking through arrays, maps and
// unions. It does not descend into those properties' own values: they are
// containers in their own right and are checked on their own.
func (c *authChecker) members(f *File, ptr string, n *Node, seen map[string]bool, out *[]member) {
	if !n.isObject() {
		return
	}
	if n.Has("$ref") {
		r, err := c.reg.resolve(f, n.Get("$ref").str())
		if err != nil {
			c.problem(f, ptr, "%v", err)
			return
		}
		key := r.file.key() + "#" + r.ptr
		if seen[key] {
			return
		}
		seen[key] = true
		c.members(r.file, r.ptr, r.node, seen, out)
		return
	}
	if props := n.Get("properties"); props.isObject() {
		for _, k := range props.Keys {
			if props.Vals[k].isObject() {
				*out = append(*out, member{file: f, ptr: joinPointer(ptr, "properties", k), node: props.Vals[k]})
			}
		}
	}
	if items := n.Get("items"); items.isObject() {
		c.members(f, joinPointer(ptr, "items"), items, seen, out)
	}
	if ap := n.Get("additionalProperties"); ap.isObject() {
		c.members(f, joinPointer(ptr, "additionalProperties"), ap, seen, out)
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		list := n.Get(kw)
		if bearing, total := countTypeBearing(list); list != nil && bearing == total {
			for i, m := range list.Items {
				c.members(f, joinPointer(ptr, kw, fmt.Sprint(i)), m, seen, out)
			}
		}
	}
}

// checkShape enforces how objects are written, which the generator relies on:
// a struct has properties and is closed; an open map has a schema for its
// values and constrains its keys with propertyNames; the two are never mixed.
// A map is not written as additionalProperties:false.
func (c *authChecker) checkShape(f *File, ptr string, n *Node) {
	ap := n.Get("additionalProperties")
	hasProps := n.Has("properties")
	hasNames := n.Has("propertyNames")
	valueSchema := ap.isObject() || ap.isTrue()
	switch {
	case hasNames && !valueSchema:
		c.problem(f, ptr, "propertyNames needs an additionalProperties schema for the values")
	case hasNames && hasProps:
		c.problem(f, ptr, "an object cannot have both properties and propertyNames: a struct is closed, a map constrains its keys")
	case valueSchema && hasProps:
		c.problem(f, ptr, "an object cannot have both properties and an additionalProperties schema")
	case valueSchema && !hasNames:
		c.problem(f, ptr, "an open map must constrain its keys with propertyNames")
	case ap.isFalse() && !hasProps:
		c.problem(f, ptr, "additionalProperties:false without properties: write an open map with propertyNames and a value schema")
	}
}

// ---------------------------------------------------------------------------

// TestAuthorityAnnotationsExhaustive: every property under schemas/actions and
// schemas/conversation carries x-alixai-authority (model, alixai or host);
// model containers hold only model members and host containers only host
// members; open maps are written with propertyNames and a value schema.
func TestAuthorityAnnotationsExhaustive(t *testing.T) {
	root := repoRoot(t)
	reg, err := loadRegistry(os.DirFS(filepath.Join(root, "schemas")), "schemas")
	if err != nil {
		t.Fatal(err)
	}
	for _, area := range authorityAreas {
		if len(reg.byArea[area]) == 0 {
			t.Fatalf("no schemas found in schemas/%s", area)
		}
	}
	if problems := checkAuthority(reg, authorityAreas); len(problems) > 0 {
		t.Errorf("%d authority problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}

	// The check must actually see the properties: count what it covered so a
	// walker that silently stops descending cannot pass by checking nothing.
	covered := map[string]int{}
	var count func(f *File, n *Node)
	count = func(f *File, n *Node) {
		if !n.isObject() {
			return
		}
		if props := n.Get("properties"); props.isObject() {
			covered[f.Area] += len(props.Keys)
		}
		if d := n.Get("$defs"); d.isObject() {
			for _, k := range d.Keys {
				count(f, d.Vals[k])
			}
		}
		if props := n.Get("properties"); props.isObject() {
			for _, k := range props.Keys {
				count(f, props.Vals[k])
			}
		}
		if items := n.Get("items"); items.isObject() {
			count(f, items)
		}
	}
	for _, area := range authorityAreas {
		for _, f := range reg.byArea[area] {
			count(f, f.Root)
		}
	}
	for _, area := range authorityAreas {
		if covered[area] < 20 {
			t.Errorf("only %d properties found in schemas/%s; the walker is not descending", covered[area], area)
		}
	}
}

// TestAuthorityCheckDetectsViolations proves the check has teeth: each fixture
// breaks exactly one rule and must be reported, and the compliant fixture must
// not be.
func TestAuthorityCheckDetectsViolations(t *testing.T) {
	obj := func(props string) string {
		return `"type":"object","additionalProperties":false,"properties":{` + props + `}`
	}
	prop := func(auth, rest string) string {
		return `{"x-alixai-authority":"` + auth + `",` + rest + `}`
	}
	str := `"type":"string"`

	cases := []struct {
		name  string
		files map[string]string
		want  string // "" = must be clean
	}{
		{"compliant", map[string]string{
			"conversation/ok": obj(`
			  "m":`+prop("model", `"$ref":"#/$defs/modelBox"`)+`,
			  "h":`+prop("host", `"type":"array","items":{"$ref":"#/$defs/hostBox"}`)+`,
			  "a":`+prop("alixai", `"$ref":"#/$defs/mixed"`)+`,
			  "fields":`+prop("model", `"type":"object","propertyNames":{"pattern":"^[a-z]+$"},"additionalProperties":{"type":"string"}`)+`,
			  "kind":`+prop("alixai", `"const":"ok"`)) + `,
			"$defs":{
			  "modelBox":{` + obj(`"x":`+prop("model", str)+`,"y":`+prop("model", `"$ref":"#/$defs/modelBox2"`)) + `},
			  "modelBox2":{` + obj(`"z":`+prop("model", str)) + `},
			  "hostBox":{` + obj(`"x":`+prop("host", str)) + `},
			  "mixed":{` + obj(`"x":`+prop("host", str)+`,"y":`+prop("model", str)+`,"z":`+prop("alixai", str)) + `}
			},
			"allOf":[{"if":{"properties":{"kind":{"const":"ok"}}},"then":{"properties":{"m":{"minProperties":1}}}}]`,
		}, ""},

		{"property without annotation", map[string]string{
			"conversation/bad": obj(`"a":{` + str + `}`),
		}, "property has no x-alixai-authority"},

		{"annotation with a bad value", map[string]string{
			"actions/bad": obj(`"a":` + prop("robot", str)),
		}, `must be one of model, alixai, host (got "robot")`},

		{"annotation that is not a string", map[string]string{
			"actions/bad": obj(`"a":{"x-alixai-authority":true,` + str + `}`),
		}, "must be one of model, alixai, host"},

		{"model member under a host container", map[string]string{
			"actions/bad": obj(`"h":`+prop("host", `"$ref":"#/$defs/box"`)) + `,
			"$defs":{"box":{` + obj(`"x":`+prop("model", str)) + `}}`,
		}, "host container holds"},

		{"host member under a model container, through an array", map[string]string{
			"conversation/bad": obj(`"m":`+prop("model", `"type":"array","items":{"$ref":"#/$defs/box"}`)) + `,
			"$defs":{"box":{` + obj(`"x":`+prop("host", str)) + `}}`,
		}, "model container holds"},

		{"alixai member under a model container, through a map", map[string]string{
			"conversation/bad": obj(`"m":`+prop("model", `"type":"object","propertyNames":{"pattern":"^a"},"additionalProperties":{"$ref":"#/$defs/box"}`)) + `,
			"$defs":{"box":{` + obj(`"x":`+prop("alixai", str)) + `}}`,
		}, "model container holds"},

		{"mixed member in a union under a host container", map[string]string{
			"actions/bad": obj(`"h":`+prop("host", `"oneOf":[{"$ref":"#/$defs/a"},{"$ref":"#/$defs/b"}]`)) + `,
			"$defs":{
			  "a":{` + obj(`"x":`+prop("host", str)) + `},
			  "b":{` + obj(`"x":`+prop("alixai", str)) + `}}`,
		}, "host container holds"},

		{"container reaches into another area's unannotated schema", map[string]string{
			"common/plain": obj(`"x":{` + str + `}`),
			"actions/bad":  obj(`"h":` + prop("host", `"$ref":"urn:alixai:schema:common:plain:v1"`)),
		}, "authority none"},

		{"open map without propertyNames", map[string]string{
			"conversation/bad": obj(`"m":` + prop("model", `"type":"object","additionalProperties":{"type":"string"}`)),
		}, "must constrain its keys with propertyNames"},

		{"closed object that is really a map", map[string]string{
			"conversation/bad": obj(`"m":` + prop("model", `"type":"object","additionalProperties":false`)),
		}, "additionalProperties:false without properties"},

		{"propertyNames without a value schema", map[string]string{
			"conversation/bad": obj(`"m":` + prop("model", `"type":"object","propertyNames":{"pattern":"^a"}`)),
		}, "propertyNames needs an additionalProperties schema"},

		{"properties mixed with a value schema", map[string]string{
			"conversation/bad": obj(`"m":` + prop("model", `"type":"object","properties":{"x":`+prop("model", str)+`},"additionalProperties":{"type":"string"}`)),
		}, "cannot have both properties and an additionalProperties schema"},

		{"type-bearing allOf hides properties", map[string]string{
			"conversation/bad": `"allOf":[{"$ref":"#/$defs/base"},{"type":"object","properties":{"x":{` + str + `}}}],
			"$defs":{"base":{` + obj(`"y":`+prop("model", str)) + `}}`,
		}, "allOf with type-bearing members"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := loadRegistry(mapSchemas(tc.files), "schemas")
			if err != nil {
				t.Fatal(err)
			}
			problems := checkAuthority(reg, authorityAreas)
			if tc.want == "" {
				if len(problems) > 0 {
					t.Fatalf("a compliant schema was reported:\n  %s", strings.Join(problems, "\n  "))
				}
				return
			}
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("no problem mentions %q; got:\n  %s", tc.want, strings.Join(problems, "\n  "))
		})
	}

	// Areas outside the rule are not checked.
	t.Run("other areas are not checked", func(t *testing.T) {
		fsys := fstest.MapFS{"common/plain.schema.json": {Data: []byte(schemaDoc("common", "plain", `"type":"object","properties":{"x":{"type":"string"}}`))}}
		reg, err := loadRegistry(fsys, "schemas")
		if err != nil {
			t.Fatal(err)
		}
		if problems := checkAuthority(reg, authorityAreas); len(problems) != 0 {
			t.Fatalf("common is outside the rule: %v", problems)
		}
	})
}
