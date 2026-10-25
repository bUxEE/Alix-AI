package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func readSchemaMap(t testing.TB, rel string) map[string]any {
	t.Helper()
	doc, err := readSchemaFile(filepath.Join(repoPath(t, "schemas"), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return doc.(map[string]any)
}

func TestModelProjection(t *testing.T) {
	full := readSchemaMap(t, proposalSchemaFile)
	proj, err := ModelProjection(full)
	if err != nil {
		t.Fatal(err)
	}

	props := proj["properties"].(map[string]any)
	if _, has := props["trace_id"]; has {
		t.Error("trace_id belongs to the core and must not be in the model's schema")
	}
	for _, name := range []string{"capability_id", "fields", "entity_refs", "intent", "uncertainty"} {
		if _, has := props[name]; !has {
			t.Errorf("model property %s was dropped", name)
		}
	}
	for _, r := range proj["required"].([]any) {
		if r == "trace_id" {
			t.Error("trace_id is still required: additionalProperties false would make the schema unsatisfiable")
		}
	}
	if len(proj["required"].([]any)) != 5 {
		t.Errorf("required = %v, want the five model properties", proj["required"])
	}
	if proj["$id"] == full["$id"] {
		t.Error("the projection must not reuse the contract's $id")
	}
	if _, has := proj["allOf"]; !has {
		t.Error("the allOf rules (task needs a capability, out_of_scope has none) must survive")
	}
	if _, has := proj["$defs"]; !has {
		t.Error("$defs must survive")
	}
	if _, has := full["properties"].(map[string]any)["trace_id"]; !has {
		t.Error("the input schema was modified")
	}
	if len(full["required"].([]any)) != 6 {
		t.Error("the input's required list was modified")
	}

	t.Run("every kept property is annotated as model authority", func(t *testing.T) {
		for name, p := range props {
			if p.(map[string]any)[authorityKey] != authorityModel {
				t.Errorf("%s is not model authority", name)
			}
		}
	})

	t.Run("an unannotated property fails closed", func(t *testing.T) {
		bad := map[string]any{
			"properties": map[string]any{
				"a": map[string]any{authorityKey: authorityModel},
				"b": map[string]any{"type": "string"},
			},
		}
		if _, err := ModelProjection(bad); err == nil || !strings.Contains(err.Error(), "no "+authorityKey) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a property with another authority is dropped", func(t *testing.T) {
		in := map[string]any{
			"properties": map[string]any{
				"a": map[string]any{authorityKey: authorityModel},
				"b": map[string]any{authorityKey: "host"},
				"c": map[string]any{authorityKey: "alixai"},
			},
			"required": []any{"a", "b", "c"},
		}
		out, err := ModelProjection(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := out["properties"].(map[string]any); len(got) != 1 || got["a"] == nil {
			t.Errorf("properties = %v", got)
		}
		if got := out["required"].([]any); len(got) != 1 || got[0] != "a" {
			t.Errorf("required = %v", got)
		}
	})

	t.Run("nothing left is an error", func(t *testing.T) {
		in := map[string]any{"properties": map[string]any{"b": map[string]any{authorityKey: "host"}}}
		if _, err := ModelProjection(in); err == nil {
			t.Error("a projection with no model property must be refused")
		}
	})
}

func TestProposalSchemaJudgesModelOutput(t *testing.T) {
	proposal, _, _ := realSchemas(t)
	good := proposalJSON(`"bookings.create"`, "task", `{"party_size":4}`)
	for name, tt := range map[string]struct {
		doc  string
		want bool
	}{
		"a proposal without a trace id":           {good, true},
		"a null capability with out_of_scope":     {proposalJSON(`null`, "out_of_scope", `{}`), true},
		"a cancel turn with no capability":        {proposalJSON(`null`, "cancel", `{}`), true},
		"a task without a capability":             {proposalJSON(`null`, "task", `{}`), false},
		"out_of_scope with a capability":          {proposalJSON(`"a.b"`, "out_of_scope", `{}`), false},
		"out_of_scope with fields":                {proposalJSON(`null`, "out_of_scope", `{"a":1}`), false},
		"a field name that is not snake case":     {proposalJSON(`"a.b"`, "task", `{"PartySize":4}`), false},
		"a nested object value two levels deep":   {proposalJSON(`"a.b"`, "task", `{"a":{"b":{"c":1}}}`), false},
		"a capability id with a wildcard":         {proposalJSON(`"a.*"`, "task", `{}`), false},
		"a capability id with whitespace":         {proposalJSON(`"a b"`, "task", `{}`), false},
		"a trace id written by the model":         {`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"help","uncertainty":{"overall":1},"trace_id":"trc_1"}`, false},
		"an extra key":                            {`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"help","uncertainty":{"overall":1},"note":"x"}`, false},
		"uncertainty outside 0 to 1":              {`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"help","uncertainty":{"overall":1.5}}`, false},
		"the correction intent with a capability": {proposalJSON(`"a.b"`, "correction", `{"x":1}`), true},
	} {
		if got := accepts(t, proposal, tt.doc); got != tt.want {
			t.Errorf("%s: accepted = %v, want %v\n%s", name, got, tt.want, tt.doc)
		}
	}
}

func TestSchemaLoading(t *testing.T) {
	t.Run("the repository's schemas are found from the package directory", func(t *testing.T) {
		dir, err := FindSchemasDir("")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "bench", "result.schema.json")); err != nil {
			t.Error(err)
		}
	})
	t.Run("an explicit directory must hold the bench schemas", func(t *testing.T) {
		if _, err := FindSchemasDir(t.TempDir()); err == nil {
			t.Error("an empty directory is not a schemas directory")
		}
		if d, err := FindSchemasDir(repoPath(t, "schemas")); err != nil || d == "" {
			t.Errorf("explicit schemas dir: %q, %v", d, err)
		}
	})
	t.Run("the loader resolves only AlixAI schema URNs", func(t *testing.T) {
		l := urnLoader{dir: repoPath(t, "schemas")}
		for _, bad := range []string{"https://example.com/x.json", "file:///etc/passwd", "../../etc/passwd", "urn:alixai:schema:common:../x:v1", "urn:other:schema:a:b:v1"} {
			if _, err := l.Load(bad); err == nil {
				t.Errorf("Load(%q) succeeded", bad)
			}
		}
		if _, err := l.Load("urn:alixai:schema:common:ids:v1"); err != nil {
			t.Errorf("the common ids schema must load: %v", err)
		}
		if _, err := l.Load("urn:alixai:schema:common:nope:v1"); err == nil {
			t.Error("a schema that does not exist must not load")
		}
	})
	t.Run("a schema file that declares another id is refused", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "common")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "ids.schema.json"), []byte(`{"$id":"urn:alixai:schema:common:other:v1"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := (urnLoader{dir: dir}).Load("urn:alixai:schema:common:ids:v1"); err == nil {
			t.Error("the id in the file must equal the id asked for")
		}
	})
}

// TestBenchSchemasAreSelfContained: the bench schemas carry no $ref to another
// schema, so a result or a corpus line can be validated without the rest of
// the repository.
func TestBenchSchemasAreSelfContained(t *testing.T) {
	for _, rel := range []string{resultSchemaFile, corpusCaseSchemaFile} {
		b, err := os.ReadFile(filepath.Join(repoPath(t, "schemas"), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "urn:alixai:schema:") && !strings.Contains(string(b), `"$id": "urn:alixai:schema:bench:`) {
			t.Errorf("%s names another schema", rel)
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if ref, ok := x["$ref"].(string); ok && !strings.HasPrefix(ref, "#/") {
					t.Errorf("%s: $ref %q leaves the document", rel, ref)
				}
				for _, e := range x {
					walk(e)
				}
			case []any:
				for _, e := range x {
					walk(e)
				}
			}
		}
		walk(doc)
	}
}

