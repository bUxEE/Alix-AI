package apppkg

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEmbeddedSchemasMatchPublished: go:embed cannot reach outside the package,
// so the loader holds copies of the published schemas. A copy that differs from
// its original, or a published package schema that is not embedded, would
// make the loader validate against something other than the contract.
func TestEmbeddedSchemasMatchPublished(t *testing.T) {
	published := func(name string) string {
		for _, dir := range []string{"application-package", "common", "rag"} {
			p := filepath.Join("..", "..", "schemas", dir, name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}
	embedded := map[string]bool{}
	for _, name := range EmbeddedSchemaNames() {
		embedded[name] = true
		orig := published(name)
		if orig == "" {
			t.Errorf("embedded %s has no published original", name)
			continue
		}
		want, err := os.ReadFile(orig)
		if err != nil {
			t.Fatal(err)
		}
		got, err := EmbeddedSchema(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("internal/apppkg/schemas/%s differs from %s; copy it", name, filepath.ToSlash(orig))
		}
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "schemas", "application-package"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".schema.json") && !embedded[e.Name()] {
			t.Errorf("published schema %s is not embedded in internal/apppkg/schemas", e.Name())
		}
	}
}

// TestSchemaExampleCorpus runs the loader's own validation over the published
// example corpus: every valid example passes and every invalid one fails.
func TestSchemaExampleCorpus(t *testing.T) {
	kinds := map[string]schemaKind{
		"manifest":              kindManifest,
		"capability":            kindCapability,
		"conversation-contract": kindContract,
		"documentation-index":   kindDocIndex,
		"policy":                kindPolicy,
		"terminology":           kindTerminology,
		"rag-source":            kindRAGSource,
		"package-tests":         kindTests,
	}
	root := filepath.Join("..", "..", "schemas", "application-package", "examples")
	names := make([]string, 0, len(kinds))
	for n := range kinds {
		names = append(names, n)
	}
	sort.Strings(names)
	total := 0
	for _, name := range names {
		for _, verdict := range []string{"valid", "invalid"} {
			files, _ := filepath.Glob(filepath.Join(root, name, verdict, "*.json"))
			if verdict == "valid" && len(files) == 0 {
				t.Errorf("no valid examples of %s", name)
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				viol, err := validateSchema(kinds[name], raw)
				if err != nil {
					t.Fatal(err)
				}
				total++
				switch {
				case verdict == "valid" && len(viol) > 0:
					t.Errorf("%s/%s/%s is rejected: %v", name, verdict, filepath.Base(f), viol)
				case verdict == "invalid" && len(viol) == 0:
					t.Errorf("%s/%s/%s is accepted", name, verdict, filepath.Base(f))
				}
			}
		}
	}
	if total < 60 {
		t.Fatalf("only %d example files were checked; the corpus was not found", total)
	}
}

// TestViolationsDoNotQuoteTheFile: a schema violation names a location and a
// keyword, never the offending value, which came from an untrusted file.
func TestViolationsDoNotQuoteTheFile(t *testing.T) {
	secret := "SUPER-SECRET-VALUE-" + strings.Repeat("x", 50)
	files := valid(t).edit(t, capFind, "audience: user", "audience: "+secret)
	res := mustLoad(t, files.signed(t), loadOptions())
	f, ok := find(res.Findings, "schema.capability")
	if !ok {
		t.Fatalf("expected a schema finding:\n%s", dump(res.Findings))
	}
	if strings.Contains(f.Message, "SUPER-SECRET") {
		t.Fatalf("the finding quotes the file's value: %s", f.Message)
	}
}
