package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// repoRoot returns the repository root (two levels above this package).
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}
	return root
}

// realConfig is the configuration `go run ./tools/schemagen` uses.
func realConfig(t *testing.T) Config {
	t.Helper()
	root := repoRoot(t)
	module, err := modulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Schemas:    os.DirFS(filepath.Join(root, "schemas")),
		SchemasRel: "schemas",
		ModulePath: module,
		GoOut:      "pkg/contracts/gen",
		TSOut:      "sdk/typescript/src/gen",
	}
}

// generateReal generates from the repository's schemas.
func generateReal(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := Generate(realConfig(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return files
}

// copySchemas copies every schema file (not the examples) from the repository
// into dst/schemas.
func copySchemas(t *testing.T, dst string) {
	t.Helper()
	src := filepath.Join(repoRoot(t), "schemas")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "examples" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, "schemas", rel), 0o755)
		}
		if !strings.HasSuffix(p, ".schema.json") {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, "schemas", rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// tempRepo makes a throw-away repository root holding a copy of the schemas and
// a go.mod, and returns it.
func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	copySchemas(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/repo\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runCLI runs schemagen against a root and returns its exit code and output.
func runCLI(t *testing.T, root string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"-root", root}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// schemaDoc wraps a schema body (the members after $id) into a complete schema
// file for the given area and name.
func schemaDoc(area, name, body string) string {
	return `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"urn:alixai:schema:` +
		area + `:` + name + `:v1",` + body + `}`
}

// mapSchemas builds an in-memory schemas root from area/name -> body.
func mapSchemas(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for key, body := range files {
		area, name, _ := strings.Cut(key, "/")
		m[area+"/"+name+".schema.json"] = &fstest.MapFile{Data: []byte(schemaDoc(area, name, body))}
	}
	return m
}

// fixtureConfig is a Config over an in-memory schemas root.
func fixtureConfig(files map[string]string) Config {
	return Config{
		Schemas:    mapSchemas(files),
		SchemasRel: "schemas",
		ModulePath: "example.test/repo",
		GoOut:      "out/go",
		TSOut:      "out/ts",
	}
}
