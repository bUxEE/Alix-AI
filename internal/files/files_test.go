package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

var ctx = context.Background()

func readAll(t testing.TB, c *Content) []byte {
	t.Helper()
	defer c.Body.Close()
	b, err := io.ReadAll(c.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// SEC-026: a reference is an opaque id. Nothing a caller, an SDK or a model can
// see of a file (the FileRef, the model's descriptor, an error) holds a path, a
// URL, the data directory or the storage key.
func TestFileRefOpaqueNoPath(t *testing.T) {
	e := newEnv(t)
	content := textBytes("quarterly numbers: 1, 2, 3\n")
	ref := e.mustPut(scopeA, `..\..\secret/quarterly report.txt`, "text/plain", content)

	raw := mustJSON(t, ref)
	validateFileRef(t, raw)
	if !ValidID(string(ref.FileID)) {
		t.Fatalf("file id %q is not fil_ plus a ULID", ref.FileID)
	}
	if ref.Name != "quarterly report.txt" {
		t.Errorf("name = %q: the client's directories were not stripped", ref.Name)
	}

	// The keys of the reference are exactly the schema's: no path, url, location or key.
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range generic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"content_hash", "expires_at", "file_id", "mime_type", "name", "origin", "owner", "permissions", "previewable", "size_bytes", "treatment"}
	if diff := cmp.Diff(want, keys); diff != "" {
		t.Errorf("FileRef members (-want +got):\n%s", diff)
	}

	models, err := e.svc.ForModel(ctx, scopeA, []string{string(ref.FileID)})
	if err != nil {
		t.Fatal(err)
	}
	modelJSON := string(mustJSON(t, models))

	key := e.blobKeyOf(string(ref.FileID))
	tenantDir := tenantDirName(scopeA.ApplicationID, scopeA.TenantID)
	forbidden := []string{e.dir, filepath.ToSlash(e.dir), filepath.Base(e.dir), key, tenantDir, key[:2] + "/", "\\\\", `"path"`, `"url"`, `"location"`, `"storage`, "blob"}
	for _, doc := range []string{string(raw), modelJSON} {
		for _, f := range forbidden {
			if strings.Contains(doc, f) {
				t.Errorf("a document handed to a client or model contains %q:\n%s", f, doc)
			}
		}
	}
	if strings.Contains(string(ref.FileID), key[:8]) {
		t.Error("the file id is derived from the storage key")
	}

	// A storage fault is reported without the location either: remove the blob
	// behind the reference and open it.
	for _, p := range diskBlobs(t, e.dir) {
		if err := os.Remove(filepath.Join(e.dir, "files", filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.svc.Open(ctx, scopeA, string(ref.FileID), PermRead)
	wantCode(t, err, contracts.ErrUnavailable)
	for _, s := range []string{err.Error(), contracts.CodeOf(err).String()} {
		if strings.Contains(s, e.dir) || strings.Contains(s, filepath.ToSlash(e.dir)) || strings.Contains(s, key) {
			t.Errorf("an error exposes where files are kept: %q", s)
		}
	}
}

// SEC-026: a reference resolves only for the application, tenant, workspace,
// principal and session it was minted for, and a failed resolution says nothing
// about whether the file exists.
func TestFileRefScopeEnforced(t *testing.T) {
	e := newEnv(t, withConfig(func(c *Config) {
		c.Object.Enabled = true
		c.Object.Visibility = string(VisibilityWorkspace)
	}))
	private := e.mustPut(scopeA, "mine.txt", "text/plain", textBytes("private to usr_1 in ses_1"))
	id := string(private.FileID)

	other := func(mut func(*contracts.Scope)) contracts.Scope {
		s := scopeA.Clone()
		mut(&s)
		return s
	}
	callers := map[string]contracts.Scope{
		"another tenant":                   scopeB,
		"another application":              other(func(s *contracts.Scope) { s.ApplicationID = "app_other" }),
		"another workspace":                scopeA2,
		"another principal":                other(func(s *contracts.Scope) { s.PrincipalID = "usr_2" }),
		"another session (ephemeral file)": other(func(s *contracts.Scope) { s.SessionID = "ses_2" }),
	}
	ops := map[string]func(contracts.Scope, string) error{
		"Stat":     func(s contracts.Scope, id string) error { _, err := e.svc.Stat(ctx, s, id); return err },
		"Open":     func(s contracts.Scope, id string) error { _, err := e.svc.Open(ctx, s, id, PermRead); return err },
		"Preview":  func(s contracts.Scope, id string) error { _, err := e.svc.Preview(ctx, s, id); return err },
		"Delete":   func(s contracts.Scope, id string) error { return e.svc.Delete(ctx, s, id) },
		"ForModel": func(s contracts.Scope, id string) error { _, err := e.svc.ForModel(ctx, s, []string{id}); return err },
	}
	// The answer to "another scope" and to "no such id" is one and the same.
	missing := "fil_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for op, call := range ops {
		ref := call(scopeA, missing)
		refErr := wantCode(t, ref, contracts.ErrScopeViolation)
		for who, sc := range callers {
			err := call(sc, id)
			ce := wantCode(t, err, contracts.ErrScopeViolation)
			if ce.Message != refErr.Message || len(ce.Details) != 0 {
				t.Errorf("%s as %s: %+v differs from the answer for an id that does not exist: %+v", op, who, ce, refErr)
			}
		}
		// A malformed id is no different.
		wantCode(t, call(scopeA, "../../etc/passwd"), contracts.ErrScopeViolation)
	}
	// The owner, in the right session, resolves it.
	if _, err := e.svc.Stat(ctx, scopeA, id); err != nil {
		t.Fatalf("the owner cannot resolve the file: %v", err)
	}
	// A scope that is not valid resolves nothing, and says why.
	bad := scopeA.Clone()
	bad.TenantID = ""
	for op, call := range ops {
		if err := call(bad, id); contracts.CodeOf(err) != contracts.ErrScopeMissing {
			t.Errorf("%s with no tenant = %v, want scope_missing", op, err)
		}
	}

	// A file the host made workspace-visible is usable by the workspace's other
	// principals (never another workspace's), and deletable only by its owner.
	shared := e.mustPut(scopeA, "shared.txt", "text/plain", textBytes("shared in wsp_1"), asTreatment(TreatmentObject))
	sid := string(shared.FileID)
	peer := other(func(s *contracts.Scope) { s.PrincipalID, s.SessionID = "usr_2", "ses_9" })
	if _, err := e.svc.Stat(ctx, peer, sid); err != nil {
		t.Errorf("a workspace peer cannot resolve a workspace-visible file: %v", err)
	}
	if _, err := e.svc.Open(ctx, peer, sid, PermDownload); err != nil {
		t.Errorf("a workspace peer cannot download it: %v", err)
	}
	wantReason(t, e.svc.Delete(ctx, peer, sid), contracts.ErrPermissionDenied, "not_owner")
	wantCode(t, ops["Stat"](scopeA2, sid), contracts.ErrScopeViolation)
	wantCode(t, ops["Stat"](scopeB, sid), contracts.ErrScopeViolation)
	if err := e.svc.Delete(ctx, scopeA, sid); err != nil {
		t.Fatal(err)
	}
	wantCode(t, ops["Stat"](peer, sid), contracts.ErrScopeViolation)
}

// SEC-026: a reference stops resolving at its expiry, for every use.
func TestExpiredRefRejected(t *testing.T) {
	e := newEnv(t)
	ref := e.mustPut(scopeA, "short.txt", "text/plain", textBytes("gone soon"))
	id := string(ref.FileID)
	if want := t0.Add(DefaultEphemeralTTL).UTC().Format(timestampLayout); string(ref.ExpiresAt) != want {
		t.Fatalf("expires_at = %s, want %s", ref.ExpiresAt, want)
	}

	e.clk.Advance(DefaultEphemeralTTL - time.Millisecond)
	if _, err := e.svc.Stat(ctx, scopeA, id); err != nil {
		t.Fatalf("a reference one millisecond before its expiry does not resolve: %v", err)
	}
	e.clk.Advance(time.Millisecond) // exactly at expires_at
	uses := map[string]func() error{
		"Stat":     func() error { _, err := e.svc.Stat(ctx, scopeA, id); return err },
		"Open":     func() error { _, err := e.svc.Open(ctx, scopeA, id, PermRead); return err },
		"Preview":  func() error { _, err := e.svc.Preview(ctx, scopeA, id); return err },
		"ForModel": func() error { _, err := e.svc.ForModel(ctx, scopeA, []string{id}); return err },
	}
	for name, use := range uses {
		wantReason(t, use(), contracts.ErrExpired, "expired")
		_ = name
	}
	// Only the owner is told it expired: another scope still learns nothing.
	wantCode(t, func() error { _, err := e.svc.Stat(ctx, scopeB, id); return err }(), contracts.ErrScopeViolation)

	// And it is gone for good: compaction tombstones it, then removes its bytes.
	rep, err := e.svc.Compact(ctx)
	if err != nil || rep.Expired != 1 {
		t.Fatalf("Compact = %+v, %v; want the expired reference tombstoned", rep, err)
	}
	if len(diskBlobs(t, e.dir)) != 1 {
		t.Fatal("the blob was removed before the tombstone grace elapsed")
	}
	e.clk.Advance(DefaultCompactGrace)
	rep, err = e.svc.Compact(ctx)
	if err != nil || rep.Rows != 1 || rep.Blobs != 1 {
		t.Fatalf("second Compact = %+v, %v", rep, err)
	}
	if n := len(diskBlobs(t, e.dir)); n != 0 || e.rowCount() != 0 {
		t.Fatalf("after compaction: %d blobs, %d rows, want none", n, e.rowCount())
	}
}

// SEC-019: the declared type must be what the bytes are, and nothing active is
// ever stored. A refused upload leaves nothing behind.
func TestMIMESniffMismatchRejected(t *testing.T) {
	e := newEnv(t, withConfig(func(c *Config) { c.Ephemeral.AllowedMIME = append(c.Ephemeral.AllowedMIME, docxMIME, xlsxMIME) }))
	html := textBytes("<!DOCTYPE html><html><script>alert(1)</script></html>")
	svg := textBytes(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)
	cases := []struct {
		name     string
		declared string
		content  []byte
		reason   string
	}{
		{"png declared as text", "text/plain", pngBytes(), "mime_mismatch"},
		{"text declared as png", "image/png", textBytes("not an image at all, just words"), "mime_mismatch"},
		{"html declared as png", "image/png", html, "mime_mismatch"},
		{"html declared as plain text", "text/plain", html, "mime_mismatch"},
		{"html declared as markdown", "text/markdown", html, "mime_mismatch"},
		{"xml declared as csv", "text/csv", textBytes(`<?xml version="1.0"?><a/>`), "mime_mismatch"},
		{"jpeg declared as png", "image/png", jpegBytes(), "mime_mismatch"},
		{"png declared as jpeg", "image/jpeg", pngBytes(), "mime_mismatch"},
		{"text declared as pdf", "application/pdf", textBytes("%PDF is not at the start"), "mime_mismatch"},
		{"pdf declared as text", "text/plain", pdfBytes(), "mime_mismatch"},
		{"binary declared as text", "text/plain", append([]byte("abc"), 0, 1, 2, 3, 0, 5), "mime_mismatch"},
		{"prose declared as json", "application/json", textBytes("hello"), "mime_mismatch"},
		{"zip declared as docx", docxMIME, plainZip(t), "mime_mismatch"},
		{"docx declared as xlsx", xlsxMIME, docxBytes(t), "mime_mismatch"},
		{"not a zip declared as docx", docxMIME, textBytes("PK but not really a zip file at all"), "mime_mismatch"},
		{"svg is not stored", "image/svg+xml", svg, "mime_not_allowed"},
		{"html is not stored", "text/html", html, "mime_not_allowed"},
		{"script is not stored", "application/javascript", textBytes("alert(1)"), "mime_not_allowed"},
		{"unknown type", "application/x-msdownload", []byte("MZ\x90\x00"), "mime_not_allowed"},
		{"no declared type", "", textBytes("hello"), "mime_declared"},
		{"malformed declared type", "text plain", textBytes("hello"), "mime_declared"},
		{"empty file", "text/plain", nil, "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.put(scopeA, "x", c.declared, c.content)
			wantReason(t, err, contracts.ErrValidationFailed, c.reason)
			if n := e.rowCount(); n != 0 {
				t.Errorf("%d rows after a refused upload", n)
			}
			if n := len(diskBlobs(t, e.dir)); n != 0 {
				t.Errorf("%d blobs after a refused upload", n)
			}
			if n := tmpFiles(t, e.dir); n != 0 {
				t.Errorf("%d upload files left behind", n)
			}
		})
	}

	// What the bytes are is what is accepted, parameters and case of the declared
	// type notwithstanding.
	good := []struct {
		declared string
		content  []byte
	}{
		{"image/png", pngBytes()}, {"IMAGE/JPEG", jpegBytes()}, {"image/gif", gifBytes()}, {"image/webp", webpBytes()},
		{"application/pdf", pdfBytes()}, {"text/plain; charset=utf-8", textBytes("plain words")},
		{"text/markdown", textBytes("# title\n\nbody")}, {"text/csv", textBytes("a,b\n1,2\n")},
		{"application/json", textBytes(`{"a": 1}`)},
	}
	for _, g := range good {
		if _, err := e.put(scopeA, "ok", g.declared, g.content); err != nil {
			t.Errorf("a genuine %s was refused: %v", g.declared, err)
		}
	}

	// The policy narrows it further: a type it does not list is refused though the bytes match.
	narrow := newEnv(t, withConfig(func(c *Config) { c.Ephemeral.AllowedMIME = []string{"image/*"} }))
	if _, err := narrow.put(scopeA, "a.png", "image/png", pngBytes()); err != nil {
		t.Errorf("image/* did not allow a png: %v", err)
	}
	_, err := narrow.put(scopeA, "a.txt", "text/plain", textBytes("hello"))
	wantReason(t, err, contracts.ErrValidationFailed, "mime_not_allowed")

	// An Office package is opened to its directory.
	office := newEnv(t, withConfig(func(c *Config) { c.Ephemeral.AllowedMIME = append(c.Ephemeral.AllowedMIME, docxMIME, xlsxMIME) }))
	if _, err := office.put(scopeA, "a.docx", docxMIME, docxBytes(t)); err != nil {
		t.Errorf("a genuine docx was refused: %v", err)
	}
	if _, err := office.put(scopeA, "a.xlsx", xlsxMIME, ooxml(t, "xl/workbook.xml")); err != nil {
		t.Errorf("a genuine xlsx was refused: %v", err)
	}
}

// Master plan 14.1: how an attachment is treated is the host's declared policy.
// The default policy is ephemeral turn context only: short-lived, readable and
// previewable by its uploader, bound to the session, never a RAG source.
func TestAttachmentPolicyEphemeral(t *testing.T) {
	e := newEnv(t, withRAG())
	ref := e.mustPut(scopeA, "note.txt", "text/plain", textBytes("remember: the invoice is due Friday"))

	if ref.Treatment == nil || *ref.Treatment != TreatmentEphemeral {
		t.Fatalf("treatment = %v, want ephemeral_context", ref.Treatment)
	}
	if ref.Origin != OriginUpload {
		t.Errorf("origin = %s", ref.Origin)
	}
	if want := []Permission{PermRead, PermPreview}; !cmp.Equal(ref.Permissions, want) {
		t.Errorf("permissions = %v, want %v", ref.Permissions, want)
	}
	if want := t0.Add(DefaultEphemeralTTL).Format(timestampLayout); string(ref.ExpiresAt) != want {
		t.Errorf("expires_at = %s, want %s", ref.ExpiresAt, want)
	}
	if !ref.Previewable {
		t.Error("a text file is not previewable")
	}
	if string(ref.Owner) != scopeA.PrincipalID {
		t.Errorf("owner = %s", ref.Owner)
	}
	// Download is not granted to an ephemeral context; reading and preview are.
	id := string(ref.FileID)
	wantReason(t, func() error { _, err := e.svc.Open(ctx, scopeA, id, PermDownload); return err }(), contracts.ErrPermissionDenied, "permission")
	if c, err := e.svc.Open(ctx, scopeA, id, PermRead); err != nil {
		t.Fatal(err)
	} else if got := readAll(t, c); string(got) != "remember: the invoice is due Friday" {
		t.Errorf("content = %q", got)
	}
	if p, err := e.svc.Preview(ctx, scopeA, id); err != nil {
		t.Fatal(err)
	} else {
		p.Body.Close()
	}

	// It belongs to the session that uploaded it.
	later := scopeA.Clone()
	later.SessionID = "ses_next"
	wantCode(t, func() error { _, err := e.svc.Stat(ctx, later, id); return err }(), contracts.ErrScopeViolation)

	// It reaches the model as turn context, as data: the opaque id, the type and the bounded text.
	models, err := e.svc.ForModel(ctx, scopeA, []string{id})
	if err != nil || len(models) != 1 || models[0].Text != "remember: the invoice is due Friday" || models[0].FileID != id {
		t.Fatalf("ForModel = %+v, %v", models, err)
	}

	// It is not a RAG source, and the host did not declare any other treatment, so asking
	// for one is refused outright and stores nothing.
	rows, blobs := e.rowCount(), len(diskBlobs(t, e.dir))
	for _, tr := range []Treatment{TreatmentRAGSource, TreatmentObject} {
		_, err := e.put(scopeA, "x.txt", "text/plain", textBytes("hello"), asTreatment(tr))
		wantReason(t, err, contracts.ErrPermissionDenied, "treatment_not_allowed")
	}
	_, err = e.put(scopeA, "x.txt", "text/plain", textBytes("hello"), asTreatment("everything"))
	wantReason(t, err, contracts.ErrValidationFailed, "treatment")
	if e.rowCount() != rows || len(diskBlobs(t, e.dir)) != blobs {
		t.Error("a refused treatment stored something")
	}
	if reg, rem := e.rag.counts(); reg != 0 || rem != 0 {
		t.Errorf("the RAG ingestion was called for an ephemeral file (%d registered, %d removed)", reg, rem)
	}

	// Past its time it is gone, and compaction removes the bytes.
	e.clk.Advance(DefaultEphemeralTTL + DefaultCompactGrace)
	wantReason(t, func() error { _, err := e.svc.Stat(ctx, scopeA, id); return err }(), contracts.ErrExpired, "expired")
	if _, err := e.svc.Compact(ctx); err != nil { // tombstones it
		t.Fatal(err)
	}
	e.clk.Advance(DefaultCompactGrace)
	if _, err := e.svc.Compact(ctx); err != nil { // removes it
		t.Fatal(err)
	}
	if len(diskBlobs(t, e.dir)) != 0 || e.rowCount() != 0 {
		t.Errorf("ephemeral file survived its expiry: %d blobs, %d rows", len(diskBlobs(t, e.dir)), e.rowCount())
	}
}

// The policy fails closed: no declared policy, an unreadable one, a rule that
// allows nothing, and a RAG source with no ingestion all store nothing.
func TestAttachmentPolicyFailsClosed(t *testing.T) {
	text := textBytes("hello")
	t.Run("a policy that names nothing", func(t *testing.T) {
		e := newEnv(t, withPolicy(PolicyFunc(func(context.Context, contracts.Scope) (Policy, error) { return Policy{}, nil })))
		_, err := e.put(scopeA, "a.txt", "text/plain", text)
		wantReason(t, err, contracts.ErrPermissionDenied, "no_policy")
	})
	t.Run("a policy that cannot be read", func(t *testing.T) {
		e := newEnv(t, withPolicy(PolicyFunc(func(context.Context, contracts.Scope) (Policy, error) {
			return Policy{}, errors.New("settings store is down")
		})))
		_, err := e.put(scopeA, "a.txt", "text/plain", text)
		wantCode(t, err, contracts.ErrUnavailable)
	})
	t.Run("a rule that allows no type", func(t *testing.T) {
		e := newEnv(t, withPolicy(PolicyFunc(func(context.Context, contracts.Scope) (Policy, error) {
			return Policy{Default: TreatmentEphemeral, Rules: map[Treatment]Rule{TreatmentEphemeral: {}}}, nil
		})))
		_, err := e.put(scopeA, "a.txt", "text/plain", text)
		wantCode(t, err, contracts.ErrPermissionDenied)
	})
	t.Run("a rule with a permission that does not exist", func(t *testing.T) {
		e := newEnv(t, withPolicy(PolicyFunc(func(context.Context, contracts.Scope) (Policy, error) {
			return Policy{Default: TreatmentEphemeral, Rules: map[Treatment]Rule{TreatmentEphemeral: {AllowedMIME: []string{"text/plain"}, Permissions: []Permission{"write"}}}}, nil
		})))
		_, err := e.put(scopeA, "a.txt", "text/plain", text)
		wantCode(t, err, contracts.ErrInternal)
	})
	t.Run("every treatment disabled", func(t *testing.T) {
		e := newEnv(t, withConfig(func(c *Config) { c.Ephemeral.Enabled = false }))
		_, err := e.put(scopeA, "a.txt", "text/plain", text)
		wantReason(t, err, contracts.ErrPermissionDenied, "no_policy")
	})
	t.Run("a RAG source with no ingestion wired", func(t *testing.T) {
		e := newEnv(t, withConfig(ragOn))
		_, err := e.put(scopeA, "a.txt", "text/plain", text, asTreatment(TreatmentRAGSource))
		wantReason(t, err, contracts.ErrUnavailable, "rag_unavailable")
		if e.rowCount() != 0 || len(diskBlobs(t, e.dir)) != 0 {
			t.Error("a refused RAG source stored something")
		}
	})
	t.Run("a scope that is not valid", func(t *testing.T) {
		e := newEnv(t)
		bad := scopeA.Clone()
		bad.WorkspaceID = ""
		_, err := e.put(bad, "a.txt", "text/plain", text)
		wantCode(t, err, contracts.ErrScopeMissing)
	})
}

// Master plan 14.1 and 3 rule 14: a rag_source file is registered with the RAG
// ingestion, withdrawn from it before it is deleted or expires, and never
// stays as a source the ingestion refused.
func TestRAGSourceTreatment(t *testing.T) {
	e := newEnv(t, withRAG(), withConfig(ragOn))
	ref := e.mustPut(scopeA, "handbook.txt", "text/plain", textBytes("rules"), asTreatment(TreatmentRAGSource))
	if ref.Treatment == nil || *ref.Treatment != TreatmentRAGSource {
		t.Fatalf("treatment = %v", ref.Treatment)
	}
	if want := []Permission{PermRead}; !cmp.Equal(ref.Permissions, want) {
		t.Errorf("permissions = %v, want %v", ref.Permissions, want)
	}
	if reg, _ := e.rag.counts(); reg != 1 || e.rag.registered[0].FileID != string(ref.FileID) {
		t.Fatalf("registered = %+v", e.rag.registered)
	}
	// A source is evidence, never an attachment a model is handed.
	_, err := e.svc.ForModel(ctx, scopeA, []string{string(ref.FileID)})
	wantReason(t, err, contracts.ErrPermissionDenied, "rag_source")

	// Deleting it withdraws it first; if that fails the file stays.
	e.rag.failRemove = errors.New("index unavailable")
	wantCode(t, e.svc.Delete(ctx, scopeA, string(ref.FileID)), contracts.ErrUnavailable)
	if _, err := e.svc.Stat(ctx, scopeA, string(ref.FileID)); err != nil {
		t.Fatalf("a source that could not be withdrawn was deleted anyway: %v", err)
	}
	e.rag.failRemove = nil
	if err := e.svc.Delete(ctx, scopeA, string(ref.FileID)); err != nil {
		t.Fatal(err)
	}
	if _, rem := e.rag.counts(); rem != 1 {
		t.Errorf("removed = %d", rem)
	}
	wantCode(t, func() error { _, err := e.svc.Stat(ctx, scopeA, string(ref.FileID)); return err }(), contracts.ErrScopeViolation)

	// An ingestion that refuses the file leaves no file behind.
	e.rag.failRegister = errors.New("parser queue full")
	_, err = e.put(scopeA, "refused.txt", "text/plain", textBytes("other rules"), asTreatment(TreatmentRAGSource))
	wantCode(t, err, contracts.ErrUnavailable)
	e.clk.Advance(DefaultCompactGrace)
	if _, err := e.svc.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	if e.rowCount() != 0 || len(diskBlobs(t, e.dir)) != 0 {
		t.Errorf("a refused source left %d rows, %d blobs", e.rowCount(), len(diskBlobs(t, e.dir)))
	}
	e.rag.failRegister = nil

	// An expiring source is withdrawn by compaction; one that cannot be withdrawn stays until it can.
	exp := e.mustPut(scopeA, "temp.txt", "text/plain", textBytes("temporary rules"), asTreatment(TreatmentRAGSource))
	e.clk.Advance(DefaultRAGSourceTTL)
	e.rag.failRemove = errors.New("index unavailable")
	if _, err := e.svc.Compact(ctx); err == nil {
		t.Error("Compact hid a source it could not withdraw")
	}
	if e.rowCount() != 1 {
		t.Fatal("the row of a source that is still indexed was removed")
	}
	e.rag.failRemove = nil
	if _, err := e.svc.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	if reg, rem := e.rag.counts(); rem != 2 {
		t.Errorf("registered=%d removed=%d, want the expired source withdrawn", reg, rem)
	}
	_ = exp
}

// SEC-028: deleting a tenant makes every one of its files stop resolving at once
// (a tombstone), and compaction removes the bytes afterwards. Other tenants'
// files are untouched.
func TestDeleteTenantTombstonesThenCompacts(t *testing.T) {
	e := newEnv(t, withRAG(), withConfig(func(c *Config) { c.Object.Enabled = true; ragOn(c) }))
	a1 := e.mustPut(scopeA, "a1.txt", "text/plain", textBytes("tenant a, workspace 1"))
	a2 := e.mustPut(scopeA2, "a2.txt", "text/plain", textBytes("tenant a, workspace 2"), asTreatment(TreatmentObject))
	src := e.mustPut(scopeA, "src.txt", "text/plain", textBytes("tenant a source"), asTreatment(TreatmentRAGSource))
	b1 := e.mustPut(scopeB, "b1.txt", "text/plain", textBytes("tenant b"))
	if len(diskBlobs(t, e.dir)) != 4 {
		t.Fatalf("%d blobs, want 4", len(diskBlobs(t, e.dir)))
	}

	n, err := e.svc.DeleteTenant(ctx, scopeA)
	if err != nil || n != 3 {
		t.Fatalf("DeleteTenant = %d, %v; want 3 files", n, err)
	}
	for _, r := range []conversation.FileRef{a1, a2, src} {
		sc := scopeA
		if r.FileID == a2.FileID {
			sc = scopeA2
		}
		wantCode(t, func() error { _, err := e.svc.Stat(ctx, sc, string(r.FileID)); return err }(), contracts.ErrScopeViolation)
	}
	if _, rem := e.rag.counts(); rem != 1 {
		t.Errorf("the RAG source was not withdrawn (%d removals)", rem)
	}
	if _, err := e.svc.Stat(ctx, scopeB, string(b1.FileID)); err != nil {
		t.Fatalf("another tenant's file was touched: %v", err)
	}
	if u, _ := e.svc.Usage(ctx, scopeA); u.Files != 0 || u.Bytes != 0 {
		t.Errorf("a deleted tenant still counts: %+v", u)
	}
	// Invisible at once, but the bytes wait for compaction.
	if len(diskBlobs(t, e.dir)) != 4 {
		t.Fatal("bytes were removed before compaction")
	}
	if rep, err := e.svc.Compact(ctx); err != nil || rep.Blobs != 0 {
		t.Fatalf("Compact inside the grace removed %+v, %v", rep, err)
	}
	e.clk.Advance(DefaultCompactGrace + time.Second)
	rep, err := e.svc.Compact(ctx)
	if err != nil || rep.Blobs != 3 || rep.Rows != 3 {
		t.Fatalf("Compact = %+v, %v; want 3 blobs and 3 rows", rep, err)
	}
	if blobs := diskBlobs(t, e.dir); len(blobs) != 1 || e.rowCount() != 1 {
		t.Errorf("after compaction %v blobs, %d rows; want only tenant b's", blobs, e.rowCount())
	}
	if c, err := e.svc.Open(ctx, scopeB, string(b1.FileID), PermRead); err != nil {
		t.Fatalf("tenant b's file is gone: %v", err)
	} else if got := readAll(t, c); string(got) != "tenant b" {
		t.Errorf("tenant b's content = %q", got)
	}
	// Deleting a tenant again is a no-op, and the tenant directory is gone.
	if n, err := e.svc.DeleteTenant(ctx, scopeA); err != nil || n != 0 {
		t.Errorf("second DeleteTenant = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "files", tenantDirName(scopeA.ApplicationID, scopeA.TenantID))); !os.IsNotExist(err) {
		t.Errorf("the deleted tenant's directory still exists: %v", err)
	}
}

// The quota is the tenant's: it spans the tenant's workspaces, it does not touch
// another tenant, and deleting a file frees its room at once.
func TestQuotaPerTenant(t *testing.T) {
	quota := func(c *Config) { c.QuotaBytes = 1000; c.QuotaFiles = 100 }
	body := func(n int, tag byte) []byte { return bytes.Repeat([]byte{tag}, n) }

	t.Run("bytes", func(t *testing.T) {
		e := newEnv(t, withConfig(quota))
		first := e.mustPut(scopeA, "1.txt", "text/plain", body(400, 'a'))
		e.mustPut(scopeA2, "2.txt", "text/plain", body(400, 'b')) // another workspace of the same tenant
		_, err := e.put(scopeA, "3.txt", "text/plain", body(400, 'c'))
		wantReason(t, err, contracts.ErrResourceExhausted, "quota_bytes")
		if n := len(diskBlobs(t, e.dir)); n != 2 || e.rowCount() != 2 || tmpFiles(t, e.dir) != 0 {
			t.Errorf("a refused upload left %d blobs, %d rows, %d temp files", n, e.rowCount(), tmpFiles(t, e.dir))
		}
		// Another tenant has its own room.
		e.mustPut(scopeB, "b.txt", "text/plain", body(900, 'd'))
		// Freed room is usable at once.
		if err := e.svc.Delete(ctx, scopeA, string(first.FileID)); err != nil {
			t.Fatal(err)
		}
		if u, _ := e.svc.Usage(ctx, scopeA); u.Files != 1 || u.Bytes != 400 {
			t.Errorf("usage after a delete = %+v", u)
		}
		e.mustPut(scopeA, "3.txt", "text/plain", body(400, 'c'))
		// An expired file no longer counts either.
		e.clk.Advance(DefaultEphemeralTTL)
		if u, _ := e.svc.Usage(ctx, scopeA); u.Files != 0 {
			t.Errorf("usage of an expired tenant = %+v", u)
		}
	})
	t.Run("a single file cannot exceed the quota", func(t *testing.T) {
		e := newEnv(t, withConfig(quota))
		_, err := e.put(scopeA, "big.txt", "text/plain", body(1001, 'x'))
		wantReason(t, err, contracts.ErrResourceExhausted, "quota_bytes")
	})
	t.Run("file count", func(t *testing.T) {
		e := newEnv(t, withConfig(func(c *Config) { c.QuotaFiles = 2 }))
		e.mustPut(scopeA, "1.txt", "text/plain", body(10, 'a'))
		e.mustPut(scopeA2, "2.txt", "text/plain", body(10, 'b'))
		_, err := e.put(scopeA, "3.txt", "text/plain", body(10, 'c'))
		wantReason(t, err, contracts.ErrResourceExhausted, "quota_files")
		e.mustPut(scopeB, "b.txt", "text/plain", body(10, 'd'))
	})
	t.Run("concurrent uploads cannot slip under the limit", func(t *testing.T) {
		e := newEnv(t, withConfig(func(c *Config) { c.QuotaFiles = 3 }))
		var ok, refused atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := e.put(scopeA, "n.txt", "text/plain", textBytes(fmt.Sprintf("upload %d", i)))
				switch contracts.CodeOf(err) {
				case "":
					ok.Add(1)
				case contracts.ErrResourceExhausted:
					refused.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 3 || refused.Load() != 9 || e.rowCount() != 3 || len(diskBlobs(t, e.dir)) != 3 {
			t.Errorf("%d stored, %d refused, %d rows, %d blobs; want 3, 9, 3, 3", ok.Load(), refused.Load(), e.rowCount(), len(diskBlobs(t, e.dir)))
		}
	})
}

// A file larger than the rule's cap is refused as soon as the cap is crossed;
// a sender whose stream fails is not a stored file either; and a body limit
// that cuts the stream is still recognisable to the gateway.
func TestUploadLimitsAndStreamFailures(t *testing.T) {
	e := newEnv(t, withConfig(func(c *Config) { c.Ephemeral.MaxBytes = 600 }))
	e.mustPut(scopeA, "fits.txt", "text/plain", bytes.Repeat([]byte("a"), 600))
	_, err := e.put(scopeA, "big.txt", "text/plain", bytes.Repeat([]byte("b"), 601))
	ce := wantCode(t, err, contracts.ErrValidationFailed)
	if ce.Details["reason"] != "too_large" || ce.Details["limit"] != int64(600) {
		t.Errorf("details = %v", ce.Details)
	}
	_, err = e.put(scopeA, "tiny.txt", "text/plain", bytes.Repeat([]byte("c"), 100), func(in *PutInput) {
		in.Content = io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("c"), 100)), errReader{errors.New("connection reset")})
	})
	wantReason(t, err, contracts.ErrValidationFailed, "read_failed")
	_, err = e.put(scopeA, "cut.txt", "text/plain", nil, func(in *PutInput) {
		in.Content = io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("d"), 550)), errReader{&http.MaxBytesError{Limit: 64}})
	})
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		t.Errorf("a body limit was not preserved for the gateway: %v", err)
	}
	if e.rowCount() != 1 || len(diskBlobs(t, e.dir)) != 1 || tmpFiles(t, e.dir) != 0 {
		t.Errorf("refused uploads left %d rows, %d blobs, %d temp files", e.rowCount(), len(diskBlobs(t, e.dir)), tmpFiles(t, e.dir))
	}
	// A rule can only narrow the deployment's cap, never widen it.
	wide := newEnv(t, withConfig(func(c *Config) { c.MaxFileBytes = 300; c.Ephemeral.MaxBytes = 5000 }))
	_, err = wide.put(scopeA, "big.txt", "text/plain", bytes.Repeat([]byte("e"), 301))
	wantReason(t, err, contracts.ErrValidationFailed, "too_large")
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// SEC-023: identical uploads by different tenants (or workspaces) are different
// files with different ids, names and directories; dedupe happens only inside one
// scope.
func TestSecContentAddressIsPerTenant(t *testing.T) {
	e := newEnv(t)
	same := textBytes("the exact same document, uploaded by two tenants")
	a := e.mustPut(scopeA, "doc.txt", "text/plain", same)
	b := e.mustPut(scopeB, "doc.txt", "text/plain", same)
	a2 := e.mustPut(scopeA2, "doc.txt", "text/plain", same)

	if a.FileID == b.FileID || a.FileID == a2.FileID {
		t.Fatal("two scopes were given the same file id")
	}
	keys := map[string]string{
		"tenant a":     e.blobKeyOf(string(a.FileID)),
		"tenant b":     e.blobKeyOf(string(b.FileID)),
		"tenant a, w2": e.blobKeyOf(string(a2.FileID)),
	}
	seen := map[string]string{}
	for who, k := range keys {
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s share the blob key %s", who, other, k)
		}
		seen[k] = who
	}
	blobs := diskBlobs(t, e.dir)
	if len(blobs) != 3 {
		t.Fatalf("%d blobs on disk, want one per scope: %v", len(blobs), blobs)
	}
	dirs := map[string]bool{}
	names := map[string]bool{}
	for _, p := range blobs {
		parts := strings.Split(p, "/")
		dirs[parts[0]], names[parts[2]] = true, true
	}
	if len(dirs) != 2 || len(names) != 3 {
		t.Errorf("tenant directories %v, file names %v; want one directory per tenant and a name per scope", dirs, names)
	}
	// Neither a scope identifier nor the plain content hash is on disk as a name.
	for _, p := range blobs {
		for _, bad := range []string{scopeA.TenantID, scopeB.TenantID, scopeA.ApplicationID, strings.TrimPrefix(hashOf(a), "sha256:")} {
			if strings.Contains(p, bad) {
				t.Errorf("path %q contains %q", p, bad)
			}
		}
	}
	// The reference of one tenant resolves nowhere in the other.
	for _, who := range []contracts.Scope{scopeB, scopeA2} {
		_, err := e.svc.Stat(ctx, who, string(a.FileID))
		wantCode(t, err, contracts.ErrScopeViolation)
	}
	// What a tenant stores is the same whether or not another tenant stored it first.
	solo := newEnv(t)
	only := solo.mustPut(scopeB, "doc.txt", "text/plain", same)
	if only.SizeBytes != b.SizeBytes || only.MimeType != b.MimeType || hashOf(only) != hashOf(b) {
		t.Errorf("tenant b's reference differs depending on tenant a: %+v vs %+v", only, b)
	}
}

