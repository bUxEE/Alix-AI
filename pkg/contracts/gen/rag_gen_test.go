package gen_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The RAG roots join the round trip of gen_test.go: every valid example of
// every schemas/rag schema decodes into its generated type and encodes back to
// the same document (the registration is here, in a file of its own, so tasks
// adding schemas never edit the same map literal).
func init() {
	for k, fn := range map[string]func([]byte) ([]byte, error){
		"rag/answer-request":        roundTrip[rag.AnswerRequest],
		"rag/answer-result":         roundTrip[rag.AnswerResult],
		"rag/evidence-citation":     roundTrip[rag.EvidenceCitation],
		"rag/evidence-item":         roundTrip[rag.EvidenceItem],
		"rag/evidence-package":      roundTrip[rag.EvidencePackage],
		"rag/grounded-answer":       roundTrip[rag.GroundedAnswer],
		"rag/index-generation":      roundTrip[rag.IndexGeneration],
		"rag/no-supported-evidence": roundTrip[rag.NoSupportedEvidence],
		"rag/search-hit":            roundTrip[rag.SearchHit],
		"rag/search-request":        roundTrip[rag.SearchRequest],
		"rag/search-response":       roundTrip[rag.SearchResponse],
		"rag/source-status":         roundTrip[rag.SourceStatus],
	} {
		roots[k] = fn
	}
}

var internalSourceMetadata = []string{"source_egress", "source_classification"}

// jsonMembers walks a generated type and returns the JSON member names of
// every struct reachable through pointers, slices, arrays and maps, as dotted
// paths. Union values (interface fields) are not followed; the variants of a
// union are passed in explicitly where they matter.
func jsonMembers(typ reflect.Type, path string, seen map[reflect.Type]bool, out *[]string) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		*out = append(*out, path+"."+name)
		jsonMembers(f.Type, path+"."+name, seen, out)
	}
}

func membersNamed(typ reflect.Type, names ...string) []string {
	var all, hit []string
	jsonMembers(typ, typ.Name(), map[reflect.Type]bool{}, &all)
	for _, m := range all {
		for _, n := range names {
			if strings.HasSuffix(m, "."+n) {
				hit = append(hit, m)
			}
		}
	}
	sort.Strings(hit)
	return hit
}

// The seam of the external-provider plan: source_egress and source_classification
// are internal. The types a client sends or receives have no member of that name
// anywhere below them, so no code path can put them on the wire; the internal
// types do carry them, which proves the walk would have seen them.
func TestRAGClientTypesNeverCarryInternalSourceMetadata(t *testing.T) {
	client := []reflect.Type{
		reflect.TypeFor[rag.SearchRequest](),
		reflect.TypeFor[rag.SearchHit](),
		reflect.TypeFor[rag.SearchResponse](),
		reflect.TypeFor[rag.AnswerRequest](),
		reflect.TypeFor[rag.GroundedAnswer](),
		reflect.TypeFor[rag.NoSupportedEvidence](),
		reflect.TypeFor[rag.SourceStatus](),
		reflect.TypeFor[rag.IndexGeneration](),
		reflect.TypeFor[conversation.Citation](),
	}
	for _, typ := range client {
		if hit := membersNamed(typ, internalSourceMetadata...); len(hit) > 0 {
			t.Errorf("client-facing %s exposes internal source metadata: %v", typ.Name(), hit)
		}
	}
	internal := map[reflect.Type]int{
		reflect.TypeFor[rag.EvidenceCitation](): 2,
		reflect.TypeFor[rag.EvidenceItem]():     2,
		reflect.TypeFor[rag.EvidencePackage]():  2, // through its items
	}
	for typ, want := range internal {
		if got := membersNamed(typ, internalSourceMetadata...); len(got) != want {
			t.Errorf("internal %s: found %v, want %d members named %v", typ.Name(), got, want, internalSourceMetadata)
		}
	}
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// normalizeMember reduces one property of the conversation Citation or of the
// EvidenceCitation to a comparable form: documentation and authority are
// dropped, a $ref into the conversation Citation's properties is replaced by
// the property it names, and a $ref into its $defs is written as a local ref.
func normalizeMember(v any, convID string, conv map[string]any, fromRAG bool) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, val := range m {
		switch k {
		case "description", "x-alixai-authority":
			continue
		case "$ref":
			ref := val.(string)
			if prop, ok := strings.CutPrefix(ref, convID+"#/properties/"); ok {
				return normalizeMember(conv["properties"].(map[string]any)[prop], convID, conv, false)
			}
			if def, ok := strings.CutPrefix(ref, convID+"#"); ok && fromRAG {
				val = "#" + def
			}
			out[k] = val
		default:
			out[k] = val
		}
	}
	return out
}

