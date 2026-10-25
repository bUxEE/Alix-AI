package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const widgetSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:alixai:schema:demo:widget:v1",
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {
    "name": { "type": "string", "minLength": 1 }
  }
}`

// tree writes files (slash-separated paths relative to a fresh temp dir) and
// returns the temp dir.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func check(t *testing.T, opts Options, dirs ...string) *Report {
	t.Helper()
	r, err := Check(dirs, opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return r
}

func kindsOf(r *Report) []FindingKind {
	var out []FindingKind
	for _, f := range r.Findings {
		out = append(out, f.Kind)
	}
	return out
}

// runCLI drives run() and returns its exit code and stdout.
func runCLI(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String() + errb.String()
}

func TestSchemacheckDetectsInvalidExamplePassing(t *testing.T) {
	// The "invalid" example is in fact perfectly valid: schemacheck must fail.
	root := tree(t, map[string]string{
		"schemas/demo/widget.schema.json":                          widgetSchema,
		"schemas/demo/examples/widget/valid/ok.json":               `{"name": "gear"}`,
		"schemas/demo/examples/widget/invalid/actually-valid.json": `{"name": "sprocket"}`,
		"schemas/demo/examples/widget/invalid/really-invalid.json": `{"name": ""}`,
	})
	dir := filepath.Join(root, "schemas", "demo")

	r := check(t, Options{}, dir)
	if r.OK() {
		t.Fatal("an invalid example that passes validation must be reported")
	}
	if diff := cmp.Diff([]FindingKind{KindInvalidAccepted}, kindsOf(r)); diff != "" {
		t.Fatalf("findings (-want +got):\n%s", diff)
	}
	if got := r.Findings[0].Path; !strings.HasSuffix(got, "invalid/actually-valid.json") {
		t.Errorf("finding must name the offending file, got %q", got)
	}
	if r.InvalidExamples != 1 || r.ValidExamples != 1 {
		t.Errorf("counts: valid=%d invalid=%d, want 1/1 (the truly invalid one was rejected)", r.ValidExamples, r.InvalidExamples)
	}

	code, out := runCLI(t, dir)
	if code != 1 {
		t.Errorf("exit code = %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"FAIL", "invalid-example-accepted", "actually-valid.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "really-invalid.json") {
		t.Errorf("a correctly rejected example must not be reported:\n%s", out)
	}
}

func TestSchemacheckDetectsValidExampleFailing(t *testing.T) {
	// The "valid" example breaks the schema (wrong type, extra property).
	root := tree(t, map[string]string{
		"schemas/demo/widget.schema.json":                  widgetSchema,
		"schemas/demo/examples/widget/valid/good.json":     `{"name": "gear"}`,
		"schemas/demo/examples/widget/valid/bad-type.json": `{"name": 7}`,
		"schemas/demo/examples/widget/valid/extra.json":    `{"name": "gear", "colour": "red"}`,
		"schemas/demo/examples/widget/invalid/empty.json":  `{"name": ""}`,
	})
	dir := filepath.Join(root, "schemas", "demo")

	r := check(t, Options{}, dir)
	if diff := cmp.Diff([]FindingKind{KindValidRejected, KindValidRejected}, kindsOf(r)); diff != "" {
		t.Fatalf("findings (-want +got):\n%s", diff)
	}
	var paths []string
	for _, f := range r.Findings {
		paths = append(paths, filepath.Base(f.Path))
		if !strings.Contains(f.Detail, "rejects it") {
			t.Errorf("detail should explain the rejection: %q", f.Detail)
		}
	}
	if diff := cmp.Diff([]string{"bad-type.json", "extra.json"}, paths); diff != "" {
		t.Errorf("offending files (-want +got):\n%s", diff)
	}
	if r.ValidExamples != 1 {
		t.Errorf("ValidExamples = %d, want 1", r.ValidExamples)
	}

	code, out := runCLI(t, dir)
	if code != 1 || !strings.Contains(out, "valid-example-rejected") || !strings.Contains(out, "bad-type.json") {
		t.Errorf("exit=%d, report:\n%s", code, out)
	}
	// The reason is in the report (the validator's own message).
	if !strings.Contains(out, "additional properties 'colour' not allowed") {
		t.Errorf("report should carry the validation error:\n%s", out)
	}
}

func TestSchemacheckPassesWhenExamplesBehave(t *testing.T) {
	root := tree(t, map[string]string{
		"schemas/demo/widget.schema.json":                 widgetSchema,
		"schemas/demo/examples/widget/valid/ok.json":      `{"name": "gear"}`,
		"schemas/demo/examples/widget/invalid/empty.json": `{"name": ""}`,
	})
	code, out := runCLI(t, "-v", filepath.Join(root, "schemas", "demo"))
	if code != 0 {
		t.Fatalf("exit code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "ok - 1 schemas, 1 valid and 1 invalid") || !strings.Contains(out, "empty.json") {
		t.Errorf("unexpected report:\n%s", out)
	}
}

func TestSchemacheckUnparseableExampleIsNotAPass(t *testing.T) {
	// A broken-JSON file in invalid/ would "fail validation" trivially; it must
	// be a finding of its own rather than count as a correctly rejected example.
	root := tree(t, map[string]string{
		"schemas/demo/widget.schema.json":                    widgetSchema,
		"schemas/demo/examples/widget/valid/ok.json":         `{"name": "gear"}`,
		"schemas/demo/examples/widget/invalid/broken.json":   `{"name": `,
		"schemas/demo/examples/widget/invalid/trailing.json": `{"name": ""} {"name": ""}`,
	})
	r := check(t, Options{}, filepath.Join(root, "schemas", "demo"))
	if diff := cmp.Diff([]FindingKind{KindExampleUnreadable, KindExampleUnreadable}, kindsOf(r)); diff != "" {
		t.Fatalf("findings (-want +got):\n%s", diff)
	}
	if r.InvalidExamples != 0 {
		t.Errorf("unparseable examples must not count as rejected, got %d", r.InvalidExamples)
	}
}

func TestSchemacheckFormatsAreAsserted(t *testing.T) {
	// "format" is annotation-only in 2020-12 unless asserted; the tool asserts it.
	root := tree(t, map[string]string{
		"schemas/demo/stamp.schema.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:alixai:schema:demo:stamp:v1",
  "type": "string",
  "format": "date-time"
}`,
		"schemas/demo/examples/stamp/valid/ok.json":         `"2026-10-02T09:30:00Z"`,
		"schemas/demo/examples/stamp/invalid/not-date.json": `"2026-13-45T99:99:99Z"`,
	})
	r := check(t, Options{}, filepath.Join(root, "schemas", "demo"))
	if !r.OK() {
		var b bytes.Buffer
		r.Write(&b, false)
		t.Fatalf("format must be asserted:\n%s", b.String())
	}
}