// Inside one scope identical content is one blob with two references; the blob
// goes only when the last reference does.
func TestDedupeWithinScope(t *testing.T) {
	e := newEnv(t)
	same := textBytes("one document, uploaded twice by the same tenant")
	r1 := e.mustPut(scopeA, "one.txt", "text/plain", same)
	r2 := e.mustPut(scopeA, "two.txt", "text/plain", same)
	if r1.FileID == r2.FileID {
		t.Fatal("two uploads share an id")
	}
	if n := len(diskBlobs(t, e.dir)); n != 1 {
		t.Fatalf("%d blobs for identical content in one scope, want 1", n)
	}
	if err := e.svc.Delete(ctx, scopeA, string(r1.FileID)); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(DefaultCompactGrace)
	if rep, err := e.svc.Compact(ctx); err != nil || rep.Rows != 1 || rep.Blobs != 0 {
		t.Fatalf("Compact = %+v, %v; want the row gone and the shared blob kept", rep, err)
	}
	c, err := e.svc.Open(ctx, scopeA, string(r2.FileID), PermRead)
	if err != nil {
		t.Fatalf("deleting one reference broke the other: %v", err)
	}
	if got := readAll(t, c); !bytes.Equal(got, same) {
		t.Errorf("content = %q", got)
	}
	if err := e.svc.Delete(ctx, scopeA, string(r2.FileID)); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(DefaultCompactGrace)
	if rep, err := e.svc.Compact(ctx); err != nil || rep.Rows != 1 || rep.Blobs != 1 {
		t.Fatalf("Compact = %+v, %v", rep, err)
	}
	if len(diskBlobs(t, e.dir)) != 0 {
		t.Error("the last reference is gone and the blob is not")
	}
}

