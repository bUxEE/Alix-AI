package blocks

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// schemaFS holds byte-identical copies of the published schemas a RichBlock is
// validated against (go:embed cannot reach outside the package): the block
// schema, the citation and shared action schemas it refers to, and the common
// libraries those use. Validation needs no file system and no network, and a
// reference that is not embedded fails closed. TestEmbeddedSchemasMatchPublished
// fails when a copy and its original differ.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

// SchemaRichBlock is the $id of the published RichBlock schema.
const SchemaRichBlock = "urn:alixai:schema:conversation:rich-block:v1"

var (
	schemaOnce sync.Once
	schemaVal  *jsonschema.Schema
	schemaErr  error
)

// refuseLoader makes every reference to a resource that was not embedded an
// error: validation never reads a file or a URL.
type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("blocks: schema reference %q is not an embedded schema", url)
}

// richBlockSchema compiles the embedded schemas once, as JSON Schema 2020-12
// with format assertions on.
func richBlockSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(refuseLoader{})
		entries, err := fs.ReadDir(schemaFS, "schemas")
		if err != nil {
			schemaErr = fmt.Errorf("blocks: embedded schemas: %w", err)
			return
		}
		for _, e := range entries {
			raw, err := schemaFS.ReadFile("schemas/" + e.Name())
			if err != nil {
				schemaErr = fmt.Errorf("blocks: embedded schema %s: %w", e.Name(), err)
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				schemaErr = fmt.Errorf("blocks: embedded schema %s is not valid JSON: %w", e.Name(), err)
				return
			}
			obj, _ := doc.(map[string]any)
			id, _ := obj["$id"].(string)
			if id == "" {
				schemaErr = fmt.Errorf("blocks: embedded schema %s has no $id", e.Name())
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				schemaErr = fmt.Errorf("blocks: embedded schema %s: %w", e.Name(), err)
				return
			}
		}
		if schemaVal, schemaErr = c.Compile(SchemaRichBlock); schemaErr != nil {
			schemaErr = fmt.Errorf("blocks: the RichBlock schema does not compile: %w", schemaErr)
		}
	})
	return schemaVal, schemaErr
}

// EmbeddedSchema returns a copy of one embedded schema by file name (for
// example "rich-block.schema.json"), for the drift test.
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

// schemaViolations validates the JSON form of a block and returns the
// violations (nil when it is valid). The validator's own messages quote the
// offending values, which are untrusted text, so only locations and keywords
// are reported. An error means the schema itself could not be used.
func schemaViolations(doc []byte) ([]Violation, error) {
	sch, err := richBlockSchema()
	if err != nil {
		return nil, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return []Violation{{Rule: RuleNotJSON}}, nil
	}
	verr := sch.Validate(inst)
	if verr == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return []Violation{{Rule: RuleSchema}}, nil
	}
	return leafViolations(ve), nil
}

func leafViolations(ve *jsonschema.ValidationError) []Violation {
	seen := map[string]bool{}
	var out []Violation
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(out) >= maxReportedViolations {
			return
		}
		if len(e.Causes) == 0 {
			p := describeViolation(e)
			if !seen[p] {
				seen[p] = true
				out = append(out, Violation{Rule: RuleSchema, Path: p})
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) == 0 {
		out = []Violation{{Rule: RuleSchema}}
	}
	return out
}

// describeViolation is "<location>: <keyword>" without any instance value.
func describeViolation(e *jsonschema.ValidationError) string {
	var loc strings.Builder
	for _, p := range e.InstanceLocation {
		loc.WriteByte('/')
		loc.WriteString(safeToken(p))
	}
	where := loc.String()
	if where == "" {
		where = "/"
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		names := append([]string(nil), k.Missing...)
		sort.Strings(names)
		for i := range names {
			names[i] = safeToken(names[i])
		}
		return where + ": missing " + strings.Join(names, ",")
	case *kind.AdditionalProperties:
		names := append([]string(nil), k.Properties...)
		sort.Strings(names)
		for i := range names {
			names[i] = safeToken(names[i])
		}
		return where + ": not allowed " + strings.Join(names, ",")
	}
	kw := "schema"
	if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
		kw = kp[len(kp)-1]
	}
	return where + ": " + kw
}

// safeToken keeps a JSON pointer token or a property name printable and short:
// both are text of the block under inspection.
func safeToken(s string) string {
	var sb strings.Builder
	n := 0
	for _, r := range s {
		if n >= 40 {
			sb.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f || r == 0xfffd || r > 0x7e {
			r = '?'
		}
		sb.WriteRune(r)
		n++
	}
	return sb.String()
}
