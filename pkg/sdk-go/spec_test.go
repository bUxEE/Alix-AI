package sdkgo_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// This file reads schemas/api/openapi.yaml and its fragments the way the Go gateway's own contract test does
// (a root that references one fragment per area) and turns the response and request schemas into validators,
// so the SDK's operation table and the fake server's answers are checked against the real contract.

const (
	apiDir       = "../../schemas/api"
	schemasDir   = "../../schemas"
	specResource = "urn:alixai:test:openapi"
)

type node = map[string]any

type specOp struct {
	id            string
	method        string
	path          string
	area          string
	authenticated bool
	op            node
}

type spec struct {
	ops       []specOp
	responses node // the merged components.responses
	compiler  *jsonschema.Compiler
	cache     map[string]*jsonschema.Schema
}

func readYAML(t testing.TB, file string) node {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	var doc node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	if doc == nil {
		doc = node{}
	}
	return doc
}

func asNode(v any) node {
	if m, ok := v.(node); ok {
		return m
	}
	return node{}
}

func toJSONDoc(t testing.TB, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// schemaIDs maps every JSON Schema file of the repository (but the API's own) to its $id.
func schemaIDs(t testing.TB) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(schemasDir, "*", "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schema files under %s: %v", schemasDir, err)
	}
	ids := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || doc.ID == "" {
			t.Fatalf("%s has no $id: %v", f, err)
		}
		abs, _ := filepath.Abs(f)
		ids[abs] = doc.ID
	}
	return ids
}

// rewriteRefs points every $ref of a document at something the validator universe resolves.
func rewriteRefs(t testing.TB, v any, dir string, ids map[string]string) any {
	t.Helper()
	switch n := v.(type) {
	case node:
		out := node{}
		for k, item := range n {
			ref, isRef := item.(string)
			if k != "$ref" || !isRef {
				out[k] = rewriteRefs(t, item, dir, ids)
				continue
			}
			file, pointer, _ := strings.Cut(ref, "#")
			if pointer != "" {
				pointer = "#" + pointer
			}
			switch {
			case file == "" || strings.HasSuffix(file, "openapi.yaml"):
				out[k] = specResource + pointer
			default:
				abs, _ := filepath.Abs(filepath.Join(dir, filepath.FromSlash(file)))
				id, ok := ids[abs]
				if !ok {
					t.Fatalf("unresolvable $ref %q in %s", ref, dir)
				}
				out[k] = id + pointer
			}
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, item := range n {
			out[i] = rewriteRefs(t, item, dir, ids)
		}
		return out
	}
	return v
}

var httpMethods = []string{"get", "post", "put", "patch", "delete"}

func loadSpec(t testing.TB) *spec {
	t.Helper()
	ids := schemaIDs(t)
	root := readYAML(t, filepath.Join(apiDir, "openapi.yaml"))
	s := &spec{responses: node{}, cache: map[string]*jsonschema.Schema{}}
	schemas := node{}

	collect := func(area string, doc node, dir string) {
		components := asNode(doc["components"])
		for name, def := range asNode(components["schemas"]) {
			schemas[name] = rewriteRefs(t, def, dir, ids)
		}
		for name, def := range asNode(components["responses"]) {
			s.responses[name] = rewriteRefs(t, def, dir, ids)
		}
		for path, item := range asNode(doc["paths"]) {
			for _, method := range httpMethods {
				op, ok := asNode(item)[method].(node)
				if !ok {
					continue
				}
				security := root["security"]
				if own, has := op["security"]; has {
					security = own
				}
				list, _ := security.([]any)
				s.ops = append(s.ops, specOp{
					id: fmt.Sprint(op["operationId"]), method: strings.ToUpper(method), path: path, area: area,
					authenticated: len(list) > 0, op: asNode(rewriteRefs(t, op, dir, ids)),
				})
			}
		}
	}
	collect("root", root, apiDir)
	areas := asNode(root["x-alixai-areas"])
	names := make([]string, 0, len(areas))
	for name := range areas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := filepath.Join(apiDir, fmt.Sprint(asNode(areas[name])["$ref"]))
		collect(name, readYAML(t, file), filepath.Dir(file))
	}

	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for abs, id := range ids {
		raw, err := os.ReadFile(abs)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.AddResource(id, doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.AddResource(specResource, toJSONDoc(t, node{"components": node{"schemas": schemas}})); err != nil {
		t.Fatal(err)
	}
	s.compiler = c
	return s
}

func (s *spec) find(id string) (specOp, bool) {
	for _, o := range s.ops {
		if o.id == id {
			return o, true
		}
	}
	return specOp{}, false
}

// declaredStatuses are the statuses an operation declares.
func (s *spec) declaredStatuses(id string) []int {
	o, _ := s.find(id)
	var out []int
	for code := range asNode(o.op["responses"]) {
		if n, err := strconv.Atoi(code); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// compile compiles a schema node under a unique name.
func (s *spec) compile(key string, schema any, t testing.TB) *jsonschema.Schema {
	if sch, ok := s.cache[key]; ok {
		return sch
	}
	url := "urn:alixai:test:" + key
	if err := s.compiler.AddResource(url, toJSONDoc(t, schema)); err != nil {
		t.Fatal(err)
	}
	sch, err := s.compiler.Compile(url)
	if err != nil {
		t.Fatalf("compile %s: %v", key, err)
	}
	s.cache[key] = sch
	return sch
}

// jsonSchemaOf returns the application/json schema of a content map, or nil.
func jsonSchemaOf(content any) any {
	return asNode(asNode(content)["application/json"])["schema"]
}

// responseSchema returns the JSON schema a response of the operation declares, or nil when it has none.
func (s *spec) responseSchema(id string, status int, t testing.TB) *jsonschema.Schema {
	o, ok := s.find(id)
	if !ok {
		return nil
	}
	resp := asNode(asNode(o.op["responses"])[strconv.Itoa(status)])
	if ref, ok := resp["$ref"].(string); ok {
		name := ref[strings.LastIndex(ref, "/")+1:]
		resp = asNode(s.responses[name])
	}
	schema := jsonSchemaOf(resp["content"])
	if schema == nil {
		return nil
	}
	return s.compile(fmt.Sprintf("response:%s:%d", id, status), schema, t)
}

// requestSchema returns the JSON schema of an operation's request body, or nil.
func (s *spec) requestSchema(id string, t testing.TB) *jsonschema.Schema {
	o, ok := s.find(id)
	if !ok {
		return nil
	}
	schema := jsonSchemaOf(asNode(o.op["requestBody"])["content"])
	if schema == nil {
		return nil
	}
	return s.compile("request:"+id, schema, t)
}

// turnEventSchema is the schema of one TurnEvent (what the `data` of each SSE frame validates against).
func (s *spec) turnEventSchema(t testing.TB) *jsonschema.Schema {
	return s.compile("turn-event", node{"$ref": specResource + "#/components/schemas/TurnEvent"}, t)
}

func validateJSON(sch *jsonschema.Schema, body []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return sch.Validate(inst)
}
