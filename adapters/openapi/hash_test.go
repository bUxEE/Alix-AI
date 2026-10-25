package openapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/adapters/openapi/testdata/fixture"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var fixtureApproved = []string{
	fixture.OpGet, fixture.OpValidate, fixture.OpPrepare, fixture.OpCommit, fixture.OpStatus, fixture.OpCancel,
	fixture.OpDescribe, fixture.OpHealth, fixture.OpInspect, fixture.OpCreate, fixture.OpCleanup,
}

// The pin of the approved operation subset moves when what an approved operation
// takes or answers changes, and not when text for people or an operation nobody
// maps changes.
func TestContractSHA256(t *testing.T) {
	spec := string(fixture.Spec)
	hash := func(s string, ids ...string) string {
		t.Helper()
		if ids == nil {
			ids = fixtureApproved
		}
		h, err := ContractSHA256([]byte(s), ids)
		if err != nil {
			t.Fatal(err)
		}
		if len(h) != 64 || strings.ToLower(h) != h {
			t.Fatalf("hash = %q", h)
		}
		return h
	}
	replace := func(old, new string) string {
		t.Helper()
		if !strings.Contains(spec, old) {
			t.Fatalf("the fixture document no longer contains %q", old)
		}
		return strings.Replace(spec, old, new, 1)
	}
	base := hash(spec)

	same := map[string]string{
		"a summary":                      replace("summary: Read one item.", "summary: Fetch an item."),
		"a description of a response":    replace("description: The contract versions the application speaks.", "description: Versions."),
		"a description of the result":    replace("description: A DeterministicResult as the host states it", "description: What the host says"),
		"an extension":                   replace("operationId: getItem\n", "operationId: getItem\n      x-internal: true\n"),
		"the document's title":           replace("title: Fixture items API", "title: Renamed"),
		"an operation nobody maps":       replace("summary: Not mapped by any capability. It is in the document and must stay unreachable.", "summary: x\n      parameters:\n        - {name: extra, in: query, schema: {type: string}}"),
		"the servers":                    replace("http://169.254.169.254:1/latest", "http://example.invalid"),
		"a comment":                      "# a comment\n" + spec,
		"the descriptions of a property": replace("preparation: { type: object }", "preparation: { type: object, description: staged action }"),
	}
	for name, s := range same {
		if got := hash(s); got != base {
			t.Errorf("%s moved the hash", name)
		}
	}
	// The order of the ids given is irrelevant.
	if got := hash(spec, reversed(fixtureApproved)...); got != base {
		t.Error("the order of the operationIds moved the hash")
	}
	// The JSON form of the same document has the same hash.
	var generic any
	if err := yaml.Unmarshal(fixture.Spec, &generic); err != nil {
		t.Fatal(err)
	}
	asJSON, err := json.Marshal(stringKeys(generic))
	if err != nil {
		t.Fatal(err)
	}
	if got := hash(string(asJSON)); got != base {
		t.Error("the JSON form of the document hashes differently")
	}

	moved := map[string]string{
		"a parameter's type":     replace("- name: id\n          in: path\n          required: true\n          schema: { type: string }\n        - name: X-AlixAI-Identity", "- name: id\n          in: path\n          required: true\n          schema: { type: integer }\n        - name: X-AlixAI-Identity"),
		"a path":                 replace("/item-drafts/prepare:", "/item-drafts/stage:"),
		"a status":               replace("\"422\": { $ref: \"#/components/responses/Result\" }", "\"400\": { $ref: \"#/components/responses/Result\" }"),
		"a method":               replace("    delete:\n      operationId: cancelItem", "    put:\n      operationId: cancelItem"),
		"a request body":         replace("name: { type: string }", "name: { type: string, maxLength: 64 }"),
		"a referenced schema":    replace("required: [status]\n      properties:\n        status: { type: string, enum:", "required: [status, errors]\n      properties:\n        status: { type: string, enum:"),
		"a referenced component": replace("description: A DeterministicResult as the host states it (operation and capability_id are the core's).\n      content:", "description: x\n      headers:\n        X-Request-Id: { schema: { type: string } }\n      content:"),
		"a required flag":        replace("- name: Idempotency-Key\n          in: header\n          required: true", "- name: Idempotency-Key\n          in: header\n          required: false"),
		"a value in an enum":     replace("enum: [ok, rejected, queued, partial, uncertain, unavailable]", "enum: [ok, rejected, queued, partial, uncertain, unavailable, fine]"),
		"a deprecation flag":     replace("operationId: inspectApplication", "operationId: inspectApplication\n      deprecated: true"),
	}
	for name, s := range moved {
		if got := hash(s); got == base {
			t.Errorf("%s did not move the hash", name)
		}
	}

	t.Run("a property that is named like an annotation is still a property", func(t *testing.T) {
		mk := func(inner string) string {
			return "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /x:\n    get:\n      operationId: op\n      responses:\n        '200':\n          description: ok\n          content:\n            application/json:\n              schema:\n                type: object\n                properties:\n" + inner
		}
		a := mk("                  description: {type: string}\n")
		b := mk("                  description: {type: integer}\n")
		if hash(a, "op") == hash(b, "op") {
			t.Error("a property called description was dropped from the hash")
		}
	})

	t.Run("references that close a cycle", func(t *testing.T) {
		doc := "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /x:\n    get:\n      operationId: op\n      responses:\n        '200':\n          description: ok\n          content:\n            application/json:\n              schema: {$ref: '#/components/schemas/Node'}\ncomponents:\n  schemas:\n    Node:\n      type: object\n      properties:\n        next: {$ref: '#/components/schemas/Node'}\n"
		if hash(doc, "op") != hash(doc, "op") {
			t.Error("the hash of a cyclic document is not stable")
		}
	})

	t.Run("what cannot be hashed", func(t *testing.T) {
		for name, tc := range map[string]struct {
			spec string
			ids  []string
		}{
			"an operationId the document lacks": {spec, []string{"nope"}},
			"an operationId twice":              {spec, []string{fixture.OpGet, fixture.OpGet}},
			"not a document":                    {"just: [text", []string{"x"}},
			"a reference that is not local":     {"openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: op\n      responses:\n        '200': {$ref: 'other.yaml#/Ok'}\n", []string{"op"}},
			"a reference to nothing":            {"openapi: 3.0.3\npaths:\n  /x:\n    get:\n      operationId: op\n      responses:\n        '200': {$ref: '#/components/responses/Nope'}\n", []string{"op"}},
		} {
			if _, err := ContractSHA256([]byte(tc.spec), tc.ids); err == nil {
				t.Errorf("%s: a hash was computed", name)
			}
		}
	})

	t.Run("New checks the pin", func(t *testing.T) {
		h := fixture.Start(t, fake.NewApp())
		cfg := fixtureConfig(t, h)
		cfg.Endpoints[0].ContractSHA256 = base
		a, err := New(cfg)
		if err != nil {
			t.Fatalf("a document that hashes as pinned is refused: %v", err)
		}
		_ = a.Close()

		// The same pin, a document whose approved subset changed.
		cfg = fixtureConfig(t, h)
		cfg.Endpoints[0].ContractSHA256 = base
		cfg.Endpoints[0].Spec = []byte(replace("name: { type: string }", "name: { type: string, maxLength: 64 }"))
		a, err = New(cfg)
		if err == nil {
			_ = a.Close()
			t.Fatal("a document whose approved subset changed is accepted")
		}
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) != ReasonContractMismatch {
			t.Errorf("error = %v, want validation_failed (%s)", err, ReasonContractMismatch)
		}

		// A change to an operation nobody maps does not matter.
		cfg = fixtureConfig(t, h)
		cfg.Endpoints[0].ContractSHA256 = base
		cfg.Endpoints[0].Spec = []byte(replace("Everything.", "Anything."))
		if a, err = New(cfg); err != nil {
			t.Fatalf("a change outside the approved subset is refused: %v", err)
		}
		_ = a.Close()
	})
}

