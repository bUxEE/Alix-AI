package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The files area runs the real internal/files service over the throwaway store of
// the API environment, with the host's attachment policy declared as: ephemeral
// turn context (the default) and application objects. The area's own services are
// provided here, so the inventory and OpenAPI tests build a server with it wired.
func init() {
	registerTestServices("v1_files", func(t testing.TB, e *apiEnv, s *Services) {
		cfg := files.DefaultConfig()
		cfg.Object.Enabled = true
		cfg.QuotaFiles = 6
		svc, err := files.New(files.Options{DB: e.db, Clock: e.clk, DataDir: t.TempDir(), Config: &cfg})
		if err != nil {
			t.Fatalf("files.New: %v", err)
		}
		t.Cleanup(func() { _ = svc.Close() })
		if err := Provide[FilesService](s, svc); err != nil {
			t.Fatal(err)
		}
		// The turn area asks for the same service as AttachModelFiles.
		if err := Provide[AttachModelFiles](s, svc); err != nil {
			t.Fatal(err)
		}
	})
}

// filesTestPNG is a small, real PNG.
func filesTestPNG() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

// filesPart is one part of an upload form.
type filesPart struct {
	field, filename, contentType string
	content                      []byte
}

// filesBody builds a multipart/form-data body.
func filesBody(t testing.TB, parts ...filesPart) ([]byte, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		if p.filename == "" && p.contentType == "" {
			if err := mw.WriteField(p.field, string(p.content)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, p.field, p.filename))
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(p.content)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), mw.FormDataContentType()
}

// upload posts one file with a fresh token.
func (e *apiEnv) upload(name, ctype string, content []byte, fields []filesPart, mods ...claims) *httptest.ResponseRecorder {
	e.t.Helper()
	parts := append(append([]filesPart{}, fields...), filesPart{field: "file", filename: name, contentType: ctype, content: content})
	body, ct := filesBody(e.t, parts...)
	return e.callRaw(e.token(mods...), http.MethodPost, "/v1/files", ct, body)
}

func (e *apiEnv) mustUpload(name, ctype string, content []byte, fields []filesPart, mods ...claims) conversation.FileRef {
	e.t.Helper()
	rec := e.upload(name, ctype, content, fields, mods...)
	wantStatus(e.t, rec, http.StatusCreated)
	validateSpec(e.t, "FileRef", rec.Body.Bytes())
	return decodeBody[conversation.FileRef](e.t, rec)
}

func filesPath(ref conversation.FileRef, suffix string) string {
	return "/v1/files/" + string(ref.FileID) + suffix
}