// EvidenceCitation is the client Citation plus the two internal members, and
// nothing else: the same members, the same required set, and for every shared
// member the same definition. A change to the conversation Citation that is not
// mirrored in the internal record fails here.
func TestEvidenceCitationIsTheClientCitationPlusInternalMetadata(t *testing.T) {
	const convID = "urn:alixai:schema:conversation:citation:v1"
	conv := readJSONFile(t, filepath.Join(schemasDir, "conversation", "citation.schema.json"))
	internal := readJSONFile(t, filepath.Join(schemasDir, "rag", "evidence-citation.schema.json"))

	convProps := conv["properties"].(map[string]any)
	intProps := internal["properties"].(map[string]any)

	wantNames := []string{}
	for k := range convProps {
		wantNames = append(wantNames, k)
	}
	wantNames = append(wantNames, internalSourceMetadata...)
	gotNames := []string{}
	for k := range intProps {
		gotNames = append(gotNames, k)
	}
	sort.Strings(wantNames)
	sort.Strings(gotNames)
	if diff := cmp.Diff(wantNames, gotNames); diff != "" {
		t.Fatalf("EvidenceCitation members differ from Citation members plus %v (-want +got):\n%s", internalSourceMetadata, diff)
	}
	if diff := cmp.Diff(conv["required"], internal["required"]); diff != "" {
		t.Errorf("required members differ (-Citation +EvidenceCitation):\n%s", diff)
	}
	if internal["additionalProperties"] != false || conv["additionalProperties"] != false {
		t.Error("both citation schemas must be closed objects")
	}
	for name, cv := range convProps {
		want := normalizeMember(cv, convID, conv, false)
		got := normalizeMember(intProps[name], convID, conv, true)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("member %s is defined differently in the two schemas (-Citation +EvidenceCitation):\n%s", name, diff)
		}
	}
}

// The client Citation rejects the internal members, and an internal record
// without them is a valid client Citation: the projection is exactly "drop the
// two members".
func TestEvidenceCitationProjectsToClientCitationSchema(t *testing.T) {
	compiler := schemaCompiler(t)
	client, err := compiler.Compile("urn:alixai:schema:conversation:citation:v1")
	if err != nil {
		t.Fatal(err)
	}
	files := examples(t, "valid")["rag/evidence-citation"]
	if len(files) == 0 {
		t.Fatal("no valid evidence-citation examples")
	}
	sawMetadata := false
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		hasMetadata := false
		for _, k := range internalSourceMetadata {
			if _, ok := doc[k]; ok {
				hasMetadata = true
			}
		}
		if hasMetadata {
			sawMetadata = true
			full, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err := client.Validate(full); err == nil {
				t.Errorf("%s: the client Citation accepted a record carrying internal source metadata", f)
			}
		}
		for _, k := range internalSourceMetadata {
			delete(doc, k)
		}
		projected, _ := json.Marshal(doc)
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(projected))
		if err := client.Validate(inst); err != nil {
			t.Errorf("%s: the projection is not a valid client Citation: %v", f, err)
		}
	}
	if !sawMetadata {
		t.Fatal("no example carries internal source metadata; the check proved nothing")
	}
}
