package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	_ "github.com/buxee/alix-ai/internal/integrationaudit/probes" // registers the runtime checks
	_ "github.com/buxee/alix-ai/internal/integrationaudit/rag"    // registers the RAG checks
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

const (
	catalogTestSchemasDir = "../../../schemas"
	catalogTestDocPath    = "../../../docs/integration/readiness-requirements.md"
	catalogTestGolden     = "testdata/requirement_ids.golden"
)

func catalogTestLoad(t *testing.T) *Catalog {
	t.Helper()
	c, err := CatalogLoad()
	if err != nil {
		t.Fatalf("CatalogLoad: %v", err)
	}
	return c
}

// The checklist is derived from the audit, so the two must agree in both directions: a check
// no requirement feeds is an audit step readiness never prepares the operator for, and a
// requirement that names an unknown check promises a verification that does not exist.
func TestEveryAuditCheckHasARequirementAndEveryRequirementAnAuditCheck(t *testing.T) {
	c := catalogTestLoad(t)

	registered := map[string]bool{}
	for _, chk := range integrationaudit.Default.Checks() {
		registered[chk.ID()] = true
	}
	for _, chk := range integrationaudit.Builtins() {
		registered[chk.ID()] = true
	}
	// package.authenticity and the other builtins, plus the five runtime checks and the four
	// RAG checks that the blank imports register. A smaller set means a registration vanished
	// and this test would pass by checking less.
	if len(registered) < 18 {
		t.Fatalf("only %d audit checks are registered (%v), want at least 18", len(registered), catalogTestKeys(registered))
	}

	fed := map[string]bool{}
	for _, r := range c.Requirements {
		fed[string(r.CheckID)] = true
		if !registered[string(r.CheckID)] {
			t.Errorf("requirement %s names check %q, which is not an integration audit check (known: %v)", r.ID, r.CheckID, catalogTestKeys(registered))
		}
	}
	for id := range registered {
		if !fed[id] {
			t.Errorf("audit check %q is referenced by no requirement", id)
		}
	}
	if got, want := c.CheckIDs(), catalogTestKeys(fed); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("CheckIDs = %v, want %v", got, want)
	}
}

// Every text exists in English and Italian, identifiers are stable (a committed list pins
// them: a report refers to them, so a rename breaks every stored report), and loading twice
// gives the same catalog.
func TestCatalogIsBilingualAndStable(t *testing.T) {
	c := catalogTestLoad(t)
	again := catalogTestLoad(t)
	if !bytes.Equal(catalogMarshal(t, c.Requirements), catalogMarshal(t, again.Requirements)) || c.SHA256() != again.SHA256() {
		t.Fatal("two loads of the catalog differ")
	}

	cats := map[readiness.Category]int{}
	mandatory := 0
	seen := map[string]bool{}
	for _, r := range c.Requirements {
		cats[r.Category]++
		if r.Level == readiness.LevelMandatory {
			mandatory++
		}
		if seen[string(r.ID)] {
			t.Errorf("requirement %s appears twice", r.ID)
		}
		seen[string(r.ID)] = true

		for name, lt := range map[string]readiness.LocalizedText{
			"title": r.Title, "description": r.Description, "how_satisfied": r.HowSatisfied, "remediation": r.Remediation,
		} {
			en, it := string(lt.En), string(lt.It)
			if strings.TrimSpace(en) == "" || strings.TrimSpace(it) == "" {
				t.Errorf("%s.%s is missing in English or Italian", r.ID, name)
			}
			if en == it {
				t.Errorf("%s.%s: the Italian text is identical to the English text", r.ID, name)
			}
			if en != strings.TrimSpace(en) || it != strings.TrimSpace(it) {
				t.Errorf("%s.%s has leading or trailing space", r.ID, name)
			}
		}
		if len(r.Description.En) < 40 || len(r.Description.It) < 40 {
			t.Errorf("%s: description is too short to say anything", r.ID)
		}
		for _, s := range r.SatisfiedBy {
			if !s.Valid() {
				t.Errorf("%s: invalid satisfied_by %q", r.ID, s)
			}
		}
	}
	for _, cat := range []readiness.Category{
		readiness.CategoryMCP, readiness.CategoryOpenAPI, readiness.CategoryNativeAdapter, readiness.CategoryModelRuntime,
		readiness.CategoryDeterministicData, readiness.CategoryDocumentation, readiness.CategoryConversationContract,
		readiness.CategoryRAGSource, readiness.CategoryScopeAndAuth, readiness.CategoryPermissions, readiness.CategoryDeployment,
	} {
		if cats[cat] == 0 {
			t.Errorf("category %s has no requirement", cat)
		}
	}
	if mandatory == 0 || mandatory == len(c.Requirements) {
		t.Errorf("%d of %d requirements are mandatory: both levels must be used", mandatory, len(c.Requirements))
	}

	golden := catalogTestGoldenText(c)
	if os.Getenv("READINESS_WRITE_DOCS") == "1" {
		catalogTestWrite(t, catalogTestGolden, golden)
		catalogTestWrite(t, catalogTestDocPath, CatalogRenderMarkdown(c))
	}
	want, err := os.ReadFile(catalogTestGolden)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with READINESS_WRITE_DOCS=1)", catalogTestGolden, err)
	}
	if string(want) != golden {
		t.Errorf("the requirement IDs or the catalog version changed. A requirement is never renamed or removed without a new catalog version; when this is intended, bump CatalogVersion and regenerate with READINESS_WRITE_DOCS=1.\nwant:\n%s\ngot:\n%s", want, golden)
	}
}

