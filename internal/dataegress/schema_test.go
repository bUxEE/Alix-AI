package dataegress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The published schemas are the contract. Every decision, disclosure, envelope and
// policy the engine produces is validated against them, which checks every
// cross-field rule the generated types cannot express.

const (
	urnDecision   = "urn:alixai:schema:providers:egress-decision:v1"
	urnDisclosure = "urn:alixai:schema:providers:egress-disclosure:v1"
	urnEnvelope   = "urn:alixai:schema:providers:request-envelope:v1"
	urnPolicy     = "urn:alixai:schema:providers:egress-policy:v1"
	urnProfile    = "urn:alixai:schema:providers:provider-profile:v1"
)

type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema reference %q is not one of the loaded schemas", url)
}

var (
	schemaOnce sync.Once
	schemaComp *jsonschema.Compiler
	schemaErr  error
	schemaMu   sync.Mutex
)

func schemaCompiler() (*jsonschema.Compiler, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(refuseLoader{})
		for _, dir := range []string{"../../schemas/common", "../../schemas/providers"} {
			files, err := filepath.Glob(filepath.Join(dir, "*.schema.json"))
			if err != nil || len(files) == 0 {
				schemaErr = fmt.Errorf("no schemas under %s", dir)
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					schemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					schemaErr = fmt.Errorf("%s: %w", f, err)
					return
				}
				obj, _ := doc.(map[string]any)
				id, _ := obj["$id"].(string)
				if id == "" {
					schemaErr = fmt.Errorf("%s has no $id", f)
					return
				}
				if err := c.AddResource(id, doc); err != nil {
					schemaErr = fmt.Errorf("%s: %w", f, err)
					return
				}
			}
		}
		schemaComp = c
	})
	return schemaComp, schemaErr
}

// schemaViolations validates the JSON of v (or the raw bytes) against the schema
// urn and returns the validator's message, or "" when it is valid.
func schemaViolations(t testing.TB, urn string, v any) string {
	t.Helper()
	raw, ok := v.([]byte)
	if !ok {
		var err error
		if raw, err = json.Marshal(v); err != nil {
			t.Fatalf("marshal for %s: %v", urn, err)
		}
	}
	c, err := schemaCompiler()
	if err != nil {
		t.Fatalf("schemas: %v", err)
	}
	schemaMu.Lock()
	sch, err := c.Compile(urn)
	schemaMu.Unlock()
	if err != nil {
		t.Fatalf("compile %s: %v", urn, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s: not JSON: %v", urn, err)
	}
	if err := sch.Validate(inst); err != nil {
		return strings.TrimSpace(err.Error())
	}
	return ""
}

func assertValid(t testing.TB, urn string, v any) {
	t.Helper()
	if msg := schemaViolations(t, urn, v); msg != "" {
		t.Errorf("does not validate against %s:\n%s", urn, msg)
	}
}
