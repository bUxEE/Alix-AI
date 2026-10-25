package models

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// SchemaID is the $id of the registry entry schema.
const SchemaID = "urn:alixai:schema:models:registry-entry:v1"

// SchemaRevision is the revision of the entry schema, the x-schema-revision
// keyword of the published file. Revision 2 (D-11) added the decision layer;
// the $id keeps v1 because every revision-1 entry is still valid.
const SchemaRevision = 2

// maxEntryBytes bounds one registry file. A real entry is a few KiB; refusing
// anything near this size stops a runaway or hostile file before it is parsed.
const maxEntryBytes = 256 << 10

// maxSchemaViolations bounds how many schema failures one file reports.
const maxSchemaViolations = 12

// schemaJSON is a byte-identical copy of schemas/models/registry-entry.schema.json
// (go:embed cannot reach outside the package). TestEmbeddedSchemaMatchesPublished
// fails when the two differ.
//
//go:embed registry-entry.schema.json
var schemaJSON []byte

// SchemaJSON returns a copy of the embedded registry entry schema.
func SchemaJSON() []byte { return append([]byte(nil), schemaJSON...) }

var (
	schemaOnce     sync.Once
	schemaCompiled *jsonschema.Schema
	schemaErr      error
)

// compiledSchema compiles the embedded schema once, as JSON Schema 2020-12 with
// format assertions on (so a malformed date is rejected, not ignored).
func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
		if err != nil {
			schemaErr = fmt.Errorf("models: embedded schema is not valid JSON: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		if err := c.AddResource(SchemaID, doc); err != nil {
			schemaErr = fmt.Errorf("models: embedded schema: %w", err)
			return
		}
		schemaCompiled, schemaErr = c.Compile(SchemaID)
		if schemaErr != nil {
			schemaErr = fmt.Errorf("models: embedded schema does not compile: %w", schemaErr)
		}
	})
	return schemaCompiled, schemaErr
}

// ParseEntry parses and validates one registry file. file is used only in
// violation messages. It checks, in order: the bytes are one plain YAML
// document, the document satisfies the embedded JSON Schema (which rejects
// unknown fields), the document decodes into Entry with unknown fields
// refused again, and the entry's own rules (Entry.Validate) hold. It returns
// the entry only when there is no violation at all.
func ParseEntry(file string, data []byte) (Entry, []Violation) {
	var zero Entry
	fail := func(rule Rule, field, format string, args ...any) (Entry, []Violation) {
		return zero, []Violation{{File: file, Rule: rule, Field: field, Detail: fmt.Sprintf(format, args...)}}
	}
	if len(data) > maxEntryBytes {
		return fail(RuleFile, "", "file is %d bytes, over the %d byte limit", len(data), maxEntryBytes)
	}

	generic, err := yamlToValue(data)
	if err != nil {
		return fail(RuleYAML, "", "%v", err)
	}
	asJSON, err := json.Marshal(generic)
	if err != nil {
		return fail(RuleYAML, "", "value cannot be represented as JSON: %v", err)
	}

	sch, err := compiledSchema()
	if err != nil {
		return fail(RuleSchema, "", "%v", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		return fail(RuleYAML, "", "%v", err)
	}
	if err := sch.Validate(instance); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return fail(RuleSchema, "", "%v", err)
		}
		return zero, schemaViolations(file, ve)
	}

	var e Entry
	dec := json.NewDecoder(bytes.NewReader(asJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return fail(RuleDecode, "", "%v", err)
	}
	if vs := e.Validate(); len(vs) > 0 {
		for i := range vs {
			vs[i].File = file
		}
		return zero, vs
	}
	return e, nil
}

// schemaViolations turns a validation failure into one Violation per leaf
// problem, in schema order, capped at maxSchemaViolations. Leaves are read from
// the detailed output tree: the library's flattened "basic" output replaces a
// leaf's message with that of the $ref above it, which would hide the cause.
func schemaViolations(file string, ve *jsonschema.ValidationError) []Violation {
	var vs []Violation
	var walk func(u jsonschema.OutputUnit)
	walk = func(u jsonschema.OutputUnit) {
		if len(vs) >= maxSchemaViolations {
			return
		}
		if len(u.Errors) == 0 {
			if u.Error != nil {
				field := u.InstanceLocation
				if field == "" {
					field = "/"
				}
				vs = append(vs, Violation{File: file, Rule: RuleSchema, Field: field, Detail: u.Error.String()})
			}
			return
		}
		for _, c := range u.Errors {
			walk(c)
		}
	}
	walk(*ve.DetailedOutput())
	if len(vs) == 0 {
		vs = append(vs, Violation{File: file, Rule: RuleSchema, Detail: ve.Error()})
	}
	return vs
}

// yamlToValue turns one YAML document into the JSON data model (maps, slices,
// strings, numbers, booleans, nil) using the plain subset registry files are
// allowed:
//
//   - exactly one document, whose root is a mapping;
//   - mapping keys are plain strings and appear once;
//   - no anchors, aliases, merge keys or custom tags;
//   - scalars are string, int, float, bool or null; an unquoted date or
//     timestamp keeps its written text, so 2026-10-02 stays a string.
//
// Working on the node tree (rather than decoding into any) is what allows the
// duplicate-key and alias checks, and what keeps dates from turning into
// time.Time values that no longer match the schema's date format.
func yamlToValue(data []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("more than one YAML document; want exactly one")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("not a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("line %d: top level must be a mapping of field name to value", root.Line)
	}
	return nodeValue(root)
}

func nodeValue(n *yaml.Node) (any, error) {
	if n.Anchor != "" {
		return nil, fmt.Errorf("line %d: YAML anchors are not allowed", n.Line)
	}
	switch n.Kind {
	case yaml.AliasNode:
		return nil, fmt.Errorf("line %d: YAML aliases are not allowed", n.Line)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		seen := make(map[string]int, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" {
				return nil, fmt.Errorf("line %d: mapping keys must be plain strings", k.Line)
			}
			if k.Anchor != "" {
				return nil, fmt.Errorf("line %d: YAML anchors are not allowed", k.Line)
			}
			if first, dup := seen[k.Value]; dup {
				return nil, fmt.Errorf("line %d: key %q already defined at line %d", k.Line, k.Value, first)
			}
			seen[k.Value] = k.Line
			val, err := nodeValue(v)
			if err != nil {
				return nil, err
			}
			m[k.Value] = val
		}
		return m, nil
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			val, err := nodeValue(c)
			if err != nil {
				return nil, err
			}
			s = append(s, val)
		}
		return s, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!str":
			return n.Value, nil
		case "!!null":
			return nil, nil
		case "!!timestamp":
			return n.Value, nil
		case "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, fmt.Errorf("line %d: %w", n.Line, err)
			}
			return v, nil
		default:
			return nil, fmt.Errorf("line %d: tag %s is not allowed", n.Line, strings.TrimSpace(n.Tag))
		}
	default:
		return nil, fmt.Errorf("line %d: unexpected YAML node", n.Line)
	}
}