// SEC-081: the files directory, the tenant directories and the blobs are
// owner-only.
func TestSecFilesStoredOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply; on Windows the ACL of the data directory governs")
	}
	e := newEnv(t)
	e.mustPut(scopeA, "a.txt", "text/plain", textBytes("owner only"))
	root := filepath.Join(e.dir, "files")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", p, info.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// SEC-019: a preview exists only for a type with a safe previewer, and is served
// as a safe type.
func TestPreviewOnlySafeTypes(t *testing.T) {
	e := newEnv(t, withConfig(func(c *Config) {
		c.Ephemeral.AllowedMIME = append(c.Ephemeral.AllowedMIME, docxMIME)
		c.PreviewTextBytes = 20
	}))
	img := e.mustPut(scopeA, "p.png", "image/png", pngBytes())
	txt := e.mustPut(scopeA, "t.md", "text/markdown", textBytes("# a heading that is longer than twenty bytes <script>alert(1)</script>"))
	pdf := e.mustPut(scopeA, "d.pdf", "application/pdf", pdfBytes())
	doc := e.mustPut(scopeA, "d.docx", docxMIME, docxBytes(t))
	if !img.Previewable || !txt.Previewable || pdf.Previewable || doc.Previewable {
		t.Errorf("previewable: png=%v md=%v pdf=%v docx=%v", img.Previewable, txt.Previewable, pdf.Previewable, doc.Previewable)
	}

	p, err := e.svc.Preview(ctx, scopeA, string(img.FileID))
	if err != nil || p.ContentType != "image/png" || p.Truncated {
		t.Fatalf("image preview = %+v, %v", p, err)
	}
	if b, _ := io.ReadAll(p.Body); !bytes.Equal(b, pngBytes()) {
		t.Error("the image preview is not the image")
	}
	p.Body.Close()

	p, err = e.svc.Preview(ctx, scopeA, string(txt.FileID))
	if err != nil {
		t.Fatal(err)
	}
	if p.ContentType != "text/plain; charset=utf-8" || !p.Truncated || p.Size != 20 {
		t.Errorf("text preview = %s truncated=%v size=%d", p.ContentType, p.Truncated, p.Size)
	}
	if b, _ := io.ReadAll(p.Body); len(b) != 20 {
		t.Errorf("text preview body is %d bytes", len(b))
	}
	p.Body.Close()

	for name, r := range map[string]conversation.FileRef{"pdf": pdf, "docx": doc} {
		_, err := e.svc.Preview(ctx, scopeA, string(r.FileID))
		if name == "pdf" || name == "docx" {
			wantReason(t, err, contracts.ErrValidationFailed, "not_previewable")
		}
	}
	// A policy that does not grant preview refuses it even for a previewable type.
	nop := newEnv(t, withConfig(func(c *Config) { c.Ephemeral.Permissions = []string{"read"} }))
	r := nop.mustPut(scopeA, "p.png", "image/png", pngBytes())
	_, err = nop.svc.Preview(ctx, scopeA, string(r.FileID))
	wantReason(t, err, contracts.ErrPermissionDenied, "permission")
}

