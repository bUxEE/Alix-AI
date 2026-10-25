package providersuite

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaFS holds byte-identical copies of the published schemas a conformance
// report and a provider profile are validated against (go:embed cannot reach
// outside the package): the two schemas and the shared, common, price-table,
// data-classification and billing-notice schemas they refer to. Validation
// needs no file system and no network, and a reference that is not embedded
// fails closed. TestEmbeddedSchemasMatchPublished fails when a copy and its
// original differ.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

// Schema ids of the two documents the suite validates.
const (
	SchemaConformanceReport = "urn:alixai:schema:providers:conformance-report:v1"
	SchemaProviderProfile   = "urn:alixai:schema:providers:provider-profile:v1"
)

type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("providersuite: schema reference %q is not an embedded schema", url)
}

var (
	schemaOnce sync.Once
	schemaComp *jsonschema.Compiler
	schemaErr  error
)

func compiler() (*jsonschema.Compiler, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(refuseLoader{})
		entries, err := fs.ReadDir(schemaFS, "schemas")
		if err != nil {
			schemaErr = err
			return
		}
		for _, e := range entries {
			raw, err := schemaFS.ReadFile("schemas/" + e.Name())
			if err != nil {
				schemaErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				schemaErr = fmt.Errorf("providersuite: embedded schema %s: %w", e.Name(), err)
				return
			}
			obj, _ := doc.(map[string]any)
			id, _ := obj["$id"].(string)
			if id == "" {
				schemaErr = fmt.Errorf("providersuite: embedded schema %s has no $id", e.Name())
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				schemaErr = fmt.Errorf("providersuite: embedded schema %s: %w", e.Name(), err)
				return
			}
		}
		schemaComp = c
	})
	return schemaComp, schemaErr
}

// ValidateAgainst validates a JSON document against an embedded schema. The
// error names locations and keywords only (the validator's own text quotes
// values).
func ValidateAgainst(schemaID string, doc []byte) error {
	c, err := compiler()
	if err != nil {
		return err
	}
	sch, err := c.Compile(schemaID)
	if err != nil {
		return fmt.Errorf("providersuite: schema %s does not compile: %w", schemaID, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("providersuite: the document is not valid JSON")
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("providersuite: the document does not match %s: %s", schemaID, flatten(err))
	}
	return nil
}

func flatten(err error) string {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return "invalid"
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/"
			for _, p := range e.InstanceLocation {
				loc += p + "/"
			}
			out = append(out, loc+" "+strings.Join(e.ErrorKind.KeywordPath(), "/"))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) > 5 {
		out = out[:5]
	}
	s := ""
	for i, o := range out {
		if i > 0 {
			s += ", "
		}
		s += o
	}
	return s
}

// EmbeddedSchema returns a copy of one embedded schema by file name, for the
// drift test.
func EmbeddedSchema(name string) ([]byte, error) { return schemaFS.ReadFile("schemas/" + name) }

// EmbeddedSchemaNames lists the embedded schema files.
func EmbeddedSchemaNames() []string {
	entries, _ := fs.ReadDir(schemaFS, "schemas")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
