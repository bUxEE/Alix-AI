package blocks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// schemasRoot is the published schema tree, relative to this package.
const blocksSchemasRoot = "../../schemas"

// blocksReadExamples returns the JSON files of one examples directory, by name.
func blocksReadExamples(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	full := filepath.Join(blocksSchemasRoot, filepath.FromSlash(dir))
	entries, err := os.ReadDir(full)
	if err != nil {
		t.Fatalf("examples directory %s: %v", dir, err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(full, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = raw
	}
	if len(out) == 0 {
		t.Fatalf("examples directory %s holds no example", dir)
	}
	return out
}

func blocksSortedNames(m map[string][]byte) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// blocksDecimalFact is a host-authored decimal fact.
func blocksDecimalFact(field, value string) actions.Fact {
	return actions.Fact{
		Field: common.FieldName("f_" + field),
		Value: actions.CanonicalValue{Value: actions.DecimalValue{Kind: "decimal", Value: value}},
	}
}

func blocksIntegerFact(field string, n int64) actions.Fact {
	return actions.Fact{
		Field: common.FieldName("f_" + field),
		Value: actions.CanonicalValue{Value: actions.IntegerValue{Kind: "integer", Value: n}},
	}
}

// blocksFactsOfChart builds the fact set a host would have stated for the
// numbers of a chart or table document: every number in chart data records and
// every numeric table cell. It is how the tests give a valid example the
// authority it needs.
func blocksFactsOfChart(t *testing.T, raw []byte) *FactSet {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return NewFactSet(nil)
	}
	var facts []actions.Fact
	records, _ := doc["data"].([]any)
	for i, r := range records {
		rec, _ := r.(map[string]any)
		for k, v := range rec {
			if n, ok := v.(json.Number); ok {
				facts = append(facts, blocksDecimalFact(k+string(rune('a'+i%26)), n.String()))
			}
		}
	}
	rows, _ := doc["rows"].([]any)
	for i, row := range rows {
		cells, _ := row.([]any)
		for j, cell := range cells {
			if n, ok := cell.(json.Number); ok {
				facts = append(facts, blocksDecimalFact(fmt.Sprintf("c%d_%d", i, j), n.String()))
			}
		}
	}
	return NewFactSet(facts)
}

// blocksChart builds a typed bar chart from records for a test.
func blocksChart(t *testing.T, doc string) conversation.RichBlock {
	t.Helper()
	var b conversation.RichBlock
	if err := json.Unmarshal([]byte(doc), &b); err != nil {
		t.Fatalf("test chart does not decode: %v", err)
	}
	return b
}

func blocksRuleOf(err error) []Rule {
	var out []Rule
	for _, v := range Violations(err) {
		out = append(out, v.Rule)
	}
	return out
}
