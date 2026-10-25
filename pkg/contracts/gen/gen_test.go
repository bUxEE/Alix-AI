package gen_test

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

var schemasDir = filepath.Join("..", "..", "..", "schemas")

// roundTrip decodes a document into T and encodes it again.
func roundTrip[T any](data []byte) ([]byte, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// roots maps "<area>/<schema stem>" to the generated root type of that schema.
// A schema added later without an entry here is logged, not failed: the
// generator output for it is still checked by the drift check.
var roots = map[string]func([]byte) ([]byte, error){
	"common/error": roundTrip[common.Error],
	"common/scope": roundTrip[common.Scope],

	"actions/action-receipt":          roundTrip[actions.ActionReceipt],
	"actions/deterministic-result":    roundTrip[actions.DeterministicResult],
	"actions/prepared-action":         roundTrip[actions.PreparedAction],
	"conversation/citation":           roundTrip[conversation.Citation],
	"conversation/dialogue-directive": roundTrip[conversation.DialogueDirective],
	"conversation/file-ref":           roundTrip[conversation.FileRef],
	"conversation/rich-block":         roundTrip[conversation.RichBlock],
	"conversation/task-proposal":      roundTrip[conversation.TaskProposal],
	"conversation/turn-event":         roundTrip[conversation.TurnEvent],

	"application-package/capability":            roundTrip[applicationpackage.Capability],
	"application-package/conversation-contract": roundTrip[applicationpackage.ConversationContract],
	"application-package/documentation-index":   roundTrip[applicationpackage.DocumentationIndex],
	"application-package/manifest":              roundTrip[applicationpackage.Manifest],
	"application-package/package-tests":         roundTrip[applicationpackage.PackageTests],
	"application-package/policy":                roundTrip[applicationpackage.Policy],
	"application-package/rag-source":            roundTrip[applicationpackage.RAGSource],
	"application-package/signature-manifest":    roundTrip[applicationpackage.SignatureManifest],
	"application-package/terminology":           roundTrip[applicationpackage.Terminology],
}

// schemaCompiler registers every schema under schemas/ by its $id, so $refs
// between files and areas resolve without any loader.
func schemaCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	err := filepath.WalkDir(schemasDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "examples" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".schema.json") {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		doc, err := jsonschema.UnmarshalJSON(f)
		if err != nil {
			return err
		}
		return c.AddResource(doc.(map[string]any)["$id"].(string), doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// examples lists every schemas/<area>/examples/<stem>/<kind>/*.json.
func examples(t *testing.T, kind string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	areas, err := os.ReadDir(schemasDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range areas {
		if !a.IsDir() {
			continue
		}
		stems, _ := os.ReadDir(filepath.Join(schemasDir, a.Name(), "examples"))
		for _, s := range stems {
			files, _ := filepath.Glob(filepath.Join(schemasDir, a.Name(), "examples", s.Name(), kind, "*.json"))
			sort.Strings(files)
			if len(files) > 0 {
				out[a.Name()+"/"+s.Name()] = files
			}
		}
	}
	return out
}

func semanticallyEqual(t *testing.T, a, b []byte) string {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return cmp.Diff(x, y)
}

// TestValidExamplesRoundTrip decodes every valid example of every schema into
// its generated root type and encodes it back: the result must be the same JSON
// document, and the schema must still accept it. That proves optional,
// nullable, required-empty, const, enum and union members all survive, against
// the examples tools/schemacheck already proved valid.
func TestValidExamplesRoundTrip(t *testing.T) {
	ex := examples(t, "valid")
	if len(ex) == 0 {
		t.Fatalf("no valid examples found under %s", schemasDir)
	}
	covered := map[string]bool{}
	compiler := schemaCompiler(t)
	for key, files := range ex {
		fn, ok := roots[key]
		if !ok {
			t.Logf("no generated root type registered for %s (%d examples not round-tripped)", key, len(files))
			continue
		}
		covered[key] = true
		area, stem, _ := strings.Cut(key, "/")
		schema, err := compiler.Compile("urn:alixai:schema:" + area + ":" + stem + ":v1")
		if err != nil {
			t.Fatalf("compile %s: %v", key, err)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := fn(data)
			if err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			if diff := semanticallyEqual(t, data, got); diff != "" {
				t.Errorf("%s: round trip changed the document (-original +encoded):\n%s", f, diff)
			}
			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(got))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(inst); err != nil {
				t.Errorf("%s: the encoded document no longer satisfies %s: %v", f, key, err)
			}
		}
	}
	for key := range roots {
		if !covered[key] {
			t.Errorf("%s has a registered root type but no valid example was found", key)
		}
	}
}

// TestInvalidExamplesRejectedByStrictDecoding shows what the types add on top
// of schema validation: with DisallowUnknownFields, enums, const members and
// union discriminators, many of the schemas' invalid examples fail to decode.
// The types cannot see every defect (patterns, lengths and cross-field rules
// stay with schema validation), so this only requires that the ones they can
// see are rejected: it fails if none are, which would mean strict decoding
// silently stopped working.
func TestInvalidExamplesRejectedByStrictDecoding(t *testing.T) {
	strict := map[string]func([]byte) error{
		"conversation/turn-event":    strictDecode[conversation.TurnEvent],
		"conversation/rich-block":    strictDecode[conversation.RichBlock],
		"actions/prepared-action":    strictDecode[actions.PreparedAction],
		"actions/action-receipt":     strictDecode[actions.ActionReceipt],
		"conversation/task-proposal": strictDecode[conversation.TaskProposal],
	}
	rejected := 0
	for key, files := range examples(t, "invalid") {
		fn, ok := strict[key]
		if !ok {
			continue
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if fn(data) != nil {
				rejected++
			}
		}
	}
	if rejected == 0 {
		t.Fatal("strict decoding rejected none of the invalid examples")
	}
	t.Logf("strict decoding rejected %d invalid examples before schema validation", rejected)
}

func strictDecode[T any](data []byte) error {
	var v T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(&v)
}

// TestGeneratedTypesFailClosed pins the guarantees the generator adds beyond
// encoding/json defaults.
func TestGeneratedTypesFailClosed(t *testing.T) {
	t.Run("zero union cannot be encoded", func(t *testing.T) {
		if _, err := json.Marshal(conversation.RichBlock{}); err == nil {
			t.Fatal("a union with no variant must not encode")
		}
	})
	t.Run("union discriminator comes from the variant", func(t *testing.T) {
		b, err := json.Marshal(conversation.RichBlock{Value: conversation.MarkdownBlock{Text: "hi"}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(b), `{"type":"markdown","text":"hi"}`; got != want {
			t.Fatalf("encoded %s, want %s", got, want)
		}
	})
	t.Run("unknown discriminator is rejected", func(t *testing.T) {
		var b conversation.RichBlock
		if err := json.Unmarshal([]byte(`{"type":"script","text":"x"}`), &b); err == nil {
			t.Fatal("an unknown block type must not decode")
		}
	})
	t.Run("missing discriminator is rejected", func(t *testing.T) {
		var b conversation.RichBlock
		if err := json.Unmarshal([]byte(`{"text":"x"}`), &b); err == nil {
			t.Fatal("a block without a type must not decode")
		}
	})
	t.Run("variant is decoded strictly", func(t *testing.T) {
		var b conversation.RichBlock
		if err := json.Unmarshal([]byte(`{"type":"markdown","text":"x","html":"<b>"}`), &b); err == nil {
			t.Fatal("an unknown member of a variant must not decode")
		}
	})
	t.Run("enum rejects unknown values and null", func(t *testing.T) {
		var c common.ErrorCode
		for _, in := range []string{`"nope"`, `null`, `1`, `""`} {
			if err := json.Unmarshal([]byte(in), &c); err == nil {
				t.Errorf("ErrorCode accepted %s", in)
			}
		}
		if err := json.Unmarshal([]byte(`"stale"`), &c); err != nil || c != common.ErrorCodeStale {
			t.Errorf("ErrorCode(stale) = %q, %v", c, err)
		}
		if _, err := json.Marshal(common.ErrorCode("")); err == nil {
			t.Error("an unset enum must not encode")
		}
	})
	t.Run("integer enum rejects out of range and null", func(t *testing.T) {
		var r actions.RiskLevel
		for _, in := range []string{`4`, `-1`, `null`, `"2"`, `1.5`} {
			if err := json.Unmarshal([]byte(in), &r); err == nil {
				t.Errorf("RiskLevel accepted %s", in)
			}
		}
		if err := json.Unmarshal([]byte(`3`), &r); err != nil || r != actions.RiskLevel3 {
			t.Errorf("RiskLevel(3) = %v, %v", r, err)
		}
		if _, err := json.Marshal(actions.RiskLevel(9)); err == nil {
			t.Error("an out-of-range RiskLevel must not encode")
		}
	})
	t.Run("required arrays encode as arrays, never null", func(t *testing.T) {
		b, err := json.Marshal(conversation.TaskProposal{
			Intent:      conversation.TaskProposalIntentHelp,
			Uncertainty: conversation.TaskProposalUncertainty{Overall: 1},
			TraceID:     "trace-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if string(m["entity_refs"]) != "[]" || string(m["fields"]) != "{}" {
			t.Errorf("required collections encoded as %s and %s, want [] and {}", m["entity_refs"], m["fields"])
		}
		if string(m["capability_id"]) != "null" {
			t.Errorf("required nullable capability_id encoded as %s, want null", m["capability_id"])
		}
	})
	t.Run("optional zero values are kept, absent ones omitted", func(t *testing.T) {
		zero := int64(0)
		b, err := json.Marshal(conversation.ProgressEvent{
			TurnID: "trn_01J0000000000000000000000A", Seq: 0,
			Stage: conversation.ProgressEventStageGenerating, State: conversation.ProgressEventStateStarted,
			Percent: &zero,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"percent":0`) || strings.Contains(string(b), "job_id") {
			t.Errorf("encoded %s", b)
		}
	})
}

// TestGeneratedPackagesImportOnlyStdlibAndSiblings keeps the generated
// packages usable from pkg/contracts: they may import the standard library and
// each other, never pkg/contracts itself (an import cycle with aliases.go) and
// nothing from internal/ or third parties.
func TestGeneratedPackagesImportOnlyStdlibAndSiblings(t *testing.T) {
	const genPrefix = "github.com/buxee/alix-ai/pkg/contracts/gen/"
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			stdlib := !strings.Contains(strings.SplitN(path, "/", 2)[0], ".")
			if !stdlib && !strings.HasPrefix(path, genPrefix) {
				t.Errorf("%s imports %s: generated packages may import only the standard library and each other", p, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no Go files found")
	}
}