// TestSchemaExamples runs every example of the bench schemas, the way
// tools/schemacheck does, and also holds each valid result to the semantic
// check that `bench check` applies.
func TestSchemaExamples(t *testing.T) {
	_, resultSchema, caseSchema := realSchemas(t)
	root := filepath.Join(repoPath(t, "schemas"), "bench", "examples")
	for _, tt := range []struct {
		name   string
		schema *jsonschema.Schema
	}{{"result", resultSchema}, {"corpus-case", caseSchema}} {
		for _, kind := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join(root, tt.name, kind, "*.json"))
			if err != nil || len(files) == 0 {
				t.Errorf("%s/%s: no examples (%v)", tt.name, kind, err)
				continue
			}
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
				if err != nil {
					t.Errorf("%s: %v", f, err)
					continue
				}
				verr := tt.schema.Validate(inst)
				switch {
				case kind == "valid" && verr != nil:
					t.Errorf("%s is a valid example but is refused: %v", filepath.Base(f), verr)
				case kind == "invalid" && verr == nil:
					t.Errorf("%s is an invalid example but is accepted", filepath.Base(f))
				}
				if kind == "valid" && tt.name == "result" && verr == nil {
					if problems := validateResultBytes(resultSchema, b); len(problems) > 0 {
						t.Errorf("%s passes the schema but not the semantic check: %v", filepath.Base(f), problems)
					}
				}
			}
		}
	}
}
