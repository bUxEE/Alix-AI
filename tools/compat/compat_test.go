package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustJSON(t testing.TB, s string) any {
	t.Helper()
	v, err := decodeJSON([]byte(s))
	if err != nil {
		t.Fatalf("bad test JSON: %v\n%s", err, s)
	}
	return v
}

func schemaChanges(t testing.TB, oldS, newS string) Report {
	t.Helper()
	r := Report{Changes: diffSchemaDocs("schema test/x", mustJSON(t, oldS), mustJSON(t, newS))}
	r.sort()
	return r
}

func apiChanges(t testing.TB, oldS, newS string) Report {
	t.Helper()
	r := Report{Changes: diffOpenAPI(mustJSON(t, oldS).(object), mustJSON(t, newS).(object))}
	r.sort()
	return r
}

func wantOnly(t testing.TB, r Report, sev Severity, code string) {
	t.Helper()
	if r.Worst() != sev || !r.Has(code) {
		t.Fatalf("want worst %s with %q, got:\n%s", sev, code, dump(r))
	}
}

func dump(r Report) string {
	var b strings.Builder
	for _, c := range r.Changes {
		b.WriteString("  " + c.String() + "\n")
	}
	if b.Len() == 0 {
		return "  (no change)"
	}
	return b.String()
}

