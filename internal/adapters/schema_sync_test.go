package adapters

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// published maps each embedded schema file to the published original.
var published = map[string]string{
	"deterministic-result.schema.json": filepath.Join("..", "..", "schemas", "actions", "deterministic-result.schema.json"),
	"shared.schema.json":               filepath.Join("..", "..", "schemas", "actions", "shared.schema.json"),
	"ids.schema.json":                  filepath.Join("..", "..", "schemas", "common", "ids.schema.json"),
	"timestamps.schema.json":           filepath.Join("..", "..", "schemas", "common", "timestamps.schema.json"),
	"error.schema.json":                filepath.Join("..", "..", "schemas", "common", "error.schema.json"),
}

// TestEmbeddedSchemasMatchPublished keeps the copies of the schemas that this
// package embeds identical to the published ones in schemas/. A change to a
// published schema that is not copied here would let the runtime check and the
// contract drift apart without anything failing.
func TestEmbeddedSchemasMatchPublished(t *testing.T) {
	var embedded []string
	embedded = append(embedded, EmbeddedSchemaNames()...)
	sort.Strings(embedded)
	var want []string
	for name := range published {
		want = append(want, name)
	}
	sort.Strings(want)
	if len(embedded) != len(want) {
		t.Fatalf("embedded schemas = %v, want %v", embedded, want)
	}
	for i := range want {
		if embedded[i] != want[i] {
			t.Fatalf("embedded schemas = %v, want %v", embedded, want)
		}
	}
	for name, path := range published {
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		copy, err := EmbeddedSchema(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(orig, copy) {
			t.Errorf("internal/adapters/schemas/%s differs from %s; copy the published file over the embedded one", name, filepath.ToSlash(path))
		}
	}
}

// TestEmbeddedSchemasCompile: the embedded set is self-contained, so the
// result and fact schemas compile with every $ref resolved.
func TestEmbeddedSchemasCompile(t *testing.T) {
	r, f, err := compiledSchemas()
	if err != nil || r == nil || f == nil {
		t.Fatalf("compile: %v", err)
	}
}
