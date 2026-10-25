package files

import (
	"context"
	"io"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// What a model may be given of an attachment (master plan 14.1: "the model
// never receives an unrestricted filesystem path"; section 3 rule 6, least
// capability).
//
// A ModelFile carries the opaque file id, the display name, the type and size,
// and only the content the file's treatment and the installed providers allow:
//
//   - ephemeral_context: the text of a text file (bounded), the bytes of an image
//     when, and only when, a vision-capable provider is installed. A PDF or an
//     Office file has no text here: extracting it is the RAG parser's job, and
//     the descriptor says so (Withheld).
//   - application_object: metadata only. The application owns the object and
//     reaches its bytes through its own adapter.
//   - rag_source: refused. A source reaches a model only as cited evidence of the
//     RAG pipeline, never as an attachment.
//
// There is no field that holds a path, a URL or a storage key, and every
// resolution re-checks the scope, the principal, the session, the expiry and the
// read permission. Everything in a ModelFile is untrusted data: the prompt
// builder fences the text and the name as such (SEC-003, SEC-007).

// maxModelFiles bounds how many attachments one turn hands to a model.
const maxModelFiles = 16

// Why content was not handed to the model.
const (
	// WithheldNoVision: an image, and no vision-capable provider is installed.
	WithheldNoVision = "no_vision_provider"
	// WithheldImageTooLarge: an image above model_image_bytes.
	WithheldImageTooLarge = "image_too_large"
	// WithheldNoText: a document the core has no text extractor for here.
	WithheldNoText = "no_text_extractor"
	// WithheldObject: an application object; its bytes are the application's.
	WithheldObject = "application_object"
)

// ModelFile is one attachment as a model may see it.
type ModelFile struct {
	// FileID is the opaque reference. It is the only handle a model ever has.
	FileID string `json:"file_id"`
	Name   string `json:"name"`
	// MIMEType is the sniffed type.
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	// Kind is image, text or document.
	Kind string `json:"kind"`
	// Text is the content of a text attachment, cut to model_text_bytes.
	Text          string `json:"text,omitempty"`
	TextTruncated bool   `json:"text_truncated,omitempty"`
	// Image holds the bytes of an image, only for a vision-capable provider. It
	// is never serialised.
	Image []byte `json:"-"`
	// Withheld says why there is no content (one of the Withheld constants).
	Withheld string `json:"withheld,omitempty"`
}

// ForModel resolves the attachments of a turn and returns what a model may see
// of them. Every id must resolve for the caller, be readable (the read
// permission) and be an attachment a model may be given; otherwise the whole
// call fails and nothing is returned (fail closed).
func (s *Service) ForModel(ctx context.Context, scope contracts.Scope, fileIDs []string) ([]ModelFile, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(fileIDs) > maxModelFiles {
		return nil, invalid("too_many", "too many attachments in one turn")
	}
	cfg := s.config()
	out := make([]ModelFile, 0, len(fileIDs))
	for _, id := range fileIDs {
		r, err := s.resolve(ctx, scope, id, PermRead)
		if err != nil {
			return nil, err
		}
		if r.treatment == TreatmentRAGSource {
			return nil, contracts.New(contracts.ErrPermissionDenied, "a RAG source reaches a model only as cited evidence").
				WithDetails("reason", "rag_source")
		}
		spec := typeTable[r.mime]
		mf := ModelFile{FileID: r.id, Name: r.name, MIMEType: r.mime, SizeBytes: r.size, Kind: string(spec.kind)}
		switch {
		case r.treatment == TreatmentObject:
			mf.Withheld = WithheldObject
		case spec.kind == kindText:
			c, err := s.openRow(r)
			if err != nil {
				return nil, err
			}
			buf, rerr := io.ReadAll(io.LimitReader(c.Body, int64(cfg.ModelTextBytes)+1))
			c.Body.Close()
			if rerr != nil {
				return nil, storageFault("read a stored file", rerr)
			}
			if len(buf) > cfg.ModelTextBytes {
				buf, mf.TextTruncated = trimToRune(buf[:cfg.ModelTextBytes]), true
			}
			mf.Text = string(buf)
		case spec.kind == kindImage:
			switch {
			case s.vision == nil || !s.vision.VisionAvailable(ctx):
				mf.Withheld = WithheldNoVision
			case r.size > cfg.ModelImageBytes:
				mf.Withheld = WithheldImageTooLarge
			default:
				c, err := s.openRow(r)
				if err != nil {
					return nil, err
				}
				buf, rerr := io.ReadAll(io.LimitReader(c.Body, cfg.ModelImageBytes+1))
				c.Body.Close()
				if rerr != nil {
					return nil, storageFault("read a stored file", rerr)
				}
				mf.Image = buf
			}
		default:
			mf.Withheld = WithheldNoText
		}
		out = append(out, mf)
	}
	return out, nil
}
