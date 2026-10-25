package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"runtime/debug"
	"sync"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// statusWriter records what the handler wrote, so the access log and the error
// path know whether a response has begun.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code) // informational: the final status is still to come
		return
	}
	w.wrote, w.status = true, code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// wrap is the outermost handler: request ID, response hardening headers, the
// body limit, panic recovery and the access log. It never reads the query
// string or a header into the log.
func (s *Server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		id := s.ids.New("req") // always minted here; a caller-supplied ID is never trusted
		logger := s.log.With(slog.String("request_id", id))
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		ctx = context.WithValue(ctx, ctxKeyLogger, logger)
		r = r.WithContext(ctx)

		w := &statusWriter{ResponseWriter: rw}
		h := w.Header()
		h.Set(HeaderRequestID, id)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")

		start := s.clk.Now()
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				stack := debug.Stack()
				if len(stack) > 4096 {
					stack = stack[:4096]
				}
				logger.Error("handler panic", slog.String("panic", fmt.Sprint(rec)), slog.String("stack", string(stack)))
				WriteError(w, r, contracts.New(contracts.ErrInternal, genericInternalMessage))
			}
			level := slog.LevelInfo
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				level = slog.LevelDebug
			}
			status := w.status
			if status == 0 {
				status = http.StatusOK
			}
			logger.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", truncate(r.URL.Path, 200)),
				slog.Int("status", status),
				slog.Int64("bytes_out", w.bytes),
				slog.Duration("dur", s.clk.Now().Sub(start)))
		}()

		// Body limit (SEC-076): a declared length over the limit is refused
		// before a byte is read, and an undeclared or lying length is cut off
		// by the reader. The raw writer goes to MaxBytesReader so it can ask
		// the server to close the connection.
		maxBody := s.attachBodyLimit(r)
		if r.ContentLength > maxBody {
			WriteError(w, r, &http.MaxBytesError{Limit: maxBody})
			return
		}
		r.Body = http.MaxBytesReader(rw, r.Body, maxBody)

		next.ServeHTTP(w, r)
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// errTrailingData is the decode failure for a body with more than one value.
var errTrailingData = errors.New("trailing data after the JSON value")

// decodeJSON reads the request body as exactly one JSON value into dst. The
// content type must be application/json, unknown members are an error and
// anything after the value is an error. On failure it writes the envelope and
// returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		WriteErrorStatus(w, r, http.StatusUnsupportedMediaType,
			contracts.New(contracts.ErrValidationFailed, "Content-Type must be application/json"))
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeDecodeError(w, r, err)
		return false
	}
	// Exactly one value: the next read must be a clean end of input.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errTrailingData
		}
		writeDecodeError(w, r, err)
		return false
	}
	return true
}

func writeDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		WriteError(w, r, err)
		return
	}
	// The decoder's message can quote the caller's own bytes; the client gets
	// a fixed one.
	WriteError(w, r, contracts.Wrap(contracts.ErrValidationFailed, err, "request body is not valid JSON for this endpoint"))
}

// ---- per-route body limits --------------------------------------------------

// gateway.max_body_bytes (64 KiB by default) is the limit of every request, and it is applied
// here, before any route runs, so a route that legitimately takes more (an upload) cannot raise
// it from inside its handler: by then the body is already capped. A route area declares the
// limit of its own route when it registers (attachSetRouteLimit), and wrap asks for it for each
// request, so the answer follows the area's configuration. The limit replaces the gateway-wide
// one for that route only, and is never above the ceiling of gateway.max_body_bytes (16 MiB):
// no configuration turns an upload route into an unbounded one.

// attachLimitSet holds the limits the areas of one Router declared. It lives on the Router
// (Router.attachLimits), so it goes away with it.
type attachLimitSet struct {
	mu      sync.RWMutex
	byRoute map[string]func() int64
}

func attachRouteKey(method, path string) string { return method + " " + path }

// attachSetRouteLimit declares the request body limit of one exact route of rt. limit is
// called for every request to it; a result that is not positive means the gateway-wide limit.
func attachSetRouteLimit(rt *Router, method, path string, limit func() int64) {
	set := &rt.attachLimits
	set.mu.Lock()
	if set.byRoute == nil {
		set.byRoute = map[string]func() int64{}
	}
	set.byRoute[attachRouteKey(method, path)] = limit
	set.mu.Unlock()
}

// attachBodyLimit is the body limit of a request: the one its route declared, clamped to the
// ceiling, else gateway.max_body_bytes.
func (s *Server) attachBodyLimit(r *http.Request) int64 {
	if s.router != nil {
		set := &s.router.attachLimits
		set.mu.RLock()
		limit := set.byRoute[attachRouteKey(r.Method, r.URL.Path)]
		set.mu.RUnlock()
		if limit != nil {
			if n := limit(); n > 0 {
				return min(n, maxMaxBodyBytes)
			}
		}
	}
	return s.cfg.MaxBodyBytes
}
