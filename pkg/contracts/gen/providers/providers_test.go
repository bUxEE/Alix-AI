package providers

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

var providersSchemas = filepath.Join("..", "..", "..", "..", "schemas", "providers")

// roundTrip decodes a document into T and encodes it again.
func roundTrip[T any](data []byte) ([]byte, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// strictDecode decodes with unknown members rejected.
func strictDecode[T any](data []byte) error {
	var v T
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(&v)
}

type rootType struct {
	roundTrip func([]byte) ([]byte, error)
	strict    func([]byte) error
}

func root[T any]() rootType {
	return rootType{roundTrip: roundTrip[T], strict: strictDecode[T]}
}

// roots maps a schema stem to the generated root type of that schema. A schema
// added to schemas/providers without an entry fails TestEveryRootIsRegistered.
var roots = map[string]rootType{
	"billing-notice-ack":  root[BillingNoticeAck](),
	"budget":              root[Budget](),
	"budget-state":        root[BudgetState](),
	"conformance-report":  root[ConformanceReport](),
	"connection-status":   root[ConnectionStatus](),
	"credential-ref":      root[CredentialRef](),
	"data-classification": root[DataClassification](),
	"egress-decision":     root[EgressDecision](),
	"egress-disclosure":   root[EgressDisclosure](),
	"egress-policy":       root[EgressPolicy](),
	"price-table":         root[PriceTable](),
	"provider-error":      root[ProviderError](),
	"provider-profile":    root[ProviderProfile](),
	"request-envelope":    root[RequestEnvelope](),
	"usage-record":        root[UsageRecord](),
}

func exampleFiles(t *testing.T, stem, kind string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(providersSchemas, "examples", stem, kind, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// TestEveryRootIsRegistered fails when a schema is added to schemas/providers
// without a root type in the table above, so the checks below cannot silently
// skip it. shared.schema.json is a definitions library and has no root type.
func TestEveryRootIsRegistered(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(providersSchemas, "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schemas found under %s (%v)", providersSchemas, err)
	}
	for _, f := range files {
		stem := strings.TrimSuffix(filepath.Base(f), ".schema.json")
		if stem == "shared" {
			continue
		}
		if _, ok := roots[stem]; !ok {
			t.Errorf("schemas/providers/%s has no registered root type", stem)
		}
	}
	for stem := range roots {
		if _, err := os.Stat(filepath.Join(providersSchemas, stem+".schema.json")); err != nil {
			t.Errorf("registered root %s has no schema: %v", stem, err)
		}
	}
}

// TestProviderExamplesRoundTrip decodes every valid example into its generated
// root type and encodes it again: the result must be the same JSON document, so
// optional, nullable, const, enum and union members all survive.
func TestProviderExamplesRoundTrip(t *testing.T) {
	total := 0
	for stem, rt := range roots {
		files := exampleFiles(t, stem, "valid")
		if len(files) == 0 {
			t.Errorf("%s has no valid examples", stem)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got, err := rt.roundTrip(data)
			if err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			var a, b any
			if err := json.Unmarshal(data, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &b); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(a, b); diff != "" {
				t.Errorf("%s: round trip changed the document (-original +encoded):\n%s", f, diff)
			}
			total++
		}
	}
	if total < 100 {
		t.Errorf("only %d valid examples were round-tripped", total)
	}
}

// TestUnknownMembersAndValuesFailDecoding: the invalid examples that add a
// member (extra-*, *-member) or use an unknown enum value (unknown-*) must not
// decode. Patterns, bounds and cross-field rules stay with schema validation, so
// the unknown-* examples that are really pattern or cross-field cases are named
// here and skipped.
func TestUnknownMembersAndValuesFailDecoding(t *testing.T) {
	notEnums := map[string]bool{
		"unknown-region":                 true, // a pattern, not an enum
		"unknown-region-literal":         true, // a pattern, not an enum
		"unknown-residency-with-regions": true, // a cross-field rule
	}
	checked := 0
	for stem, rt := range roots {
		for _, f := range exampleFiles(t, stem, "invalid") {
			name := strings.TrimSuffix(filepath.Base(f), ".json")
			wantRejected := strings.HasPrefix(name, "extra-") || strings.HasSuffix(name, "-member") || strings.HasSuffix(name, "-members") ||
				(strings.HasPrefix(name, "unknown-") && !notEnums[name])
			if !wantRejected {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if rt.strict(data) == nil {
				t.Errorf("%s/%s: strict decoding accepted it", stem, name)
			}
			checked++
		}
	}
	if checked < 60 {
		t.Errorf("only %d examples were checked; the naming convention changed?", checked)
	}
}

// TestProviderTypesFailClosed pins the guarantees that matter most for external
// mode: an unknown value never decodes, restricted data and the decision scorer
// have no representation, and a request part cannot carry a path.
func TestProviderTypesFailClosed(t *testing.T) {
	t.Run("egress mode rejects unknown values and null", func(t *testing.T) {
		for _, in := range []string{`"allow_all"`, `null`, `1`, `""`, `"LOCAL_ONLY"`} {
			var m EgressMode
			if err := json.Unmarshal([]byte(in), &m); err == nil {
				t.Errorf("EgressMode accepted %s", in)
			}
		}
		if _, err := json.Marshal(EgressMode("")); err == nil {
			t.Error("an unset EgressMode must not encode")
		}
	})
	t.Run("restricted is a classification but never egress-eligible", func(t *testing.T) {
		var c DataClassification
		if err := json.Unmarshal([]byte(`"restricted"`), &c); err != nil {
			t.Fatalf("restricted must be a valid classification: %v", err)
		}
		var e EgressEligibleClass
		if err := json.Unmarshal([]byte(`"restricted"`), &e); err == nil {
			t.Error("EgressEligibleClass accepted restricted")
		}
		for _, in := range []string{`"credential"`, `"credentials"`, `"raw_database_content"`} {
			if err := json.Unmarshal([]byte(in), &c); err == nil {
				t.Errorf("DataClassification accepted %s: credentials and raw database content are not classes", in)
			}
		}
	})
	t.Run("credentials and raw database content are not sendable categories", func(t *testing.T) {
		for _, in := range []string{`"credential"`, `"raw_database_content"`, `"filesystem_path"`, `"database_handle"`, `"mcp_url"`, `"local_only_field"`} {
			var c SendCategory
			if err := json.Unmarshal([]byte(in), &c); err == nil {
				t.Errorf("SendCategory accepted %s", in)
			}
			var b BlockedCategory
			if err := json.Unmarshal([]byte(in), &b); err != nil {
				t.Errorf("BlockedCategory rejected %s: %v", in, err)
			}
		}
	})
	t.Run("the decision scorer is not an external role", func(t *testing.T) {
		var r Role
		if err := json.Unmarshal([]byte(`"router"`), &r); err == nil {
			t.Error("Role accepted router")
		}
	})
	t.Run("a request part has no path, url or unknown type", func(t *testing.T) {
		var p RequestEnvelopePart
		if err := json.Unmarshal([]byte(`{"type":"video"}`), &p); err == nil {
			t.Error("an unknown part type decoded")
		}
		withPath := `{"type":"text","text":"x","internal":{"category":"user_message","classification":"internal","size_bytes":1},"path":"/etc/passwd"}`
		if err := json.Unmarshal([]byte(withPath), &p); err == nil {
			t.Error("a part with a path member decoded")
		}
		if err := json.Unmarshal([]byte(`{"text":"x"}`), &p); err == nil {
			t.Error("a part without a type decoded")
		}
	})
	t.Run("a provider error kind is a closed set", func(t *testing.T) {
		var k ProviderErrorKind
		if err := json.Unmarshal([]byte(`"exploded"`), &k); err == nil {
			t.Error("ProviderErrorKind accepted an unknown kind")
		}
		if err := json.Unmarshal([]byte(`"connection_lost"`), &k); err != nil {
			t.Errorf("connection_lost rejected: %v", err)
		}
	})
}

// TestEgressVocabulariesMatchRAG: the source egress modes and classifications
// of schemas/rag (M2-01) are the same vocabularies as the ones defined here.
// They are declared separately because schemas/rag predates this area, so this
// test is what keeps them from drifting apart.
func TestEgressVocabulariesMatchRAG(t *testing.T) {
	modes := map[EgressMode]rag.SourceEgress{
		EgressModeLocalOnly:     rag.SourceEgressLocalOnly,
		EgressModeAllowExcerpt:  rag.SourceEgressAllowExcerpt,
		EgressModeAllowMetadata: rag.SourceEgressAllowMetadata,
		EgressModeDeny:          rag.SourceEgressDeny,
	}
	for p, r := range modes {
		if string(p) != string(r) {
			t.Errorf("egress mode %q differs from rag %q", p, r)
		}
	}
	classes := map[DataClassification]rag.SourceClassification{
		DataClassificationPublic:       rag.SourceClassificationPublic,
		DataClassificationInternal:     rag.SourceClassificationInternal,
		DataClassificationConfidential: rag.SourceClassificationConfidential,
		DataClassificationRestricted:   rag.SourceClassificationRestricted,
	}
	for p, r := range classes {
		if string(p) != string(r) {
			t.Errorf("classification %q differs from rag %q", p, r)
		}
	}
}

// TestPropertyNamesAvoidLogRedactor: internal/log redacts by substring of the
// key name, so a property named like a secret would be hidden whenever one of
// these records is logged (and a counter named *_tokens would be too). No
// property of any schema in this area may match.
func TestPropertyNamesAvoidLogRedactor(t *testing.T) {
	banned := []string{"token", "secret", "password", "authorization", "apikey", "cookie", "privatekey", "credential"}
	files, err := filepath.Glob(filepath.Join(providersSchemas, "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schemas found (%v)", err)
	}
	seen := 0
	var walk func(file, ptr string, n any)
	walk = func(file, ptr string, n any) {
		switch v := n.(type) {
		case map[string]any:
			if props, ok := v["properties"].(map[string]any); ok {
				for name := range props {
					seen++
					low := strings.ToLower(name)
					for _, b := range banned {
						if strings.Contains(low, b) {
							t.Errorf("%s%s: property %q contains %q, which the log redactor matches", file, ptr, name, b)
						}
					}
				}
			}
			for k, c := range v {
				walk(file, ptr+"/"+k, c)
			}
		case []any:
			for i, c := range v {
				walk(file, ptr+"/"+strconv.Itoa(i), c)
			}
		}
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		walk(filepath.Base(f), "", doc)
	}
	if seen < 300 {
		t.Errorf("only %d properties were inspected; the walker is not descending", seen)
	}
}
