package loop

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

//go:embed schemas/*.schema.json
var schemaFS embed.FS

const (
	schemaStepProposal = "urn:alixai:schema:analyst:step-proposal:v1"
	modelStepID        = "urn:alixai:schema:analyst:step-proposal-model:v1"
	authorityKey       = "x-alixai-authority"
	authorityModel     = "model"
)

var (
	baseSchemaOnce sync.Once
	baseSchemaDoc  map[string]any
	baseSchemaErr  error
	compiledOnce   sync.Map // key: kindsKey → *jsonschema.Schema
)

// EmbeddedSchema returns a copy of one embedded schema file by name (drift tests).
func EmbeddedSchema(name string) ([]byte, error) { return schemaFS.ReadFile("schemas/" + name) }

// EmbeddedSchemaNames lists embedded schema files.
func EmbeddedSchemaNames() []string {
	entries, _ := fs.ReadDir(schemaFS, "schemas")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func loadBaseStepSchema() (map[string]any, error) {
	baseSchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		entries, err := fs.ReadDir(schemaFS, "schemas")
		if err != nil {
			baseSchemaErr = err
			return
		}
		var step map[string]any
		for _, e := range entries {
			raw, err := schemaFS.ReadFile("schemas/" + e.Name())
			if err != nil {
				baseSchemaErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				baseSchemaErr = fmt.Errorf("analyst/loop: schema %s: %w", e.Name(), err)
				return
			}
			obj, _ := doc.(map[string]any)
			id, _ := obj["$id"].(string)
			if id == schemaStepProposal {
				step = obj
				continue
			}
			if err := c.AddResource(id, doc); err != nil {
				baseSchemaErr = err
				return
			}
		}
		if step == nil {
			baseSchemaErr = fmt.Errorf("analyst/loop: step-proposal schema missing")
			return
		}
		// Keep supporting resources on a throwaway compiler load; we only need the step doc
		// for projection. Validation compiles per ability set below.
		_ = c
		baseSchemaDoc = step
	})
	return baseSchemaDoc, baseSchemaErr
}

// CompactSchema builds the JSON Schema offered to the model for this ability set:
// model-authority projection with kind enum limited to AllowedKinds. Disabled
// abilities are absent from the grammar.
func CompactSchema(a AbilityFlags) (json.RawMessage, error) {
	doc, err := loadBaseStepSchema()
	if err != nil {
		return nil, err
	}
	proj, err := modelProjection(doc)
	if err != nil {
		return nil, err
	}
	kinds := AllowedKinds(a)
	enum := make([]any, len(kinds))
	for i, k := range kinds {
		enum[i] = string(k)
	}
	props, _ := proj["properties"].(map[string]any)
	kindProp, _ := props["kind"].(map[string]any)
	if kindProp == nil {
		return nil, fmt.Errorf("analyst/loop: kind property missing after projection")
	}
	kindCopy := copyMap(kindProp)
	kindCopy["enum"] = enum
	propsCopy := copyMap(props)
	propsCopy["kind"] = kindCopy
	// Drop payload properties for disabled kinds so the grammar matches the prompt.
	for _, drop := range []struct {
		on   bool
		name string
	}{
		{a.Compute, "compute"},
		{a.WebSearch, "search"},
		{a.WebFetch, "fetch"},
	} {
		if !drop.on {
			delete(propsCopy, drop.name)
		}
	}
	out := copyMap(proj)
	out["properties"] = propsCopy
	out["$id"] = modelStepID
	delete(out, "title")
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func modelProjection(schema map[string]any) (map[string]any, error) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("analyst/loop: step schema has no properties")
	}
	out := copyMap(schema)
	kept := make(map[string]any, len(props))
	removed := map[string]bool{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("analyst/loop: property %q is not an object", name)
		}
		auth, ok := p[authorityKey].(string)
		if !ok {
			return nil, fmt.Errorf("analyst/loop: property %q has no %s", name, authorityKey)
		}
		if auth == authorityModel {
			kept[name] = raw
		} else {
			removed[name] = true
		}
	}
	out["properties"] = kept
	if req, ok := schema["required"].([]any); ok {
		var keep []any
		for _, r := range req {
			if s, ok := r.(string); ok && removed[s] {
				continue
			}
			keep = append(keep, r)
		}
		out["required"] = keep
	}
	return out, nil
}

func kindsKey(a AbilityFlags) string {
	return fmt.Sprintf("c%d_s%d_f%d", btoi(a.Compute), btoi(a.WebSearch), btoi(a.WebFetch))
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

// validateStepJSON validates raw against the ability-filtered model schema.
func validateStepJSON(raw []byte, a AbilityFlags) error {
	sch, err := compiledModelSchema(a)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("not valid JSON")
	}
	return sch.Validate(inst)
}

func compiledModelSchema(a AbilityFlags) (*jsonschema.Schema, error) {
	key := kindsKey(a)
	if v, ok := compiledOnce.Load(key); ok {
		return v.(*jsonschema.Schema), nil
	}
	compact, err := CompactSchema(a)
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	entries, err := fs.ReadDir(schemaFS, "schemas")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		raw, err := schemaFS.ReadFile("schemas/" + e.Name())
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		obj, _ := doc.(map[string]any)
		id, _ := obj["$id"].(string)
		if id == schemaStepProposal {
			continue
		}
		if err := c.AddResource(id, doc); err != nil {
			return nil, err
		}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(compact))
	if err != nil {
		return nil, err
	}
	if err := c.AddResource(modelStepID, doc); err != nil {
		return nil, err
	}
	sch, err := c.Compile(modelStepID)
	if err != nil {
		return nil, err
	}
	compiledOnce.Store(key, sch)
	return sch, nil
}

// Ensure kinds referenced for documentation / tests.
var _ = an.StepProposalKindRead