// The loader fails closed on anything the catalog should never contain.
func TestCatalogLoaderFailsClosed(t *testing.T) {
	c := catalogTestLoad(t)
	var base []map[string]any
	if err := json.Unmarshal(catalogRaw, &base); err != nil {
		t.Fatal(err)
	}
	if len(base) != len(c.Requirements) {
		t.Fatalf("raw has %d entries, loaded %d", len(base), len(c.Requirements))
	}
	mutate := func(fn func(first map[string]any, all []map[string]any) []map[string]any) []byte {
		var cp []map[string]any
		b, _ := json.Marshal(base)
		_ = json.Unmarshal(b, &cp)
		out, _ := json.Marshal(fn(cp[0], cp))
		return out
	}
	cases := map[string][]byte{
		"empty":            []byte(`[]`),
		"not a list":       []byte(`{}`),
		"trailing data":    append(append([]byte(nil), catalogRaw...), []byte(` []`)...),
		"unknown field":    mutate(func(f map[string]any, a []map[string]any) []map[string]any { f["authorised"] = true; return a }),
		"unknown category": mutate(func(f map[string]any, a []map[string]any) []map[string]any { f["category"] = "chat"; return a }),
		"unknown level":    mutate(func(f map[string]any, a []map[string]any) []map[string]any { f["level"] = "optional"; return a }),
		"bad id":           mutate(func(f map[string]any, a []map[string]any) []map[string]any { f["id"] = "Package"; return a }),
		"bad check id":     mutate(func(f map[string]any, a []map[string]any) []map[string]any { f["check_id"] = "authenticity"; return a }),
		"bad version":      mutate(func(f map[string]any, a []map[string]any) []map[string]any { f["schema_version"] = 2; return a }),
		"empty italian": mutate(func(f map[string]any, a []map[string]any) []map[string]any {
			f["title"] = map[string]any{"en": "x", "it": " "}
			return a
		}),
		"missing italian": mutate(func(f map[string]any, a []map[string]any) []map[string]any {
			f["title"] = map[string]any{"en": "x"}
			return a
		}),
		"no satisfied_by": mutate(func(f map[string]any, a []map[string]any) []map[string]any { f["satisfied_by"] = []string{}; return a }),
		"model_unverified": mutate(func(f map[string]any, a []map[string]any) []map[string]any {
			f["satisfied_by"] = []string{"model_unverified"}
			return a
		}),
		"repeated sources": mutate(func(f map[string]any, a []map[string]any) []map[string]any {
			f["satisfied_by"] = []string{"deterministic", "deterministic"}
			return a
		}),
		"duplicate id": mutate(func(f map[string]any, a []map[string]any) []map[string]any { a[1]["id"] = f["id"]; return a }),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := catalogParse(raw); err == nil {
				t.Fatalf("catalogParse accepted it (%d requirements)", len(got.Requirements))
			}
		})
	}
}