// A text preview never ends in half a character.
func TestPreviewTruncatesOnACharacter(t *testing.T) {
	e := newEnv(t, withConfig(func(c *Config) { c.PreviewTextBytes = 5 }))
	r := e.mustPut(scopeA, "u.txt", "text/plain", textBytes("ab€€€")) // 2 + 3-byte runes
	p, err := e.svc.Preview(ctx, scopeA, string(r.FileID))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(p.Body)
	p.Body.Close()
	if string(b) != "ab€" || !p.Truncated {
		t.Errorf("preview = %q truncated=%v, want %q", b, p.Truncated, "ab€")
	}
}

// Master plan 14.1: an image goes to a model only when a vision-capable provider
// is installed; a model never gets more of a file than its treatment allows.
func TestForModelImageNeedsAVisionProvider(t *testing.T) {
	img := pngBytes()
	for _, tc := range []struct {
		name    string
		vision  bool
		cfg     func(*Config)
		want    string
		hasData bool
	}{
		{"no vision provider installed", false, nil, WithheldNoVision, false},
		{"vision provider installed", true, nil, "", true},
		{"image above the model's limit", true, func(c *Config) { c.ModelImageBytes = 10 }, WithheldImageTooLarge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []envOpt{withVision(tc.vision)}
			if tc.cfg != nil {
				opts = append(opts, withConfig(tc.cfg))
			}
			e := newEnv(t, opts...)
			ref := e.mustPut(scopeA, "photo.png", "image/png", img)
			out, err := e.svc.ForModel(ctx, scopeA, []string{string(ref.FileID)})
			if err != nil || len(out) != 1 {
				t.Fatalf("ForModel = %+v, %v", out, err)
			}
			if out[0].Withheld != tc.want || (len(out[0].Image) > 0) != tc.hasData || out[0].Kind != "image" {
				t.Errorf("descriptor = %+v", out[0])
			}
			if tc.hasData && !bytes.Equal(out[0].Image, img) {
				t.Error("the model was given other bytes")
			}
			if strings.Contains(string(mustJSON(t, out)), "Image") {
				t.Error("image bytes are serialised")
			}
		})
	}
	// No vision gate at all is no vision.
	e := newEnv(t)
	ref := e.mustPut(scopeA, "photo.png", "image/png", img)
	out, err := e.svc.ForModel(ctx, scopeA, []string{string(ref.FileID)})
	if err != nil || out[0].Withheld != WithheldNoVision || out[0].Image != nil {
		t.Errorf("without a vision gate: %+v, %v", out, err)
	}
}

