package gateway

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The files part of the public API (master plan 14.1, "Attachments and generated
// files use scoped opaque file references"):
//
//	POST   /v1/files                     upload one file (multipart/form-data)
//	GET    /v1/files/{file_id}           the reference: size, type, owner, expiry, permissions
//	GET    /v1/files/{file_id}/content   download (always as an attachment)
//	GET    /v1/files/{file_id}/preview   a safe rendering, for the types that have one
//	DELETE /v1/files/{file_id}           delete (a tombstone; the bytes follow in compaction)
//
// The handlers hold only the tenant-facing part of internal/files (FilesService):
// the tenant deletion and the compaction sweeps work across tenants and are not
// here, so no request can reach them. Every operation takes the verified scope of
// the context token and nothing from the path or the body but the file id; a
// reference that does not resolve in that scope (another tenant, principal or
// session, a deleted file, an id that never existed) is one and the same
// scope_violation, and an expired one is 410. A quarantined application serves no
// file traffic, like it serves no other.
//
// What goes on the wire is data, never executable: a download is always an
// attachment and a preview is raster-image or plain-text only, both served with
// nosniff and a Content-Security-Policy that sandboxes everything, so a stored
// file can never run in the browser that fetches it (SEC-019). The media type
// the sender declares has to be what the bytes are, or nothing is stored.
//
// An upload is a single multipart/form-data request: an optional `treatment`
// field (it must precede the file, and the host's attachment policy decides
// whether it is accepted) and one `file` part. The upload route has a body limit
// of its own: files.max_file_bytes plus a fixed allowance for the multipart
// framing (attachUploadOverhead), never above the 16 MiB ceiling of the gateway
// (maxMaxBodyBytes), applied before the handler runs like gateway.max_body_bytes
// is for every other route, so a declared length over it is a 413 before a byte is
// read. The policy's max_bytes and the settings' attachment ceiling can only
// narrow it further, per treatment.

// FilesService is the part of the files service a tenant request may reach: the
// operations that take the verified scope. *files.Service implements it.
type FilesService interface {
	Put(ctx context.Context, scope contracts.Scope, in files.PutInput) (conversation.FileRef, error)
	Stat(ctx context.Context, scope contracts.Scope, id string) (conversation.FileRef, error)
	Open(ctx context.Context, scope contracts.Scope, id string, perm files.Permission) (*files.Content, error)
	Preview(ctx context.Context, scope contracts.Scope, id string) (*files.Preview, error)
	Delete(ctx context.Context, scope contracts.Scope, id string) error
}

var _ FilesService = (*files.Service)(nil)

type filesHandler struct {
	files FilesService
	apps  ApplicationService
}

func init() { RegisterArea("v1_files", registerV1Files) }

// registerV1Files needs the files service (as FilesService) and the application
// service.
func registerV1Files(rt *Router, d *Deps) {
	n := d.Needs()
	h := &filesHandler{
		files: Need[FilesService](n),
		apps:  Need[ApplicationService](n),
	}
	if !n.Ready() {
		return
	}
	v1 := rt.V1()
	attachSetRouteLimit(rt, http.MethodPost, "/v1/files", attachUploadLimit)
	v1.HandleFunc("POST /files", h.upload)
	v1.HandleFunc("GET /files/{file_id}", h.get)
	v1.HandleFunc("GET /files/{file_id}/content", h.download)
	v1.HandleFunc("GET /files/{file_id}/preview", h.preview)
	v1.HandleFunc("DELETE /files/{file_id}", h.remove)
}

// filesError writes the error of a file operation. The statuses beyond the
// defaults are the ones this API declares: an expired reference is 410 (not the
// 401 of an expired credential: the client's token is fine), a tenant over its
// quota is 507, a file over its limit is 413 and a preview of a type that has
// none is 415. Everything else is the gateway's mapping of the code.
func filesError(w http.ResponseWriter, r *http.Request, err error) {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		reason, _ := ce.Details["reason"].(string)
		switch {
		case ce.Code == contracts.ErrExpired:
			WriteErrorStatus(w, r, http.StatusGone, err)
			return
		case ce.Code == contracts.ErrResourceExhausted:
			WriteErrorStatus(w, r, http.StatusInsufficientStorage, err)
			return
		case ce.Code == contracts.ErrValidationFailed && reason == "too_large":
			WriteErrorStatus(w, r, http.StatusRequestEntityTooLarge, err)
			return
		case ce.Code == contracts.ErrValidationFailed && reason == "not_previewable":
			WriteErrorStatus(w, r, http.StatusUnsupportedMediaType, err)
			return
		}
	}
	WriteError(w, r, err)
}

// begin is the common start of a file route: the verified scope, an active
// application and a well-formed file id.
func (h *filesHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, string, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return scope, "", false
	}
	id := r.PathValue("file_id")
	if !files.ValidID(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the file id is malformed"))
		return scope, "", false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return scope, "", false
	}
	return scope, id, true
}

