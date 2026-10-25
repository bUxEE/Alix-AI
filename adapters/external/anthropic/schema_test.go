package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
)

func TestConvertSchemaKeepsShapeAndStripsBounds(t *testing.T) {
	in := `{
	  "type":"object",
	  "required":["name","when"],
	  "properties":{
	    "name":{"type":"string","minLength":1,"maxLength":40,"pattern":"^[A-Z]"},
	    "when":{"type":"string","format":"date-time"},
	    "age":{"type":"integer","minimum":0,"maximum":150},
	    "kind":{"oneOf":[{"const":"a"},{"const":"b"}]},
	    "tags":{"type":"array","items":{"type":"string"},"maxItems":3,"minItems":2},
	    "ref":{"$ref":"#/$defs/addr"}
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
	for _, gone := range []string{"minLength", "maxLength", "pattern", "minimum", "maximum", "maxItems", "oneOf"} {
		if strings.Contains(s, `"`+gone+`"`) {
			t.Errorf("%s survived: %s", gone, s)
		}
	}
	// every object is closed, including the one reached through a reference
	if m["additionalProperties"] != false || m["$defs"].(map[string]any)["addr"].(map[string]any)["additionalProperties"] != false {
		t.Fatalf("objects are not closed: %s", s)
	}
	// optional members stay optional: nothing was made nullable or required
	if fmt.Sprint(m["required"]) != "[name when]" || !strings.Contains(s, `"format":"date-time"`) || strings.Contains(s, "null") {
		t.Fatalf("shape changed: %s", s)
	}
	for _, want := range []string{"maxItems", "maxLength", "maximum", "minLength", "minimum", "oneOf(exclusive)", "pattern"} {
		if !strings.Contains(" "+strings.Join(rep.Stripped, " ")+" ", " "+want+" ") {
			t.Errorf("%s is not reported as stripped: %v", want, rep.Stripped)
		}
	}
	// four optional members at the root (age, kind, tags, ref) and city in the definition
	if rep.OptionalProps != 5 || rep.Unions != 1 {
		t.Fatalf("report %+v", rep)
	}
}

func TestConvertSchemaRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"not json":                         `{`,
		"not an object":                    `[1]`,
		"root not an object":               `{"type":"string"}`,
		"additionalProperties":             `{"type":"object","additionalProperties":true}`,
		"open map":                         `{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":"integer"}}}}`,
		"boolean subschema":                `{"type":"object","properties":{"x":true}}`,
		"external ref":                     `{"type":"object","properties":{"x":{"$ref":"other.json#/a"}}}`,
		"self reference":                   `{"type":"object","properties":{"x":{"$ref":"#"}}}`,
		"mutual recursion":                 `{"type":"object","properties":{"x":{"$ref":"#/$defs/a"}},"$defs":{"a":{"type":"object","properties":{"y":{"$ref":"#/$defs/b"}}},"b":{"type":"object","properties":{"z":{"$ref":"#/$defs/a"}}}}}`,
		"additionalProperties on a string": `{"type":"object","properties":{"x":{"type":"string","additionalProperties":false}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ConvertSchema(json.RawMessage(in))
			if _, ok := providers.IsUnsupported(err); !ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestComplexityLimitsRefuseTheRequest(t *testing.T) {
	var props []string
	for i := 0; i < 25; i++ {
		props = append(props, fmt.Sprintf(`"p%d":{"type":"string"}`, i))
	}
	wide := `{"type":"object","additionalProperties":false,"properties":{` + strings.Join(props, ",") + `}}`
	_, rep, err := ConvertSchema(json.RawMessage(wide))
	if err != nil || rep.OptionalProps != 25 {
		t.Fatalf("%+v %v", rep, err)
	}
	if err := checkComplexity([]SchemaReport{rep}); err == nil {
		t.Fatal("25 optional properties passed")
	}
	if err := checkComplexity([]SchemaReport{{OptionalProps: 12}, {OptionalProps: 12}}); err != nil {
		t.Fatalf("24 optional properties refused: %v", err)
	}
	if err := checkComplexity([]SchemaReport{{Unions: 9}, {Unions: 8}}); err == nil {
		t.Fatal("17 unions passed")
	}
}