// What a model may see of the other kinds of attachment.
func TestForModelByKind(t *testing.T) {
	e := newEnv(t, withConfig(func(c *Config) {
		c.ModelTextBytes = 8
		c.Object.Enabled = true
	}))
	txt := e.mustPut(scopeA, "t.txt", "text/plain", textBytes("0123456789"))
	pdf := e.mustPut(scopeA, "d.pdf", "application/pdf", pdfBytes())
	obj := e.mustPut(scopeA, "o.txt", "text/plain", textBytes("an application's object"), asTreatment(TreatmentObject))

	out, err := e.svc.ForModel(ctx, scopeA, []string{string(txt.FileID), string(pdf.FileID), string(obj.FileID)})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Text != "01234567" || !out[0].TextTruncated || out[0].Kind != "text" {
		t.Errorf("text = %+v", out[0])
	}
	if out[1].Withheld != WithheldNoText || out[1].Text != "" || out[1].Kind != "document" {
		t.Errorf("pdf = %+v", out[1])
	}
	if out[2].Withheld != WithheldObject || out[2].Text != "" {
		t.Errorf("object = %+v", out[2])
	}
	// One bad reference fails the whole call: nothing partial reaches the model.
	out, err = e.svc.ForModel(ctx, scopeA, []string{string(txt.FileID), "fil_01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	wantCode(t, err, contracts.ErrScopeViolation)
	if out != nil {
		t.Errorf("a failed ForModel returned %+v", out)
	}
	// A policy that does not grant read keeps the file from the model.
	nor := newEnv(t, withConfig(func(c *Config) { c.Ephemeral.Permissions = []string{"preview", "download"} }))
	r := nor.mustPut(scopeA, "t.txt", "text/plain", textBytes("hello"))
	_, err = nor.svc.ForModel(ctx, scopeA, []string{string(r.FileID)})
	wantReason(t, err, contracts.ErrPermissionDenied, "permission")
	// At most a few attachments per turn.
	many := make([]string, maxModelFiles+1)
	for i := range many {
		many[i] = string(txt.FileID)
	}
	_, err = e.svc.ForModel(ctx, scopeA, many)
	wantReason(t, err, contracts.ErrValidationFailed, "too_many")
}

// What is stored is what is read back, and the display name is a label only.
func TestPutOpenRoundTripAndNames(t *testing.T) {
	e := newEnv(t, withConfig(func(c *Config) { c.Object.Enabled = true }))
	content := pngBytes()
	ref := e.mustPut(scopeA, "C:\\Users\\bob\\Desktop\\holiday\u202egnp.exe.png", "image/png", content, asTreatment(TreatmentObject))
	if strings.ContainsAny(ref.Name, "\\/\u202e") || ref.Name != "holidaygnp.exe.png" {
		t.Errorf("name = %q", ref.Name)
	}
	if ref.SizeBytes != int64(len(content)) || ref.MimeType != "image/png" || hashOf(ref) == "" {
		t.Errorf("ref = %+v", ref)
	}
	validateFileRef(t, mustJSON(t, ref))
	c, err := e.svc.Open(ctx, scopeA, string(ref.FileID), PermDownload)
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Equal(c.Ref, ref) {
		t.Errorf("Open's reference differs: %s", cmp.Diff(ref, c.Ref))
	}
	if got := readAll(t, c); !bytes.Equal(got, content) {
		t.Error("content differs")
	}
	for raw, want := range map[string]string{"": "file", "..": "file", "  ": "file", "a/b/": "file", "résumé.pdf": "résumé.pdf", "x\x00y\tz": "xyz"} {
		if got := cleanName(raw); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", raw, got, want)
		}
	}
	if long := cleanName(strings.Repeat("é", 500)); len([]rune(long)) != 200 {
		t.Errorf("a long name has %d characters", len([]rune(long)))
	}
}

// A crash can leave an upload that was never placed and a blob whose row was never
// committed; Recover clears them once they are old enough to be nobody's in-flight
// work, and leaves everything a row names.
func TestRecoverClearsCrashResidue(t *testing.T) {
	now := time.Now().UTC()
	e := newEnvAt(t, now)
	kept := e.mustPut(scopeA, "kept.txt", "text/plain", textBytes("a file with a row"))
	orphan := e.mustPut(scopeA, "orphan.txt", "text/plain", textBytes("a blob whose row was lost"))
	if _, err := e.db.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, string(orphan.FileID)); err != nil {
		t.Fatal(err)
	}
	f, _, err := e.svc.blobs.tmpFile()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if rep, err := e.svc.Recover(ctx); err != nil || rep.Orphans != 0 || rep.Temp != 0 {
		t.Fatalf("Recover removed young residue: %+v, %v", rep, err)
	}
	e.clk.Advance(2 * time.Hour)
	rep, err := e.svc.Recover(ctx)
	if err != nil || rep.Orphans != 1 || rep.Temp != 1 {
		t.Fatalf("Recover = %+v, %v; want one orphan and one upload file", rep, err)
	}
	if blobs := diskBlobs(t, e.dir); len(blobs) != 1 {
		t.Errorf("blobs after recovery: %v", blobs)
	}
	if _, err := e.svc.Open(ctx, scopeA, string(kept.FileID), PermRead); err == nil || contracts.CodeOf(err) != contracts.ErrExpired {
		// Two hours later the ephemeral reference has expired, but its blob is intact.
		t.Errorf("kept file: %v", err)
	}
}

