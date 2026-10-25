package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The bench reads its schemas from the repository's schemas/ tree at run time
// instead of embedding copies: go:embed cannot reach a sibling directory, and a
// copy would drift from the TaskProposal contract the model output is judged
// against. The tool is run from the repository (go run ./tools/bench), so the
// tree is always there.

const (
	resultSchemaFile     = "bench/result.schema.json"
	corpusCaseSchemaFile = "bench/corpus-case.schema.json"
	proposalSchemaFile   = "conversation/task-proposal.schema.json"

	// proposalModelID names the model-authority subset of the TaskProposal
	// schema once it is derived. It is a registered resource only; no file
	// carries this id.
	proposalModelID = "urn:alixai:schema:bench:task-proposal-model:v1"

	authorityKey   = "x-alixai-authority"
	authorityModel = "model"
)

var schemaURNPattern = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9]+(?:-[a-z0-9]+)*):([a-z0-9]+(?:-[a-z0-9]+)*):v[1-9][0-9]*$`)

// FindSchemasDir returns the schemas directory to use: explicit when not
// empty, otherwise the "schemas" directory beside the nearest go.mod found by
// walking up from the working directory (so tests, which run in the package
// directory, find it too).
func FindSchemasDir(explicit string) (string, error) {
	if explicit != "" {
		if st, err := os.Stat(filepath.Join(explicit, filepath.FromSlash(resultSchemaFile))); err != nil || st.IsDir() {
			return "", fmt.Errorf("%s does not look like the schemas directory (no %s)", explicit, resultSchemaFile)
		}
		return explicit, nil
	}
	root, err := FindRepoRoot()
	if err != nil {
		return "", fmt.Errorf("cannot find the schemas directory: %w (pass --schemas)", err)
	}
	return filepath.Join(root, "schemas"), nil
}

// FindRepoRoot walks up from the working directory to the directory holding
// go.mod.
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

// urnLoader resolves urn:alixai:schema:<area>:<name>:v<N> to
// <dir>/<area>/<name>.schema.json. It refuses everything else, so compiling a
// schema never reads another path or the network.
type urnLoader struct{ dir string }

func (l urnLoader) Load(url string) (any, error) {
	m := schemaURNPattern.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("only urn:alixai:schema:<area>:<name>:v<N> references can be resolved; got %q", url)
	}
	p := filepath.Join(l.dir, m[1], m[2]+".schema.json")
	doc, err := readSchemaFile(p)
	if err != nil {
		return nil, err
	}
	if obj, ok := doc.(map[string]any); !ok || obj["$id"] != url {
		return nil, fmt.Errorf("%s does not declare $id %q", p, url)
	}
	return doc, nil
}

func readSchemaFile(path string) (any, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("schema file %s does not exist", path)
		}
		return nil, err
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

func newCompiler(dir string) *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(urnLoader{dir: dir})
	return c
}

// compileFile compiles the schema stored at rel (slash separated, under dir).
func compileFile(dir, rel string) (*jsonschema.Schema, error) {
	doc, err := readSchemaFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: a schema must be a JSON object", rel)
	}
	id, _ := obj["$id"].(string)
	if id == "" {
		return nil, fmt.Errorf("%s declares no $id", rel)
	}
	c := newCompiler(dir)
	if err := c.AddResource(id, doc); err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	s, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("%s does not compile: %w", rel, err)
	}
	return s, nil
}

// LoadResultSchema compiles schemas/bench/result.schema.json.
func LoadResultSchema(dir string) (*jsonschema.Schema, error) {
	return compileFile(dir, resultSchemaFile)
}

// LoadCorpusCaseSchema compiles schemas/bench/corpus-case.schema.json.
func LoadCorpusCaseSchema(dir string) (*jsonschema.Schema, error) {
	return compileFile(dir, corpusCaseSchemaFile)
}

// LoadProposalSchema compiles the model-authority subset of the TaskProposal
// schema: the document a model's output is validated against.
func LoadProposalSchema(dir string) (*jsonschema.Schema, error) {
	doc, err := readSchemaFile(filepath.Join(dir, filepath.FromSlash(proposalSchemaFile)))
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: a schema must be a JSON object", proposalSchemaFile)
	}
	proj, err := ModelProjection(obj)
	if err != nil {
		return nil, err
	}
	c := newCompiler(dir)
	if err := c.AddResource(proposalModelID, proj); err != nil {
		return nil, err
	}
	s, err := c.Compile(proposalModelID)
	if err != nil {
		return nil, fmt.Errorf("model-only TaskProposal schema does not compile: %w", err)
	}
	return s, nil
}

// ModelProjection derives the schema a model is asked to satisfy from a schema
// that annotates every property with x-alixai-authority (see
// docs/integration/conversation-objects.md section 2): every property that is
// not authority "model" (trace_id, which the core attaches after the call) is
// removed from both "properties" and "required", because additionalProperties
// false plus a required property that no longer exists would be
// unsatisfiable. Everything else, the allOf rules and $defs included, is kept
// and the result gets its own $id. The input is not modified.
//
// A property without an authority annotation is an error rather than a guess:
// the projection is a security boundary (the model must not produce what the
// core owns), so an unlabelled property fails closed.
func ModelProjection(schema map[string]any) (map[string]any, error) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("task proposal schema has no properties")
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	keptProps := make(map[string]any, len(props))
	removed := map[string]bool{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("property %q is not a schema object", name)
		}
		auth, ok := p[authorityKey].(string)
		if !ok {
			return nil, fmt.Errorf("property %q has no %s annotation", name, authorityKey)
		}
		if auth == authorityModel {
			keptProps[name] = raw
		} else {
			removed[name] = true
		}
	}
	if len(keptProps) == 0 {
		return nil, errors.New("no model-authority property left after projection")
	}
	out["properties"] = keptProps
	if req, ok := schema["required"].([]any); ok {
		var kept []any
		for _, r := range req {
			if s, ok := r.(string); ok && removed[s] {
				continue
			}
			kept = append(kept, r)
		}
		out["required"] = kept
	}
	out["$id"] = proposalModelID
	delete(out, "title")
	if d, ok := out["description"].(string); ok {
		out["description"] = "Model-authority subset of: " + firstSentence(d)
	}
	return out, nil
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i+1]
	}
	return s
}