const base = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,
 "required":["id"],"properties":{"id":{"type":"string","maxLength":10},"kind":{"enum":["a","b"]},"note":{"type":"string"}}}`

func TestRemovedFieldIsBreaking(t *testing.T) {
	r := schemaChanges(t, base, `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,
 "required":["id"],"properties":{"id":{"type":"string","maxLength":10},"kind":{"enum":["a","b"]}}}`)
	wantOnly(t, r, Breaking, "removed_field")
	if !strings.Contains(r.Changes[0].Pointer, "/properties/note") {
		t.Fatalf("pointer should name the field: %s", dump(r))
	}
}

func TestNewRequiredFieldIsBreaking(t *testing.T) {
	// a brand-new field that is required, reported once
	r := schemaChanges(t, base, `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,
 "required":["id","owner"],"properties":{"id":{"type":"string","maxLength":10},"kind":{"enum":["a","b"]},"note":{"type":"string"},"owner":{"type":"string"}}}`)
	wantOnly(t, r, Breaking, "new_required_field")
	if len(r.Changes) != 1 {
		t.Fatalf("a new required field must be reported once:\n%s", dump(r))
	}
	// an existing optional field that becomes required
	r = schemaChanges(t, base, strings.Replace(base, `"required":["id"]`, `"required":["id","note"]`, 1))
	wantOnly(t, r, Breaking, "optional_became_required")
	// dropping a guarantee consumers could rely on
	r = schemaChanges(t, base, strings.Replace(base, `"required":["id"]`, `"required":[]`, 1))
	wantOnly(t, r, Breaking, "required_removed")
}

func TestNewOptionalFieldIsAdditive(t *testing.T) {
	r := schemaChanges(t, base, strings.Replace(base, `"note":{"type":"string"}`, `"note":{"type":"string"},"extra":{"type":"integer"}`, 1))
	wantOnly(t, r, Additive, "new_optional_field")
	if len(r.Changes) != 1 || r.Count(Breaking) != 0 {
		t.Fatalf("unexpected:\n%s", dump(r))
	}
}

func TestEnumValueRemovalIsBreaking(t *testing.T) {
	r := schemaChanges(t, base, strings.Replace(base, `["a","b"]`, `["a"]`, 1))
	wantOnly(t, r, Breaking, "enum_value_removed")
	if !strings.Contains(r.Changes[0].Message, `"b"`) {
		t.Fatalf("message should name the value: %s", dump(r))
	}
	// adding a value is additive
	r = schemaChanges(t, base, strings.Replace(base, `["a","b"]`, `["a","b","c"]`, 1))
	wantOnly(t, r, Additive, "enum_value_added")
}

const apiBase = `{"openapi":"3.1.0","info":{"title":"t","version":"1.0.0"},"security":[{"bearerAuth":[]}],
 "paths":{
  "/v1/things":{"get":{"operationId":"listThings","tags":["a"],"summary":"List",
     "parameters":[{"name":"limit","in":"query","schema":{"type":"integer","maximum":100}}],
     "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Thing"}}}},
                  "429":{"description":"slow down"}}},
   "post":{"operationId":"createThing","requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Thing"}}}},
     "responses":{"201":{"description":"created"}}}},
  "/v1/other":{"get":{"operationId":"getOther","responses":{"200":{"description":"ok"}}}}},
 "components":{"schemas":{"Thing":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}},
  "securitySchemes":{"bearerAuth":{"type":"http","scheme":"bearer"}}}}`

func TestOpenAPIPathRemovalIsBreaking(t *testing.T) {
	removed := strings.Replace(apiBase, `,
  "/v1/other":{"get":{"operationId":"getOther","responses":{"200":{"description":"ok"}}}}`, ``, 1)
	if removed == apiBase {
		t.Fatal("test fixture did not change")
	}
	r := apiChanges(t, apiBase, removed)
	wantOnly(t, r, Breaking, "path_removed")
	if !strings.Contains(r.Changes[0].Message, "/v1/other") {
		t.Fatalf("message should name the path: %s", dump(r))
	}
	// the other direction is additive
	r = apiChanges(t, removed, apiBase)
	wantOnly(t, r, Additive, "path_added")
}

func TestOpenAPIOperationRules(t *testing.T) {
	cases := []struct {
		name, from, to string
		sev            Severity
		code           string
	}{
		{"operation removed", `"post":{"operationId":"createThing"`, `"put":{"operationId":"createThing"`, Breaking, "operation_removed"},
		{"operationId renamed", `"operationId":"listThings"`, `"operationId":"listAllThings"`, Breaking, "operation_id_changed"},
		{"optional parameter added", `"parameters":[{"name":"limit"`, `"parameters":[{"name":"cursor","in":"query","schema":{"type":"string"}},{"name":"limit"`, Additive, "new_optional_parameter"},
		{"required parameter added", `"parameters":[{"name":"limit"`, `"parameters":[{"name":"tenant","in":"query","required":true,"schema":{"type":"string"}},{"name":"limit"`, Breaking, "new_required_parameter"},
		{"parameter schema tightened", `"maximum":100`, `"maximum":50`, Breaking, "bound_tightened"},
		{"response removed", `,
                  "429":{"description":"slow down"}`, ``, Breaking, "response_removed"},
		{"extension on a request body", `"requestBody":{"required":true`, `"requestBody":{"required":true,"x-note":"n"`, DocsOnly, "docs_changed"},
		{"component schema field removed", `"properties":{"id":{"type":"string"}}`, `"properties":{}`, Breaking, "removed_field"},
		{"component removed", `"securitySchemes":{"bearerAuth":{"type":"http","scheme":"bearer"}}`, `"securitySchemes":{}`, Breaking, "component_removed"},
		{"summary reworded", `"summary":"List"`, `"summary":"List all"`, DocsOnly, "docs_changed"},
		{"root security dropped", `"security":[{"bearerAuth":[]}],`, ``, Breaking, "security_changed"},
		{"success media type removed", `"responses":{"201":{"description":"created"}}`, `"responses":{"201":{"description":"created","content":{"text/plain":{}}}}`, Additive, "media_type_added"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changed := strings.Replace(apiBase, c.from, c.to, 1)
			if changed == apiBase {
				t.Fatal("test fixture did not change")
			}
			r := apiChanges(t, apiBase, changed)
			if !r.Has(c.code) || r.Worst() < c.sev {
				t.Fatalf("want %s %s, got:\n%s", c.sev, c.code, dump(r))
			}
			if c.sev != Breaking && r.Count(Breaking) != 0 {
				t.Fatalf("must not be breaking:\n%s", dump(r))
			}
		})
	}
	if r := apiChanges(t, apiBase, apiBase); len(r.Changes) != 0 {
		t.Fatalf("identical documents differ:\n%s", dump(r))
	}
}

func TestSchemaClassification(t *testing.T) {
	cases := []struct {
		name, from, to string
		sev            Severity
		code           string
	}{
		{"description only", `"type":"string"}`, `"type":"string","description":"words"}`, DocsOnly, "docs_changed"},
		{"type narrowed", `"id":{"type":"string"`, `"id":{"type":"integer"`, Breaking, "type_narrowed"},
		{"maxLength tightened", `"maxLength":10`, `"maxLength":5`, Breaking, "bound_tightened"},
		{"maxLength loosened", `"maxLength":10`, `"maxLength":20`, Additive, "bound_loosened"},
		{"pattern added", `"maxLength":10`, `"maxLength":10,"pattern":"^a"`, Breaking, "constraint_added"},
		{"closed to open", `"additionalProperties":false`, `"additionalProperties":true`, Additive, "schema_loosened"},
		{"open to closed", `"additionalProperties":false`, `"additionalProperties":false,"unevaluatedProperties":false`, Breaking, "schema_tightened"},
		{"authority marker moved", `"id":{"type":"string"`, `"id":{"x-alixai-authority":"host","type":"string"`, Breaking, "authority_changed"},
		{"ref retargeted", `"kind":{"enum":["a","b"]}`, `"kind":{"$ref":"#/$defs/kind"}`, Breaking, "ref_changed"},
		{"definition removed", `"type":"object","additionalProperties"`, `"$defs":{"k":{"type":"string"}},"type":"object","additionalProperties"`, Additive, "new_definition"},
		{"const changed", `"kind":{"enum":["a","b"]}`, `"kind":{"const":"a"}`, Breaking, "const_added"},
		{"revision annotation", `"type":"object","additionalProperties"`, `"x-schema-revision":2,"type":"object","additionalProperties"`, DocsOnly, "annotation_changed"},
		{"unknown keyword", `"type":"object","additionalProperties"`, `"frobnicate":1,"type":"object","additionalProperties"`, Breaking, "keyword_changed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			changed := strings.Replace(base, c.from, c.to, 1)
			if changed == base {
				t.Fatal("test fixture did not change")
			}
			r := schemaChanges(t, base, changed)
			if !r.Has(c.code) || r.Worst() != c.sev {
				t.Fatalf("want %s %s, got:\n%s", c.sev, c.code, dump(r))
			}
		})
	}
	if r := schemaChanges(t, base, base); len(r.Changes) != 0 {
		t.Fatalf("identical schemas differ:\n%s", dump(r))
	}
}

func TestSchemaBranches(t *testing.T) {
	two := `{"oneOf":[{"properties":{"type":{"const":"a"},"x":{"type":"string"}}},{"properties":{"type":{"const":"b"}}}]}`
	one := `{"oneOf":[{"properties":{"type":{"const":"b"}}}]}`
	three := `{"oneOf":[{"properties":{"type":{"const":"a"},"x":{"type":"string"}}},{"properties":{"type":{"const":"b"}}},{"properties":{"type":{"const":"c"}}}]}`
	wantOnly(t, schemaChanges(t, two, one), Breaking, "branch_removed")
	wantOnly(t, schemaChanges(t, two, three), Additive, "branch_added")
	// a field removed inside one branch is found through the branch identity, wherever the branch sits
	moved := `{"oneOf":[{"properties":{"type":{"const":"b"}}},{"properties":{"type":{"const":"a"}}}]}`
	wantOnly(t, schemaChanges(t, two, moved), Breaking, "removed_field")
	// allOf is the intersection: adding a member tightens
	wantOnly(t, schemaChanges(t, `{"allOf":[{"type":"object"}]}`, `{"allOf":[{"type":"object"},{"required":["id"]}]}`), Breaking, "branch_added")
	// a brand-new anyOf/oneOf is a new constraint: a document lacking both a and b was valid and now is not
	ab := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}}}`
	for _, k := range []string{"anyOf", "oneOf"} {
		r := schemaChanges(t, ab, strings.Replace(ab, `"type":"object",`, `"type":"object","`+k+`":[{"required":["a"]},{"required":["b"]}],`, 1))
		wantOnly(t, r, Breaking, "constraint_added")
		if r.Has("branch_added") {
			t.Fatalf("%s added: branches reported as added to an existing list:\n%s", k, dump(r))
		}
	}
	// if/then/else: a changed condition is breaking, a changed description is not
	cond := `{"if":{"properties":{"k":{"const":"a"}}},"then":{"required":["x"]}}`
	wantOnly(t, schemaChanges(t, cond, strings.Replace(cond, `"a"`, `"b"`, 1)), Breaking, "constraint_changed")
	wantOnly(t, schemaChanges(t, cond, strings.Replace(cond, `"if":{`, `"if":{"description":"d",`, 1)), DocsOnly, "docs_changed")
}

func TestMergeOpenAPIFragments(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("openapi.yaml", "openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\npaths: {}\ncomponents:\n  schemas:\n    Root: {type: string}\nx-alixai-areas:\n  a: {$ref: paths/a.yaml}\n  b: {$ref: paths/b.yaml}\n")
	write("paths/a.yaml", "paths:\n  /v1/a:\n    get:\n      operationId: getA\n      responses:\n        '200':\n          description: ok\n          content:\n            application/json:\n              schema: {$ref: '../openapi.yaml#/components/schemas/Root'}\ncomponents:\n  schemas:\n    A: {$ref: ../../x/y.schema.json}\n")
	write("paths/b.yaml", "paths:\n  /v1/a:\n    post:\n      operationId: postA\n      responses: {'201': {description: ok}}\n")
	doc, err := mergeOpenAPI(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["x-alixai-areas"]; ok {
		t.Fatal("the area index must not survive the merge")
	}
	item := asObject(asObject(doc["paths"])["/v1/a"])
	if item["get"] == nil || item["post"] == nil {
		t.Fatalf("fragments were not merged into one path item: %v", item)
	}
	if got := canon(item["get"]); !strings.Contains(got, `"$ref":"#/components/schemas/Root"`) {
		t.Fatalf("a reference into openapi.yaml must become local: %s", got)
	}
	if got := canon(doc["components"]); !strings.Contains(got, `"$ref":"x/y.schema.json"`) && !strings.Contains(got, `schemas/x/y.schema.json`) {
		t.Fatalf("a schema file reference must be repository-relative: %s", got)
	}
	// the same operation defined twice is an error, never a silent override
	write("paths/b.yaml", "paths:\n  /v1/a:\n    get:\n      operationId: dup\n      responses: {'200': {description: ok}}\n")
	if _, err := mergeOpenAPI(dir); err == nil {
		t.Fatal("a duplicate operation must fail the merge")
	}
}

// miniRepo is a throwaway repository with one schema and one area fragment.
func miniRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("go.mod", "module example.test/mini\n")
	put("schemas/demo/thing.schema.json", base)
	put("schemas/demo/examples/thing/valid/a.json", `{"id":"x"}`)
	put("schemas/api/openapi.yaml", "openapi: 3.1.0\ninfo: {title: t, version: 1.0.0}\nx-alixai-areas:\n  demo: {$ref: paths/demo.yaml}\n")
	put("schemas/api/paths/demo.yaml", "paths:\n  /v1/things:\n    get:\n      operationId: listThings\n      responses: {'200': {description: ok}}\n")
	return dir
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLILifecycle(t *testing.T) {
	repo := miniRepo(t)
	schema := filepath.Join(repo, "schemas", "demo", "thing.schema.json")

	if code, _, errs := runCLI(t, "check", "-root", repo); code != 1 || !strings.Contains(errs, "no frozen contract set") {
		t.Fatalf("a missing freeze must fail, got %d %q", code, errs)
	}
	if code, _, errs := runCLI(t, "freeze", "-root", repo); code != 0 {
		t.Fatalf("freeze: %d %s", code, errs)
	}
	for _, f := range []string{"demo/thing.frozen.json", "openapi.frozen.json"} {
		if _, err := os.Stat(filepath.Join(repo, "schemas", "_frozen", "v1", filepath.FromSlash(f))); err != nil {
			t.Fatalf("frozen file missing: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "schemas", "_frozen", "v1", "demo", "examples")); err == nil {
		t.Fatal("examples must not be frozen")
	}
	if code, out, _ := runCLI(t, "check", "-root", repo); code != 0 || !strings.Contains(out, "0 breaking, 0 additive, 0 docs-only") {
		t.Fatalf("a fresh freeze must be clean, got %d %s", code, out)
	}

	// additive: passes, is reported, and can be frozen
	if err := os.WriteFile(schema, []byte(strings.Replace(base, `"note":{"type":"string"}`, `"note":{"type":"string"},"extra":{"type":"string"}`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "check", "-root", repo)
	if code != 0 || !strings.Contains(out, "ADDITIVE") || !strings.Contains(out, "new_optional_field") {
		t.Fatalf("additive change: %d %s", code, out)
	}
	if code, _, errs := runCLI(t, "freeze", "-root", repo); code != 0 {
		t.Fatalf("freeze of an additive change: %d %s", code, errs)
	}

	// breaking: fails, and freeze refuses and leaves the frozen set alone
	frozenFile := filepath.Join(repo, "schemas", "_frozen", "v1", "demo", "thing.frozen.json")
	before, _ := os.ReadFile(frozenFile)
	if err := os.WriteFile(schema, []byte(strings.Replace(base, `"kind":{"enum":["a","b"]},`, ``, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runCLI(t, "check", "-root", repo)
	if code != 1 || !strings.Contains(out, "BREAKING") || !strings.Contains(out, "removed_field") {
		t.Fatalf("breaking change must fail: %d %s", code, out)
	}
	if code, _, errs := runCLI(t, "freeze", "-root", repo); code != 1 || !strings.Contains(errs, "refusing to freeze") {
		t.Fatalf("freeze must refuse a breaking change: %d %s", code, errs)
	}
	if after, _ := os.ReadFile(frozenFile); !bytes.Equal(before, after) {
		t.Fatal("a refused freeze must not touch the frozen set")
	}

	// removing a whole public schema is breaking too
	if err := os.Remove(schema); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCLI(t, "check", "-root", repo); code != 1 || !strings.Contains(out, "schema_removed") {
		t.Fatalf("a removed schema must fail: %d %s", code, out)
	}
}

func TestCompareFrozenTrees(t *testing.T) {
	repo := miniRepo(t)
	if code, _, errs := runCLI(t, "freeze", "-root", repo); code != 0 {
		t.Fatal(errs)
	}
	oldDir := filepath.Join(t.TempDir(), "v1")
	copyTree(t, filepath.Join(repo, "schemas", "_frozen", "v1"), oldDir)
	frag := filepath.Join(repo, "schemas", "api", "paths", "demo.yaml")
	if err := os.WriteFile(frag, []byte("paths: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// freeze refuses (path removed), so build the new tree by hand from the current state
	cur, err := LoadCurrent(repo)
	if err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(t.TempDir(), "v1")
	if err := WriteFrozen(newDir, cur); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "compare", oldDir, newDir)
	if code != 1 || !strings.Contains(out, "path_removed") {
		t.Fatalf("compare must flag the removed path: %d %s", code, out)
	}
	if code, _, _ := runCLI(t, "compare", oldDir, oldDir); code != 0 {
		t.Fatal("a tree compared with itself must pass")
	}
	if code, _, _ := runCLI(t, "compare", oldDir, filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Fatal("a missing tree must fail, never read as empty")
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEmptyFrozenSetIsRejected(t *testing.T) {
	if _, err := LoadFrozen(t.TempDir()); err == nil {
		t.Fatal("an empty frozen directory must not read as a valid freeze")
	}
}

// TestRepositoryMatchesFrozenV1 is the real gate: the working tree must not break the frozen v1 contracts, and the
// frozen set must cover every schema area and the provider and decision API paths.
func TestRepositoryMatchesFrozenV1(t *testing.T) {
	repo, err := findRepo(".")
	if err != nil {
		t.Skipf("not inside the repository: %v", err)
	}
	frozen, err := LoadFrozen(filepath.Join(repo, "schemas", "_frozen", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := LoadCurrent(repo)
	if err != nil {
		t.Fatal(err)
	}
	if rep := Compare(frozen, current); rep.Worst() == Breaking {
		t.Fatalf("breaking changes against the frozen v1 contracts:\n%s", dump(rep))
	}
	areas := map[string]bool{}
	for k := range frozen.Schemas {
		areas[strings.SplitN(k, "/", 2)[0]] = true
	}
	entries, err := os.ReadDir(filepath.Join(repo, "schemas"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || n == "api" || strings.HasPrefix(n, "_") || strings.HasPrefix(n, ".") {
			continue
		}
		if !areas[n] {
			t.Errorf("schema area %q is not in the frozen v1 set", n)
		}
	}
	for _, area := range []string{"decision", "providers", "bench", "memory"} {
		if !areas[area] {
			t.Errorf("the %s area must be frozen", area)
		}
	}
	paths := asObject(frozen.OpenAPI["paths"])
	for _, p := range []string{"/v1/providers", "/v1/providers/mode", "/v1/decision/status", "/v1/sessions"} {
		if paths[p] == nil {
			t.Errorf("frozen OpenAPI lacks %s", p)
		}
	}
}
