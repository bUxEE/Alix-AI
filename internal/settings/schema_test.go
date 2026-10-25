package settings

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

var publishedDir = filepath.Join("..", "..", "schemas", "settings")

// TestEmbeddedSchemasMatchPublished fails when a copy embedded in this package
// differs from the published schema it stands for: validation at runtime is
// against the published contract, not a drifting copy.
func TestEmbeddedSchemasMatchPublished(t *testing.T) {
	pairs := map[string]string{
		"shared.schema.json":              filepath.Join(publishedDir, "shared.schema.json"),
		"user-settings.schema.json":       filepath.Join(publishedDir, "user-settings.schema.json"),
		"tenant-settings.schema.json":     filepath.Join(publishedDir, "tenant-settings.schema.json"),
		"deployment-settings.schema.json": filepath.Join(publishedDir, "deployment-settings.schema.json"),
		"ids.schema.json":                 filepath.Join("..", "..", "schemas", "common", "ids.schema.json"),
	}
	entries, err := schemaFS.ReadDir("schemas")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(pairs) {
		t.Errorf("%d embedded schemas, %d expected", len(entries), len(pairs))
	}
	for name, published := range pairs {
		embedded, err := schemaFS.ReadFile("schemas/" + name)
		if err != nil {
			t.Errorf("%s is not embedded: %v", name, err)
			continue
		}
		orig, err := os.ReadFile(published)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(embedded, orig) {
			t.Errorf("internal/settings/schemas/%s differs from %s: copy it again", name, published)
		}
	}
	// And the ids the levels declare are the $id of the published files.
	for _, level := range Levels() {
		name := strings.TrimPrefix(level.SchemaID(), "urn:alixai:schema:settings:")
		name = strings.TrimSuffix(name, ":v1") + ".schema.json"
		raw, err := os.ReadFile(filepath.Join(publishedDir, name))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.ID != level.SchemaID() {
			t.Errorf("%s: $id %q, level says %q", name, doc.ID, level.SchemaID())
		}
	}
	if EffectiveSchemaID != "urn:alixai:schema:settings:effective-settings:v1" {
		t.Errorf("EffectiveSchemaID = %q", EffectiveSchemaID)
	}
}

// TestPublishedExamplesBehave runs the published examples through the validation
// the service uses (strict JSON, then the embedded schema): the valid ones pass
// and survive a round trip through the typed document, the invalid ones are refused.
func TestPublishedExamplesBehave(t *testing.T) {
	for _, level := range Levels() {
		stem := string(level) + "-settings"
		valid, _ := filepath.Glob(filepath.Join(publishedDir, "examples", stem, "valid", "*.json"))
		invalid, _ := filepath.Glob(filepath.Join(publishedDir, "examples", stem, "invalid", "*.json"))
		if len(valid) < 2 || len(invalid) < 8 {
			t.Fatalf("%s: %d valid and %d invalid examples", stem, len(valid), len(invalid))
		}
		for _, f := range valid {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkStrictJSON(raw); err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			if err := validateAgainstSchema(level, raw); err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			doc, err := decodeDocument(level, raw)
			if err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			canon, err := doc.canonical()
			if err != nil {
				t.Errorf("%s: %v", f, err)
				continue
			}
			var a, b any
			_ = json.Unmarshal(raw, &a)
			_ = json.Unmarshal(canon, &b)
			if level != LevelUser || !strings.Contains(f, "narrowing") { // sorting reorders lists, which cmp sees as a difference
				if diff := cmp.Diff(normaliseLists(a), normaliseLists(b)); diff != "" {
					t.Errorf("%s: the canonical form changed the document:\n%s", f, diff)
				}
			}
			// A canonical document is a fixed point.
			doc2, err := decodeDocument(level, canon)
			if err != nil {
				t.Fatal(err)
			}
			canon2, _ := doc2.canonical()
			if !bytes.Equal(canon, canon2) {
				t.Errorf("%s: canonical form is not a fixed point:\n%s\n%s", f, canon, canon2)
			}
		}
		for _, f := range invalid {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkStrictJSON(raw); err != nil {
				continue // refused before the schema: fine, a non-object
			}
			if err := validateAgainstSchema(level, raw); err == nil {
				t.Errorf("%s: the invalid example was accepted", f)
			} else if code(err) != contracts.ErrValidationFailed {
				t.Errorf("%s: code %s", f, code(err))
			}
		}
	}
}

// normaliseLists sorts every list of strings so documents compare regardless of the
// order a list was written in.
func normaliseLists(v any) any {
	switch n := v.(type) {
	case map[string]any:
		for k, c := range n {
			n[k] = normaliseLists(c)
		}
		return n
	case []any:
		for i, c := range n {
			n[i] = normaliseLists(c)
		}
		sort.SliceStable(n, func(i, j int) bool { s1, _ := n[i].(string); s2, _ := n[j].(string); return s1 < s2 })
		return n
	}
	return v
}

