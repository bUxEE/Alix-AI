package repo

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaURN is the form of every AlixAI schema id:
// urn:alixai:schema:<area>:<name>:v<N>, stored at schemas/<area>/<name>.schema.json.
var schemaURN = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9]+(?:-[a-z0-9]+)*):([a-z0-9]+(?:-[a-z0-9]+)*):v[1-9][0-9]*$`)

// urnLoader resolves AlixAI schema URNs to files under <root>/schemas and
// refuses everything else, so compiling a schema never reads another path or
// the network.
type urnLoader struct{ root string }

func (l urnLoader) Load(url string) (any, error) {
	m := schemaURN.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("only urn:alixai:schema:<area>:<name>:v<N> references can be resolved, got %q", url)
	}
	p := filepath.Join(l.root, "schemas", m[1], m[2]+".schema.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if obj, ok := doc.(map[string]any); !ok || obj["$id"] != url {
		return nil, fmt.Errorf("%s does not declare $id %q", p, url)
	}
	return doc, nil
}

// compileSchema compiles the schema (or a "#/..." fragment of it) named by an
// AlixAI URN reference, with formats asserted as the project's own tools do.
func compileSchema(t testing.TB, root, ref string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(urnLoader{root: root})
	s, err := c.Compile(ref)
	if err != nil {
		t.Fatalf("schema %s does not compile: %v", ref, err)
	}
	return s
}

// decodeJSON decodes data keeping numbers exact (json.Number), as the schema
// validator expects.
func decodeJSON(data []byte) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}

// ---- helpers over decoded JSON ----

func jsonObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// jsonAt follows keys through nested objects.
func jsonAt(v any, keys ...string) (any, bool) {
	for _, k := range keys {
		m, ok := jsonObject(v)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

func jsonFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case interface{ Float64() (float64, error) }: // json.Number
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func jsonBool(v any) (value, ok bool) {
	b, ok := v.(bool)
	return b, ok
}

func jsonString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}
