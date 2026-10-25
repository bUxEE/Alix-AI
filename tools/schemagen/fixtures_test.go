package main

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// gen runs the generator on in-memory schemas and returns the generated files.
func gen(t *testing.T, files map[string]string) map[string]string {
	t.Helper()
	out, err := Generate(fixtureConfig(files))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	m := map[string]string{}
	for p, b := range out {
		m[p] = string(b)
	}
	return m
}

func genErr(t *testing.T, files map[string]string) error {
	t.Helper()
	_, err := Generate(fixtureConfig(files))
	if err == nil {
		t.Fatal("Generate succeeded, want an error")
	}
	return err
}

// squash collapses runs of spaces and tabs, so expectations do not depend on
// gofmt's column alignment.
func squash(s string) string { return spaceRun.ReplaceAllString(s, " ") }

var spaceRun = regexp.MustCompile("[ 	]+")

func mustContain(t *testing.T, what, haystack string, needles ...string) {
	t.Helper()
	haystack = squash(haystack)
	for _, n := range needles {
		n = squash(n)
		if !strings.Contains(haystack, n) {
			t.Errorf("%s lacks %q:\n%s", what, n, haystack)
		}
	}
}

func mustNotContain(t *testing.T, what, haystack string, needles ...string) {
	t.Helper()
	haystack = squash(haystack)
	for _, n := range needles {
		n = squash(n)
		if strings.Contains(haystack, n) {
			t.Errorf("%s contains %q:\n%s", what, n, haystack)
		}
	}
}

// TestFixtureTypeMapping covers the mapping rules on small schemas, one rule
// per case, independent of the repository's real schemas.
func TestFixtureTypeMapping(t *testing.T) {
	out := gen(t, map[string]string{
		"demo/thing": `"type":"object","additionalProperties":false,
		  "required":["name","note","tags","meta","kind_of","level","ratio","flag","fixed"],
		  "properties":{
		    "name":{"type":"string"},
		    "nick":{"type":"string"},
		    "note":{"oneOf":[{"type":"string"},{"type":"null"}]},
		    "tags":{"type":"array","items":{"type":"string"}},
		    "opt_tags":{"type":"array","items":{"type":"string"}},
		    "meta":{"type":"object","propertyNames":{"pattern":"^[a-z]+$"},"additionalProperties":{"type":"integer"}},
		    "kind_of":{"type":"string","enum":["a","b-c"]},
		    "level":{"type":"integer","enum":[0,1,2]},
		    "ratio":{"type":"number"},
		    "flag":{"type":"boolean"},
		    "fixed":{"const":"x"},
		    "scalar":{"type":["string","number","null"]},
		    "nested":{"type":"object","properties":{"deep":{"type":"string"}},"required":["deep"]},
		    "gone":false,
		    "free":{"type":"object"}
		  }`,
	})
	goSrc := out["out/go/demo/thing.gen.go"]
	mustContain(t, "thing.gen.go", goSrc,
		"Name string `json:\"name\"`",
		"Nick *string `json:\"nick,omitzero\"`",
		"Note *string `json:\"note\"`",
		"Tags []string `json:\"tags\"`",
		"OptTags []string `json:\"opt_tags,omitzero\"`",
		"Meta map[string]int64 `json:\"meta\"`",
		"KindOf ThingKindOf `json:\"kind_of\"`",
		"Level ThingLevel `json:\"level\"`",
		"Ratio float64 `json:\"ratio\"`",
		"Flag bool `json:\"flag\"`",
		"Fixed string `json:\"fixed\"`",
		"Scalar any `json:\"scalar,omitzero\"`",
		"Nested *ThingNested `json:\"nested,omitzero\"`",
		"Free map[string]any `json:\"free,omitzero\"`",
		"ThingKindOfA ThingKindOf = \"a\"",
		"ThingKindOfBC ThingKindOf = \"b-c\"",
		"ThingLevel0 ThingLevel = 0",
		// Required members that are collections never encode as null; consts are forced.
		"a.Tags = []string{}",
		"a.Meta = map[string]int64{}",
		`a.Fixed = "x"`,
	)
	mustNotContain(t, "thing.gen.go", goSrc, "Gone", "a.OptTags")

	tsSrc := out["out/ts/demo/thing.ts"]
	mustContain(t, "thing.ts", tsSrc,
		"  name: string;",
		"  nick?: string;",
		"  note: string | null;",
		"  tags: string[];",
		"  meta: Record<string, number>;",
		`  kind_of:`, `  | "a"`, `  | "b-c";`,
		"  level:", "  | 0", "  | 2;",
		`  fixed: "x";`,
		"  scalar?: string | number | null;",
		"  free?: Record<string, unknown>;",
	)
}

