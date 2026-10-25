package localai

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxErrorBody bounds how much of a failure response is read for
// classification.
const maxErrorBody = 64 << 10

// errResponseTooLarge is returned by capReader beyond Config.MaxResponseBytes.
var errResponseTooLarge = errors.New("localai: response exceeds the size limit")

// capReader reads at most cap+1 bytes: a body of exactly cap bytes still ends
// with io.EOF, anything longer ends with errResponseTooLarge.
type capReader struct {
	r    io.Reader
	left int64
}

func newCapReader(r io.Reader, limit int64) *capReader { return &capReader{r: r, left: limit + 1} }

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errResponseTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left <= 0 && err == nil {
		err = errResponseTooLarge
	}
	return n, err
}

// classification is how a failure is reported. The label is a fixed phrase:
// response bodies are used to choose it and are never copied into an error,
// because they may hold prompt text.
type classification struct {
	code  contracts.ErrCode
	label string
}

// Phrases that, in a failure body, identify the failure's nature. They come
// from LocalAI v4.10.0 (pkg/model/initializers.go, pkg/model/loader.go,
// core/backend/global_admission.go) and from llama.cpp. Matching is on the
// lower-cased message; "oom" is matched as a word.
var (
	contextPhrases = []string{
		"exceeds the available context size", "context length exceeded", "maximum context length",
		"context window", "prompt is too long", "input is too long",
	}
	resourcePhrases = []string{
		"out of memory", "failed to allocate", "unable to allocate", "cannot allocate memory",
		"bad_alloc", "insufficient memory", "not enough memory",
		"could not load model", "failed to load model", "failed loading model",
		"load is in cooldown", "inference capacity reached",
	}
	unavailablePhrases = []string{
		"grpc service not ready", "backend process exited", "connection refused",
		"connection reset", "no such host", "model_loading", "is still loading",
	}
	oomWordRE = regexp.MustCompile(`\boom\b`)
)

func containsAny(s string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// classifyMessage classifies a failure from its (lower-cased) message alone.
func classifyMessage(msg string) (classification, bool) {
	switch {
	case containsAny(msg, contextPhrases):
		return classification{contracts.ErrValidationFailed, "request exceeds the model context window"}, true
	case containsAny(msg, resourcePhrases) || oomWordRE.MatchString(msg):
		return classification{contracts.ErrResourceExhausted, "runtime out of resources or model failed to load"}, true
	case containsAny(msg, unavailablePhrases):
		return classification{contracts.ErrUnavailable, "runtime not ready"}, true
	}
	return classification{}, false
}

// classifyStatus classifies an HTTP failure. msg is the lower-cased error
// message from the body, or "" when there is none (opaque errors).
func classifyStatus(status int, msg string) classification {
	switch status {
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return classification{contracts.ErrTimeout, "runtime timed out"}
	case http.StatusTooManyRequests:
		return classification{contracts.ErrResourceExhausted, "runtime is at its concurrency limit"}
	case http.StatusInsufficientStorage:
		return classification{contracts.ErrResourceExhausted, "runtime out of resources"}
	case http.StatusRequestEntityTooLarge:
		return classification{contracts.ErrValidationFailed, "request too large for the runtime"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return classification{contracts.ErrInternal, "runtime rejected the configured API key"}
	case http.StatusNotFound:
		return classification{contracts.ErrUnavailable, "model or route not found on the runtime"}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if c, ok := classifyMessage(msg); ok && c.code == contracts.ErrValidationFailed {
			return c
		}
		return classification{contracts.ErrInternal, "runtime rejected the request"}
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		if c, ok := classifyMessage(msg); ok {
			return c
		}
		if status == http.StatusInternalServerError {
			return classification{contracts.ErrInternal, "runtime reported an internal error"}
		}
		return classification{contracts.ErrUnavailable, "runtime unavailable"}
	}
	if status >= 300 && status < 400 {
		return classification{contracts.ErrInternal, "runtime answered with a redirect"}
	}
	return classification{contracts.ErrInternal, "runtime answered with an unexpected status"}
}

// errorMessage extracts the message of an OpenAI-style error body, or the raw
// text when the body is not JSON, lower-cased and bounded. The result is for
// classification only.
func errorMessage(body []byte) string {
	var env struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		return strings.ToLower(env.Error.Message + " " + env.Error.Type)
	}
	if len(body) > 2048 {
		body = body[:2048]
	}
	return strings.ToLower(string(body))
}

// retryAfterSeconds reads a Retry-After header given in whole seconds.
func retryAfterSeconds(h http.Header) int {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || n < 0 {
		return 0
	}
	return min(n, 3600)
}

// mapHTTPError converts a non-success response into a *contracts.Error. The
// message is a fixed phrase plus the status; the body is never included.
func mapHTTPError(status int, header http.Header, body []byte) error {
	c := classifyStatus(status, errorMessage(body))
	e := contracts.Newf(c.code, "localai: %s (HTTP %d)", c.label, status).
		WithDetails("upstream_status", status)
	if ra := retryAfterSeconds(header); ra > 0 {
		e.WithDetails("retry_after_s", ra)
	}
	return e
}

// apiError is the error object of an OpenAI-style body or stream event.
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// mapEventError converts an error event received inside a 200 stream.
func mapEventError(e apiError) error {
	msg := strings.ToLower(e.Message + " " + e.Type)
	if c, ok := classifyMessage(msg); ok {
		return contracts.New(c.code, "localai: "+c.label+" (reported mid-stream)")
	}
	return contracts.New(contracts.ErrInternal, "localai: runtime reported an error mid-stream")
}

// mapTransportError converts a failure of the HTTP exchange itself. The
// caller's context decides first: a deadline is a timeout, cancellation stays
// a plain error wrapping context.Canceled (providers.ContextError).
func mapTransportError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return providers.ContextError(cerr)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return providers.ContextError(err)
	case errors.Is(err, errForbiddenEndpoint):
		return contracts.Wrap(contracts.ErrInternal, err, "localai: request refused by the endpoint allowlist")
	case errors.Is(err, errResponseTooLarge), errors.Is(err, bufio.ErrTooLong):
		return contracts.Wrap(contracts.ErrSchemaInvalid, err, "localai: response exceeds the size limit")
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return contracts.Wrap(contracts.ErrUnavailable, err, "localai: the runtime's TLS certificate was not accepted")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return contracts.Wrap(contracts.ErrTimeout, err, "localai: network timeout")
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return contracts.Wrap(contracts.ErrUnavailable, err, "localai: runtime refused the connection")
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "localai: runtime unreachable")
}

// schemaInvalid reports an answer that does not have the expected shape.
func schemaInvalid(msg string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "localai: "+msg)
}
