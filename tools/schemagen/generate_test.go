package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestGeneratedOutputDeterministic proves the output is a pure function of the
// schema files: repeated runs, a copy of the schemas at another path, and the
// shape of every file (header, LF endings, gofmt-stable) all agree byte for byte.
func TestGeneratedOutputDeterministic(t *testing.T) {
	first := generateReal(t)
	if len(first) == 0 {
		t.Fatal("nothing was generated")
	}

	// Go randomises map iteration on every range loop, so a dependence on it
	// shows up as a difference between runs.
	for i := 0; i < 5; i++ {
		again := generateReal(t)
		assertSameFiles(t, "repeat run", first, again)
	}

	// The same schemas somewhere else on disk produce the same bytes: no
	// absolute path, working directory or timestamp leaks into the output.
	copyDir := t.TempDir()
	copySchemas(t, copyDir)
	cfg := realConfig(t)
	cfg.Schemas = os.DirFS(filepath.Join(copyDir, "schemas"))
	moved, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFiles(t, "schemas copied to another directory", first, moved)

	root := repoRoot(t)
	for p, content := range first {
		if !isGenerated(content) {
			t.Errorf("%s: the first line must say %q", p, generatedMarker)
		}
		if bytes.Contains(content, []byte("\r")) {
			t.Errorf("%s: contains a carriage return; output must use LF", p)
		}
		if !bytes.HasSuffix(content, []byte("\n")) || bytes.HasSuffix(content, []byte("\n\n")) {
			t.Errorf("%s: must end with exactly one newline", p)
		}
		if bytes.Contains(content, []byte(filepath.ToSlash(root))) || bytes.Contains(content, []byte(copyDir)) {
			t.Errorf("%s: contains an absolute path", p)
		}
		if strings.HasSuffix(p, ".go") {
			formatted, err := format.Source(content)
			if err != nil || !bytes.Equal(formatted, content) {
				t.Errorf("%s: is not gofmt-formatted (%v)", p, err)
			}
		}
	}
}

func assertSameFiles(t *testing.T, what string, a, b map[string][]byte) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: %d files vs %d files", what, len(a), len(b))
	}
	for p, content := range a {
		other, ok := b[p]
		if !ok {
			t.Fatalf("%s: %s missing from the second run", what, p)
		}
		if !bytes.Equal(content, other) {
			t.Fatalf("%s: %s differs between runs", what, p)
		}
	}
}

// TestCommittedOutputIsCurrent is the drift check as a test: the generated
// files committed in pkg/contracts/gen and sdk/typescript/src/gen are exactly
// what the schemas generate now. When it fails, run go run ./tools/schemagen.
func TestCommittedOutputIsCurrent(t *testing.T) {
	code, out, errOut := runCLI(t, repoRoot(t), "-check")
	if code != 0 {
		t.Fatalf("schemagen -check exited %d\n%s%s", code, out, errOut)
	}
}