var (
	effOnce sync.Once
	effSch  *jsonschema.Schema
	effErr  error
)

// validateEffectiveSchema checks the effective settings against the published
// effective-settings schema.
func validateEffectiveSchema(t testing.TB, eff gen.EffectiveSettings) {
	t.Helper()
	effOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		files, _ := filepath.Glob(filepath.Join(publishedDir, "*.schema.json"))
		files = append(files, filepath.Join("..", "..", "schemas", "common", "ids.schema.json"))
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				effErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				effErr = err
				return
			}
			if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
				effErr = err
				return
			}
		}
		effSch, effErr = c.Compile(EffectiveSchemaID)
	})
	if effErr != nil {
		t.Fatalf("effective schema: %v", effErr)
	}
	raw, err := json.Marshal(eff)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := effSch.Validate(inst); err != nil {
		t.Fatalf("the effective settings do not match their schema: %v\n%s", err, raw)
	}
}

func TestStrictJSON(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 8) + `1` + strings.Repeat(`}`, 8)
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"empty object", `{}`, true},
		{"an object", `{"language":"it"}`, true},
		{"surrounding whitespace", " \n{\"language\":\"it\"}\n ", true},
		{"nothing", ``, false},
		{"not JSON", `language=it`, false},
		{"truncated", `{"language":"it"`, false},
		{"an array", `["language"]`, false},
		{"null", `null`, false},
		{"a string", `"language"`, false},
		{"trailing data", `{"language":"it"} {}`, false},
		{"trailing garbage", `{"language":"it"}x`, false},
		{"a duplicate member", `{"language":"it","language":"en"}`, false},
		{"a nested duplicate member", `{"voice":{"enabled":true,"enabled":false}}`, false},
		{"a duplicate in a list of objects", `{"x":[{"a":1,"a":2}]}`, false},
		{"too deep", deep, false},
		{"invalid UTF-8", "{\"language\":\"\xff\"}", false},
		{"too large", `{"language":"` + strings.Repeat("a", MaxDocumentBytes) + `"}`, false},
	}
	for _, tc := range cases {
		err := checkStrictJSON([]byte(tc.raw))
		if tc.ok != (err == nil) {
			t.Errorf("%s: err = %v, want ok=%t", tc.name, err, tc.ok)
		}
		if err != nil && code(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: code %s", tc.name, code(err))
		}
	}
	// The refusals say nothing about the content.
	err := checkStrictJSON([]byte(`{"api_key":"sk-live-SECRET","api_key":"x"}`))
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "api_key") {
		t.Errorf("a duplicate-member refusal repeats the member: %v", err)
	}
}

// Numbers the schema accepts as integers but the typed document can not hold are
// refused, never silently rounded.
func TestSettingsNumbersAreExact(t *testing.T) {
	f := newFixture(t)
	for _, doc := range []string{
		`{"voice":{"speech_rate_pct":1e2}}`,
		`{"voice":{"speech_rate_pct":100.0}}`,
		`{"history":{"retention_days":9007199254740993}}`,
		`{"accessibility":{"text_scale_pct":-100}}`,
	} {
		err := f.tryPut(ada(), LevelUser, 0, doc)
		if err == nil {
			// An integral float is acceptable only if it decoded exactly.
			rec := f.get(ada(), LevelUser)
			if strings.Contains(doc, "e2") || strings.Contains(doc, "100.0") {
				if string(rec.Settings) != `{"voice":{"speech_rate_pct":100}}` {
					t.Errorf("%s was stored as %s", doc, rec.Settings)
				}
				continue
			}
			t.Errorf("%s: accepted", doc)
			continue
		}
		wantCode(t, err, contracts.ErrValidationFailed)
	}
}

// The refusal of a schema failure names members and keywords, never values.
func TestRefusalNamesMembersNotValues(t *testing.T) {
	f := newFixture(t)
	err := f.tryPut(ada(), LevelUser, 0, `{"voice":{"speech_rate_pct":"SECRET-VALUE-77"},"history":{"retention_days":99999,"nope":1}}`)
	wantCode(t, err, contracts.ErrValidationFailed)
	var ce *contracts.Error
	asErr(err, &ce)
	wire, _ := json.Marshal(ce)
	if strings.Contains(string(wire), "SECRET-VALUE-77") || strings.Contains(string(wire), "99999") {
		t.Errorf("the refusal repeats a submitted value: %s", wire)
	}
	if !strings.Contains(string(wire), "history.nope") {
		t.Errorf("the refusal does not name the unknown member: %s", wire)
	}
	// Hostile member names are reduced to identifier characters.
	err = f.tryPut(ada(), LevelUser, 0, "{\"a\\u0000b<script>\":1}")
	wantCode(t, err, contracts.ErrValidationFailed)
	if strings.ContainsAny(err.Error(), "<>\x00") {
		t.Errorf("an unsafe member name reached the message: %q", err.Error())
	}
}