func (e *apiEnv) liveFiles() int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM files WHERE tombstoned_at IS NULL`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func filesDetail(t testing.TB, rec *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var env contracts.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	s, _ := env.Details[key].(string)
	return s
}

// The whole life of an ephemeral attachment over HTTP: upload, read the
// reference, preview, delete. Every document is checked against the OpenAPI
// document, and the reference carries no path.
func TestFilesRoundTripOverHTTP(t *testing.T) {
	e := newAPIEnv(t)
	img := filesTestPNG()
	rec := e.upload("holiday.png", "image/png", img, nil)
	wantStatus(t, rec, http.StatusCreated)
	validateSpec(t, "FileRef", rec.Body.Bytes())
	ref := decodeBody[conversation.FileRef](t, rec)
	if loc := rec.Header().Get("Location"); loc != filesPath(ref, "") {
		t.Errorf("Location = %q", loc)
	}
	if ref.Treatment == nil || *ref.Treatment != conversation.FileRefTreatmentEphemeralContext || !ref.Previewable || string(ref.Owner) == "" {
		t.Errorf("ref = %+v", ref)
	}
	for _, leak := range []string{"path", "url", "blob", "data"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), `"`+leak) {
			t.Errorf("the reference has a %q member: %s", leak, rec.Body.String())
		}
	}

	got := e.call(http.MethodGet, filesPath(ref, ""), nil)
	wantStatus(t, got, http.StatusOK)
	validateSpec(t, "FileRef", got.Body.Bytes())
	if g := decodeBody[conversation.FileRef](t, got); g.FileID != ref.FileID || g.SizeBytes != int64(len(img)) {
		t.Errorf("GET returned %+v", g)
	}

	pv := e.call(http.MethodGet, filesPath(ref, "/preview"), nil)
	wantStatus(t, pv, http.StatusOK)
	if !bytes.Equal(pv.Body.Bytes(), img) || pv.Header().Get("Content-Type") != "image/png" {
		t.Errorf("preview = %s, %d bytes", pv.Header().Get("Content-Type"), pv.Body.Len())
	}
	if csp := pv.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") || !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("the preview is not sandboxed: %q", csp)
	}
	if pv.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(pv.Header().Get("Content-Disposition"), "inline") {
		t.Errorf("preview headers: %v", pv.Header())
	}

	// An ephemeral context is not downloadable by default: the policy grants read and preview.
	dl := e.call(http.MethodGet, filesPath(ref, "/content"), nil)
	wantError(t, dl, http.StatusForbidden, contracts.ErrPermissionDenied)

	wantStatus(t, e.call(http.MethodDelete, filesPath(ref, ""), nil), http.StatusNoContent)
	wantError(t, e.call(http.MethodGet, filesPath(ref, ""), nil), http.StatusForbidden, contracts.ErrScopeViolation)
	wantError(t, e.call(http.MethodGet, filesPath(ref, "/preview"), nil), http.StatusForbidden, contracts.ErrScopeViolation)
}

// A download is always an attachment, with headers that keep the bytes inert,
// and a hostile file name cannot reach a header.
func TestFilesDownloadIsAnInertAttachment(t *testing.T) {
	e := newAPIEnv(t)
	body := []byte("# notes\n<script>alert(1)</script>\n")
	ref := e.mustUpload("x\r\nX-Injected: 1\r\n\r\n<script>.md", "text/markdown", body,
		[]filesPart{{field: "treatment", content: []byte("application_object")}})
	if ref.Treatment == nil || *ref.Treatment != conversation.FileRefTreatmentApplicationObject {
		t.Fatalf("treatment = %v", ref.Treatment)
	}
	rec := e.call(http.MethodGet, filesPath(ref, "/content"), nil)
	wantStatus(t, rec, http.StatusOK)
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Error("the download is not the upload")
	}
	h := rec.Header()
	if !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || strings.ContainsAny(h.Get("Content-Disposition"), "\r\n") {
		t.Errorf("Content-Disposition = %q", h.Get("Content-Disposition"))
	}
	if h.Get("X-Injected") != "" {
		t.Error("a file name injected a header")
	}
	if h.Get("Content-Type") != "text/markdown" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("headers = %v", h)
	}
	if h.Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length = %q", h.Get("Content-Length"))
	}
	// The reference says exactly what the policy granted.
	got := decodeBody[conversation.FileRef](t, e.call(http.MethodGet, filesPath(ref, ""), nil))
	if len(got.Permissions) != 3 {
		t.Errorf("permissions = %v", got.Permissions)
	}
}