func TestSchemacheckResolvesRefsIntoSiblingAreas(t *testing.T) {
	files := map[string]string{
		"schemas/common/ids.schema.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:alixai:schema:common:ids:v1",
  "$defs": { "slug": { "type": "string", "pattern": "^[a-z]+$" } }
}`,
		"schemas/demo/thing.schema.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:alixai:schema:demo:thing:v1",
  "type": "object",
  "additionalProperties": false,
  "required": ["slug"],
  "properties": { "slug": { "$ref": "urn:alixai:schema:common:ids:v1#/$defs/slug" } }
}`,
		"schemas/demo/examples/thing/valid/ok.json":       `{"slug": "abc"}`,
		"schemas/demo/examples/thing/invalid/digits.json": `{"slug": "a1"}`,
	}
	root := tree(t, files)

	// Only schemas/demo is passed: schemas/common is found through the
	// "schemas" ancestor, and the referenced pattern is really applied.
	r := check(t, Options{}, filepath.Join(root, "schemas", "demo"))
	if !r.OK() || r.ValidExamples != 1 || r.InvalidExamples != 1 {
		var b bytes.Buffer
		r.Write(&b, false)
		t.Fatalf("cross-area ref not resolved/applied:\n%s", b.String())
	}

	// An explicit -root works for layouts without a "schemas" ancestor.
	bare := t.TempDir()
	for rel, content := range files {
		if strings.HasPrefix(rel, "schemas/demo/") {
			p := filepath.Join(bare, "area", strings.TrimPrefix(rel, "schemas/demo/"))
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(content), 0o644)
		}
	}
	r = check(t, Options{}, filepath.Join(bare, "area"))
	if !r.Has(KindSchema) {
		t.Errorf("without a schemas root the ref must fail loudly, got %v", kindsOf(r))
	}
	r = check(t, Options{Roots: []string{filepath.Join(root, "schemas")}}, filepath.Join(bare, "area"))
	// (the area directory is not named "demo", so the id/location convention is not enforced there)
	if !r.OK() {
		var b bytes.Buffer
		r.Write(&b, false)
		t.Fatalf("-root should resolve the ref:\n%s", b.String())
	}
}

