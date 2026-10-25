package realtime

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reasons carried in the "reason" detail of a side-door error.
const (
	reasonSideDoor     = "model_side_door"
	reasonNotTransOnly = "not_transcription_only"
)

// mapDialError converts a failure to open the connection (the TCP connect, the
// upgrade request, the upgrade answer). Nothing has been sent yet, so every
// "the runtime is not there or not ready" outcome is unavailable, which is the
// one code the fallback acts on. The caller's context decides first. A body is
// never copied into an error.
func mapDialError(ctx context.Context, resp *http.Response, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return providers.ContextError(cerr)
	}
	if resp != nil {
		switch s := resp.StatusCode; {
		case s == http.StatusTooManyRequests:
			return contracts.Newf(contracts.ErrResourceExhausted, "localai realtime: runtime is at its concurrency limit (HTTP %d)", s).WithDetails("upstream_status", s)
		case s == http.StatusUnauthorized || s == http.StatusForbidden:
			return contracts.Newf(contracts.ErrInternal, "localai realtime: runtime rejected the configured API key (HTTP %d)", s).WithDetails("upstream_status", s)
		case s == http.StatusNotFound || s == http.StatusRequestTimeout || s >= 500:
			return contracts.Newf(contracts.ErrUnavailable, "localai realtime: runtime has no usable realtime route (HTTP %d)", s).WithDetails("upstream_status", s)
		case s != http.StatusSwitchingProtocols:
			return contracts.Newf(contracts.ErrInternal, "localai realtime: runtime refused the upgrade (HTTP %d)", s).WithDetails("upstream_status", s)
		}
	}
	switch {
	case errors.Is(err, errForbiddenEndpoint):
		return contracts.Wrap(contracts.ErrInternal, err, "localai realtime: request refused by the endpoint allowlist")
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return contracts.Wrap(contracts.ErrUnavailable, err, "localai realtime: the runtime's TLS certificate was not accepted")
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "localai realtime: runtime unreachable")
}

// errorEvent is the error object of an "error" or ".failed" event.
type errorEvent struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// mapEventError converts an error event received on a session. The message is
// used to choose the code and is never copied: it can hold the transcript.
func mapEventError(e errorEvent) error {
	msg := strings.ToLower(e.Code + " " + e.Message + " " + e.Type)
	switch {
	case e.Code == "session_update_error":
		return contracts.New(contracts.ErrUnavailable, "localai realtime: the runtime cannot run a transcription-only session on this model")
	case strings.Contains(msg, "buffer_size_exceeded"), strings.Contains(msg, "out of memory"), strings.Contains(msg, "failed to allocate"):
		return contracts.New(contracts.ErrResourceExhausted, "localai realtime: runtime out of resources")
	case e.Code == "transcription_failed":
		return contracts.New(contracts.ErrUnavailable, "localai realtime: the runtime failed to transcribe the audio")
	}
	return contracts.New(contracts.ErrInternal, "localai realtime: runtime reported an error")
}

// sideDoor reports a server event that a transcription-only session must never
// produce: a model answer, a tool or MCP call. It is a hard failure (never
// unavailable, so it never triggers the fallback): the runtime is not behaving
// as configured, and nothing it said is relayed.
func sideDoor(eventType string) error {
	return contracts.New(contracts.ErrInternal, "localai realtime: the runtime sent a model response or tool event on a transcription-only session; session closed").
		WithDetails("reason", reasonSideDoor).
		WithDetails("event_type", eventType)
}

func schemaInvalid(msg string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "localai realtime: "+msg)
}

func interrupted(msg string) error {
	return contracts.New(contracts.ErrUnavailable, "localai realtime: "+msg)
}