// SEC-026 over HTTP: another tenant, workspace, principal or session gets the
// very answer an id that does not exist gets.
func TestFilesScopeOverHTTP(t *testing.T) {
	e := newAPIEnv(t)
	ref := e.mustUpload("mine.png", "image/png", filesTestPNG(), nil)
	missing := e.call(http.MethodGet, "/v1/files/fil_01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	want := wantError(t, missing, http.StatusForbidden, contracts.ErrScopeViolation)

	for name, mod := range map[string]claims{
		"another tenant":    withTenant(apiOther),
		"another workspace": withWorkspace(apiWsp2),
		"another principal": withPrincipal("usr_other"),
		"another session":   withSession("ses_other"),
	} {
		for _, suffix := range []string{"", "/preview", "/content"} {
			rec := e.call(http.MethodGet, filesPath(ref, suffix), nil, mod)
			got := wantError(t, rec, http.StatusForbidden, contracts.ErrScopeViolation)
			if got.Message != want.Message || len(got.Details) != 1 { // only the request id
				t.Errorf("%s %s: %+v differs from the answer for a missing file: %+v", name, suffix, got, want)
			}
		}
		wantError(t, e.call(http.MethodDelete, filesPath(ref, ""), nil, mod), http.StatusForbidden, contracts.ErrScopeViolation)
	}
	// None of that touched it.
	wantStatus(t, e.call(http.MethodGet, filesPath(ref, ""), nil), http.StatusOK)

	// A malformed id never reaches a lookup.
	for _, bad := range []string{"nope", "fil_short", "fil_01ARZ3NDEKTSV4RRFFQ69G5FAV0", "FIL_01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		wantError(t, e.call(http.MethodGet, "/v1/files/"+bad, nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	}
	// A quarantined application serves no file traffic.
	e.fapps.quarantine(true)
	wantError(t, e.call(http.MethodGet, filesPath(ref, ""), nil), http.StatusForbidden, contracts.ErrQuarantined)
	wantError(t, func() *httptest.ResponseRecorder {
		return e.upload("a.png", "image/png", filesTestPNG(), nil)
	}(), http.StatusForbidden, contracts.ErrQuarantined)
}

// An expired reference is 410: the token is fine, the file's time is up.
func TestFilesExpiredIs410(t *testing.T) {
	e := newAPIEnv(t)
	ref := e.mustUpload("short.png", "image/png", filesTestPNG(), nil)
	e.clk.Advance(2 * time.Hour)
	for _, suffix := range []string{"", "/preview"} {
		env := wantError(t, e.call(http.MethodGet, filesPath(ref, suffix), nil), http.StatusGone, contracts.ErrExpired)
		if env.Retryable {
			t.Error("an expired reference is not retryable")
		}
	}
	wantError(t, e.call(http.MethodDelete, filesPath(ref, ""), nil), http.StatusGone, contracts.ErrExpired)
	// Another scope is still told nothing.
	wantError(t, e.call(http.MethodGet, filesPath(ref, ""), nil, withTenant(apiOther)), http.StatusForbidden, contracts.ErrScopeViolation)

	// Compaction removes it; the reference then does not exist.
	fs, ok := Lookup[FilesService](e.svc)
	if !ok {
		t.Fatal("no files service")
	}
	svc := fs.(*files.Service)
	if _, err := svc.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Minute)
	if rep, err := svc.Compact(context.Background()); err != nil || rep.Rows != 1 || rep.Blobs != 1 {
		t.Fatalf("Compact = %+v, %v", rep, err)
	}
	wantError(t, e.call(http.MethodGet, filesPath(ref, ""), nil), http.StatusForbidden, contracts.ErrScopeViolation)
}

// SEC-019: what is declared must be what is sent, and only previewable types are
// previewed.
func TestFilesUploadRefusals(t *testing.T) {
	e := newAPIEnv(t)
	img := filesTestPNG()
	html := []byte("<!DOCTYPE html><script>alert(1)</script>")
	treat := func(v string) []filesPart { return []filesPart{{field: "treatment", content: []byte(v)}} }

	type refusal struct {
		name   string
		rec    func() *httptest.ResponseRecorder
		status int
		code   contracts.ErrCode
		reason string
	}
	postBody := func(ct string, b []byte) *httptest.ResponseRecorder {
		return e.callRaw(e.token(), http.MethodPost, "/v1/files", ct, b)
	}
	twoFiles, twoCT := filesBody(t,
		filesPart{field: "file", filename: "a.png", contentType: "image/png", content: img},
		filesPart{field: "file", filename: "b.png", contentType: "image/png", content: img})
	lateTreatment, lateCT := filesBody(t,
		filesPart{field: "file", filename: "a.png", contentType: "image/png", content: img},
		filesPart{field: "treatment", content: []byte("ephemeral_context")})
	noFile, noFileCT := filesBody(t, filesPart{field: "treatment", content: []byte("ephemeral_context")})
	stray, strayCT := filesBody(t, filesPart{field: "owner", content: []byte("usr_admin")},
		filesPart{field: "file", filename: "a.png", contentType: "image/png", content: img})

	cases := []refusal{
		{"png declared as text", func() *httptest.ResponseRecorder { return e.upload("a.txt", "text/plain", img, nil) }, 400, contracts.ErrValidationFailed, "mime_mismatch"},
		{"html declared as png", func() *httptest.ResponseRecorder { return e.upload("a.png", "image/png", html, nil) }, 400, contracts.ErrValidationFailed, "mime_mismatch"},
		{"html is not a storable type", func() *httptest.ResponseRecorder { return e.upload("a.html", "text/html", html, nil) }, 400, contracts.ErrValidationFailed, "mime_not_allowed"},
		{"svg is not a storable type", func() *httptest.ResponseRecorder {
			return e.upload("a.svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), nil)
		}, 400, contracts.ErrValidationFailed, "mime_not_allowed"},
		{"no declared type", func() *httptest.ResponseRecorder { return e.upload("a.png", "", img, nil) }, 400, contracts.ErrValidationFailed, "mime_declared"},
		{"an empty file", func() *httptest.ResponseRecorder { return e.upload("a.txt", "text/plain", nil, nil) }, 400, contracts.ErrValidationFailed, "empty"},
		{"a treatment the policy does not declare", func() *httptest.ResponseRecorder { return e.upload("a.png", "image/png", img, treat("rag_source")) }, 403, contracts.ErrPermissionDenied, "treatment_not_allowed"},
		{"a treatment that does not exist", func() *httptest.ResponseRecorder { return e.upload("a.png", "image/png", img, treat("public_cdn")) }, 400, contracts.ErrValidationFailed, ""},
		{"a JSON body", func() *httptest.ResponseRecorder { return postBody("application/json", []byte(`{"file":"x"}`)) }, 415, contracts.ErrValidationFailed, ""},
		{"no content type", func() *httptest.ResponseRecorder { return postBody("", img) }, 415, contracts.ErrValidationFailed, ""},
		{"a boundary-less form", func() *httptest.ResponseRecorder { return postBody("multipart/form-data", img) }, 415, contracts.ErrValidationFailed, ""},
		{"a form with no file", func() *httptest.ResponseRecorder { return postBody(noFileCT, noFile) }, 400, contracts.ErrValidationFailed, ""},
		{"a field that is not part of the form", func() *httptest.ResponseRecorder { return postBody(strayCT, stray) }, 400, contracts.ErrValidationFailed, ""},
		{"two files", func() *httptest.ResponseRecorder { return postBody(twoCT, twoFiles) }, 400, contracts.ErrValidationFailed, ""},
		{"a treatment after the file", func() *httptest.ResponseRecorder { return postBody(lateCT, lateTreatment) }, 400, contracts.ErrValidationFailed, ""},
		{"a body cut off before its boundary", func() *httptest.ResponseRecorder {
			return postBody(twoCT, twoFiles[:len(twoFiles)/3])
		}, 400, contracts.ErrValidationFailed, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := c.rec()
			env := wantError(t, rec, c.status, c.code)
			if c.reason != "" && filesDetail(t, rec, "reason") != c.reason {
				t.Errorf("reason = %q, want %q (%+v)", filesDetail(t, rec, "reason"), c.reason, env)
			}
			if n := e.liveFiles(); n != 0 {
				t.Errorf("%d files are live after a refused upload", n)
			}
		})
	}

	// The upload route has a body limit of its own (files.max_file_bytes and the multipart
	// framing), checked ahead of any handler; the gateway's default 64 KiB is not its limit.
	attachFilesLimit(t, 32<<10)
	big := bytes.Repeat([]byte("a"), 32<<10+attachUploadOverhead+1024)
	rec := e.upload("big.txt", "text/plain", big, nil)
	wantError(t, rec, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed)
	if e.liveFiles() != 0 {
		t.Error("an over-limit upload stored a file")
	}
}

// A tenant has a quota of live files, across its workspaces: the next upload is
// 507 until something is deleted, and another tenant is unaffected.
func TestFilesQuotaIs507(t *testing.T) {
	e := newAPIEnv(t)
	var first conversation.FileRef
	for i := 0; i < 6; i++ {
		ref := e.mustUpload("a.txt", "text/plain", []byte(fmt.Sprintf("file number %d", i)), nil, withWorkspace([]string{apiWsp, apiWsp2}[i%2]))
		if i == 0 {
			first = ref
		}
	}
	rec := e.upload("a.txt", "text/plain", []byte("one too many"), nil)
	wantError(t, rec, http.StatusInsufficientStorage, contracts.ErrResourceExhausted)
	if filesDetail(t, rec, "reason") != "quota_files" {
		t.Errorf("reason = %q", filesDetail(t, rec, "reason"))
	}
	e.mustUpload("b.txt", "text/plain", []byte("another tenant"), nil, withTenant(apiOther))
	wantStatus(t, e.call(http.MethodDelete, filesPath(first, ""), nil), http.StatusNoContent)
	e.mustUpload("c.txt", "text/plain", []byte("room again"), nil)
}

// Only a type with a safe previewer is previewed: a PDF has none.
func TestFilesPreviewOnlySafeTypes(t *testing.T) {
	e := newAPIEnv(t)
	pdf := e.mustUpload("d.pdf", "application/pdf", []byte("%PDF-1.4\n%%EOF\n"), nil)
	if pdf.Previewable {
		t.Error("a PDF is previewable")
	}
	rec := e.call(http.MethodGet, filesPath(pdf, "/preview"), nil)
	wantError(t, rec, http.StatusUnsupportedMediaType, contracts.ErrValidationFailed)
	if filesDetail(t, rec, "reason") != "not_previewable" {
		t.Errorf("reason = %q", filesDetail(t, rec, "reason"))
	}

	txt := e.mustUpload("t.txt", "text/plain", []byte("just text, <b>not</b> markup"), nil)
	pv := e.call(http.MethodGet, filesPath(txt, "/preview"), nil)
	wantStatus(t, pv, http.StatusOK)
	if ct := pv.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" || pv.Body.String() != "just text, <b>not</b> markup" {
		t.Errorf("text preview = %q %q", ct, pv.Body.String())
	}
}

