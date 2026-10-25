package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// These tests keep the Go types and schemas/common/*.schema.json in step: the
// schemas are the public contract, the Go types must not drift from them.

var commonDir = filepath.Join("..", "..", "schemas", "common")

// commonCompiler registers every schema in schemas/common by its $id.
func commonCompiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(commonDir, "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schemas found in %s (%v)", commonDir, err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for _, f := range files {
		raw, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(raw)
		raw.Close()
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		id := doc.(map[string]any)["$id"].(string)
		if err := c.AddResource(id, doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return c
}

func mustCompile(t *testing.T, c *jsonschema.Compiler, loc string) *jsonschema.Schema {
	t.Helper()
	s, err := c.Compile(loc)
	if err != nil {
		t.Fatalf("compile %s: %v", loc, err)
	}
	return s
}

// instanceOf round-trips v through JSON so the schema sees what a client sees.
func instanceOf(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestScopeJSONMatchesSchema(t *testing.T) {
	c := commonCompiler(t)
	sch := mustCompile(t, c, "urn:alixai:schema:common:scope:v1")

	if err := sch.Validate(instanceOf(t, validScope())); err != nil {
		t.Fatalf("a valid Go Scope must satisfy scope.schema.json: %v", err)
	}

	// Go and the schema must agree on what is invalid, for every kind of defect.
	defects := map[string]func(*Scope){
		"zero scope":          func(s *Scope) { *s = Scope{} },
		"no deployment":       func(s *Scope) { s.DeploymentID = "" },
		"no application":      func(s *Scope) { s.ApplicationID = "" },
		"no tenant":           func(s *Scope) { s.TenantID = "" },
		"no workspace":        func(s *Scope) { s.WorkspaceID = "" },
		"no principal":        func(s *Scope) { s.PrincipalID = "" },
		"nil roles":           func(s *Scope) { s.Roles = nil },
		"blank role":          func(s *Scope) { s.Roles = []string{""} },
		"nil capabilities":    func(s *Scope) { s.CapabilityScopes = nil },
		"blank capability":    func(s *Scope) { s.CapabilityScopes = []string{""} },
		"no session":          func(s *Scope) { s.SessionID = "" },
		"wildcard tenant":     func(s *Scope) { s.TenantID = "*" },
		"wildcard capability": func(s *Scope) { s.CapabilityScopes = []string{"tasks.*"} },
		"padded workspace":    func(s *Scope) { s.WorkspaceID = " wsp" },
		"long principal":      func(s *Scope) { s.PrincipalID = strings.Repeat("p", MaxScopeValueLen+1) },
	}
	for name, mutate := range defects {
		s := validScope()
		mutate(&s)
		goErr := s.Validate()
		schemaErr := sch.Validate(instanceOf(t, s))
		if goErr == nil || schemaErr == nil {
			t.Errorf("%s: Go Validate error=%v, schema error=%v; both must reject", name, goErr, schemaErr)
		}
	}
}

func TestIdentifierGrammarMatchesSchema(t *testing.T) {
	c := commonCompiler(t)
	ident := mustCompile(t, c, "urn:alixai:schema:common:ids:v1#/$defs/identifier")

	agree := func(label, v string) {
		t.Helper()
		present, ok := scopeValueState(v)
		goValid := present && ok
		schemaValid := ident.Validate(v) == nil
		if goValid != schemaValid {
			t.Errorf("%s %q: Go says valid=%t, schema says valid=%t", label, v, goValid, schemaValid)
		}
	}

	// Every code point of the BMP, alone and embedded: this catches any
	// whitespace or control character Go rejects that the schema pattern does
	// not (and the reverse).
	for r := rune(0); r <= 0xFFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogates are not valid UTF-8
		}
		agree("rune", string(r))
		agree("embedded rune", "a"+string(r)+"b")
	}
	for _, r := range []rune{0x10000, 0x1F44D, 0x10FFFF} {
		agree("astral rune", "a"+string(r)+"b")
	}

	cases := []string{
		"a", "tnt_acme", "auth0|64f0c1", "user@example.com", "tasks.create", "a:b/c-d_e",
		"", " ", "a b", " a", "a ", "*", "a*b", "tasks.*",
		strings.Repeat("a", MaxScopeValueLen), strings.Repeat("a", MaxScopeValueLen+1),
		strings.Repeat(string(rune(0xE9)), MaxScopeValueLen), strings.Repeat(string(rune(0xE9)), MaxScopeValueLen+1),
	}
	for _, v := range cases {
		agree("case", v)
	}
}

func TestErrorSchemaMatchesTaxonomy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(commonDir, "error.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Properties struct {
			Code struct {
				Enum []string `json:"enum"`
			} `json:"code"`
		} `json:"properties"`
		AllOf []struct {
			If struct {
				Properties struct {
					Code struct {
						Enum []string `json:"enum"`
					} `json:"code"`
				} `json:"properties"`
			} `json:"if"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	var codes, retryable []string
	for _, c := range AllErrCodes() {
		codes = append(codes, string(c))
		if c.Retryable() {
			retryable = append(retryable, string(c))
		}
	}
	if diff := cmp.Diff(codes, doc.Properties.Code.Enum); diff != "" {
		t.Errorf("error.schema.json code enum differs from AllErrCodes (-go +schema):\n%s", diff)
	}
	if len(doc.AllOf) != 1 {
		t.Fatalf("error.schema.json: expected one allOf rule tying retryable to code, got %d", len(doc.AllOf))
	}
	if diff := cmp.Diff(retryable, doc.AllOf[0].If.Properties.Code.Enum); diff != "" {
		t.Errorf("error.schema.json retryable codes differ from Retryable() (-go +schema):\n%s", diff)
	}

	// Behaviour, not just text: every code's Error validates, and a flipped
	// retryable flag is rejected.
	sch := mustCompile(t, commonCompiler(t), "urn:alixai:schema:common:error:v1")
	for _, c := range AllErrCodes() {
		e := New(c, "message").WithDetails("k", "v")
		if err := sch.Validate(instanceOf(t, e)); err != nil {
			t.Errorf("%s: a constructed Error must satisfy the schema: %v", c, err)
		}
		flipped := *e
		flipped.Retryable = !flipped.Retryable
		if err := sch.Validate(instanceOf(t, &flipped)); err == nil {
			t.Errorf("%s: a flipped retryable flag must be rejected by the schema", c)
		}
	}
	// The error a failing Scope produces is itself schema-valid.
	if err := sch.Validate(instanceOf(t, (Scope{}).Validate())); err != nil {
		t.Errorf("scope_missing error must satisfy the schema: %v", err)
	}
}

func TestCommonDefsGrammars(t *testing.T) {
	c := commonCompiler(t)
	cases := []struct {
		def     string
		valid   []any
		invalid []any
	}{
		{
			def:     "urn:alixai:schema:common:timestamps:v1#/$defs/timestamp",
			valid:   []any{"2026-10-02T09:30:00Z", "2026-10-02T09:30:00.123456789Z", "2024-02-29T23:59:59Z"},
			invalid: []any{"2026-10-02T09:30:00+02:00", "2026-10-02 09:30:00Z", "2026-10-02", "2026-13-02T09:30:00Z", "2026-02-30T09:30:00Z", "2026-10-02T24:00:00Z", "2026-10-02T09:30:00.1234567890Z", "2026-10-02T09:30:00", 1759397400},
		},
		{
			def:     "urn:alixai:schema:common:timestamps:v1#/$defs/durationMs",
			valid:   []any{0, 1, 86400000},
			invalid: []any{-1, 1.5, "10"},
		},
		{
			def:     "urn:alixai:schema:common:timestamps:v1#/$defs/ttlSeconds",
			valid:   []any{1, 3600},
			invalid: []any{0, -5, 2.5},
		},
		{
			def:     "urn:alixai:schema:common:ids:v1#/$defs/typedId",
			valid:   []any{"ses_01J0000000000000000000000A", "prep_01ARZ3NDEKTSV4RRFFQ69G5FAV", "rcpt_7ZZZZZZZZZZZZZZZZZZZZZZZZZ"},
			invalid: []any{"ses_", "ses_01j0000000000000000000000a", "01J0000000000000000000000A", "SES_01J0000000000000000000000A", "ses_01J0000000000000000000000", "ses_01J0000000000000000000000AB", "ses_01J000000000000000000000IL0"},
		},
		{
			def:     "urn:alixai:schema:common:ids:v1#/$defs/fieldName",
			valid:   []any{"title", "due_date", "a1", strings.Repeat("a", 64)},
			invalid: []any{"", "Title", "due-date", "1st", "due date", "_x", strings.Repeat("a", 65)},
		},
	}
	for _, tc := range cases {
		sch := mustCompile(t, c, tc.def)
		for _, v := range tc.valid {
			if err := sch.Validate(normalise(t, v)); err != nil {
				t.Errorf("%s: %v should be valid: %v", tc.def, v, err)
			}
		}
		for _, v := range tc.invalid {
			if err := sch.Validate(normalise(t, v)); err == nil {
				t.Errorf("%s: %v should be invalid", tc.def, v)
			}
		}
	}
}

// normalise converts Go numbers to json.Number the way the schema library's
// own decoder would.
func normalise(t *testing.T, v any) any { return instanceOf(t, v) }
