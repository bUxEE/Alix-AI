package scan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/pkg/contracts/gen/readiness"
)

const scanTestData = "../testdata"

func scanTestScan(t *testing.T, dir string, lim ScanLimits) *ScanResult {
	t.Helper()
	res, err := ScanDir(dir, ScanOptions{Limits: lim})
	if err != nil {
		t.Fatalf("ScanDir(%s): %v", dir, err)
	}
	return res
}

func scanTestFinding(t *testing.T, res *ScanResult, id string) readiness.Finding {
	t.Helper()
	for _, f := range res.Findings {
		if string(f.RequirementID) == id {
			return f
		}
	}
	t.Fatalf("no finding for %s", id)
	return readiness.Finding{}
}

// scanTestWrite writes files under a fresh directory; a content ending in a NUL-free string only.
func scanTestWrite(t *testing.T, files map[string]string) string {
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

// scanRecordingFS wraps a scanFS and records every file the scanner opens.
type scanRecordingFS struct {
	inner  scanFS
	mu     sync.Mutex
	opened []string
}

func (r *scanRecordingFS) ReadDir(dir string, limit int) ([]scanDirEntry, bool, error) {
	return r.inner.ReadDir(dir, limit)
}

func (r *scanRecordingFS) Open(name string) (scanReader, error) {
	r.mu.Lock()
	r.opened = append(r.opened, name)
	r.mu.Unlock()
	return r.inner.Open(name)
}

func (r *scanRecordingFS) Close() error { return r.inner.Close() }

func scanTestRecordingFS(t *testing.T, dir string) *scanRecordingFS {
	t.Helper()
	fsys, err := scanOpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return &scanRecordingFS{inner: fsys}
}

// scanFakeFS is an in-memory scanFS that can state any entry type, for the cases a test cannot
// build on every operating system (a junction, a socket, a hostile name).
type scanFakeFS struct {
	dirs   map[string][]scanDirEntry
	files  map[string]string
	opened []string
}

type scanFakeFile struct{ *bytes.Reader }

func (scanFakeFile) Close() error { return nil }
func (f scanFakeFile) Stat() (fs.FileInfo, error) {
	return scanFakeInfo{size: int64(f.Len())}, nil
}

type scanFakeInfo struct{ size int64 }

func (scanFakeInfo) Name() string       { return "f" }
func (i scanFakeInfo) Size() int64      { return i.size }
func (scanFakeInfo) Mode() fs.FileMode  { return 0 }
func (scanFakeInfo) ModTime() time.Time { return time.Time{} }
func (scanFakeInfo) IsDir() bool        { return false }
func (scanFakeInfo) Sys() any           { return nil }

func (f *scanFakeFS) ReadDir(dir string, limit int) ([]scanDirEntry, bool, error) {
	e, ok := f.dirs[dir]
	if !ok {
		return nil, false, errors.New("no such directory")
	}
	return e, false, nil
}

func (f *scanFakeFS) Open(name string) (scanReader, error) {
	f.opened = append(f.opened, name)
	c, ok := f.files[name]
	if !ok {
		return nil, errors.New("no such file")
	}
	return scanFakeFile{bytes.NewReader([]byte(c))}, nil
}

func (f *scanFakeFS) Close() error { return nil }

func scanHasSkip(res *scanWalkResult, path, reason string) bool {
	for _, s := range res.skipped {
		if s.Path == path && s.Reason == reason {
			return true
		}
	}
	return false
}

func scanWalkedPaths(res *scanWalkResult) []string {
	var out []string
	for _, f := range res.files {
		out = append(out, f.Path)
	}
	return out
}

func TestWalkerRespectsCapsAndSkipsAndNeverLeavesTheRoot(t *testing.T) {
	t.Run("skips dependency, build and VCS directories and says why", func(t *testing.T) {
		root := scanTestWrite(t, map[string]string{
			"keep/readme.md":            "# keep",
			"node_modules/dep/index.js": "x",
			".git/config":               "x",
			"vendor/lib/a.go":           "package a",
			"dist/out.js":               "x",
			"sub/build/output.txt":      "x",
			"sub/src/real.ts":           "export {}",
			"__pycache__/mod.pyc":       "x",
		})
		fsys := scanTestRecordingFS(t, root)
		res := scanWalk(fsys, ScanLimits{}.scanWithDefaults())
		got := scanWalkedPaths(res)
		want := []string{"keep/readme.md", "sub/src/real.ts"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("walked %v, want %v", got, want)
		}
		for _, d := range []string{".git", "node_modules", "vendor", "dist", "sub/build", "__pycache__"} {
			if !scanHasSkip(res, d, ScanSkipDependencyDir) {
				t.Errorf("%s was not reported as skipped (%v)", d, res.skipped)
			}
		}
	})

	t.Run("file count, depth, per-file and total byte caps", func(t *testing.T) {
		files := map[string]string{"a/b/c/d/deep.md": "deep", "top.md": "top"}
		for i := 0; i < 8; i++ {
			files[fmt.Sprintf("many/f%02d.md", i)] = "# f"
		}
		root := scanTestWrite(t, files)
		big := filepath.Join(root, "huge.json")
		f, err := os.Create(big)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(1 << 30); err != nil {
			t.Skipf("cannot make a sparse file here: %v", err)
		}
		f.Close()

		rec := scanTestRecordingFS(t, root)
		res := scanWalk(rec, ScanLimits{MaxFiles: 4, MaxDepth: 2, MaxFileBytes: 100, MaxTotalBytes: 10}.scanWithDefaults())
		if len(res.files) != 4 || !res.truncated || !scanHasSkip(res, "many/f04.md", ScanSkipFileCap) && !hasReason(res, ScanSkipFileCap) {
			t.Errorf("file cap: %d files, truncated=%v, skips=%v", len(res.files), res.truncated, res.skipped)
		}
		if !scanHasSkip(res, "a/b/c", ScanSkipDepthCap) {
			t.Errorf("depth cap not reported: %v", res.skipped)
		}
		for _, o := range rec.opened {
			if o == "huge.json" {
				t.Errorf("the oversized file was opened")
			}
		}

		rec2 := scanTestRecordingFS(t, root)
		res2 := scanWalk(rec2, ScanLimits{MaxFileBytes: 100, MaxTotalBytes: 20}.scanWithDefaults())
		if !scanHasSkip(res2, "huge.json", ScanSkipTooLarge) {
			t.Errorf("oversized file not reported: %v", res2.skipped)
		}
		if res2.totalBytes > 20 || !hasReason(res2, ScanSkipTotalCap) {
			t.Errorf("total cap: read %d bytes, skips=%v", res2.totalBytes, res2.skipped)
		}
		for _, o := range rec2.opened {
			if o == "huge.json" {
				t.Errorf("the oversized file was opened")
			}
		}
	})

	t.Run("directory entry cap", func(t *testing.T) {
		files := map[string]string{}
		for i := 0; i < 12; i++ {
			files[fmt.Sprintf("flood/f%02d.md", i)] = "x"
		}
		root := scanTestWrite(t, files)
		res := scanWalk(scanTestRecordingFS(t, root), ScanLimits{MaxDirEntries: 5}.scanWithDefaults())
		if len(res.files) != 5 || !res.truncated || !scanHasSkip(res, "flood", ScanSkipDirEntryCap) {
			t.Errorf("entry cap: %d files, truncated=%v, skips=%v", len(res.files), res.truncated, res.skipped)
		}
	})

	t.Run("a real symlink out of the root is not followed", func(t *testing.T) {
		outside := scanTestWrite(t, map[string]string{"outside.md": "# outside secret place"})
		root := scanTestWrite(t, map[string]string{"inside.md": "# inside"})
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		if err := os.Symlink(filepath.Join(outside, "outside.md"), filepath.Join(root, "file-link.md")); err != nil {
			t.Skipf("cannot create a file symlink here: %v", err)
		}
		res := scanTestScan(t, root, ScanLimits{})
		for _, f := range res.Files {
			if strings.Contains(f.Path, "outside") || strings.HasPrefix(f.Path, "link") || f.Path == "file-link.md" {
				t.Errorf("followed a symlink: %s", f.Path)
			}
		}
		if !scanHasSkipResult(res, "link", ScanSkipSymlink) || !scanHasSkipResult(res, "file-link.md", ScanSkipSymlink) {
			t.Errorf("symlinks not reported: %v", res.Skipped)
		}
	})

	t.Run("a real Windows junction out of the root is not followed", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("junctions exist on Windows only")
		}
		outside := scanTestWrite(t, map[string]string{"outside.md": "# outside"})
		root := scanTestWrite(t, map[string]string{"inside.md": "# inside"})
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "jct"), outside).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction here: %v %s", err, out)
		}
		res := scanTestScan(t, root, ScanLimits{})
		for _, f := range res.Files {
			if strings.HasPrefix(f.Path, "jct") || strings.Contains(f.Path, "outside") {
				t.Errorf("followed a junction: %s", f.Path)
			}
		}
		if !scanHasSkipResult(res, "jct", ScanSkipSymlink) {
			t.Errorf("junction not reported: %v", res.Skipped)
		}
	})

	t.Run("symlinks, junctions, devices and hostile names are refused by entry type", func(t *testing.T) {
		fake := &scanFakeFS{
			dirs: map[string][]scanDirEntry{
				".": {
					{Name: "a.md"},
					{Name: "junction", Mode: fs.ModeIrregular | fs.ModeDir},
					{Name: "linkdir", Mode: fs.ModeSymlink},
					{Name: "pipe", Mode: fs.ModeNamedPipe},
					{Name: "back\\slash.md"},
					{Name: "colon:name.md"},
					{Name: "ctl\x01.md"},
					{Name: strings.Repeat("x", 600) + ".md"},
				},
			},
			files: map[string]string{"a.md": "# a"},
		}
		res := scanWalk(fake, ScanLimits{}.scanWithDefaults())
		if got := scanWalkedPaths(res); len(got) != 1 || got[0] != "a.md" {
			t.Fatalf("walked %v, want only a.md", got)
		}
		for _, p := range []string{"junction", "linkdir"} {
			if !scanHasSkip(res, p, ScanSkipSymlink) {
				t.Errorf("%s not refused: %v", p, res.skipped)
			}
		}
		if !scanHasSkip(res, "pipe", ScanSkipSpecialFile) {
			t.Errorf("pipe not refused: %v", res.skipped)
		}
		unsafe := 0
		for _, s := range res.skipped {
			if s.Reason == ScanSkipUnsafeName {
				unsafe++
				if strings.ContainsAny(s.Path, "\\:\x01") {
					t.Errorf("a hostile name reached the report: %q", s.Path)
				}
			}
		}
		if unsafe != 4 {
			t.Errorf("%d unsafe names reported, want 4: %v", unsafe, res.skipped)
		}
	})

	t.Run("a drive root, a system directory and a non-directory are refused", func(t *testing.T) {
		vol := filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
		if _, err := ScanDir(vol, ScanOptions{}); err == nil {
			t.Errorf("the filesystem root %q was accepted", vol)
		}
		if runtime.GOOS == "windows" {
			if sr := os.Getenv("SystemRoot"); sr != "" {
				if _, err := ScanDir(sr, ScanOptions{}); err == nil {
					t.Errorf("SystemRoot was accepted")
				}
			}
		}
		for _, c := range []struct {
			path, goos string
			env        map[string]string
		}{
			{"/", "linux", nil}, {"/etc", "linux", nil}, {"/proc/1", "linux", nil}, {"/home", "linux", nil},
			{`C:\`, "windows", nil}, {`C:\Windows\System32`, "windows", map[string]string{"SystemRoot": `C:\Windows`}},
			{`C:\Users`, "windows", map[string]string{"SystemDrive": `C:`}},
		} {
			if scanRootRefusal(filepath.FromSlash(c.path), func(k string) string { return c.env[k] }, c.goos) == "" && !(c.goos == "windows" && runtime.GOOS != "windows") {
				t.Errorf("%s (%s) was accepted", c.path, c.goos)
			}
		}
		if why := ScanRootRefusal(t.TempDir()); why != "" {
			t.Errorf("a temp directory was refused: %s", why)
		}
		file := filepath.Join(scanTestWrite(t, map[string]string{"f.md": "x"}), "f.md")
		if _, err := ScanDir(file, ScanOptions{}); err == nil {
			t.Errorf("a file path was accepted as a root")
		}
		if _, err := ScanDir("", ScanOptions{}); err == nil {
			t.Errorf("an empty path was accepted")
		}
	})
}

func hasReason(res *scanWalkResult, reason string) bool {
	for _, s := range res.skipped {
		if s.Reason == reason {
			return true
		}
	}
	return false
}

func scanHasSkipResult(res *ScanResult, path, reason string) bool {
	for _, s := range res.Skipped {
		if s.Path == path && s.Reason == reason {
			return true
		}
	}
	return false
}

// scanVerifyEvidence is the check a verifier makes: the file is in the scan with that hash, the
// bytes on disk still hash the same, and the cited span exists.
func scanVerifyEvidence(t *testing.T, root string, res *ScanResult) {
	t.Helper()
	bySHA := map[string]string{}
	for _, f := range res.Files {
		bySHA[f.Path] = f.SHA256
	}
	for _, f := range res.Findings {
		for _, e := range f.Evidence {
			sha, ok := bySHA[string(e.Path)]
			if !ok || sha == "" || sha != string(e.FileSHA256) {
				t.Errorf("%s: evidence %s is not a scanned file with that hash", f.RequirementID, e.Path)
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(string(e.Path))))
			if err != nil {
				t.Errorf("%s: %v", e.Path, err)
				continue
			}
			n := int64(ScanLineCount(data))
			if e.LineStart < 1 || e.LineEnd < e.LineStart || e.LineEnd > n {
				t.Errorf("%s: span %d-%d outside %d lines of %s", f.RequirementID, e.LineStart, e.LineEnd, n, e.Path)
			}
		}
	}
}

type scanRefuseLoader struct{}

func (scanRefuseLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("remote schema loading is refused: %s", url)
}

var (
	scanSchemaOnce sync.Once
	scanSchemaComp *jsonschema.Compiler
	scanSchemaErr  error
)

func scanValidateFinding(t *testing.T, raw []byte) string {
	t.Helper()
	scanSchemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		c.UseLoader(scanRefuseLoader{})
		for _, area := range []string{"common", "readiness"} {
			files, err := filepath.Glob(filepath.Join("..", "..", "..", "schemas", area, "*.schema.json"))
			if err != nil || len(files) == 0 {
				scanSchemaErr = fmt.Errorf("no schemas under schemas/%s", area)
				return
			}
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					scanSchemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
				if err != nil {
					scanSchemaErr = err
					return
				}
				id, _ := doc.(map[string]any)["$id"].(string)
				if err := c.AddResource(id, doc); err != nil {
					scanSchemaErr = err
					return
				}
			}
		}
		scanSchemaComp = c
	})
	if scanSchemaErr != nil {
		t.Fatalf("schemas: %v", scanSchemaErr)
	}
	sch, err := scanSchemaComp.Compile("urn:alixai:schema:readiness:finding:v1")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return "not JSON: " + err.Error()
	}
	if err := sch.Validate(inst); err != nil {
		return err.Error()
	}
	return ""
}

func TestDetectorsFindOpenAPIMCPAndDocsInTheClinicFixture(t *testing.T) {
	root := filepath.Join(scanTestData, "clinic")
	res := scanTestScan(t, root, ScanLimits{})

	// Every finding is a valid readiness finding that satisfies the schema's evidence rules.
	cat := map[string]bool{}
	for _, f := range res.Findings {
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if msg := scanValidateFinding(t, raw); msg != "" {
			t.Errorf("%s does not validate: %s", f.RequirementID, msg)
		}
		if cat[string(f.RequirementID)] {
			t.Errorf("%s has two findings", f.RequirementID)
		}
		cat[string(f.RequirementID)] = true
		if f.Source != readiness.SourceDeterministic {
			t.Errorf("%s: source %s", f.RequirementID, f.Source)
		}
	}
	if len(res.Findings) != 33 && len(res.Findings) != 32 {
		t.Errorf("%d findings, want one per catalog requirement", len(res.Findings))
	}
	scanVerifyEvidence(t, root, res)

	want := map[string]readiness.Status{
		"req.package.manifest":                    readiness.StatusFound,
		"req.package.signatures":                  readiness.StatusFound,
		"req.package.tests":                       readiness.StatusFound,
		"req.documentation.capabilities":          readiness.StatusFound,
		"req.documentation.fields_and_validation": readiness.StatusFound,
		"req.documentation.risk_and_outcomes":     readiness.StatusFound,
		"req.contracts.conversation":              readiness.StatusFound,
		"req.contracts.terminology":               readiness.StatusFound,
		"req.mcp.server":                          readiness.StatusFound,
		"req.mcp.tool_list":                       readiness.StatusFound,
		"req.openapi.description":                 readiness.StatusFound,
		"req.surface.control_surface":             readiness.StatusFound,
		"req.data.read":                           readiness.StatusFound,
		"req.data.validate_prepare":               readiness.StatusFound,
		"req.data.commit":                         readiness.StatusFound,
		"req.data.safe_execution":                 readiness.StatusFound,
		"req.data.typed_failures":                 readiness.StatusFound,
		"req.scope.tenant_workspace":              readiness.StatusFound,
		"req.scope.principal_roles":               readiness.StatusFound,
		"req.permissions.model":                   readiness.StatusFound,
		"req.permissions.policy_files":            readiness.StatusFound,
		"req.rag.source_candidates":               readiness.StatusFound,
		"req.rag.source_declarations":             readiness.StatusFound,
		"req.rag.live_sources":                    readiness.StatusUnverified,
		"req.rag.citation_resolver":               readiness.StatusFound,
		"req.rag.scope_per_source":                readiness.StatusFound,
		"req.model.runtime_profile":               readiness.StatusFound,
		"req.model.external_egress":               readiness.StatusFound,
		"req.deployment.inspection":               readiness.StatusFound,
		"req.deployment.compose_and_ports":        readiness.StatusFound,
		"req.deployment.smoke_environment":        readiness.StatusFound,
	}
	for id, st := range want {
		if got := scanTestFinding(t, res, id).Status; got != st {
			f := scanTestFinding(t, res, id)
			t.Errorf("%s: %s, want %s (summary %v)", id, got, st, f.Summary)
		}
	}
	if f := scanTestFinding(t, res, "req.rag.synthetic_documents"); f.Status == readiness.StatusFound {
		t.Errorf("synthetic documents reported found: no fixture documents exist in this tree")
	}

	// OpenAPI: both documents, their operations, and the remote $ref counted but never resolved.
	docs := map[string]ScanAPIDoc{}
	for _, d := range res.Inventory.OpenAPI {
		docs[d.Path] = d
	}
	oa, sw := docs["api/openapi.yaml"], docs["api/legacy-swagger.json"]
	if oa.Format != "openapi-3" || len(oa.Operations) != 5 || oa.ExternalRefs != 1 {
		t.Errorf("api/openapi.yaml: %+v", oa)
	}
	if sw.Format != "swagger-2" || len(sw.Operations) != 1 || sw.Operations[0].OperationID != "findPatients" || !sw.Operations[0].Typed {
		t.Errorf("api/legacy-swagger.json: %+v", sw)
	}
	var commit ScanOperation
	for _, op := range oa.Operations {
		if op.OperationID == "commitBooking" {
			commit = op
		}
	}
	if commit.Method != "POST" || commit.Path != "/bookings/commit" || !commit.Typed || commit.Line < 1 {
		t.Errorf("commitBooking: %+v", commit)
	}

	// MCP tools, capabilities and the package root.
	if got := strings.Join(res.Inventory.MCPTools, ","); !strings.Contains(got, "availability_list") || !strings.Contains(got, "appointments_book_prepare") {
		t.Errorf("MCP tools: %s", got)
	}
	if got := strings.Join(res.Inventory.Capabilities, ","); got != "appointments.book,availability.list" || res.Inventory.PackageRoot != "package" {
		t.Errorf("capabilities %s, package root %q", got, res.Inventory.PackageRoot)
	}
	if len(res.SecretFiles) != 0 {
		t.Errorf("secret files in the clinic fixture: %v", res.SecretFiles)
	}
}

func TestDetectorsOnIncompleteAndReadmeOnlyProjects(t *testing.T) {
	inc := scanTestScan(t, filepath.Join(scanTestData, "incomplete"), ScanLimits{})
	scanVerifyEvidence(t, filepath.Join(scanTestData, "incomplete"), inc)
	for id, st := range map[string]readiness.Status{
		"req.openapi.description":          readiness.StatusFound,
		"req.package.manifest":             readiness.StatusMissing,
		"req.mcp.server":                   readiness.StatusMissing,
		"req.data.commit":                  readiness.StatusPartial,
		"req.data.validate_prepare":        readiness.StatusPartial,
		"req.deployment.compose_and_ports": readiness.StatusContradicted,
		"req.documentation.capabilities":   readiness.StatusPartial,
		"req.permissions.policy_files":     readiness.StatusMissing,
	} {
		if got := scanTestFinding(t, inc, id).Status; got != st {
			t.Errorf("incomplete %s: %s, want %s", id, got, st)
		}
	}
	if f := scanTestFinding(t, inc, "req.deployment.compose_and_ports"); len(f.Evidence) < 2 {
		t.Errorf("a contradiction cites both sides, got %d evidence entries", len(f.Evidence))
	}

	only := scanTestScan(t, filepath.Join(scanTestData, "readme-only"), ScanLimits{})
	for _, f := range only.Findings {
		if f.Status != readiness.StatusMissing || len(f.Evidence) != 0 {
			t.Errorf("README-only project: %s is %s with %d evidence entries", f.RequirementID, f.Status, len(f.Evidence))
		}
	}
	if len(only.Findings) == 0 {
		t.Fatal("no findings for a README-only project: missing requirements are still reported")
	}
}

func TestSecretFilesAreNeverOpenedAndValuesAreRedacted(t *testing.T) {
	// TEST-ONLY values, assembled at run time so no credential-shaped literal sits in the source.
	apiKey := "sk-" + "TESTONLY" + "0123456789abcdefABCDEF"
	ghTok := "ghp_" + "TESTONLY0123456789abcdefghijklmnop"
	pw := "hunter" + "2-testonly"
	jwt := "eyJ" + "TESTONLYhdr0" + "." + "TESTONLYbody0" + "." + "TESTONLYsig0"
	dbURL := "postgres://app:" + "testonlypw" + "@db.example.invalid/app"
	pem := "-----BEGIN " + "RSA PRIVATE KEY-----"
	secretValues := []string{apiKey, ghTok, pw, jwt, "testonlypw", "TESTONLYhdr0", "TESTONLYbody0"}

	root := scanTestWrite(t, map[string]string{
		".env":             "API_KEY=" + apiKey + "\nPASSWORD=" + pw + "\n",
		".env.production":  "TOKEN=" + ghTok + "\n",
		"certs/server.pem": pem + "\nTESTONLYkeybody\n",
		"certs/server.key": pem + "\n",
		"home/.ssh/id_rsa": pem + "\n",
		"credentials.json": `{"password": "` + pw + `"}`,
		"token.txt":        ghTok,
		"keystore.jks":     "TESTONLY",
		"config/app.yaml":  "database_url: " + dbURL + "\napi_key: " + apiKey + "\npassword: " + pw + "\ncredential_ref: cred:clinic\nretries: 3\n",
		"docs/setup.md":    "# Setup\n\n## Notes\n\nRisk: set the token=" + ghTok + " and Authorization: Bearer " + jwt + " before use. Confirm each step.\n",
		"src/client.ts":    "const secret = \"" + apiKey + "\";\nexport const url = \"" + dbURL + "\";\n",
	})
	rec := scanTestRecordingFS(t, root)
	res, err := scanRun(rec, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	wantSecret := []string{".env", ".env.production", "certs/server.key", "certs/server.pem", "credentials.json", "home/.ssh/id_rsa", "keystore.jks", "token.txt"}
	if strings.Join(res.SecretFiles, ",") != strings.Join(wantSecret, ",") {
		t.Errorf("secret files %v, want %v", res.SecretFiles, wantSecret)
	}
	secret := map[string]bool{}
	for _, p := range wantSecret {
		secret[p] = true
	}
	for _, o := range rec.opened {
		if secret[o] {
			t.Errorf("the secret file %s was opened", o)
		}
	}
	for _, f := range res.Files {
		if secret[f.Path] && (f.SHA256 != "" || f.Kind != scanKindSecret) {
			t.Errorf("the secret file %s was hashed or mis-typed: %+v", f.Path, f)
		}
	}
	for _, f := range res.Findings {
		for _, e := range f.Evidence {
			if secret[string(e.Path)] {
				t.Errorf("%s cites the secret file %s", f.RequirementID, e.Path)
			}
		}
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range secretValues {
		if bytes.Contains(raw, []byte(v)) {
			t.Errorf("the secret value %q reached the result", v)
		}
	}
	if bytes.Contains(raw, []byte(pem)) {
		t.Errorf("a private key header reached the result")
	}
	found := false
	for _, ex := range res.Excerpts {
		if ex.Path == "docs/setup.md" && strings.Contains(ex.Text, ScanRedacted) {
			found = true
		}
	}
	if !found {
		t.Errorf("no excerpt from docs/setup.md: %v", res.Excerpts)
	}

	for _, c := range []struct{ in, notWant string }{
		{"password = " + pw, pw},
		{"export API_KEY='" + apiKey + "'", apiKey},
		{"url: " + dbURL, "testonlypw"},
		{"Authorization: Bearer " + jwt, jwt},
		{pem + " trailing", "RSA PRIVATE"},
		{"token: " + ghTok, ghTok},
	} {
		if out := ScanRedact(c.in); strings.Contains(out, c.notWant) {
			t.Errorf("ScanRedact(%q) = %q still holds the secret", c.in, out)
		}
	}
	for _, keep := range []string{"credential_ref: cred:clinic", "max_tokens: 100", "password: ${DB_PASSWORD}", "retries: 3", "token_file: ./token.txt"} {
		if out := ScanRedact(keep); out != keep {
			t.Errorf("ScanRedact(%q) = %q, a reference or setting should be kept", keep, out)
		}
	}
}

func scanTestCopy(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestScanIsDeterministic(t *testing.T) {
	src := filepath.Join(scanTestData, "clinic")
	copyA, copyB := filepath.Join(t.TempDir(), "alpha"), filepath.Join(t.TempDir(), "much-longer-directory-name", "beta")
	for _, d := range []string{copyA, copyB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		scanTestCopy(t, src, d)
	}
	var outs [][]byte
	for _, d := range []string{src, src, copyA, copyB} {
		res := scanTestScan(t, d, ScanLimits{})
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, b)
	}
	for i := 1; i < len(outs); i++ {
		if !bytes.Equal(outs[0], outs[i]) {
			t.Fatalf("scan %d differs from scan 0 (%d vs %d bytes): the output depends on the location or on run order", i, len(outs[i]), len(outs[0]))
		}
	}
	var res ScanResult
	if err := json.Unmarshal(outs[0], &res); err != nil {
		t.Fatal(err)
	}
	if !sort.SliceIsSorted(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path }) {
		t.Errorf("files are not in a stable order")
	}
	for _, abs := range []string{copyA, copyB, filepath.ToSlash(copyA)} {
		if bytes.Contains(outs[0], []byte(strings.ReplaceAll(abs, `\`, `\\`))) || bytes.Contains(outs[2], []byte(strings.ReplaceAll(abs, `\`, `\\`))) {
			t.Errorf("an absolute path leaked into the result: %s", abs)
		}
	}
	// A changed file changes the input hash and only that file's findings.
	changed := scanTestWrite(t, map[string]string{"a.md": "# one"})
	h1 := scanTestScan(t, changed, ScanLimits{}).InputsSHA256
	if err := os.WriteFile(filepath.Join(changed, "a.md"), []byte("# two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h2 := scanTestScan(t, changed, ScanLimits{}).InputsSHA256; h1 == h2 {
		t.Errorf("the inputs hash did not change when a file changed")
	}
}

func TestHostileContentIsBounded(t *testing.T) {
	deep := strings.Repeat("[", 200000)
	root := scanTestWrite(t, map[string]string{
		"deep-openapi.json":   `{"openapi": "3.0.0", "paths": ` + deep + `}`,
		"broken-openapi.json": `{"openapi": "3.0.0", "paths": {`,
		"minified.js":         strings.Repeat("a", 500000),
		"api.yaml":            "openapi: 3.0.0\npaths:\n  /x:\n    get:\n      operationId: getX\n\tbad tab line\n",
		"loop/a/b.md":         "# x",
	})
	res := scanTestScan(t, root, ScanLimits{})
	if !scanHasSkipResult(res, "deep-openapi.json", ScanSkipTooDeep) {
		t.Errorf("deep JSON not refused: %v", res.Skipped)
	}
	if !scanHasSkipResult(res, "broken-openapi.json", ScanSkipTooDeep) && !scanHasSkipResult(res, "broken-openapi.json", ScanSkipParseError) {
		t.Errorf("broken JSON not reported: %v", res.Skipped)
	}
	f := scanTestFinding(t, res, "req.openapi.description")
	if f.Status == readiness.StatusMissing {
		t.Errorf("OpenAPI documents were recognised, the finding must not say missing: %s", f.Status)
	}
	for _, id := range []string{"req.mcp.server"} {
		if scanTestFinding(t, res, id).Status != readiness.StatusMissing {
			t.Errorf("%s should be missing", id)
		}
	}
}

func TestScannerImportsNoNetworkOrExec(t *testing.T) {
	const modulePath = "github.com/buxee/alix-ai/"
	allowedModule := map[string]bool{
		"internal/readiness/catalog":  true,
		"internal/rootguard":          true, // stdlib only: the root refusal the RAG folder provider shares
		"pkg/contracts/gen/readiness": true,
	}
	forbidden := map[string]bool{"os/exec": true, "syscall": true, "plugin": true, "unsafe": true, "net": true, "net/http": true, "crypto/tls": true}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) < 6 {
		t.Fatalf("found %d files (%v); the glob is not seeing the package", len(files), err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case forbidden[p], strings.HasPrefix(p, "net/"):
				t.Errorf("%s imports %s: the scanner opens no connection and runs no process", f, p)
			case strings.HasPrefix(p, modulePath):
				if rel := strings.TrimPrefix(p, modulePath); !allowedModule[rel] && !strings.HasPrefix(rel, "pkg/contracts") {
					t.Errorf("%s imports %s, which the module map does not allow", f, rel)
				}
			case strings.Contains(strings.SplitN(p, "/", 2)[0], "."):
				t.Errorf("%s imports third-party %s: production code is stdlib plus this module's contracts", f, p)
			}
		}
	}
	if checked < 6 {
		t.Fatalf("checked only %d production files", checked)
	}
}