func TestFixtureDefinedTypesAndAliases(t *testing.T) {
	out := gen(t, map[string]string{
		"demo/lib": `"$defs":{
		  "slug":{"type":"string","pattern":"^[a-z]+$","maxLength":8},
		  "slugAgain":{"description":"alias of slug","$ref":"#/$defs/slug"},
		  "both":{"allOf":[{"$ref":"#/$defs/slug"},{"type":"string","minLength":2}]},
		  "count":{"type":"integer","minimum":0},
		  "names":{"type":"array","items":{"$ref":"#/$defs/slug"}},
		  "byName":{"type":"object","propertyNames":{"pattern":"^a"},"additionalProperties":{"$ref":"#/$defs/count"}},
		  "anything":{},
		  "loose":{"oneOf":[{"type":"string"},{"type":"number"}]}
		}`,
	})
	goSrc := out["out/go/demo/lib.gen.go"]
	mustContain(t, "lib.gen.go", goSrc,
		"type Slug string",
		"type SlugAgain = Slug",
		"type Both string",
		"type Count int64",
		"type Names []Slug",
		"type ByName map[string]Count",
		"type Anything = any",
		"type Loose = any",
		"pattern `^[a-z]+$`",
		"maxLength 8",
	)
	tsSrc := out["out/ts/demo/lib.ts"]
	mustContain(t, "lib.ts", tsSrc,
		"export type Slug = string;",
		"export type SlugAgain = Slug;",
		"export type Names = Slug[];",
		"export type ByName = Record<string, Count>;",
		"export type Anything = unknown;",
		"export type Loose = string | number;",
	)
}

func TestFixtureInlineVariantNames(t *testing.T) {
	out := gen(t, map[string]string{
		"demo/shape": `"oneOf":[
		  {"type":"object","additionalProperties":false,"required":["kind","r"],"properties":{"kind":{"const":"circle"},"r":{"type":"number"}}},
		  {"$ref":"#/$defs/square"},
		  {"type":"object","additionalProperties":false,"required":["kind"],"properties":{"kind":{"const":"dot"}},
		   "oneOf":[{"required":["kind"]}]}
		],
		"$defs":{"square":{"type":"object","additionalProperties":false,"required":["kind","side"],"properties":{"kind":{"const":"square"},"side":{"type":"number"}}}}`,
	})
	goSrc := out["out/go/demo/shape.gen.go"]
	mustContain(t, "shape.gen.go", goSrc,
		"type Shape struct {",
		"Value ShapeValue",
		"type ShapeValue interface {\n\tisShape()",
		"type ShapeCircle struct {",
		"type ShapeDot struct {",
		"type Square struct {", // a def of a union-root file keeps its own name
		"func (ShapeCircle) isShape() {}",
		"func (Square) isShape() {}",
		`case "dot":`,
		"decodeStrict[ShapeDot](data)",
		`a.Kind = "circle"`,
	)
	tsSrc := out["out/ts/demo/shape.ts"]
	mustContain(t, "shape.ts", tsSrc, "export type Shape =", "  | ShapeCircle", "  | Square", "  | ShapeDot;", `  kind: "circle";`)
	if doc := out["out/go/demo/doc.gen.go"]; !strings.Contains(doc, "func decodeStrict[T any]") {
		t.Errorf("a package with a union needs the strict decoder:\n%s", doc)
	}
}

func TestFixtureOverlaysAreIgnored(t *testing.T) {
	out := gen(t, map[string]string{
		"demo/overlay": `"type":"object","required":["a"],
		  "properties":{"a":{"type":"string"},"b":{"type":"string"},"mode":{"type":"string","enum":["x","y"]}},
		  "oneOf":[{"required":["a"]},{"required":["b"]}],
		  "allOf":[
		    {"if":{"properties":{"mode":{"const":"x"}},"required":["mode"]},"then":{"required":["b"]},"else":{"properties":{"b":false}}},
		    {"properties":{"a":{"maxLength":3}}}
		  ],
		  "dependentRequired":{"b":["a"]}`,
	})
	goSrc := out["out/go/demo/overlay.gen.go"]
	mustContain(t, "overlay.gen.go", goSrc,
		"A string `json:\"a\"`",
		"B *string `json:\"b,omitzero\"`",
		"cross-field rules",
	)
}

func TestFixtureRefsIntoPropertiesAndAcrossAreas(t *testing.T) {
	out := gen(t, map[string]string{
		"base/item": `"type":"object","required":["code"],"properties":{"code":{"type":"string","enum":["ok","bad"]}}`,
		"demo/user": `"type":"object","required":["status","other"],"properties":{
		    "status":{"$ref":"urn:alixai:schema:base:item:v1#/properties/code"},
		    "other":{"$ref":"urn:alixai:schema:base:item:v1"},
		    "maybe":{"anyOf":[{"$ref":"urn:alixai:schema:base:item:v1#/properties/code"},{"type":"null"}]}
		}`,
	})
	goSrc := out["out/go/demo/user.gen.go"]
	mustContain(t, "user.gen.go", goSrc,
		`"example.test/repo/out/go/base"`,
		"Status base.ItemCode `json:\"status\"`",
		"Other base.Item `json:\"other\"`",
		"Maybe *base.ItemCode `json:\"maybe,omitzero\"`",
	)
	mustContain(t, "item.ts import", out["out/ts/demo/user.ts"], `import type { Item, ItemCode } from "../base/item.js";`)
}