// Every file route is behind the identity middleware: none answers without a
// valid token, whatever the id.
func TestFilesRoutesNeedAnIdentity(t *testing.T) {
	e := newAPIEnv(t)
	ref := e.mustUpload("a.png", "image/png", filesTestPNG(), nil)
	n := 0
	for _, route := range e.srv.AreaRoutes("v1_files") {
		method, path, _ := strings.Cut(route, " ")
		path = strings.ReplaceAll(path, "{file_id}", string(ref.FileID))
		for _, tok := range []string{"", "x.y.z"} {
			wantUnauthorized(t, e.callAs(tok, method, path, nil))
		}
		n++
	}
	if n != 5 {
		t.Errorf("the area mounts %d routes, want 5", n)
	}
}

// The handlers hold the tenant-facing part of the files service and nothing
// more: the tenant deletion, the compaction and the crash recovery work across
// tenants and are not reachable from a request.
func TestFilesServiceInterfaceIsTenantFacingOnly(t *testing.T) {
	typ := reflect.TypeOf((*FilesService)(nil)).Elem()
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	if want := "Delete,Open,Preview,Put,Stat"; strings.Join(got, ",") != want {
		t.Errorf("FilesService has %v, want %s", got, want)
	}
	// And the concrete service does have the sweeps, so the assertion above is not vacuous.
	svc := reflect.TypeOf(&files.Service{})
	for _, m := range []string{"Compact", "DeleteTenant", "Recover", "ForModel"} {
		if _, ok := svc.MethodByName(m); !ok {
			t.Errorf("files.Service has no %s", m)
		}
	}
}

