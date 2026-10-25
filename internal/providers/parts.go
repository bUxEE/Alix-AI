package providers

import (
	"fmt"
	"regexp"
	"strings"
)

// Modality is a kind of content a model can read or produce.
type Modality string

const (
	ModText  Modality = "text"
	ModImage Modality = "image"
	ModFile  Modality = "file"
	ModAudio Modality = "audio"
)

// Valid reports whether m is a known modality.
func (m Modality) Valid() bool {
	switch m {
	case ModText, ModImage, ModFile, ModAudio:
		return true
	}
	return false
}

// Limits on content parts, independent of any budget.
const (
	maxPartsPerMessage = 32
	maxRefIDLen        = 128
	// MaxPartBytes caps one provider-produced media part (image, file, audio).
	MaxPartBytes = 16 << 20
)

var refIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// mediaTypes is the allowlist per kind. Anything else is rejected: SVG and
// HTML are active content and never travel as an image or a file.
var mediaTypes = map[Modality]map[string]bool{
	ModImage: {"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true},
	ModAudio: {
		"audio/wav": true, "audio/x-wav": true, "audio/mpeg": true, "audio/ogg": true,
		"audio/flac": true, "audio/webm": true, "audio/mp4": true, "audio/aac": true,
	},
	ModFile: {
		"application/pdf": true, "text/plain": true, "text/markdown": true,
		"text/csv": true, "application/json": true,
	},
}

// MediaTypeAllowed reports whether mt is an accepted media type for kind.
func MediaTypeAllowed(kind Modality, mt string) bool { return mediaTypes[kind][mt] }

// ContentPart is one piece of a message or reply.
//
// In a request the part is text or an AlixAI-managed REFERENCE (image, file or
// audio): the provider adapter resolves the reference after the egress
// checks, so a request never carries raw bytes or a URL. In a reply the part
// is text or the produced media as BYTES (generated image, synthesised audio,
// a file); AlixAI stores it locally and hands out its own reference. Provider
// Markdown, HTML, image and chart output is untrusted data: this package only
// carries it and never interprets it.
type ContentPart struct {
	Kind Modality `json:"kind"`
	// Text is the content of a text part.
	Text string `json:"text,omitempty"`
	// RefID names an AlixAI-managed, scoped, expiring reference (request only).
	RefID string `json:"ref_id,omitempty"`
	// MediaType is the media type of a reference or of Data.
	MediaType string `json:"media_type,omitempty"`
	// Data is the produced media (reply only).
	Data []byte `json:"data,omitempty"`
}

// TextPart returns a text part.
func TextPart(s string) ContentPart { return ContentPart{Kind: ModText, Text: s} }

// ImageRef returns an image reference part.
func ImageRef(refID, mediaType string) ContentPart {
	return ContentPart{Kind: ModImage, RefID: refID, MediaType: mediaType}
}

// FileRef returns a file reference part.
func FileRef(refID, mediaType string) ContentPart {
	return ContentPart{Kind: ModFile, RefID: refID, MediaType: mediaType}
}

// AudioRef returns an audio reference part.
func AudioRef(refID, mediaType string) ContentPart {
	return ContentPart{Kind: ModAudio, RefID: refID, MediaType: mediaType}
}

// ValidateInput checks a part of a request: text, or a well-formed reference
// with an allowed media type, and never bytes.
func (p ContentPart) ValidateInput() error {
	switch p.Kind {
	case ModText:
		if p.Text == "" {
			return invalidRequest("text part is empty")
		}
		if p.RefID != "" || p.MediaType != "" || len(p.Data) > 0 {
			return invalidRequest("text part carries a reference, media type or data")
		}
	case ModImage, ModFile, ModAudio:
		if p.Text != "" || len(p.Data) > 0 {
			return invalidRequest(fmt.Sprintf("%s part carries text or inline data; requests carry references only", p.Kind))
		}
		if len(p.RefID) > maxRefIDLen || !refIDPattern.MatchString(p.RefID) {
			return invalidRequest(fmt.Sprintf("%s part has no valid reference id (a URL or path is not a reference)", p.Kind))
		}
		if !MediaTypeAllowed(p.Kind, p.MediaType) {
			return invalidRequest(fmt.Sprintf("%s part has unsupported media type %q", p.Kind, truncate(p.MediaType, 40)))
		}
	default:
		return invalidRequest(fmt.Sprintf("unknown content part kind %q", truncate(string(p.Kind), 20)))
	}
	return nil
}

// ValidateOutput checks a part of a reply: text, or media bytes with an
// allowed media type. A provider can never name an AlixAI reference.
func (p ContentPart) ValidateOutput() error {
	switch p.Kind {
	case ModText:
		if p.Text == "" || p.RefID != "" || p.MediaType != "" || len(p.Data) > 0 {
			return schemaInvalid("malformed text part in provider reply")
		}
	case ModImage, ModFile, ModAudio:
		if p.Text != "" || p.RefID != "" {
			return schemaInvalid("provider media part carries text or a reference")
		}
		if len(p.Data) == 0 || len(p.Data) > MaxPartBytes {
			return schemaInvalid("provider media part is empty or oversize")
		}
		if !MediaTypeAllowed(p.Kind, p.MediaType) {
			return schemaInvalid("provider media part has an unsupported media type")
		}
	default:
		return schemaInvalid("provider reply has an unknown part kind")
	}
	return nil
}

// Clone returns a deep copy.
func (p ContentPart) Clone() ContentPart {
	p.Data = append([]byte(nil), p.Data...)
	return p
}

func truncate(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "") + "..."
	}
	return s
}
