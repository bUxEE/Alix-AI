package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	taskProposalRel     = "conversation/task-proposal.schema.json"
	analystStepRel      = "analyst/step-proposal.schema.json"
	taskProposalModelID = "urn:alixai:schema:llmbench:task-proposal-model:v1"
	analystStepModelID  = "urn:alixai:schema:llmbench:analyst-step-model:v1"
	authorityKey        = "x-alixai-authority"
	authorityModel      = "model"
)

var schemaURNPattern = regexp.MustCompile(`^urn:alixai:schema:([a-z0-9]+(?:-[a-z0-9]+)*):([a-z0-9]+(?:-[a-z0-9]+)*):v[1-9][0-9]*$`)

// QualitySchemas holds compiled model-authority projections.
type QualitySchemas struct {
	TaskProposal *jsonschema.Schema
	AnalystStep  *jsonschema.Schema
}

type urnLoader struct{ dir string }

func (l urnLoader) Load(url string) (any, error) {
	m := schemaURNPattern.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("only urn:alixai:schema references; got %q", url)
	}
	p := filepath.Join(l.dir, m[1], m[2]+".schema.json")
	return readSchemaFile(p)
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

func findSchemasDir(explicit string) (string, error) {
	if explicit != "" {
		if st, err := os.Stat(filepath.Join(explicit, filepath.FromSlash(taskProposalRel))); err != nil || st.IsDir() {
			return "", fmt.Errorf("%s is not a schemas directory", explicit)
		}
		return explicit, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		cand := filepath.Join(d, "schemas")
		if st, err := os.Stat(filepath.Join(cand, filepath.FromSlash(taskProposalRel))); err == nil && !st.IsDir() {
			return cand, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("schemas/ not found (pass -schemas)")
		}
	}
}

func newSchemaCompiler(dir string) *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(urnLoader{dir: dir})
	return c
}

// modelProjection keeps only x-alixai-authority=model properties (drops core-owned trace_id).
func modelProjection(schema map[string]any, newID string) (map[string]any, error) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("schema has no properties")
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	kept := make(map[string]any)
	removed := map[string]bool{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("property %q not an object", name)
		}
		auth, ok := p[authorityKey].(string)
		if !ok {
			return nil, fmt.Errorf("property %q missing %s", name, authorityKey)
		}
		if auth == authorityModel {
			kept[name] = raw
		} else {
			removed[name] = true
		}
	}
	out["properties"] = kept
	if req, ok := schema["required"].([]any); ok {
		var keptReq []any
		for _, r := range req {
			if s, ok := r.(string); ok && removed[s] {
				continue
			}
			keptReq = append(keptReq, r)
		}
		out["required"] = keptReq
	}
	out["$id"] = newID
	return out, nil
}

func loadModelSchema(dir, rel, modelID string) (*jsonschema.Schema, error) {
	doc, err := readSchemaFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: not an object", rel)
	}
	proj, err := modelProjection(obj, modelID)
	if err != nil {
		return nil, err
	}
	c := newSchemaCompiler(dir)
	if err := c.AddResource(modelID, proj); err != nil {
		return nil, err
	}
	return c.Compile(modelID)
}

// LoadQualitySchemas compiles TaskProposal and AnalystStepProposal model subsets.
func LoadQualitySchemas(schemasDir string) (*QualitySchemas, error) {
	dir, err := findSchemasDir(schemasDir)
	if err != nil {
		return nil, err
	}
	tp, err := loadModelSchema(dir, taskProposalRel, taskProposalModelID)
	if err != nil {
		return nil, fmt.Errorf("task proposal: %w", err)
	}
	as, err := loadModelSchema(dir, analystStepRel, analystStepModelID)
	if err != nil {
		return nil, fmt.Errorf("analyst step: %w", err)
	}
	return &QualitySchemas{TaskProposal: tp, AnalystStep: as}, nil
}
