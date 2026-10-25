package identity

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// MiddlewareOption customises Middleware.
type MiddlewareOption func(*middleware)

// WithLogger sets where the middleware records refusals (code, reason, method
// and path only; never a header value). The default discards.
func WithLogger(l *slog.Logger) MiddlewareOption {
	return func(m *middleware) {
		if l != nil {
			m.log = l
		}
	}
}

type middleware struct {
	auth Authenticator
	log  *slog.Logger
}

// Middleware authenticates every request it wraps. The credential is read from
// the Authorization header only, as "Bearer <token>": never from the query
// string, a cookie or the body, where it would end up in logs and proxies.
// On success the request context carries the Verified context (read it with
// ScopeFromContext or RequireScope) and the Authorization header is removed so
// nothing downstream can forward or log the bearer credential. On failure the
// handler is not called and the response is the typed error envelope:
// 401 for unauthenticated, expired, replayed and scope_missing, 403 for
// scope_violation, 503 when the replay cache or key store cannot be consulted.
//
// Scope comes from the credential and from nothing else: tenant, workspace and
// principal values in headers, query or body are ignored by construction,
// because no code here reads them.
func Middleware(auth Authenticator, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	m := &middleware{auth: auth, log: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(m)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := BearerToken(r.Header)
			if err != nil {
				m.refuse(w, r, err)
				return
			}
			v, err := m.auth.Authenticate(r.Context(), token)
			if err != nil {
				m.refuse(w, r, err)
				return
			}
			ctx, err := WithVerified(r.Context(), v)
			if err != nil {
				// An Authenticator that returns an invalid Scope without an
				// error is a bug; the request is refused, not defaulted.
				m.refuse(w, r, err)
				return
			}
			m.log.LogAttrs(ctx, slog.LevelDebug, "context token accepted",
				slog.String("application_id", v.Scope.ApplicationID),
				slog.String("tenant_id", v.Scope.TenantID),
				slog.String("workspace_id", v.Scope.WorkspaceID),
				slog.String("jti", v.TokenID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path))
			r2 := r.WithContext(ctx)
			r2.Header = r.Header.Clone()
			r2.Header.Del("Authorization")
			next.ServeHTTP(w, r2)
		})
	}
}

// BearerToken extracts the token from exactly one "Authorization: Bearer
// <token>" header. Zero headers is no_credential; several, a different scheme,
// an empty token, whitespace inside it or an over-long one is malformed_token.
func BearerToken(h http.Header) (string, error) {
	vals := h.Values("Authorization")
	switch len(vals) {
	case 0:
		return "", unauthenticated(ReasonNoCredential)
	case 1:
	default:
		return "", unauthenticated(ReasonMalformed)
	}
	scheme, token, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > MaxTokenSize ||
		strings.ContainsAny(token, " \t\r\n") {
		return "", unauthenticated(ReasonMalformed)
	}
	return token, nil
}

// statusFor maps a failure code to the HTTP status of the refusal.
func statusFor(code contracts.ErrCode) int {
	switch code {
	case contracts.ErrUnauthenticated, contracts.ErrExpired, contracts.ErrReplayed, contracts.ErrScopeMissing:
		return http.StatusUnauthorized
	case contracts.ErrScopeViolation, contracts.ErrPermissionDenied:
		return http.StatusForbidden
	case contracts.ErrUnavailable, contracts.ErrDegraded, contracts.ErrResourceExhausted, contracts.ErrTimeout:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

func (m *middleware) refuse(w http.ResponseWriter, r *http.Request, err error) {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil {
		// An untyped failure is never described to the caller and never read
		// as anything but a refusal.
		ce = contracts.New(contracts.ErrInternal, "context could not be verified")
	}
	m.log.LogAttrs(r.Context(), slog.LevelWarn, "context token refused",
		slog.String("code", string(ce.Code)),
		slog.String("reason", string(ReasonOf(ce))),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path))

	// Only the typed fields go out: the wrapped cause stays server-side.
	body := contracts.Error{Code: ce.Code, Message: ce.Message, Retryable: ce.Retryable, Details: ce.Details}
	status := statusFor(ce.Code)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", "Bearer")
	}
	if ce.Retryable {
		h.Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