func TestSchemacheckRefToMissingSchemaFails(t *testing.T) {
	root := tree(t, map[string]string{
		"schemas/demo/thing.schema.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:alixai:schema:demo:thing:v1",
  "type": "object",
  "properties": { "x": { "$ref": "urn:alixai:schema:common:nope:v1" } }
}`,
	})
	r := check(t, Options{}, filepath.Join(root, "schemas", "demo"))
	if diff := cmp.Diff([]FindingKind{KindSchema}, kindsOf(r)); diff != "" {
		t.Fatalf("findings (-want +got):\n%s", diff)
	}
	if !strings.Contains(r.Findings[0].Detail, "nope") {
		t.Errorf("detail should name the unresolved ref: %q", r.Findings[0].Detail)
	}
	// File and network references are never followed.
	root = tree(t, map[string]string{
		"schemas/demo/thing.schema.json": `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:alixai:schema:demo:thing:v1",
  "properties": { "x": { "$ref": "https://example.com/remote.json" } }
}`,
	})
	if r := check(t, Options{}, filepath.Join(root, "schemas", "demo")); !r.Has(KindSchema) {
		t.Errorf("a remote $ref must be refused, got %v", kindsOf(r))
	}
}

func TestSchemacheckRejectsBadSchemaFiles(t *testing.T) {
	cases := map[string]string{
		"not json":           `{"$id": `,
		"not an object":      `[]`,
		"wrong draft":        `{"$schema": "http://json-schema.org/draft-07/schema#", "$id": "urn:alixai:schema:demo:x:v1"}`,
		"missing id":         `{"$schema": "https://json-schema.org/draft/2020-12/schema"}`,
		"non-urn id":         `{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "https://example.com/x"}`,
		"unversioned id":     `{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:alixai:schema:demo:x"}`,
		"id mismatches path": `{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:alixai:schema:demo:other:v1"}`,
		"does not compile":   `{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:alixai:schema:demo:x:v1", "type": "no-such-type"}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := tree(t, map[string]string{"schemas/demo/x.schema.json": content})
			r := check(t, Options{}, filepath.Join(root, "schemas", "demo"))
			if diff := cmp.Diff([]FindingKind{KindSchema}, kindsOf(r)); diff != "" {
				t.Fatalf("findings (-want +got):\n%s", diff)
			}
			if code, _ := runCLI(t, filepath.Join(root, "schemas", "demo")); code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
		})
	}

	t.Run("duplicate id", func(t *testing.T) {
		dup := `{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:alixai:schema:demo:a:v1"}`
		root := tree(t, map[string]string{
			"x/a.schema.json": dup,
			"x/b.schema.json": dup,
		})
		r := check(t, Options{}, filepath.Join(root, "x"))
		if diff := cmp.Diff([]FindingKind{KindSchema}, kindsOf(r)); diff != "" {
			t.Fatalf("findings (-want +got):\n%s", diff)
		}
		if !strings.Contains(r.Findings[0].Detail, "duplicate $id") {
			t.Errorf("detail = %q", r.Findings[0].Detail)
		}
	})
}

func TestSchemacheckMissingOrEmptyDirFails(t *testing.T) {
	root := t.TempDir()
	if r := check(t, Options{}, filepath.Join(root, "typo")); !r.Has(KindSchema) {
		t.Errorf("a missing directory must fail, got %v", kindsOf(r))
	}
	if r := check(t, Options{}, root); !r.Has(KindSchema) {
		t.Errorf("a directory without schemas must fail, got %v", kindsOf(r))
	}
	if code, _ := runCLI(t, filepath.Join(root, "typo")); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestSchemacheckLayoutViolations(t *testing.T) {
	root := tree(t, map[string]string{
		"schemas/demo/widget.schema.json":                       widgetSchema,
		"schemas/demo/examples/widget/valid/ok.json":            `{"name": "gear"}`,
		"schemas/demo/examples/widget/valid/notes.txt":          `not an example`,
		"schemas/demo/examples/widget/valid/nested/deeper.json": `{"name": "x"}`,
		"schemas/demo/examples/widget/stray.json":               `{"name": "x"}`,
		"schemas/demo/examples/ghost/valid/ok.json":             `{}`,
		"schemas/demo/examples/README.md":                       `hello`,
	})
	r := check(t, Options{}, filepath.Join(root, "schemas", "demo"))
	want := map[string]bool{
		"valid/notes.txt":    true,
		"valid/nested":       true,
		"widget/stray.json":  true,
		"examples/ghost":     true,
		"examples/README.md": true,
	}
	got := map[string]bool{}
	for _, f := range r.Findings {
		if f.Kind != KindLayout {
			t.Errorf("unexpected finding kind %s: %+v", f.Kind, f)
			continue
		}
		for w := range want {
			if strings.HasSuffix(f.Path, w) {
				got[w] = true
			}
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("layout findings (-want +got):\n%s\nall: %+v", diff, r.Findings)
	}
}

func TestSchemacheckRequireExamples(t *testing.T) {
	root := tree(t, map[string]string{"schemas/demo/widget.schema.json": widgetSchema})
	dir := filepath.Join(root, "schemas", "demo")
	if r := check(t, Options{}, dir); !r.OK() {
		t.Errorf("a schema without examples is fine by default, got %v", kindsOf(r))
	}
	if r := check(t, Options{RequireExamples: true}, dir); !r.Has(KindNoExamples) {
		t.Errorf("-require-examples must flag it, got %v", kindsOf(r))
	}
	if code, _ := runCLI(t, "-require-examples", dir); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestSchemacheckUsage(t *testing.T) {
	if code, out := runCLI(t); code != 2 || !strings.Contains(out, "usage: schemacheck") {
		t.Errorf("no args: exit=%d out=%q", code, out)
	}
	if code, _ := runCLI(t, "-nope"); code != 2 {
		t.Errorf("bad flag: exit=%d, want 2", code)
	}
}
