package gemini

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestConvertSchemaKeepsShapeAndStripsBounds(t *testing.T) {
	in := `{
	  "type":"object",
	  "required":["name","when"],
	  "properties":{
	    "name":{"type":"string","minLength":1,"maxLength":40,"pattern":"^[A-Z]"},
	    "when":{"type":"string","format":"date-time"},
	    "age":{"type":"integer","minimum":0,"maximum":150,"exclusiveMinimum":-1},
	    "kind":{"oneOf":[{"const":"a"},{"const":"b"}]},
	    "tags":{"type":"array","items":{"type":"string"},"maxItems":3,"minItems":2,"uniqueItems":true},
	    "ref":{"$ref":"#/$defs/addr","description":"where"}
	  },
	  "$defs":{"addr":{"type":"object","properties":{"city":{"type":"string"}}}}
	}`
	out, rep, err := ConvertSchema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, gone := range []string{"minLength", "maxLength", "pattern", "exclusiveMinimum", "uniqueItems", "oneOf", "const", "$ref", "$defs"} {
		if strings.Contains(s, `"`+gone+`"`) {
			t.Errorf("%s survived: %s", gone, s)
		}
	}
	// kept: bounds the vendor lists, enum from const, anyOf from oneOf
	for _, kept := range []string{`"minimum":0`, `"maximum":150`, `"maxItems":3`, `"minItems":2`, `"enum":["a"]`, `"anyOf"`, `"format":"date-time"`} {
		if !strings.Contains(s, kept) {
			t.Errorf("%s missing: %s", kept, s)
		}
	}
	// every object is closed, including the one reached through a reference, which was inlined
	ref := m["properties"].(map[string]any)["ref"].(map[string]any)
	if m["additionalProperties"] != false || ref["additionalProperties"] != false || ref["description"] != "where" || ref["properties"] == nil {
		t.Fatalf("objects are not closed or the reference was not inlined: %s", s)
	}
	// optional members stay optional: nothing was made nullable or required
	if fmt.Sprint(m["required"]) != "[name when]" || strings.Contains(s, "null") {
		t.Fatalf("shape changed: %s", s)
	}
	for _, want := range []string{"exclusiveMinimum", "maxLength", "minLength", "oneOf(exclusive)", "pattern", "uniqueItems"} {
		if !strings.Contains(" "+strings.Join(rep.Stripped, " ")+" ", " "+want+" ") {
			t.Errorf("%s is not reported as stripped: %v", want, rep.Stripped)
		}
	}
	if rep.Inlined != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestConvertSchemaRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"not JSON":              `{`,
		"not an object root":    `{"type":"string"}`,
		"an open map":           `{"type":"object","additionalProperties":{"type":"string"}}`,
		"additional allowed":    `{"type":"object","additionalProperties":true}`,
		"an external reference": `{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`,
		"a dangling reference":  `{"type":"object","properties":{"a":{"$ref":"#/$defs/nope"}}}`,
		"a recursive schema":    `{"type":"object","properties":{"n":{"$ref":"#/$defs/node"}},"$defs":{"node":{"type":"object","properties":{"next":{"$ref":"#/$defs/node"}}}}}`,
		"allOf":                 `{"type":"object","allOf":[{"type":"object"}]}`,
		"a boolean subschema":   `{"type":"object","properties":{"a":true}}`,
		"too deep": func() string {
			s := `{"type":"string"}`
			for i := 0; i < 40; i++ {
				s = `{"type":"object","properties":{"a":` + s + `}}`
			}
			return s
		}(),
		"a reference fan-out": func() string {
			defs := `"d0":{"type":"string"}`
			for i := 1; i < 14; i++ {
				defs += fmt.Sprintf(`,"d%d":{"type":"object","properties":{"a":{"$ref":"#/$defs/d%d"},"b":{"$ref":"#/$defs/d%d"}}}`, i, i-1, i-1)
			}
			return `{"type":"object","properties":{"x":{"$ref":"#/$defs/d13"}},"$defs":{` + defs + `}}`
		}(),
	} {
		if _, _, err := ConvertSchema(json.RawMessage(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
