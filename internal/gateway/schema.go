package gateway

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaFS holds byte-identical copies of the published schemas the spike
// validates against (go:embed cannot reach outside the package):
// task-proposal.schema.json and the ids library it references.
// TestEmbeddedSchemasMatchPublished fails when a copy and its original differ.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

const (
	// schemaProposalID is the $id of schemas/conversation/task-proposal.
	schemaProposalID = "urn:alixai:schema:conversation:task-proposal:v1"
	// schemaProposalModelID names the model-authority subset once derived. It
	// is a registered resource only; no file carries this id.
	schemaProposalModelID = "urn:alixai:schema:gateway:task-proposal-model:v1"

	authorityKey   = "x-alixai-authority"
	authorityModel = "model"
)

// proposalSchemas are the compiled validators for a TaskProposal.
type proposalSchemas struct {
	// model is the subset a model is asked to produce: every property whose
	// authority is not "model" (trace_id) removed. A model that writes a
	// trace_id fails it (additionalProperties is false).
	model *jsonschema.Schema
	// full is the whole contract, applied after the core has attached the
	// trace_id.
	full *jsonschema.Schema
}

var (
	proposalOnce sync.Once
	proposalVal  *proposalSchemas
	proposalErr  error
)

// loadProposalSchemas compiles the embedded schemas once.
func loadProposalSchemas() (*proposalSchemas, error) {
	proposalOnce.Do(func() { proposalVal, proposalErr = compileProposalSchemas() })
	return proposalVal, proposalErr
}

// refuseLoader resolves nothing. Every schema the gateway compiles is
// registered up front, so a $ref that is not is a bug or an attack, never a
// reason to read a file or reach the network.
type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("gateway: schema reference %q is not available", url)
}

func newCompiler() *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(refuseLoader{})
	return c
}

func compileProposalSchemas() (*proposalSchemas, error) {
	c := newCompiler()
	var proposalDoc map[string]any
	entries, err := fs.ReadDir(schemaFS, "schemas")
	if err != nil {
		return nil, fmt.Errorf("gateway: embedded schemas: %w", err)
	}
	for _, e := range entries {
		raw, err := schemaFS.ReadFile("schemas/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("gateway: embedded schema %s: %w", e.Name(), err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("gateway: embedded schema %s is not valid JSON: %w", e.Name(), err)
		}
		obj, ok := doc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gateway: embedded schema %s is not an object", e.Name())
		}
		id, _ := obj["$id"].(string)
		if id == "" {
			return nil, fmt.Errorf("gateway: embedded schema %s has no $id", e.Name())
		}
		if err := c.AddResource(id, doc); err != nil {
			return nil, fmt.Errorf("gateway: embedded schema %s: %w", e.Name(), err)
		}
		if id == schemaProposalID {
			proposalDoc = obj
		}
	}
	if proposalDoc == nil {
		return nil, errors.New("gateway: the task proposal schema is not embedded")
	}
	proj, err := modelProjection(proposalDoc)
	if err != nil {
		return nil, err
	}
	if err := c.AddResource(schemaProposalModelID, proj); err != nil {
		return nil, fmt.Errorf("gateway: model-authority schema: %w", err)
	}
	out := &proposalSchemas{}
	if out.full, err = c.Compile(schemaProposalID); err != nil {
		return nil, fmt.Errorf("gateway: the task proposal schema does not compile: %w", err)
	}
	if out.model, err = c.Compile(schemaProposalModelID); err != nil {
		return nil, fmt.Errorf("gateway: the model-authority schema does not compile: %w", err)
	}
	return out, nil
}

// modelProjection derives the schema a model is asked to satisfy from a schema
// that annotates every property with x-alixai-authority (see
// docs/integration/conversation-objects.md section 2): every property that is
// not authority "model" is removed from both "properties" and "required",
// because additionalProperties false plus a required property that no longer
// exists would be unsatisfiable. Everything else, the allOf rules and $defs
// included, is kept and the result gets its own $id. The input is not modified.
//
// A property without an authority annotation is an error rather than a guess:
// the projection is a security boundary (the model must not produce what the
// core owns), so an unlabelled property fails closed.
func modelProjection(schema map[string]any) (map[string]any, error) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("gateway: task proposal schema has no properties")
	}
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		out[k] = v
	}
	kept := make(map[string]any, len(props))
	removed := map[string]bool{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gateway: schema property %q is not an object", name)
		}
		auth, ok := p[authorityKey].(string)
		if !ok {
			return nil, fmt.Errorf("gateway: schema property %q has no %s annotation", name, authorityKey)
		}
		if auth == authorityModel {
			kept[name] = raw
		} else {
			removed[name] = true
		}
	}
	if len(kept) == 0 {
		return nil, errors.New("gateway: no model-authority property left after projection")
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
	out["$id"] = schemaProposalModelID
	delete(out, "title")
	return out, nil
}

// maxViolations bounds how many schema violations an error names.
const maxViolations = 4

// violations lists where and by which keyword a value broke a schema, for
// example "intent: enum". The validator's own messages quote the offending
// value, which here is model output, so they never leave this function.
func violations(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{"(root): schema"}
	}
	var out []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(out) >= maxViolations {
			return
		}
		if len(e.Causes) == 0 {
			kw := "schema"
			if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
				kw = kp[len(kp)-1]
			}
			loc := "(root)"
			if len(e.InstanceLocation) > 0 {
				parts := make([]string, len(e.InstanceLocation))
				for i, p := range e.InstanceLocation {
					parts[i] = printable(p, 40)
				}
				loc = strings.Join(parts, "/")
			}
			out = append(out, loc+": "+kw)
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) == 0 {
		out = []string{"(root): schema"}
	}
	return out
}

// printable keeps a JSON pointer token printable and short; tokens are member
// names chosen by the model.
func printable(s string, max int) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < max; i++ {
		c := s[i]
		if c < ' ' || c > '~' {
			c = '?'
		}
		b = append(b, c)
	}
	return string(b)
}

// EmbeddedSchema returns a copy of one embedded schema file by file name, for
// the drift test.
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
