package apppkg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/buxee/alix-ai/internal/policy"
)

// TestPolicySchemaAndRuntimeParserAgree: the policy schema of the package
// (schemas/application-package/policy.schema.json) carries its own copy of the
// policy/v1 rule grammar, and the runtime parses policies with
// policy.ParseHostPolicy. The loader runs both on every policy file, and a
// document one accepts and the other refuses would make the loader and the
// runtime disagree. Over every example of both schemas the two verdicts must
// be the same, which is what keeps the loader's policy.invalid check a defence
// in depth and not the only line of defence.
func TestPolicySchemaAndRuntimeParserAgree(t *testing.T) {
	roots := []string{
		filepath.Join("..", "..", "schemas", "application-package", "examples", "policy"),
		filepath.Join("..", "..", "schemas", "policy", "examples", "host-policy"),
	}
	checked := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(p) != ".json" {
				return err
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(filepath.Join("..", ".."), p)
			viol, err := validateSchema(kindPolicy, raw)
			if err != nil {
				t.Fatal(err)
			}
			_, perr := policy.ParseHostPolicy(raw)
			schemaOK, parserOK := len(viol) == 0, perr == nil
			if schemaOK != parserOK {
				t.Errorf("%s: the package schema says valid=%v (%v), the runtime parser says valid=%v (%v)", filepath.ToSlash(rel), schemaOK, viol, parserOK, perr)
			}
			checked++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d policy examples were compared; the corpus was not found", checked)
	}
}
