package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// env is a Service over a throwaway database and data directory.
type env struct {
	t   *testing.T
	svc *Service
	db  *store.DB
	clk *clock.Fake
	dir string
	rag *fakeRAG
}

// build is what an option adjusts before the Service is made.
type build struct {
	opts   Options
	cfg    Config
	useRAG bool
}

type envOpt func(*build)

func withConfig(f func(*Config)) envOpt { return func(b *build) { f(&b.cfg) } }

func withVision(on bool) envOpt {
	return func(b *build) { b.opts.Vision = VisionFunc(func(context.Context) bool { return on }) }
}

func withPolicy(p PolicySource) envOpt { return func(b *build) { b.opts.Policy = p } }

// withRAG wires the env's fake RAG ingestion.
func withRAG() envOpt { return func(b *build) { b.useRAG = true } }

// ragOn enables the rag_source treatment for text files.
func ragOn(c *Config) {
	c.RAGSource.Enabled = true
}

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	return newEnvAt(t, t0, opts...)
}

func newEnvAt(t *testing.T, start time.Time, opts ...envOpt) *env {
	t.Helper()
	b := build{cfg: DefaultConfig(), opts: Options{DataDir: t.TempDir()}}
	for _, f := range opts {
		f(&b)
	}
	e := &env{t: t, clk: clock.NewFake(start), dir: b.opts.DataDir, rag: &fakeRAG{}}
	e.db = storetest.Open(t, nil)
	o := b.opts
	o.DB, o.Clock, o.Config = e.db, e.clk, &b.cfg
	if b.useRAG {
		o.RAG = e.rag
	}
	svc, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	e.svc = svc
	return e
}

func (e *env) put(scope contracts.Scope, name, mime string, content []byte, opts ...func(*PutInput)) (conversation.FileRef, error) {
	e.t.Helper()
	in := PutInput{Name: name, DeclaredMIME: mime, Content: bytes.NewReader(content)}
	for _, f := range opts {
		f(&in)
	}
	return e.svc.Put(context.Background(), scope, in)
}

func (e *env) mustPut(scope contracts.Scope, name, mime string, content []byte, opts ...func(*PutInput)) conversation.FileRef {
	e.t.Helper()
	ref, err := e.put(scope, name, mime, content, opts...)
	if err != nil {
		e.t.Fatalf("Put(%s): %v", name, err)
	}
	return ref
}

func asTreatment(t Treatment) func(*PutInput) { return func(in *PutInput) { in.Treatment = t } }

// scopeOf builds a valid scope.
func scopeOf(app, tenant, workspace, principal, session string) contracts.Scope {
	s := storetest.Scope(app, tenant, workspace)
	s.PrincipalID, s.SessionID = principal, session
	return s
}

var (
	scopeA  = scopeOf("app_demo", "ten_a", "wsp_1", "usr_1", "ses_1")
	scopeB  = scopeOf("app_demo", "ten_b", "wsp_1", "usr_1", "ses_1")
	scopeA2 = scopeOf("app_demo", "ten_a", "wsp_2", "usr_1", "ses_1")
)

// ---- content ----

func pngBytes() []byte {
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func jpegBytes() []byte {
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	_ = jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func gifBytes() []byte { return append([]byte("GIF89a"), make([]byte, 20)...) }

func webpBytes() []byte {
	return append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 20)...)
}

func pdfBytes() []byte { return []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n") }

func textBytes(s string) []byte { return []byte(s) }

func docxBytes(t testing.TB) []byte { return ooxml(t, "word/document.xml") }

func ooxml(t testing.TB, part string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, n := range []string{"[Content_Types].xml", part} {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("<x/>"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func plainZip(t testing.TB) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("readme.txt")
	_, _ = w.Write([]byte("hello"))
	_ = zw.Close()
	return b.Bytes()
}

const (
	docxMIME = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	xlsxMIME = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// ---- assertions ----

func wantCode(t testing.TB, err error, code contracts.ErrCode) *contracts.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s, want %s: %v", got, code, err)
	}
	var ce *contracts.Error
	if !asError(err, &ce) {
		t.Fatalf("%v is not a contracts.Error", err)
	}
	return ce
}

func asError(err error, target **contracts.Error) bool {
	for err != nil {
		if e, ok := err.(*contracts.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func wantReason(t testing.TB, err error, code contracts.ErrCode, reason string) {
	t.Helper()
	ce := wantCode(t, err, code)
	if got, _ := ce.Details["reason"].(string); got != reason {
		t.Fatalf("reason = %q, want %q (%v)", got, reason, err)
	}
}

// diskBlobs lists the blobs on disk (not the upload directory).
func diskBlobs(t testing.TB, dir string) []string {
	t.Helper()
	var out []string
	root := filepath.Join(dir, "files")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() || strings.HasPrefix(rel, tmpDirName) {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tmpFiles(t testing.TB, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(dir, "files", tmpDirName))
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func (e *env) rowCount() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM files`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) blobKeyOf(id string) string {
	e.t.Helper()
	var k string
	if err := e.db.QueryRowContext(context.Background(), `SELECT blob_key FROM files WHERE id = ?`, id).Scan(&k); err != nil {
		e.t.Fatal(err)
	}
	return k
}

// ---- FileRef schema ----

var (
	schemaOnce sync.Once
	schemaFile *jsonschema.Schema
	schemaErr  error
)

// validateFileRef checks raw against schemas/conversation/file-ref.
func validateFileRef(t testing.TB, raw []byte) {
	t.Helper()
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		for _, area := range []string{"common", "actions", "conversation", "memory", "analyst"} {
			files, _ := filepath.Glob(filepath.Join("..", "..", "schemas", area, "*.schema.json"))
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					schemaErr = err
					return
				}
				doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
				if err != nil {
					schemaErr = err
					return
				}
				if err := c.AddResource(doc.(map[string]any)["$id"].(string), doc); err != nil {
					schemaErr = err
					return
				}
			}
		}
		schemaFile, schemaErr = c.Compile("urn:alixai:schema:conversation:file-ref:v1")
	})
	if schemaErr != nil {
		t.Fatalf("file-ref schema: %v", schemaErr)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schemaFile.Validate(inst); err != nil {
		t.Fatalf("the FileRef does not match its schema: %v\n%s", err, raw)
	}
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- a RAG ingestion fake ----

type fakeRAG struct {
	mu           sync.Mutex
	registered   []RAGFile
	removed      []string
	failRegister error
	failRemove   error
}

func (f *fakeRAG) RegisterFile(_ context.Context, _ contracts.Scope, file RAGFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRegister != nil {
		return f.failRegister
	}
	f.registered = append(f.registered, file)
	return nil
}

func (f *fakeRAG) RemoveFile(_ context.Context, _ Location, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRemove != nil {
		return f.failRemove
	}
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeRAG) counts() (registered, removed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registered), len(f.removed)
}

// hashOf is the reference's content hash, or "".
func hashOf(r conversation.FileRef) string {
	if r.ContentHash == nil {
		return ""
	}
	return *r.ContentHash
}