func TestFilesTableCarriesTheScope(t *testing.T) {
	e := newEnv(t)
	storetest.AssertScopedTables(t, e.db)
}

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("the defaults are invalid: %v", err)
	}
	bad := map[string]func(*Config){
		"negative size":           func(c *Config) { c.MaxFileBytes = -1 },
		"huge quota":              func(c *Config) { c.QuotaBytes = 1 << 50 },
		"negative ttl":            func(c *Config) { c.Ephemeral.TTL = -time.Second },
		"unknown permission":      func(c *Config) { c.Ephemeral.Permissions = []string{"write"} },
		"unknown visibility":      func(c *Config) { c.Ephemeral.Visibility = "everyone" },
		"a type that is not safe": func(c *Config) { c.Ephemeral.AllowedMIME = []string{"text/html"} },
		"a family that is not":    func(c *Config) { c.Ephemeral.AllowedMIME = []string{"application/*"} },
		"enabled with no types":   func(c *Config) { c.Object.Enabled, c.Object.AllowedMIME = true, nil },
	}
	for name, mut := range bad {
		c := DefaultConfig()
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: an invalid configuration was accepted", name)
		}
	}
	if ConfigSection() == nil || len(KnownTypes()) == 0 {
		t.Error("the section or the type list is empty")
	}
	for _, m := range KnownTypes() {
		if strings.Contains(m, "html") || strings.Contains(m, "svg") || strings.Contains(m, "xml") && !strings.Contains(m, "openxml") || strings.Contains(m, "javascript") {
			t.Errorf("an active type is in the table: %s", m)
		}
	}
}

func TestNewRefusesIncompleteOptions(t *testing.T) {
	db := storetest.Open(t, nil)
	if _, err := New(Options{DataDir: t.TempDir()}); err == nil {
		t.Error("New accepted no store")
	}
	if _, err := New(Options{DB: db}); err == nil {
		t.Error("New accepted no data directory")
	}
	bad := DefaultConfig()
	bad.Ephemeral.Permissions = []string{"write"}
	if _, err := New(Options{DB: db, DataDir: t.TempDir(), Config: &bad}); err == nil {
		t.Error("New accepted an invalid configuration")
	}
}
