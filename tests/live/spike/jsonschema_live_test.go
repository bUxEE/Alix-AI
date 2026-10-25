//go:build live

package spike

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TestSpikeJSONSchemaConstraint finds out which shapes of response_format json_schema LocalAI v4.10.0 turns into a grammar
// that constrains decoding. The runtime converts the schema to a GBNF grammar itself (core/http/endpoints/openai/chat.go)
// and, when the conversion fails, logs "Failed generating grammar" and runs the request UNCONSTRAINED. A request that was
// not constrained is visible from outside only as output that does not follow the schema, so each variant is sent several
// times with a prompt that, unconstrained, a small model answers in prose, and the outputs are checked.
//
// It writes docs/evidence/m0-spike/json-schema-probe.json. The test never fails on a variant that is not enforced: that is
// the finding. It fails only if the runtime cannot be reached or a variant that the report relies on gives no output at all.
func TestSpikeJSONSchemaConstraint(t *testing.T) {
	env := needEnv(t)
	_, localaiName := controllerUnderTest(t)

	// The compact schema exactly as internal/gateway/propose.go builds it for a two-capability catalogue.
	// A variant that drops a property must drop it from "required" too, or the schema is unsatisfiable and "output does not
	// satisfy the schema" would say nothing about the runtime (the first version of this probe had that flaw in two variants).
	compact := func(mutate func(props map[string]any), dropRequired ...string) json.RawMessage {
		props := map[string]any{
			"capability_id": map[string]any{"enum": []any{"bookings.create", "menu.search", nil}},
			"fields": map[string]any{"type": "object", "additionalProperties": false,
				"properties": map[string]any{"party_size": map[string]any{"type": "integer"}, "query": map[string]any{"type": "string"}}},
			"entity_refs": map[string]any{"type": "array", "maxItems": 0},
			"intent":      map[string]any{"enum": []any{"task", "cancel", "help", "out_of_scope"}},
			"uncertainty": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"overall"},
				"properties": map[string]any{"overall": map[string]any{"type": "number", "minimum": 0, "maximum": 1}}},
		}
		if mutate != nil {
			mutate(props)
		}
		required := []any{}
		for _, name := range []string{"capability_id", "fields", "entity_refs", "intent", "uncertainty"} {
			if !slices.Contains(dropRequired, name) {
				required = append(required, name)
			}
		}
		b, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props})
		return b
	}
	variants := []struct {
		name, note string
		schema     json.RawMessage
	}{
		{"as-built-by-the-gateway", "the M0-12 compact schema unchanged", compact(nil)},
		// The next three differ from the as-built schema in the entity_refs property only; the last two drop it (and its "required").
		{"entity_refs-items-string", "entity_refs is {type: array, items: {type: string}}: no maxItems", compact(func(p map[string]any) {
			p["entity_refs"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		})},
		{"entity_refs-items-string-maxItems-0", "entity_refs is {type: array, items: {type: string}, maxItems: 0}", compact(func(p map[string]any) {
			p["entity_refs"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 0}
		})},
		{"entity_refs-plain-array", "entity_refs is {type: array}: no items, no maxItems", compact(func(p map[string]any) {
			p["entity_refs"] = map[string]any{"type": "array"}
		})},
		{"entity_refs-removed", "entity_refs is in neither properties nor required", compact(func(p map[string]any) { delete(p, "entity_refs") }, "entity_refs")},
		{"no-enums-no-entity_refs", "capability_id and intent are plain strings; entity_refs removed", compact(func(p map[string]any) {
			p["capability_id"] = map[string]any{"type": "string"}
			p["intent"] = map[string]any{"type": "string"}
			delete(p, "entity_refs")
		}, "entity_refs")},
	}

	prompt := "Reply with JSON only. The user says: book a table for four people. Capabilities: bookings.create (party_size:integer), menu.search (query:string). " +
		"Fields: capability_id, fields, entity_refs, intent, uncertainty.overall."
	const n = 5

	type row struct {
		Variant     string   `json:"variant"`
		Note        string   `json:"note"`
		Requests    int      `json:"requests"`
		ValidJSON   int      `json:"output_is_json"`
		SchemaValid int      `json:"output_satisfies_the_variants_own_schema"`
		Constrained string   `json:"verdict"`
		Samples     []string `json:"first_outputs"`
	}
	var rows []row
	for _, v := range variants {
		compiled := func() *jsonschema.Schema {
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(v.schema))
			if err != nil {
				t.Fatal(err)
			}
			c := jsonschema.NewCompiler()
			c.DefaultDraft(jsonschema.Draft2020)
			if err := c.AddResource("urn:alixai:probe:v1", doc); err != nil {
				t.Fatal(err)
			}
			s, err := c.Compile("urn:alixai:probe:v1")
			if err != nil {
				t.Fatal(err)
			}
			return s
		}()
		r := row{Variant: v.name, Note: v.note, Requests: n}
		for i := 0; i < n; i++ {
			body, _ := json.Marshal(map[string]any{
				"model": localaiName, "temperature": 0, "max_tokens": 200,
				"messages":        []any{map[string]any{"role": "user", "content": fmt.Sprintf("%s (variation %d)", prompt, i)}},
				"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "probe", "strict": true, "schema": json.RawMessage(v.schema)}},
			})
			code, resp, err := httpDo(http.MethodPost, env.runtimeURL+"/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + env.key})
			if err != nil || code != http.StatusOK {
				t.Fatalf("variant %s: chat = %d, %v: %s", v.name, code, err, truncate(string(resp), 300))
			}
			var out struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			_ = json.Unmarshal(resp, &out)
			text := ""
			if len(out.Choices) > 0 {
				text = out.Choices[0].Message.Content
			}
			if len(r.Samples) < 2 {
				r.Samples = append(r.Samples, truncate(text, 240))
			}
			if doc, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(text))); err == nil {
				r.ValidJSON++
				if compiled.Validate(doc) == nil {
					r.SchemaValid++
				}
			}
		}
		switch {
		case r.SchemaValid == n:
			r.Constrained = "enforced or followed on every request"
		case r.ValidJSON == 0:
			r.Constrained = "not enforced (no request produced JSON)"
		default:
			r.Constrained = "not enforced (output only sometimes valid)"
		}
		rows = append(rows, r)
		t.Logf("%s: %d/%d JSON, %d/%d satisfy the schema", v.name, r.ValidJSON, n, r.SchemaValid, n)
	}
	path := writeEvidence(t, env, "json-schema-probe.json", map[string]any{
		"task":        "M0-13",
		"model":       localaiName,
		"what":        "which json_schema shapes LocalAI v4.10.0 turns into an enforcing grammar",
		"method":      "five greedy requests per variant; output that is not JSON or does not satisfy the schema means the constraint was not applied",
		"variants":    rows,
		"measured_at": time.Now().UTC().Format(time.RFC3339),
	})
	t.Logf("wrote %s", path)

}