// attachFilesLimit sets files.max_file_bytes of the registered "files" section, which is
// what the upload route reads its body limit from, for one test.
func attachFilesLimit(t testing.TB, n int64) {
	t.Helper()
	sec := files.ConfigSection()
	old := sec.MaxFileBytes
	sec.MaxFileBytes = n
	t.Cleanup(func() { sec.MaxFileBytes = old })
}

// Uploads need more than the gateway's 64 KiB, and nothing else does: the upload route has a
// limit of its own, from the files configuration and never above the gateway's ceiling, and
// every other route keeps gateway.max_body_bytes. A declared length over it is refused before
// a byte of the body is read.
func TestUploadLimitIsPerRouteAndAnswers413BeforeReading(t *testing.T) {
	e := newAPIEnv(t)
	attachFilesLimit(t, 200<<10)
	limit := int64(200<<10 + attachUploadOverhead)

	t.Run("a file over the gateway default and under the upload limit is stored", func(t *testing.T) {
		body := bytes.Repeat([]byte("a"), 150<<10)
		ref := e.mustUpload("notes.txt", "text/plain", body, nil)
		if ref.SizeBytes != int64(len(body)) {
			t.Fatalf("stored %d bytes of %d", ref.SizeBytes, len(body))
		}
	})

	t.Run("every other route keeps the gateway default", func(t *testing.T) {
		big := `{"text":"` + strings.Repeat("a", 70<<10) + `"}`
		rec := e.call(http.MethodPost, sessionsPath(apiSess)+"/turns", big)
		wantError(t, rec, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed)
		if got := filesLimitDetail(t, rec); got != float64(DefaultMaxBodyBytes) {
			t.Errorf("a turn's limit = %v, want the gateway's %d", got, DefaultMaxBodyBytes)
		}
		// Not even the same path with another method: only POST /v1/files is declared.
		req := httptest.NewRequest(http.MethodPut, "/v1/files", &failOnRead{t: t})
		req.ContentLength = DefaultMaxBodyBytes + 1
		rec = httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		wantError(t, rec, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed)
	})

	t.Run("a declared length over the upload limit is a 413 before the body is read", func(t *testing.T) {
		before := e.liveFiles()
		req := httptest.NewRequest(http.MethodPost, "/v1/files", &failOnRead{t: t})
		req.ContentLength = limit + 1
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		req.Header.Set("Authorization", "Bearer "+e.token())
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		wantError(t, rec, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed)
		if got := filesLimitDetail(t, rec); got != float64(limit) {
			t.Errorf("details.limit = %v, want the upload limit %d", got, limit)
		}
		if e.liveFiles() != before {
			t.Error("an over-limit upload stored a file")
		}
	})

	t.Run("a body that lies about its length is cut at the limit", func(t *testing.T) {
		before := e.liveFiles()
		body, ct := filesBody(t, filesPart{field: "file", filename: "big.txt", contentType: "text/plain", content: bytes.Repeat([]byte("a"), int(limit))})
		req := httptest.NewRequest(http.MethodPost, "/v1/files", io.MultiReader(bytes.NewReader(body))) // length unknown
		req.ContentLength = -1
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer "+e.token())
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		wantError(t, rec, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed)
		if e.liveFiles() != before {
			t.Error("a cut upload stored a file")
		}
	})

	t.Run("no configuration lifts it above the gateway's ceiling", func(t *testing.T) {
		attachFilesLimit(t, 1<<30)
		if got := attachUploadLimit(); got != maxMaxBodyBytes {
			t.Fatalf("limit = %d, want the ceiling %d", got, maxMaxBodyBytes)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/files", &failOnRead{t: t})
		req.ContentLength = maxMaxBodyBytes + 1
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		wantError(t, rec, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed)
		if got := filesLimitDetail(t, rec); got != float64(maxMaxBodyBytes) {
			t.Errorf("details.limit = %v, want %d", got, maxMaxBodyBytes)
		}
	})
}

func filesLimitDetail(t testing.TB, rec *httptest.ResponseRecorder) float64 {
	t.Helper()
	var env contracts.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	v, _ := env.Details["limit"].(float64)
	return v
}