func TestFixtureNamesAreStableWhenSchemasAreAdded(t *testing.T) {
	base := map[string]string{
		"demo/a": `"type":"object","properties":{"x":{"$ref":"#/$defs/part"}},"$defs":{"part":{"type":"object","properties":{"v":{"type":"string"}}}}`,
	}
	before := gen(t, base)["out/go/demo/a.gen.go"]
	withOther := map[string]string{
		"demo/a": base["demo/a"],
		"demo/b": `"type":"object","properties":{"y":{"$ref":"#/$defs/part"}},"$defs":{"part":{"type":"object","properties":{"w":{"type":"string"}}}}`,
	}
	out := gen(t, withOther)
	if out["out/go/demo/a.gen.go"] != before {
		t.Error("adding schema b changed the generated code of schema a: names must not depend on other files")
	}
	mustContain(t, "a.gen.go", before, "type APart struct")
	mustContain(t, "b.gen.go", out["out/go/demo/b.gen.go"], "type BPart struct")
}

func TestGenerateRejectsWhatItCannotRepresent(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"mixed type and overlay members", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"required":["z"]}]}}`}, "mixes type-bearing"},
		{"allOf of different types", map[string]string{
			"demo/x": `"$defs":{"d":{"allOf":[{"type":"string"},{"type":"integer"}]}}`}, "same primitive"},
		{"patternProperties", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"type":"object","patternProperties":{"^x":{"type":"string"}}}}`}, "patternProperties"},
		{"properties and additionalProperties together", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"string"}`}, "both properties and an additionalProperties"},
		{"required property that is not declared", map[string]string{
			"demo/x": `"type":"object","required":["ghost"],"properties":{"a":{"type":"string"}}`}, "ghost"},
		{"file reference", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"$ref":"other.json"}}`}, "only same-file pointers"},
		{"missing target", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"$ref":"#/$defs/nope"}}`}, "no member"},
		{"unknown schema", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"$ref":"urn:alixai:schema:demo:missing:v1"}}`}, "no schema with that $id"},
		{"nullable discriminated union", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"oneOf":[
			  {"type":"object","required":["k"],"properties":{"k":{"const":"p"}}},
			  {"type":"object","required":["k"],"properties":{"k":{"const":"q"}}},
			  {"type":"null"}]}}`}, "nullable discriminated union"},
		{"union variant in another area", map[string]string{
			"other/v": `"type":"object","required":["k"],"properties":{"k":{"const":"p"}}`,
			"demo/x":  `"oneOf":[{"$ref":"urn:alixai:schema:other:v:v1"},{"type":"object","required":["k"],"properties":{"k":{"const":"q"}}}]`}, "must share the union's package"},
		{"areas that import each other", map[string]string{
			"one/a": `"type":"object","properties":{"x":{"$ref":"urn:alixai:schema:two:b:v1"}}`,
			"two/b": `"type":"object","properties":{"y":{"$ref":"urn:alixai:schema:one:a:v1"}}`}, "cycle"},
		{"two definitions with one name", map[string]string{
			"demo/x": `"$defs":{"fooBar":{"type":"string"},"foo_bar":{"type":"string"}}`}, "rename one definition"},
		{"enum values with one name", map[string]string{
			"demo/x": `"type":"object","properties":{"a":{"type":"string","enum":["a-b","a_b"]}}`}, "both map to the name"},
		{"fields with one name", map[string]string{
			"demo/x": `"type":"object","properties":{"a_b":{"type":"string"},"aB":{"type":"string"}}`}, "both map to the field name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := genErr(t, tc.files)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadRegistryRules(t *testing.T) {
	// Directories without schemas (such as schemas/api, which holds OpenAPI
	// YAML) and subdirectories of an area (such as examples/) are not errors.
	fsys := fstest.MapFS{
		"demo/x.schema.json":           {Data: []byte(schemaDoc("demo", "x", `"type":"string"`))},
		"demo/examples/x/valid/a.json": {Data: []byte(`"a"`)},
		"demo/notes.md":                {Data: []byte("not a schema")},
		"api/openapi.yaml":             {Data: []byte("openapi: 3.1.0")},
		"api/paths/tasks.yaml":         {Data: []byte("paths: {}")},
		"empty/.gitkeep":               {Data: nil},
	}
	reg, err := loadRegistry(fsys, "schemas")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.areas) != 1 || reg.areas[0] != "demo" || len(reg.byArea["demo"]) != 1 {
		t.Fatalf("areas = %v, demo files = %d", reg.areas, len(reg.byArea["demo"]))
	}

	for name, body := range map[string]string{
		"wrong id":      `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"urn:alixai:schema:demo:other:v1"}`,
		"wrong dialect": `{"$schema":"http://json-schema.org/draft-07/schema#","$id":"urn:alixai:schema:demo:x:v1"}`,
		"duplicate key": `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"urn:alixai:schema:demo:x:v1","type":"string","type":"string"}`,
		"not an object": `[]`,
		"not json":      `{`,
	} {
		t.Run(name, func(t *testing.T) {
			m := fstest.MapFS{"demo/x.schema.json": {Data: []byte(body)}}
			if _, err := loadRegistry(m, "schemas"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