// docs/integration/readiness-requirements.md is rendered from the catalog.
func TestCatalogDocIsCurrent(t *testing.T) {
	c := catalogTestLoad(t)
	want := CatalogRenderMarkdown(c)
	if os.Getenv("READINESS_WRITE_DOCS") == "1" {
		catalogTestWrite(t, catalogTestDocPath, want)
	}
	got, err := os.ReadFile(catalogTestDocPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with READINESS_WRITE_DOCS=1)", catalogTestDocPath, err)
	}
	if string(got) != want {
		t.Errorf("%s is out of date with internal/readiness/catalog/requirements.json: regenerate it with READINESS_WRITE_DOCS=1 go test ./internal/readiness/catalog", catalogTestDocPath)
	}
	for _, r := range c.Requirements {
		if !strings.Contains(string(got), "`"+string(r.ID)+"`") {
			t.Errorf("the document does not mention %s", r.ID)
		}
	}
	for _, line := range []string{"never certifies", "audit"} {
		if !strings.Contains(string(got), line) {
			t.Errorf("the document does not say %q", line)
		}
	}
}

// ---------------------------------------------------------------------------
// Schemas

type catalogRefuseLoader struct{}

func (catalogRefuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema reference %q is not one of the loaded schemas", url)
}

var (
	catalogSchemaOnce sync.Once
	catalogSchemaComp *jsonschema.Compiler
	catalogSchemaErr  error
	catalogSchemaMu   sync.Mutex
)

func catalogTestCompiler() (*jsonschema.Compiler, error) {
	catalogSchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(catalogRefuseLoader{})
		for _, area := range []string{"common", "readiness"} {
			files, err := filepath.Glob(filepath.Join(catalogTestSchemasDir, area, "*.schema.json"))
			if err != nil || len(files) == 0 {
				catalogSchemaErr = fmt.Errorf("no schemas under schemas/%s", area)
				return
			}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					catalogSchemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if err != nil {
					catalogSchemaErr = fmt.Errorf("%s: %w", f, err)
					return
				}
				id, _ := doc.(map[string]any)["$id"].(string)
				if id == "" {
					catalogSchemaErr = fmt.Errorf("%s has no $id", f)
					return
				}
				if err := c.AddResource(id, doc); err != nil {
					catalogSchemaErr = fmt.Errorf("%s: %w", f, err)
					return
				}
			}
		}
		catalogSchemaComp = c
	})
	return catalogSchemaComp, catalogSchemaErr
}

