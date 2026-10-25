package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ragResultSchema is the schema of the .rag file. It is embedded, so the tool
// validates its own output whatever directory it runs from; the bench schema
// belongs to M0-11 and is read from the repository's schemas/ tree, as
// tools/bench reads it.
//
//go:embed rag-result.schema.json
var ragResultSchema []byte

const (
	ragResultSchemaID   = "urn:alixai:schema:rageval:rag-result:v1"
	benchResultSchema   = "bench/result.schema.json"
	benchResultSchemaID = "urn:alixai:schema:bench:result:v1"
)

// FindSchemasDir returns the schemas directory to use: explicit when not empty,
// otherwise the "schemas" directory beside the nearest go.mod found by walking
// up from the working directory (so tests, which run in the package directory,
// find it too).
func FindSchemasDir(explicit string) (string, error) {
	if explicit != "" {
		if st, err := os.Stat(filepath.Join(explicit, filepath.FromSlash(benchResultSchema))); err != nil || st.IsDir() {
			return "", fmt.Errorf("%s does not look like the schemas directory (no %s)", explicit, benchResultSchema)
		}
		return explicit, nil
	}
	root, err := FindRepoRoot()
	if err != nil {
		return "", fmt.Errorf("cannot find the schemas directory: %w (pass --schemas)", err)
	}
	return filepath.Join(root, "schemas"), nil
}

// FindRepoRoot walks up from the working directory to the directory holding go.mod.
func FindRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if st, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !st.IsDir() {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("no go.mod above the working directory")
		}
	}
}

func newSchemaCompiler() *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	return c
}

// LoadBenchSchema compiles schemas/bench/result.schema.json, which is
// self-contained (no $ref into another schema).
func LoadBenchSchema(dir string) (*jsonschema.Schema, error) {
	p := filepath.Join(dir, filepath.FromSlash(benchResultSchema))
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("schema file %s does not exist", p)
		}
		return nil, err
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	c := newSchemaCompiler()
	if err := c.AddResource(benchResultSchemaID, doc); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	s, err := c.Compile(benchResultSchemaID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return s, nil
}

// LoadRAGSchema compiles the embedded schema of the .rag file.
func LoadRAGSchema() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(ragResultSchema)))
	if err != nil {
		return nil, fmt.Errorf("rag-result.schema.json: %w", err)
	}
	c := newSchemaCompiler()
	if err := c.AddResource(ragResultSchemaID, doc); err != nil {
		return nil, err
	}
	return c.Compile(ragResultSchemaID)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// firstProblemLines returns up to n problem lines of a validation error.
func firstProblemLines(err error, n int) string {
	var lines []string
	for _, l := range strings.Split(err.Error(), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "- ") {
			lines = append(lines, strings.TrimPrefix(l, "- "))
			if len(lines) == n {
				break
			}
		}
	}
	if len(lines) == 0 {
		return clipRunes(err.Error(), 300)
	}
	return strings.Join(lines, "; ")
}