// A schema reachable by many paths is copied on each of them: a document of a
// few kilobytes whose approved operations expand past the bound is refused, not
// expanded until memory runs out.
func TestContractSHA256Bounded(t *testing.T) {
	// S0 has fan properties of type S1, which has fan of type S2, and so on: the
	// last schema, whose properties are leaf, is reached by fan^(levels-1) paths.
	schemas := func(levels, fan int, leaf string) string {
		var b strings.Builder
		for i := 0; i < levels; i++ {
			fmt.Fprintf(&b, "    S%d:\n      type: object\n      properties:\n", i)
			for j := 0; j < fan; j++ {
				if i == levels-1 {
					fmt.Fprintf(&b, "        p%d: %s\n", j, leaf)
				} else {
					fmt.Fprintf(&b, "        p%d: { $ref: '#/components/schemas/S%d' }\n", j, i+1)
				}
			}
		}
		return b.String()
	}
	const fan = 4
	for name, tc := range map[string]struct {
		levels int
		leaf   string
	}{
		"many paths":                  {10, "{ type: string }"},
		"a long string on many paths": {7, "{ type: string, pattern: '" + strings.Repeat("x", 4<<10) + "' }"},
		"a long enum on many paths":   {7, "{ type: string, enum: [" + strings.Repeat("e, ", 149) + "e] }"},
	} {
		doc := "openapi: 3.0.3\ninfo: {title: t, version: '1'}\npaths:\n  /x:\n    post:\n      operationId: op\n      requestBody:\n        content:\n          application/json:\n            schema: {$ref: '#/components/schemas/S0'}\n      responses:\n        '200': {description: ok}\ncomponents:\n  schemas:\n" + schemas(tc.levels, fan, tc.leaf)
		if len(doc) > 20<<10 {
			t.Fatalf("%s: the document is %d bytes, want a small one", name, len(doc))
		}
		if _, err := ContractSHA256([]byte(doc), []string{"op"}); err == nil {
			t.Errorf("%s: a document that expands past the bound was hashed", name)
		}
	}

	t.Run("New refuses it", func(t *testing.T) {
		spec := string(fixture.Spec)
		for old, new := range map[string]string{
			"              name: { type: string }\n": "              name: { $ref: '#/components/schemas/S0' }\n",
			"  schemas:\n":                           "  schemas:\n" + schemas(10, fan, "{ type: string }"),
		} {
			if !strings.Contains(spec, old) {
				t.Fatalf("the fixture document no longer contains %q", old)
			}
			spec = strings.Replace(spec, old, new, 1)
		}
		cfg := fixtureConfig(t, fixture.Start(t, fake.NewApp()))
		cfg.Endpoints[0].Spec = []byte(spec)
		cfg.Endpoints[0].ContractSHA256 = strings.Repeat("0", 64)
		a, err := New(cfg)
		if err == nil {
			_ = a.Close()
			t.Fatal("a document that expands past the bound is accepted")
		}
		if contracts.CodeOf(err) != contracts.ErrValidationFailed || reasonIn(err) == ReasonContractMismatch {
			t.Errorf("error = %v, want validation_failed: the subset cannot be hashed", err)
		}
	})
}

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}