// catalogViolations validates raw JSON against a schema and returns the validator's message,
// or "" when it is valid.
func catalogViolations(t *testing.T, urn string, raw []byte) string {
	t.Helper()
	c, err := catalogTestCompiler()
	if err != nil {
		t.Fatalf("schemas: %v", err)
	}
	catalogSchemaMu.Lock()
	sch, err := c.Compile(urn)
	catalogSchemaMu.Unlock()
	if err != nil {
		t.Fatalf("compile %s: %v", urn, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return "not JSON: " + err.Error()
	}
	if err := sch.Validate(inst); err != nil {
		return strings.TrimSpace(err.Error())
	}
	return ""
}

// catalogDecodeStrict decodes with the generated type, rejecting unknown fields.
func catalogDecodeStrict(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

var catalogExampleAreas = map[string]struct {
	urn string
	new func() any
}{
	"finding":     {"urn:alixai:schema:readiness:finding:v1", func() any { return new(readiness.Finding) }},
	"requirement": {"urn:alixai:schema:readiness:requirement:v1", func() any { return new(readiness.Requirement) }},
	"report":      {"urn:alixai:schema:readiness:report:v1", func() any { return new(readiness.Report) }},
	"status-view": {"urn:alixai:schema:readiness:status-view:v1", func() any { return new(readiness.StatusView) }},
}

// Every valid example validates against its schema and decodes into the generated type; every
// invalid example is rejected by the schema itself. The catalog data is itself a list of valid
// requirements.
func TestReadinessSchemaExamples(t *testing.T) {
	for area, spec := range catalogExampleAreas {
		for _, sub := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join(catalogTestSchemasDir, "readiness", "examples", area, sub, "*.json"))
			if err != nil || len(files) < 2 {
				t.Fatalf("schemas/readiness/examples/%s/%s: %d examples (%v), want at least 2", area, sub, len(files), err)
			}
			for _, f := range files {
				name := area + "/" + sub + "/" + filepath.Base(f)
				t.Run(name, func(t *testing.T) {
					raw, err := os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
					msg := catalogViolations(t, spec.urn, raw)
					if sub == "valid" {
						if msg != "" {
							t.Fatalf("should be valid: %s", msg)
						}
						if err := catalogDecodeStrict(raw, spec.new()); err != nil {
							t.Fatalf("valid example does not decode into the generated type: %v", err)
						}
						return
					}
					if msg == "" {
						t.Fatalf("should be rejected by the schema")
					}
				})
			}
		}
	}

	t.Run("catalog entries are valid requirements", func(t *testing.T) {
		var entries []json.RawMessage
		if err := json.Unmarshal(catalogRaw, &entries); err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			t.Fatal("empty catalog")
		}
		for i, e := range entries {
			if msg := catalogViolations(t, catalogExampleAreas["requirement"].urn, e); msg != "" {
				t.Errorf("entry %d does not validate: %s", i, msg)
			}
		}
	})
}

// No readiness property is shaped like an authorisation, a permission grant or an approval, and
// every property carries the authority annotation: alixai or host, never model. (The shared
// authority check lives in tools/schemagen; this is the same rule, held inside the area.)
func TestReadinessSchemasCarryAuthorityAndGrantNothing(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(catalogTestSchemasDir, "readiness", "*.schema.json"))
	if err != nil || len(files) < 5 {
		t.Fatalf("found %d readiness schemas (%v), want at least 5", len(files), err)
	}
	forbidden := regexp.MustCompile(`(?i)(^|_)(authori[sz]ed?|authori[sz]ation|permit|permitted|allow|allowed|grant|grants|granted|approve|approved|approval|certif|certified|certificate|sign|signed|signature|activate|activated|activation|pass|passed|verdict|score)($|_)`)
	total := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var walk func(ptr string, n any)
		walk = func(ptr string, n any) {
			switch v := n.(type) {
			case map[string]any:
				if props, ok := v["properties"].(map[string]any); ok {
					if ap, has := v["additionalProperties"]; !has || ap != false {
						t.Errorf("%s %s: an object with properties must set additionalProperties false", filepath.Base(f), ptr)
					}
					for name, p := range props {
						total++
						pm, _ := p.(map[string]any)
						auth, _ := pm["x-alixai-authority"].(string)
						if auth != "alixai" && auth != "host" {
							t.Errorf("%s %s.%s: x-alixai-authority is %q, want alixai or host", filepath.Base(f), ptr, name, auth)
						}
						if forbidden.MatchString(name) {
							t.Errorf("%s %s.%s: the name is shaped like an authorisation, grant, approval or verdict", filepath.Base(f), ptr, name)
						}
					}
				}
				for k, c := range v {
					if k == "if" || k == "then" || k == "else" || k == "not" {
						continue // constraints, not properties
					}
					walk(ptr+"/"+k, c)
				}
			case []any:
				for i, c := range v {
					walk(fmt.Sprintf("%s/%d", ptr, i), c)
				}
			}
		}
		walk("", doc)
	}
	if total < 60 {
		t.Errorf("only %d properties were checked; the walker is not descending", total)
	}
}

// ---------------------------------------------------------------------------
// Helpers

func catalogTestKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func catalogMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func catalogTestGoldenText(c *Catalog) string {
	var b strings.Builder
	fmt.Fprintf(&b, "catalog_version %d\n", CatalogVersion)
	for _, r := range c.Requirements {
		fmt.Fprintf(&b, "%s %s\n", r.ID, r.CheckID)
	}
	return b.String()
}

func catalogTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
