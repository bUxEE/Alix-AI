package scaffold

import (
	"context"
	"errors"
	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func scaffoldMust(t *testing.T, out string, in ScaffoldInput) *ScaffoldResult {
	t.Helper()
	res, err := ScaffoldWrite(out, in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestScaffoldUsesOnlyVerifiedFindings(t *testing.T) {
	w := scaffoldNewWorld()
	out := filepath.Join(t.TempDir(), "draft")
	res := scaffoldMust(t, out, w.in)

	if want := []string{"catalog.find_title", "reservations.create"}; !slices.Equal(res.Capabilities, want) {
		t.Fatalf("capabilities = %v, want %v", res.Capabilities, want)
	}
	if want := []string{"documents"}; !slices.Equal(res.RAGSources, want) {
		t.Fatalf("rag sources = %v, want %v", res.RAGSources, want)
	}
	if res.FindingsUsed != 3 || res.FindingsDropped != 5 {
		t.Fatalf("used %d dropped %d, want 3 and 5", res.FindingsUsed, res.FindingsDropped)
	}
	// the tool names found in a verified file are listed for the owner, never fields
	if !slices.Contains(res.Candidates, "find_title") || !slices.Contains(res.Candidates, "look_up_title") || slices.Contains(res.Candidates, "unknown_tool") {
		t.Fatalf("candidates = %v", res.Candidates)
	}

	tree := scaffoldReadTree(t, out)
	var all strings.Builder
	for name, c := range tree {
		all.WriteString(c)
		if strings.HasSuffix(name, "/") {
			continue
		}
		// every generated file says it is a draft on its first line
		first, _, _ := strings.Cut(c, "\n")
		if !strings.Contains(first, "DRAFT") {
			t.Errorf("%s does not start with a DRAFT marker: %q", name, first)
		}
		if strings.Contains(c, "\r") {
			t.Errorf("%s has a carriage return", name)
		}
	}
	blob := all.String()

	// nothing an unused finding says, cites or names reaches any file; nothing a used finding says is copied
	for _, banned := range []string{
		"unverified.cap", "contra.cap", "model.cap", "illegal.cap", "stale.cap", "ghost.cap", "unknown_tool",
		"unverified-notes.md", "contradicted.md", "model-claim.md", "illegal.md", "stale.md", "fnd_000000000000000",
		scaffoldCanary, scaffoldSum(w.files["notes/stale.md"]), scaffoldSum(w.files["notes/illegal.md"]),
	} {
		if strings.Contains(blob, banned) {
			t.Errorf("the draft carries %q, which no verified finding supports", banned)
		}
	}
	// what verified evidence supports is there, with its file and hash
	for _, want := range []string{
		"catalog.find_title", "reservations.create", "docs/guide.md lines 1-1 sha256 " + scaffoldSum(w.files["docs/guide.md"]),
		"documents/loan-policies.md", "id: demo-app",
	} {
		if !strings.Contains(blob, want) {
			t.Errorf("the draft lacks %q", want)
		}
	}

	// a risk level is the owner's: it never appears as a field, and confirmation defaults to the strictest mode
	for name, c := range tree {
		if strings.HasSuffix(name, "/") {
			continue
		}
		if strings.HasSuffix(name, ".yaml") && strings.Contains(scaffoldUncomment(c), "risk_level") {
			t.Errorf("%s sets a risk level", name)
		}
		if strings.HasPrefix(name, "capabilities/") {
			body := scaffoldUncomment(c)
			if strings.Contains(body, "effect_class") || strings.Contains(body, "mode: none") || !strings.Contains(body, "mode: step_up") {
				t.Errorf("%s does not default confirmation to the strictest mode:\n%s", name, body)
			}
		}
		if strings.HasPrefix(name, "rag-sources/") {
			body := scaffoldUncomment(c)
			if !strings.Contains(body, "classification: restricted") || !strings.Contains(body, "egress_mode: local_only") {
				t.Errorf("%s does not default to the strictest data class and egress mode:\n%s", name, body)
			}
		}
	}
	// the manifest forbids external providers until the owner says otherwise
	if m := scaffoldUncomment(tree["alixai.yaml"]); !strings.Contains(m, "forbid: true") {
		t.Errorf("the manifest does not forbid external providers:\n%s", m)
	}
	// documentation is a stub: references and TODO markers, no generated prose
	doc := tree["documentation/cap-001.md"]
	if !strings.Contains(doc, "TODO(owner)") || !strings.Contains(doc, "docs/guide.md") {
		t.Errorf("the documentation stub is not a stub with references:\n%s", doc)
	}
	// the requirements without verified evidence are listed by id only, whatever the report said about them
	todo := tree["DRAFT-TODO.md"]
	if !strings.Contains(todo, "`req.data.commit`") || strings.Contains(todo, "`req.surface.control_surface`") {
		t.Errorf("DRAFT-TODO.md lists the wrong gaps:\n%s", todo)
	}
}

// scaffoldComplete fills every TODO of a draft the way an owner would, with the smallest valid values, to show that the files that
// were generated fit the schemas once completed.
func scaffoldComplete(t *testing.T, dir string, caps int, firstCap string, rag bool) {
	t.Helper()
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	write := func(rel, c string) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const strict = "mode: step_up\n  step_up_channel: step_up_credential\n"
	for i := 1; i <= caps; i++ {
		rel := "capabilities/cap-00" + string(rune('0'+i)) + ".yaml"
		c := read(rel)
		if !strings.Contains(c, strict) {
			t.Fatalf("%s has no strict confirmation to relax", rel)
		}
		c = strings.Replace(c, strict, "mode: none\n", 1)
		write(rel, c+`version: 1.0.0
audience: user
title: A capability
purpose: What the capability does.
scope_level: workspace
permissions:
- thing.read
fields: []
entity_types: {}
ambiguity: []
validation: []
failure_states:
- code: unavailable
  description: The service cannot be reached.
effect_class: none
risk_level: 0
operations:
  read:
    endpoint: todo-endpoint
    mcp_tool: a_tool
    result_schema: schemas/result.schema.json
response_obligations:
- fact: thing.count
  when: success
  description: How many.
next_actions: []
doc_refs:
- d-risk
examples:
- id: ex-one
  language: en
  test_only: true
  utterance: Show me the things
  expected_fields: {}
`)
	}
	idx := read("documentation/index.yaml")
	write("documentation/index.yaml", strings.Replace(idx, "entries: []\n", `entries:
- id: d-risk
  capability_id: `+firstCap+`
  facet: risk_level
  language: en
  title: Risk
  summary: The risk level of the capability.
  location:
    path: documentation/cap-001.md
`, 1))
	if rag {
		c := read("rag-sources/src-001.yaml")
		write("rag-sources/src-001.yaml", c+`type: local_folder
owner:
  name: The owner
scope:
  application_id: demo-app
  tenant:
    mode: shared
  workspace:
    mode: shared
root:
  kind: folder
  path: /srv/documents
include:
- "**/*.md"
exclude: []
credential_ref: null
formats:
- text/markdown
limits:
  max_file_bytes: 1000000
  max_total_bytes: 100000000
  max_files: 1000
sync:
  method: manual
deletion:
  purge_after_hours: 0
  on_source_removed: purge
parser:
  ocr: disabled
  max_pages_per_file: 10
  timeout_seconds: 30
citation:
  kind: alixai_preview
retention:
  derived_data_days: null
  import_originals: false
  audit:
    record_access: true
    retention_days: 30
`)
	}
}

func scaffoldCritical(res *apppkg.Result) map[string]bool {
	out := map[string]bool{}
	for _, f := range res.Findings {
		if f.Severity == contracts.SeverityCritical {
			out[f.CheckID] = true
		}
	}
	return out
}

func TestScaffoldIsUnsignedAndTheLoaderRefusesIt(t *testing.T) {
	w := scaffoldNewWorld()
	out := filepath.Join(t.TempDir(), "draft")
	scaffoldMust(t, out, w.in)

	// no signature of any kind was written
	if _, err := os.Lstat(filepath.Join(out, "signatures")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the draft has a signatures directory (err %v)", err)
	}
	for name := range scaffoldReadTree(t, out) {
		if strings.Contains(name, "signature") {
			t.Fatalf("the draft carries %s", name)
		}
	}

	load := func() *apppkg.Result {
		res, err := apppkg.Load(context.Background(), apppkg.FromDir(out), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// the draft as it is: refused, as unsigned, and no typed package comes out of it
	res := load()
	crit := scaffoldCritical(res)
	if !crit["signature.missing"] {
		t.Fatalf("the loader does not refuse the draft as unsigned: %v", crit)
	}
	if res.Package != nil || res.OK() || res.Authenticity == apppkg.AuthVerified || res.Authenticity == apppkg.AuthIntegrityOnly {
		t.Fatalf("the loader let the draft through: authenticity %q", res.Authenticity)
	}
	// and the unfinished parts are refused too, so a draft that was never completed cannot be signed by accident
	if !crit["schema.capability"] || !crit["schema.documentation_index"] || !crit["schema.rag_source"] {
		t.Fatalf("the unfinished stubs are not refused: %v", crit)
	}

	// completed: no schema or syntax error is left, and it is still refused as unsigned
	scaffoldComplete(t, out, 2, "catalog.find_title", true)
	res = load()
	crit = scaffoldCritical(res)
	for id := range crit {
		if strings.HasPrefix(id, "schema.") || strings.HasPrefix(id, "yaml.") || strings.HasPrefix(id, "manifest.") {
			t.Errorf("the completed draft still fails %s", id)
		}
	}
	if !crit["signature.missing"] || res.Package != nil {
		t.Fatalf("the completed draft is not refused as unsigned: %v", crit)
	}
}

// scaffoldLink makes a directory link at link pointing at target: a symlink, or on Windows a junction. It skips when the host does
// not allow it.
func scaffoldLink(t *testing.T, link, target string, junction bool) {
	t.Helper()
	if junction {
		if runtime.GOOS != "windows" {
			t.Skip("junctions exist on Windows only")
		}
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction here: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
}

func TestScaffoldNeverOverwritesOrEscapesTheOutputDirectory(t *testing.T) {
	w := scaffoldNewWorld()
	base := t.TempDir()

	refuse := func(t *testing.T, name, out, scanned string) {
		t.Helper()
		in := w.in
		in.ScannedRoot = scanned
		if _, err := ScaffoldWrite(out, in); err == nil {
			t.Fatalf("%s: the scaffold wrote to %s", name, out)
		}
	}

	t.Run("an existing directory is never touched", func(t *testing.T) {
		dir := filepath.Join(base, "exists")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(dir, "mine.txt")
		if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, "dir", dir, "")
		if got := scaffoldNames(scaffoldReadTree(t, dir)); !slices.Equal(got, []string{"./", "mine.txt"}) {
			t.Fatalf("the directory changed: %v", got)
		}
		if b, _ := os.ReadFile(keep); string(b) != "mine" {
			t.Fatal("a file in the directory was overwritten")
		}
	})
	t.Run("an empty existing directory is refused too", func(t *testing.T) {
		dir := filepath.Join(base, "empty")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		refuse(t, "empty dir", dir, "")
		if got := scaffoldNames(scaffoldReadTree(t, dir)); !slices.Equal(got, []string{"./"}) {
			t.Fatalf("the directory changed: %v", got)
		}
	})
	t.Run("an existing file is never overwritten", func(t *testing.T) {
		f := filepath.Join(base, "afile")
		if err := os.WriteFile(f, []byte("precious"), 0o600); err != nil {
			t.Fatal(err)
		}
		refuse(t, "file", f, "")
		if b, _ := os.ReadFile(f); string(b) != "precious" {
			t.Fatal("the file was changed")
		}
	})
	for _, junction := range []bool{false, true} {
		name, tag := "a symlink", "s"
		if junction {
			name, tag = "a junction", "j"
		}
		t.Run(name+" to a directory is not followed", func(t *testing.T) {
			target := filepath.Join(base, "victim-"+tag)
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(base, "link-"+tag)
			scaffoldLink(t, link, target, junction)
			refuse(t, name, link, "")
			if got := scaffoldNames(scaffoldReadTree(t, target)); !slices.Equal(got, []string{"./"}) {
				t.Fatalf("something was written through the link: %v", got)
			}
		})
	}
	t.Run("a dangling symlink is refused", func(t *testing.T) {
		link := filepath.Join(base, "dangling")
		scaffoldLink(t, link, filepath.Join(base, "nowhere"), false)
		refuse(t, "dangling", link, "")
		if _, err := os.Lstat(filepath.Join(base, "nowhere")); err == nil {
			t.Fatal("the link target was created")
		}
	})
	t.Run("the scanned directory is never written to", func(t *testing.T) {
		scanned := filepath.Join(base, "project")
		if err := os.MkdirAll(filepath.Join(scanned, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		before := scaffoldNames(scaffoldReadTree(t, scanned))
		refuse(t, "inside", filepath.Join(scanned, "draft"), scanned)
		refuse(t, "inside, deeper", filepath.Join(scanned, "sub", "draft"), scanned)
		refuse(t, "root itself", scanned, scanned)
		refuse(t, "through a dotdot", filepath.Join(scanned, "sub", "..", "draft"), scanned)
		if after := scaffoldNames(scaffoldReadTree(t, scanned)); !slices.Equal(before, after) {
			t.Fatalf("the scanned directory changed: %v -> %v", before, after)
		}
	})
	t.Run("a missing parent is refused and nothing is created", func(t *testing.T) {
		refuse(t, "no parent", filepath.Join(base, "no", "such", "parent", "draft"), "")
		if _, err := os.Lstat(filepath.Join(base, "no")); err == nil {
			t.Fatal("a parent directory was created")
		}
	})
	t.Run("without the scanned files nothing is created", func(t *testing.T) {
		out := filepath.Join(base, "nofiles")
		in := w.in
		in.Source = nil
		if _, err := ScaffoldWrite(out, in); err == nil {
			t.Fatal("the scaffold ran without a way to re-check the evidence")
		}
		if _, err := os.Lstat(out); err == nil {
			t.Fatal("the output directory was created")
		}
	})
	t.Run("hostile names never become a path", func(t *testing.T) {
		names := []string{"../x", "con", "nul", "a/b", `..\y`, "aux.txt", "C:/evil", "x\x00y"}
		hostile := scaffoldBuildWorld(strings.Join(names, " ") + "\n")
		hostile.in.Inventory.Capabilities = append(slices.Clone(names), "catalog.find_title")
		parent := t.TempDir()
		out := filepath.Join(parent, "draft")
		scaffoldMust(t, out, hostile.in)

		want := map[string]bool{
			"./": true, "draft/": true, "draft/alixai.yaml": true, "draft/DRAFT-TODO.md": true, "draft/documentation/": true,
			"draft/documentation/index.yaml": true, "draft/capabilities/": true, "draft/rag-sources/": true,
		}
		for i := 1; i <= 4; i++ { // con, nul, aux.txt and catalog.find_title are valid ids; their files are numbered, never named after them
			n := string(rune('0' + i))
			want["draft/capabilities/cap-00"+n+".yaml"] = true
			want["draft/documentation/cap-00"+n+".md"] = true
		}
		want["draft/rag-sources/src-001.yaml"] = true
		for name := range scaffoldReadTree(t, parent) {
			if !want[name] {
				t.Errorf("unexpected path %s", name)
			}
		}
		m := scaffoldReadTree(t, parent)["draft/alixai.yaml"]
		for _, bad := range []string{"../x", `..\y`, "C:/evil", "a/b", "\x00"} {
			if strings.Contains(m, bad) {
				t.Errorf("the hostile id %q reached the manifest", bad)
			}
		}
		if ids := regexp.MustCompile(`(?m)^  ([a-z][a-z0-9_.]*): capabilities/cap-00\d\.yaml$`).FindAllStringSubmatch(m, -1); len(ids) != 4 {
			t.Errorf("the manifest lists %d capabilities, want 4:\n%s", len(ids), m)
		}
	})
	t.Run("a directory that is not the scanned one cannot confirm anything", func(t *testing.T) {
		other := scaffoldNewWorld()
		other.in.Source = os.DirFS(t.TempDir())
		res := scaffoldMust(t, filepath.Join(base, "other"), other.in)
		if res.FindingsUsed != 0 || len(res.Capabilities) != 0 || len(res.RAGSources) != 0 {
			t.Fatalf("evidence was accepted from a directory that does not hold it: %+v", res)
		}
	})
}

// Folder names that slug to the same id must not make the id search loop: x-3, x and x all become distinct ids.
func TestScaffoldDuplicateRAGFolderNamesGetDistinctIDs(t *testing.T) {
	w := scaffoldBuildWorld("")
	w.files["a/x-3/one.md"] = "one\n"
	w.files["b/x/two.md"] = "two\n"
	w.files["c/x/three.md"] = "three\n"
	for i, p := range []string{"a/x-3/one.md", "b/x/two.md", "c/x/three.md"} {
		w.fsys[p] = &fstest.MapFile{Data: []byte(w.files[p])}
		w.in.Report.Findings = append(w.in.Report.Findings, scaffoldFinding("00000000000001"+string(rune('a'+i))+"0", "req.rag.source_candidates", readiness.StatusFound, readiness.SourceDeterministic, w.files, p))
	}
	done := make(chan *ScaffoldResult, 1)
	go func() {
		res, err := ScaffoldWrite(filepath.Join(t.TempDir(), "draft"), w.in)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil {
			return
		}
		seen := map[string]bool{}
		for _, id := range res.RAGSources {
			if seen[id] {
				t.Fatalf("duplicate RAG source id %q in %v", id, res.RAGSources)
			}
			seen[id] = true
		}
		if len(res.RAGSources) != 4 { // documents, x-3, x, x-2
			t.Fatalf("RAG sources = %v", res.RAGSources)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the RAG id search does not end")
	}
}

// The gap list follows the rule for alternative transports: once verified evidence meets one transport (MCP here), the
// requirements of the others are not gaps. With no transport met they all are.
func TestScaffoldGapsLeaveOutTheTransportsNotInUse(t *testing.T) {
	gapsOf := func(w *scaffoldWorld) string {
		w.in.Report.Checklist = append(w.in.Report.Checklist,
			readiness.ReportChecklistItem{RequirementID: "req.mcp.server", CheckID: "application.control_surface", Level: readiness.LevelRecommended, Status: readiness.StatusMissing},
			readiness.ReportChecklistItem{RequirementID: "req.openapi.description", CheckID: "application.control_surface", Level: readiness.LevelRecommended, Status: readiness.StatusMissing},
			readiness.ReportChecklistItem{RequirementID: "req.native.adapter", CheckID: "package.inventory", Level: readiness.LevelRecommended, Status: readiness.StatusMissing},
		)
		out := filepath.Join(t.TempDir(), "draft")
		scaffoldMust(t, out, w.in)
		return scaffoldReadTree(t, out)["DRAFT-TODO.md"]
	}
	todo := gapsOf(scaffoldNewWorld())
	for _, id := range []string{"req.openapi.description", "req.native.adapter"} {
		if strings.Contains(todo, "`"+id+"`") {
			t.Errorf("with MCP met %s is listed as a gap:\n%s", id, todo)
		}
	}
	if !strings.Contains(todo, "`req.mcp.server`") {
		t.Errorf("a missing requirement of the transport in use is not listed:\n%s", todo)
	}

	w := scaffoldNewWorld()
	w.in.Report.Findings = slices.DeleteFunc(w.in.Report.Findings, func(f readiness.Finding) bool { return f.RequirementID == "req.mcp.tool_list" })
	todo = gapsOf(w)
	for _, id := range []string{"req.mcp.server", "req.openapi.description", "req.native.adapter"} {
		if !strings.Contains(todo, "`"+id+"`") {
			t.Errorf("with no transport met %s is not listed as a gap:\n%s", id, todo)
		}
	}
}
