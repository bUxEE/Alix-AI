package adapters

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Schema IDs of the contract objects this package validates.
const (
	// SchemaDeterministicResult is the $id of schemas/actions/deterministic-result.
	SchemaDeterministicResult = "urn:alixai:schema:actions:deterministic-result:v1"
	// schemaFact is the fact definition inside schemas/actions/shared, used to
	// validate inspection facts.
	schemaFact = "urn:alixai:schema:actions:shared:v1#/$defs/fact"
)

// schemaFS holds byte-identical copies of the published schemas that
// DeterministicResult references (go:embed cannot reach outside the package).
// TestEmbeddedSchemasMatchPublished fails when a copy and its original differ,
// so a schema change that is not copied here cannot go unnoticed.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

var (
	schemaOnce   sync.Once
	resultSchema *jsonschema.Schema
	factSchema   *jsonschema.Schema
	schemaErr    error
)

// compiledSchemas compiles the embedded schemas once, as JSON Schema 2020-12
// with format assertions on (so a malformed date is rejected, not ignored).
func compiledSchemas() (result, fact *jsonschema.Schema, err error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		entries, err := fs.ReadDir(schemaFS, "schemas")
		if err != nil {
			schemaErr = fmt.Errorf("adapters: embedded schemas: %w", err)
			return
		}
		for _, e := range entries {
			raw, err := schemaFS.ReadFile("schemas/" + e.Name())
			if err != nil {
				schemaErr = fmt.Errorf("adapters: embedded schema %s: %w", e.Name(), err)
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				schemaErr = fmt.Errorf("adapters: embedded schema %s is not valid JSON: %w", e.Name(), err)
				return
			}
			id, _ := doc.(map[string]any)["$id"].(string)
			if id == "" {
				schemaErr = fmt.Errorf("adapters: embedded schema %s has no $id", e.Name())
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				schemaErr = fmt.Errorf("adapters: embedded schema %s: %w", e.Name(), err)
				return
			}
		}
		if resultSchema, schemaErr = c.Compile(SchemaDeterministicResult); schemaErr != nil {
			schemaErr = fmt.Errorf("adapters: deterministic-result schema does not compile: %w", schemaErr)
			return
		}
		if factSchema, schemaErr = c.Compile(schemaFact); schemaErr != nil {
			schemaErr = fmt.Errorf("adapters: fact schema does not compile: %w", schemaErr)
		}
	})
	return resultSchema, factSchema, schemaErr
}

// EmbeddedSchema returns a copy of one embedded schema file by its file name
// (for example "deterministic-result.schema.json"), for the drift test.
func EmbeddedSchema(name string) ([]byte, error) {
	return schemaFS.ReadFile("schemas/" + name)
}

// EmbeddedSchemaNames lists the embedded schema files.
func EmbeddedSchemaNames() []string {
	entries, _ := fs.ReadDir(schemaFS, "schemas")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// maxViolations bounds how many schema violations an error message names.
const maxViolations = 3

// violations summarises a schema validation failure as locations and keywords
// only. The validator's own messages quote the offending values, and a value
// came from an untrusted reply, so they never reach an error.
func violations(err error) string {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return "does not satisfy the schema"
	}
	var leaves []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(leaves) >= maxViolations {
			return
		}
		if len(e.Causes) == 0 {
			kw := "schema"
			if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
				kw = kp[len(kp)-1]
			}
			loc := "/"
			for _, p := range e.InstanceLocation {
				loc += "/" + sanitizeToken(p)
			}
			if len(e.InstanceLocation) == 0 {
				loc = "(root)"
			} else {
				loc = loc[1:]
			}
			leaves = append(leaves, fmt.Sprintf("%s: %s", loc, kw))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(leaves) == 0 {
		return "does not satisfy the schema"
	}
	out := leaves[0]
	for _, l := range leaves[1:] {
		out += "; " + l
	}
	return out
}

// sanitizeToken keeps a JSON pointer token printable and short. Tokens are
// member names of the reply, which the host controls.
func sanitizeToken(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < 40; i++ {
		c := s[i]
		if c < ' ' || c > '~' {
			c = '?'
		}
		b = append(b, c)
	}
	return string(b)
}

// toInstance converts a value to the generic form the validator reads, by way
// of its JSON encoding.
func toInstance(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(b))
}
