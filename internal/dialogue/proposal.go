package dialogue

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// schemaFS holds byte-identical copies of the published schemas the proposal
// check needs (go:embed cannot reach outside the package). TestEmbeddedSchemasMatchPublished
// fails when a copy and its original differ, so a schema change that is not copied
// here cannot go unnoticed.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

const (
	// SchemaTaskProposal is the $id of the published TaskProposal schema.
	SchemaTaskProposal = "urn:alixai:schema:conversation:task-proposal:v1"

	// proposalModelID names the model-authority subset of the TaskProposal schema
	// once it is derived. It is a registered resource only; no file carries it.
	proposalModelID = "urn:alixai:schema:dialogue:task-proposal-model:v1"

	authorityKey   = "x-alixai-authority"
	authorityModel = "model"
)

var (
	modelSchemaOnce sync.Once
	modelSchema     *jsonschema.Schema
	modelSchemaErr  error
)

// modelProposalSchema compiles, once, the schema a model's output is validated
// against: the TaskProposal schema with every property that is not authority
// "model" removed (docs/integration/conversation-objects.md, section 3).
func modelProposalSchema() (*jsonschema.Schema, error) {
	modelSchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		var proposal map[string]any
		entries, err := fs.ReadDir(schemaFS, "schemas")
		if err != nil {
			modelSchemaErr = fmt.Errorf("dialogue: embedded schemas: %w", err)
			return
		}
		for _, e := range entries {
			raw, err := schemaFS.ReadFile("schemas/" + e.Name())
			if err != nil {
				modelSchemaErr = fmt.Errorf("dialogue: embedded schema %s: %w", e.Name(), err)
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				modelSchemaErr = fmt.Errorf("dialogue: embedded schema %s is not valid JSON: %w", e.Name(), err)
				return
			}
			obj, _ := doc.(map[string]any)
			id, _ := obj["$id"].(string)
			if id == "" {
				modelSchemaErr = fmt.Errorf("dialogue: embedded schema %s has no $id", e.Name())
				return
			}
			if id == SchemaTaskProposal {
				proposal = obj
				continue // registered below, projected
			}
			if err := c.AddResource(id, doc); err != nil {
				modelSchemaErr = fmt.Errorf("dialogue: embedded schema %s: %w", e.Name(), err)
				return
			}
		}
		if proposal == nil {
			modelSchemaErr = errors.New("dialogue: the TaskProposal schema is not embedded")
			return
		}
		proj, err := modelProjection(proposal)
		if err != nil {
			modelSchemaErr = err
			return
		}
		if err := c.AddResource(proposalModelID, proj); err != nil {
			modelSchemaErr = err
			return
		}
		if modelSchema, modelSchemaErr = c.Compile(proposalModelID); modelSchemaErr != nil {
			modelSchemaErr = fmt.Errorf("dialogue: the model-only TaskProposal schema does not compile: %w", modelSchemaErr)
		}
	})
	return modelSchema, modelSchemaErr
}

// modelProjection derives the schema a model is asked to satisfy from a schema
// that annotates every property with x-alixai-authority: every property that is
// not authority "model" (trace_id, which the core attaches after the call) is
// removed from both "properties" and "required", because additionalProperties
// false plus a required property that no longer exists would be unsatisfiable.
// Everything else, the allOf rules and $defs included, is kept. The input is not
// modified.
//
// A property without an authority annotation is an error rather than a guess: the
// projection is a security boundary (the model must not produce what the core
// owns), so an unlabelled property fails closed.
func modelProjection(schema map[string]any) (map[string]any, error) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, errors.New("dialogue: the TaskProposal schema has no properties")
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
			return nil, fmt.Errorf("dialogue: property %q is not a schema object", name)
		}
		auth, ok := p[authorityKey].(string)
		if !ok {
			return nil, fmt.Errorf("dialogue: property %q has no %s annotation", name, authorityKey)
		}
		if auth == authorityModel {
			kept[name] = raw
		} else {
			removed[name] = true
		}
	}
	if len(kept) == 0 {
		return nil, errors.New("dialogue: no model-authority property is left after projection")
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
	out["$id"] = proposalModelID
	delete(out, "title")
	return out, nil
}