// filesBadRequest is a malformed upload.
func filesBadRequest(w http.ResponseWriter, r *http.Request, msg string) {
	WriteError(w, r, contracts.New(contracts.ErrValidationFailed, msg))
}

func (h *filesHandler) upload(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		WriteErrorStatus(w, r, http.StatusUnsupportedMediaType,
			contracts.New(contracts.ErrValidationFailed, "Content-Type must be multipart/form-data"))
		return
	}
	ctx := r.Context()
	mr := multipart.NewReader(r.Body, params["boundary"])
	var (
		treatment files.Treatment
		ref       conversation.FileRef
		stored    bool
	)
	// Exactly one file is an upload: if the request turns out to be anything
	// else after the file was taken, the file is withdrawn again.
	fail := func(send func()) {
		if stored {
			_ = h.files.Delete(ctx, scope, string(ref.FileID))
		}
		send()
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF { // the closing boundary; a body cut short is a wrapped EOF and an error
			break
		}
		if err != nil {
			fail(func() { filesMultipartError(w, r, err) })
			return
		}
		switch part.FormName() {
		case "treatment":
			if stored || treatment != "" {
				fail(func() { filesBadRequest(w, r, "the treatment field must come once, before the file") })
				return
			}
			b, err := io.ReadAll(io.LimitReader(part, 64))
			if err != nil {
				fail(func() { filesMultipartError(w, r, err) })
				return
			}
			treatment = files.Treatment(strings.TrimSpace(string(b)))
			if !treatment.Valid() {
				fail(func() {
					filesBadRequest(w, r, "the treatment is not ephemeral_context, rag_source or application_object")
				})
				return
			}
		case "file":
			if stored {
				fail(func() { filesBadRequest(w, r, "an upload carries exactly one file") })
				return
			}
			extendWriteDeadline(w)
			ref, err = h.files.Put(ctx, scope, files.PutInput{
				Name:         part.FileName(),
				DeclaredMIME: part.Header.Get("Content-Type"),
				Treatment:    treatment,
				Origin:       files.OriginUpload, // a client only ever uploads; generated and application files are the core's and the host's
				Content:      part,
			})
			if err != nil {
				filesError(w, r, err)
				return
			}
			stored = true
		default:
			fail(func() { filesBadRequest(w, r, "the upload has a field that is neither treatment nor file") })
			return
		}
	}
	if !stored {
		filesBadRequest(w, r, "the upload has no file part")
		return
	}
	w.Header().Set("Location", "/v1/files/"+string(ref.FileID))
	writeJSON(w, http.StatusCreated, ref)
}

// filesMultipartError writes the error of reading a multipart body: a body
// limit keeps its status, anything else is a malformed upload.
func filesMultipartError(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		WriteError(w, r, err)
		return
	}
	filesBadRequest(w, r, "the multipart body is malformed")
}

func (h *filesHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	ref, err := h.files.Stat(r.Context(), scope, id)
	if err != nil {
		filesError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

// filesServe writes a file's bytes with the headers that keep them inert.
func filesServe(w http.ResponseWriter, contentType, disposition, name string, size int64, body io.Reader) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	if cd := mime.FormatMediaType(disposition, map[string]string{"filename": name}); cd != "" {
		h.Set("Content-Disposition", cd)
	} else {
		h.Set("Content-Disposition", disposition)
	}
	// Whatever the bytes are, the browser may not run, embed or frame them.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	extendWriteDeadline(w)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (h *filesHandler) download(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	c, err := h.files.Open(r.Context(), scope, id, files.PermDownload)
	if err != nil {
		filesError(w, r, err)
		return
	}
	defer c.Body.Close()
	filesServe(w, c.Ref.MimeType, "attachment", c.Ref.Name, c.Ref.SizeBytes, c.Body)
}

func (h *filesHandler) preview(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	p, err := h.files.Preview(r.Context(), scope, id)
	if err != nil {
		filesError(w, r, err)
		return
	}
	defer p.Body.Close()
	if p.Truncated {
		w.Header().Set("X-Alixai-Preview-Truncated", "true")
	}
	filesServe(w, p.ContentType, "inline", p.Ref.Name, p.Size, p.Body)
}

func (h *filesHandler) remove(w http.ResponseWriter, r *http.Request) {
	scope, id, ok := h.begin(w, r)
	if !ok {
		return
	}
	if err := h.files.Delete(r.Context(), scope, id); err != nil {
		filesError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// attachUploadOverhead is what the multipart framing of an upload (boundaries, part headers,
// the treatment field) may add to the file itself.
const attachUploadOverhead = 64 << 10

// attachUploadLimit is the body limit of POST /v1/files, from the "files" configuration. It is
// read for every request, so a reloaded section takes effect, and the gateway clamps it to its
// ceiling.
func attachUploadLimit() int64 {
	maxFile := files.ConfigSection().MaxFileBytes
	if maxFile <= 0 {
		maxFile = files.DefaultMaxFileBytes
	}
	return min(maxFile+attachUploadOverhead, maxMaxBodyBytes)
}