// TestCheckDetectsDrift makes every kind of drift in a throw-away repository
// and requires -check to notice each one, then to accept the repaired state.
func TestCheckDetectsDrift(t *testing.T) {
	root := tempRepo(t)

	if code, _, errOut := runCLI(t, root, "-check"); code != 1 {
		t.Fatalf("-check with no output at all exited %d, want 1\n%s", code, errOut)
	}
	if code, out, errOut := runCLI(t, root); code != 0 {
		t.Fatalf("generate exited %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, root, "-check"); code != 0 {
		t.Fatalf("-check right after generating exited %d\n%s%s", code, out, errOut)
	}

	goFile := filepath.Join(root, "pkg", "contracts", "gen", "conversation", "task_proposal.gen.go")
	tsFile := filepath.Join(root, "sdk", "typescript", "src", "gen", "actions", "prepared-action.ts")

	drift := []struct {
		name    string
		mutate  func(t *testing.T) (undo func())
		mention string // what the report must name
	}{
		{"edited generated Go file", func(t *testing.T) func() {
			return editFile(t, goFile, func(b []byte) []byte { return append(b, []byte("\n// hand edit\n")...) })
		}, "task_proposal.gen.go"},
		{"edited generated TypeScript file", func(t *testing.T) func() {
			return editFile(t, tsFile, func(b []byte) []byte { return bytes.Replace(b, []byte("export "), []byte("export declare "), 1) })
		}, "prepared-action.ts"},
		{"deleted generated file", func(t *testing.T) func() {
			orig, err := os.ReadFile(goFile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(goFile); err != nil {
				t.Fatal(err)
			}
			return func() { os.WriteFile(goFile, orig, 0o644) }
		}, "task_proposal.gen.go: missing"},
		{"leftover generated file", func(t *testing.T) func() {
			extra := filepath.Join(root, "pkg", "contracts", "gen", "common", "retired.gen.go")
			content := "// " + generatedMarker + " from schemas/common/retired.schema.json. DO NOT EDIT.\n\npackage common\n"
			if err := os.WriteFile(extra, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			return func() { os.Remove(extra) }
		}, "retired.gen.go"},
		{"schema changed without regenerating", func(t *testing.T) func() {
			schema := filepath.Join(root, "schemas", "conversation", "file-ref.schema.json")
			return editFile(t, schema, func(b []byte) []byte {
				out := bytes.Replace(b, []byte("Opaque file ID."), []byte("Opaque file ID, changed."), 1)
				if bytes.Equal(out, b) {
					t.Fatal("fixture text not found in file-ref.schema.json")
				}
				return out
			})
		}, "file_ref.gen.go"},
		{"schema property added without regenerating", func(t *testing.T) func() {
			schema := filepath.Join(root, "schemas", "common", "scope.schema.json")
			return editFile(t, schema, func(b []byte) []byte {
				out := bytes.Replace(b, []byte(`"properties": {`), []byte(`"properties": {"extra_level": {"type": "string"},`), 1)
				if bytes.Equal(out, b) {
					t.Fatal("fixture text not found in scope.schema.json")
				}
				return out
			})
		}, "scope"},
	}
	for _, tc := range drift {
		t.Run(tc.name, func(t *testing.T) {
			undo := tc.mutate(t)
			code, _, errOut := runCLI(t, root, "-check")
			if code != 1 {
				t.Errorf("-check exited %d, want 1 (stderr: %s)", code, errOut)
			}
			if !strings.Contains(errOut, tc.mention) {
				t.Errorf("the report must mention %q, got:\n%s", tc.mention, errOut)
			}
			undo()
			if code, out, errOut := runCLI(t, root, "-check"); code != 0 {
				t.Fatalf("-check after undoing the change exited %d\n%s%s", code, out, errOut)
			}
		})
	}

	// Hand-written files next to generated ones are not generated output and
	// must neither fail the check nor be deleted by a regeneration.
	t.Run("hand-written files are left alone", func(t *testing.T) {
		own := filepath.Join(root, "pkg", "contracts", "gen", "doc.go")
		if err := os.WriteFile(own, []byte("package gen\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := runCLI(t, root, "-check"); code != 0 {
			t.Fatalf("a hand-written file made -check fail: %s", errOut)
		}
		if code, _, errOut := runCLI(t, root); code != 0 {
			t.Fatalf("regeneration failed: %s", errOut)
		}
		if _, err := os.Stat(own); err != nil {
			t.Fatalf("regeneration removed a hand-written file: %v", err)
		}
	})

	// Regeneration repairs drift, including removing a generated file nothing
	// produces any more.
	t.Run("regeneration repairs drift", func(t *testing.T) {
		extra := filepath.Join(root, "sdk", "typescript", "src", "gen", "common", "retired.ts")
		if err := os.WriteFile(extra, []byte("// "+generatedMarker+" from schemas/common/retired.schema.json. DO NOT EDIT.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		undo := editFile(t, goFile, func(b []byte) []byte {
			return []byte("// " + generatedMarker + ". DO NOT EDIT.\n\npackage conversation\n")
		})
		_ = undo
		if code, _, _ := runCLI(t, root, "-check"); code != 1 {
			t.Fatalf("-check must fail before regenerating, exited %d", code)
		}
		if code, out, errOut := runCLI(t, root); code != 0 {
			t.Fatalf("regeneration exited %d\n%s%s", code, out, errOut)
		}
		if _, err := os.Stat(extra); !os.IsNotExist(err) {
			t.Errorf("the leftover generated file was not removed (%v)", err)
		}
		if code, out, errOut := runCLI(t, root, "-check"); code != 0 {
			t.Fatalf("-check after regenerating exited %d\n%s%s", code, out, errOut)
		}
	})
}

// editFile rewrites a file and returns a function that restores it.
func editFile(t *testing.T, path string, f func([]byte) []byte) func() {
	t.Helper()
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, f(append([]byte(nil), orig...)), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---------------------------------------------------------------------------
// The schema constructs M1-03 uses, checked on the real generated output.

func parseGen(t *testing.T, files map[string][]byte, path string) *ast.File {
	t.Helper()
	src, ok := files[path]
	if !ok {
		t.Fatalf("%s was not generated", path)
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("%s does not parse: %v", path, err)
	}
	return f
}

// goStruct returns the fields of a struct type by Go field name.
func goStruct(t *testing.T, f *ast.File, name string) map[string]*ast.Field {
	t.Helper()
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range gd.Specs {
			ts, ok := s.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("%s is not a struct", name)
			}
			out := map[string]*ast.Field{}
			for _, fld := range st.Fields.List {
				for _, n := range fld.Names {
					out[n.Name] = fld
				}
			}
			return out
		}
	}
	t.Fatalf("type %s not found", name)
	return nil
}

func hasType(f *ast.File, name string) bool {
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok {
			for _, s := range gd.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok && ts.Name.Name == name {
					return true
				}
			}
		}
	}
	return false
}

// methods returns the names of the methods declared on type recv (value or
// pointer receiver), counted per name.
func methods(f *ast.File, recv string) map[string]int {
	out := map[string]int{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
			continue
		}
		if types.ExprString(fd.Recv.List[0].Type) == recv || types.ExprString(fd.Recv.List[0].Type) == "*"+recv {
			out[fd.Name.Name]++
		}
	}
	return out
}