// EmbeddedSchema returns a copy of one embedded schema file by name, for the drift test.
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

// maxProposalBytes bounds the model output the engine will parse. The constraint
// and the output budget keep a real proposal far below this.
const maxProposalBytes = 64 << 10

// parseProposal turns the model's constrained output into a TaskProposal. It is
// the "schema validation" step of the pipeline: the output must be one JSON
// document that satisfies the model-authority schema (so a trace_id, an unknown
// member, a wrong type or a missing member is rejected) and then it decodes into
// the generated type. Anything else is schema_invalid, and no part of a rejected
// output is used. The core's trace ID is attached afterwards.
//
// The errors never echo the output: it came from a model and may hold user or
// tenant text.
func parseProposal(raw []byte, traceID string) (contracts.TaskProposal, error) {
	var none contracts.TaskProposal
	if len(raw) == 0 || len(raw) > maxProposalBytes {
		return none, contracts.New(contracts.ErrSchemaInvalid, "dialogue: the model output is empty or too large to be a task proposal").
			WithDetails("output_len", len(raw))
	}
	sch, err := modelProposalSchema()
	if err != nil {
		return none, contracts.Wrap(contracts.ErrInternal, err, "dialogue: the proposal schema is unavailable")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return none, contracts.New(contracts.ErrSchemaInvalid, "dialogue: the model output is not valid JSON")
	}
	if err := sch.Validate(inst); err != nil {
		return none, contracts.New(contracts.ErrSchemaInvalid, "dialogue: the model output is not a valid task proposal: "+violationSummary(err))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var tp conversation.TaskProposal
	if err := dec.Decode(&tp); err != nil {
		return none, contracts.New(contracts.ErrSchemaInvalid, "dialogue: the model output does not decode as a task proposal")
	}
	tp.TraceID = common.TraceID(traceID)
	return tp, nil
}

// maxViolations bounds how many schema violations an error message names.
const maxViolations = 3

// violationSummary lists the locations and keywords of a schema failure, never
// the values: a value came from a model.
func violationSummary(err error) string {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return "does not satisfy the schema"
	}
	var leaves []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(leaves) >= maxViolations {
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
			leaves = append(leaves, loc+": "+kw)
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(leaves) == 0 {
		return "does not satisfy the schema"
	}
	return strings.Join(leaves, "; ")
}

// printable keeps a JSON pointer token printable and short.
func printable(s string, n int) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < n; i++ {
		c := s[i]
		if c < ' ' || c > '~' {
			c = '?'
		}
		b = append(b, c)
	}
	return string(b)
}

// ---- the constraint sent to the runtime ----

// capSchemaInfo is what the constraint builder needs of one shortlisted
// capability: its ID and its visible fields by name and JSON type.
type capSchemaInfo struct {
	ID     string
	Fields []fieldInfo
}

// fieldInfo is one visible field as the prompt and the constraint describe it.
type fieldInfo struct {
	Name       string
	Type       string // the package field type
	Required   bool
	Enum       []string
	EntityType string
	ItemType   string
	MaxItems   int64
}

// jsonType maps a package field type to the JSON type of its proposed value.
func jsonType(t string) string {
	switch t {
	case "integer":
		return "integer"
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "list":
		return "array"
	}
	return "string" // string, date, datetime, duration, enum, entity_ref
}

// proposalIntents are the intents the prompt offers. Correction and cancel need
// an earlier turn, so they are offered only when the session has a goal.
func proposalIntents(hasGoal bool) []string {
	if hasGoal {
		return []string{"task", "correction", "cancel", "help", "out_of_scope"}
	}
	return []string{"task", "help", "out_of_scope"}
}

