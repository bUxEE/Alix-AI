package apppkg

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

// schemaFS holds byte-identical copies of the published schemas the loader
// validates against (go:embed cannot reach outside the package). The package
// schemas of schemas/application-package and the common ones they refer to
// are all here, so validation needs no file system and no network, and a
// reference that is not embedded fails closed. TestEmbeddedSchemasMatchPublished
// fails when a copy and its original differ, so a schema change that is not
// copied here cannot go unnoticed.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

// schemaIDPrefix is the $id prefix of the package schemas.
const schemaIDPrefix = "urn:alixai:schema:application-package:"

// schemaKind is one structured file kind of a package and the schema it is
// validated against.
type schemaKind struct {
	name  string
	check *check
}

func (k schemaKind) id() string { return schemaIDPrefix + k.name + ":v1" }

var (
	kindManifest    = schemaKind{"manifest", chkSchemaManifest}
	kindCapability  = schemaKind{"capability", chkSchemaCapability}
	kindContract    = schemaKind{"conversation-contract", chkSchemaContract}
	kindDocIndex    = schemaKind{"documentation-index", chkSchemaDocIndex}
	kindPolicy      = schemaKind{"policy", chkSchemaPolicy}
	kindTerminology = schemaKind{"terminology", chkSchemaTerminology}
	kindRAGSource   = schemaKind{"rag-source", chkSchemaRAG}
	kindTests       = schemaKind{"package-tests", chkSchemaTests}
)

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

// refuseLoader makes every reference to a resource that was not embedded an
// error: validation never reads a file or a URL.
type refuseLoader struct{}

func (refuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema reference %q is not an embedded schema", url)
}

// compiledSchemas compiles the embedded schemas once, as JSON Schema 2020-12
// with format assertions on, so that a malformed URI is rejected, not ignored.
func compiledSchemas() (map[string]*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(refuseLoader{})
		entries, err := fs.ReadDir(schemaFS, "schemas")
		if err != nil {
			compileErr = fmt.Errorf("apppkg: embedded schemas: %w", err)
			return
		}
		for _, e := range entries {
			raw, err := schemaFS.ReadFile("schemas/" + e.Name())
			if err != nil {
				compileErr = fmt.Errorf("apppkg: embedded schema %s: %w", e.Name(), err)
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				compileErr = fmt.Errorf("apppkg: embedded schema %s is not valid JSON: %w", e.Name(), err)
				return
			}
			obj, _ := doc.(map[string]any)
			id, _ := obj["$id"].(string)
			if id == "" {
				compileErr = fmt.Errorf("apppkg: embedded schema %s has no $id", e.Name())
				return
			}
			if err := c.AddResource(id, doc); err != nil {
				compileErr = fmt.Errorf("apppkg: embedded schema %s: %w", e.Name(), err)
				return
			}
		}
		out := map[string]*jsonschema.Schema{}
		for _, k := range []schemaKind{kindManifest, kindCapability, kindContract, kindDocIndex, kindPolicy, kindTerminology, kindRAGSource, kindTests} {
			sch, err := c.Compile(k.id())
			if err != nil {
				compileErr = fmt.Errorf("apppkg: schema %s does not compile: %w", k.id(), err)
				return
			}
			out[k.name] = sch
		}
		compiled = out
	})
	return compiled, compileErr
}

// EmbeddedSchema returns a copy of one embedded schema by file name (for
// example "manifest.schema.json"), for the drift test.
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

// maxViolations bounds how many schema violations one finding names.
const maxViolations = 5

// validateSchema validates the JSON form of a file against the schema of its
// kind. It returns the violations (empty when the document is valid) as short
// strings of location and keyword. The validator's own messages quote the
// offending values, which came from an untrusted file, so only locations,
// keywords and schema-defined property names are reported.
func validateSchema(k schemaKind, doc []byte) (violations []string, err error) {
	set, err := compiledSchemas()
	if err != nil {
		return nil, err
	}
	sch := set[k.name]
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return []string{"(root): not valid JSON"}, nil
	}
	verr := sch.Validate(inst)
	if verr == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return []string{"(root): does not satisfy the schema"}, nil
	}
	return leafViolations(ve), nil
}

func leafViolations(ve *jsonschema.ValidationError) []string {
	seen := map[string]bool{}
	var out []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(out) >= maxViolations {
			return
		}
		if len(e.Causes) == 0 {
			s := describeViolation(e)
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) == 0 {
		out = []string{"(root): does not satisfy the schema"}
	}
	return out
}

func describeViolation(e *jsonschema.ValidationError) string {
	loc := "(root)"
	if len(e.InstanceLocation) > 0 {
		var sb strings.Builder
		for _, p := range e.InstanceLocation {
			sb.WriteByte('/')
			sb.WriteString(sanitizeToken(p))
		}
		loc = sb.String()
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		names := append([]string(nil), k.Missing...)
		sort.Strings(names)
		for i := range names {
			names[i] = sanitizeToken(names[i])
		}
		return fmt.Sprintf("%s: missing required property %s", loc, strings.Join(names, ", "))
	case *kind.AdditionalProperties:
		names := append([]string(nil), k.Properties...)
		sort.Strings(names)
		for i := range names {
			names[i] = sanitizeToken(names[i])
		}
		return fmt.Sprintf("%s: property not allowed: %s", loc, strings.Join(names, ", "))
	}
	kw := "schema"
	if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
		kw = kp[len(kp)-1]
	}
	return fmt.Sprintf("%s: fails %s", loc, kw)
}

// sanitizeToken keeps a JSON pointer token or property name printable and
// short: both are text of the file under inspection.
func sanitizeToken(s string) string {
	var sb strings.Builder
	n := 0
	for _, r := range s {
		if n >= 48 {
			sb.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f || r == 0xfffd {
			r = '?'
		}
		sb.WriteRune(r)
		n++
	}
	return sb.String()
}