func fieldType(f *ast.Field) string { return types.ExprString(f.Type) }

func fieldJSONTag(t *testing.T, f *ast.Field) string {
	t.Helper()
	raw, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		t.Fatal(err)
	}
	return reflect.StructTag(raw).Get("json")
}

func TestSchemaConstructs(t *testing.T) {
	files := generateReal(t)
	const g = "pkg/contracts/gen/"
	const ts = "sdk/typescript/src/gen/"
	text := func(path string) string {
		t.Helper()
		b, ok := files[path]
		if !ok {
			t.Fatalf("%s was not generated", path)
		}
		return string(b)
	}

	t.Run("$ref into another file's properties", func(t *testing.T) {
		// common/error#/properties/code is an enum declared by Error.code and
		// referenced from actions/shared (issue.code) and dialogue-directive
		// (reason_code).
		shared := parseGen(t, files, g+"actions/shared.gen.go")
		if got := fieldType(goStruct(t, shared, "Issue")["Code"]); got != "common.ErrorCode" {
			t.Errorf("Issue.Code is %s, want common.ErrorCode", got)
		}
		dd := parseGen(t, files, g+"conversation/dialogue_directive.gen.go")
		if got := fieldType(goStruct(t, dd, "DialogueDirective")["ReasonCode"]); got != "*common.ErrorCode" {
			t.Errorf("DialogueDirective.ReasonCode is %s, want *common.ErrorCode", got)
		}
		errFile := parseGen(t, files, g+"common/error.gen.go")
		if got := fieldType(goStruct(t, errFile, "Error")["Code"]); got != "ErrorCode" {
			t.Errorf("Error.Code is %s, want ErrorCode", got)
		}
		if !hasType(errFile, "ErrorCode") {
			t.Error("common.ErrorCode is not declared")
		}
		if m := methods(errFile, "ErrorCode"); m["Valid"] != 1 || m["MarshalJSON"] != 1 || m["UnmarshalJSON"] != 1 {
			t.Errorf("ErrorCode enum methods = %v", m)
		}
		if s := text(ts + "actions/shared.ts"); !strings.Contains(s, `import type { ErrorCode } from "../common/error.js";`) ||
			!strings.Contains(s, "  code: ErrorCode;") {
			t.Error("actions/shared.ts does not reference common ErrorCode")
		}
		// application-package declares its own ErrorCode alias next to the common
		// one; the TypeScript import must not clash with it.
		if s := text(ts + "application-package/defs.ts"); !strings.Contains(s, `import type { ErrorCode as CommonErrorCode } from "../common/error.js";`) ||
			!strings.Contains(s, "export type ErrorCode = CommonErrorCode;") {
			t.Error("application-package/defs.ts must alias the clashing import")
		}
	})

	t.Run("$ref into another file's $defs", func(t *testing.T) {
		// turn-event's clarification event points at rich-block#/$defs/clarificationChoicesBlock.
		te := parseGen(t, files, g+"conversation/turn_event.gen.go")
		if got := fieldType(goStruct(t, te, "ClarificationEvent")["Clarification"]); got != "ClarificationChoicesBlock" {
			t.Errorf("ClarificationEvent.Clarification is %s, want ClarificationChoicesBlock", got)
		}
		rb := parseGen(t, files, g+"conversation/rich_block.gen.go")
		if !hasType(rb, "ClarificationChoicesBlock") {
			t.Error("ClarificationChoicesBlock is not declared in rich_block.gen.go")
		}
		s := text(ts + "conversation/turn-event.ts")
		if !strings.Contains(s, `ClarificationChoicesBlock, RichBlock } from "./rich-block.js";`) ||
			!strings.Contains(s, "  clarification: ClarificationChoicesBlock;") {
			t.Error("turn-event.ts does not import ClarificationChoicesBlock from rich-block")
		}
		// A $ref into shared defs of another area.
		if got := fieldType(goStruct(t, te, "PreparedActionEvent")["PreparedAction"]); got != "actions.PreparedAction" {
			t.Errorf("PreparedActionEvent.PreparedAction is %s, want actions.PreparedAction", got)
		}
	})

	t.Run("anyOf and oneOf with null", func(t *testing.T) {
		tp := parseGen(t, files, g+"conversation/task_proposal.gen.go")
		f := goStruct(t, tp, "TaskProposal")["CapabilityID"]
		if got := fieldType(f); got != "*common.CapabilityID" {
			t.Errorf("TaskProposal.CapabilityID is %s, want *common.CapabilityID", got)
		}
		// Required and nullable: the key is always written, null when nil.
		if tag := fieldJSONTag(t, f); tag != "capability_id" {
			t.Errorf("TaskProposal.CapabilityID json tag is %q, want capability_id with no omit option", tag)
		}
		dd := parseGen(t, files, g+"conversation/dialogue_directive.gen.go")
		if got := fieldType(goStruct(t, dd, "DialogueDirective")["CapabilityID"]); got != "*common.CapabilityID" {
			t.Errorf("DialogueDirective.CapabilityID is %s", got)
		}
		rs := parseGen(t, files, g+"applicationpackage/rag_source.gen.go")
		f = goStruct(t, rs, "RAGSource")["CredentialRef"]
		if got := fieldType(f); got != "*CredentialRef" || fieldJSONTag(t, f) != "credential_ref" {
			t.Errorf("RAGSource.CredentialRef is %s with tag %q (oneOf [ref, null] must be a required pointer)", got, fieldJSONTag(t, f))
		}
		if s := text(ts + "conversation/task-proposal.ts"); !strings.Contains(s, "  capability_id: CapabilityID | null;") {
			t.Error("task-proposal.ts: capability_id must be CapabilityID | null and required")
		}
	})

	t.Run("oneOf discriminated by a const member", func(t *testing.T) {
		rb := parseGen(t, files, g+"conversation/rich_block.gen.go")
		if got := fieldType(goStruct(t, rb, "RichBlock")["Value"]); got != "RichBlockValue" {
			t.Errorf("RichBlock.Value is %s, want RichBlockValue", got)
		}
		m := methods(rb, "RichBlock")
		if m["MarshalJSON"] != 1 || m["UnmarshalJSON"] != 1 {
			t.Errorf("RichBlock methods = %v, want MarshalJSON and UnmarshalJSON", m)
		}
		variants := []string{"MarkdownBlock", "CodeBlock", "TableBlock", "ChartBlock", "DiagramBlock",
			"CitationListBlock", "ActionCardBlock", "ClarificationChoicesBlock", "ConfirmationPanelBlock"}
		for _, v := range variants {
			if methods(rb, v)["isRichBlock"] != 1 {
				t.Errorf("%s does not implement the RichBlock union", v)
			}
			// Each variant forces its discriminator when encoded.
			if methods(rb, v)["MarshalJSON"] != 1 {
				t.Errorf("%s has no MarshalJSON to force its type member", v)
			}
		}
		te := parseGen(t, files, g+"conversation/turn_event.gen.go")
		n := 0
		for name, c := range methods(te, "TokenEvent") {
			if name == "isTurnEvent" {
				n += c
			}
		}
		if n != 1 {
			t.Errorf("TokenEvent does not implement the TurnEvent union")
		}
		s := text(ts + "conversation/rich-block.ts")
		for _, v := range variants {
			if !strings.Contains(s, "  | "+v) {
				t.Errorf("rich-block.ts: RichBlock union lacks %s", v)
			}
		}
		if !strings.Contains(s, `  type: "markdown";`) {
			t.Error("rich-block.ts: the markdown variant must carry the literal type")
		}
		// Inline (unnamed) variants are named after the union and the discriminator value.
		rs := parseGen(t, files, g+"applicationpackage/rag_source.gen.go")
		for _, n := range []string{"RAGSourceRoot", "RAGSourceRootFolder", "RAGSourceRootMount", "RAGSourceRootAPI", "RAGSourceRootPackage"} {
			if !hasType(rs, n) {
				t.Errorf("rag_source.gen.go lacks %s", n)
			}
		}
	})

	t.Run("if/then/else overlays give loose types", func(t *testing.T) {
		pa := parseGen(t, files, g+"actions/prepared_action.gen.go")
		fields := goStruct(t, pa, "PreparedAction")
		for _, name := range []string{"PreparationID", "CapabilityID", "State", "Summary", "Effects", "RiskLevel",
			"DataVersions", "PreparedAt", "ExpiresAt", "InvalidationConditions", "Confirmation", "IdempotencyKey"} {
			if fields[name] == nil {
				t.Errorf("PreparedAction lacks %s: an allOf/if overlay must not remove members", name)
			}
		}
		if got := fieldType(fields["RiskLevel"]); got != "RiskLevel" && got != "actions.RiskLevel" {
			t.Errorf("PreparedAction.RiskLevel is %s", got)
		}
		src := text(g + "actions/prepared_action.gen.go")
		if !strings.Contains(src, "cross-field rules") {
			t.Error("a type with overlays must say that schema validation stays authoritative")
		}
		// The conditional "capability_id is required when action is invoke_capability"
		// cannot be expressed: the member stays optional in the type.
		shared := parseGen(t, files, g+"actions/shared.gen.go")
		if got := fieldType(goStruct(t, shared, "AllowedNextAction")["CapabilityID"]); got != "*common.CapabilityID" {
			t.Errorf("AllowedNextAction.CapabilityID is %s, want an optional pointer", got)
		}
	})

	t.Run("open maps", func(t *testing.T) {
		tp := parseGen(t, files, g+"conversation/task_proposal.gen.go")
		if got := fieldType(goStruct(t, tp, "TaskProposal")["Fields"]); got != "map[string]TaskProposalCandidateValue" {
			t.Errorf("TaskProposal.Fields is %s, want a map keyed by string", got)
		}
		if got := fieldType(goStruct(t, tp, "TaskProposalUncertainty")["Fields"]); got != "map[string]string" {
			t.Errorf("TaskProposalUncertainty.Fields is %s", got)
		}
	})

	t.Run("authority is carried into the types", func(t *testing.T) {
		tp := parseGen(t, files, g+"conversation/task_proposal.gen.go")
		f := goStruct(t, tp, "TaskProposal")["TraceID"]
		raw, _ := strconv.Unquote(f.Tag.Value)
		if got := reflect.StructTag(raw).Get("alixai"); got != "alixai" {
			t.Errorf("TraceID alixai tag = %q", got)
		}
		raw, _ = strconv.Unquote(goStruct(t, tp, "TaskProposal")["Intent"].Tag.Value)
		if got := reflect.StructTag(raw).Get("alixai"); got != "model" {
			t.Errorf("Intent alixai tag = %q", got)
		}
		if s := text(ts + "conversation/task-proposal.ts"); !strings.Contains(s, "@authority model") {
			t.Error("TypeScript output lacks the @authority annotation")
		}
	})

	t.Run("every area and file is generated", func(t *testing.T) {
		var got []string
		for p := range files {
			got = append(got, p)
		}
		sort.Strings(got)
		for _, want := range []string{
			g + "common/error.gen.go", g + "common/doc.gen.go",
			g + "actions/shared.gen.go", g + "conversation/turn_event.gen.go",
			g + "applicationpackage/manifest.gen.go",
			ts + "index.ts", ts + "common/index.ts", ts + "application-package/capability.ts",
		} {
			if _, ok := files[want]; !ok {
				t.Errorf("%s was not generated; got %v", want, got)
			}
		}
	})
}