// compactSchema is the JSON Schema sent to the runtime as the output constraint:
// the model-authority shape of a TaskProposal narrowed to the shortlisted
// capabilities (their IDs and their visible field names) and nothing else. It is
// far smaller than the contract schema, whose $defs would be counted against the
// prompt budget on every call, it names no capability the model was not offered
// and no field it may not see, and it never carries what the core owns (trace_id).
// It is a request, not a check: a runtime may apply only part of it, so every
// output is validated against the full model-authority schema regardless.
func compactSchema(caps []capSchemaInfo, hasGoal bool) json.RawMessage {
	return compactSchemaMemory(caps, hasGoal, nil, false)
}

// compactSchemaUI is compactSchema with the optional ui_action member when UI tools are offered.
func compactSchemaUI(caps []capSchemaInfo, hasGoal bool, uiNames []string) json.RawMessage {
	return compactSchemaMemory(caps, hasGoal, uiNames, false)
}

// compactSchemaMemory is compactSchemaUI plus optional memory_proposal when long-term memory is enabled (ADR 0042).
func compactSchemaMemory(caps []capSchemaInfo, hasGoal bool, uiNames []string, allowMemory bool) json.RawMessage {
	ids := make([]any, 0, len(caps)+1)
	for _, c := range caps {
		ids = append(ids, c.ID)
	}
	ids = append(ids, nil)
	intents := make([]any, 0, 5)
	for _, s := range proposalIntents(hasGoal) {
		intents = append(intents, s)
	}
	types := map[string]map[string]bool{}
	for _, c := range caps {
		for _, f := range c.Fields {
			if types[f.Name] == nil {
				types[f.Name] = map[string]bool{}
			}
			types[f.Name][jsonType(f.Type)] = true
		}
	}
	props := make(map[string]any, len(types))
	for name, set := range types {
		if set["number"] {
			delete(set, "integer")
		}
		list := make([]string, 0, len(set))
		for t := range set {
			list = append(list, t)
		}
		sort.Strings(list)
		var typ any = list[0]
		if len(list) > 1 {
			anyList := make([]any, len(list))
			for i, t := range list {
				anyList[i] = t
			}
			typ = anyList
		}
		prop := map[string]any{"type": typ}
		if len(list) == 1 && list[0] == "array" {
			prop["items"] = map[string]any{"type": []any{"string", "number", "boolean"}}
			prop["maxItems"] = 64
		}
		props[name] = prop
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"capability_id", "fields", "entity_refs", "intent", "uncertainty"},
		"properties": map[string]any{
			"capability_id": map[string]any{"enum": ids},
			"fields": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           props,
			},
			"entity_refs": map[string]any{
				"type":     "array",
				"maxItems": 4,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"entity_type", "mention"},
					"properties": map[string]any{
						"entity_type": map[string]any{"type": "string"},
						"mention":     map[string]any{"type": "string", "maxLength": 512},
						"field":       map[string]any{"type": "string"},
					},
				},
			},
			"intent": map[string]any{"enum": intents},
			"uncertainty": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []any{"overall"},
				"properties": map[string]any{
					"overall": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
			},
		},
	}
	if len(uiNames) > 0 {
		names := make([]any, len(uiNames))
		for i, n := range uiNames {
			names[i] = n
		}
		schema["properties"].(map[string]any)["ui_action"] = map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"name", "arguments"},
			"properties": map[string]any{
				"name":      map[string]any{"enum": names},
				"arguments": map[string]any{"type": "object"},
			},
		}
	}
	if allowMemory {
		schema["properties"].(map[string]any)["memory_proposal"] = map[string]any{
			"type":                 []any{"object", "null"},
			"additionalProperties": false,
			"required":             []any{"text"},
			"properties": map[string]any{
				"text":       map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
				"structured": map[string]any{"type": "object", "maxProperties": 32, "additionalProperties": map[string]any{"type": "string", "maxLength": 512}},
				"data_class": map[string]any{"enum": []any{"public", "internal", "confidential", "restricted"}},
			},
		}
	}
	b, err := json.Marshal(schema) // map keys are sorted: the output is deterministic
	if err != nil {
		panic(fmt.Sprintf("dialogue: cannot marshal the compact schema: %v", err)) // a bug: only literals go in
	}
	return b
}
