package openai

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// compactTaskProposal has the shape internal/dialogue.compactSchema builds for
// two shortlisted capabilities: closed objects, optional properties, an enum
// with null and no type, bounds that strict mode does not take.
const compactTaskProposal = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["capability_id", "fields", "entity_refs", "intent", "uncertainty"],
  "properties": {
    "capability_id": {"enum": ["booking.create", "booking.cancel", null]},
    "fields": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "date": {"type": "string"},
        "party_size": {"type": "integer"},
        "tags": {"type": "array", "items": {"type": ["string", "number", "boolean"]}, "maxItems": 64}
      }
    },
    "entity_refs": {
      "type": "array",
      "maxItems": 4,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["entity_type", "mention"],
        "properties": {
          "entity_type": {"type": "string"},
          "mention": {"type": "string", "maxLength": 512},
          "field": {"type": "string"}
        }
      }
    },
    "intent": {"enum": ["task", "help", "out_of_scope"]},
    "uncertainty": {
      "type": "object",
      "additionalProperties": false,
      "required": ["overall"],
      "properties": {"overall": {"type": "number", "minimum": 0, "maximum": 1}}
    }
  }
}`

func compileSchema(t testing.TB, schema string) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://schema.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("mem://schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validates(t testing.TB, s *jsonschema.Schema, doc []byte) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(doc)))
	if err != nil {
		t.Fatal(err)
	}
	return s.Validate(inst)
}

func TestTaskProposalCompactSchemaPassesStrictLimits(t *testing.T) {
	converted, plan, err := ConvertStrict(json.RawMessage(compactTaskProposal))
	if err != nil {
		t.Fatal(err)
	}
	if v := CheckStrict(converted); len(v) > 0 {
		t.Fatalf("converted schema is not strict: %v", v)
	}
	// Every removed keyword is reported, none is silent.
	for _, kw := range []string{"maxItems", "maxLength", "minimum", "maximum"} {
		found := false
		for _, s := range plan.Stripped {
			found = found || s == kw
		}
		if !found {
			t.Errorf("stripped keywords %v do not report %s", plan.Stripped, kw)
		}
	}
	if plan.Identity() {
		t.Fatal("optional properties were made required, so the plan must not be the identity")
	}
	// A strict-shaped answer comes back in the original shape and validates
	// against the ORIGINAL schema.
	answer := `{"capability_id":"booking.create","fields":{"date":"2026-10-12","party_size":4,"tags":null},"entity_refs":[{"entity_type":"table","mention":"the corner one","field":null}],"intent":"task","uncertainty":{"overall":0.9}}`
	restored, err := plan.Restore(json.RawMessage(answer))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restored), "null") {
		t.Fatalf("a null of an optional property survived: %s", restored)
	}
	if err := validates(t, compileSchema(t, compactTaskProposal), restored); err != nil {
		t.Fatalf("restored answer fails the original schema: %v\n%s", err, restored)
	}
	// The unrestored strict answer fails it, which is why Restore exists.
	if err := validates(t, compileSchema(t, compactTaskProposal), []byte(answer)); err == nil {
		t.Fatal("the strict-shaped answer unexpectedly validates against the original")
	}
	// A null the original schema accepts (capability_id) is data and stays.
	nullCap := `{"capability_id":null,"fields":{"date":null,"party_size":null,"tags":null},"entity_refs":[],"intent":"help","uncertainty":{"overall":0.2}}`
	restored, err = plan.Restore(json.RawMessage(nullCap))
	if err != nil || !strings.Contains(string(restored), `"capability_id":null`) {
		t.Fatalf("null capability_id: %v %s", err, restored)
	}
	if err := validates(t, compileSchema(t, compactTaskProposal), restored); err != nil {
		t.Fatal(err)
	}
}

// The contract schema (schemas/conversation/task-proposal.schema.json) is not
// what is sent: dialogue sends the compact schema. It is recorded here that the
// contract schema as written cannot be sent as a strict output (external
// $refs, a union that needs a shape conversion), and that the adapter refuses
// it rather than send it with strict switched off.
func TestTaskProposalContractSchemaIsRefusedNotDowngraded(t *testing.T) {
	raw, err := os.ReadFile("../../../schemas/conversation/task-proposal.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ConvertStrict(raw)
	ce := wantCode(t, err, contracts.ErrValidationFailed)
	if ce.Details["unsupported"] != "strict_schema" {
		t.Fatalf("details %v", ce.Details)
	}
}

func TestStrictRestoreInverseShapes(t *testing.T) {
	const orig = `{
	  "type": "object",
	  "required": ["fields", "args", "label"],
	  "properties": {
	    "fields": {"type": "object", "additionalProperties": {"type": ["string", "number", "boolean", "null"]}},
	    "args": {"type": "object"},
	    "label": {"$ref": "#/$defs/label"},
	    "extra": {"type": "string"}
	  },
	  "$defs": {"label": {"type": "string", "pattern": "^[a-z]+$", "minLength": 1}}
	}`
	converted, plan, err := ConvertStrict(json.RawMessage(orig))
	if err != nil {
		t.Fatal(err)
	}
	if v := CheckStrict(converted); len(v) > 0 {
		t.Fatalf("not strict: %v", v)
	}
	if strings.Contains(string(converted), "pattern") || strings.Contains(string(converted), "$ref") {
		t.Fatalf("pattern or $ref survived: %s", converted)
	}
	answer := `{"fields":[{"key":"a","value":"x"},{"key":"b","value":2}],"args":"{\"k\":[1,2]}","label":"abc","extra":null}`
	restored, err := plan.Restore(json.RawMessage(answer))
	if err != nil {
		t.Fatal(err)
	}
	if err := validates(t, compileSchema(t, orig), restored); err != nil {
		t.Fatalf("%v: %s", err, restored)
	}
	var m map[string]any
	_ = json.Unmarshal(restored, &m)
	if _, has := m["extra"]; has || m["fields"].(map[string]any)["b"] != float64(2) {
		t.Fatalf("restored %s", restored)
	}
	for _, bad := range []string{
		`{"fields":[{"key":"a","value":"x"},{"key":"a","value":"y"}],"args":"{}","label":"q","extra":null}`, // repeated key
		`{"fields":[],"args":"[1]","label":"q","extra":null}`,                                               // not an object
		`{"fields":[],"args":"{}","label":"q","extra":null,"surprise":1}`,                                   // undeclared property
		`{"fields":{"a":1},"args":"{}","label":"q","extra":null}`,                                           // wrong shape
	} {
		if _, err := plan.Restore(json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestConvertStrictRefusals(t *testing.T) {
	for name, schema := range map[string]string{
		"root array":     `{"type":"array","items":{"type":"string"}}`,
		"external ref":   `{"type":"object","properties":{"a":{"$ref":"urn:x:y#/$defs/z"}}}`,
		"recursive":      `{"type":"object","properties":{"a":{"$ref":"#/$defs/n"}},"$defs":{"n":{"type":"object","properties":{"a":{"$ref":"#/$defs/n"}}}}}`,
		"no type":        `{"type":"object","properties":{"a":{}}}`,
		"boolean schema": `{"type":"object","properties":{"a":true}}`,
		"ambiguous union": `{"type":"object","properties":{"a":{"anyOf":[
			{"type":"string"},{"type":"object","additionalProperties":{"type":"string"}}]}}}`,
		"too deep": `{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"object","properties":{"a":{"type":"string"}}}}}}}}}}}}}}}}}}}}}`,
		"not json": `[`,
	} {
		if _, _, err := ConvertStrict(json.RawMessage(schema)); err == nil {
			t.Errorf("%s: converted", name)
		} else if ce := wantCode(t, err, contracts.ErrValidationFailed); ce.Details["unsupported"] != "strict_schema" {
			t.Errorf("%s: details %v", name, ce.Details)
		}
	}
}

func TestCheckStrictFindsViolations(t *testing.T) {
	v := CheckStrict(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string","pattern":"x"},"b":{"type":"object","properties":{"c":{"type":"string"}},"required":["c"]}},"required":["a"],"allOf":[]}`))
	joined := strings.Join(v, "\n")
	for _, want := range []string{"additionalProperties is not false", "not listed in required", "keyword pattern", "keyword allOf"} {
		mustContain(t, "violations", joined, want)
	}
}
