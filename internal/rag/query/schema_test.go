package query

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// compileSchemas compiles every published schema, so a $ref between areas
// resolves; it reads the repository's schemas directory, the contract itself.
func compileSchemas(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	root := filepath.Join("..", "..", "..", "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		obj, _ := doc.(map[string]any)
		id, _ := obj["$id"].(string)
		if id == "" {
			return nil
		}
		return c.AddResource(id, doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The package this pipeline builds is the package the schema defines: a package
// that does not validate would be refused by the answer stage, or worse, trusted
// by one that does not check. It also decodes through the generated fail-closed
// decoder and comes back equal.
func TestPackageMatchesItsSchema(t *testing.T) {
	sch, err := compileSchemas(t).Compile("urn:alixai:schema:rag:evidence-package:v1")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	sc := scopeOf("ta")
	e.addSource(sc, "Warranty handbook", docSpec{id: "warranty", md: warrantyMD()})
	e.addSource(sc, "Unicode notes", docSpec{id: "uni", md: doc("Größe", mdSection("Café 日本語", "Ünïcode paragraph about the warranty in 日本語."))})

	cases := map[string]Deps{"hybrid": e.deps()}
	fts := e.deps()
	fts.Providers = nil
	cases["fts only"] = fts
	for name, d := range cases {
		for _, expand := range []bool{false, true} {
			cfg := DefaultConfig()
			cfg.ParentExpansion = expand
			res := mustQuery(t, e.pipeline(d, cfg), sc, Request{Text: "does the warranty cover water damage"})
			if len(res.Package.Items) == 0 {
				t.Fatalf("%s: empty package", name)
			}
			raw, err := json.Marshal(res.Package)
			if err != nil {
				t.Fatalf("%s: marshal: %v", name, err)
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Errorf("%s (expansion %v): the package does not match its schema: %v\n%s", name, expand, err, raw)
			}
			var back rag.EvidencePackage
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Errorf("%s: the generated decoder refuses the package: %v", name, err)
				continue
			}
			if diff := cmp.Diff(res.Package, back); diff != "" {
				t.Errorf("%s: round trip differs (-built +decoded):\n%s", name, diff)
			}
		}
	}

	t.Run("an empty package is valid", func(t *testing.T) {
		res := mustQuery(t, e.pipeline(e.deps(), DefaultConfig()), scopeOf("nobody"), Request{Text: "warranty"})
		raw, err := json.Marshal(res.Package)
		if err != nil {
			t.Fatal(err)
		}
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err := sch.Validate(inst); err != nil {
			t.Errorf("empty package: %v\n%s", err, raw)
		}
		if len(res.Package.Items) != 0 || res.Package.Budget.Truncated {
			t.Errorf("items %d truncated %v", len(res.Package.Items), res.Package.Budget.Truncated)
		}
	})
}
